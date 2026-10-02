package repository

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRenewalSQLFragments(t *testing.T) {
	fragment := accountRenewalInGraceSQL("a.extra", "a.expires_at", "NOW()")
	require.Equal(t, accountRenewalInGraceSQLAliasANow, fragment)
	for _, part := range []string{"lower(btrim(a.extra", "^[0-9]{1,9}$", "make_interval(days =>", "a.expires_at"} {
		require.Contains(t, fragment, part)
	}
	require.NotContains(t, fragment, "?")
	require.Contains(t, groupAccountAvailableSQL, fragment)
	require.Contains(t, groupAccountTemporarilyLimitedSQL, fragment)
}

func TestNotExpiredPostgresPredicateInGroupQuery(t *testing.T) {
	var query string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &query}))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	now := time.Now()
	mock.ExpectQuery("account groups").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	_, err = client.AccountGroup.Query().Where(dbaccountgroup.HasAccountWith(dbaccount.And(notExpiredPredicate(now)))).All(context.Background())
	require.NoError(t, err)
	require.NotContains(t, query, "?")
	require.Contains(t, query, `"accounts"."extra"->>'auto_renewal_enabled'`)
	require.NotContains(t, query, `"account_groups"."extra"`)
	require.Regexp(t, `make_interval\(days =>.* > \$\d+`, query)
	require.NoError(t, mock.ExpectationsWereMet())
	t.Log(query)
}

func TestAccountRenewalPendingAndAutoPauseSQL(t *testing.T) {
	var query string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &query}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newAccountRepositoryWithSQL(nil, db, nil)
	now := time.Now()
	mock.ExpectQuery("pending").WithArgs(now, 200).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "expires_at", "extra", "config"}).AddRow(1, "test", now, `{"auto_renewal_enabled":true}`, `{"auto_renewal_enabled":true,"auto_renewal_cycle":null,"auto_renewal_grace_days":null}`))
	items, err := repo.ListAccountsPendingRenewal(context.Background(), now, 200)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.True(t, items[0].Extra[service.AccountRenewalEnabledExtraKey].(bool))
	require.Contains(t, query, "a.status = 'active'")
	require.Contains(t, query, accountRenewalInGraceSQL("a.extra", "a.expires_at", "$1"))
	require.Contains(t, query, "str.finished_at > a.expires_at")
	t.Log(query)
	mock.ExpectQuery("pause").WithArgs(now).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	_, err = repo.AutoPauseExpiredAccounts(context.Background(), now)
	require.NoError(t, err)
	require.Contains(t, query, "AND NOT "+accountRenewalInGraceSQL("extra", "expires_at", "$1"))
	t.Log(query)
	mock.ExpectQuery("capacity").WithArgs(sqlmock.AnyArg(), service.StatusActive, sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"group_id"}))
	_, err = repo.ListSchedulableCapacityByGroupIDs(context.Background(), []int64{1})
	require.NoError(t, err)
	require.Contains(t, query, accountRenewalInGraceSQL("a.extra", "a.expires_at", "$3"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRenewalExpiryCAS(t *testing.T) {
	for _, affected := range []int64{0, 1} {
		t.Run(string(rune('0'+affected)), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := newAccountRepositoryWithSQL(client, db, nil)
			old := time.Now()
			next := old.AddDate(0, 1, 0)
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT id FROM accounts.*FOR NO KEY UPDATE`).WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(9)))
			mock.ExpectExec(`(?s)UPDATE accounts.*WHERE id = \$3 AND deleted_at IS NULL AND expires_at = \$4`).WithArgs(next, `{"auto_renewal_cycles":1}`, int64(9), old, `{"auto_renewal_enabled":true}`).WillReturnResult(sqlmock.NewResult(0, affected))
			if affected == 1 {
				mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WithArgs(service.SchedulerOutboxEventAccountChanged, int64(9), nil, nil, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			err = repo.RenewAccountExpiry(context.Background(), 9, next, old, json.RawMessage(`{"auto_renewal_enabled":true}`), map[string]any{service.AccountRenewalCyclesExtraKey: 1})
			if affected == 0 {
				require.ErrorIs(t, err, service.ErrAccountRenewalConflict)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
