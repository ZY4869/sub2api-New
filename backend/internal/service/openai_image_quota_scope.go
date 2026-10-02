package service

// [local] 专用生图端点的额度与通用 Codex 额度隔离。
import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const openAIImageQuotaReason = "openai_image_quota_exhausted"
const codexImageHeadersSnapshotKey = "codex_image_headers_snapshot"

var openAIImagesSnapshotThrottle = newAccountWriteThrottle(time.Minute)

func isOpenAIImageScopedRateLimit(ctx context.Context, account *Account, status int, body []byte) bool {
	if status != http.StatusTooManyRequests {
		return false
	}
	if isOpenAIImageRateLimitError(status, body) {
		return true
	}
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth &&
		(OpenAIImagesEndpointFromContext(ctx) || isOpenAIImagesSelfBuiltRequest(ctx))
}

func isOpenAIImageQuotaBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "usage_limit") || strings.Contains(lower, "quota")
}

func openAIImageScopedCooldown(headers http.Header, body []byte, now time.Time) (time.Time, string) {
	reason, fallback := openAIImageRateLimitReason, time.Minute
	if isOpenAIImageQuotaBody(body) {
		reason, fallback = openAIImageQuotaReason, 30*time.Minute
	}
	// 不按 error.type 过滤：Codex 的 detail 和顶层也可能带重置时间。
	for _, prefix := range []string{"error.", "detail.", ""} {
		for _, key := range []string{"resets_at", "reset_at"} {
			value := gjson.GetBytes(body, prefix+key)
			if reset := time.Unix(value.Int(), 0); value.Exists() && reset.After(now) {
				return reset, reason
			}
			if reset, err := time.Parse(time.RFC3339, value.String()); err == nil && reset.After(now) {
				return reset, reason
			}
		}
		if seconds := gjson.GetBytes(body, prefix+"resets_in_seconds").Int(); seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
			return now.Add(time.Duration(seconds) * time.Second), reason
		}
	}
	if delay := parseOpenAIImageTryAgainCooldown(body); delay > 0 {
		return now.Add(delay), reason
	}
	if reset := parseRetryAfterResetTime(headers, now); reset != nil && reset.After(now) {
		return *reset, reason
	}
	if snapshot := ParseCodexRateLimitHeaders(headers); snapshot != nil {
		var latest time.Time
		for _, window := range []struct {
			used  *float64
			reset *int
		}{
			{snapshot.PrimaryUsedPercent, snapshot.PrimaryResetAfterSeconds},
			{snapshot.SecondaryUsedPercent, snapshot.SecondaryResetAfterSeconds},
		} {
			if window.used != nil && *window.used >= 100 && window.reset != nil && *window.reset > 0 && int64(*window.reset) <= int64((1<<63-1)/time.Second) {
				if reset := now.Add(time.Duration(*window.reset) * time.Second); reset.After(latest) {
					latest = reset
				}
			}
		}
		if latest.After(now) {
			return latest, reason
		}
	}
	return now.Add(fallback), reason
}

func (s *RateLimitService) HandleOpenAIImageScopedRateLimit(ctx context.Context, account *Account, status int, headers http.Header, body []byte) bool {
	if s == nil || s.accountRepo == nil || account == nil || account.Platform != PlatformOpenAI ||
		!account.ShouldHandleErrorCode(status) || !isOpenAIImageScopedRateLimit(ctx, account, status, body) {
		return false
	}
	reset, reason := openAIImageScopedCooldown(headers, body, time.Now())
	persistOpenAIImageCooldown(ctx, s.accountRepo, account, reset, reason)
	return true
}

