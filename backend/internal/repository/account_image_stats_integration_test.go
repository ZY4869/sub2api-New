//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountImageStatsRecordedOutputs(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "image-count@example.invalid"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "image-count-test-key", Name: "image-count"})
	a := mustCreateAccount(t, client, &service.Account{Name: "image-count-a"})
	b := mustCreateAccount(t, client, &service.Account{Name: "image-count-b"})
	empty := mustCreateAccount(t, client, &service.Account{Name: "image-count-empty"})
	other := mustCreateAccount(t, client, &service.Account{Name: "image-count-not-requested"})
	today := time.Date(2026, 10, 2, 0, 0, 0, 0, time.FixedZone("+08", 8*3600))
	now := today.Add(2 * time.Hour)
	for i, row := range []struct {
		id             int64
		at             time.Time
		images, videos int
		mode           string
	}{
		{a.ID, today.Add(-time.Second), 2, 0, "image"},
		{a.ID, today, 3, 0, "image"},
		{a.ID, today.Add(time.Hour), 1, 0, "token"},
		{a.ID, today, 0, 0, "token"},
		{a.ID, today, 5, 0, "video"},
		{a.ID, today, 7, 1, "token"},
		{a.ID, now.Add(time.Second), 11, 0, "image"},
		{a.ID, today.AddDate(0, 0, -30), 4, 0, ""},
		{b.ID, now, 2, 0, "image"},
		{other.ID, now, 8, 0, "image"},
	} {
		mode := row.mode
		size := "1K"
		log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: row.id,
			RequestID: fmt.Sprintf("image-stat-%d-%d", a.ID, i), Model: "test-image-model", CreatedAt: row.at,
			ImageCount: row.images, ImageSize: &size, VideoCount: row.videos, BillingMode: &mode}
		inserted, err := repo.Create(ctx, log)
		require.NoError(t, err)
		require.True(t, inserted)
		if i == 1 {
			inserted, err = repo.Create(ctx, log)
			require.NoError(t, err)
			require.False(t, inserted)
		}
	}
	got, err := repo.GetAccountImageStatsBatch(ctx, []int64{a.ID, b.ID, empty.ID}, today, now)
	require.NoError(t, err)
	require.Equal(t, &service.AccountImageStats{TodayCount: 4, TotalCount: 10}, got[a.ID])
	require.Equal(t, &service.AccountImageStats{TodayCount: 2, TotalCount: 2}, got[b.ID])
	require.Equal(t, &service.AccountImageStats{}, got[empty.ID])
	require.NotContains(t, got, other.ID)
}
