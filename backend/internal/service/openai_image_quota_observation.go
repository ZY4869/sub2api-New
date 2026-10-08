package service

// [local] 生图额度池：按原生响应头或 /wham/usage 主动暂停生图，并记录“用满”观测，
// 供按套餐推算生图上限。上游生图池的窗口和命名尚未实测，一律按上游返回的数据处理，不写死窗口。
import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	codexImageUsageSnapshotKey     = "codex_image_usage_snapshot"
	codexImageQuotaObservationsKey = "codex_image_quota_observations"

	openAIImageQuotaPauseReason = "openai_image_quota_pause"
	openAIImagePlanLimitReason  = "openai_image_plan_limit"

	openAIImageObservationSourceHeaders = "headers"
	openAIImageObservationSourceUsage   = "usage_api"
	openAIImageObservationSource429     = "429"

	openAIImageQuotaObservationLimit       = 20
	openAIImageObservationDedupeWindow     = time.Minute
	openAIImageObservationLookback         = 31 * 24 * time.Hour
	openAIImageObservationWriteTimeout     = 5 * time.Second
	openAIImagePoolMaxResetAfter           = 400 * 24 * time.Hour
	openAIImageHeadersMirrorPercentEpsilon = 0.5
)

var openAIImageObservationThrottle = newAccountWriteThrottle(time.Minute)

