package service

// [local] 账号自动续期（本地定制功能）：extra 配置解析 + 锚点续期日期计算。
//
// 设计要点：
//   - 配置全部存放在 Account.Extra（JSONB）中，不新增 ent schema 字段、不新增 migration，
//     便于跟随上游合并。key 采用扁平命名，与上游既有的 quota_daily_* / crs_account_id 惯例一致。
//   - 续期日期基于「首次设定的过期时间」（anchor）推算，而不是基于"续期发生时刻"。
//     例：锚点 9-01 过期，宽限期内 9-02 调用成功 → 新过期时间是 10-01 而不是 10-02。
//   - 加月必须做月末钳位，语义与前端 frontend/src/components/account/accountExpiry.ts
//     的 getAccountExpiryTimestamp 保持一致。
//
// 注意：本文件也承载 Account 的续期相关方法，好让 account.go 的热路径只需改动一行。

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// 自动续期在 Account.Extra 中使用的 key。
// 其中 Enabled / GraceDays 两个键必须同时登记到 repository 层
// scheduler_cache.go 的 filterSchedulerExtra 白名单，否则调度快照投影会丢弃它们，
// 导致热路径走 Redis 缓存时读不到宽限期配置。
const (
	AccountRenewalEnabledExtraKey   = "auto_renewal_enabled"
	AccountRenewalCycleExtraKey     = "auto_renewal_cycle"
	AccountRenewalGraceDaysExtraKey = "auto_renewal_grace_days"
	AccountRenewalAnchorExtraKey    = "auto_renewal_anchor_at"
	AccountRenewalCyclesExtraKey    = "auto_renewal_cycles"
	AccountRenewalLastAtExtraKey    = "auto_renewal_last_at"
)

// 续期周期。
const (
	AccountRenewalCycleMonth = "month"
	AccountRenewalCycleYear  = "year"
)

const (
	// DefaultAccountRenewalGraceDays 是默认宽限天数：账号过期后仍允许调用的天数。
	DefaultAccountRenewalGraceDays = 7
	// maxAccountRenewalGraceDays 给宽限期设上限，避免误配置成"永不过期"。
	maxAccountRenewalGraceDays = 365
	// maxAccountRenewalCatchUpCycles 限制一次补齐的周期数，纯防御性上限，避免死循环。
	maxAccountRenewalCatchUpCycles = 1200
)

// AccountRenewalConfig 是从 Account.Extra 解析出的自动续期配置。
type AccountRenewalConfig struct {
	Enabled   bool
	Cycle     string // AccountRenewalCycleMonth / AccountRenewalCycleYear
	GraceDays int
	Anchor    *time.Time // 首次设定的过期时间；续期过程中保持不变
	Cycles    int        // 已相对 Anchor 续期的周期数
	LastAt    *time.Time
}

// ParseAccountRenewalConfig 从 extra 解析自动续期配置。
// 对缺失/类型异常的字段一律回落到安全默认值，不返回错误：
// extra 是用户可编辑的自由结构，解析失败时应当表现为"未开启自动续期"而不是让调度链路报错。
func ParseAccountRenewalConfig(extra map[string]any) AccountRenewalConfig {
	cfg := AccountRenewalConfig{
		Cycle:     AccountRenewalCycleMonth,
		GraceDays: DefaultAccountRenewalGraceDays,
	}
	if len(extra) == 0 {
		return cfg
	}

	cfg.Enabled = renewalExtraBool(extra[AccountRenewalEnabledExtraKey])

	if cycle, ok := renewalExtraString(extra[AccountRenewalCycleExtraKey]); ok {
		if normalized := NormalizeAccountRenewalCycle(cycle); normalized != "" {
			cfg.Cycle = normalized
		}
	}

	cfg.GraceDays = renewalGraceDays(extra[AccountRenewalGraceDaysExtraKey])

	if anchor, ok := renewalExtraTime(extra[AccountRenewalAnchorExtraKey]); ok {
		cfg.Anchor = &anchor
	}

	if cycles, ok := renewalExtraInt(extra[AccountRenewalCyclesExtraKey]); ok && cycles > 0 {
		cfg.Cycles = cycles
	}

	if lastAt, ok := renewalExtraTime(extra[AccountRenewalLastAtExtraKey]); ok {
		cfg.LastAt = &lastAt
	}

	return cfg
}

