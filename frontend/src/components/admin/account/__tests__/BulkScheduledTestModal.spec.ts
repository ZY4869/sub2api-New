import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import BulkScheduledTestModal from '../BulkScheduledTestModal.vue'
import { scheduledTestsAPI } from '@/api/admin/scheduledTests'

vi.mock('@/api/admin/scheduledTests', () => ({ scheduledTestsAPI: { listByAccount: vi.fn(), create: vi.fn(), update: vi.fn() } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, values?: object) => `${key}${values ? JSON.stringify(values) : ''}`, locale: 'zh' }) }))

function render(ids = [1, 2, 3]) {
  return mount(BulkScheduledTestModal, {
    props: { show: true, accountIds: ids },
    global: { stubs: { BaseDialog: { template: '<div><slot /></div>' }, Toggle: true, ScheduleCronField: true } }
  })
}

describe('bulk scheduled plans', () => {
  beforeEach(() => { vi.resetAllMocks() })

  it.each(['skip', 'overwrite', 'append'] as const)('applies %s and continues after an account fails', async policy => {
    vi.mocked(scheduledTestsAPI.listByAccount).mockImplementation(async id => {
      if (id === 3) throw new Error('account unavailable')
      return id === 1 ? [{ id: 10 }, { id: 11 }] as Awaited<ReturnType<typeof scheduledTestsAPI.listByAccount>> : []
    })
    const wrapper = render()
    await wrapper.get('select').setValue(policy)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    const expected = { model_id: '', cron_expression: '0 7 * * *', enabled: true, auto_recover: false, max_results: 100 }
    expect(scheduledTestsAPI.create).toHaveBeenCalledWith({ ...expected, account_id: 2 })
    if (policy === 'overwrite') {
      expect(scheduledTestsAPI.update).toHaveBeenCalledWith(10, expected)
      expect(scheduledTestsAPI.update).not.toHaveBeenCalledWith(11, expect.anything())
    } else {
      expect(scheduledTestsAPI.update).not.toHaveBeenCalled()
      if (policy === 'append') expect(scheduledTestsAPI.create).toHaveBeenCalledWith({ ...expected, account_id: 1 })
      else expect(scheduledTestsAPI.create).not.toHaveBeenCalledWith(expect.objectContaining({ account_id: 1 }))
    }
    expect(wrapper.text()).toContain('#3: account unavailable')
    expect(wrapper.text()).toContain('"failed":1')
    expect(wrapper.get('button[type="submit"]').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })

  it('shows the server message when the API client rejects with a plain object', async () => {
    vi.mocked(scheduledTestsAPI.listByAccount).mockResolvedValue([])
    vi.mocked(scheduledTestsAPI.create).mockImplementation(async request => {
      if (request.account_id === 2) throw { status: 400, code: 400, message: 'invalid cron expression' }
      return {} as Awaited<ReturnType<typeof scheduledTestsAPI.create>>
    })
    const wrapper = render([1, 2])
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('#2: invalid cron expression')
    expect(wrapper.text()).toContain('"success":1')
    wrapper.unmount()
  })

  it('limits concurrent accounts to five and keeps the target set stable', async () => {
    const release: (() => void)[] = []
    let active = 0
    let peak = 0
    vi.mocked(scheduledTestsAPI.listByAccount).mockImplementation(async () => {
      active++
      peak = Math.max(peak, active)
      await new Promise<void>(resolve => release.push(resolve))
      active--
      return []
    })
    const ids = Array.from({ length: 12 }, (_, index) => index + 1)
    const wrapper = render(ids)
    await wrapper.get('form').trigger('submit')
    expect(active).toBe(5)
    await wrapper.setProps({ accountIds: [999] })
    for (let batch = 0; batch < 3; batch++) {
      release.splice(0).forEach(resolve => resolve())
      await flushPromises()
    }
    expect(peak).toBe(5)
    expect(vi.mocked(scheduledTestsAPI.create).mock.calls.map(([request]) => request.account_id).sort((a, b) => a - b)).toEqual(ids)
    expect(wrapper.text()).toContain('"success":12')
    wrapper.unmount()
  })
})
