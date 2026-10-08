package admin

// [local] 生图额度统计：按套餐汇总用满样本，按账号给出当前窗口估算，供手动设定套餐限额参考。
import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// GetOpenAIImageQuotaStats 获取生图额度统计
// GET /api/v1/admin/accounts/openai-image-quota-stats
func (h *AccountHandler) GetOpenAIImageQuotaStats(c *gin.Context) {
	if h.accountUsageService == nil {
		response.Error(c, http.StatusServiceUnavailable, "account usage service is unavailable")
		return
	}
	stats, err := h.accountUsageService.GetOpenAIImageQuotaStats(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, stats)
}