// NormalizeAccountRenewalCycle 归一化周期取值，无法识别时返回空串。
func NormalizeAccountRenewalCycle(cycle string) string {
	switch strings.ToLower(strings.TrimSpace(cycle)) {
	case AccountRenewalCycleMonth, "monthly", "m":
		return AccountRenewalCycleMonth
	case AccountRenewalCycleYear, "yearly", "annual", "y":
		return AccountRenewalCycleYear
	default:
		return ""
	}
}

// NormalizeAccountRenewalExtra 只规范调用者实际提供的配置键，保留增量更新语义。
func NormalizeAccountRenewalExtra(extra map[string]any) {
	cfg := ParseAccountRenewalConfig(extra)
	if _, ok := extra[AccountRenewalEnabledExtraKey]; ok {
		extra[AccountRenewalEnabledExtraKey] = cfg.Enabled
	}
	if _, ok := extra[AccountRenewalCycleExtraKey]; ok {
		extra[AccountRenewalCycleExtraKey] = cfg.Cycle
	}
	if _, ok := extra[AccountRenewalGraceDaysExtraKey]; ok {
		extra[AccountRenewalGraceDaysExtraKey] = cfg.GraceDays
	}
}

func stripAccountRenewalManagedExtra(extra map[string]any) {
	delete(extra, AccountRenewalAnchorExtraKey)
	delete(extra, AccountRenewalCyclesExtraKey)
	delete(extra, AccountRenewalLastAtExtraKey)
}

// ResolveRenewalBase 在服务器本地日历中验证存档锚点；改期、改周期或导入后重新锚定。
func ResolveRenewalBase(expiresAt time.Time, cfg AccountRenewalConfig, loc *time.Location) (time.Time, int) {
	if loc == nil {
		loc = time.Local
	}
	expiresAt = expiresAt.In(loc).Truncate(time.Second)
	if cfg.Anchor != nil && !cfg.Anchor.IsZero() && cfg.Cycles >= 0 && cfg.Cycles <= maxAccountRenewalCatchUpCycles {
		anchor := cfg.Anchor.In(loc).Truncate(time.Second)
		if nextExpiryFromAnchor(anchor, cfg.Cycle, cfg.Cycles).Unix() == expiresAt.Unix() {
			return anchor, cfg.Cycles
		}
	}
	return expiresAt, 0
}

// ClampAccountRenewalGraceDays 把宽限天数钳到 [0, maxAccountRenewalGraceDays]。
// 负数视为配置错误，回落到默认值而不是 0 —— 0 会让宽限期形同虚设，静默改变用户预期。
func ClampAccountRenewalGraceDays(days int) int {
	if days < 0 {
		return DefaultAccountRenewalGraceDays
	}
	if days > maxAccountRenewalGraceDays {
		return maxAccountRenewalGraceDays
	}
	return days
}

// AccountRenewalGraceEnd 返回宽限期结束时刻（不含）。
func AccountRenewalGraceEnd(expiresAt time.Time, graceDays int) time.Time {
	return expiresAt.AddDate(0, 0, ClampAccountRenewalGraceDays(graceDays))
}

// addMonthsClamped 在 t 上加 months 个月，目标月份没有对应日号时钳到该月最后一天。
//
// 必须自行实现而不能直接用 time.AddDate：后者会把 2026-01-31 +1 月算成 2026-03-03（溢出进位），
// 而账号续期需要的是"整月"语义 → 2026-02-28。与前端 getAccountExpiryTimestamp 的行为一致。
func addMonthsClamped(t time.Time, months int) time.Time {
	year, month, day := t.Date()
	hour, minute, sec := t.Clock()
	loc := t.Location()

	// 先落到目标月的 1 号再取月长：1 号在任何月份都存在，AddDate 不会溢出。
	target := time.Date(year, month, 1, hour, minute, sec, t.Nanosecond(), loc).AddDate(0, months, 0)
	if lastDay := daysInMonth(target.Year(), target.Month()); day > lastDay {
		day = lastDay
	}
	return time.Date(target.Year(), target.Month(), day, hour, minute, sec, t.Nanosecond(), loc)
}

