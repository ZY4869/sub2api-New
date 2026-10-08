package service

// [local] 生图额度设置：生图池主动暂停阈值与按套餐的手动限额。热路径读进程内缓存。
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"golang.org/x/sync/singleflight"
)

const (
	defaultOpenAIImageQuotaPauseThresholdPercent = 100
	openAIImageQuotaMaxWindowMinutes             = 43200
	openAIImageQuotaMaxImages                    = 1000000
	openAIImageQuotaMaxRulesPerPlan              = 4
	openAIImageQuotaMaxPlans                     = 32
	openAIImageQuotaMaxPlanKeyLength             = 64
	openAIImageQuotaSettingsCacheTTL             = 30 * time.Second
)

// OpenAIImagePlanLimitRule 在滚动窗口内限制账号的生图张数。
type OpenAIImagePlanLimitRule struct {
	WindowMinutes int `json:"window_minutes"`
	MaxImages     int `json:"max_images"`
}

// OpenAIImageQuotaSettings 生图额度设置。PauseThresholdPercent 为 0 时关闭主动暂停。
type OpenAIImageQuotaSettings struct {
	PauseThresholdPercent int                                   `json:"pause_threshold_percent"`
	PlanLimits            map[string][]OpenAIImagePlanLimitRule `json:"plan_limits"`
}

func DefaultOpenAIImageQuotaSettings() *OpenAIImageQuotaSettings {
	return &OpenAIImageQuotaSettings{
		PauseThresholdPercent: defaultOpenAIImageQuotaPauseThresholdPercent,
		PlanLimits:            map[string][]OpenAIImagePlanLimitRule{},
	}
}

// NormalizeOpenAIPlanType 与前端 openAIPlanTypeKey 一致：小写并去掉空白、下划线和连字符，chatgptpro 视为 pro。
func NormalizeOpenAIPlanType(value string) string {
	key := strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
	if key == "chatgptpro" {
		return "pro"
	}
	return key
}

// NormalizeOpenAIImageQuotaSettings 校验并规范化设置：套餐键统一口径，规则按窗口排序，空规则的套餐移除。
func NormalizeOpenAIImageQuotaSettings(settings *OpenAIImageQuotaSettings) (*OpenAIImageQuotaSettings, error) {
	if settings == nil {
		return nil, errors.New("settings cannot be nil")
	}
	if settings.PauseThresholdPercent < 0 || settings.PauseThresholdPercent > 100 {
		return nil, errors.New("pause_threshold_percent must be between 0-100")
	}
	if len(settings.PlanLimits) > openAIImageQuotaMaxPlans {
		return nil, fmt.Errorf("plan_limits supports at most %d plans", openAIImageQuotaMaxPlans)
	}
	normalized := &OpenAIImageQuotaSettings{
		PauseThresholdPercent: settings.PauseThresholdPercent,
		PlanLimits:            make(map[string][]OpenAIImagePlanLimitRule, len(settings.PlanLimits)),
	}
	for plan, rules := range settings.PlanLimits {
		key := NormalizeOpenAIPlanType(plan)
		if key == "" || len(key) > openAIImageQuotaMaxPlanKeyLength {
			return nil, fmt.Errorf("invalid plan type %q", plan)
		}
		if len(rules) > openAIImageQuotaMaxRulesPerPlan {
			return nil, fmt.Errorf("plan %s supports at most %d rules", key, openAIImageQuotaMaxRulesPerPlan)
		}
		for _, rule := range rules {
			if rule.WindowMinutes < 1 || rule.WindowMinutes > openAIImageQuotaMaxWindowMinutes {
				return nil, fmt.Errorf("plan %s window_minutes must be between 1-%d", key, openAIImageQuotaMaxWindowMinutes)
			}
			if rule.MaxImages < 1 || rule.MaxImages > openAIImageQuotaMaxImages {
				return nil, fmt.Errorf("plan %s max_images must be between 1-%d", key, openAIImageQuotaMaxImages)
			}
		}
		if len(rules) == 0 {
			continue
		}
		if _, duplicated := normalized.PlanLimits[key]; duplicated {
			return nil, fmt.Errorf("plan %s is configured more than once", key)
		}
		sorted := append([]OpenAIImagePlanLimitRule(nil), rules...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].WindowMinutes < sorted[j].WindowMinutes })
		normalized.PlanLimits[key] = sorted
	}
	return normalized, nil
}

