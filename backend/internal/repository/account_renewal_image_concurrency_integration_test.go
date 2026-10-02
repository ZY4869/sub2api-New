//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newRenewalImageTestAccount(t *testing.T) (*accountRepository, *service.Account) {
	t.Helper()
	client := testEntClient(t)
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	a := mustCreateAccount(t, client, &service.Account{
		Name: t.Name(), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{service.AccountRenewalEnabledExtraKey: true, service.AccountRenewalCycleExtraKey: "month", service.AccountRenewalGraceDaysExtraKey: 7},
	})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec("DELETE FROM scheduler_outbox WHERE account_id = $1", a.ID)
		_, _ = integrationDB.Exec("DELETE FROM accounts WHERE id = $1", a.ID)
	})
	return repo, a
}

func storedImageCooldown(t *testing.T, repo *accountRepository, id int64) map[string]any {
	t.Helper()
	a, err := repo.GetByID(context.Background(), id)
	require.NoError(t, err)
	limits, ok := a.Extra["model_rate_limits"].(map[string]any)
	require.True(t, ok)
	state, ok := limits[accountImageCooldownScope].(map[string]any)
	require.True(t, ok)
	return state
}

func imageTestOutboxCount(t *testing.T, id int64) int {
	t.Helper()
	var count int
	require.NoError(t, integrationDB.QueryRow("SELECT count(*) FROM scheduler_outbox WHERE account_id = $1", id).Scan(&count))
	return count
}

func TestImageCooldownPersistenceAndRollback(t *testing.T) {
	ctx := context.Background()
	repo, a := newRenewalImageTestAccount(t)
	cache := &schedulerCacheRecorder{}
	repo.schedulerCache = cache
	longer := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	require.NoError(t, repo.SetModelRateLimit(ctx, a.ID, "other:model", longer, "other"))
	require.NoError(t, repo.SetModelRateLimit(ctx, a.ID, accountImageCooldownScope, longer, "quota"))
	initial := storedImageCooldown(t, repo, a.ID)
	count := imageTestOutboxCount(t, a.ID)
	cacheWrites := len(cache.setAccounts)
	for _, shorter := range []time.Time{longer.Add(-59 * time.Minute), longer, longer.Add(500 * time.Millisecond)} {
		require.NoError(t, repo.SetModelRateLimit(ctx, a.ID, accountImageCooldownScope, shorter, "must-not-replace"))
		require.Equal(t, initial, storedImageCooldown(t, repo, a.ID))
		require.Equal(t, count, imageTestOutboxCount(t, a.ID))
		cacheWrites++
		require.Len(t, cache.setAccounts, cacheWrites)
		require.Equal(t, initial, cache.setAccounts[cacheWrites-1].Extra["model_rate_limits"].(map[string]any)[accountImageCooldownScope])
	}
	require.NoError(t, repo.SetModelRateLimit(ctx, a.ID, accountImageCooldownScope, longer.Add(time.Hour), "extended"))
	require.Equal(t, "extended", storedImageCooldown(t, repo, a.ID)["reason"])
	current, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Contains(t, current.Extra["model_rate_limits"], "other:model")

	tx, err := testEntClient(t).Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	count = imageTestOutboxCount(t, a.ID)
	cacheWrites = len(cache.setAccounts)
	require.NoError(t, repo.SetModelRateLimit(dbent.NewTxContext(ctx, tx), a.ID, accountImageCooldownScope, longer.Add(3*time.Hour), "rolled-back"))
	require.Len(t, cache.setAccounts, cacheWrites, "an uncommitted outer transaction cannot publish a snapshot")
	require.NoError(t, tx.Rollback())
	require.Equal(t, "extended", storedImageCooldown(t, repo, a.ID)["reason"])
	require.Equal(t, count, imageTestOutboxCount(t, a.ID))
}

func TestImageCooldownConcurrentConnectionsKeepLongest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo, a := newRenewalImageTestAccount(t)
	longer := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	first, err := testEntClient(t).Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Rollback() })
	second, err := testEntClient(t).Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Rollback() })
	secondCtx := dbent.NewTxContext(ctx, second)
	rows, err := second.Client().QueryContext(secondCtx, "SELECT pg_backend_pid()")
	require.NoError(t, err)
	require.True(t, rows.Next())
	var pid int
	require.NoError(t, rows.Scan(&pid))
	require.NoError(t, rows.Close())
	require.NoError(t, repo.SetModelRateLimit(dbent.NewTxContext(ctx, first), a.ID, accountImageCooldownScope, longer, "long"))
	done := make(chan error, 1)
	go func() {
		err := repo.SetModelRateLimit(secondCtx, a.ID, accountImageCooldownScope, longer.Add(-59*time.Minute), "short")
		if err == nil {
			err = second.Commit()
		}
		done <- err
	}()
	// Observe a real lock wait, rather than assuming goroutine scheduling order.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')", pid).Scan(&waiting))
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("second writer completed before the first committed: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	require.NoError(t, first.Commit())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	state := storedImageCooldown(t, repo, a.ID)
	require.Equal(t, longer.Format(time.RFC3339), state["rate_limit_reset_at"])
	require.Equal(t, "long", state["reason"])
	require.Equal(t, 1, imageTestOutboxCount(t, a.ID))
}

