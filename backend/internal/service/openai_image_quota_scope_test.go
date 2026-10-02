//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestImageScopedRateLimitMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, platform, kind string
		ctx                  context.Context
		body                 string
		status               int
		want                 bool
	}{
		{"dedicated OAuth", PlatformOpenAI, AccountTypeOAuth, WithOpenAIImagesEndpoint(nil), `{"error":{"type":"usage_limit_reached"}}`, 429, true},
		{"self built", PlatformOpenAI, AccountTypeOAuth, withOpenAIImagesSelfBuiltRequest(context.Background()), `{}`, 429, true},
		{"text OAuth", PlatformOpenAI, AccountTypeOAuth, context.Background(), `{"error":{"type":"usage_limit_reached"}}`, 429, false},
		{"intent only", PlatformOpenAI, AccountTypeOAuth, WithOpenAIImageGenerationIntent(nil), `{}`, 429, false},
		{"Grok images context", PlatformGrok, AccountTypeOAuth, WithOpenAIImagesEndpoint(nil), `{}`, 429, false},
		{"API key without marker", PlatformOpenAI, AccountTypeAPIKey, WithOpenAIImagesEndpoint(nil), `{}`, 429, false},
		{"legacy marker", PlatformOpenAI, AccountTypeAPIKey, context.Background(), `gpt-image rate limited`, 429, true},
		{"not 429", PlatformOpenAI, AccountTypeOAuth, WithOpenAIImagesEndpoint(nil), `gpt-image`, 502, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isOpenAIImageScopedRateLimit(tc.ctx, &Account{Platform: tc.platform, Type: tc.kind}, tc.status, []byte(tc.body)))
		})
	}
}

func TestImageScopedCooldownPrecedence(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("Retry-After", "20")
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "600")
	for _, tc := range []struct {
		body    string
		headers http.Header
		want    time.Duration
	}{
		{fmt.Sprintf(`{"detail":{"resets_at":%d,"type":"unusual"},"message":"try again in 2s"}`, now.Add(time.Hour).Unix()), headers, time.Hour},
		{`{"resets_in_seconds":"42"}`, headers, 42 * time.Second},
		{`{"error":{"resets_at":1,"message":"Please try again in 2s."}}`, headers, 2 * time.Second},
		{`{}`, headers, 20 * time.Second},
		{`{"error":{"type":"usage_limit_reached"}}`, nil, 30 * time.Minute},
		{`{"error":{"type":"rate_limit_error"}}`, nil, time.Minute},
	} {
		reset, _ := openAIImageScopedCooldown(tc.headers, []byte(tc.body), now)
		require.True(t, now.Add(tc.want).Equal(reset), "reset=%s want=%s", reset, now.Add(tc.want))
	}
	headers.Del("Retry-After")
	reset, _ := openAIImageScopedCooldown(headers, nil, now)
	require.Equal(t, now.Add(10*time.Minute), reset)
	headers.Set("x-codex-primary-used-percent", "99")
	reset, _ = openAIImageScopedCooldown(headers, nil, now)
	require.Equal(t, now.Add(time.Minute), reset)
}

func TestImageScoped429FastPathDoesNotBlockText(t *testing.T) {
	for _, imageScope := range []bool{true, false} {
		t.Run(fmt.Sprint(imageScope), func(t *testing.T) {
			repo := &oauth429RateLimitRepo{}
			limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc := &OpenAIGatewayService{rateLimitService: limits}
			limits.SetAccountRuntimeBlocker(svc)
			a := &Account{ID: 7412, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			body := []byte(fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, time.Now().Add(time.Hour).Unix()))
			ctx := context.Background()
			if imageScope {
				ctx = WithOpenAIImagesEndpoint(ctx)
			}
			svc.handleOpenAIAccountUpstreamError(ctx, a, 429, nil, body)
			if imageScope {
				require.Zero(t, repo.setRateLimitedCalls)
				require.Equal(t, 1, repo.setModelRateLimitCalls)
				require.Equal(t, openAIImageGenerationRateLimitKey, repo.lastModelRateLimitKey)
				require.False(t, svc.isOpenAIAccountRuntimeBlocked(a))
				_, exists := svc.openaiOAuth429RetryStartedAt.Load(a.ID)
				require.False(t, exists)
			} else {
				require.Equal(t, 1, repo.setRateLimitedCalls)
				require.Zero(t, repo.setModelRateLimitCalls)
				require.True(t, svc.isOpenAIAccountRuntimeBlocked(a))
			}
		})
	}
}

