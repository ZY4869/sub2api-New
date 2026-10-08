package repository

// [local] 主池耗尽的账号级限流与 extra 标记在同一条 UPDATE 中写入，二者不会只成功一半。
import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// SetOpenAIMainPoolRateLimited 与 SetRateLimited 相同，并记录本次限流仅来自 Codex 主池。
func (r *accountRepository) SetOpenAIMainPoolRateLimited(ctx context.Context, id int64, resetAt time.Time, reason string) error {
	now := time.Now().UTC()
	payload, err := json.Marshal(map[string]string{
		"reset_at":   resetAt.UTC().Format(time.RFC3339Nano),
		"reason":     reason,
		"limited_at": now.Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	client := clientFromContext(ctx, r.client)
	if _, err := client.ExecContext(ctx, `UPDATE accounts SET
		rate_limited_at = $1,
		rate_limit_reset_at = $2,
		extra = jsonb_set(COALESCE(extra, '{}'::jsonb), ARRAY[$3]::text[], $4::jsonb, true),
		updated_at = NOW()
		WHERE id = $5 AND deleted_at IS NULL`, now, resetAt, service.OpenAIMainPoolRateLimitExtraKey, payload, id); err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue main pool rate limit failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}
