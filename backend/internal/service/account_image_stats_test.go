package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

type imageStatsBatchRepo struct {
	usageBatchLogRepoStub
	ids   []int64
	start time.Time
	end   time.Time
	err   error
}

func (r *imageStatsBatchRepo) GetAccountImageStatsBatch(ctx context.Context, ids []int64, start, end time.Time) (map[int64]*AccountImageStats, error) {
	r.ids, r.start, r.end = ids, start, end
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("query must have a deadline")
	}
	if r.err != nil {
		return nil, r.err
	}
	return map[int64]*AccountImageStats{1: {TodayCount: 3, TotalCount: 20}, 2: {}}, nil
}

func TestAccountImageStatsServiceBatch(t *testing.T) {
	repo := &imageStatsBatchRepo{}
	svc := &AccountUsageService{usageLogRepo: repo}
	got, err := svc.GetImageStatsBatch(context.Background(), []int64{1, 2, 1, 0, -1})
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, repo.ids)
	require.Equal(t, timezone.Today(), repo.start)
	require.WithinDuration(t, time.Now(), repo.end, time.Second)
	require.Equal(t, int64(3), got[1].TodayCount)
	require.Equal(t, int64(20), got[1].TotalCount)
	repo.err = errors.New("query canceled")
	got, err = svc.GetImageStatsBatch(context.Background(), []int64{1})
	require.Error(t, err)
	require.Nil(t, got)
	got, err = svc.GetImageStatsBatch(context.Background(), []int64{0, -1})
	require.NoError(t, err)
	require.Empty(t, got)
}
