import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import ScheduleCronField from '../ScheduleCronField.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, locale: 'zh' }) }))

describe('ScheduleCronField', () => {
  it('preserves an unsupported historical cron until the user changes it', async () => {
    const wrapper = mount(ScheduleCronField, { props: { modelValue: '*/30 * * * *' } })
    expect((wrapper.get('select').element as HTMLSelectElement).value).toBe('custom')
    expect((wrapper.get('input').element as HTMLInputElement).value).toBe('*/30 * * * *')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    await wrapper.setProps({ modelValue: '0 9 * * 1,5' })
    expect((wrapper.get('select').element as HTMLSelectElement).value).toBe('weekly')
    expect(wrapper.findAll('input:checked')).toHaveLength(2)
    wrapper.unmount()
  })

  it('emits a valid daily schedule when switching modes', async () => {
    const wrapper = mount(ScheduleCronField, { props: { modelValue: '*/30 * * * *' } })
    await wrapper.get('select').setValue('daily')
    expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual(['0 7 * * *'])
    wrapper.unmount()
  })
})
