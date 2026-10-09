// "Default server" preference: the connection opened first (dashboard order,
// connect page fallback). Kept per browser in localStorage, separate from
// the connections store's `activeId` (which follows navigation).
import { ref, type Ref } from 'vue'

export const DEFAULT_SERVER_KEY = 'gos3.defaultServer.v1'

export function readDefaultServerId(): string | null {
  try {
    return globalThis.localStorage?.getItem(DEFAULT_SERVER_KEY) || null
  } catch {
    return null
  }
}

export function writeDefaultServerId(id: string | null): void {
  try {
    if (id) globalThis.localStorage?.setItem(DEFAULT_SERVER_KEY, id)
    else globalThis.localStorage?.removeItem(DEFAULT_SERVER_KEY)
  } catch {
    // storage unavailable: preference lasts for this page load only
  }
}

let state: Ref<string | null> | undefined

/** Shared reactive default-server ID. */
export function useDefaultServer() {
  state ??= ref(readDefaultServerId())
  const defaultId = state
  function setDefault(id: string | null) {
    defaultId.value = id
    writeDefaultServerId(id)
  }
  return { defaultId, setDefault }
}

/** Sort helper: default server first, then the given order. */
export function defaultFirst<T extends { id: string }>(list: readonly T[], defaultId: string | null | undefined): T[] {
  if (!defaultId) return [...list]
  const d = list.find(c => c.id === defaultId)
  return d ? [d, ...list.filter(c => c !== d)] : [...list]
}