// OpenAIImageQuotaObservation 记录一次生图池用满；用满时窗口内的生图数在统计时按日志计算。
type OpenAIImageQuotaObservation struct {
	ObservedAt    time.Time  `json:"observed_at"`
	Source        string     `json:"source"`
	PlanType      string     `json:"plan_type,omitempty"`
	WindowMinutes int        `json:"window_minutes,omitempty"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	CountedFrom   *time.Time `json:"counted_from,omitempty"`
}

type openAIImagePoolWindow struct {
	WindowMinutes int       `json:"window_minutes,omitempty"`
	UsedPercent   float64   `json:"used_percent"`
	ResetAt       time.Time `json:"reset_at"`
}

func openAIImagePoolResetAfter(now time.Time, seconds int64) (time.Time, bool) {
	if seconds < 0 || seconds > int64(openAIImagePoolMaxResetAfter/time.Second) {
		return time.Time{}, false
	}
	return now.Add(time.Duration(seconds) * time.Second), true
}

func openAIImagePoolWindowsFromHeaders(snapshot *OpenAICodexUsageSnapshot, now time.Time) []openAIImagePoolWindow {
	if snapshot == nil {
		return nil
	}
	var windows []openAIImagePoolWindow
	add := func(used *float64, resetAfter, minutes *int) {
		if used == nil || resetAfter == nil {
			return
		}
		resetAt, ok := openAIImagePoolResetAfter(now, int64(*resetAfter))
		if !ok {
			return
		}
		window := openAIImagePoolWindow{UsedPercent: *used, ResetAt: resetAt}
		if minutes != nil && *minutes > 0 {
			window.WindowMinutes = *minutes
		}
		windows = append(windows, window)
	}
	add(snapshot.PrimaryUsedPercent, snapshot.PrimaryResetAfterSeconds, snapshot.PrimaryWindowMinutes)
	add(snapshot.SecondaryUsedPercent, snapshot.SecondaryResetAfterSeconds, snapshot.SecondaryWindowMinutes)
	return windows
}

func openAIImagePoolWindowsFromUsage(limit *OpenAIRateLimit, now time.Time) []openAIImagePoolWindow {
	if limit == nil {
		return nil
	}
	var windows []openAIImagePoolWindow
	for _, source := range []*OpenAIRateLimitWindow{limit.PrimaryWindow, limit.SecondaryWindow} {
		if source == nil {
			continue
		}
		window := openAIImagePoolWindow{UsedPercent: source.UsedPercent, WindowMinutes: int(source.LimitWindowSeconds / 60)}
		if source.ResetAt > 0 {
			window.ResetAt = time.Unix(source.ResetAt, 0)
		} else if resetAt, ok := openAIImagePoolResetAfter(now, source.ResetAfterSeconds); ok && source.ResetAfterSeconds > 0 {
			window.ResetAt = resetAt
		} else {
			continue
		}
		windows = append(windows, window)
	}
	return windows
}

// openAIImageHeadersMirrorMainPool 原生生图响应头与主池快照的窗口和用量都一致时，可能是上游回显了主池。
// 此时不据此暂停生图，否则会和反向隔离相互抵消。
func openAIImageHeadersMirrorMainPool(account *Account, snapshot *OpenAICodexUsageSnapshot) bool {
	if account == nil || snapshot == nil {
		return false
	}
	compared := 0
	same := func(used *float64, minutes *int, prefix string) bool {
		if used == nil {
			return true
		}
		mainUsed, okUsed := resolveAccountExtraNumber(account.Extra, "codex_"+prefix+"_used_percent")
		mainMinutes, okMinutes := resolveAccountExtraNumber(account.Extra, "codex_"+prefix+"_window_minutes")
		if !okUsed || !okMinutes || minutes == nil || int(mainMinutes) != *minutes || math.Abs(mainUsed-*used) > openAIImageHeadersMirrorPercentEpsilon {
			return false
		}
		compared++
		return true
	}
	return same(snapshot.PrimaryUsedPercent, snapshot.PrimaryWindowMinutes, "primary") &&
		same(snapshot.SecondaryUsedPercent, snapshot.SecondaryWindowMinutes, "secondary") && compared > 0
}

// applyOpenAIImagePoolPause 任一窗口用量达到阈值时冷却生图到该窗口重置；阈值为 0 表示关闭。
func applyOpenAIImagePoolPause(ctx context.Context, repo AccountRepository, account *Account, windows []openAIImagePoolWindow, thresholdPercent int, reason string, now time.Time) {
	if repo == nil || account == nil || thresholdPercent <= 0 {
		return
	}
	var until time.Time
	for _, window := range windows {
		if window.UsedPercent >= float64(thresholdPercent) && window.ResetAt.After(now) && window.ResetAt.After(until) {
			until = window.ResetAt
		}
	}
	if until.IsZero() {
		return
	}
	if existing := account.modelRateLimitResetAt(openAIImageGenerationRateLimitKey); existing != nil && !until.After(*existing) {
		return
	}
	persistOpenAIImageCooldown(ctx, repo, account, until, reason)
}

// observeOpenAIImagePoolHeaders 处理原生 /codex/images/* 的额度响应头：主动暂停与用满观测。
// 不受诊断快照节流影响；返回是否疑似回显主池。
func (s *OpenAIGatewayService) observeOpenAIImagePoolHeaders(ctx context.Context, account *Account, snapshot *OpenAICodexUsageSnapshot, now time.Time) bool {
	windows := openAIImagePoolWindowsFromHeaders(snapshot, now)
	if len(windows) == 0 {
		return false
	}
	if openAIImageHeadersMirrorMainPool(account, snapshot) {
		slog.Debug("openai_image_headers_mirror_suspected", "account_id", account.ID)
		return true
	}
	settings := s.settingService.openAIImageQuotaSettingsCached(ctx)
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	applyOpenAIImagePoolPause(stateCtx, s.accountRepo, account, windows, settings.PauseThresholdPercent, openAIImageQuotaPauseReason, now)
	s.recordOpenAIImagePoolExhaustion(account, windows, openAIImageObservationSourceHeaders, now)
	return false
}

// recordOpenAIImageNative429Exhaustion 原生端点额度用尽是套餐生图上限最直接的样本。
func (s *OpenAIGatewayService) recordOpenAIImageNative429Exhaustion(account *Account, headers http.Header, resetAt time.Time, now time.Time) {
	window := openAIImagePoolWindow{UsedPercent: 100, ResetAt: resetAt}
	for _, candidate := range openAIImagePoolWindowsFromHeaders(ParseCodexRateLimitHeaders(headers), now) {
		if candidate.UsedPercent >= 100 && candidate.WindowMinutes > 0 {
			window.WindowMinutes = candidate.WindowMinutes
		}
	}
	s.recordOpenAIImagePoolExhaustion(account, []openAIImagePoolWindow{window}, openAIImageObservationSource429, now)
}

// recordOpenAIImagePoolExhaustion 异步记录用满的窗口，同一账号每分钟最多写一次。
func (s *OpenAIGatewayService) recordOpenAIImagePoolExhaustion(account *Account, windows []openAIImagePoolWindow, source string, now time.Time) {
	if s == nil || s.accountRepo == nil || !isOpenAIImageIsolationAccount(account) {
		return
	}
	observations := openAIImagePoolObservations(windows, false, source, now)
	if len(observations) == 0 || !openAIImageObservationThrottle.Allow(account.ID, now) {
		return
	}
	repo, accountID := s.accountRepo, account.ID
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("openai_image_quota_observation_panic", "account_id", accountID, "panic", recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), openAIImageObservationWriteTimeout)
		defer cancel()
		if err := recordOpenAIImageQuotaObservations(ctx, repo, accountID, observations); err != nil {
			slog.Warn("openai_image_quota_observation_failed", "account_id", accountID, "error", err)
		}
	}()
}

// openAIImagePoolObservations 取用满的窗口；上游只给 limit_reached 时以最晚重置的窗口为准。
func openAIImagePoolObservations(windows []openAIImagePoolWindow, limitReached bool, source string, now time.Time) []OpenAIImageQuotaObservation {
	var exhausted []openAIImagePoolWindow
	latest := -1
	for i, window := range windows {
		if window.UsedPercent >= 100 {
			exhausted = append(exhausted, window)
		}
		if latest < 0 || window.ResetAt.After(windows[latest].ResetAt) {
			latest = i
		}
	}
	if len(exhausted) == 0 && limitReached && latest >= 0 {
		exhausted = append(exhausted, windows[latest])
	}
	observations := make([]OpenAIImageQuotaObservation, 0, len(exhausted))
	for _, window := range exhausted {
		resetAt := window.ResetAt.UTC()
		used := window.UsedPercent
		observations = append(observations, OpenAIImageQuotaObservation{
			ObservedAt: now.UTC(), Source: source, WindowMinutes: window.WindowMinutes, ResetAt: &resetAt, UsedPercent: &used,
		})
	}
	return observations
}

func openAIImageQuotaObservationsFromExtra(extra map[string]any) []OpenAIImageQuotaObservation {
	raw, ok := extra[codexImageQuotaObservationsKey]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var observations []OpenAIImageQuotaObservation
	if err := json.Unmarshal(data, &observations); err != nil {
		return nil
	}
	return observations
}

// openAIImageObservationRecorded 同一窗口（重置时间相差一分钟内）只记一次，来源不同也算同一次用满。
func openAIImageObservationRecorded(existing []OpenAIImageQuotaObservation, observation OpenAIImageQuotaObservation) bool {
	for _, item := range existing {
		if item.ResetAt == nil || observation.ResetAt == nil {
			continue
		}
		sameWindow := item.WindowMinutes == 0 || observation.WindowMinutes == 0 || item.WindowMinutes == observation.WindowMinutes
		if sameWindow && item.ResetAt.Sub(*observation.ResetAt).Abs() <= openAIImageObservationDedupeWindow {
			return true
		}
	}
	return false
}

// openAIImageObservationWindowStart 窗口已知时取 reset−窗口；未知时取上一次用满的重置时刻。
func openAIImageObservationWindowStart(existing []OpenAIImageQuotaObservation, observation OpenAIImageQuotaObservation) *time.Time {
	if observation.WindowMinutes > 0 && observation.ResetAt != nil {
		start := observation.ResetAt.Add(-time.Duration(observation.WindowMinutes) * time.Minute)
		return &start
	}
	var start *time.Time
	for _, item := range existing {
		if item.ResetAt == nil || !item.ResetAt.Before(observation.ObservedAt) || observation.ObservedAt.Sub(*item.ResetAt) > openAIImageObservationLookback {
			continue
		}
		if start == nil || item.ResetAt.After(*start) {
			value := *item.ResetAt
			start = &value
		}
	}
	return start
}

// recordOpenAIImageQuotaObservations 追加观测并只保留最近若干条；诊断键不触发调度刷新。
func recordOpenAIImageQuotaObservations(ctx context.Context, repo AccountRepository, accountID int64, observations []OpenAIImageQuotaObservation) error {
	if repo == nil || len(observations) == 0 {
		return nil
	}
	current, err := repo.GetByID(ctx, accountID)
	if err != nil || current == nil {
		return err
	}
	existing := openAIImageQuotaObservationsFromExtra(current.Extra)
	planType := NormalizeOpenAIPlanType(current.GetCredential("plan_type"))
	added := false
	for _, observation := range observations {
		if observation.ResetAt == nil || openAIImageObservationRecorded(existing, observation) {
			continue
		}
		observation.PlanType = planType
		observation.CountedFrom = openAIImageObservationWindowStart(existing, observation)
		existing = append(existing, observation)
		added = true
	}
	if !added {
		return nil
	}
	if len(existing) > openAIImageQuotaObservationLimit {
		existing = existing[len(existing)-openAIImageQuotaObservationLimit:]
	}
	return repo.UpdateExtra(ctx, accountID, map[string]any{codexImageQuotaObservationsKey: existing})
}

// findOpenAIImagePoolLimit 上游生图池命名尚未实测，按 metered_feature 或 limit_name 含 image 匹配。
func findOpenAIImagePoolLimit(usage *OpenAIQuotaUsage) *OpenAIAdditionalRateLimit {
	if usage == nil {
		return nil
	}
	for i := range usage.AdditionalRateLimits {
		entry := &usage.AdditionalRateLimits[i]
		if strings.Contains(strings.ToLower(entry.MeteredFeature+" "+entry.LimitName), "image") {
			return entry
		}
	}
	return nil
}

// applyOpenAIImagePoolUsage 保存 /wham/usage 的生图池快照；上游明确用满时冷却生图并记录观测。
func applyOpenAIImagePoolUsage(ctx context.Context, repo AccountRepository, accountID int64, usage *OpenAIQuotaUsage, now time.Time) {
	pool := findOpenAIImagePoolLimit(usage)
	if repo == nil || pool == nil || pool.RateLimit == nil {
		return
	}
	windows := openAIImagePoolWindowsFromUsage(pool.RateLimit, now)
	snapshot := map[string]any{
		"limit_name":      pool.LimitName,
		"metered_feature": pool.MeteredFeature,
		"limit_reached":   pool.RateLimit.LimitReached,
		"windows":         windows,
		"fetched_at":      now.UTC().Format(time.RFC3339),
	}
	if err := repo.UpdateExtra(ctx, accountID, map[string]any{codexImageUsageSnapshotKey: snapshot}); err != nil {
		slog.Warn("openai_image_usage_snapshot_failed", "account_id", accountID, "error", err)
	}
	observations := openAIImagePoolObservations(windows, pool.RateLimit.LimitReached, openAIImageObservationSourceUsage, now)
	if len(observations) == 0 {
		return
	}
	account, err := repo.GetByID(ctx, accountID)
	if err != nil || !isOpenAIImageIsolationAccount(account) {
		return
	}
	var until time.Time
	for _, observation := range observations {
		if observation.ResetAt.After(until) {
			until = *observation.ResetAt
		}
	}
	if until.After(now) {
		persistOpenAIImageCooldown(ctx, repo, account, until, openAIImageQuotaReason)
	}
	if err := recordOpenAIImageQuotaObservations(ctx, repo, accountID, observations); err != nil {
		slog.Warn("openai_image_quota_observation_failed", "account_id", accountID, "error", err)
	}
}

// ApplyImagePoolUsage 由会写入状态的额度刷新调用（POST 刷新、自动重置），只读查询不调用。
func (s *OpenAIQuotaService) ApplyImagePoolUsage(ctx context.Context, accountID int64, usage *OpenAIQuotaUsage) {
	if s == nil {
		return
	}
	applyOpenAIImagePoolUsage(ctx, s.accountRepo, accountID, usage, time.Now())
}
