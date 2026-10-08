//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// mainPoolStateRepo 模拟生产仓储：常规候选查询按瞬时状态过滤，
// 模型可用性候选忽略瞬时状态；主池标记与限流列一起写入当前账号。
type mainPoolStateRepo struct {
	oauth429RateLimitRepo
	current            map[int64]*Account
	mainPoolCalls      int
	lastMainPoolReason string
}

func newMainPoolStateRepo(accounts ...*Account) *mainPoolStateRepo {
	repo := &mainPoolStateRepo{current: make(map[int64]*Account, len(accounts))}
	for _, account := range accounts {
		repo.current[account.ID] = account
	}
	return repo
}

func (r *mainPoolStateRepo) SetOpenAIMainPoolRateLimited(_ context.Context, id int64, resetAt time.Time, reason string) error {
	r.mainPoolCalls++
	r.lastMainPoolReason = reason
	if account := r.current[id]; account != nil {
		account.RateLimitResetAt = &resetAt
		if account.Extra == nil {
			account.Extra = map[string]any{}
		}
		account.Extra[OpenAIMainPoolRateLimitExtraKey] = map[string]any{"reset_at": resetAt.UTC().Format(time.RFC3339Nano), "reason": reason}
	}
	return nil
}

func (r *mainPoolStateRepo) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	_ = r.oauth429RateLimitRepo.SetRateLimited(ctx, id, resetAt)
	if account := r.current[id]; account != nil {
		account.RateLimitResetAt = &resetAt
	}
	return nil
}

func (r *mainPoolStateRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if account := r.current[id]; account != nil {
		clone := *account
		return &clone, nil
	}
	return nil, errors.New("account not found")
}

func (r *mainPoolStateRepo) list(platforms []string, includeTransient bool) []Account {
	now := time.Now()
	ids := make([]int64, 0, len(r.current))
	for id := range r.current {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]Account, 0, len(ids))
	for _, id := range ids {
		account := r.current[id]
		matched := false
		for _, platform := range platforms {
			matched = matched || account.Platform == platform
		}
		transient := (account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt)) ||
			(account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil)) ||
			(account.OverloadUntil != nil && now.Before(*account.OverloadUntil))
		if matched && (includeTransient || !transient) {
			out = append(out, *account)
		}
	}
	return out
}

func (r *mainPoolStateRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]Account, error) {
	return r.list([]string{platform}, false), nil
}

func (r *mainPoolStateRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]Account, error) {
	return r.list([]string{platform}, false), nil
}

func (r *mainPoolStateRepo) ListSchedulableUngroupedByPlatform(_ context.Context, platform string) ([]Account, error) {
	return r.list([]string{platform}, false), nil
}

func (r *mainPoolStateRepo) ListModelAvailabilityCandidates(_ context.Context, _ *int64, platforms []string, _ bool) ([]Account, error) {
	return r.list(platforms, true), nil
}

func mainPoolLimitedTestAccount(id int64, resetAt time.Time) *Account {
	return &Account{
		ID: id, Name: fmt.Sprintf("main-pool-%d", id), Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:      map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"},
		RateLimitResetAt: &resetAt,
		Extra: map[string]any{OpenAIMainPoolRateLimitExtraKey: map[string]any{
			"reset_at": resetAt.UTC().Format(time.RFC3339Nano), "reason": openAIMainPoolReasonCodexWindow,
		}},
	}
}

func nativeImageTestContext(model string) context.Context {
	return WithOpenAIImagesForwardModel(WithOpenAIImagesEndpoint(WithOpenAIImageGenerationIntent(context.Background())), model)
}

func useOpenAIImageMainPoolIsolation(t *testing.T, enabled bool) {
	t.Helper()
	SetOpenAIImageMainPoolIsolationEnabled(enabled)
	t.Cleanup(func() { SetOpenAIImageMainPoolIsolationEnabled(true) })
}

func exhaustedCodexWindowHeaders() http.Header {
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-primary-reset-after-seconds", "3600")
	return headers
}

