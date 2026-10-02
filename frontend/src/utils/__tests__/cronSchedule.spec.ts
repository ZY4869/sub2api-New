import { describe, expect, it } from 'vitest'
import { buildCron, describeCron, parseCron } from '../cronSchedule'

describe('cron schedule', () => {
  it('round-trips daily, weekly and regular hourly schedules', () => {
    for (const expression of ['0 7 * * *', '15 9 * * 1,3,5', '5 */6 * * *', '0 * * * *']) {
      const parsed = parseCron(expression)
      expect(buildCron(parsed.mode, parsed)).toBe(expression)
    }
    expect(buildCron('daily', { hour: 7 })).toBe('0 7 * * *')
    expect(parseCron('0 7 * * 7').weekdays).toEqual([0])
  })
  it('preserves historical expressions it cannot represent', () => {
    for (const expression of ['*/30 * * * *', '@daily', '0 7 * * MON-FRI', '0 7 1 * *', '0 */5 * * *', '99 25 * * *', '0 7 * * 9', '  custom  ']) {
      const parsed = parseCron(expression)
      expect(parsed.mode).toBe('custom')
      expect(buildCron('custom', parsed)).toBe(expression)
    }
  })
  it('rejects invalid generated schedules and describes supported schedules', () => {
    expect(() => buildCron('weekly', { weekdays: [] })).toThrow()
    expect(() => buildCron('hourly', { everyHours: 5 })).toThrow()
    expect(() => buildCron('daily', { minute: 60 })).toThrow()
    expect(describeCron('0 7 * * *')).toBe('每天 07:00')
    expect(describeCron('0 7 * * *', 'en')).toBe('Daily at 07:00')
    expect(describeCron('*/30 * * * *')).toBe('*/30 * * * *')
  })
})
