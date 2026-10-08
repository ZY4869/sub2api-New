//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// imageQuotaStateRepo 像数据库一样保存当前账号：extra 经 JSON 往返，生图冷却按调用记录。
type imageQuotaStateRepo struct {
	mockAccountRepoForGemini
	mu            sync.Mutex
	current       *Account
	listed        []Account
	cooldowns     []string
	cooldownUntil time.Time
}

func (r *imageQuotaStateRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return nil, errors.New("account not found")
	}
	clone := *r.current
	clone.Extra = copyTestJSONMap(r.current.Extra)
	return &clone, nil
}

func (r *imageQuotaStateRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	var out []Account
	for _, account := range r.listed {
		if account.Platform == platform {
			out = append(out, account)
		}
	}
	return out, nil
}

func (r *imageQuotaStateRepo) SetModelRateLimit(_ context.Context, _ int64, _ string, until time.Time, reason ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cooldowns = append(r.cooldowns, strings.Join(reason, ","))
	r.cooldownUntil = until
	return nil
}

func (r *imageQuotaStateRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	decoded := copyTestJSONMap(updates)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil {
		if r.current.Extra == nil {
			r.current.Extra = map[string]any{}
		}
		for key, value := range decoded {
			r.current.Extra[key] = value
		}
	}
	return nil
}

func (r *imageQuotaStateRepo) state() ([]string, time.Time, map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var extra map[string]any
	if r.current != nil {
		extra = copyTestJSONMap(r.current.Extra)
	}
	return append([]string(nil), r.cooldowns...), r.cooldownUntil, extra
}

func copyTestJSONMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

// imageWindowCounterStub 按区间返回张数，并记录套餐限额的计数参数。
type imageWindowCounterStub struct {
	UsageLogRepository
	count       int64
	earliest    *time.Time
	sinceCalls  int
	lastSince   time.Time
	lastExclude string
	rangeCount  func(AccountImageCountRange) int64
}

func (s *imageWindowCounterStub) CountAccountImagesSince(_ context.Context, _ int64, since time.Time, excludeRequestID string) (int64, *time.Time, error) {
	s.sinceCalls++
	s.lastSince, s.lastExclude = since, excludeRequestID
	return s.count, s.earliest, nil
}

func (s *imageWindowCounterStub) CountAccountImagesInRanges(_ context.Context, ranges []AccountImageCountRange) ([]int64, error) {
	counts := make([]int64, len(ranges))
	for i, item := range ranges {
		counts[i] = s.rangeCount(item)
	}
	return counts, nil
}

func newImageQuotaSettingService(t *testing.T, settings *OpenAIImageQuotaSettings) *SettingService {
	t.Helper()
	service := NewSettingService(newMockSettingRepo(), &config.Config{})
	t.Cleanup(func() { openAIImageQuotaSettingsCache.Delete(service) })
	if settings != nil {
		_, err := service.SetOpenAIImageQuotaSettings(context.Background(), settings)
		require.NoError(t, err)
	}
	return service
}

