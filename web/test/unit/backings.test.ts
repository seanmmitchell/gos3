import { describe, expect, it } from 'vitest'
import type { BackingChoices } from '~/types/admin'
import {
  backingChoices,
  backingModeInfo,
  backingModeOf,
  backingSelectItems,
  backingSupportsVersioning,
  defaultBacking,
  LOCAL_BACKING
} from '~/utils/backings'

const CHOICES: BackingChoices = {
  default: 'archive',
  available: [
    { name: 'local', mode: 'local' },
    { name: 'aws', mode: 'gateway' },
    { name: 'archive', mode: 'blob' }
  ]
}

describe('storage backings', () => {
  it('offers a choice only when there is more than local storage', () => {
    expect(backingChoices(undefined)).toEqual([])
    expect(backingChoices({ default: 'local', available: [{ name: 'local', mode: 'local' }] })).toEqual([])
    expect(backingChoices(CHOICES).map(b => b.name)).toEqual(['local', 'aws', 'archive'])
  })

  it('resolves the default, falling back to local for unknown names', () => {
    expect(defaultBacking(CHOICES)).toBe('archive')
    expect(defaultBacking({ ...CHOICES, default: 'gone' })).toBe(LOCAL_BACKING)
    expect(defaultBacking(null)).toBe(LOCAL_BACKING)
  })

  it('maps backing names to modes', () => {
    expect(backingModeOf('aws', CHOICES)).toBe('gateway')
    expect(backingModeOf('local', null)).toBe('local')
    expect(backingModeOf('other', CHOICES)).toBeUndefined()
    expect(backingModeOf(undefined, CHOICES)).toBeUndefined()
  })

  it('knows that only gateway backings lack versioning', () => {
    expect(backingSupportsVersioning('gateway')).toBe(false)
    expect(backingSupportsVersioning('blob')).toBe(true)
    expect(backingSupportsVersioning('local')).toBe(true)
    expect(backingSupportsVersioning(undefined)).toBe(true)
  })

  it('builds select items with the default marked and the mode described', () => {
    const items = backingSelectItems(CHOICES)
    expect(items.map(i => [i.value, i.label])).toEqual([['local', 'local'], ['aws', 'aws'], ['archive', 'archive (default)']])
    expect(items[0]!.description).toBe(backingModeInfo('local').description)
    expect(items[1]!.description).toMatch(/^Remote \(gateway\): .*no versioning or object lock/)
    expect(items[2]!.description).toMatch(/^Remote \(blob\): .*all features/)
    expect(backingSelectItems({ default: 'local', available: [{ name: 'local', mode: 'local' }] })).toEqual([])
  })

  it('describes unknown modes generically', () => {
    expect(backingModeInfo('tape').label).toBe('tape')
    expect(backingModeInfo(undefined).label).toBe('Unknown')
  })
})
