import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { effectScope, nextTick, ref, watch } from 'vue'
import { SEARCH_DEBOUNCE_MS, useBrowserSearch } from '~/composables/useBrowserSearch'

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

/** Runs useBrowserSearch in a scope and records every listPrefix the page would list. */
function setup(initial = '') {
  const prefix = ref(initial)
  const scope = effectScope()
  const s = scope.run(() => useBrowserSearch(prefix))!
  const listed: string[] = []
  // Like the page's listing watcher (default 'pre' flush).
  scope.run(() => watch(s.listPrefix, p => listed.push(p)))
  return { prefix, scope, listed, ...s }
}

describe('useBrowserSearch', () => {
  it('appends the debounced prefix-search text to the folder', async () => {
    const s = setup('docs/')
    s.mode.value = 'prefix'
    s.search.value = 'rep'
    await nextTick()
    expect(s.listPrefix.value).toBe('docs/')
    vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS)
    await nextTick()
    expect(s.listPrefix.value).toBe('docs/rep')
    expect(s.listed).toEqual(['docs/rep'])

    // Clearing the text lists the folder again right away.
    s.search.value = ''
    await nextTick()
    expect(s.listed).toEqual(['docs/rep', 'docs/'])
    s.scope.stop()
  })

  it('ignores the text for the listing in filter mode', async () => {
    const s = setup()
    s.search.value = 'rep'
    await nextTick()
    vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS)
    await nextTick()
    expect(s.serverQuery.value).toBe('')
    expect(s.listed).toEqual([])
    s.scope.stop()
  })

  it('clears the search when another folder is opened (one listing, without the old query)', async () => {
    const s = setup()
    s.mode.value = 'prefix'
    s.search.value = 'rep'
    await nextTick()
    vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS)
    await nextTick()
    expect(s.listed).toEqual(['rep'])

    s.prefix.value = 'reports/'
    await nextTick()
    expect(s.search.value).toBe('')
    expect(s.serverQuery.value).toBe('')
    expect(s.mode.value).toBe('prefix')
    vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS)
    await nextTick()
    // Never "reports/rep", and the folder is listed once.
    expect(s.listed).toEqual(['rep', 'reports/'])
    s.scope.stop()
  })

  it('drops a pending (debounced) query when the folder changes', async () => {
    const s = setup()
    s.mode.value = 'prefix'
    s.search.value = 'rep'
    await nextTick()
    s.prefix.value = 'reports/'
    vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS)
    await nextTick()
    expect(s.listPrefix.value).toBe('reports/')
    expect(s.listed).toEqual(['reports/'])
    s.scope.stop()
  })

  it('also clears filter text when the folder changes', async () => {
    const s = setup()
    s.search.value = 'x'
    s.prefix.value = 'a/'
    await nextTick()
    expect(s.search.value).toBe('')
    s.scope.stop()
  })
})
