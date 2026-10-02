import { ref, watch } from 'vue'

const storageKey = 'account-compact-mode'

export function useAccountsDensity() {
  let initial = false
  try { initial = localStorage.getItem(storageKey) === 'true' } catch { /* Storage may be unavailable. */ }
  const compact = ref(initial)
  watch(compact, value => {
    try { localStorage.setItem(storageKey, String(value)) } catch { /* Keep the in-memory preference. */ }
  })
  return { compact }
}
