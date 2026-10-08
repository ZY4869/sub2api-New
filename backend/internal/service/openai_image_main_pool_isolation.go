package service

// [local] 普通额度（Codex 主池）耗尽只拦文本：原生 /codex/images/* 生图走独立额度池，
// 仍可调度到该账号。主池封停有两种来源，都按自身数据校验，不依赖清理：
//  1. handle429 写账号级限流时，同一条 UPDATE 写入 extra 标记；截止时间一致才有效。
//  2. 平台调度阈值按 codex 5h/7d 写入的临时不可调度；原因 JSON 自带来源与窗口。
import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

const (
	// OpenAIMainPoolRateLimitExtraKey 标记账号级限流来自主池耗尽。
	OpenAIMainPoolRateLimitExtraKey = "openai_main_pool_rate_limit"

	openAIMainPoolReasonCodexWindow = "codex_window_exhausted"
	openAIMainPoolReasonUsageLimit  = "usage_limit_reached"

	openAIMainPoolMarkerTolerance    = time.Second
	openAIImageMainPoolCandidateTTL  = 5 * time.Second
	openAIImageMainPoolCandidateWait = 3 * time.Second
)

var openAIImageMainPoolIsolationDisabled atomic.Bool

// SetOpenAIImageMainPoolIsolationEnabled 发布反向隔离开关。判定分散在调度热路径的纯函数里，
// 无法逐一注入配置，故由持有配置的服务在构造时发布进程级快照。
func SetOpenAIImageMainPoolIsolationEnabled(enabled bool) {
	openAIImageMainPoolIsolationDisabled.Store(!enabled)
}

func openAIImageMainPoolIsolationEnabled() bool {
	return !openAIImageMainPoolIsolationDisabled.Load()
}

type openAIImagesForwardModelContextKey struct{}

type openAIImagesUpstreamDirectContextKey struct{}

// WithOpenAIImagesForwardModel 记录 /v1/images 渠道映射后的转发模型，调度据此预测上游端点。
func WithOpenAIImagesForwardModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIImagesForwardModelContextKey{}, strings.TrimSpace(model))
}

func openAIImagesForwardModelFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	model, _ := ctx.Value(openAIImagesForwardModelContextKey{}).(string)
	return model, model != ""
}

// withOpenAIImagesUpstreamDirect 记录本次上游尝试是否为原生 /codex/images/*。
func withOpenAIImagesUpstreamDirect(ctx context.Context, direct bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIImagesUpstreamDirectContextKey{}, direct)
}

// openAIImagesUpstreamIsResponses 报告本次生图尝试走 /codex/responses，即消耗主池。
func openAIImagesUpstreamIsResponses(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	direct, ok := ctx.Value(openAIImagesUpstreamDirectContextKey{}).(bool)
	return ok && !direct
}

func isOpenAIImageIsolationAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth && !account.IsShadow()
}

// isOpenAINativeImageRequest 报告本次 /v1/images 请求在该账号上会走原生 /codex/images/*。
func isOpenAINativeImageRequest(ctx context.Context, account *Account) bool {
	if !openAIImageMainPoolIsolationEnabled() || !OpenAIImagesEndpointFromContext(ctx) || !isOpenAIImageIsolationAccount(account) {
		return false
	}
	model, ok := openAIImagesForwardModelFromContext(ctx)
	return ok && usesCodexDirectImages(account.GetMappedModel(resolveOpenAIImagesOAuthRequestModel("", model)))
}

// openAIMainPoolRateLimitCovered 报告生效中的账号级限流与主池标记的截止时间一致。
func (a *Account) openAIMainPoolRateLimitCovered(now time.Time) bool {
	if a == nil || a.RateLimitResetAt == nil || !now.Before(*a.RateLimitResetAt) {
		return false
	}
	marker, ok := a.Extra[OpenAIMainPoolRateLimitExtraKey].(map[string]any)
	if !ok {
		return false
	}
	raw, _ := marker["reset_at"].(string)
	resetAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	diff := resetAt.Sub(*a.RateLimitResetAt)
	return diff >= -openAIMainPoolMarkerTolerance && diff <= openAIMainPoolMarkerTolerance
}

