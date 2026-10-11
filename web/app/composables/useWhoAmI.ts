import type { MaybeRefOrGetter } from 'vue'
import type { Capabilities, CapabilityName } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'

/**
 * Identity and capabilities of the current credentials on a connection.
 * Generic S3 servers (kind 's3') never have admin capabilities.
 *
 *   const { can, isAdmin } = useWhoAmI()
 *   if (can('manageUsers')) ...
 *
 * The status is fetched (and cached ~30 s in the connections store) on first use.
 */
export function useWhoAmI(id?: MaybeRefOrGetter<string | null | undefined>) {
  const store = useConnectionsStore()
  const { id: connId, connection, isGos3 } = useConnection(id)

  const status = computed(() => (connId.value ? store.statusOf(connId.value) : undefined))
  const whoami = computed(() => status.value?.whoami ?? null)
  const capabilities = computed<Capabilities>(() => (connId.value ? store.capabilitiesOf(connId.value) : NO_CAPABILITIES))
  const loading = computed(() => status.value?.state === 'checking')
  const isAdmin = computed(() => capabilities.value.admin)
  const isRoot = computed(() => whoami.value?.isRoot ?? false)

  function can(cap: CapabilityName): boolean {
    return !!capabilities.value[cap]
  }

  async function refresh(force = true) {
    if (!connId.value) return
    if (force) await store.refreshStatus(connId.value)
    else await store.ensureStatus(connId.value)
  }

  watch(connId, (v) => {
    if (v && connection.value) void store.ensureStatus(v)
  }, { immediate: true })

  return { whoami, capabilities, isAdmin, isRoot, isGos3, can, loading, status, refresh }
}