func renewalImageCandidate(t *testing.T, repo *accountRepository, id int64) service.AccountRenewalCandidate {
	t.Helper()
	expiry := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	_, err := integrationDB.Exec("UPDATE accounts SET expires_at=$1, last_used_at=NOW(), auto_pause_on_expired=true WHERE id=$2", expiry, id)
	require.NoError(t, err)
	items, err := repo.ListAccountsPendingRenewal(context.Background(), time.Now(), 200)
	require.NoError(t, err)
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatal("expected renewal candidate")
	return service.AccountRenewalCandidate{}
}

func TestAccountRenewalRejectsChangedCandidates(t *testing.T) {
	for _, change := range []string{"disabled", "cycle", "grace", "expiry", "status", "deleted", "grace elapsed"} {
		t.Run(change, func(t *testing.T) {
			repo, a := newRenewalImageTestAccount(t)
			candidate := renewalImageCandidate(t, repo, a.ID)
			var err error
			switch change {
			case "disabled", "cycle", "grace":
				patch := map[string]any{}
				switch change {
				case "disabled":
					patch[service.AccountRenewalEnabledExtraKey] = false
				case "cycle":
					patch[service.AccountRenewalCycleExtraKey] = "year"
				case "grace":
					patch[service.AccountRenewalGraceDaysExtraKey] = 9
				}
				raw, marshalErr := json.Marshal(patch)
				require.NoError(t, marshalErr)
				_, err = integrationDB.Exec("UPDATE accounts SET extra = extra || $1::jsonb WHERE id=$2", string(raw), a.ID)
			case "expiry":
				_, err = integrationDB.Exec("UPDATE accounts SET expires_at=expires_at+interval '1 hour' WHERE id=$1", a.ID)
			case "status":
				_, err = integrationDB.Exec("UPDATE accounts SET status='disabled' WHERE id=$1", a.ID)
			case "deleted":
				_, err = integrationDB.Exec("UPDATE accounts SET deleted_at=NOW() WHERE id=$1", a.ID)
			case "grace elapsed":
				// Represent an aged scan without waiting seven days; expiry/config
				// still match, so only the write-time grace check can reject it.
				candidate.ExpiresAt = time.Now().UTC().Truncate(time.Second).Add(-8 * 24 * time.Hour)
				_, err = integrationDB.Exec("UPDATE accounts SET expires_at=$1 WHERE id=$2", candidate.ExpiresAt, a.ID)
			}
			require.NoError(t, err)
			var before time.Time
			require.NoError(t, integrationDB.QueryRow("SELECT expires_at FROM accounts WHERE id=$1", a.ID).Scan(&before))
			err = repo.RenewAccountExpiry(context.Background(), a.ID, time.Now().AddDate(0, 1, 0), candidate.ExpiresAt, candidate.RenewalConfigSnapshot, map[string]any{service.AccountRenewalCyclesExtraKey: 1})
			require.ErrorIs(t, err, service.ErrAccountRenewalConflict)
			var after time.Time
			require.NoError(t, integrationDB.QueryRow("SELECT expires_at FROM accounts WHERE id=$1", a.ID).Scan(&after))
			require.True(t, before.Equal(after))
			require.Zero(t, imageTestOutboxCount(t, a.ID))
		})
	}
}

func TestAccountRenewalRawConfigAndUnrelatedUpdates(t *testing.T) {
	repo, a := newRenewalImageTestAccount(t)
	_, err := integrationDB.Exec(`UPDATE accounts SET extra=jsonb_set(extra, '{auto_renewal_grace_days}', '9007199254740993'::jsonb) WHERE id=$1`, a.ID)
	require.NoError(t, err)
	candidate := renewalImageCandidate(t, repo, a.ID)
	require.Contains(t, string(candidate.RenewalConfigSnapshot), "9007199254740993")
	_, err = integrationDB.Exec(`UPDATE accounts SET extra=extra||'{"unrelated":42}'::jsonb WHERE id=$1`, a.ID)
	require.NoError(t, err)
	next := candidate.ExpiresAt.AddDate(0, 1, 0)
	require.NoError(t, repo.RenewAccountExpiry(context.Background(), a.ID, next, candidate.ExpiresAt, candidate.RenewalConfigSnapshot, map[string]any{service.AccountRenewalCyclesExtraKey: 1}))
	got, err := repo.GetByID(context.Background(), a.ID)
	require.NoError(t, err)
	require.True(t, next.Equal(*got.ExpiresAt))
	require.Equal(t, float64(42), got.Extra["unrelated"])
}

