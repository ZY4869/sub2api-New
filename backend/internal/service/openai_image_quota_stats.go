package service

// [local] 生图额度统计：按套餐汇总“用满时窗口内的生图张数”，并按账号给出当前窗口的估算上限，
// 供手动设定套餐限额参考。张数来自保留的用量日志，日志清理会使历史样本变小。
import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	openAIImageQuotaStatsTimeout            = 10 * time.Second
	openAIImageQuotaEstimateMinUsedPercent  = 10
	openAIImageQuotaUnknownPlanType         = "unknown"
	openAIImageQuotaSourceHeadersSnapshot   = "headers"
	openAIImageQuotaSourceUsageSnapshot     = "usage_api"
	openAIImageQuotaMaxSnapshotWindowMinute = openAIImageQuotaMaxWindowMinutes
)

// openAIImageQuotaPlanOrder 按套餐档位排序，其余套餐排在后面。
var openAIImageQuotaPlanOrder = []string{"go", "plus", "prolite", "pro", "promax"}

type OpenAIImageQuotaStats struct {
	GeneratedAt time.Time                      `json:"generated_at"`
	Plans       []OpenAIImageQuotaPlanStats    `json:"plans"`
	Accounts    []OpenAIImageQuotaAccountStats `json:"accounts"`
}

type OpenAIImageQuotaPlanStats struct {
	PlanType         string                        `json:"plan_type"`
	AccountCount     int                           `json:"account_count"`
	ObservationCount int                           `json:"observation_count"`
	Windows          []OpenAIImageQuotaWindowStats `json:"windows"`
}

// OpenAIImageQuotaWindowStats 同一窗口时长下“用满时的生图张数”分布；WindowMinutes 为 0 表示窗口未知。
type OpenAIImageQuotaWindowStats struct {
	WindowMinutes int     `json:"window_minutes"`
	Samples       int     `json:"samples"`
	MinImages     int64   `json:"min_images"`
	MedianImages  float64 `json:"median_images"`
	MaxImages     int64   `json:"max_images"`
}

type OpenAIImageQuotaAccountStats struct {
	AccountID           int64                              `json:"account_id"`
	Name                string                             `json:"name"`
	PlanType            string                             `json:"plan_type"`
	MainPoolOnlyBlocked bool                               `json:"main_pool_only_blocked"`
	ImageCooldownUntil  *time.Time                         `json:"image_cooldown_until,omitempty"`
	ImageCooldownReason string                             `json:"image_cooldown_reason,omitempty"`
	CurrentWindow       *OpenAIImageQuotaCurrentWindow     `json:"current_window,omitempty"`
	Observations        []OpenAIImageQuotaObservationStats `json:"observations,omitempty"`
}

// OpenAIImageQuotaCurrentWindow 张数统计到快照时刻，与上游已用比例同一时点，估算上限才可比。
type OpenAIImageQuotaCurrentWindow struct {
	Source          string    `json:"source"`
	WindowMinutes   int       `json:"window_minutes"`
	From            time.Time `json:"from"`
	ResetAt         time.Time `json:"reset_at"`
	SnapshotAt      time.Time `json:"snapshot_at"`
	UsedPercent     float64   `json:"used_percent"`
	Images          int64     `json:"images"`
	EstimatedLimit  *int64    `json:"estimated_limit,omitempty"`
	MirrorSuspected bool      `json:"mirror_suspected,omitempty"`
}

type OpenAIImageQuotaObservationStats struct {
	OpenAIImageQuotaObservation
	ImagesInWindow *int64 `json:"images_in_window,omitempty"`
}