// daysInMonth 返回指定年月的天数（下个月的第 0 天即本月最后一天）。
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// nextExpiryFromAnchor 返回锚点经过 cycles 个周期后的过期时间。
// 年周期按 12 个月处理，好让 2 月 29 日同样获得月末钳位（闰年 → 平年得 2 月 28 日）。
func nextExpiryFromAnchor(anchor time.Time, cycle string, cycles int) time.Time {
	if cycle == AccountRenewalCycleYear {
		return addMonthsClamped(anchor, 12*cycles)
	}
	return addMonthsClamped(anchor, cycles)
}

// ResolveRenewedExpiry 计算续期后的新过期时间。
//
// 从 currentCycles 起逐周期递增，返回第一个晚于 now 的过期时间及其周期数；
// 逐周期递增而非一次算出，是为了让长期无人值守（过期数月后才恢复调用）的账号
// 也能一次补齐到当前，且每个中间周期都严格落在锚点日上。
// anchor 为零值或超出补齐上限时返回 ok=false，调用方应跳过本次续期。
func ResolveRenewedExpiry(anchor time.Time, cycle string, currentCycles int, now time.Time) (time.Time, int, bool) {
	if anchor.IsZero() {
		return time.Time{}, 0, false
	}
	if currentCycles < 0 {
		currentCycles = 0
	}
	cycles := currentCycles
	for i := 0; i < maxAccountRenewalCatchUpCycles; i++ {
		cycles++
		next := nextExpiryFromAnchor(anchor, cycle, cycles)
		if next.After(now) {
			return next, cycles, true
		}
	}
	return time.Time{}, 0, false
}

// ----- Account 上的续期辅助方法 -----
// 定义在本文件而非 account.go，是为了把上游文件的改动面压到最小（只有 IsSchedulable 一行）。

// IsWithinRenewalGrace 报告账号当前是否处于"已过期但仍在自动续期宽限期内"。
//
// 热路径考量：调用方（IsSchedulable）已先判定 ExpiresAt != nil 且已过期，
// 因此绝大多数账号不会走到这里，extra 解析的开销不会进入常态调度路径。
func (a *Account) IsWithinRenewalGrace(now time.Time) bool {
	if a == nil || a.ExpiresAt == nil {
		return false
	}
	// 未到期不属于宽限期，交由调用方的常规分支处理。
	if now.Before(*a.ExpiresAt) {
		return false
	}
	cfg := ParseAccountRenewalConfig(a.Extra)
	if !cfg.Enabled {
		return false
	}
	return now.Before(AccountRenewalGraceEnd(*a.ExpiresAt, cfg.GraceDays))
}

// ----- extra 取值辅助 -----
// extra 源自 JSONB，同一个字段在不同写入路径下可能是 bool/string/float64/json.Number，
// 因此每个读取器都要兼容这些形态。

func renewalExtraBool(value any) bool {
	// 与 PostgreSQL lower(btrim(extra->>key)) 保持一致，btrim 默认只去空格。
	switch strings.ToLower(strings.Trim(renewalScalarText(value), " ")) {
	case "true", "t", "1":
		return true
	}
	return false
}

func renewalScalarText(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func renewalGraceDays(value any) int {
	s := renewalScalarText(value)
	if s == "" {
		return DefaultAccountRenewalGraceDays
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return DefaultAccountRenewalGraceDays
		}
	}
	if len(s) > 9 {
		return maxAccountRenewalGraceDays
	}
	days, _ := strconv.Atoi(s)
	return ClampAccountRenewalGraceDays(days)
}

func renewalExtraString(value any) (string, bool) {
	s, ok := value.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

func renewalExtraInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func renewalExtraTime(value any) (time.Time, bool) {
	switch v := value.(type) {
	case time.Time:
		if v.IsZero() {
			return time.Time{}, false
		}
		return v, true
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}, false
		}
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil || parsed.IsZero() {
			return time.Time{}, false
		}
		return parsed, true
	default:
		return time.Time{}, false
	}
}
