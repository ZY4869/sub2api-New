//go:build unit

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// imageQuotaSettingRepoStub 在只读替身之上允许保存设置。
type imageQuotaSettingRepoStub struct {
	settingHandlerRepoStub
}

func (s *imageQuotaSettingRepoStub) Set(_ context.Context, key, value string) error {
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[key] = value
	return nil
}

func TestOpenAIImageQuotaSettingsHandlerRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &imageQuotaSettingRepoStub{}
	handler := NewSettingHandler(service.NewSettingService(repo, &config.Config{}), nil, nil, nil, nil, nil, nil)
	call := func(method string, body any, handle func(*gin.Context)) (int, map[string]any) {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(method, "/api/v1/admin/settings/openai-image-quota", bytes.NewReader(raw))
		c.Request.Header.Set("Content-Type", "application/json")
		handle(c)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		return rec.Code, envelope
	}

	code, _ := call(http.MethodPut, map[string]any{"pause_threshold_percent": 150}, handler.UpdateOpenAIImageQuotaSettings)
	require.Equal(t, http.StatusBadRequest, code)
	require.NotContains(t, repo.values, service.SettingKeyOpenAIImageQuotaSettings, "invalid settings are not saved")

	code, envelope := call(http.MethodPut, map[string]any{
		"pause_threshold_percent": 90,
		"plan_limits":             map[string]any{"Pro Lite": []map[string]int{{"window_minutes": 180, "max_images": 40}}},
	}, handler.UpdateOpenAIImageQuotaSettings)
	require.Equal(t, http.StatusOK, code)
	data := envelope["data"].(map[string]any)
	require.Equal(t, float64(90), data["pause_threshold_percent"])
	require.Contains(t, data["plan_limits"], "prolite")

	code, envelope = call(http.MethodGet, nil, handler.GetOpenAIImageQuotaSettings)
	require.Equal(t, http.StatusOK, code)
	rules := envelope["data"].(map[string]any)["plan_limits"].(map[string]any)["prolite"].([]any)
	require.Equal(t, float64(40), rules[0].(map[string]any)["max_images"])
}