func persistOpenAIImageCooldown(ctx context.Context, repo AccountRepository, account *Account, reset time.Time, reason string) {
	if err := repo.SetModelRateLimit(ctx, account.ID, openAIImageGenerationRateLimitKey, reset, reason); err != nil {
		slog.Warn("openai_image_rate_limit_set_model_rate_limit_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("openai_image_rate_limited", "account_id", account.ID, "scope", openAIImageGenerationRateLimitKey, "reason", reason, "requested_reset_at", reset)
}

func (s *OpenAIGatewayService) newOpenAIImagesAccountFailoverError(ctx context.Context, endpoint string, account *Account, status int, headers http.Header, body []byte, message string, disabled, retrySame bool) *UpstreamFailoverError {
	if strings.Contains(endpoint, "/codex/images/") {
		s.recordOpenAIImagesHeadersSnapshot(ctx, account, endpoint, status, headers)
	}
	if isOpenAIImageScopedRateLimit(ctx, account, status, body) {
		now := time.Now()
		reset, _ := openAIImageScopedCooldown(headers, body, now)
		if existing := account.modelRateLimitResetAt(openAIImageGenerationRateLimitKey); existing != nil && existing.After(reset) {
			reset = *existing
		}
		// The request snapshot may predate another response's longer cooldown.
		if s != nil && s.accountRepo != nil && account != nil {
			stateCtx, cancel := openAIAccountStateContext(ctx)
			current, err := s.accountRepo.GetByID(stateCtx, account.ID)
			cancel()
			if err != nil {
				slog.Warn("openai_image_cooldown_read_failed", "account_id", account.ID, "error", err)
			} else if current != nil {
				if persisted := current.modelRateLimitResetAt(openAIImageGenerationRateLimitKey); persisted != nil && persisted.After(reset) {
					reset = *persisted
				}
			}
		}
		if reset.Sub(now) > openAIOAuth429MaxRetryDelay {
			// 直接构造以免创建文本共享的 OAuth 重试窗口；换号计入正常预算。
			return newOpenAIUpstreamFailoverError(status, headers, body, message, false)
		}
	}
	return s.newOpenAIAccountFailoverError(account, status, headers, body, message, disabled, retrySame)
}

func (s *OpenAIGatewayService) coolOpenAIImagesInBandQuota(ctx context.Context, account *Account, upstreamErr *OpenAIImagesUpstreamError, headers http.Header) {
	if s == nil || s.accountRepo == nil || account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || upstreamErr == nil || upstreamErr.SynthesizedFromModelText || !account.ShouldHandleErrorCode(upstreamErr.StatusCode) {
		return
	}
	body := openAIImagesUpstreamErrorResponseBody(upstreamErr)
	if upstreamErr.StatusCode != http.StatusTooManyRequests && !isOpenAIImageQuotaBody(body) {
		return
	}
	reset, reason := openAIImageScopedCooldown(headers, body, time.Now())
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	persistOpenAIImageCooldown(stateCtx, s.accountRepo, account, reset, reason)
}

// RecordOpenAIImagesResponseSnapshot 按实际上游端点归属响应头；Luna 回退仍属于通用池。
func (s *OpenAIGatewayService) RecordOpenAIImagesResponseSnapshot(ctx context.Context, account *Account, result *OpenAIForwardResult) {
	if s == nil || account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.IsShadow() || result == nil {
		return
	}
	if strings.Contains(result.UpstreamEndpoint, "/codex/images/") {
		s.recordOpenAIImagesHeadersSnapshot(ctx, account, result.UpstreamEndpoint, http.StatusOK, result.ResponseHeaders)
		return
	}
	s.UpdateCodexUsageSnapshotFromHeaders(ctx, account.ID, result.ResponseHeaders)
}

func (s *OpenAIGatewayService) recordOpenAIImagesHeadersSnapshot(ctx context.Context, account *Account, endpoint string, status int, headers http.Header) {
	if s == nil || s.accountRepo == nil || account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.IsShadow() {
		return
	}
	snapshot := ParseCodexRateLimitHeaders(headers)
	now := time.Now()
	if snapshot == nil || !openAIImagesSnapshotThrottle.Allow(account.ID, now) {
		return
	}
	// 不写 codex_5h/7d_*，不通知自动重置；独立节流也不抑制通用快照。
	value := map[string]any{
		"primary_used_percent":          snapshot.PrimaryUsedPercent,
		"primary_reset_after_seconds":   snapshot.PrimaryResetAfterSeconds,
		"primary_window_minutes":        snapshot.PrimaryWindowMinutes,
		"secondary_used_percent":        snapshot.SecondaryUsedPercent,
		"secondary_reset_after_seconds": snapshot.SecondaryResetAfterSeconds,
		"secondary_window_minutes":      snapshot.SecondaryWindowMinutes,
		"status_code":                   status, "endpoint": endpoint, "collected_at": now.UTC().Format(time.RFC3339),
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(stateCtx, account.ID, map[string]any{codexImageHeadersSnapshotKey: value}); err != nil {
		slog.Warn("openai_image_headers_snapshot_failed", "account_id", account.ID, "error", err)
	}
}
