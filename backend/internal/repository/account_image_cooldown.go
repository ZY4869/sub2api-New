package repository

// [local] Serialize image cooldown writes across requests and server processes.
import (
	"context"
	"encoding/json"
	"errors"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const accountImageCooldownScope = "openai:image_generation"

func (r *accountRepository) extendImageCooldown(ctx context.Context, id int64, resetAt time.Time, payload []byte) error {
	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if contextTx == nil {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}

	rows, err := client.QueryContext(ctx, `SELECT extra->'model_rate_limits'->'openai:image_generation'
		FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR NO KEY UPDATE`, id)
	if err != nil {
		return err
	}
	var raw []byte
	if !rows.Next() {
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		return service.ErrAccountNotFound
	}
	err = rows.Scan(&raw)
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}

	var stored struct {
		ResetAt string `json:"rate_limit_reset_at"`
	}
	_ = json.Unmarshal(raw, &stored)
	previous, parseErr := time.Parse(time.RFC3339, stored.ResetAt)
	// Compare at the precision actually stored in JSON; subsecond differences
	// must not rewrite the reason or first-limited time for an equal deadline.
	if parseErr != nil || resetAt.UTC().Truncate(time.Second).After(previous) {
		_, err = client.ExecContext(ctx, `UPDATE accounts SET
			extra = jsonb_set(
				jsonb_set(COALESCE(extra, '{}'::jsonb), '{model_rate_limits}'::text[],
					CASE WHEN jsonb_typeof(extra->'model_rate_limits') = 'object' THEN extra->'model_rate_limits' ELSE '{}'::jsonb END, true),
				ARRAY['model_rate_limits', $1]::text[], $2::jsonb, true),
			updated_at = NOW()
			WHERE id = $3 AND deleted_at IS NULL`, accountImageCooldownScope, payload, id)
		if err != nil {
			return err
		}
		if err = enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
			return err
		}
	}
	if tx != nil {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	// Caller-owned transactions publish through the outbox after their commit.
	if tx != nil {
		r.syncSchedulerAccountSnapshot(baseCtx, id)
	}
	return nil
}

// These fields belong to background services. Merge from the row already locked
// by account editing, including absence after an explicit reset.
func mergeAccountRenewalAndImageState(extra map[string]any, raw []byte) (map[string]any, error) {
	var current map[string]json.RawMessage
	if err := json.Unmarshal(raw, &current); len(raw) > 0 && err != nil {
		return nil, err
	}
	if extra == nil {
		extra = make(map[string]any)
	}
	for _, key := range []string{service.AccountRenewalAnchorExtraKey, service.AccountRenewalCyclesExtraKey, service.AccountRenewalLastAtExtraKey, service.OpenAIMainPoolRateLimitExtraKey} {
		delete(extra, key)
		if value, present, err := decodeAccountExtraJSON(current[key]); err != nil {
			return nil, err
		} else if present {
			extra[key] = value
		}
	}
	limits, _ := extra["model_rate_limits"].(map[string]any)
	limits = copyJSONMap(limits)
	delete(limits, accountImageCooldownScope)
	imageState, present, err := decodeAccountExtraJSON(current[accountImageCooldownScope])
	if err != nil {
		return nil, err
	}
	if present {
		if limits == nil {
			limits = make(map[string]any)
		}
		limits[accountImageCooldownScope] = imageState
	}
	if len(limits) > 0 {
		extra["model_rate_limits"] = limits
	} else {
		delete(extra, "model_rate_limits")
	}
	return extra, nil
}
