import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import OpenAIImageQuotaModal from '../OpenAIImageQuotaModal.vue'
import { accountsAPI, type OpenAIImageQuotaStats } from '@/api/admin/accounts'
import { getOpenAIImageQuotaSettings, updateOpenAIImageQuotaSettings } from '@/api/admin/settings'

const toast = vi.hoisted(() => ({ showSuccess: vi.fn(), showError: vi.fn() }))

vi.mock('@/api/admin/accounts', () => ({ accountsAPI: { getOpenAIImageQuotaStats: vi.fn() } }))
vi.mock('@/api/admin/settings', () => ({ getOpenAIImageQuotaSettings: vi.fn(), updateOpenAIImageQuotaSettings: vi.fn() }))
vi.mock('@/stores/app', () => ({ useAppStore: () => toast }))
vi.mock('vue-i18n', async importOriginal => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string, values?: object) => `${key}${values ? JSON.stringify(values) : ''}` })
}))

const stats: OpenAIImageQuotaStats = {
  generated_at: '2026-10-05T00:00:00Z',
  plans: [{ plan_type: 'plus', account_count: 2, observation_count: 3, windows: [{ window_minutes: 180, samples: 3, min_images: 30, median_images: 32, max_images: 34 }] }],
  accounts: [{
    account_id: 7,
    name: 'plus-a',
    plan_type: 'plus',
    main_pool_only_blocked: true,
    image_cooldown_until: '2026-10-05T03:00:00Z',
    image_cooldown_reason: 'openai_image_plan_limit',
    current_window: { source: 'headers', window_minutes: 180, from: '', reset_at: '', snapshot_at: '', used_percent: 50, images: 15, estimated_limit: 30 },
    observations: [{ observed_at: '2026-10-04T00:00:00Z', source: '429', images_in_window: 32 }]
  }]
}

function render() {
  return mount(OpenAIImageQuotaModal, {
    props: { show: true },
    global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } }
  })
}

describe('OpenAIImageQuotaModal', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    vi.mocked(accountsAPI.getOpenAIImageQuotaStats).mockResolvedValue(stats)
    vi.mocked(getOpenAIImageQuotaSettings).mockResolvedValue({ pause_threshold_percent: 100, plan_limits: { prolite: [{ window_minutes: 1440, max_images: 200 }] } })
  })

  it('shows the requested plans with exhaustion samples and saves added rules', async () => {
    vi.mocked(updateOpenAIImageQuotaSettings).mockImplementation(async settings => settings)
    const wrapper = render()
    await flushPromises()

    for (const plan of ['go', 'plus', 'prolite', 'pro', 'promax']) {
      expect(wrapper.find(`[data-testid="image-quota-plan-${plan}"]`).exists()).toBe(true)
    }
    expect(wrapper.get('[data-testid="image-quota-plan-plus"]').text()).toContain('"min":30,"median":32,"max":34,"samples":3')

    await wrapper.get('[data-testid="image-quota-add-plus"]').trigger('click')
    const inputs = wrapper.get('[data-testid="image-quota-plan-plus"]').findAll('input')
    await inputs[0]!.setValue(180)
    await inputs[1]!.setValue(30)
    await wrapper.get('[data-testid="image-quota-threshold"]').setValue(95)
    await wrapper.get('[data-testid="image-quota-save"]').trigger('click')
    await flushPromises()

    expect(updateOpenAIImageQuotaSettings).toHaveBeenCalledWith({
      pause_threshold_percent: 95,
      plan_limits: {
        prolite: [{ window_minutes: 1440, max_images: 200 }],
        plus: [{ window_minutes: 180, max_images: 30 }]
      }
    })
    expect(toast.showSuccess).toHaveBeenCalled()
  })

  it('blocks saving an out-of-range threshold', async () => {
    const wrapper = render()
    await flushPromises()
    await wrapper.get('[data-testid="image-quota-threshold"]').setValue(150)
    expect(wrapper.get('[data-testid="image-quota-save"]').attributes('disabled')).toBeDefined()
  })

  it('lists per-account windows, estimates and cooldown reasons', async () => {
    const wrapper = render()
    await flushPromises()
    await wrapper.get('[data-testid="image-quota-tab-accounts"]').trigger('click')
    const row = wrapper.get('[data-testid="image-quota-account-7"]')
    expect(row.text()).toContain('admin.accounts.mainPoolRateLimited')
    expect(row.text()).toContain('"images":15,"percent":"50"')
    expect(row.text()).toContain('30')
    expect(row.text()).toContain('admin.accounts.imageCooldownReasons.openai_image_plan_limit')
    expect(row.text()).toContain('"images":32')
  })

  it('shows the API message when loading fails', async () => {
    vi.mocked(accountsAPI.getOpenAIImageQuotaStats).mockRejectedValue({ status: 500, message: 'stats unavailable' })
    const wrapper = render()
    await flushPromises()
    expect(wrapper.get('[data-testid="image-quota-error"]').text()).toBe('stats unavailable')
    expect(wrapper.get('[data-testid="image-quota-save"]').attributes('disabled')).toBeDefined()
  })
})
