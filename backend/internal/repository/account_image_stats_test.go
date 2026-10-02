package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountImageStatsBatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newUsageLogRepositoryWithSQL(nil, db)
	today := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	now := today.Add(time.Hour)
	mock.ExpectQuery(`(?s)SUM\(image_count\).*created_at >= \$2.*account_id = ANY\(\$1\).*image_count > 0.*video_count.*billing_mode`).
		WithArgs("{1,2}", today, now).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "today", "total"}).AddRow(int64(1), int64(4), int64(17)))
	got, err := repo.GetAccountImageStatsBatch(context.Background(), []int64{1, 2}, today, now)
	require.NoError(t, err)
	require.Equal(t, &service.AccountImageStats{TodayCount: 4, TotalCount: 17}, got[1])
	require.Equal(t, &service.AccountImageStats{}, got[2])
	empty, err := repo.GetAccountImageStatsBatch(context.Background(), nil, today, now)
	require.NoError(t, err)
	require.Empty(t, empty)
	mock.ExpectQuery(`SELECT account_id`).WillReturnError(errors.New("database unavailable"))
	got, err = repo.GetAccountImageStatsBatch(context.Background(), []int64{1}, today, now)
	require.Error(t, err)
	require.Nil(t, got, "an unavailable statistic must not be returned as zero")
	require.NoError(t, mock.ExpectationsWereMet())
}
