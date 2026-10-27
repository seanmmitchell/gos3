<script setup lang="ts">
import { canNotify, noticeKey, readChoices, saveChoice, type NoticeChoices } from '~/utils/update-notices'

const { connection, admin, status } = useConnection()
const { updateStatus, refreshUpdates } = useUpdates()
const serverId = computed(() => status.value?.info?.id ?? connection.value?.serverId)
const choices = ref<NoticeChoices>({})
const ownsNotice = ref(false)
const mounted = ref(false)
let releaseLock: (() => void) | undefined
let generation = 0
let poll: ReturnType<typeof setInterval> | undefined
let claimTimer: ReturnType<typeof setTimeout> | undefined
const eligible = computed(() => mounted.value && !!serverId.value && !!admin.value && connection.value?.kind === 'gos3' && canNotify(updateStatus.value, choices.value))
const visible = computed(() => eligible.value && ownsNotice.value)

function browserStorage(): Pick<Storage, 'getItem' | 'setItem'> {
  try { return window.localStorage } catch { return { getItem: () => null, setItem: () => {} } }
}
function loadChoices() {
  if (serverId.value) choices.value = readChoices(browserStorage(), serverId.value)
}
function onStorage(e: StorageEvent) {
  if (e.key === null || e.key === noticeKey(serverId.value ?? '')) loadChoices()
}
function choose(skip: boolean) {
  if (serverId.value && updateStatus.value) choices.value = saveChoice(browserStorage(), serverId.value, updateStatus.value, skip)
}

watch([eligible, serverId, () => updateStatus.value?.available?.version], () => {
  const current = ++generation
  clearTimeout(claimTimer)
  releaseLock?.()
  releaseLock = undefined
  ownsNotice.value = false
  if (!eligible.value || !serverId.value) return
  // A browser-wide lock keeps simultaneous tabs from displaying duplicates.
  // It is released automatically if a tab closes or navigates away.
  if (navigator.locks) {
    void navigator.locks.request(noticeKey(serverId.value), { ifAvailable: true }, async (lock) => {
      if (!lock || current !== generation) return
      loadChoices()
      if (!eligible.value) return
      ownsNotice.value = true
      await new Promise<void>((resolve) => { releaseLock = resolve })
    })
  } else {
    // Web Locks require a secure context. Claim this check once in other
    // contexts so extra tabs remain quiet; Dismiss/Skip still use storage.
    const key = `${noticeKey(serverId.value)}.shown`
    const token = `${updateStatus.value?.lastCheck}:${updateStatus.value?.available?.version}`
    try {
      const storage = browserStorage()
      const previous = JSON.parse(storage.getItem(key) ?? 'null') as { token?: string } | null
      if (previous?.token === token) return
      const claim = JSON.stringify({ token, owner: Math.random().toString(36).slice(2) })
      storage.setItem(key, claim)
      // Let concurrently opened tabs settle on one owner before rendering.
      claimTimer = setTimeout(() => {
        if (current === generation && eligible.value && (storage.getItem(key) === claim || storage.getItem(key) === null)) ownsNotice.value = true
      }, 50)
    } catch { ownsNotice.value = true }
  }
}, { flush: 'post' })

watch(serverId, () => { if (mounted.value) loadChoices() })
watch([() => connection.value?.id, admin], () => { if (mounted.value) void refreshUpdates() })
onMounted(() => {
  loadChoices()
  mounted.value = true
  window.addEventListener('storage', onStorage)
  void refreshUpdates()
  poll = setInterval(() => void refreshUpdates(), 60_000)
})
onBeforeUnmount(() => {
  generation++
  releaseLock?.()
  clearInterval(poll)
  clearTimeout(claimTimer)
  window.removeEventListener('storage', onStorage)
})
</script>

<template>
  <aside v-if="visible && updateStatus?.available" aria-label="gos3 update available" aria-live="polite" role="status" class="fixed bottom-4 right-4 z-40 w-[calc(100%-2rem)] max-w-sm rounded-lg border border-accented bg-default p-4 shadow-lg">
    <div class="flex items-center gap-2 font-semibold text-highlighted">
      <UIcon name="i-lucide-bell" class="size-5 text-primary" />
      gos3 {{ updateStatus.available.version }} is available
    </div>
    <p class="mt-2 text-sm text-muted">
      {{ connection?.name }} is running {{ updateStatus.installedVersion }}.
      A {{ updateStatus.available.type }} release is ready to review.
    </p>
    <UButton :to="updateStatus.available.url" target="_blank" rel="noopener noreferrer" variant="link" class="mt-1 px-0" trailing-icon="i-lucide-external-link">Release notes</UButton>
    <div class="mt-3 flex gap-2">
      <UButton color="neutral" variant="soft" size="sm" @click="choose(false)">Dismiss</UButton>
      <UButton color="neutral" variant="ghost" size="sm" @click="choose(true)">Skip this version</UButton>
    </div>
  </aside>
</template>
