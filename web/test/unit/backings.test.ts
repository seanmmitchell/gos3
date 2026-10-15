import { describe, expect, it } from 'vitest'
import type { AvailableBackings, Backing, Placement } from '~/types/admin'
import { moveTargets, placementChips, replicaChoices } from '~/components/admin/-storage'
import {
  backingChoices,
  backingKindInfo,
  backingSelectItems,
  defaultBacking,
  findBacking,
  isGatewayBacking
} from '~/utils/backings'

const CHOICES: AvailableBackings = {
  default: 'cold',
  available: [
    { name: 'default', kind: 'fs' },
    { name: 'gw', kind: 's3', mode: 'gateway' },
    { name: 'cold', kind: 's3' }
  ]
}

describe('storage backing choices (WhoAmI.backings)', () => {
  it('offers a choice only when there is more than one backing', () => {
    expect(backingChoices(undefined)).toEqual([])
    expect(backingChoices({ default: 'default', available: null })).toEqual([])
    expect(backingChoices({ default: 'default', available: [{ name: 'default', kind: 'fs' }] })).toEqual([])
    expect(backingChoices(CHOICES).map(b => b.name)).toEqual(['default', 'gw', 'cold'])
  })

  it('resolves the default, falling back to the first available backing', () => {
    expect(defaultBacking(CHOICES)).toBe('cold')
    expect(defaultBacking({ ...CHOICES, default: 'gone' })).toBe('default')
    expect(defaultBacking(null)).toBe('')
  })

  it('finds backings and recognises gateway ones', () => {
    expect(findBacking('gw', CHOICES)).toEqual({ name: 'gw', kind: 's3', mode: 'gateway' })
    expect(findBacking('other', CHOICES)).toBeUndefined()
    expect(findBacking(undefined, CHOICES)).toBeUndefined()
    expect(isGatewayBacking(findBacking('gw', CHOICES))).toBe(true)
    expect(isGatewayBacking(findBacking('cold', CHOICES))).toBe(false)
    expect(isGatewayBacking({ kind: 'fs', mode: 'gateway' })).toBe(false)
    expect(isGatewayBacking(undefined)).toBe(false)
  })

  it('describes kinds and s3 modes, with a generic fallback', () => {
    expect(backingKindInfo({ kind: 'fs' }).label).toBe('Disk')
    expect(backingKindInfo({ kind: 'memory' }).description).toMatch(/lost on restart/)
    expect(backingKindInfo({ kind: 's3' })).toEqual({ label: 'Remote S3', description: 'Data and metadata stored in a remote S3 bucket; all features available.', icon: 'i-lucide-cloud' })
    expect(backingKindInfo({ kind: 's3', mode: '' }).label).toBe('Remote S3')
    expect(backingKindInfo({ kind: 's3', mode: 'gateway' })).toEqual({ label: 'Remote S3 (gateway)', description: 'Objects stored natively; no versioning or object lock.', icon: 'i-lucide-cloud-cog' })
    // The removed blob mode is no longer a known variant.
    expect(backingKindInfo({ kind: 's3', mode: 'blob' }).label).toBe('s3 (blob)')
    expect(backingKindInfo({ kind: 's3', mode: 'tape' }).label).toBe('s3 (tape)')
    expect(backingKindInfo({ kind: 'nfs' }).label).toBe('nfs')
    expect(backingKindInfo(undefined).label).toBe('Unknown')
  })

  it('builds select items with the default marked and the kind described', () => {
    const items = backingSelectItems(CHOICES)
    expect(items.map(i => [i.value, i.label])).toEqual([['default', 'default'], ['gw', 'gw'], ['cold', 'cold (default)']])
    expect(items[0]!.description).toMatch(/^Disk: /)
    expect(items[1]!.description).toMatch(/^Remote S3 \(gateway\): .*no versioning or object lock/)
    expect(items[2]!.description).toMatch(/^Remote S3: .*all features/)
  })
})

describe('gateway backings in bucket placement', () => {
  const backing = (name: string, over: Partial<Backing> = {}): Backing => ({
    name, kind: 'fs', path: `/mnt/${name}`, default: false, healthy: true, lastError: '', buckets: 0, replicas: 0, disk: null, ...over
  })
  const BACKINGS = [backing('default', { default: true }), backing('gw', { kind: 's3', mode: 'gateway', path: '' }), backing('cold', { kind: 's3', path: '', location: 's3://cold/gos3/' })]
  const placement: Placement = { bucket: 'b', mode: 'single', primary: 'default', members: [{ backing: 'default', role: 'primary', state: 'in-sync', healthy: true, pending: 0, progress: null, lastError: '' }], move: null }

  it('are neither replica choices nor selectable move targets', () => {
    expect(replicaChoices(BACKINGS, placement).map(c => c.value)).toEqual(['cold'])
    const targets = moveTargets(BACKINGS, placement)
    expect(targets.find(t => t.value === 'gw')?.disabledReason).toBe('gateway backing')
    expect(targets.find(t => t.value === 'cold')?.disabledReason).toBeUndefined()
  })

  it('give the primary chip their kind when described', () => {
    const chips = placementChips({ mode: 'single', primary: 'gw', replicas: null, moving: false }, name => backingKindInfo(findBacking(name, CHOICES)))
    expect(chips[0]).toMatchObject({ label: 'gw', icon: 'i-lucide-cloud-cog', title: 'Primary copy on gw · Remote S3 (gateway)' })
  })
})
