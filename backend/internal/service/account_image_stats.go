package service

// [local] Image counts are local output statistics, not upstream quota estimates.
import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

type AccountImageStats struct {
	TodayCount int64 `json:"today_count"`
	TotalCount int64 `json:"total_count"`
}

type accountImageStatsBatchReader interface {
	GetAccountImageStatsBatch(context.Context, []int64, time.Time, time.Time) (map[int64]*AccountImageStats, error)
}

func (s *AccountUsageService) GetImageStatsBatch(ctx context.Context, accountIDs []int64) (map[int64]*AccountImageStats, error) {
	ids := make([]int64, 0, len(accountIDs))
	seen := make(map[int64]bool, len(accountIDs))
	for _, id := range accountIDs {
		if id > 0 && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	if len(ids) == 0 {
		return map[int64]*AccountImageStats{}, nil
	}
	reader, ok := s.usageLogRepo.(accountImageStatsBatchReader)
	if !ok {
		return nil, fmt.Errorf("account image statistics are unavailable")
	}
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return reader.GetAccountImageStatsBatch(queryCtx, ids, timezone.Today(), time.Now())
}
