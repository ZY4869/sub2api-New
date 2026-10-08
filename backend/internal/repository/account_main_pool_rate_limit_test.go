package repository

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type mainPoolMarkerPayload struct {
	reset  time.Time
	reason string
}

func (p mainPoolMarkerPayload) Match(value driver.Value) bool {
	var raw []byte
	switch v := value.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return false
	}
	var payload map[string]string
	if json.Unmarshal(raw, &payload) != nil {
		return false
	}
	resetAt, err := time.Parse(time.RFC3339Nano, payload["reset_at"])
	return err == nil && resetAt.Equal(p.reset) && payload["reason"] == p.reason && payload["limited_at"] != ""
}

func TestSetOpenAIMainPoolRateLimitedWritesColumnsAndMarkerTogether(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)
	reset := time.Now().Add(time.Hour).Round(0)

	mock.ExpectExec(`(?s)UPDATE accounts SET.*rate_limited_at = \$1.*rate_limit_reset_at = \$2.*jsonb_set.*deleted_at IS NULL`).
		WithArgs(sqlmock.AnyArg(), reset, service.OpenAIMainPoolRateLimitExtraKey, mainPoolMarkerPayload{reset, "codex_window_exhausted"}, int64(88)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).WillReturnResult(sqlmock.NewResult(1, 1))

	require.NoError(t, repo.SetOpenAIMainPoolRateLimited(context.Background(), 88, reset, "codex_window_exhausted"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerMetadataKeepsOpenAIMainPoolMarker(t *testing.T) {
	marker := map[string]any{"reset_at": "2026-10-05T10:00:00Z", "reason": "codex_window_exhausted"}
	account := buildSchedulerMetadataAccount(service.Account{ID: 1, Extra: map[string]any{
		service.OpenAIMainPoolRateLimitExtraKey: marker,
		"unrelated_display_state":               true,
	}})
	require.Equal(t, marker, account.Extra[service.OpenAIMainPoolRateLimitExtraKey])
	require.NotContains(t, account.Extra, "unrelated_display_state")
}

func TestAccountEditMergeUsesLockedMainPoolMarker(t *testing.T) {
	stale := map[string]any{service.OpenAIMainPoolRateLimitExtraKey: map[string]any{"reset_at": "stale"}, "keep": true}
	merged, err := mergeAccountRenewalAndImageState(stale, []byte(`{"openai_main_pool_rate_limit":{"reset_at":"2026-10-05T10:00:00Z"}}`))
	require.NoError(t, err)
	require.Equal(t, "2026-10-05T10:00:00Z", merged[service.OpenAIMainPoolRateLimitExtraKey].(map[string]any)["reset_at"])
	require.Equal(t, true, merged["keep"])

	merged, err = mergeAccountRenewalAndImageState(map[string]any{service.OpenAIMainPoolRateLimitExtraKey: map[string]any{"reset_at": "stale"}}, []byte(`{"openai_main_pool_rate_limit":null}`))
	require.NoError(t, err)
	require.NotContains(t, merged, service.OpenAIMainPoolRateLimitExtraKey, "an absent locked marker must not be revived by the form")
}
