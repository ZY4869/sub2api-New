<template>
  <BaseDialog :show="show" :title="t('admin.accounts.imageQuota.title')" width="extra-wide" @close="close">
    <div class="space-y-4">
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.accounts.imageQuota.description') }}</p>
      <div v-if="loading" class="py-8 text-center text-sm text-gray-500">{{ t('common.loading') }}</div>
      <p v-else-if="loadError" class="text-sm text-red-600 dark:text-red-400" data-testid="image-quota-error">{{ loadError }}</p>
      <template v-else>
        <label class="flex flex-wrap items-center gap-2 text-sm">
          {{ t('admin.accounts.imageQuota.pauseThreshold') }}
          <input
            v-model.number="form.pause_threshold_percent"
            type="number"
            min="0"
            max="100"
            class="input w-24"
            data-testid="image-quota-threshold"
          />
          <span class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.imageQuota.pauseThresholdHint') }}</span>
        </label>

        <div class="flex gap-4 border-b border-gray-200 dark:border-dark-700" role="tablist">
          <button
            v-for="tab in tabs"
            :key="tab"
            type="button"
            role="tab"
            :aria-selected="activeTab === tab"
            :class="[
              '-mb-px border-b-2 px-1 pb-2 text-sm font-medium',
              activeTab === tab
                ? 'border-primary-500 text-primary-600 dark:text-primary-400'
                : 'border-transparent text-gray-500 hover:text-gray-700 dark:text-gray-400'
            ]"
            :data-testid="`image-quota-tab-${tab}`"
            @click="activeTab = tab"
          >{{ t(`admin.accounts.imageQuota.tabs.${tab}`) }}</button>
        </div>

        <div v-if="activeTab === 'plans'" class="overflow-x-auto">
          <table class="w-full text-left text-sm">
            <thead class="text-xs text-gray-500 dark:text-gray-400">
              <tr>
                <th class="py-2 pr-3">{{ t('admin.accounts.imageQuota.columns.plan') }}</th>
                <th class="py-2 pr-3">{{ t('admin.accounts.imageQuota.columns.accounts') }}</th>
                <th class="py-2 pr-3">{{ t('admin.accounts.imageQuota.columns.samples') }}</th>
                <th class="py-2 pr-3">{{ t('admin.accounts.imageQuota.columns.exhaustedImages') }}</th>
                <th class="py-2">{{ t('admin.accounts.imageQuota.columns.rules') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="plan in planRows"
                :key="plan.key"
                class="border-t border-gray-100 align-top dark:border-dark-700"
                :data-testid="`image-quota-plan-${plan.key}`"
              >
                <td class="py-2 pr-3 font-medium">{{ plan.label }}</td>
                <td class="py-2 pr-3 tabular-nums">{{ plan.stats?.account_count ?? 0 }}</td>
                <td class="py-2 pr-3 tabular-nums">{{ plan.stats?.observation_count ?? 0 }}</td>
                <td class="py-2 pr-3">
                  <div v-for="window in plan.stats?.windows ?? []" :key="window.window_minutes" class="whitespace-nowrap">
                    {{ t('admin.accounts.imageQuota.sampleRange', {
                      window: formatWindow(window.window_minutes),
                      min: window.min_images,
                      median: window.median_images,
                      max: window.max_images,
                      samples: window.samples
                    }) }}
                  </div>
                  <span v-if="!plan.stats?.windows?.length" class="text-gray-400">—</span>
                </td>
                <td class="space-y-1 py-2">
                  <div v-for="(rule, index) in form.plan_limits[plan.key] ?? []" :key="index" class="flex flex-wrap items-center gap-1">
                    <input
                      v-model.number="rule.window_minutes"
                      type="number"
                      min="1"
                      :max="maxWindowMinutes"
                      class="input w-24"
                      :aria-label="t('admin.accounts.imageQuota.ruleWindow')"
                    />
                    <span class="text-xs text-gray-500">{{ t('admin.accounts.imageQuota.ruleWindowUnit') }}</span>
                    <input
                      v-model.number="rule.max_images"
                      type="number"
                      min="1"
                      class="input w-20"
                      :aria-label="t('admin.accounts.imageQuota.ruleImages')"
                    />
                    <span class="text-xs text-gray-500">{{ t('admin.accounts.imageQuota.ruleImagesUnit') }}</span>
                    <button type="button" class="text-xs text-red-600 hover:underline dark:text-red-400" @click="removeRule(plan.key, index)">
                      {{ t('admin.accounts.imageQuota.removeRule') }}
                    </button>
                  </div>
                  <button
                    v-if="(form.plan_limits[plan.key]?.length ?? 0) < maxRulesPerPlan"
                    type="button"
                    class="text-xs text-primary-600 hover:underline dark:text-primary-400"
                    :data-testid="`image-quota-add-${plan.key}`"
                    @click="addRule(plan.key)"
                  >+ {{ t('admin.accounts.imageQuota.addRule') }}</button>
                </td>
              </tr>
            </tbody>
          </table>
          <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.imageQuota.planLimitHint') }}</p>
        </div>

        <div v-else class="max-h-[50vh] overflow-auto">
          <p v-if="!stats?.accounts.length" class="py-6 text-center text-sm text-gray-500">{{ t('admin.accounts.imageQuota.empty') }}</p>
          <table v-else class="w-full text-left text-sm">
            <thead class="text-xs text-gray-500 dark:text-gray-400">
              <tr>
                <th class="py-2 pr-3">{{ t('admin.accounts.imageQuota.columns.account') }}</th>
                <th class="py-2 pr-3">{{ t('admin.accounts.imageQuota.columns.plan') }}</th>
                <th class="py-2 pr-3">{{ t('admin.accounts.imageQuota.columns.currentWindow') }}</th>
                <th class="py-2 pr-3">{{ t('admin.accounts.imageQuota.columns.estimatedLimit') }}</th>
                <th class="py-2 pr-3">{{ t('admin.accounts.imageQuota.columns.cooldown') }}</th>
                <th class="py-2">{{ t('admin.accounts.imageQuota.columns.latestObservation') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="account in stats.accounts"
                :key="account.account_id"
                class="border-t border-gray-100 align-top dark:border-dark-700"
                :data-testid="`image-quota-account-${account.account_id}`"
              >
                <td class="py-2 pr-3">
                  <div class="font-medium">{{ account.name }}</div>
                  <div v-if="account.main_pool_only_blocked" class="text-xs text-emerald-600 dark:text-emerald-400">
                    {{ t('admin.accounts.mainPoolRateLimited') }}
                  </div>
                </td>
                <td class="py-2 pr-3">{{ planLabel(account.plan_type) }}</td>
                <td class="py-2 pr-3">
                  <template v-if="account.current_window">
                    <div>{{ formatWindow(account.current_window.window_minutes) }}</div>
                    <div class="text-xs text-gray-500 dark:text-gray-400">
                      {{ t('admin.accounts.imageQuota.windowUsage', {
                        images: account.current_window.images,
                        percent: formatPercent(account.current_window.used_percent)
                      }) }}
                    </div>
                    <div v-if="account.current_window.mirror_suspected" class="text-xs text-amber-600 dark:text-amber-400">
                      {{ t('admin.accounts.imageQuota.mirrorSuspected') }}
                    </div>
                  </template>
                  <span v-else class="text-gray-400">—</span>
                </td>
                <td class="py-2 pr-3 tabular-nums">{{ account.current_window?.estimated_limit ?? '—' }}</td>
                <td class="py-2 pr-3">
                  <template v-if="account.image_cooldown_until">
                    <div v-if="isKnownImageCooldownReason(account.image_cooldown_reason)">
                      {{ t(`admin.accounts.imageCooldownReasons.${account.image_cooldown_reason}`) }}
                    </div>
                    <div class="text-xs text-gray-500 dark:text-gray-400">{{ formatDateTime(account.image_cooldown_until) }}</div>
                  </template>
                  <span v-else class="text-gray-400">—</span>
                </td>
                <td class="py-2">{{ latestObservationText(account) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
      <div class="flex justify-end gap-2">
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="close">{{ t('common.close') }}</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="saving || loading || !!loadError || !formValid"
          data-testid="image-quota-save"
          @click="save"
        >{{ saving ? t('common.loading') : t('common.save') }}</button>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { accountsAPI, type OpenAIImageQuotaAccountStats, type OpenAIImageQuotaStats } from '@/api/admin/accounts'
import { getOpenAIImageQuotaSettings, updateOpenAIImageQuotaSettings, type OpenAIImagePlanLimitRule } from '@/api/admin/settings'
import { isKnownImageCooldownReason } from '@/components/account/imageCooldownReasons'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime } from '@/utils/format'
import { openAIPlanTypeKey, openAIPlanTypeLabel } from '@/utils/planType'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ close: [] }>()
const { t } = useI18n()
const appStore = useAppStore()

// [local] Go / Plus / Pro 100 / Pro 200 / Pro 500 固定显示；其余套餐有数据或已配置时追加。
const preferredPlans = ['go', 'plus', 'prolite', 'pro', 'promax']
const maxWindowMinutes = 43200
const maxRulesPerPlan = 4
const tabs = ['plans', 'accounts'] as const
const activeTab = ref<(typeof tabs)[number]>('plans')
const loading = ref(false)
const saving = ref(false)
const loadError = ref('')
const stats = ref<OpenAIImageQuotaStats | null>(null)
const form = reactive<{ pause_threshold_percent: number; plan_limits: Record<string, OpenAIImagePlanLimitRule[]> }>({
  pause_threshold_percent: 100,
  plan_limits: {}
})

const planRows = computed(() => {
  const keys = new Set(preferredPlans)
  for (const plan of stats.value?.plans ?? []) keys.add(plan.plan_type)
  for (const key of Object.keys(form.plan_limits)) keys.add(key)
  return [...keys].map(key => ({
    key,
    label: planLabel(key),
    stats: stats.value?.plans.find(plan => plan.plan_type === key)
  }))
})

const formValid = computed(() => {
  const threshold = form.pause_threshold_percent
  return Number.isInteger(threshold) && threshold >= 0 && threshold <= 100 &&
    Object.values(form.plan_limits).every(rules => rules.every(rule =>
      Number.isInteger(rule.window_minutes) && rule.window_minutes >= 1 && rule.window_minutes <= maxWindowMinutes &&
      Number.isInteger(rule.max_images) && rule.max_images >= 1))
})

function planLabel(key: string): string {
  if (key === 'unknown') return t('admin.accounts.imageQuota.unknownPlan')
  return openAIPlanTypeLabel(key) || key
}

function formatWindow(minutes: number): string {
  if (!minutes) return t('admin.accounts.imageQuota.unknownWindow')
  if (minutes % 1440 === 0) return t('admin.accounts.imageQuota.windowDays', { n: minutes / 1440 })
  if (minutes % 60 === 0) return t('admin.accounts.imageQuota.windowHours', { n: minutes / 60 })
  return t('admin.accounts.imageQuota.windowMinutes', { n: minutes })
}

function formatPercent(value: number): string {
  return Number.isInteger(value) ? String(value) : value.toFixed(1)
}

function latestObservationText(account: OpenAIImageQuotaAccountStats): string {
  const latest = account.observations?.[account.observations.length - 1]
  if (!latest) return '—'
  const time = formatDateTime(latest.observed_at)
  return typeof latest.images_in_window === 'number'
    ? t('admin.accounts.imageQuota.observationText', { time, images: latest.images_in_window })
    : t('admin.accounts.imageQuota.observationNoCount', { time })
}

function addRule(plan: string) {
  const rules = form.plan_limits[plan] ?? []
  rules.push({ window_minutes: 60, max_images: 1 })
  form.plan_limits[plan] = rules
}

function removeRule(plan: string, index: number) {
  const rules = form.plan_limits[plan]
  if (!rules) return
  rules.splice(index, 1)
  if (!rules.length) delete form.plan_limits[plan]
}

function applySettings(settings: { pause_threshold_percent: number; plan_limits?: Record<string, OpenAIImagePlanLimitRule[]> }) {
  form.pause_threshold_percent = settings.pause_threshold_percent
  form.plan_limits = Object.fromEntries(Object.entries(settings.plan_limits ?? {})
    .map(([plan, rules]) => [openAIPlanTypeKey(plan), rules.map(rule => ({ ...rule }))]))
}

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const [settings, quotaStats] = await Promise.all([getOpenAIImageQuotaSettings(), accountsAPI.getOpenAIImageQuotaStats()])
    applySettings(settings)
    stats.value = quotaStats
  } catch (error) {
    loadError.value = extractApiErrorMessage(error, t('admin.accounts.imageQuota.loadFailed'))
  } finally {
    loading.value = false
  }
}

watch(() => props.show, show => {
  if (!show) return
  activeTab.value = 'plans'
  void load()
}, { immediate: true })

function close() {
  if (!saving.value) emit('close')
}

async function save() {
  if (saving.value || !formValid.value) return
  saving.value = true
  try {
    const planLimits = Object.fromEntries(Object.entries(form.plan_limits)
      .filter(([, rules]) => rules.length > 0)
      .map(([plan, rules]) => [plan, rules.map(rule => ({ window_minutes: rule.window_minutes, max_images: rule.max_images }))]))
    applySettings(await updateOpenAIImageQuotaSettings({ pause_threshold_percent: form.pause_threshold_percent, plan_limits: planLimits }))
    appStore.showSuccess(t('admin.accounts.imageQuota.saved'))
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('admin.accounts.imageQuota.saveFailed')))
  } finally {
    saving.value = false
  }
}
</script>