func resetOpenAIImageObservationThrottle(t *testing.T, accountID int64) {
	t.Helper()
	clear := func() {
		openAIImageObservationThrottle.mu.Lock()
		delete(openAIImageObservationThrottle.lastByID, accountID)
		openAIImageObservationThrottle.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func timePointer(value time.Time) *time.Time { return &value }

func TestNormalizeOpenAIImageQuotaSettings(t *testing.T) {
	normalized, err := NormalizeOpenAIImageQuotaSettings(&OpenAIImageQuotaSettings{
		PauseThresholdPercent: 95,
		PlanLimits: map[string][]OpenAIImagePlanLimitRule{
			"Pro Lite":    {{WindowMinutes: 1440, MaxImages: 200}, {WindowMinutes: 180, MaxImages: 40}},
			"ChatGPT_Pro": {{WindowMinutes: 300, MaxImages: 100}},
			"plus":        {},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 95, normalized.PauseThresholdPercent)
	require.Equal(t, []OpenAIImagePlanLimitRule{{WindowMinutes: 180, MaxImages: 40}, {WindowMinutes: 1440, MaxImages: 200}}, normalized.PlanLimits["prolite"])
	require.Equal(t, []OpenAIImagePlanLimitRule{{WindowMinutes: 300, MaxImages: 100}}, normalized.PlanLimits["pro"])
	require.NotContains(t, normalized.PlanLimits, "plus", "plans without rules are dropped")

	rule := func(window, images int) OpenAIImagePlanLimitRule {
		return OpenAIImagePlanLimitRule{WindowMinutes: window, MaxImages: images}
	}
	for name, settings := range map[string]*OpenAIImageQuotaSettings{
		"threshold above 100": {PauseThresholdPercent: 101},
		"negative threshold":  {PauseThresholdPercent: -1},
		"zero window":         {PlanLimits: map[string][]OpenAIImagePlanLimitRule{"go": {rule(0, 1)}}},
		"window too long":     {PlanLimits: map[string][]OpenAIImagePlanLimitRule{"go": {rule(openAIImageQuotaMaxWindowMinutes+1, 1)}}},
		"zero images":         {PlanLimits: map[string][]OpenAIImagePlanLimitRule{"go": {rule(60, 0)}}},
		"duplicate plan":      {PlanLimits: map[string][]OpenAIImagePlanLimitRule{"pro": {rule(60, 1)}, "chatgptpro": {rule(60, 2)}}},
		"too many rules":      {PlanLimits: map[string][]OpenAIImagePlanLimitRule{"go": {rule(1, 1), rule(2, 1), rule(3, 1), rule(4, 1), rule(5, 1)}}},
		"blank plan":          {PlanLimits: map[string][]OpenAIImagePlanLimitRule{" - ": {rule(60, 1)}}},
	} {
		_, err := NormalizeOpenAIImageQuotaSettings(settings)
		require.Error(t, err, name)
	}
}

func TestOpenAIImageQuotaSettingsStoreAndCache(t *testing.T) {
	repo := newMockSettingRepo()
	settings := NewSettingService(repo, &config.Config{})
	t.Cleanup(func() { openAIImageQuotaSettingsCache.Delete(settings) })
	ctx := context.Background()

	loaded, err := settings.GetOpenAIImageQuotaSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, defaultOpenAIImageQuotaPauseThresholdPercent, loaded.PauseThresholdPercent)
	require.Equal(t, defaultOpenAIImageQuotaPauseThresholdPercent, settings.openAIImageQuotaSettingsCached(ctx).PauseThresholdPercent)

	_, err = settings.SetOpenAIImageQuotaSettings(ctx, &OpenAIImageQuotaSettings{PauseThresholdPercent: 120})
	require.Error(t, err)
	saved, err := settings.SetOpenAIImageQuotaSettings(ctx, &OpenAIImageQuotaSettings{
		PauseThresholdPercent: 90,
		PlanLimits:            map[string][]OpenAIImagePlanLimitRule{"Plus": {{WindowMinutes: 60, MaxImages: 2}}},
	})
	require.NoError(t, err)
	require.Contains(t, saved.PlanLimits, "plus")
	cached := settings.openAIImageQuotaSettingsCached(ctx)
	require.Equal(t, 90, cached.PauseThresholdPercent, "saving refreshes the hot-path cache immediately")
	require.Equal(t, []OpenAIImagePlanLimitRule{{WindowMinutes: 60, MaxImages: 2}}, cached.planRules("PLUS"))

	repo.data[SettingKeyOpenAIImageQuotaSettings] = `{not json`
	loaded, err = settings.GetOpenAIImageQuotaSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, defaultOpenAIImageQuotaPauseThresholdPercent, loaded.PauseThresholdPercent, "corrupt settings fall back to defaults")
}

func TestObserveOpenAIImagePoolHeadersPausesAtThreshold(t *testing.T) {
	headers := func(used string) *OpenAICodexUsageSnapshot {
		header := http.Header{}
		header.Set("x-codex-primary-used-percent", used)
		header.Set("x-codex-primary-window-minutes", "180")
		header.Set("x-codex-primary-reset-after-seconds", "3600")
		return ParseCodexRateLimitHeaders(header)
	}
	for i, tc := range []struct {
		name       string
		threshold  int
		used       string
		mainExtra  map[string]any
		existing   time.Duration
		wantPause  bool
		wantMirror bool
	}{
		{name: "exhausted at default threshold", threshold: 100, used: "100", wantPause: true},
		{name: "early pause below exhaustion", threshold: 90, used: "95", wantPause: true},
		{name: "below threshold", threshold: 90, used: "50"},
		{name: "pause disabled", threshold: 0, used: "100"},
		{name: "longer cooldown already stored", threshold: 100, used: "100", existing: 2 * time.Hour},
		{name: "headers mirror the main pool", threshold: 100, used: "100", wantMirror: true,
			mainExtra: map[string]any{"codex_primary_used_percent": 100.0, "codex_primary_window_minutes": 180.0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accountID := int64(53001 + i)
			resetOpenAIImageObservationThrottle(t, accountID)
			account := &Account{ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: tc.mainExtra}
			now := time.Now()
			if tc.existing > 0 {
				setAccountModelRateLimitSnapshot(account, openAIImageGenerationRateLimitKey, now.Add(tc.existing), "quota", now)
			}
			repo := &imageQuotaStateRepo{current: account}
			svc := &OpenAIGatewayService{accountRepo: repo, settingService: newImageQuotaSettingService(t, &OpenAIImageQuotaSettings{PauseThresholdPercent: tc.threshold})}

			require.Equal(t, tc.wantMirror, svc.observeOpenAIImagePoolHeaders(context.Background(), account, headers(tc.used), now))
			cooldowns, until, _ := repo.state()
			if tc.wantPause {
				require.Equal(t, []string{openAIImageQuotaPauseReason}, cooldowns)
				require.WithinDuration(t, now.Add(time.Hour), until, time.Second)
				return
			}
			require.Empty(t, cooldowns)
		})
	}
}

func TestRecordOpenAIImagePoolExhaustionWritesObservationAsync(t *testing.T) {
	account := &Account{ID: 53050, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "plus"}}
	resetOpenAIImageObservationThrottle(t, account.ID)
	repo := &imageQuotaStateRepo{current: account}
	svc := &OpenAIGatewayService{accountRepo: repo}
	header := http.Header{}
	header.Set("x-codex-primary-used-percent", "100")
	header.Set("x-codex-primary-window-minutes", "180")
	header.Set("x-codex-primary-reset-after-seconds", "600")
	now := time.Now()

	svc.recordOpenAIImageNative429Exhaustion(account, header, now.Add(10*time.Minute), now)
	require.Eventually(t, func() bool {
		_, _, extra := repo.state()
		return len(openAIImageQuotaObservationsFromExtra(extra)) == 1
	}, 2*time.Second, 10*time.Millisecond)
	_, _, extra := repo.state()
	observation := openAIImageQuotaObservationsFromExtra(extra)[0]
	require.Equal(t, openAIImageObservationSource429, observation.Source)
	require.Equal(t, 180, observation.WindowMinutes)
	require.Equal(t, "plus", observation.PlanType)
	require.WithinDuration(t, now.Add(10*time.Minute-180*time.Minute), *observation.CountedFrom, time.Second)
}

func TestRecordOpenAIImageQuotaObservationsDedupesAndBoundsHistory(t *testing.T) {
	ctx := context.Background()
	account := &Account{ID: 53101, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "Pro Lite"}}
	repo := &imageQuotaStateRepo{current: account}
	now := time.Now().UTC().Truncate(time.Second)
	used := 100.0
	firstReset := now.Add(-2 * time.Hour)
	require.NoError(t, recordOpenAIImageQuotaObservations(ctx, repo, account.ID, []OpenAIImageQuotaObservation{
		{ObservedAt: now.Add(-3 * time.Hour), Source: openAIImageObservationSource429, ResetAt: &firstReset},
	}))

	observation := OpenAIImageQuotaObservation{ObservedAt: now, Source: openAIImageObservationSourceHeaders, ResetAt: timePointer(now.Add(time.Hour)), UsedPercent: &used}
	require.NoError(t, recordOpenAIImageQuotaObservations(ctx, repo, account.ID, []OpenAIImageQuotaObservation{observation}))
	duplicate := observation
	duplicate.Source, duplicate.ResetAt = openAIImageObservationSource429, timePointer(now.Add(time.Hour+30*time.Second))
	require.NoError(t, recordOpenAIImageQuotaObservations(ctx, repo, account.ID, []OpenAIImageQuotaObservation{duplicate}))

	_, _, extra := repo.state()
	stored := openAIImageQuotaObservationsFromExtra(extra)
	require.Len(t, stored, 2, "the same exhaustion seen by 429 and headers is recorded once")
	require.Equal(t, "prolite", stored[1].PlanType)
	require.True(t, firstReset.Equal(*stored[1].CountedFrom), "an unknown window counts from the previous exhaustion reset")

	windowed := OpenAIImageQuotaObservation{ObservedAt: now, Source: openAIImageObservationSourceHeaders, WindowMinutes: 180, ResetAt: timePointer(now.Add(5 * time.Hour))}
	require.NoError(t, recordOpenAIImageQuotaObservations(ctx, repo, account.ID, []OpenAIImageQuotaObservation{windowed}))
	_, _, extra = repo.state()
	stored = openAIImageQuotaObservationsFromExtra(extra)
	require.True(t, now.Add(2*time.Hour).Equal(*stored[len(stored)-1].CountedFrom), "a known window counts from reset minus the window")

	for i := 0; i < 30; i++ {
		require.NoError(t, recordOpenAIImageQuotaObservations(ctx, repo, account.ID, []OpenAIImageQuotaObservation{
			{ObservedAt: now, Source: openAIImageObservationSourceHeaders, WindowMinutes: 60, ResetAt: timePointer(now.Add(time.Duration(10+i) * time.Hour))},
		}))
	}
	_, _, extra = repo.state()
	require.Len(t, openAIImageQuotaObservationsFromExtra(extra), openAIImageQuotaObservationLimit)
}

