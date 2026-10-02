package repository

// [local] 账号自动续期（本地定制功能）的数据访问实现。
//
// 刻意独立成文件，并且**不**扩展 service.AccountRepository 接口：
// 续期服务依赖的是 service.AccountRenewalRepository 这个窄接口，*accountRepository 天然满足它。
// 这样既不必给上游的一大批测试 stub 补方法，也把与上游的合并冲突面降到最低。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbpredicate "github.com/Wei-Shaw/sub2api/ent/predicate"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// accountRenewalGraceIntervalSQL 由 extra 推导宽限期长度。
//
// 正则守卫不可省略：extra 是用户可编辑的自由结构，直接 ::int 遇到非数字值会让整条语句报错。
// 上限 365 与 service.ClampAccountRenewalGraceDays 保持一致，缺省 7 对应
// service.DefaultAccountRenewalGraceDays。AutoPauseExpiredAccounts 复用同一片段，
// 确保"谁该被暂停"与"谁该被续期"两侧的宽限期判定不会漂移。
func accountRenewalGraceIntervalSQL(extraColumn string) string {
	v := extraColumn + "->>'auto_renewal_grace_days'"
	return "make_interval(days => CASE WHEN " + v + " ~ '^[0-9]{1,9}$' THEN LEAST((" + v + ")::int, 365) WHEN " + v + " ~ '^[0-9]+$' THEN 365 ELSE 7 END)"
}

// accountRenewalEnabledSQL 判定自动续期开关。
// extra 里的布尔既可能是 JSON true，也可能是写入路径留下的字符串 "true"/"1"。
func accountRenewalEnabledSQL(extraColumn string) string {
	return "COALESCE(lower(btrim(" + extraColumn + "->>'auto_renewal_enabled')), 'false') IN ('true', 't', '1')"
}

// Compare the original JSON values, including missing/null and large numeric values.
func accountRenewalConfigSQL(extraColumn string) string {
	return "jsonb_build_object('auto_renewal_enabled', " + extraColumn + "->'auto_renewal_enabled', 'auto_renewal_cycle', " + extraColumn + "->'auto_renewal_cycle', 'auto_renewal_grace_days', " + extraColumn + "->'auto_renewal_grace_days')"
}

func accountRenewalInGraceSQL(extraColumn, expiryColumn, nowExpr string) string {
	return "(" + accountRenewalEnabledSQL(extraColumn) + " AND " + expiryColumn + " + " + accountRenewalGraceIntervalSQL(extraColumn) + " > " + nowExpr + ")"
}

// group_repo 的两个 const 查询需要常量；测试锁定其与函数输出完全相等。
const accountRenewalInGraceSQLAliasANow = "(COALESCE(lower(btrim(a.extra->>'auto_renewal_enabled')), 'false') IN ('true', 't', '1') AND a.expires_at + make_interval(days => CASE WHEN a.extra->>'auto_renewal_grace_days' ~ '^[0-9]{1,9}$' THEN LEAST((a.extra->>'auto_renewal_grace_days')::int, 365) WHEN a.extra->>'auto_renewal_grace_days' ~ '^[0-9]+$' THEN 365 ELSE 7 END) > NOW())"

func accountRenewalGracePredicate(now time.Time) dbpredicate.Account {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("(" + accountRenewalEnabledSQL(s.C(dbaccount.FieldExtra)) + " AND " + s.C(dbaccount.FieldExpiresAt) + " + " + accountRenewalGraceIntervalSQL(s.C(dbaccount.FieldExtra)) + " > ").Arg(now).WriteString(")")
		}))
	}
}

func NewAccountRenewalRepository(client *dbent.Client, sqlDB *sql.DB, schedulerCache service.SchedulerCache) service.AccountRenewalRepository {
	return newAccountRepositoryWithSQL(client, sqlDB, schedulerCache)
}