func TestOpenAIMainPoolBlockedOnlyMatrix(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	threshold := func(platform, window, scope string) string {
		return BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
			Platform: platform, Window: window, Scope: scope, ThresholdPercent: 90, UsedPercent: 100, Until: reset, Now: now,
		})
	}
	at := func(d time.Duration) *time.Time {
		value := now.Add(d)
		return &value
	}
	parentID := int64(1)
	for _, tc := range []struct {
		name   string
		mutate func(*Account)
		want   bool
	}{
		{"marker covers rate limit", func(*Account) {}, true},
		{"marker within tolerance", func(a *Account) { a.RateLimitResetAt = at(time.Hour + 500*time.Millisecond) }, true},
		{"rate limit rewritten by another cause", func(a *Account) { a.RateLimitResetAt = at(2 * time.Hour) }, false},
		{"rate limit without marker", func(a *Account) { a.Extra = nil }, false},
		{"rate limit expired", func(a *Account) { a.RateLimitResetAt = at(-time.Minute) }, false},
		{"overload also active", func(a *Account) { a.OverloadUntil = at(time.Minute) }, false},
		{"main pool threshold pause", func(a *Account) {
			a.RateLimitResetAt, a.Extra = nil, nil
			a.TempUnschedulableUntil, a.TempUnschedulableReason = at(time.Hour), threshold(PlatformOpenAI, "5h", "")
		}, true},
		{"threshold pause plus marker", func(a *Account) {
			a.TempUnschedulableUntil, a.TempUnschedulableReason = at(time.Hour), threshold(PlatformOpenAI, "7d", "")
		}, true},
		{"credential failure", func(a *Account) {
			a.TempUnschedulableUntil, a.TempUnschedulableReason = at(time.Hour), "401 unauthorized"
		}, false},
		{"anthropic threshold reason", func(a *Account) {
			a.RateLimitResetAt, a.Extra = nil, nil
			a.TempUnschedulableUntil, a.TempUnschedulableReason = at(time.Hour), threshold(PlatformAnthropic, "5h", "")
		}, false},
		{"scoped threshold reason", func(a *Account) {
			a.RateLimitResetAt, a.Extra = nil, nil
			a.TempUnschedulableUntil, a.TempUnschedulableReason = at(time.Hour), threshold(PlatformOpenAI, "7d", "spark")
		}, false},
		{"api key account", func(a *Account) { a.Type = AccountTypeAPIKey }, false},
		{"spark shadow", func(a *Account) { a.ParentAccountID = &parentID }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := mainPoolLimitedTestAccount(52001, reset)
			tc.mutate(account)
			require.Equal(t, tc.want, account.openAIMainPoolBlockedOnly(now))
		})
	}
}

func TestOpenAIImageMainPoolBypassOnlyForNativeImageRequests(t *testing.T) {
	useOpenAIImageMainPoolIsolation(t, true)
	reset := time.Now().Add(time.Hour)
	limited := mainPoolLimitedTestAccount(52101, reset)
	require.False(t, limited.IsSchedulable())

	native := nativeImageTestContext("gpt-image-2")
	require.True(t, openAIAccountSchedulable(native, limited))
	require.True(t, openAIAccountSchedulableForModel(native, limited, "gpt-image-2"))
	for name, ctx := range map[string]context.Context{
		"text":                   context.Background(),
		"responses image intent": WithOpenAIImageGenerationIntent(context.Background()),
		"missing forward model":  WithOpenAIImagesEndpoint(context.Background()),
		"luna image model":       nativeImageTestContext("gpt-image-1"),
	} {
		require.False(t, openAIAccountSchedulable(ctx, limited), name)
	}

	mapped := mainPoolLimitedTestAccount(52102, reset)
	mapped.Credentials["model_mapping"] = map[string]any{"gpt-image-2": "gpt-image-1"}
	require.False(t, openAIAccountSchedulable(native, mapped), "account mapping routes the request to the Luna path")

	cooled := mainPoolLimitedTestAccount(52103, reset)
	setAccountModelRateLimitSnapshot(cooled, openAIImageGenerationRateLimitKey, reset, "quota", time.Now())
	require.True(t, openAIAccountSchedulable(native, cooled))
	require.False(t, openAIAccountSchedulableForModel(native, cooled, "gpt-image-2"), "the image cooldown still applies")
	require.True(t, shouldClearOpenAIStickySession(native, cooled, "gpt-image-2"))
	require.False(t, shouldClearOpenAIStickySession(native, limited, "gpt-image-2"))

	paused := mainPoolLimitedTestAccount(52104, reset)
	paused.Schedulable = false
	require.False(t, openAIAccountSchedulable(native, paused), "a manual pause is not a main-pool block")

	useOpenAIImageMainPoolIsolation(t, false)
	require.False(t, openAIAccountSchedulable(native, limited))
}

