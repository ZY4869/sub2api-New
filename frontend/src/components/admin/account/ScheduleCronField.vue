<template>
  <div class="space-y-2">
    <select v-model="schedule.mode" class="input" :aria-label="t('admin.scheduledTests.scheduleMode')" @change="publish">
      <option v-for="mode in modes" :key="mode" :value="mode">{{ t(`admin.scheduledTests.${mode}`) }}</option>
    </select>
    <input v-if="schedule.mode === 'custom'" v-model="schedule.expression" class="input font-mono" :aria-label="t('admin.scheduledTests.cronExpression')" placeholder="*/30 * * * *" @input="publish" />
    <div v-else class="flex flex-wrap items-center gap-2">
      <label v-if="schedule.mode === 'hourly'" class="flex items-center gap-1 text-xs">
        {{ t('admin.scheduledTests.everyHours') }}
        <select v-model.number="schedule.everyHours" class="input w-20" @change="publish">
          <option v-for="hours in cronHourIntervals" :key="hours" :value="hours">{{ hours }}</option>
        </select>
      </label>
      <label v-else class="flex items-center gap-1 text-xs">
        {{ t('admin.scheduledTests.hour') }}
        <select v-model.number="schedule.hour" class="input w-20" @change="publish">
          <option v-for="hour in 24" :key="hour" :value="hour - 1">{{ String(hour - 1).padStart(2, '0') }}</option>
        </select>
      </label>
      <label class="flex items-center gap-1 text-xs">
        {{ t('admin.scheduledTests.minute') }}
        <select v-model.number="schedule.minute" class="input w-20" @change="publish">
          <option v-for="minute in 60" :key="minute" :value="minute - 1">{{ String(minute - 1).padStart(2, '0') }}</option>
        </select>
      </label>
      <div v-if="schedule.mode === 'weekly'" class="flex w-full flex-wrap gap-2">
        <label v-for="day in [1, 2, 3, 4, 5, 6, 0]" :key="day" class="flex items-center gap-1 text-xs">
          <input v-model="schedule.weekdays" type="checkbox" :value="day" :disabled="schedule.weekdays.length === 1 && schedule.weekdays.includes(day)" @change="publish" />
          {{ t(`admin.scheduledTests.weekdays.${day}`) }}
        </label>
      </div>
    </div>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ describeCron(modelValue, locale) }}</p>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.scheduledTests.serverTimezone') }}</p>
  </div>
</template>

<script setup lang="ts">
import { reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { buildCron, cronHourIntervals, describeCron, parseCron, type CronMode } from '@/utils/cronSchedule'

const props = defineProps<{ modelValue: string }>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()
const { t, locale } = useI18n()
const modes: CronMode[] = ['daily', 'weekly', 'hourly', 'custom']
const schedule = reactive(parseCron(props.modelValue))
let lastEmitted: string | undefined

watch(() => props.modelValue, value => {
  if (value !== lastEmitted) Object.assign(schedule, parseCron(value))
})

function publish() {
  const value = buildCron(schedule.mode, schedule)
  schedule.expression = value
  lastEmitted = value
  emit('update:modelValue', value)
}
</script>
