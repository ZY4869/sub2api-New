//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func mustParseRenewalTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}

func TestAddMonthsClamped_ClampsToMonthEnd(t *testing.T) {
	cases := []struct {
		name   string
		from   string
		months int
		want   string
	}{
		// 核心诉求：整月语义，而不是 time.AddDate 的溢出进位（1-31 +1 月会算成 3-03）。
		{"jan31 plus one month lands on feb28", "2026-01-31T00:00:00Z", 1, "2026-02-28T00:00:00Z"},
		{"jan31 plus one month lands on feb29 in leap year", "2028-01-31T00:00:00Z", 1, "2028-02-29T00:00:00Z"},
		{"aug31 plus one month lands on sep30", "2026-08-31T00:00:00Z", 1, "2026-09-30T00:00:00Z"},
		{"mar31 plus one month lands on apr30", "2026-03-31T00:00:00Z", 1, "2026-04-30T00:00:00Z"},
		// 普通日期不受钳位影响。
		{"sep01 plus one month keeps day of month", "2026-09-01T11:37:00Z", 1, "2026-10-01T11:37:00Z"},
		{"dec15 plus one month crosses year", "2026-12-15T08:30:00Z", 1, "2027-01-15T08:30:00Z"},
		{"jan31 plus 12 months stays jan31", "2026-01-31T00:00:00Z", 12, "2027-01-31T00:00:00Z"},
		// 闰日按年推进时需要落到平年的 2-28。
		{"feb29 plus 12 months lands on feb28", "2028-02-29T00:00:00Z", 12, "2029-02-28T00:00:00Z"},
		{"feb29 plus 48 months returns to feb29", "2028-02-29T00:00:00Z", 48, "2032-02-29T00:00:00Z"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := addMonthsClamped(mustParseRenewalTime(t, tc.from), tc.months)
			require.Equal(t, mustParseRenewalTime(t, tc.want), got)
		})
	}
}

func TestAddMonthsClamped_PreservesClockAndLocation(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	from := time.Date(2026, time.January, 31, 23, 45, 30, 0, loc)
	got := addMonthsClamped(from, 1)

	require.Equal(t, 2026, got.Year())
	require.Equal(t, time.February, got.Month())
	require.Equal(t, 28, got.Day())
	require.Equal(t, 23, got.Hour())
	require.Equal(t, 45, got.Minute())
	require.Equal(t, 30, got.Second())
	require.Equal(t, loc.String(), got.Location().String())
}

// 用户场景：9-01 过期，宽限期内 9-02 调用成功，新过期时间必须是 10-01 而非 10-02。
func TestResolveRenewedExpiry_AnchorsOnOriginalExpiry(t *testing.T) {
	anchor := mustParseRenewalTime(t, "2026-09-01T00:00:00Z")
	now := mustParseRenewalTime(t, "2026-09-02T10:00:00Z")

	next, cycles, ok := ResolveRenewedExpiry(anchor, AccountRenewalCycleMonth, 0, now)

	require.True(t, ok)
	require.Equal(t, 1, cycles)
	require.Equal(t, mustParseRenewalTime(t, "2026-10-01T00:00:00Z"), next)
}

// 续期多轮后仍应严格落在锚点日上，不能随续期时刻漂移。
func TestResolveRenewedExpiry_DoesNotDriftAcrossCycles(t *testing.T) {
	anchor := mustParseRenewalTime(t, "2026-09-01T00:00:00Z")

	next, cycles, ok := ResolveRenewedExpiry(
		anchor,
		AccountRenewalCycleMonth,
		1,
		mustParseRenewalTime(t, "2026-10-05T23:59:00Z"),
	)

	require.True(t, ok)
	require.Equal(t, 2, cycles)
	require.Equal(t, mustParseRenewalTime(t, "2026-11-01T00:00:00Z"), next)
}

// 长期无人值守：过期数月后才恢复调用，应一次补齐到当前之后的第一个周期。
func TestResolveRenewedExpiry_CatchesUpAfterLongGap(t *testing.T) {
	anchor := mustParseRenewalTime(t, "2026-09-01T00:00:00Z")
	now := mustParseRenewalTime(t, "2026-12-15T00:00:00Z")

	next, cycles, ok := ResolveRenewedExpiry(anchor, AccountRenewalCycleMonth, 0, now)

	require.True(t, ok)
	require.Equal(t, 4, cycles)
	require.Equal(t, mustParseRenewalTime(t, "2027-01-01T00:00:00Z"), next)
}

