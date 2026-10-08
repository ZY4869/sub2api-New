package service

// [local] 按套餐的生图限额：生图入账后异步按滚动窗口核对，达到上限即冷却该账号的生图。
// 冷却复用 openai:image_generation，调度热路径不新增查询。
import (
	"context"
	"log/slog"
	"time"
)

const openAIImagePlanLimitCheckTimeout = 5 * time.Second

// scheduleOpenAIImagePlanLimitCheck 不阻塞计费链路；未配置规则时直接返回。
func (s *OpenAIGatewayService) scheduleOpenAIImagePlanLimitCheck(account *Account, requestID string, imageCount int) {
	if s == nil || s.settingService == nil || s.accountRepo == nil || imageCount <= 0 || !isOpenAIImageIsolationAccount(account) {
		return
	}
	if _, ok := s.usageLogRepo.(accountImageWindowCounter); !ok {
		return
	}
	// 请求结束后账号对象可能被复用，后台只带走 ID 与套餐。
	accountID, planType := account.ID, account.GetCredential("plan_type")
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("openai_image_plan_limit_panic", "account_id", accountID, "panic", recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), openAIImagePlanLimitCheckTimeout)
		defer cancel()
		s.enforceOpenAIImagePlanLimit(ctx, accountID, planType, requestID, imageCount, time.Now())
	}()
}

// enforceOpenAIImagePlanLimit 任一规则在窗口内达到上限时，冷却生图到窗口内最早一张滑出窗口。
// 并发中尚未落库的请求无法计入，属于软限制。
func (s *OpenAIGatewayService) enforceOpenAIImagePlanLimit(ctx context.Context, accountID int64, planType, requestID string, imageCount int, now time.Time) {
	rules := s.settingService.openAIImageQuotaSettingsCached(ctx).planRules(planType)
	counter, ok := s.usageLogRepo.(accountImageWindowCounter)
	if len(rules) == 0 || !ok {
		return
	}
	var (
		until    time.Time
		hit      OpenAIImagePlanLimitRule
		hitCount int64
	)
	for _, rule := range rules {
		window := time.Duration(rule.WindowMinutes) * time.Minute
		count, earliest, err := counter.CountAccountImagesSince(ctx, accountID, now.Add(-window), requestID)
		if err != nil {
			slog.Warn("openai_image_plan_limit_count_failed", "account_id", accountID, "error", err)
			return
		}
		total := count + int64(imageCount)
		if total < int64(rule.MaxImages) {
			continue
		}
		oldest := now
		if earliest != nil && earliest.Before(oldest) {
			oldest = *earliest
		}
		if candidate := oldest.Add(window); candidate.After(until) {
			until, hit, hitCount = candidate, rule, total
		}
	}
	if !until.After(now) {
		return
	}
	persistOpenAIImageCooldown(ctx, s.accountRepo, &Account{ID: accountID}, until, openAIImagePlanLimitReason)
	slog.Info("openai_image_plan_limit_reached", "account_id", accountID, "plan_type", NormalizeOpenAIPlanType(planType),
		"window_minutes", hit.WindowMinutes, "max_images", hit.MaxImages, "images", hitCount, "until", until)
}
