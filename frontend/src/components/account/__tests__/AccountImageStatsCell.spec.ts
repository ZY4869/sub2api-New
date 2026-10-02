import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountImageStatsCell from '../AccountImageStatsCell.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    locale: { value: 'zh' },
    t: (key: string, args?: { count: string }) => ({
      'admin.accounts.imageStats.today': `今日 ${args?.count} 张`,
      'admin.accounts.imageStats.total': `累计 ${args?.count} 张`,
      'admin.accounts.imageStats.hint': '本站保留的用量记录',
      'admin.accounts.imageStats.loadFailed': '统计加载失败',
      'common.loading': '加载中'
    } as Record<string, string>)[key] ?? key
  })
}))

describe('AccountImageStatsCell', () => {
  it('shows exact image counts including zero without abbreviating totals', () => {
    const wrapper = mount(AccountImageStatsCell, { props: { stats: { today_count: 0, total_count: 12345 } } })
    expect(wrapper.text()).toContain('今日 0 张')
    expect(wrapper.text()).toContain('累计 12,345 张')
    expect(wrapper.attributes('title')).toContain('本站')
  })
  it('does not misrepresent missing or failed data as zero', async () => {
    const wrapper = mount(AccountImageStatsCell)
    expect(wrapper.text()).toBe('—')
    await wrapper.setProps({ loading: true })
    expect(wrapper.find('[aria-label="加载中"]').exists()).toBe(true)
    await wrapper.setProps({ loading: false, stats: { today_count: 3, total_count: 9 }, error: 'failed' })
    expect(wrapper.text()).toBe('—')
    expect(wrapper.get('span').attributes('title')).toBe('统计加载失败')
  })
})