// planRules 返回账号套餐对应的限额规则。
func (s OpenAIImageQuotaSettings) planRules(planType string) []OpenAIImagePlanLimitRule {
	return s.PlanLimits[NormalizeOpenAIPlanType(planType)]
}

// GetOpenAIImageQuotaSettings 读取生图额度设置；缺失或已损坏时返回默认值。
func (s *SettingService) GetOpenAIImageQuotaSettings(ctx context.Context) (*OpenAIImageQuotaSettings, error) {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAIImageQuotaSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return DefaultOpenAIImageQuotaSettings(), nil
		}
		return nil, fmt.Errorf("get openai image quota settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return DefaultOpenAIImageQuotaSettings(), nil
	}
	var stored OpenAIImageQuotaSettings
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		slog.Warn("openai_image_quota_settings_invalid", "error", err)
		return DefaultOpenAIImageQuotaSettings(), nil
	}
	normalized, err := NormalizeOpenAIImageQuotaSettings(&stored)
	if err != nil {
		slog.Warn("openai_image_quota_settings_invalid", "error", err)
		return DefaultOpenAIImageQuotaSettings(), nil
	}
	return normalized, nil
}

// SetOpenAIImageQuotaSettings 校验后保存，并立即刷新本实例缓存。
func (s *SettingService) SetOpenAIImageQuotaSettings(ctx context.Context, settings *OpenAIImageQuotaSettings) (*OpenAIImageQuotaSettings, error) {
	normalized, err := NormalizeOpenAIImageQuotaSettings(settings)
	if err != nil {
		return nil, infraerrors.BadRequest("INVALID_OPENAI_IMAGE_QUOTA_SETTINGS", err.Error())
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal openai image quota settings: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyOpenAIImageQuotaSettings, string(data)); err != nil {
		return nil, err
	}
	openAIImageQuotaSettingsCache.Store(s, &cachedOpenAIImageQuotaSettings{settings: *normalized, expiresAt: time.Now().Add(openAIImageQuotaSettingsCacheTTL)})
	return normalized, nil
}

type cachedOpenAIImageQuotaSettings struct {
	settings  OpenAIImageQuotaSettings
	expiresAt time.Time
}

// 以 SettingService 实例为键，避免测试或多个实例之间串用缓存。
var (
	openAIImageQuotaSettingsCache sync.Map // key: *SettingService, value: *cachedOpenAIImageQuotaSettings
	openAIImageQuotaSettingsSF    singleflight.Group
)

// openAIImageQuotaSettingsCached 供热路径读取；读取失败时按默认值处理，不阻塞请求。
func (s *SettingService) openAIImageQuotaSettingsCached(ctx context.Context) OpenAIImageQuotaSettings {
	if s == nil || s.settingRepo == nil {
		return *DefaultOpenAIImageQuotaSettings()
	}
	if cached, ok := openAIImageQuotaSettingsCache.Load(s); ok {
		if entry, ok := cached.(*cachedOpenAIImageQuotaSettings); ok && time.Now().Before(entry.expiresAt) {
			return entry.settings
		}
	}
	value, _, _ := openAIImageQuotaSettingsSF.Do(fmt.Sprintf("%p", s), func() (any, error) {
		settings, err := s.GetOpenAIImageQuotaSettings(context.WithoutCancel(ctx))
		if err != nil {
			slog.Warn("openai_image_quota_settings_load_failed", "error", err)
			settings = DefaultOpenAIImageQuotaSettings()
		}
		openAIImageQuotaSettingsCache.Store(s, &cachedOpenAIImageQuotaSettings{settings: *settings, expiresAt: time.Now().Add(openAIImageQuotaSettingsCacheTTL)})
		return *settings, nil
	})
	settings, _ := value.(OpenAIImageQuotaSettings)
	return settings
}