func TestApplyOpenAIImagePoolUsageMatchesImagePoolOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	reset := now.Add(3 * time.Hour).Unix()
	spark := OpenAIAdditionalRateLimit{LimitName: "Codex Spark", MeteredFeature: "codex_bengalfox",
		RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 100, LimitWindowSeconds: 18000, ResetAt: reset}}}
	image := func(used float64, reached bool) OpenAIAdditionalRateLimit {
		return OpenAIAdditionalRateLimit{LimitName: "Image generation", MeteredFeature: "images",
			RateLimit: &OpenAIRateLimit{LimitReached: reached, PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: used, LimitWindowSeconds: 10800, ResetAt: reset}}}
	}
	newRepo := func(id int64) *imageQuotaStateRepo {
		return &imageQuotaStateRepo{current: &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "plus"}}}
	}

	exhausted := newRepo(53201)
	applyOpenAIImagePoolUsage(ctx, exhausted, 53201, &OpenAIQuotaUsage{AdditionalRateLimits: []OpenAIAdditionalRateLimit{spark, image(100, true)}}, now)
	cooldowns, until, extra := exhausted.state()
	require.Equal(t, []string{openAIImageQuotaReason}, cooldowns)
	require.True(t, time.Unix(reset, 0).Equal(until))
	require.Equal(t, "images", extra[codexImageUsageSnapshotKey].(map[string]any)["metered_feature"])
	observations := openAIImageQuotaObservationsFromExtra(extra)
	require.Len(t, observations, 1)
	require.Equal(t, 180, observations[0].WindowMinutes)
	require.Equal(t, openAIImageObservationSourceUsage, observations[0].Source)

	partial := newRepo(53202)
	applyOpenAIImagePoolUsage(ctx, partial, 53202, &OpenAIQuotaUsage{AdditionalRateLimits: []OpenAIAdditionalRateLimit{image(40, false)}}, now)
	cooldowns, _, extra = partial.state()
	require.Empty(t, cooldowns)
	require.Contains(t, extra, codexImageUsageSnapshotKey)
	require.Empty(t, openAIImageQuotaObservationsFromExtra(extra))

	sparkOnly := newRepo(53203)
	applyOpenAIImagePoolUsage(ctx, sparkOnly, 53203, &OpenAIQuotaUsage{AdditionalRateLimits: []OpenAIAdditionalRateLimit{spark}}, now)
	cooldowns, _, extra = sparkOnly.state()
	require.Empty(t, cooldowns, "the Spark pool must not be mistaken for the image pool")
	require.NotContains(t, extra, codexImageUsageSnapshotKey)
}