func TestAdminEditPreservesConcurrentRenewalAndImageState(t *testing.T) {
	for _, mode := range []string{"omitted", "set", "clear", "negative clear"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			repo, a := newRenewalImageTestAccount(t)
			candidate := renewalImageCandidate(t, repo, a.ID)
			stale, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			next := candidate.ExpiresAt.AddDate(0, 1, 0)
			managed := map[string]any{service.AccountRenewalAnchorExtraKey: candidate.ExpiresAt.UTC().Format(time.RFC3339), service.AccountRenewalCyclesExtraKey: 1, service.AccountRenewalLastAtExtraKey: time.Now().UTC().Format(time.RFC3339)}
			require.NoError(t, repo.RenewAccountExpiry(ctx, a.ID, next, candidate.ExpiresAt, candidate.RenewalConfigSnapshot, managed))
			require.NoError(t, repo.SetModelRateLimit(ctx, a.ID, accountImageCooldownScope, time.Now().Add(time.Hour), "quota"))
			imageBefore := storedImageCooldown(t, repo, a.ID)
			var intent *int64
			var desired int64
			switch mode {
			case "set":
				desired = next.AddDate(0, 2, 0).Unix()
				intent = &desired
			case "clear":
				intent = &desired
			case "negative clear":
				desired = -1
				intent = &desired
			}
			stale.Name = "name-only-edit"
			require.NoError(t, repo.UpdateWithAccountBillingSettings(ctx, stale, nil, nil, nil, intent))
			got, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, "name-only-edit", got.Name)
			switch mode {
			case "omitted":
				require.True(t, next.Equal(*got.ExpiresAt))
			case "set":
				require.Equal(t, desired, got.ExpiresAt.Unix())
			default:
				require.Nil(t, got.ExpiresAt)
			}
			require.Equal(t, got.ExpiresAt, stale.ExpiresAt)
			require.Equal(t, float64(1), got.Extra[service.AccountRenewalCyclesExtraKey])
			require.Equal(t, got.Extra, stale.Extra)
			require.Equal(t, imageBefore, storedImageCooldown(t, repo, a.ID))

			// A later ordinary edit must not resurrect a cooldown explicitly cleared.
			require.NoError(t, repo.ClearModelRateLimits(ctx, a.ID))
			require.NoError(t, repo.UpdateWithAccountBillingSettings(ctx, stale, nil, nil, nil, nil))
			got, err = repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			limits, _ := got.Extra["model_rate_limits"].(map[string]any)
			require.NotContains(t, limits, accountImageCooldownScope)
		})
	}
}

func TestGeneralAccountUpdateRetainsExpiryPrecision(t *testing.T) {
	repo, a := newRenewalImageTestAccount(t)
	expiry := time.Date(2027, 1, 1, 0, 0, 0, 123456000, time.UTC)
	a.ExpiresAt = &expiry
	require.NoError(t, repo.Update(context.Background(), a))
	got, err := repo.GetByID(context.Background(), a.ID)
	require.NoError(t, err)
	require.True(t, expiry.Equal(*got.ExpiresAt))
}

func TestAccountRenewalGraceCheckedAfterLockWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo, a := newRenewalImageTestAccount(t)
	var now time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&now))
	expiry := now.Add(-7*24*time.Hour + 500*time.Millisecond)
	_, err := integrationDB.ExecContext(ctx, "UPDATE accounts SET expires_at=$1,last_used_at=NOW() WHERE id=$2", expiry, a.ID)
	require.NoError(t, err)
	candidates, err := repo.ListAccountsPendingRenewal(ctx, now, 200)
	require.NoError(t, err)
	var candidate service.AccountRenewalCandidate
	for _, item := range candidates {
		if item.ID == a.ID {
			candidate = item
		}
	}
	require.Equal(t, a.ID, candidate.ID)
	lock, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback() }()
	var id int64
	require.NoError(t, lock.QueryRowContext(ctx, "SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE", a.ID).Scan(&id))
	done := make(chan error, 1)
	go func() {
		done <- repo.RenewAccountExpiry(ctx, a.ID, expiry.AddDate(0, 1, 0), candidate.ExpiresAt, candidate.RenewalConfigSnapshot, map[string]any{service.AccountRenewalCyclesExtraKey: 1})
	}()
	// Hold the row across the deadline without changing its version.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var elapsed bool
		require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT clock_timestamp()>$1::timestamptz+interval '7 days'", candidate.ExpiresAt).Scan(&elapsed))
		if elapsed {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("renewal bypassed lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	require.NoError(t, lock.Commit())
	select {
	case err := <-done:
		require.ErrorIs(t, err, service.ErrAccountRenewalConflict)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Zero(t, imageTestOutboxCount(t, a.ID))
}
