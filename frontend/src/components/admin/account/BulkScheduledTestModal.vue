<template>
  <BaseDialog :show="show" :title="t('admin.scheduledTests.bulkTitle')" width="normal" @close="close">
    <form class="space-y-4" @submit.prevent="submit">
      <p class="text-sm text-amber-700 dark:text-amber-300">{{ t('admin.scheduledTests.quotaNotice') }}</p>
      <p class="text-sm text-gray-500">{{ t('admin.scheduledTests.selectedAccounts', { count: targetIds.length }) }}</p>
      <fieldset :disabled="saving" class="space-y-3">
        <ScheduleCronField v-model="form.cron_expression" />
        <label class="block text-sm">
          {{ t('admin.scheduledTests.model') }}
          <input v-model="form.model_id" class="input mt-1" :placeholder="t('admin.scheduledTests.defaultModel')" />
        </label>
        <label class="block text-sm">
          {{ t('admin.scheduledTests.conflictPolicy') }}
          <select v-model="conflictPolicy" class="input mt-1">
            <option value="skip">{{ t('admin.scheduledTests.conflictSkip') }}</option>
            <option value="overwrite">{{ t('admin.scheduledTests.conflictOverwrite') }}</option>
            <option value="append">{{ t('admin.scheduledTests.conflictAppend') }}</option>
          </select>
        </label>
        <label class="block text-sm">
          {{ t('admin.scheduledTests.maxResults') }}
          <input v-model.number="form.max_results" type="number" min="1" max="1000" required class="input mt-1" />
        </label>
        <label class="flex items-center gap-2 text-sm"><Toggle v-model="form.enabled" />{{ t('admin.scheduledTests.enabled') }}</label>
        <label class="flex items-center gap-2 text-sm"><Toggle v-model="form.auto_recover" />{{ t('admin.scheduledTests.autoRecover') }}</label>
        <p class="text-xs text-gray-500">{{ t('admin.scheduledTests.autoRecoverHelp') }}</p>
      </fieldset>
      <div v-if="results.length" aria-live="polite" class="space-y-2 text-sm">
        <p>{{ t('admin.scheduledTests.bulkSummary', summary) }}</p>
        <ul class="max-h-40 overflow-auto text-red-600 dark:text-red-400">
          <li v-for="result in failures" :key="result.id">#{{ result.id }}: {{ result.error }}</li>
        </ul>
      </div>
      <div class="flex justify-end gap-2">
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="close">{{ t('common.close') }}</button>
        <button type="submit" class="btn btn-primary" :disabled="saving || submitted || !targetIds.length || !form.cron_expression.trim()">
          {{ saving ? t('common.loading') : t('common.save') }}
        </button>
      </div>
    </form>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Toggle from '@/components/common/Toggle.vue'
import ScheduleCronField from './ScheduleCronField.vue'
import { scheduledTestsAPI } from '@/api/admin/scheduledTests'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ show: boolean; accountIds: number[] }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { t } = useI18n()
const targetIds = ref<number[]>([])
const saving = ref(false)
const submitted = ref(false)
const conflictPolicy = ref<'skip' | 'overwrite' | 'append'>('skip')
const form = reactive({ cron_expression: '0 7 * * *', model_id: '', enabled: true, auto_recover: false, max_results: 100 })
const results = ref<{ id: number; status: 'success' | 'skipped' | 'failed'; error?: string }[]>([])
const failures = computed(() => results.value.filter(result => result.status === 'failed'))
const summary = computed(() => ({
  success: results.value.filter(result => result.status === 'success').length,
  skipped: results.value.filter(result => result.status === 'skipped').length,
  failed: failures.value.length
}))

watch(() => props.show, show => {
  if (show && !saving.value) {
    targetIds.value = [...new Set(props.accountIds)]
    results.value = []
    submitted.value = false
  }
}, { immediate: true })

function close() { if (!saving.value) emit('close') }

async function submit() {
  if (saving.value || submitted.value || !targetIds.value.length || !form.cron_expression.trim()) return
  if (!Number.isInteger(form.max_results) || form.max_results < 1 || form.max_results > 1000) return
  saving.value = true
  const request = { ...form, model_id: form.model_id.trim(), cron_expression: form.cron_expression.trim() }
  const policy = conflictPolicy.value
  let cursor = 0
  async function worker() {
    while (cursor < targetIds.value.length) {
      const id = targetIds.value[cursor++]!
      try {
        const plans = await scheduledTestsAPI.listByAccount(id)
        const first = plans[0]
        if (first && policy === 'skip') {
          results.value.push({ id, status: 'skipped' })
          continue
        }
        if (first && policy === 'overwrite') await scheduledTestsAPI.update(first.id, request)
        else await scheduledTestsAPI.create({ ...request, account_id: id })
        results.value.push({ id, status: 'success' })
      } catch (error) {
        // The API client rejects with plain { status, message } objects, not Error instances.
        results.value.push({ id, status: 'failed', error: extractApiErrorMessage(error, t('admin.scheduledTests.failed')) })
      }
    }
  }
  try {
    await Promise.all(Array.from({ length: Math.min(5, targetIds.value.length) }, worker))
    submitted.value = true
    emit('saved')
  } finally { saving.value = false }
}
</script>