// openAIMainPoolThresholdPauseActive 报告临时不可调度来自平台调度阈值对 codex 5h/7d 的暂停。
// openAIThresholdCandidates 只读这两个主池窗口，所以该暂停与生图池无关。
func (a *Account) openAIMainPoolThresholdPauseActive(now time.Time) bool {
	if a == nil || a.TempUnschedulableUntil == nil || !now.Before(*a.TempUnschedulableUntil) {
		return false
	}
	payload, ok := parseTempUnschedReasonPayload(a.TempUnschedulableReason)
	if !ok || payload.Source != AccountSchedulingThresholdReasonSource || strings.TrimSpace(payload.Scope) != "" ||
		!strings.EqualFold(strings.TrimSpace(payload.Platform), PlatformOpenAI) {
		return false
	}
	switch strings.TrimSpace(payload.Window) {
	case "5h", "7d":
		return true
	default:
		return false
	}
}

// openAIMainPoolBlockedOnly 报告账号存在主池封停，且生效的账号级封停全部来自主池。
func (a *Account) openAIMainPoolBlockedOnly(now time.Time) bool {
	if !isOpenAIImageIsolationAccount(a) || (a.OverloadUntil != nil && now.Before(*a.OverloadUntil)) {
		return false
	}
	rateLimited := a.RateLimitResetAt != nil && now.Before(*a.RateLimitResetAt)
	if rateLimited && !a.openAIMainPoolRateLimitCovered(now) {
		return false
	}
	tempBlocked := a.TempUnschedulableUntil != nil && now.Before(*a.TempUnschedulableUntil)
	if tempBlocked && !a.openAIMainPoolThresholdPauseActive(now) {
		return false
	}
	return rateLimited || tempBlocked
}

// openAIImageMainPoolBypass 报告原生生图请求能否越过该账号的主池封停。
// 其余可调度条件（状态、手动开关、过期与宽限、过载）复用 IsSchedulable，避免两套口径漂移。
func openAIImageMainPoolBypass(ctx context.Context, account *Account, now time.Time) bool {
	if !isOpenAINativeImageRequest(ctx, account) || !account.openAIMainPoolBlockedOnly(now) {
		return false
	}
	unblocked := *account
	unblocked.RateLimitResetAt = nil
	unblocked.TempUnschedulableUntil = nil
	return unblocked.IsSchedulable()
}

// openAIAccountSchedulable 是 OpenAI 调度对 IsSchedulable 的请求感知版本。
func openAIAccountSchedulable(ctx context.Context, account *Account) bool {
	return account != nil && (account.IsSchedulable() || openAIImageMainPoolBypass(ctx, account, time.Now()))
}

// openAIAccountSchedulableForModel 越过主池封停时仍检查模型级限流，生图冷却照常生效。
func openAIAccountSchedulableForModel(ctx context.Context, account *Account, requestedModel string) bool {
	if account == nil {
		return false
	}
	if account.IsSchedulableForModelWithContext(ctx, requestedModel) {
		return true
	}
	return openAIImageMainPoolBypass(ctx, account, time.Now()) && !account.isModelRateLimitedWithContext(ctx, requestedModel)
}

// shouldClearOpenAIStickySession 原生生图请求不因主池封停解除粘性绑定。
func shouldClearOpenAIStickySession(ctx context.Context, account *Account, requestedModel string) bool {
	if account != nil && openAIImageMainPoolBypass(ctx, account, time.Now()) {
		return account.GetRateLimitRemainingTimeWithContext(ctx, requestedModel) > 0
	}
	return shouldClearStickySession(account, requestedModel)
}