func TestNativeImageRequestsIgnoreMainPoolQuotaThresholds(t *testing.T) {
	useOpenAIImageMainPoolIsolation(t, true)
	reset := time.Now().Add(time.Hour).UTC()
	newAccount := func() *Account {
		return &Account{ID: 52201, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
			Extra: map[string]any{
				"codex_5h_used_percent":   100.0,
				"codex_5h_reset_at":       reset.Format(time.RFC3339),
				"codex_usage_updated_at":  time.Now().UTC().Format(time.RFC3339),
				"auto_pause_5h_threshold": 0.95,
			}}
	}
	paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), newAccount())
	require.True(t, paused)
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(nativeImageTestContext("gpt-image-2"), newAccount())
	require.False(t, paused)

	resetThresholds := func() {
		accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
		accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	}
	resetThresholds()
	t.Cleanup(resetThresholds)
	settings := newMockSettingRepo()
	settings.data[SettingKeyAccountSchedulingThresholds] = `{"openai":90}`
	repo := &rateLimitAccountRepoStub{}
	limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	limits.SetSettingService(NewSettingService(settings, &config.Config{}))
	svc := &OpenAIGatewayService{rateLimitService: limits}

	require.False(t, svc.isOpenAIAccountBlockedBySchedulingThreshold(nativeImageTestContext("gpt-image-2"), newAccount()))
	require.Zero(t, repo.tempCalls, "image requests must not park the account for text")
	require.True(t, svc.isOpenAIAccountBlockedBySchedulingThreshold(context.Background(), newAccount()))
	require.Equal(t, 1, repo.tempCalls)
}

func TestSelectAccountWithSchedulerForImagesUsesMainPoolLimitedAccount(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("advanced=%t", advanced), func(t *testing.T) {
			useOpenAIImageMainPoolIsolation(t, true)
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			groupID := int64(52300)
			limited := mainPoolLimitedTestAccount(52301, time.Now().Add(time.Hour))
			limited.GroupIDs = []int64{groupID}
			svc := &OpenAIGatewayService{
				accountRepo:        newMainPoolStateRepo(limited),
				cache:              &schedulerTestGatewayCache{sessionBindings: map[string]int64{}},
				cfg:                &config.Config{},
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			if advanced {
				svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
			}
			svc.BlockAccountScheduling(limited, *limited.RateLimitResetAt, "429")

			selection, _, err := svc.SelectAccountWithSchedulerForImages(nativeImageTestContext("gpt-image-2"), &groupID, "", "gpt-image-2", nil, OpenAIImagesCapabilityBasic)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, limited.ID, selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			require.True(t, svc.isOpenAIAccountRuntimeBlocked(limited), "the shared account block still guards text")

			_, _, err = svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.4", nil, OpenAIUpstreamTransportAny, false)
			require.Error(t, err)
			_, _, err = svc.SelectAccountWithSchedulerForImages(nativeImageTestContext("gpt-image-1"), &groupID, "", "gpt-image-1", nil, OpenAIImagesCapabilityBasic)
			require.Error(t, err, "the Luna path consumes the main pool")

			useOpenAIImageMainPoolIsolation(t, false)
			_, _, err = svc.SelectAccountWithSchedulerForImages(nativeImageTestContext("gpt-image-2"), &groupID, "", "gpt-image-2", nil, OpenAIImagesCapabilityBasic)
			require.Error(t, err)
		})
	}
}

