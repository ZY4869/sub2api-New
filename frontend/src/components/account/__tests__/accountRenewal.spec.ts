import { describe, expect, it } from 'vitest'
import { accountRenewalGraceDaysRemaining, readAccountRenewal, withAccountRenewal } from '../accountRenewal'

describe('account renewal', () => {
  it('keeps existing accounts disabled and normalizes imported config', () => {
    expect(readAccountRenewal()).toEqual({ enabled: false, cycle: 'month', graceDays: 7 })
    expect(readAccountRenewal({ auto_renewal_enabled: ' TRUE ', auto_renewal_cycle: 'YEARLY', auto_renewal_grace_days: '999999999999999999' })).toEqual({ enabled: true, cycle: 'year', graceDays: 365 })
    expect(readAccountRenewal({ auto_renewal_enabled: 2, auto_renewal_grace_days: -1 })).toEqual({ enabled: false, cycle: 'month', graceDays: 7 })
    expect(readAccountRenewal({ auto_renewal_grace_days: ' 3 ' }).graceDays).toBe(7)
  })
  it('writes canonical config without mutating unrelated fields', () => {
    const source = { other: 42 }
    expect(withAccountRenewal(source, { enabled: true, cycle: 'month', graceDays: 7 })).toEqual({ other: 42, auto_renewal_enabled: true, auto_renewal_cycle: 'month', auto_renewal_grace_days: 7 })
    expect(source).toEqual({ other: 42 })
  })
  it('handles grace boundaries and missing expiry', () => {
    const expiry = new Date(2026, 8, 1, 12).getTime() / 1000
    const extra = { auto_renewal_enabled: true }
    expect(accountRenewalGraceDaysRemaining(expiry, extra, expiry * 1000 - 1)).toBeNull()
    expect(accountRenewalGraceDaysRemaining(expiry, extra, expiry * 1000)).toBe(7)
    expect(accountRenewalGraceDaysRemaining(expiry, extra, new Date(2026, 8, 8, 11).getTime())).toBe(1)
    expect(accountRenewalGraceDaysRemaining(expiry, extra, new Date(2026, 8, 8, 12).getTime())).toBeNull()
    expect(accountRenewalGraceDaysRemaining(null, extra)).toBeNull()
    expect(accountRenewalGraceDaysRemaining(expiry, {}, expiry * 1000)).toBeNull()
  })
})
