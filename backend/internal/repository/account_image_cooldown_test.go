package repository

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type imageCooldownPayload struct{ reset time.Time }

func (p imageCooldownPayload) Match(value driver.Value) bool {
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
	return json.Unmarshal(raw, &payload) == nil && payload["rate_limit_reset_at"] == p.reset.UTC().Format(time.RFC3339) && payload["reason"] == "quota"
}

func TestImageCooldownAtomicPersistence(t *testing.T) {
	reset := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	for _, tc := range []struct {
		name                          string
		stored                        any
		write, missing, outboxFailure bool
	}{
		{name: "keep longer", stored: reset.Add(time.Hour).Format(time.RFC3339)},
		{name: "keep equal", stored: reset.Format(time.RFC3339)},
		{name: "extend shorter", stored: reset.Add(-time.Minute).Format(time.RFC3339), write: true},
		{name: "missing cooldown", write: true},
		{name: "malformed timestamp", stored: "not-a-time", write: true},
		{name: "wrong timestamp type", stored: float64(42), write: true},
		{name: "missing account", missing: true},
		{name: "outbox rolls back cooldown", write: true, outboxFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := newAccountRepositoryWithSQL(client, db, nil)
			mock.ExpectBegin()
			rows := sqlmock.NewRows([]string{"image_cooldown"})
			if !tc.missing {
				payload, err := json.Marshal(map[string]any{"rate_limit_reset_at": tc.stored, "reason": "original"})
				require.NoError(t, err)
				rows.AddRow(payload)
			}
			mock.ExpectQuery(`(?s)SELECT.*FROM accounts.*FOR NO KEY UPDATE`).WithArgs(int64(73)).WillReturnRows(rows)
			if tc.write {
				mock.ExpectExec(`(?s)UPDATE accounts SET.*jsonb_set`).WithArgs("openai:image_generation", imageCooldownPayload{reset}, int64(73)).WillReturnResult(sqlmock.NewResult(0, 1))
				outbox := mock.ExpectExec(`INSERT INTO scheduler_outbox`)
				if tc.outboxFailure {
					outbox.WillReturnError(errors.New("outbox unavailable"))
				} else {
					outbox.WillReturnResult(sqlmock.NewResult(1, 1))
				}
			}
			if tc.missing || tc.outboxFailure {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			err = repo.SetModelRateLimit(context.Background(), 73, "openai:image_generation", reset, "quota")
			switch {
			case tc.missing:
				require.ErrorIs(t, err, service.ErrAccountNotFound)
			case tc.outboxFailure:
				require.EqualError(t, err, "outbox unavailable")
			default:
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
