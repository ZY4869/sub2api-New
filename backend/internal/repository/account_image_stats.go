package repository

// [local] Reuse recorded output counts; no mutable counter or migration is needed.
import (
	"context"
	"database/sql"
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

// CountAccountImagesSince 统计账号自 since 起的生图张数及最早一条的时间，用于套餐滚动窗口限额。
// 排除指定请求：用量日志批量异步落库，调用方另行计入本次张数。
func (r *usageLogRepository) CountAccountImagesSince(ctx context.Context, accountID int64, since time.Time, excludeRequestID string) (int64, *time.Time, error) {
	rows, err := r.sql.QueryContext(ctx, `
		SELECT COALESCE(SUM(image_count), 0), MIN(created_at)
		FROM usage_logs
		WHERE account_id = $1 AND created_at >= $2 AND request_id IS DISTINCT FROM $3
			AND image_count > 0 AND COALESCE(video_count, 0) = 0
			AND COALESCE(billing_mode, '') <> 'video'`, accountID, since, excludeRequestID)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()
	var total int64
	var earliest sql.NullTime
	if rows.Next() {
		if err := rows.Scan(&total, &earliest); err != nil {
			return 0, nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if !earliest.Valid {
		return total, nil, nil
	}
	return total, &earliest.Time, nil
}

// CountAccountImagesInRanges 按区间批量统计生图张数，结果与 ranges 一一对应。
func (r *usageLogRepository) CountAccountImagesInRanges(ctx context.Context, ranges []service.AccountImageCountRange) ([]int64, error) {
	counts := make([]int64, len(ranges))
	if len(ranges) == 0 {
		return counts, nil
	}
	ids := make([]int64, len(ranges))
	froms := make([]string, len(ranges))
	tos := make([]string, len(ranges))
	for i, item := range ranges {
		ids[i] = item.AccountID
		froms[i] = item.From.UTC().Format(time.RFC3339Nano)
		tos[i] = item.To.UTC().Format(time.RFC3339Nano)
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT r.ord, COALESCE(SUM(u.image_count), 0)
		FROM unnest($1::bigint[], $2::text[], $3::text[]) WITH ORDINALITY AS r(account_id, from_at, to_at, ord)
		LEFT JOIN usage_logs u ON u.account_id = r.account_id
			AND u.created_at >= r.from_at::timestamptz AND u.created_at <= r.to_at::timestamptz
			AND u.image_count > 0 AND COALESCE(u.video_count, 0) = 0
			AND COALESCE(u.billing_mode, '') <> 'video'
		GROUP BY r.ord`, pq.Array(ids), pq.Array(froms), pq.Array(tos))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ord, total int64
		if err := rows.Scan(&ord, &total); err != nil {
			return nil, err
		}
		if ord >= 1 && ord <= int64(len(counts)) {
			counts[ord-1] = total
		}
	}
	return counts, rows.Err()
}
