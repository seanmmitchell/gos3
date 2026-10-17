import { describe, expect, it } from 'vitest'
import type { AvailableBackings, Backing, Placement } from '~/types/admin'
import { copyRefusal, moveTargets, placementChips, replicaChoices } from '~/components/admin/-storage'
import {
  backingChoices,
  backingContainers,
  backingKindInfo,
  backingSelectItems,
  bucketKindInfo,
  containerItems,
  defaultBacking,
  defaultBucketKind,
  defaultContainer,
  findBacking,
  isBucketKind,
  kindChoices,
  offeredKinds
} from '~/utils/backings'

const CHOICES: AvailableBackings = {
  default: 'cold',
  available: [
    { name: 'default', kind: 'fs' },
    { name: 'acct', kind: 's3', kinds: ['native'], defaultKind: 'native', containers: [] },
    {
      name: 'cold',
      kind: 's3',
      kinds: ['native', 'wrapped'],
      defaultKind: 'wrapped',
      containers: [{ id: 'home', label: 'shared', default: true }, { id: 'c-7f3a', label: 'photos-archive' }, { id: 'c-0001' }]
    }
  ]
}

describe('storage backing choices (WhoAmI.backings)', () => {
  it('offers a choice only when there is more than one backing', () => {
    expect(backingChoices(undefined)).toEqual([])
    expect(backingChoices({ default: 'default', available: null })).toEqual([])
    expect(backingChoices({ default: 'default', available: [{ name: 'default', kind: 'fs' }] })).toEqual([])
    expect(backingChoices(CHOICES).map(b => b.name)).toEqual(['default', 'acct', 'cold'])
  })

  it('resolves the default, falling back to the first available backing', () => {
    expect(defaultBacking(CHOICES)).toBe('cold')
    expect(defaultBacking({ ...CHOICES, default: 'gone' })).toBe('default')
    expect(defaultBacking(null)).toBe('')
  })

  it('finds backings by name', () => {
    expect(findBacking('acct', CHOICES)?.kinds).toEqual(['native'])
    expect(findBacking('other', CHOICES)).toBeUndefined()
    expect(findBacking(undefined, CHOICES)).toBeUndefined()
  })

  it('describes backing kinds, with a generic fallback', () => {
    expect(backingKindInfo({ kind: 'fs' }).label).toBe('Disk')
    expect(backingKindInfo({ kind: 'memory' }).description).toMatch(/lost on restart/)
    expect(backingKindInfo({ kind: 's3' })).toMatchObject({ label: 'Remote S3', icon: 'i-lucide-cloud' })
    expect(backingKindInfo({ kind: 's3' }).description).toMatch(/native buckets .* wrapped buckets/)
    expect(backingKindInfo({ kind: 'nfs' }).label).toBe('nfs')
    expect(backingKindInfo(undefined).label).toBe('Unknown')
  })

  it('builds select items with the default marked and the kind described', () => {
    const items = backingSelectItems(CHOICES)
    expect(items.map(i => [i.value, i.label])).toEqual([['default', 'default'], ['acct', 'acct'], ['cold', 'cold (default)']])
    expect(items[0]!.description).toMatch(/^Disk: /)
    expect(items[2]!.description).toMatch(/^Remote S3: /)
  })
})

describe('bucket kinds (s3 backings)', () => {
  it('describes native and wrapped buckets', () => {
    expect(bucketKindInfo('native')).toEqual({
      label: 'Native',
      description: 'Objects stored as-is in a remote bucket; versioning and object lock follow the remote bucket.',
      icon: 'i-lucide-cloud'
    })
    expect(bucketKindInfo('wrapped')).toEqual({ label: 'Wrapped', description: 'gos3 format inside a container; every gos3 feature.', icon: 'i-lucide-package' })
    expect(bucketKindInfo('tape').label).toBe('tape')
    expect(bucketKindInfo(undefined).label).toBe('Unknown')
    expect(isBucketKind('native')).toBe(true)
    expect(isBucketKind('gateway')).toBe(false)
  })

  it('offers the known kinds of s3 backings only, wrapped first', () => {
    expect(offeredKinds(findBacking('cold', CHOICES))).toEqual(['wrapped', 'native'])
    expect(offeredKinds(findBacking('acct', CHOICES))).toEqual(['native'])
    expect(offeredKinds(findBacking('default', CHOICES))).toEqual([])
    // A server that reports no kinds: the server decides (no header sent).
    expect(offeredKinds({ kind: 's3' })).toEqual([])
    expect(offeredKinds({ kind: 's3', kinds: ['wrapped', 'tape'] })).toEqual(['wrapped'])
    expect(offeredKinds({ kind: 'fs', kinds: ['native'] })).toEqual([])
  })

  it('picks the default kind, falling back to the first offered one', () => {
    expect(defaultBucketKind(findBacking('cold', CHOICES))).toBe('wrapped')
    expect(defaultBucketKind(findBacking('acct', CHOICES))).toBe('native')
    expect(defaultBucketKind({ kind: 's3', kinds: ['native', 'wrapped'], defaultKind: 'native' })).toBe('native')
    expect(defaultBucketKind({ kind: 's3', kinds: ['native', 'wrapped'], defaultKind: 'tape' })).toBe('wrapped')
    expect(defaultBucketKind({ kind: 'fs' })).toBe('')
    expect(defaultBucketKind(undefined)).toBe('')
  })

  it('builds radio items with the default marked', () => {
    expect(kindChoices(findBacking('cold', CHOICES)).map(k => [k.value, k.label])).toEqual([['wrapped', 'Wrapped (default)'], ['native', 'Native']])
    expect(kindChoices(findBacking('cold', CHOICES))[1]!.description).toMatch(/follow the remote bucket/)
    expect(kindChoices(findBacking('default', CHOICES))).toEqual([])
  })
})