func TestImageScopedCooldownDoesNotShortenExisting(t *testing.T) {
	later := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	current := &Account{ID: 7420, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{modelRateLimitsKey: map[string]any{openAIImageGenerationRateLimitKey: map[string]any{"rate_limit_reset_at": later.Format(time.RFC3339)}}}}
	repo := &imageCooldownCurrentRepo{current: current}
	svc := &RateLimitService{accountRepo: repo}
	stale := &Account{ID: 7420, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.True(t, svc.HandleOpenAIImageScopedRateLimit(WithOpenAIImagesEndpoint(nil), stale, 429, nil, []byte(`{"error":{"type":"usage_limit_reached"}}`)))
	require.Equal(t, later, *repo.current.modelRateLimitResetAt(openAIImageGenerationRateLimitKey))
	require.Equal(t, 1, repo.setModelRateLimitCalls, "the repository arbitrates even with an old snapshot")
}

func TestForwardImagesImageScopedLongCooldownSkipsRetryWindow(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2","prompt":"draw"}`)
	c, _ := newOpenAIImagesTestContext(t, body)
	repo := &oauth429RateLimitRepo{}
	svc := newOpenAIImagesTestService(&httpUpstreamRecorder{resp: &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached"}}`))}})
	svc.rateLimitService = NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.rateLimitService.SetAccountRuntimeBlocker(svc)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	a := directImagesTestAccount()
	_, err = svc.ForwardImages(context.Background(), c, a, body, parsed, "")
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, 429, failover.StatusCode)
	require.False(t, failover.RetryableOnSameAccount)
	require.True(t, failover.SameAccountRetryDeadline.IsZero())
	_, exists := svc.openaiOAuth429RetryStartedAt.Load(a.ID)
	require.False(t, exists)
	require.Zero(t, repo.setRateLimitedCalls)
	require.Equal(t, openAIImageGenerationRateLimitKey, repo.lastModelRateLimitKey)
}

func TestImagesInBandQuotaKeepsResponseSemantics(t *testing.T) {
	for _, status := range []int{429, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			repo := &oauth429RateLimitRepo{}
			svc := &OpenAIGatewayService{accountRepo: repo}
			a := directImagesTestAccount()
			upstreamErr := &OpenAIImagesUpstreamError{StatusCode: status, Code: "usage_limit_reached", Message: "usage_limit_reached"}
			c, _ := newOpenAIImagesTestContext(t, []byte(`{}`))
			err := svc.handleOpenAIImagesOAuthResponseError(WithOpenAIImagesEndpoint(nil), c, a, "gpt-image-2", "", nil, OpenAIImagesJSONKeepaliveAdjustedWrittenSize(c), upstreamErr)
			require.Equal(t, openAIImageGenerationRateLimitKey, repo.lastModelRateLimitKey)
			require.Zero(t, repo.setRateLimitedCalls)
			require.WithinDuration(t, time.Now().Add(30*time.Minute), repo.lastModelRateLimitedUntil, time.Second)
			if status == 429 {
				require.ErrorIs(t, err, upstreamErr)
			} else {
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, 502, failover.StatusCode)
			}
		})
	}
}

