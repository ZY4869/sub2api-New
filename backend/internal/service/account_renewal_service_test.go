//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type renewalTestRepo struct {
	candidates []AccountRenewalCandidate
	newExpiry  time.Time
	expected   time.Time
	config     json.RawMessage
	updates    map[string]any
	err        error
}

func (r *renewalTestRepo) ListAccountsPendingRenewal(context.Context, time.Time, int) ([]AccountRenewalCandidate, error) {
	return r.candidates, nil
}
func (r *renewalTestRepo) RenewAccountExpiry(_ context.Context, _ int64, next, expected time.Time, config json.RawMessage, updates map[string]any) error {
	r.newExpiry, r.expected, r.updates, r.config = next, expected, updates, config
	return r.err
}

func TestAccountRenewalServiceRenewOne(t *testing.T) {
	expiry := time.Date(2026, 9, 1, 8, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name, cycle, anchor string
		cycles              int
		err                 error
		year                int
		month               time.Month
	}{
		{"missing anchor", "month", "", 0, nil, 2026, time.October},
		{"manual expiry", "month", "2026-01-31T00:00:00Z", 1, nil, 2026, time.October},
		{"changed to annual", "year", "2026-06-01T00:00:00Z", 3, nil, 2027, time.September},
		{"CAS conflict", "month", "", 0, ErrAccountRenewalConflict, 2026, time.October},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &renewalTestRepo{err: tc.err}
			s := NewAccountRenewalService(repo, time.Minute)
			ok := s.renewOne(context.Background(), AccountRenewalCandidate{ID: 1, ExpiresAt: expiry, RenewalConfigSnapshot: json.RawMessage(`{"auto_renewal_enabled":true}`), Extra: map[string]any{AccountRenewalEnabledExtraKey: true, AccountRenewalCycleExtraKey: tc.cycle, AccountRenewalAnchorExtraKey: tc.anchor, AccountRenewalCyclesExtraKey: tc.cycles}}, expiry.Add(24*time.Hour))
			require.Equal(t, tc.err == nil, ok)
			require.Equal(t, expiry, repo.expected)
			require.JSONEq(t, `{"auto_renewal_enabled":true}`, string(repo.config))
			require.Equal(t, tc.year, repo.newExpiry.Year())
			require.Equal(t, tc.month, repo.newExpiry.Month())
			require.Equal(t, 1, repo.newExpiry.Day())
			require.Equal(t, 1, repo.updates[AccountRenewalCyclesExtraKey])
		})
	}
}

func TestAccountRenewalServiceStoppedScanDoesNotRenew(t *testing.T) {
	repo := &renewalTestRepo{candidates: []AccountRenewalCandidate{{ID: 1, ExpiresAt: time.Now().Add(-time.Hour), Extra: map[string]any{AccountRenewalEnabledExtraKey: true}}}}
	s := NewAccountRenewalService(repo, time.Minute)
	s.Stop()
	s.runOnce()
	require.True(t, repo.newExpiry.IsZero())
}
