// Object browser: multi-select logic (checkboxes with shift-click ranges,
// select all, pruning when the listing changes). The pure functions are
// unit-tested; useBrowserSelection() wraps them in Vue state.
import { computed, ref, shallowRef } from 'vue'

export interface SelectionState {
  selected: ReadonlySet<string>
  /** Last id clicked without shift (start of a shift-range). */
  anchor: string | null
}

export const EMPTY_SELECTION: SelectionState = Object.freeze({ selected: new Set<string>(), anchor: null })

/**
 * Click on a row's checkbox. Without shift the id is toggled and becomes the
 * anchor. With shift, every id between the anchor and `id` (in display order)
 * is set to the new state of `id`.
 */
export function selectionToggle(state: SelectionState, id: string, orderedIds: readonly string[], shift = false): SelectionState {
  const next = new Set(state.selected)
  const on = !next.has(id)
  if (shift && state.anchor !== null) {
    const a = orderedIds.indexOf(state.anchor)
    const b = orderedIds.indexOf(id)
    if (a >= 0 && b >= 0) {
      const [lo, hi] = a < b ? [a, b] : [b, a]
      for (let i = lo; i <= hi; i++) {
        const x = orderedIds[i]!
        if (on) next.add(x)
        else next.delete(x)
      }
      return { selected: next, anchor: state.anchor }
    }
  }
  if (on) next.add(id)
  else next.delete(id)
  return { selected: next, anchor: id }
}

/** Selects (on) or deselects (off) all `ids`, keeping selections outside them. */
export function selectionSetAll(state: SelectionState, ids: readonly string[], on: boolean): SelectionState {
  const next = new Set(state.selected)
  for (const id of ids) {
    if (on) next.add(id)
    else next.delete(id)
  }
  return { selected: next, anchor: state.anchor }
}

/** Drops selected ids that are no longer listed. */
export function selectionPrune(state: SelectionState, validIds: Iterable<string>): SelectionState {
  const valid = validIds instanceof Set ? validIds : new Set(validIds)
  const next = new Set<string>()
  for (const id of state.selected) if (valid.has(id)) next.add(id)
  const anchor = state.anchor !== null && valid.has(state.anchor) ? state.anchor : null
  if (next.size === state.selected.size && anchor === state.anchor) return state
  return { selected: next, anchor }
}

/** 'all' when every id is selected, 'some' when at least one is, else 'none'. */
export function selectionCoverage(state: SelectionState, ids: readonly string[]): 'all' | 'some' | 'none' {
  if (!ids.length) return 'none'
  let n = 0
  for (const id of ids) if (state.selected.has(id)) n++
  if (n === 0) return 'none'
  return n === ids.length ? 'all' : 'some'
}

/**
 * Reactive selection for a list whose display order is given by `orderedIds`.
 *
 *   const sel = useBrowserSelection(() => rows.value.map(r => r.id))
 *   sel.toggle(id, event.shiftKey); sel.isSelected(id); sel.ids.value
 */
export function useBrowserSelection(orderedIds: () => readonly string[]) {
  const state = shallowRef<SelectionState>(EMPTY_SELECTION)

  const ids = computed(() => [...state.value.selected])
  const count = computed(() => state.value.selected.size)
  const coverage = computed(() => selectionCoverage(state.value, orderedIds()))
  /** Bumped on every change (handy as a watch source). */
  const version = ref(0)

  function set(s: SelectionState) {
    if (s === state.value) return
    state.value = s
    version.value++
  }

  return {
    state,
    ids,
    count,
    coverage,
    version,
    isSelected: (id: string) => state.value.selected.has(id),
    toggle: (id: string, shift = false) => set(selectionToggle(state.value, id, orderedIds(), shift)),
    /** Header checkbox: select all visible rows, or clear them when all are selected. */
    toggleAll: () => {
      const visible = orderedIds()
      set(selectionSetAll(state.value, visible, selectionCoverage(state.value, visible) !== 'all'))
    },
    setAll: (on: boolean) => set(selectionSetAll(state.value, orderedIds(), on)),
    prune: (valid: Iterable<string>) => set(selectionPrune(state.value, valid)),
    clear: () => set(EMPTY_SELECTION)
  }
}

export type BrowserSelection = ReturnType<typeof useBrowserSelection>