// isOpenAIAccountRequestRuntimeBlockedWithContext 原生生图请求越过主池封停时，
// 既不读也不清除账号级内存封停（它属于主池，文本请求仍需要），只看模型级瞬时封停。
func (s *OpenAIGatewayService) isOpenAIAccountRequestRuntimeBlockedWithContext(ctx context.Context, account *Account, requestedModel string) bool {
	if s != nil && openAIImageMainPoolBypass(ctx, account, time.Now()) {
		return s.isOpenAIAccountModelRuntimeBlocked(account, requestedModel)
	}
	return s.isOpenAIAccountRequestRuntimeBlocked(account, requestedModel)
}

type openAIImageMainPoolCandidateCache struct {
	entries sync.Map // key: string, value: *openAIImageMainPoolCandidateEntry
	group   singleflight.Group
}

type openAIImageMainPoolCandidateEntry struct {
	accounts  []Account
	expiresAt time.Time
}

// appendOpenAIImageMainPoolCandidates 为原生生图请求补回被候选查询按限流过滤掉的仅主池封停账号。
func (s *OpenAIGatewayService) appendOpenAIImageMainPoolCandidates(ctx context.Context, groupID *int64, platform string, accounts []Account) []Account {
	if s == nil || s.accountRepo == nil || platform != PlatformOpenAI || !openAIImageMainPoolIsolationEnabled() || !OpenAIImagesEndpointFromContext(ctx) {
		return accounts
	}
	if _, ok := openAIImagesForwardModelFromContext(ctx); !ok {
		return accounts
	}
	candidates := s.loadOpenAIImageMainPoolCandidates(ctx, groupID, platform)
	if len(candidates) == 0 {
		return accounts
	}
	seen := make(map[int64]struct{}, len(accounts))
	for i := range accounts {
		seen[accounts[i].ID] = struct{}{}
	}
	now := time.Now()
	var merged []Account
	for i := range candidates {
		candidate := candidates[i]
		if _, ok := seen[candidate.ID]; ok || !openAIImageMainPoolBypass(ctx, &candidate, now) {
			continue
		}
		// 缓存条目跨请求共享，后续路径可能就地改写 Extra，合并前深拷贝。
		extra, extraErr := cloneAccountJSONMap(candidate.Extra)
		credentials, credentialsErr := cloneAccountJSONMap(candidate.Credentials)
		if extraErr != nil || credentialsErr != nil {
			slog.Warn("openai_image_main_pool_candidate_clone_failed", "account_id", candidate.ID)
			continue
		}
		candidate.Extra, candidate.Credentials = extra, credentials
		if merged == nil {
			merged = make([]Account, 0, len(accounts)+len(candidates))
			merged = append(merged, accounts...)
		}
		merged = append(merged, candidate)
		slog.Debug("openai_image_main_pool_bypass", "account_id", candidate.ID)
	}
	if merged == nil {
		return accounts
	}
	return merged
}

// loadOpenAIImageMainPoolCandidates 复用忽略瞬时状态的候选查询，取法与 listSchedulableAccounts 的分支一致。
func (s *OpenAIGatewayService) loadOpenAIImageMainPoolCandidates(ctx context.Context, groupID *int64, platform string) []Account {
	simple := s.cfg != nil && s.cfg.RunMode == config.RunModeSimple
	var queryGroupID *int64
	if !simple {
		queryGroupID = groupID
	}
	key := fmt.Sprintf("%s|%d|%t|%t", platform, derefGroupID(queryGroupID), queryGroupID != nil, simple)
	if cached, ok := s.openaiImageMainPoolCandidates.entries.Load(key); ok {
		if entry, ok := cached.(*openAIImageMainPoolCandidateEntry); ok && time.Now().Before(entry.expiresAt) {
			return entry.accounts
		}
	}
	value, _, _ := s.openaiImageMainPoolCandidates.group.Do(key, func() (any, error) {
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIImageMainPoolCandidateWait)
		defer cancel()
		accounts, err := s.accountRepo.ListModelAvailabilityCandidates(queryCtx, queryGroupID, []string{platform}, simple)
		if err != nil {
			slog.Warn("openai_image_main_pool_candidates_failed", "group_id", derefGroupID(queryGroupID), "error", err)
		}
		now := time.Now()
		blocked := make([]Account, 0)
		for i := range accounts {
			if accounts[i].openAIMainPoolBlockedOnly(now) {
				blocked = append(blocked, accounts[i])
			}
		}
		s.openaiImageMainPoolCandidates.entries.Store(key, &openAIImageMainPoolCandidateEntry{accounts: blocked, expiresAt: now.Add(openAIImageMainPoolCandidateTTL)})
		return blocked, nil
	})
	accounts, _ := value.([]Account)
	return accounts
}