func TestImagesSnapshotSeparateFromCodexUsageSnapshot(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{updateExtraCh: make(chan map[string]any, 5)}
	svc := &OpenAIGatewayService{accountRepo: repo, codexSnapshotThrottle: newAccountWriteThrottle(time.Minute)}
	a := &Account{ID: 743023, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	t.Cleanup(func() {
		openAIImagesSnapshotThrottle.mu.Lock()
		delete(openAIImagesSnapshotThrottle.lastByID, a.ID)
		openAIImagesSnapshotThrottle.mu.Unlock()
	})
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-primary-reset-after-seconds", "1800")
	result := &OpenAIForwardResult{UpstreamEndpoint: "/backend-api/codex/images/generations", ResponseHeaders: headers}
	svc.RecordOpenAIImagesResponseSnapshot(context.Background(), a, result)
	require.Len(t, repo.updateExtraCh, 1)
	updates := <-repo.updateExtraCh
	require.Len(t, updates, 1)
	snapshot := updates[codexImageHeadersSnapshotKey].(map[string]any)
	require.Equal(t, 200, snapshot["status_code"])
	require.Equal(t, result.UpstreamEndpoint, snapshot["endpoint"])
	svc.RecordOpenAIImagesResponseSnapshot(context.Background(), a, result)
	require.Empty(t, repo.updateExtraCh)
	result.UpstreamEndpoint = "/backend-api/codex/responses"
	svc.RecordOpenAIImagesResponseSnapshot(context.Background(), a, result)
	select {
	case updates = <-repo.updateExtraCh:
		require.Contains(t, updates, "codex_5h_used_percent")
		require.NotContains(t, updates, codexImageHeadersSnapshotKey)
	case <-time.After(2 * time.Second):
		t.Fatal("general snapshot was suppressed by image throttle")
	}
}

// An in-flight request can carry an older snapshot than the stored cooldown.
type imageCooldownCurrentRepo struct {
	oauth429RateLimitRepo
	current *Account
}

func (r *imageCooldownCurrentRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.current, nil
}

func TestImageScopedFailoverUsesPersistedCooldown(t *testing.T) {
	repo := &imageCooldownCurrentRepo{current: &Account{ID: 99123, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{modelRateLimitsKey: map[string]any{openAIImageGenerationRateLimitKey: map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}}}}
	svc := &OpenAIGatewayService{accountRepo: repo}
	stale := &Account{ID: 99123, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	err := svc.newOpenAIImagesAccountFailoverError(WithOpenAIImagesEndpoint(nil), "/codex/images/generations", stale, 429, http.Header{"Retry-After": []string{"1"}}, []byte(`{"error":{"type":"rate_limit_error"}}`), "rate limited", false, true)
	require.False(t, err.RetryableOnSameAccount)
	require.True(t, err.SameAccountRetryDeadline.IsZero())
	_, exists := svc.openaiOAuth429RetryStartedAt.Load(stale.ID)
	require.False(t, exists)
	require.Nil(t, stale.Extra, "do not mutate a shared request account snapshot")
}

func (r *imageCooldownCurrentRepo) SetModelRateLimit(ctx context.Context, id int64, scope string, reset time.Time, reason ...string) error {
	if err := r.oauth429RateLimitRepo.SetModelRateLimit(ctx, id, scope, reset, reason...); err != nil {
		return err
	}
	if r.current == nil {
		r.current = &Account{ID: id}
	}
	previous := r.current.modelRateLimitResetAt(scope)
	if previous == nil || reset.After(*previous) {
		setAccountModelRateLimitSnapshot(r.current, scope, reset, "quota", time.Now())
	}
	return nil
}

func TestImageScoped429ThenTextResponseSucceeds(t *testing.T) {
	account := directImagesTestAccount()
	account.Status, account.Schedulable = StatusActive, true
	current := *account
	repo := &imageCooldownCurrentRepo{current: &current}
	completed := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\ndata: [DONE]\n\n"
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached"}}`))},
		{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(completed))},
	}}
	svc := newOpenAIImagesTestService(upstream)
	svc.accountRepo = repo
	svc.rateLimitService = NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.rateLimitService.SetAccountRuntimeBlocker(svc)
	imageBody := []byte(`{"model":"gpt-image-2","prompt":"draw"}`)
	imageContext, _ := newOpenAIImagesTestContext(t, imageBody)
	parsed, err := svc.ParseOpenAIImagesRequest(imageContext, imageBody)
	require.NoError(t, err)
	_, err = svc.ForwardImages(context.Background(), imageContext, account, imageBody, parsed, "")
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.False(t, failover.RetryableOnSameAccount)
	require.True(t, repo.current.IsSchedulable())
	require.Nil(t, repo.current.RateLimitResetAt)
	require.NotNil(t, repo.current.modelRateLimitResetAt(openAIImageGenerationRateLimitKey))
	textBody := []byte(`{"model":"gpt-5.4","stream":true,"instructions":"Reply OK","input":"hello"}`)
	textContext, response := newOpenAIAstraProRetryContext(textBody)
	result, err := svc.Forward(context.Background(), textContext, repo.current, textBody)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), "OK")
	require.Zero(t, repo.setRateLimitedCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(repo.current))
}