// ListAccountsPendingRenewal 返回"已过期、仍在宽限期内、且有调用正常证据"的账号。
//
// 两路信号取并集，缺一不可：
//   - last_used_at：真实业务流量。由 DeferredService 在计费落账后刷新，语义是"成功产生过用量"。
//   - scheduled_test_results：定时唤醒计划的成功记录。定时测试直连上游、不走计费链路，
//     因此不会更新 last_used_at；只看 last_used_at 会漏掉纯靠定时唤醒保活的账号。
func (r *accountRepository) ListAccountsPendingRenewal(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]service.AccountRenewalCandidate, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.sql.QueryContext(ctx, `
		SELECT a.id, a.name, a.expires_at, COALESCE(a.extra, '{}'::jsonb), `+accountRenewalConfigSQL("a.extra")+`
		FROM accounts a
		WHERE a.deleted_at IS NULL
			AND a.status = 'active'
			AND a.expires_at IS NOT NULL
			AND a.expires_at <= $1
			AND `+accountRenewalInGraceSQL("a.extra", "a.expires_at", "$1")+`
			AND (
				(a.last_used_at IS NOT NULL AND a.last_used_at > a.expires_at)
				OR EXISTS (
					SELECT 1
					FROM scheduled_test_results str
					JOIN scheduled_test_plans stp ON stp.id = str.plan_id
					WHERE stp.account_id = a.id
						AND str.status = 'success'
						AND str.finished_at > a.expires_at
				)
			)
		ORDER BY a.expires_at
		LIMIT $2
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()

	candidates := make([]service.AccountRenewalCandidate, 0)
	for rows.Next() {
		var (
			candidate service.AccountRenewalCandidate
			expiresAt time.Time
			rawExtra  []byte
			rawConfig []byte
		)
		if err := rows.Scan(&candidate.ID, &candidate.Name, &expiresAt, &rawExtra, &rawConfig); err != nil {
			return nil, err
		}
		candidate.ExpiresAt = expiresAt
		candidate.RenewalConfigSnapshot = json.RawMessage(rawConfig)

		extra := map[string]any{}
		if len(rawExtra) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(rawExtra))
			decoder.UseNumber()
			if err := decoder.Decode(&extra); err != nil {
				// 单个账号的 extra 损坏不应中断整批续期扫描。
				logger.LegacyPrintf(
					"repository.account_renewal",
					"[AccountRenewal] skip account=%d: malformed extra: %v", candidate.ID, err,
				)
				continue
			}
		}
		candidate.Extra = extra
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

// RenewAccountExpiry 原子地推进过期时间并合并续期状态到 extra。
//
// expires_at 直接影响 Account.IsSchedulable，因此必须入调度 outbox 并刷新账号快照，
// 否则调度侧会继续拿着旧的过期时间判断，续期结果要等到下次全量重建才生效。
// 条件里的 expires_at = $4 是乐观并发保护：管理员在同一时刻手工改过期时间时本次续期直接放弃，
// 避免自动续期覆盖掉人工决定。
func (r *accountRepository) RenewAccountExpiry(
	ctx context.Context,
	id int64,
	newExpiresAt time.Time,
	expectedExpiresAt time.Time,
	expectedConfig json.RawMessage,
	extraUpdates map[string]any,
) error {
	if !json.Valid(expectedConfig) {
		return service.ErrAccountRenewalConflict
	}
	payload, err := json.Marshal(extraUpdates)
	if err != nil {
		return err
	}

	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if contextTx == nil {
		var txErr error
		tx, txErr = r.client.Tx(ctx)
		if txErr != nil && !errors.Is(txErr, dbent.ErrTxStarted) {
			return txErr
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}

	// Acquire the row before evaluating clock_timestamp(). PostgreSQL can evaluate
	// an UPDATE filter before waiting for a row lock; a plain predicate alone
	// would allow a candidate whose grace period ended during that wait.
	rows, err := client.QueryContext(ctx, "SELECT id FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR NO KEY UPDATE", id)
	if err != nil {
		return err
	}
	found := rows.Next()
	rowErr := rows.Err()
	closeErr := rows.Close()
	if rowErr != nil {
		return rowErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !found {
		return service.ErrAccountRenewalConflict
	}

	result, err := client.ExecContext(
		ctx,
		`UPDATE accounts
		 SET expires_at = $1,
		     extra = COALESCE(extra, '{}'::jsonb) || $2::jsonb,
		     updated_at = NOW()
		 WHERE id = $3 AND deleted_at IS NULL AND expires_at = $4
		   AND status = 'active'
		   AND `+accountRenewalConfigSQL("extra")+` = $5::jsonb
		   AND expires_at <= clock_timestamp()
		   AND `+accountRenewalInGraceSQL("extra", "expires_at", "clock_timestamp()"),
		newExpiresAt, string(payload), id, expectedExpiresAt, string(expectedConfig),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// 账号已被删除，或过期时间在本次扫描期间被改动：放弃本次续期，下一轮重新判定。
		return service.ErrAccountRenewalConflict
	}

	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		return err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	// Caller-owned transactions publish through the outbox after their commit.
	if tx != nil {
		r.syncSchedulerAccountSnapshot(baseCtx, id)
	}
	return nil
}