func TestHandle429WritesMainPoolMarkerOnlyForMainPoolSignals(t *testing.T) {
	usageLimit := []byte(fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, time.Now().Add(time.Hour).Unix()))
	shortLimit := []byte(`{"error":{"type":"rate_limit_exceeded","resets_in_seconds":30}}`)
	for _, tc := range []struct {
		name      string
		kind      string
		headers   http.Header
		body      []byte
		isolation bool
		reason    string
	}{
		{"exhausted window", AccountTypeOAuth, exhaustedCodexWindowHeaders(), nil, true, openAIMainPoolReasonCodexWindow},
		{"usage limit body", AccountTypeOAuth, nil, usageLimit, true, openAIMainPoolReasonUsageLimit},
		{"short rate limit", AccountTypeOAuth, nil, shortLimit, true, ""},
		{"api key account", AccountTypeAPIKey, exhaustedCodexWindowHeaders(), nil, true, ""},
		{"isolation disabled", AccountTypeOAuth, exhaustedCodexWindowHeaders(), nil, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useOpenAIImageMainPoolIsolation(t, tc.isolation)
			account := &Account{ID: 52401, Platform: PlatformOpenAI, Type: tc.kind, Status: StatusActive, Schedulable: true}
			repo := newMainPoolStateRepo(account)
			NewRateLimitService(repo, nil, &config.Config{}, nil, nil).handle429(context.Background(), account, tc.headers, tc.body)
			if tc.reason != "" {
				require.Equal(t, 1, repo.mainPoolCalls)
				require.Equal(t, tc.reason, repo.lastMainPoolReason)
				require.Zero(t, repo.setRateLimitedCalls)
				require.True(t, account.openAIMainPoolBlockedOnly(time.Now()))
				return
			}
			require.Zero(t, repo.mainPoolCalls)
			require.Equal(t, 1, repo.setRateLimitedCalls)
			require.False(t, account.openAIMainPoolBlockedOnly(time.Now()))
		})
	}
}