// openAIMainPoolRateLimitWriter 由仓储实现：限流列与主池标记在同一条 UPDATE 中写入。
type openAIMainPoolRateLimitWriter interface {
	SetOpenAIMainPoolRateLimited(ctx context.Context, id int64, resetAt time.Time, reason string) error
}

// persistOpenAIRateLimit 写入账号级限流；主池耗尽时一并写标记，原生生图据此放行。
// 仓储不支持或开关关闭时退回普通限流，即不放行。
func persistOpenAIRateLimit(ctx context.Context, repo AccountRepository, account *Account, resetAt time.Time, mainPoolReason string) error {
	if mainPoolReason != "" && openAIImageMainPoolIsolationEnabled() && isOpenAIImageIsolationAccount(account) {
		if writer, ok := repo.(openAIMainPoolRateLimitWriter); ok {
			if err := writer.SetOpenAIMainPoolRateLimited(ctx, account.ID, resetAt, mainPoolReason); err != nil {
				return err
			}
			slog.Info("openai_main_pool_rate_limited", "account_id", account.ID, "reason", mainPoolReason, "reset_at", resetAt)
			return nil
		}
	}
	return repo.SetRateLimited(ctx, account.ID, resetAt)
}

// openAIMainPoolReasonFromBody 只把 usage_limit_reached 视为主池耗尽；
// rate_limit_exceeded 可能只是短时限速，带生图标记的报错属于生图池。
func openAIMainPoolReasonFromBody(body []byte) string {
	if gjson.GetBytes(body, "error.type").String() != openAIMainPoolReasonUsageLimit || isOpenAIImageRateLimitError(http.StatusTooManyRequests, body) {
		return ""
	}
	return openAIMainPoolReasonUsageLimit
}

// isOpenAIImagesResponsesMainPoolExhaustion 报告走 /codex/responses 的生图请求撞上了主池耗尽。
func isOpenAIImagesResponsesMainPoolExhaustion(ctx context.Context, headers http.Header, body []byte) bool {
	if !openAIImageMainPoolIsolationEnabled() || !openAIImagesUpstreamIsResponses(ctx) {
		return false
	}
	if disposition, _ := classifyOpenAIOAuth429(headers, body); disposition == openAIOAuth429Quota5h || disposition == openAIOAuth429Quota7d {
		return true
	}
	return openAIMainPoolReasonFromBody(body) != ""
}

// RecoverAccountAfterSuccessfulModelTest 原生生图测试成功只说明生图池可用；
// 账号仅因主池被封时保留封停，避免文本请求被放回后再撞 429。
func (s *RateLimitService) RecoverAccountAfterSuccessfulModelTest(ctx context.Context, accountID int64, modelID string) (*SuccessfulTestRecoveryResult, error) {
	modelID = strings.TrimSpace(modelID)
	if s != nil && s.accountRepo != nil && modelID != "" && openAIImageMainPoolIsolationEnabled() {
		account, err := s.accountRepo.GetByID(ctx, accountID)
		if err == nil && isOpenAIImageIsolationAccount(account) && usesCodexDirectImages(account.GetMappedModel(modelID)) &&
			account.openAIMainPoolBlockedOnly(time.Now()) {
			slog.Info("openai_image_test_recovery_skipped", "account_id", accountID, "model", modelID)
			return &SuccessfulTestRecoveryResult{}, nil
		}
	}
	return s.RecoverAccountAfterSuccessfulTest(ctx, accountID)
}
