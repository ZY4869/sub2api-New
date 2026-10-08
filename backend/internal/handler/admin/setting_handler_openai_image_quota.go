package admin

// [local] 生图额度设置：主动暂停阈值与按套餐的生图限额。
import (
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// GetOpenAIImageQuotaSettings 获取生图额度设置
// GET /api/v1/admin/settings/openai-image-quota
func (h *SettingHandler) GetOpenAIImageQuotaSettings(c *gin.Context) {
	settings, err := h.settingService.GetOpenAIImageQuotaSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, openAIImageQuotaSettingsDTO(settings))
}

// UpdateOpenAIImageQuotaSettings 更新生图额度设置
// PUT /api/v1/admin/settings/openai-image-quota
func (h *SettingHandler) UpdateOpenAIImageQuotaSettings(c *gin.Context) {
	var req dto.OpenAIImageQuotaSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	settings := &service.OpenAIImageQuotaSettings{
		PauseThresholdPercent: req.PauseThresholdPercent,
		PlanLimits:            make(map[string][]service.OpenAIImagePlanLimitRule, len(req.PlanLimits)),
	}
	for plan, rules := range req.PlanLimits {
		for _, rule := range rules {
			settings.PlanLimits[plan] = append(settings.PlanLimits[plan], service.OpenAIImagePlanLimitRule{WindowMinutes: rule.WindowMinutes, MaxImages: rule.MaxImages})
		}
	}
	updated, err := h.settingService.SetOpenAIImageQuotaSettings(c.Request.Context(), settings)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, openAIImageQuotaSettingsDTO(updated))
}

func openAIImageQuotaSettingsDTO(settings *service.OpenAIImageQuotaSettings) dto.OpenAIImageQuotaSettings {
	out := dto.OpenAIImageQuotaSettings{PlanLimits: map[string][]dto.OpenAIImagePlanLimitRule{}}
	if settings == nil {
		return out
	}
	out.PauseThresholdPercent = settings.PauseThresholdPercent
	for plan, rules := range settings.PlanLimits {
		for _, rule := range rules {
			out.PlanLimits[plan] = append(out.PlanLimits[plan], dto.OpenAIImagePlanLimitRule{WindowMinutes: rule.WindowMinutes, MaxImages: rule.MaxImages})
		}
	}
	return out
}
