export interface AccountRenewalConfig {
  enabled: boolean
  cycle: 'month' | 'year'
  graceDays: number
}

const scalarText = (value: unknown): string =>
  ['string', 'number', 'boolean'].includes(typeof value) ? String(value) : ''

/** Mirrors the backend/JSONB rules, including malformed imported values. */
export function readAccountRenewal(extra?: Record<string, unknown> | null): AccountRenewalConfig {
  const enabled = scalarText(extra?.auto_renewal_enabled).replace(/^ +| +$/g, '').toLowerCase()
  const days = scalarText(extra?.auto_renewal_grace_days)
  const cycle = scalarText(extra?.auto_renewal_cycle).trim().toLowerCase()
  return {
    enabled: ['true', 't', '1'].includes(enabled),
    cycle: ['year', 'yearly', 'annual', 'y'].includes(cycle) ? 'year' : 'month',
    graceDays: /^[0-9]+$/.test(days) ? (days.length > 9 ? 365 : Math.min(Number(days), 365)) : 7
  }
}

export function withAccountRenewal(extra: Record<string, unknown> | undefined, config: AccountRenewalConfig): Record<string, unknown> {
  const normalized = readAccountRenewal({ auto_renewal_enabled: config.enabled, auto_renewal_cycle: config.cycle, auto_renewal_grace_days: config.graceDays })
  return {
    ...extra,
    auto_renewal_enabled: normalized.enabled,
    auto_renewal_cycle: normalized.cycle,
    auto_renewal_grace_days: normalized.graceDays
  }
}

/** null means outside the grace period. Display rounds the remaining partial day up. */
export function accountRenewalGraceDaysRemaining(expiresAt: number | null | undefined, extra?: Record<string, unknown>, now = Date.now()): number | null {
  const config = readAccountRenewal(extra)
  if (!config.enabled || !expiresAt || !Number.isFinite(expiresAt) || now < expiresAt * 1000) return null
  const end = new Date(expiresAt * 1000)
  end.setDate(end.getDate() + config.graceDays)
  const remaining = end.getTime() - now
  return remaining > 0 ? Math.ceil(remaining / 86_400_000) : null
}
