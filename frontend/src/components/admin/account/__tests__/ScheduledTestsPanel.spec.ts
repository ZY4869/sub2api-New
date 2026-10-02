import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ScheduledTestsPanel from '../ScheduledTestsPanel.vue'
import type { ScheduledTestPlan } from '@/types'

const { listByAccount, update } = vi.hoisted(() => ({ listByAccount: vi.fn(), update: vi.fn() }))

vi.mock('@/api/admin', () => ({ adminAPI: { scheduledTests: { listByAccount, update } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({
  ...(await vi.importActual<typeof import('vue-i18n')>('vue-i18n')),
  useI18n: () => ({ t: (key: string) => key, locale: 'zh' })
}))

const plan: ScheduledTestPlan = {
  id: 5, account_id: 1, model_id: '', cron_expression: '0 7 * * *', enabled: true, max_results: 100,
  auto_recover: false, last_run_at: null, next_run_at: null, created_at: '', updated_at: ''
}

async function openPanel() {
  const wrapper = mount(ScheduledTestsPanel, {
    props: { show: false, accountId: 1, modelOptions: [{ value: 'gpt-5.4', label: 'gpt-5.4' }] },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /></div>' },
        ConfirmDialog: true,
        HelpTooltip: true,
        Select: { props: ['placeholder'], template: '<div class="select-stub">{{ placeholder }}</div>' }
      }
    }
  })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

// [local] 批量下发的“平台默认模型”计划必须能在单账号面板中继续编辑。
describe('ScheduledTestsPanel default-model plans', () => {
  beforeEach(() => {
    listByAccount.mockReset().mockResolvedValue([plan])
    update.mockReset().mockImplementation(async (id: number, request: Partial<ScheduledTestPlan>) => ({ ...plan, ...request, id }))
  })

  it('labels a blank model and saves edits without forcing a model choice', async () => {
    const wrapper = await openPanel()
    expect(wrapper.text()).toContain('admin.scheduledTests.platformDefaultModel')

    await wrapper.get('button[title="admin.scheduledTests.editPlan"]').trigger('click')
    expect(wrapper.get('.select-stub').text()).toBe('admin.scheduledTests.defaultModel')
    const save = wrapper.findAll('button').find(button => button.text() === 'common.save')!
    expect(save.attributes('disabled')).toBeUndefined()

    await save.trigger('click')
    await flushPromises()
    expect(update).toHaveBeenCalledWith(5, { model_id: '', cron_expression: '0 7 * * *', max_results: 100, enabled: true, auto_recover: false })
    wrapper.unmount()
  })

  it('still requires a cron expression before saving', async () => {
    listByAccount.mockResolvedValue([{ ...plan, cron_expression: '' }])
    const wrapper = await openPanel()
    await wrapper.get('button[title="admin.scheduledTests.editPlan"]').trigger('click')
    const save = wrapper.findAll('button').find(button => button.text() === 'common.save')!
    expect(save.attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })
})
