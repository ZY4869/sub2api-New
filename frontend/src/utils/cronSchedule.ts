export type CronMode = 'daily' | 'weekly' | 'hourly' | 'custom'
export interface CronSchedule {
  mode: CronMode
  hour: number
  minute: number
  weekdays: number[]
  everyHours: number
  expression: string
}

// Only divisors of 24 represent a regular interval across midnight in a five-field cron.
export const cronHourIntervals = [1, 2, 3, 4, 6, 8, 12] as const

export function buildCron(mode: CronMode, options: Partial<CronSchedule> = {}): string {
  if (mode === 'custom') return options.expression ?? ''
  const minute = options.minute ?? 0
  const hour = options.hour ?? 7
  if (!Number.isInteger(minute) || minute < 0 || minute > 59) throw new RangeError('Invalid cron minute')
  if (mode === 'hourly') {
    const interval = options.everyHours ?? 1
    if (!(cronHourIntervals as readonly number[]).includes(interval)) throw new RangeError('Invalid cron hour interval')
    return `${minute} ${interval === 1 ? '*' : `*/${interval}`} * * *`
  }
  if (!Number.isInteger(hour) || hour < 0 || hour > 23) throw new RangeError('Invalid cron hour')
  if (mode === 'daily') return `${minute} ${hour} * * *`
  const weekdays = [...new Set(options.weekdays ?? [1])].sort((a, b) => a - b)
  if (!weekdays.length || weekdays.some(day => !Number.isInteger(day) || day < 0 || day > 6)) throw new RangeError('Invalid cron weekdays')
  return `${minute} ${hour} * * ${weekdays.join(',')}`
}

export function parseCron(expression: string): CronSchedule {
  const result: CronSchedule = { mode: 'custom', hour: 7, minute: 0, weekdays: [1], everyHours: 1, expression }
  const parts = expression.trim().split(/\s+/)
  if (parts.length !== 5) return result
  const [minute = '', hour = '', day, month, weekday = ''] = parts
  if (day !== '*' || month !== '*' || !/^\d+$/.test(minute) || Number(minute) > 59) return result
  result.minute = Number(minute)
  if (weekday === '*' && (hour === '*' || /^\*\/\d+$/.test(hour))) {
    const interval = hour === '*' ? 1 : Number(hour.slice(2))
    if ((cronHourIntervals as readonly number[]).includes(interval)) return { ...result, mode: 'hourly', everyHours: interval }
    return result
  }
  if (!/^\d+$/.test(hour) || Number(hour) > 23) return result
  result.hour = Number(hour)
  if (weekday === '*') return { ...result, mode: 'daily' }
  if (/^[0-7](,[0-7])*$/.test(weekday)) {
    return { ...result, mode: 'weekly', weekdays: [...new Set(weekday.split(',').map(day => Number(day) % 7))].sort((a, b) => a - b) }
  }
  return result
}

export function describeCron(expression: string, locale = 'zh'): string {
  const config = parseCron(expression)
  const clock = `${String(config.hour).padStart(2, '0')}:${String(config.minute).padStart(2, '0')}`
  const english = locale.startsWith('en')
  if (config.mode === 'daily') return english ? `Daily at ${clock}` : `每天 ${clock}`
  if (config.mode === 'hourly') return english ? `Every ${config.everyHours} hour(s), minute ${config.minute}` : `每 ${config.everyHours} 小时，第 ${config.minute} 分钟`
  if (config.mode === 'weekly') {
    const names = english ? ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'] : ['日', '一', '二', '三', '四', '五', '六']
    return `${english ? '' : '每周'}${config.weekdays.map(day => names[day]).join(english ? ', ' : '、')} ${clock}`
  }
  return expression
}
