// "Default server" preference: the connection opened first (dashboard and
// server list order, sidebar fallback when no server is active). It lives in
// the connections store (`defaultId`, persisted with the definitions); this
// module keeps the small composable the pages use plus a sort helper.
import { computed, type ComputedRef } from 'vue'
import { useConnectionsStore } from '~/stores/connections'

/** Reactive default-server ID backed by the connections store. */
export function useDefaultServer(): { defaultId: ComputedRef<string | null>, setDefault: (id: string | null) => void } {
  const store = useConnectionsStore()
  const defaultId = computed(() => store.defaultConnection?.id ?? null)
  return { defaultId, setDefault: (id: string | null) => store.setDefault(id) }
}

/** Sort helper: default server first, then the given order. */
export function defaultFirst<T extends { id: string }>(list: readonly T[], defaultId: string | null | undefined): T[] {
  if (!defaultId) return [...list]
  const d = list.find(c => c.id === defaultId)
  return d ? [d, ...list.filter(c => c !== d)] : [...list]
}
