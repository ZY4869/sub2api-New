<template>
  <div class="text-xs" :title="t('admin.accounts.imageStats.hint')" data-testid="account-image-stats">
    <span v-if="error" class="text-red-500" :title="t('admin.accounts.imageStats.loadFailed')">—</span>
    <div v-else-if="loading && !stats" class="h-7 w-20 animate-pulse rounded bg-gray-200 dark:bg-dark-700" :aria-label="t('common.loading')"></div>
    <div v-else-if="stats" class="space-y-0.5 tabular-nums">
      <div class="whitespace-nowrap text-gray-600 dark:text-gray-300">{{ t('admin.accounts.imageStats.today', { count: formatCount(stats.today_count) }) }}</div>
      <div class="whitespace-nowrap font-medium text-primary-600 dark:text-primary-400">{{ t('admin.accounts.imageStats.total', { count: formatCount(stats.total_count) }) }}</div>
    </div>
    <span v-else class="text-gray-400">—</span>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { AccountImageStats } from '@/api/admin/accounts'

defineProps<{ stats?: AccountImageStats | null; loading?: boolean; error?: string | null }>()
const { t, locale } = useI18n()
const formatCount = (value: number) => new Intl.NumberFormat(locale.value).format(value)
</script>