func TestImagesLunaMainPool429BlocksAccountButNativeStaysImageScoped(t *testing.T) {
	useOpenAIImageMainPoolIsolation(t, true)
	for _, tc := range []struct {
		name         string
		direct       bool
		body         string
		accountLevel bool
	}{
		{"luna main pool", false, `{"error":{"type":"usage_limit_reached"}}`, true},
		{"native image pool", true, `{"error":{"type":"usage_limit_reached"}}`, false},
		{"luna image marker", false, `{"error":{"message":"Rate limit reached for gpt-image-2-codex"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 52501, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
			repo := newMainPoolStateRepo(account)
			limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc := &OpenAIGatewayService{rateLimitService: limits}
			limits.SetAccountRuntimeBlocker(svc)
			ctx := withOpenAIImagesUpstreamDirect(withOpenAIImagesSelfBuiltRequest(WithOpenAIImagesEndpoint(context.Background())), tc.direct)
			svc.handleOpenAIAccountUpstreamError(ctx, account, http.StatusTooManyRequests, exhaustedCodexWindowHeaders(), []byte(tc.body), "gpt-image-1")
			if tc.accountLevel {
				require.Equal(t, 1, repo.mainPoolCalls)
				require.Zero(t, repo.setModelRateLimitCalls)
				require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
				return
			}
			require.Zero(t, repo.mainPoolCalls)
			require.Zero(t, repo.setRateLimitedCalls)
			require.Equal(t, 1, repo.setModelRateLimitCalls)
			require.Equal(t, openAIImageGenerationRateLimitKey, repo.lastModelRateLimitKey)
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
		})
	}
}

func TestForwardImagesLunaMainPool429WritesAccountLevelLimit(t *testing.T) {
	useOpenAIImageMainPoolIsolation(t, true)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusTooManyRequests, Header: exhaustedCodexWindowHeaders(),
		Body: io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`))}}
	account := directImagesTestAccount()
	account.Status, account.Schedulable = StatusActive, true
	repo := newMainPoolStateRepo(account)
	svc := newOpenAIImagesTestService(upstream)
	svc.accountRepo = repo
	svc.rateLimitService = NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.rateLimitService.SetAccountRuntimeBlocker(svc)
	body := []byte(`{"model":"gpt-image-1","prompt":"draw"}`)
	c, _ := newOpenAIImagesTestContext(t, body)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)

	_, err = svc.ForwardImages(WithOpenAIImagesEndpoint(context.Background()), c, account, body, parsed, "")
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
	require.Equal(t, 1, repo.mainPoolCalls)
	require.Zero(t, repo.setModelRateLimitCalls, "a main-pool 429 must not cool native images")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestMainPool429ThenNativeImageSucceedsOnSameAccount(t *testing.T) {
	useOpenAIImageMainPoolIsolation(t, true)
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	account := directImagesTestAccount()
	account.Status, account.Schedulable, account.Concurrency = StatusActive, true, 1
	repo := newMainPoolStateRepo(account)
	upstream := &httpUpstreamRecorder{resp: openAIImagesJSONResponse()}
	svc := newOpenAIImagesTestService(upstream)
	svc.accountRepo = repo
	svc.cache = &schedulerTestGatewayCache{sessionBindings: map[string]int64{}}
	svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{})
	svc.rateLimitService = NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.rateLimitService.SetAccountRuntimeBlocker(svc)

	svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, exhaustedCodexWindowHeaders(), []byte(`{"error":{"type":"usage_limit_reached"}}`), "gpt-5.4")
	require.Equal(t, 1, repo.mainPoolCalls)
	require.NotNil(t, account.RateLimitResetAt)
	resetAt := *account.RateLimitResetAt

	ctx := nativeImageTestContext("gpt-image-2")
	selection, _, err := svc.SelectAccountWithSchedulerForImages(ctx, nil, "", "gpt-image-2", nil, OpenAIImagesCapabilityBasic)
	require.NoError(t, err)
	require.Equal(t, account.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}

	body := []byte(`{"model":"gpt-image-2","prompt":"draw"}`)
	c, _ := newOpenAIImagesTestContext(t, body)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	result, err := svc.ForwardImages(ctx, c, selection.Account, body, parsed, "gpt-image-2")
	require.NoError(t, err)
	require.Equal(t, 1, result.ImageCount)
	require.Equal(t, "/backend-api/codex/images/generations", upstream.lastReq.URL.Path)
	require.Equal(t, resetAt, *repo.current[account.ID].RateLimitResetAt, "the main-pool limit stays for text")
	require.True(t, repo.current[account.ID].openAIMainPoolBlockedOnly(time.Now()))
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestRecoverAfterNativeImageTestKeepsMainPoolBlock(t *testing.T) {
	useOpenAIImageMainPoolIsolation(t, true)
	limited := mainPoolLimitedTestAccount(52701, time.Now().Add(time.Hour))
	repo := &rateLimitClearRepoStub{getByIDAccount: limited}
	limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

	result, err := limits.RecoverAccountAfterSuccessfulModelTest(context.Background(), limited.ID, "gpt-image-2")
	require.NoError(t, err)
	require.False(t, result.ClearedRateLimit)
	require.Zero(t, repo.clearRateLimitCalls)

	result, err = limits.RecoverAccountAfterSuccessfulModelTest(context.Background(), limited.ID, "gpt-5.4")
	require.NoError(t, err)
	require.True(t, result.ClearedRateLimit)
	require.Equal(t, 1, repo.clearRateLimitCalls)
}