describe('containers of wrapped buckets', () => {
  const cold = findBacking('cold', CHOICES)

  it('lists the containers and the default one', () => {
    expect(backingContainers(cold).map(c => c.id)).toEqual(['home', 'c-7f3a', 'c-0001'])
    expect(backingContainers(findBacking('default', CHOICES))).toEqual([])
    expect(defaultContainer(cold)).toBe('home')
    expect(defaultContainer(findBacking('acct', CHOICES))).toBe('')
    expect(defaultContainer({ containers: [{ id: 'c-1' }] })).toBe('')
  })

  it('builds select items: label (or ID), default marked, ID as description', () => {
    expect(containerItems(cold)).toEqual([
      { label: 'shared (default)', value: 'home', description: 'home' },
      { label: 'photos-archive', value: 'c-7f3a', description: 'c-7f3a' },
      { label: 'c-0001', value: 'c-0001' }
    ])
    expect(containerItems({ containers: [{ id: 'home', default: true }] })).toEqual([{ label: 'shared (home) (default)', value: 'home' }])
    expect(containerItems(null)).toEqual([])
  })
})

describe('s3 backings in bucket placement', () => {
  const backing = (name: string, over: Partial<Backing> = {}): Backing => ({
    name, kind: 'fs', path: `/mnt/${name}`, default: false, healthy: true, lastError: '', buckets: 0, replicas: 0, disk: null, ...over
  })
  const account = (containers: number) => ({
    home: null, records: 'local', defaultKind: 'native', defaultContainer: '', kinds: containers ? ['native', 'wrapped'] : ['native'], nativeBuckets: 1, wrappedBuckets: 0,
    containers: Array.from({ length: containers }, (_, i) => ({ id: `c-${i}`, location: { bucket: `b${i}` }, role: 'dedicated', buckets: 0, state: 'ready' }))
  })
  const BACKINGS = [
    backing('default', { default: true }),
    backing('acct', { kind: 's3', path: '', location: 'local:', kinds: ['native'], account: account(0) }),
    backing('cold', { kind: 's3', path: '', location: 's3://cold/gos3/', kinds: ['native', 'wrapped'], account: account(1) }),
    backing('old', { kind: 's3', path: '', location: 's3://old/' })
  ]
  const placement: Placement = { bucket: 'b', mode: 'single', primary: 'default', members: [{ backing: 'default', role: 'primary', state: 'in-sync', healthy: true, pending: 0, progress: null, lastError: '' }], move: null }

  it('refuses copies on s3 backings without a container, and replicas without a default one', () => {
    expect(copyRefusal(BACKINGS[0]!)).toBe('')
    expect(copyRefusal(BACKINGS[1]!)).toBe('no container')
    expect(copyRefusal(BACKINGS[1]!, 'replica')).toBe('no container')
    // cold has a container but no default: moves name a container, replicas cannot.
    expect(copyRefusal(BACKINGS[2]!)).toBe('')
    expect(copyRefusal(BACKINGS[2]!, 'replica')).toBe('no default container')
    expect(copyRefusal({ ...BACKINGS[2]!, account: { ...BACKINGS[2]!.account!, defaultContainer: 'c-0' } }, 'replica')).toBe('')
    // Without kinds or account (older server), the server decides.
    expect(copyRefusal(BACKINGS[3]!)).toBe('')
    expect(copyRefusal({ kind: 's3', account: { ...account(0), kinds: null } })).toBe('no container')
  })

  it('are neither replica choices nor selectable move targets without a container', () => {
    expect(replicaChoices(BACKINGS, placement).map(c => c.value)).toEqual(['old'])
    const targets = moveTargets(BACKINGS, placement)
    expect(targets.find(t => t.value === 'acct')?.disabledReason).toBe('no container')
    expect(targets.find(t => t.value === 'cold')?.disabledReason).toBeUndefined()
  })

  it('give the primary chip their kind when described', () => {
    const chips = placementChips({ mode: 'single', primary: 'cold', replicas: null, moving: false }, name => backingKindInfo(findBacking(name, CHOICES)))
    expect(chips[0]).toMatchObject({ label: 'cold', icon: 'i-lucide-cloud', title: 'Primary copy on cold · Remote S3' })
  })
})
