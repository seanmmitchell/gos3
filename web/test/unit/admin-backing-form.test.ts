import { describe, expect, it } from 'vitest'
import { copyBackingConfiguration, validateBackingConfiguration } from '~/components/admin/-backing-form'
import type { BackingConfiguration } from '~/types/admin'

const form = (): BackingConfiguration => ({ backings: [{ name: 'archive', backend: 'fs', path: '/mnt/archive' }], defaultBacking: 'archive', revision: 'abc', persistent: true })

describe('backing configuration', () => {
  it('accepts filesystem and memory backings and preserves the revision when copied', () => {
    const f = form()
    f.backings.push({ name: 'cache', backend: 'memory', path: '' })
    expect(validateBackingConfiguration(f)).toBeUndefined()
    const copy = copyBackingConfiguration(f)
    copy.backings[0]!.path = '/new'
    expect(f.backings[0]!.path).toBe('/mnt/archive')
    expect(copy.revision).toBe('abc')
  })

  it.each(['default', 'INVALID', '', '-archive', 'a'.repeat(33)])('rejects name %j', (name) => {
    const f = form()
    f.backings[0]!.name = name
    expect(validateBackingConfiguration(f)).toBeTruthy()
  })

  it('rejects duplicate names, paths, missing paths, and a removed default', () => {
    const f = form()
    f.backings.push({ ...f.backings[0]! })
    expect(validateBackingConfiguration(f)).toContain('already used')
    f.backings[1]!.name = 'spare'
    expect(validateBackingConfiguration(f)).toContain('own directory')
    f.backings.pop()
    f.backings[0]!.path = ' '
    expect(validateBackingConfiguration(f)).toContain('directory')
    f.backings = []
    expect(validateBackingConfiguration(f)).toContain('existing backing')
  })
})