func TestEnforceOpenAIImagePlanLimitCoolsUntilOldestImageLeavesWindow(t *testing.T) {
	ctx := context.Background()
	settings := newImageQuotaSettingService(t, &OpenAIImageQuotaSettings{
		PauseThresholdPercent: 100,
		PlanLimits:            map[string][]OpenAIImagePlanLimitRule{"plus": {{WindowMinutes: 60, MaxImages: 2}}},
	})
	now := time.Now()
	oldest := now.Add(-20 * time.Minute)
	newAccount := func(plan string) *Account {
		return &Account{ID: 53301, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": plan}}
	}

	repo := &imageQuotaStateRepo{current: newAccount("plus")}
	counter := &imageWindowCounterStub{count: 1, earliest: &oldest}
	svc := &OpenAIGatewayService{accountRepo: repo, usageLogRepo: counter, settingService: settings}
	svc.enforceOpenAIImagePlanLimit(ctx, 53301, "Plus", "req-2", 1, now)
	cooldowns, until, _ := repo.state()
	require.Equal(t, []string{openAIImagePlanLimitReason}, cooldowns)
	require.WithinDuration(t, oldest.Add(time.Hour), until, time.Second)
	require.Equal(t, "req-2", counter.lastExclude, "the current request is counted separately")
	require.WithinDuration(t, now.Add(-time.Hour), counter.lastSince, time.Second)

	belowRepo := &imageQuotaStateRepo{current: newAccount("plus")}
	svc = &OpenAIGatewayService{accountRepo: belowRepo, usageLogRepo: &imageWindowCounterStub{}, settingService: settings}
	svc.enforceOpenAIImagePlanLimit(ctx, 53301, "plus", "req-1", 1, now)
	cooldowns, _, _ = belowRepo.state()
	require.Empty(t, cooldowns)

	unconfigured := &imageWindowCounterStub{count: 99}
	svc = &OpenAIGatewayService{accountRepo: &imageQuotaStateRepo{current: newAccount("go")}, usageLogRepo: unconfigured, settingService: settings}
	svc.enforceOpenAIImagePlanLimit(ctx, 53301, "go", "req-3", 1, now)
	require.Zero(t, unconfigured.sinceCalls, "plans without rules do not query usage logs")
}

func TestGetOpenAIImageQuotaStatsSummarizesPlans(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	observations := func(items ...OpenAIImageQuotaObservation) []OpenAIImageQuotaObservation { return items }
	exhausted := func(observedAgo time.Duration, window int) OpenAIImageQuotaObservation {
		observedAt := now.Add(-observedAgo)
		reset := observedAt.Add(30 * time.Minute)
		from := reset.Add(-time.Duration(window) * time.Minute)
		if window == 0 {
			from = observedAt.Add(-5 * time.Hour)
		}
		return OpenAIImageQuotaObservation{ObservedAt: observedAt, Source: openAIImageObservationSourceHeaders, WindowMinutes: window, ResetAt: &reset, CountedFrom: &from}
	}
	account := func(id int64, name, plan string, kind string, extra map[string]any) Account {
		return Account{ID: id, Name: name, Platform: PlatformOpenAI, Type: kind, Credentials: map[string]any{"plan_type": plan}, Extra: copyTestJSONMap(extra)}
	}
	collectedAt := now.Add(-10 * time.Minute)
	cooldownUntil := now.Add(time.Hour)
	repo := &imageQuotaStateRepo{listed: []Account{
		account(1, "plus-b", "plus", AccountTypeOAuth, map[string]any{
			codexImageQuotaObservationsKey: observations(exhausted(48*time.Hour, 180)),
			codexImageHeadersSnapshotKey: map[string]any{
				"primary_used_percent": 50.0, "primary_window_minutes": 180, "primary_reset_after_seconds": 3600,
				"collected_at": collectedAt.Format(time.RFC3339),
			},
		}),
		account(2, "plus-a", "Plus", AccountTypeOAuth, map[string]any{
			codexImageQuotaObservationsKey: observations(exhausted(72*time.Hour, 180), exhausted(24*time.Hour, 180)),
			modelRateLimitsKey: map[string]any{openAIImageGenerationRateLimitKey: map[string]any{
				"rate_limit_reset_at": cooldownUntil.Format(time.RFC3339), "reason": openAIImagePlanLimitReason,
			}},
		}),
		account(3, "pro100", "prolite", AccountTypeOAuth, map[string]any{codexImageQuotaObservationsKey: observations(exhausted(24*time.Hour, 0))}),
		account(4, "api-key", "plus", AccountTypeAPIKey, nil),
		account(5, "no-plan", "", AccountTypeOAuth, nil),
	}}
	counts := map[int64][]int64{1: {32}, 2: {30, 34}, 3: {80}}
	counter := &imageWindowCounterStub{rangeCount: func(item AccountImageCountRange) int64 {
		if item.To.Equal(collectedAt) {
			return 15 // 当前窗口统计到快照时刻
		}
		values := counts[item.AccountID]
		value := values[0]
		counts[item.AccountID] = values[1:]
		return value
	}}
	stats, err := (&AccountUsageService{accountRepo: repo, usageLogRepo: counter}).GetOpenAIImageQuotaStats(context.Background())
	require.NoError(t, err)

	require.Len(t, stats.Plans, 3)
	require.Equal(t, "plus", stats.Plans[0].PlanType)
	require.Equal(t, 2, stats.Plans[0].AccountCount)
	require.Equal(t, 3, stats.Plans[0].ObservationCount)
	require.Equal(t, []OpenAIImageQuotaWindowStats{{WindowMinutes: 180, Samples: 3, MinImages: 30, MedianImages: 32, MaxImages: 34}}, stats.Plans[0].Windows)
	require.Equal(t, "prolite", stats.Plans[1].PlanType)
	require.Equal(t, []OpenAIImageQuotaWindowStats{{WindowMinutes: 0, Samples: 1, MinImages: 80, MedianImages: 80, MaxImages: 80}}, stats.Plans[1].Windows)
	require.Equal(t, openAIImageQuotaUnknownPlanType, stats.Plans[2].PlanType)

	require.Len(t, stats.Accounts, 4, "API-key accounts are excluded")
	require.Equal(t, []string{"plus-a", "plus-b", "pro100", "no-plan"}, []string{stats.Accounts[0].Name, stats.Accounts[1].Name, stats.Accounts[2].Name, stats.Accounts[3].Name})
	require.Equal(t, openAIImagePlanLimitReason, stats.Accounts[0].ImageCooldownReason)
	current := stats.Accounts[1].CurrentWindow
	require.NotNil(t, current)
	require.Equal(t, int64(15), current.Images)
	require.Equal(t, int64(30), *current.EstimatedLimit)
	require.Equal(t, 180, current.WindowMinutes)
	require.Equal(t, openAIImageQuotaSourceHeadersSnapshot, current.Source)
}
