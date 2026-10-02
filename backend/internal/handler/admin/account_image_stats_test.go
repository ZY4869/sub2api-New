package admin

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountImageStatsCacheSeparatesOptionalPayload(t *testing.T) {
	ids := []int64{982173}
	legacyKey := buildAccountTodayStatsBatchCacheKey(ids)
	imageKey := buildAccountTodayStatsBatchCacheKey(ids, true)
	require.NotEqual(t, legacyKey, imageKey)
	require.Contains(t, imageKey, timezone.Today().Format("2006-01-02"))
	accountTodayStatsBatchCache.Set(legacyKey, gin.H{"stats": gin.H{"982173": gin.H{"requests": 1}}})
	accountTodayStatsBatchCache.Set(imageKey, gin.H{"stats": gin.H{}, "image_stats": gin.H{"982173": gin.H{"today_count": 3, "total_count": 20}}})
	for _, include := range []bool{false, true} {
		body, err := json.Marshal(BatchTodayStatsRequest{AccountIDs: ids, IncludeImageStats: include})
		require.NoError(t, err)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/admin/accounts/today-stats/batch", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		(&AccountHandler{}).GetBatchTodayStats(c)
		require.Equal(t, 200, w.Code)
		var response struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		if include {
			require.Contains(t, response.Data, "image_stats")
		} else {
			require.NotContains(t, response.Data, "image_stats")
		}
	}
}
