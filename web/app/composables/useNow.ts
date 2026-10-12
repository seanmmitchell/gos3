// A shared, coarse "current time" for relative timestamps. One interval
// serves every subscriber (a listing can show thousands of "3 minutes ago"
// cells); it runs only while at least one component uses it.
import { onBeforeUnmount, onMounted, readonly, ref, type Ref } from 'vue'

/** Refresh period of useNow(). */
export const NOW_INTERVAL_MS = 30000

const now = ref(Date.now())
let subscribers = 0
let timer: ReturnType<typeof setInterval> | undefined

/** Date.now(), updated every NOW_INTERVAL_MS while mounted components use it. */
export function useNow(): Readonly<Ref<number>> {
  onMounted(() => {
    if (subscribers++ === 0) {
      now.value = Date.now()
      timer = setInterval(() => (now.value = Date.now()), NOW_INTERVAL_MS)
    }
  })
  onBeforeUnmount(() => {
    if (--subscribers === 0) {
      clearInterval(timer)
      timer = undefined
    }
  })
  return readonly(now)
}