func TestResolveRenewedExpiry_YearCycle(t *testing.T) {
	anchor := mustParseRenewalTime(t, "2028-02-29T00:00:00Z")
	now := mustParseRenewalTime(t, "2028-03-02T00:00:00Z")

	next, cycles, ok := ResolveRenewedExpiry(anchor, AccountRenewalCycleYear, 0, now)

	require.True(t, ok)
	require.Equal(t, 1, cycles)
	require.Equal(t, mustParseRenewalTime(t, "2029-02-28T00:00:00Z"), next)
}

func TestResolveRenewedExpiry_RejectsZeroAnchor(t *testing.T) {
	_, _, ok := ResolveRenewedExpiry(time.Time{}, AccountRenewalCycleMonth, 0, time.Now())
	require.False(t, ok)
}

func TestParseAccountRenewalConfig_Defaults(t *testing.T) {
	cfg := ParseAccountRenewalConfig(nil)

	require.False(t, cfg.Enabled)
	require.Equal(t, AccountRenewalCycleMonth, cfg.Cycle)
	require.Equal(t, DefaultAccountRenewalGraceDays, cfg.GraceDays)
	require.Nil(t, cfg.Anchor)
	require.Zero(t, cfg.Cycles)
}

// extra 来自 JSONB，同一字段在不同写入路径下可能是 bool/string/float64，都要能解析。
func TestParseAccountRenewalConfig_AcceptsJSONShapes(t *testing.T) {
	cfg := ParseAccountRenewalConfig(map[string]any{
		AccountRenewalEnabledExtraKey:   "true",
		AccountRenewalCycleExtraKey:     "YEARLY",
		AccountRenewalGraceDaysExtraKey: float64(10),
		AccountRenewalAnchorExtraKey:    "2026-09-01T00:00:00Z",
		AccountRenewalCyclesExtraKey:    float64(3),
	})

	require.True(t, cfg.Enabled)
	require.Equal(t, AccountRenewalCycleYear, cfg.Cycle)
	require.Equal(t, 10, cfg.GraceDays)
	require.NotNil(t, cfg.Anchor)
	require.Equal(t, mustParseRenewalTime(t, "2026-09-01T00:00:00Z"), *cfg.Anchor)
	require.Equal(t, 3, cfg.Cycles)
}

// 损坏的 extra 不能让调度链路报错，只能表现为"未开启自动续期"。
func TestParseAccountRenewalConfig_FallsBackOnGarbage(t *testing.T) {
	cfg := ParseAccountRenewalConfig(map[string]any{
		AccountRenewalEnabledExtraKey:   map[string]any{"nope": 1},
		AccountRenewalCycleExtraKey:     "fortnightly",
		AccountRenewalGraceDaysExtraKey: "not-a-number",
		AccountRenewalAnchorExtraKey:    "31/12/2026",
	})

	require.False(t, cfg.Enabled)
	require.Equal(t, AccountRenewalCycleMonth, cfg.Cycle)
	require.Equal(t, DefaultAccountRenewalGraceDays, cfg.GraceDays)
	require.Nil(t, cfg.Anchor)
}

func TestClampAccountRenewalGraceDays(t *testing.T) {
	require.Equal(t, 0, ClampAccountRenewalGraceDays(0))
	require.Equal(t, 7, ClampAccountRenewalGraceDays(7))
	require.Equal(t, maxAccountRenewalGraceDays, ClampAccountRenewalGraceDays(9999))
	// 负数属于配置错误，回落默认值而不是 0：0 会静默让宽限期失效。
	require.Equal(t, DefaultAccountRenewalGraceDays, ClampAccountRenewalGraceDays(-1))
}

func TestAccountIsWithinRenewalGrace(t *testing.T) {
	expiresAt := mustParseRenewalTime(t, "2026-09-01T00:00:00Z")
	enabledExtra := map[string]any{
		AccountRenewalEnabledExtraKey:   true,
		AccountRenewalGraceDaysExtraKey: 7,
	}

	cases := []struct {
		name  string
		extra map[string]any
		now   string
		want  bool
	}{
		{"not yet expired", enabledExtra, "2026-08-31T23:59:00Z", false},
		{"just expired stays within grace", enabledExtra, "2026-09-01T00:00:01Z", true},
		{"inside grace window", enabledExtra, "2026-09-05T12:00:00Z", true},
		{"last moment of grace", enabledExtra, "2026-09-07T23:59:59Z", true},
		{"grace exhausted", enabledExtra, "2026-09-08T00:00:01Z", false},
		{"renewal disabled", map[string]any{AccountRenewalEnabledExtraKey: false}, "2026-09-02T00:00:00Z", false},
		{"no renewal config", nil, "2026-09-02T00:00:00Z", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ExpiresAt: &expiresAt, Extra: tc.extra}
			require.Equal(t, tc.want, account.IsWithinRenewalGrace(mustParseRenewalTime(t, tc.now)))
		})
	}
}