// GetOpenAIImageQuotaStats 汇总 OpenAI OAuth 账号的生图额度观测。
func (s *AccountUsageService) GetOpenAIImageQuotaStats(ctx context.Context) (*OpenAIImageQuotaStats, error) {
	counter, ok := s.usageLogRepo.(accountImageWindowCounter)
	if !ok || s.accountRepo == nil {
		return nil, errors.New("account image statistics are unavailable")
	}
	queryCtx, cancel := context.WithTimeout(ctx, openAIImageQuotaStatsTimeout)
	defer cancel()
	accounts, err := s.accountRepo.ListByPlatform(queryCtx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	stats := &OpenAIImageQuotaStats{GeneratedAt: now.UTC(), Plans: []OpenAIImageQuotaPlanStats{}, Accounts: []OpenAIImageQuotaAccountStats{}}
	type countTarget struct{ account, observation int } // observation < 0 表示当前窗口
	var (
		ranges  []AccountImageCountRange
		targets []countTarget
	)
	for i := range accounts {
		account := &accounts[i]
		if !isOpenAIImageIsolationAccount(account) {
			continue
		}
		item := OpenAIImageQuotaAccountStats{
			AccountID: account.ID, Name: account.Name, PlanType: openAIImageQuotaPlanKey(account),
			MainPoolOnlyBlocked: account.openAIMainPoolBlockedOnly(now),
		}
		if resetAt := account.modelRateLimitResetAt(openAIImageGenerationRateLimitKey); resetAt != nil && resetAt.After(now) {
			item.ImageCooldownUntil = resetAt
			item.ImageCooldownReason = modelRateLimitReason(account, openAIImageGenerationRateLimitKey)
		}
		index := len(stats.Accounts)
		if window := openAIImageQuotaCurrentWindow(account.Extra, now); window != nil {
			item.CurrentWindow = window
			targets = append(targets, countTarget{index, -1})
			ranges = append(ranges, AccountImageCountRange{AccountID: account.ID, From: window.From, To: window.SnapshotAt})
		}
		for _, observation := range openAIImageQuotaObservationsFromExtra(account.Extra) {
			item.Observations = append(item.Observations, OpenAIImageQuotaObservationStats{OpenAIImageQuotaObservation: observation})
			if observation.CountedFrom != nil && observation.CountedFrom.Before(observation.ObservedAt) {
				targets = append(targets, countTarget{index, len(item.Observations) - 1})
				ranges = append(ranges, AccountImageCountRange{AccountID: account.ID, From: *observation.CountedFrom, To: observation.ObservedAt})
			}
		}
		stats.Accounts = append(stats.Accounts, item)
	}
	counts, err := counter.CountAccountImagesInRanges(queryCtx, ranges)
	if err != nil {
		return nil, err
	}
	for i, target := range targets {
		if i >= len(counts) {
			break
		}
		item := &stats.Accounts[target.account]
		if target.observation >= 0 {
			value := counts[i]
			item.Observations[target.observation].ImagesInWindow = &value
			continue
		}
		item.CurrentWindow.Images = counts[i]
		if item.CurrentWindow.UsedPercent >= openAIImageQuotaEstimateMinUsedPercent {
			estimate := int64(math.Round(float64(counts[i]) * 100 / item.CurrentWindow.UsedPercent))
			item.CurrentWindow.EstimatedLimit = &estimate
		}
	}
	stats.Plans = summarizeOpenAIImageQuotaPlans(stats.Accounts)
	sort.SliceStable(stats.Accounts, func(i, j int) bool {
		left, right := openAIImageQuotaPlanRank(stats.Accounts[i].PlanType), openAIImageQuotaPlanRank(stats.Accounts[j].PlanType)
		if left != right {
			return left < right
		}
		return stats.Accounts[i].Name < stats.Accounts[j].Name
	})
	return stats, nil
}

func openAIImageQuotaPlanKey(account *Account) string {
	if key := NormalizeOpenAIPlanType(account.GetCredential("plan_type")); key != "" {
		return key
	}
	return openAIImageQuotaUnknownPlanType
}

func openAIImageQuotaPlanRank(planType string) int {
	for i, plan := range openAIImageQuotaPlanOrder {
		if plan == planType {
			return i
		}
	}
	return len(openAIImageQuotaPlanOrder)
}

func modelRateLimitReason(account *Account, scope string) string {
	limits, _ := account.Extra[modelRateLimitsKey].(map[string]any)
	entry, _ := limits[scope].(map[string]any)
	reason, _ := entry["reason"].(string)
	return reason
}

// openAIImageQuotaCurrentWindow 取最新一份生图池快照中尚未重置的最短窗口。
func openAIImageQuotaCurrentWindow(extra map[string]any, now time.Time) *OpenAIImageQuotaCurrentWindow {
	var best *OpenAIImageQuotaCurrentWindow
	consider := func(window *OpenAIImageQuotaCurrentWindow) {
		if window == nil || window.WindowMinutes <= 0 || window.WindowMinutes > openAIImageQuotaMaxSnapshotWindowMinute ||
			!window.ResetAt.After(now) || window.SnapshotAt.After(now) {
			return
		}
		window.From = window.ResetAt.Add(-time.Duration(window.WindowMinutes) * time.Minute)
		if !window.From.Before(window.SnapshotAt) {
			return
		}
		switch {
		case best == nil, window.SnapshotAt.After(best.SnapshotAt):
			best = window
		case window.SnapshotAt.Equal(best.SnapshotAt) && window.WindowMinutes < best.WindowMinutes:
			best = window
		}
	}
	if snapshot, ok := extra[codexImageHeadersSnapshotKey].(map[string]any); ok {
		collectedAt := parseSchedulingResetAt(snapshot["collected_at"])
		mirror, _ := snapshot["mirror_suspected"].(bool)
		for _, prefix := range []string{"primary", "secondary"} {
			used, okUsed := resolveAccountExtraNumber(snapshot, prefix+"_used_percent")
			resetAfter, okReset := resolveAccountExtraNumber(snapshot, prefix+"_reset_after_seconds")
			minutes, okMinutes := resolveAccountExtraNumber(snapshot, prefix+"_window_minutes")
			if collectedAt == nil || !okUsed || !okReset || !okMinutes {
				continue
			}
			resetAt, ok := openAIImagePoolResetAfter(*collectedAt, int64(resetAfter))
			if !ok {
				continue
			}
			consider(&OpenAIImageQuotaCurrentWindow{Source: openAIImageQuotaSourceHeadersSnapshot, WindowMinutes: int(minutes),
				ResetAt: resetAt, SnapshotAt: *collectedAt, UsedPercent: used, MirrorSuspected: mirror})
		}
	}
	if snapshot, ok := extra[codexImageUsageSnapshotKey].(map[string]any); ok {
		fetchedAt := parseSchedulingResetAt(snapshot["fetched_at"])
		windows, _ := snapshot["windows"].([]any)
		for _, raw := range windows {
			window, _ := raw.(map[string]any)
			used, okUsed := resolveAccountExtraNumber(window, "used_percent")
			minutes, okMinutes := resolveAccountExtraNumber(window, "window_minutes")
			resetAt := parseSchedulingResetAt(window["reset_at"])
			if fetchedAt == nil || resetAt == nil || !okUsed || !okMinutes {
				continue
			}
			consider(&OpenAIImageQuotaCurrentWindow{Source: openAIImageQuotaSourceUsageSnapshot, WindowMinutes: int(minutes),
				ResetAt: *resetAt, SnapshotAt: *fetchedAt, UsedPercent: used})
		}
	}
	return best
}

func summarizeOpenAIImageQuotaPlans(accounts []OpenAIImageQuotaAccountStats) []OpenAIImageQuotaPlanStats {
	type bucket struct {
		stats   OpenAIImageQuotaPlanStats
		samples map[int][]int64
	}
	buckets := map[string]*bucket{}
	for _, account := range accounts {
		entry := buckets[account.PlanType]
		if entry == nil {
			entry = &bucket{stats: OpenAIImageQuotaPlanStats{PlanType: account.PlanType, Windows: []OpenAIImageQuotaWindowStats{}}, samples: map[int][]int64{}}
			buckets[account.PlanType] = entry
		}
		entry.stats.AccountCount++
		for _, observation := range account.Observations {
			entry.stats.ObservationCount++
			if observation.ImagesInWindow != nil {
				entry.samples[observation.WindowMinutes] = append(entry.samples[observation.WindowMinutes], *observation.ImagesInWindow)
			}
		}
	}
	plans := make([]OpenAIImageQuotaPlanStats, 0, len(buckets))
	for _, entry := range buckets {
		for minutes, samples := range entry.samples {
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			median := float64(samples[len(samples)/2])
			if len(samples)%2 == 0 {
				median = float64(samples[len(samples)/2-1]+samples[len(samples)/2]) / 2
			}
			entry.stats.Windows = append(entry.stats.Windows, OpenAIImageQuotaWindowStats{
				WindowMinutes: minutes, Samples: len(samples), MinImages: samples[0], MedianImages: median, MaxImages: samples[len(samples)-1],
			})
		}
		sort.Slice(entry.stats.Windows, func(i, j int) bool {
			return entry.stats.Windows[i].WindowMinutes < entry.stats.Windows[j].WindowMinutes
		})
		plans = append(plans, entry.stats)
	}
	sort.Slice(plans, func(i, j int) bool {
		left, right := openAIImageQuotaPlanRank(plans[i].PlanType), openAIImageQuotaPlanRank(plans[j].PlanType)
		if left != right {
			return left < right
		}
		return strings.Compare(plans[i].PlanType, plans[j].PlanType) < 0
	})
	return plans
}
