package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestCountAccountImagesSinceExcludesCurrentRequest(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &usageLogRepository{sql: db}
	since := time.Now().Add(-time.Hour)
	earliest := since.Add(10 * time.Minute)

	mock.ExpectQuery(`(?s)SUM\(image_count\).*MIN\(created_at\).*request_id IS DISTINCT FROM \$3.*billing_mode`).
		WithArgs(int64(91), since, "req-current").
		WillReturnRows(sqlmock.NewRows([]string{"sum", "min"}).AddRow(int64(3), earliest))
	count, oldest, err := repo.CountAccountImagesSince(context.Background(), 91, since, "req-current")
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
	require.True(t, earliest.Equal(*oldest))

	mock.ExpectQuery(`(?s)SUM\(image_count\)`).
		WithArgs(int64(92), since, "").
		WillReturnRows(sqlmock.NewRows([]string{"sum", "min"}).AddRow(int64(0), nil))
	count, oldest, err = repo.CountAccountImagesSince(context.Background(), 92, since, "")
	require.NoError(t, err)
	require.Zero(t, count)
	require.Nil(t, oldest)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCountAccountImagesInRangesKeepsRangeOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &usageLogRepository{sql: db}
	from := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	to := from.Add(3 * time.Hour)
	ranges := []service.AccountImageCountRange{{AccountID: 7, From: from, To: to}, {AccountID: 8, From: from, To: to}, {AccountID: 7, From: to, To: to.Add(time.Hour)}}

	mock.ExpectQuery(`(?s)unnest\(\$1::bigint\[\], \$2::text\[\], \$3::text\[\]\) WITH ORDINALITY.*GROUP BY r.ord`).
		WithArgs(pq.Array([]int64{7, 8, 7}),
			pq.Array([]string{"2026-10-05T01:00:00Z", "2026-10-05T01:00:00Z", "2026-10-05T04:00:00Z"}),
			pq.Array([]string{"2026-10-05T04:00:00Z", "2026-10-05T04:00:00Z", "2026-10-05T05:00:00Z"})).
		WillReturnRows(sqlmock.NewRows([]string{"ord", "sum"}).AddRow(int64(3), int64(5)).AddRow(int64(1), int64(12)).AddRow(int64(2), int64(0)))
	counts, err := repo.CountAccountImagesInRanges(context.Background(), ranges)
	require.NoError(t, err)
	require.Equal(t, []int64{12, 0, 5}, counts)

	counts, err = repo.CountAccountImagesInRanges(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, counts, "no ranges means no query")
	require.NoError(t, mock.ExpectationsWereMet())
}