func TestAccountIsWithinRenewalGrace_NilSafety(t *testing.T) {
	var nilAccount *Account
	require.False(t, nilAccount.IsWithinRenewalGrace(time.Now()))

	noExpiry := &Account{Extra: map[string]any{AccountRenewalEnabledExtraKey: true}}
	require.False(t, noExpiry.IsWithinRenewalGrace(time.Now()))
}

func TestResolveRenewalBase(t *testing.T) {
	loc := time.FixedZone("+08", 8*60*60)
	anchor := time.Date(2026, 1, 31, 0, 15, 0, 123, loc)
	expiry := time.Date(2026, 2, 28, 0, 15, 0, 0, loc)
	for _, tc := range []struct {
		name, cycle        string
		expiry             time.Time
		cfgAnchor          *time.Time
		cycles, wantCycles int
	}{
		{"missing", "month", expiry, nil, 0, 0},
		{"valid local month end", "month", expiry.UTC(), &anchor, 1, 1},
		{"manual expiry", "month", expiry.AddDate(0, 0, 2), &anchor, 1, 0},
		{"month to year", "year", expiry, &anchor, 1, 0},
		{"untrusted cycles", "month", expiry, &anchor, int(^uint(0) >> 1), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, cycles := ResolveRenewalBase(tc.expiry, AccountRenewalConfig{Anchor: tc.cfgAnchor, Cycle: tc.cycle, Cycles: tc.cycles}, loc)
			require.Equal(t, tc.wantCycles, cycles)
			require.Same(t, loc, base.Location())
			require.Zero(t, base.Nanosecond())
			if cycles == 0 {
				require.Equal(t, tc.expiry.Unix(), base.Unix())
			}
			next, _, ok := ResolveRenewedExpiry(base, tc.cycle, cycles, tc.expiry.Add(time.Hour))
			require.True(t, ok)
			if tc.cycle == "year" {
				require.Equal(t, 2027, next.Year())
			}
			if tc.name == "valid local month end" {
				require.Equal(t, 31, next.Day())
				require.Equal(t, time.March, next.Month())
			}
		})
	}
}

func TestNormalizeAccountRenewalExtra(t *testing.T) {
	for _, tc := range []struct {
		enabled, days any
		wantEnabled   bool
		wantDays      int
	}{
		{" TRUE ", "7", true, 7}, {"t", 0, true, 0}, {1, 999, true, 365},
		{2, -1, false, 7}, {"yes", 1.5, false, 7}, {"\ttrue", " 8 ", false, 7},
		{true, "99999999999999999999999999", true, 365}, {false, "0000000000", false, 365},
	} {
		extra := map[string]any{AccountRenewalEnabledExtraKey: tc.enabled, AccountRenewalGraceDaysExtraKey: tc.days, AccountRenewalCycleExtraKey: "YEARLY", "other": 42}
		NormalizeAccountRenewalExtra(extra)
		require.Equal(t, tc.wantEnabled, extra[AccountRenewalEnabledExtraKey])
		require.Equal(t, tc.wantDays, extra[AccountRenewalGraceDaysExtraKey])
		require.Equal(t, "year", extra[AccountRenewalCycleExtraKey])
		require.Equal(t, 42, extra["other"])
	}
	NormalizeAccountRenewalExtra(nil)
	extra := map[string]any{AccountRenewalAnchorExtraKey: "old", AccountRenewalCyclesExtraKey: 4, AccountRenewalLastAtExtraKey: "old", "other": 42}
	stripAccountRenewalManagedExtra(extra)
	NormalizeAccountRenewalExtra(extra)
	require.Equal(t, map[string]any{"other": 42}, extra)
}

func TestAccountIsSchedulableRenewalGrace(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ago           time.Duration
		enabled, want bool
	}{
		{"future", -time.Hour, false, true}, {"within grace", time.Hour, true, true},
		{"exhausted", 8 * 24 * time.Hour, true, false}, {"disabled", time.Hour, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiry := time.Now().Add(-tc.ago)
			a := &Account{Status: StatusActive, Schedulable: true, AutoPauseOnExpired: true, ExpiresAt: &expiry, Extra: map[string]any{AccountRenewalEnabledExtraKey: tc.enabled}}
			require.Equal(t, tc.want, a.IsSchedulable())
			require.Equal(t, tc.want, a.IsCredentialUsableForShadow())
		})
	}
}
