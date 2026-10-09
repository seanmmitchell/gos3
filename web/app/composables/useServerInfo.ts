import type { MaybeRefOrGetter } from 'vue'
import type { Info, ServerInfo } from '~/types/admin'

/**
 * Server information for a gos3 connection: the admin `ServerInfo` when the
 * identity may read it (admin:ServerInfo), otherwise the public `Info`.
 */
export function useServerInfo(id?: MaybeRefOrGetter<string | null | undefined>) {
  const { admin, connection, status, isGos3 } = useConnection(id)
  const { can } = useWhoAmI(id)

  const serverInfo = ref<ServerInfo | null>(null)
  const loading = ref(false)
  const error = ref<unknown>(null)

  /** Public identity document (always available for reachable gos3 servers). */
  const info = computed<Info | null>(() => serverInfo.value ?? status.value?.info ?? null)

  async function refresh() {
    error.value = null
    if (!isGos3.value || !connection.value) {
      serverInfo.value = null
      return
    }
    loading.value = true
    try {
      if (admin.value && can('viewInfo')) {
        serverInfo.value = await admin.value.serverInfo()
      } else {
        serverInfo.value = null
      }
    } catch (e) {
      error.value = e
    } finally {
      loading.value = false
    }
  }

  watch([() => connection.value?.id, () => can('viewInfo')], () => void refresh(), { immediate: true })

  return { info, serverInfo, loading, error, refresh }
}
