import { describe, expect, it } from 'vitest'
import { ref } from 'vue'
import {
  EMPTY_SELECTION,
  selectionCoverage,
  selectionPrune,
  selectionSetAll,
  selectionToggle,
  useBrowserSelection
} from '~/composables/useBrowserSelection'

const ids = ['a', 'b', 'c', 'd', 'e']
const sel = (s: { selected: ReadonlySet<string> }) => [...s.selected].sort()

describe('selection logic', () => {
  it('toggles single ids and sets the anchor', () => {
    let s = selectionToggle(EMPTY_SELECTION, 'b', ids)
    expect(sel(s)).toEqual(['b'])
    expect(s.anchor).toBe('b')
    s = selectionToggle(s, 'b', ids)
    expect(sel(s)).toEqual([])
    // The frozen empty state is never mutated.
    expect(EMPTY_SELECTION.selected.size).toBe(0)
  })

  it('selects ranges with shift (both directions) and keeps the anchor', () => {
    let s = selectionToggle(EMPTY_SELECTION, 'b', ids)
    s = selectionToggle(s, 'd', ids, true)
    expect(sel(s)).toEqual(['b', 'c', 'd'])
    expect(s.anchor).toBe('b')
    // Extending upwards from the same anchor.
    s = selectionToggle(s, 'a', ids, true)
    expect(sel(s)).toEqual(['a', 'b', 'c', 'd'])
  })

  it('deselects a range when the shift-clicked row was selected', () => {
    let s = selectionSetAll(EMPTY_SELECTION, ids, true)
    s = selectionToggle(s, 'b', ids) // deselect b, anchor b
    s = selectionToggle(s, 'd', ids, true) // d was selected → range off
    expect(sel(s)).toEqual(['a', 'e'])
  })

  it('falls back to a plain toggle when the anchor is not listed', () => {
    let s = selectionToggle(EMPTY_SELECTION, 'x', ['x', ...ids])
    s = selectionToggle(s, 'c', ids, true)
    expect(sel(s)).toEqual(['c', 'x'])
    expect(s.anchor).toBe('c')
  })

  it('select all / none only touches the given ids', () => {
    let s = selectionToggle(EMPTY_SELECTION, 'hidden', ['hidden'])
    s = selectionSetAll(s, ['a', 'b'], true)
    expect(sel(s)).toEqual(['a', 'b', 'hidden'])
    s = selectionSetAll(s, ['a', 'b'], false)
    expect(sel(s)).toEqual(['hidden'])
  })

  it('prunes ids that disappeared and keeps identity when nothing changed', () => {
    const s = selectionSetAll(EMPTY_SELECTION, ['a', 'b', 'c'], true)
    const same = selectionPrune(s, ['a', 'b', 'c', 'z'])
    expect(same).toBe(s)
    const pruned = selectionPrune({ ...s, anchor: 'c' }, new Set(['a']))
    expect(sel(pruned)).toEqual(['a'])
    expect(pruned.anchor).toBeNull()
  })

  it('computes coverage', () => {
    expect(selectionCoverage(EMPTY_SELECTION, [])).toBe('none')
    expect(selectionCoverage(EMPTY_SELECTION, ids)).toBe('none')
    expect(selectionCoverage(selectionSetAll(EMPTY_SELECTION, ['a'], true), ids)).toBe('some')
    expect(selectionCoverage(selectionSetAll(EMPTY_SELECTION, ids, true), ids)).toBe('all')
  })
})

describe('useBrowserSelection', () => {
  it('wraps the logic reactively over the current display order', () => {
    const order = ref(['a', 'b', 'c'])
    const s = useBrowserSelection(() => order.value)
    s.toggle('a')
    s.toggle('c', true)
    expect(s.ids.value.sort()).toEqual(['a', 'b', 'c'])
    expect(s.coverage.value).toBe('all')
    expect(s.isSelected('b')).toBe(true)

    order.value = ['a', 'b', 'c', 'd']
    expect(s.coverage.value).toBe('some')
    s.toggleAll() // not all → select all visible
    expect(s.count.value).toBe(4)
    s.toggleAll() // all → clear visible
    expect(s.count.value).toBe(0)

    s.setAll(true)
    s.prune(['a', 'd'])
    expect(s.ids.value.sort()).toEqual(['a', 'd'])
    const v = s.version.value
    s.clear()
    expect(s.count.value).toBe(0)
    expect(s.version.value).toBe(v + 1)
  })
})
