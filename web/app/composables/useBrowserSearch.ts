// Object browser: the search box. In 'filter' mode the text filters the
// loaded rows client-side; in 'prefix' mode it is appended to the folder's
// prefix and the server lists the matching keys (debounced while typing).
//
//   const { search, mode, serverQuery, listPrefix } = useBrowserSearch(prefix)
//   watch(listPrefix, p => list(p))
//
// The text belongs to the folder it was typed in: opening another folder
// clears it, so the new folder is not listed with the old query appended.
import { computed, onScopeDispose, ref, toValue, watch } from 'vue'
import type { MaybeRefOrGetter } from 'vue'

/** Delay between the last keystroke and a prefix search on the server. */
export const SEARCH_DEBOUNCE_MS = 350

export type BrowserSearchMode = 'filter' | 'prefix'

export function useBrowserSearch(prefix: MaybeRefOrGetter<string>, debounceMs = SEARCH_DEBOUNCE_MS) {
  const search = ref('')
  const mode = ref<BrowserSearchMode>('filter')
  /** The prefix-search text the server listing uses ('' in filter mode). */
  const serverQuery = ref('')
  let timer: ReturnType<typeof setTimeout> | undefined

  watch([search, mode], () => {
    clearTimeout(timer)
    if (mode.value !== 'prefix' || search.value === '') {
      serverQuery.value = ''
      return
    }
    timer = setTimeout(() => (serverQuery.value = search.value), debounceMs)
  })

  // Synchronous, so the query is gone before any (pre-flush) watcher of
  // listPrefix sees the new folder: one listing, never "folder + old query".
  watch(() => toValue(prefix), () => {
    clearTimeout(timer)
    search.value = ''
    serverQuery.value = ''
  }, { flush: 'sync' })

  /** Prefix to list: the folder plus the (debounced) prefix-search text. */
  const listPrefix = computed(() => toValue(prefix) + serverQuery.value)

  onScopeDispose(() => clearTimeout(timer))

  return { search, mode, serverQuery, listPrefix }
}
