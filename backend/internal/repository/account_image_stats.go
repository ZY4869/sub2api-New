package repository

// [local] Reuse recorded output counts; no mutable counter or migration is needed.
import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func (r *usageLogRepository) GetAccountImageStatsBatch(ctx context.Context, ids []int64, today, now time.Time) (map[int64]*service.AccountImageStats, error) {
	result := make(map[int64]*service.AccountImageStats, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT account_id,
			COALESCE(SUM(image_count) FILTER (WHERE created_at >= $2), 0),
			COALESCE(SUM(image_count), 0)
		FROM usage_logs
		WHERE account_id = ANY($1) AND created_at <= $3
			AND image_count > 0 AND COALESCE(video_count, 0) = 0
			AND COALESCE(billing_mode, '') <> 'video'
		GROUP BY account_id`, pq.Array(ids), today, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		stats := &service.AccountImageStats{}
		if err := rows.Scan(&id, &stats.TodayCount, &stats.TotalCount); err != nil {
			return nil, err
		}
		result[id] = stats
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if result[id] == nil {
			result[id] = &service.AccountImageStats{}
		}
	}
	return result, nil
}
