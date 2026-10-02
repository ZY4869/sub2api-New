package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type scheduledPlanUpdateRepo struct {
	service.ScheduledTestPlanRepository
	updated *service.ScheduledTestPlan
}

func (r *scheduledPlanUpdateRepo) GetByID(context.Context, int64) (*service.ScheduledTestPlan, error) {
	return &service.ScheduledTestPlan{ID: 1, ModelID: "old-model", CronExpression: "0 7 * * *"}, nil
}
func (r *scheduledPlanUpdateRepo) Update(_ context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	r.updated = plan
	return plan, nil
}

func TestScheduledTestUpdateModelDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct{ name, body, model string }{
		{"omitted", `{}`, "old-model"},
		{"default", `{"model_id":""}`, ""},
		{"explicit", `{"model_id":"new-model"}`, "new-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &scheduledPlanUpdateRepo{}
			h := NewScheduledTestHandler(service.NewScheduledTestService(repo, nil))
			r := gin.New()
			r.PUT("/plans/:id", h.Update)
			req := httptest.NewRequest(http.MethodPut, "/plans/1", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.NotNil(t, repo.updated)
			require.Equal(t, tc.model, repo.updated.ModelID)
		})
	}
}
