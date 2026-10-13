// Storage backings and bucket placement: admin client calls, capability
// gating and the pure helpers behind the Storage page, the Placement tab
// and the bucket list chips.
import { describe, expect, it, vi } from 'vitest'
import { AdminClient } from '~/utils/admin/client'
import type { Backing, Capabilities, Placement, PlacementMember } from '~/types/admin'
import {
  BUCKET_TABS,
  NO_CAPS,
  bucketSettingsTabs,
  canManageStorage,
  canViewStorage,
  resolveBucketTab
} from '~/components/admin/-capabilities'
import {
  addedReplicas,
  copyMembers,
  formatProgress,
  memberStateMeta,
  modeLabel,
  moveTargets,
  placementBusy,
  placementChips,
  placementWorthShowing,
  promoteAdvice,
  removedReplicas,
  replicaChoices,
  replicaNames,
  replicationChanged,
  sortBackings,
  validateReplication
} from '~/components/admin/-storage'
import { NO_CAPABILITIES } from '~/stores/connections'

interface Captured { method: string, url: string, body: string, payloadHash: string }

function makeAdmin(responder: (c: Captured) => Response) {
  const calls: Captured[] = []
  const fetch = vi.fn(async (req: Request) => {
    const body = ['GET', 'HEAD'].includes(req.method) ? '' : await req.text()
    const c = { method: req.method, url: req.url, body, payloadHash: req.headers.get('x-amz-content-sha256') ?? '' }
    calls.push(c)
    return responder(c)
  })
  const admin = new AdminClient({ endpoint: 'http://localhost:9000', accessKeyId: 'AK', secretAccessKey: 'SK' }, { fetch, datetime: '20261009T120000Z' })
  return { admin, calls }
}

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })

const backing = (name: string, over: Partial<Backing> = {}): Backing => ({
  name, kind: 'fs', path: `/mnt/${name}`, default: false, healthy: true, lastError: '', checkedAt: '2026-10-12T10:00:00Z', buckets: 0, replicas: 0, disk: null, ...over
})

const member = (b: string, over: Partial<PlacementMember> = {}): PlacementMember => ({
  backing: b, role: 'replica', state: 'in-sync', healthy: true, pending: 0, progress: null, lastError: '', ...over
})

const placement = (over: Partial<Placement> = {}): Placement => ({
  bucket: 'photos',
  mode: 'active-passive',
  primary: 'default',
  members: [member('default', { role: 'primary' }), member('fast')],
  move: null,
  ...over
})

const BACKINGS = [backing('default', { default: true }), backing('fast'), backing('archive'), backing('broken', { healthy: false, lastError: 'disk gone' })]

describe('AdminClient storage endpoints', () => {
  it('lists backings and tolerates a null list', async () => {
    const { admin, calls } = makeAdmin(() => json({ defaultBacking: 'default', autoFailover: true, backings: null }))
    expect(await admin.listBackings()).toEqual({ defaultBacking: 'default', autoFailover: true, backings: [] })
    expect(calls[0]).toMatchObject({ method: 'GET', url: 'http://localhost:9000/_gos3/admin/v1/backings' })
  })

  it('reads and changes a bucket placement with signed JSON bodies', async () => {
    const { admin, calls } = makeAdmin(() => json(placement()))
    await admin.getBucketPlacement('my bucket')
    await admin.updateBucketPlacement('photos', 'active-active', ['fast', 'archive'])
    await admin.updateBucketPlacement('photos', 'single', [])
    expect(calls.map(c => `${c.method} ${c.url.replace('http://localhost:9000/_gos3/admin/v1', '')}`)).toEqual([
      'GET /buckets/my%20bucket/placement',
      'PUT /buckets/photos/placement',
      'PUT /buckets/photos/placement'
    ])
    expect(JSON.parse(calls[1]!.body)).toEqual({ mode: 'active-active', replicas: ['fast', 'archive'] })
    expect(JSON.parse(calls[2]!.body)).toEqual({ mode: 'single', replicas: [] })
    for (const c of calls) expect(c.payloadHash).toMatch(/^[0-9a-f]{64}$/)
  })

  it('moves, cancels, promotes and resyncs', async () => {
    const { admin, calls } = makeAdmin(c => json(placement(), c.method === 'POST' ? 202 : 200))
    await admin.moveBucket('photos', 'archive')
    await admin.moveBucket('photos', 'archive', 'fast')
    await admin.cancelBucketMove('photos')
    await admin.promoteReplica('photos', 'fast')
    await admin.promoteReplica('photos', 'fast', true)
    await admin.resyncBucket('photos')
    await admin.resyncBucket('photos', 'fast')
    const sent = calls.map(c => [c.method, c.url.replace('http://localhost:9000/_gos3/admin/v1/buckets/photos/placement', ''), c.body])
    expect(sent).toEqual([
      ['POST', '/move', '{"to":"archive"}'],
      ['POST', '/move', '{"to":"archive","from":"fast"}'],
      ['DELETE', '/move', ''],
      ['POST', '/promote', '{"backing":"fast"}'],
      ['POST', '/promote', '{"backing":"fast","force":true}'],
      ['POST', '/resync', '{}'],
      ['POST', '/resync', '{"backing":"fast"}']
    ])
  })

  it('surfaces 409 conflicts as AdminError', async () => {
    const { admin } = makeAdmin(() => json({ error: { code: 'Conflict', message: 'a move is already in progress' } }, 409))
    await expect(admin.moveBucket('photos', 'archive')).rejects.toMatchObject({ code: 'Conflict', status: 409, message: 'a move is already in progress' })
  })
})

describe('storage capabilities', () => {
  const caps = (over: Partial<Capabilities> = {}): Capabilities => ({ ...NO_CAPS, ...over })

  it('defaults to false everywhere capabilities are enumerated', () => {
    expect(NO_CAPS).toMatchObject({ viewStorage: false, manageStorage: false })
    expect(NO_CAPABILITIES).toMatchObject({ viewStorage: false, manageStorage: false })
  })

  it('needs gos3 and the capability; a missing field (older server) is false', () => {
    expect(canViewStorage('gos3', caps({ viewStorage: true }))).toBe(true)
    expect(canViewStorage('s3', caps({ viewStorage: true }))).toBe(false)
    expect(canManageStorage('gos3', caps({ viewStorage: true }))).toBe(false)
    expect(canManageStorage('gos3', caps({ manageStorage: true }))).toBe(true)
    const { viewStorage: _v, manageStorage: _m, ...old } = caps({ admin: true })
    expect(canViewStorage('gos3', old)).toBe(false)
    expect(canManageStorage('gos3', null)).toBe(false)
  })

  it('adds the Placement tab before the danger zone only with viewStorage', () => {
    expect(bucketSettingsTabs('gos3', NO_CAPS)).toBe(BUCKET_TABS)
    expect(bucketSettingsTabs('s3', caps({ viewStorage: true }))).toBe(BUCKET_TABS)
    const tabs = bucketSettingsTabs('gos3', caps({ viewStorage: true })).map(t => t.value)
    expect(tabs.slice(-2)).toEqual(['placement', 'danger'])
    expect(tabs).toHaveLength(BUCKET_TABS.length + 1)
    expect(resolveBucketTab('placement')).toBe('overview')
    expect(resolveBucketTab('placement', bucketSettingsTabs('gos3', caps({ viewStorage: true })))).toBe('placement')
  })
})

describe('placement helpers', () => {
  it('polls while copies run, changes are pending or a move is in progress', () => {
    expect(placementBusy(null)).toBe(false)
    expect(placementBusy(placement())).toBe(false)
    expect(placementBusy(placement({ members: [member('default', { role: 'primary' }), member('fast', { state: 'syncing', progress: { keys: 1, versions: 1, bytes: 1 } })] }))).toBe(true)
    expect(placementBusy(placement({ members: [member('default', { role: 'primary' }), member('fast', { state: 'lagging', pending: 3 })] }))).toBe(true)
    expect(placementBusy(placement({ move: { from: 'default', to: 'archive', startedAt: '2026-10-12T10:00:00Z', phase: 'copying' } }))).toBe(true)
    // A degraded or offline member without pending work does not poll forever.
    expect(placementBusy(placement({ members: [member('default', { role: 'primary' }), member('fast', { state: 'offline', healthy: false })] }))).toBe(false)
  })

  it('lists members, replicas and labels', () => {
    const p = placement({ members: [member('default', { role: 'primary' }), member('fast'), member('archive', { role: 'move-target', state: 'syncing' })] })
    expect(copyMembers(p).map(m => m.backing)).toEqual(['default', 'fast'])
    expect(replicaNames(p)).toEqual(['fast'])
    expect(modeLabel('active-active')).toBe('Active-active')
    expect(modeLabel('weird')).toBe('weird')
    expect(memberStateMeta('lagging')).toMatchObject({ label: 'Lagging', color: 'warning' })
    expect(memberStateMeta('in-sync').color).toBe('success')
    expect(memberStateMeta('degraded').color).toBe('error')
    expect(memberStateMeta('mystery')).toMatchObject({ label: 'mystery', color: 'neutral' })
  })

  it('offers every backing but the primary as a replica, marking unhealthy ones', () => {
    const choices = replicaChoices(BACKINGS, placement())
    expect(choices.map(c => c.value)).toEqual(['archive', 'broken', 'fast'])
    expect(choices.find(c => c.value === 'broken')?.healthy).toBe(false)
    // Replicas on backings the listing does not know about stay visible (and can be removed).
    expect(replicaChoices([], placement()).map(c => c.value)).toEqual(['fast'])
  })

  it('validates and diffs the replication form', () => {
    expect(validateReplication('single', [])).toBe('')
    expect(validateReplication('single', ['fast'])).toMatch(/no replicas/)
    expect(validateReplication('active-passive', [])).toMatch(/at least one/)
    expect(validateReplication('active-active', ['fast'])).toBe('')
    expect(removedReplicas(['fast', 'archive'], ['archive', 'other'])).toEqual(['fast'])
    expect(addedReplicas(['fast', 'archive'], ['archive', 'other'])).toEqual(['other'])
    expect(replicationChanged(placement(), 'active-passive', ['fast'])).toBe(false)
    expect(replicationChanged(placement(), 'active-active', ['fast'])).toBe(true)
    expect(replicationChanged(placement(), 'active-passive', ['fast', 'archive'])).toBe(true)
  })

  it('offers backings without a copy as move targets (unhealthy ones disabled)', () => {
    const targets = moveTargets(BACKINGS, placement())
    expect(targets.map(t => t.value)).toEqual(['archive', 'broken'])
    expect(targets.find(t => t.value === 'broken')?.disabledReason).toBe('unhealthy')
    const moving = placement({ members: [member('default', { role: 'primary' }), member('fast'), member('archive', { role: 'move-target' })] })
    expect(moveTargets(BACKINGS, moving).map(t => t.value)).toEqual(['broken'])
  })

  it('suggests a forced promote only when data could be lost or the primary is down', () => {
    expect(promoteAdvice(placement(), 'fast')).toEqual({ suggestForce: false, reason: '', normalRefused: false })
    const lagging = placement({ members: [member('default', { role: 'primary' }), member('fast', { state: 'lagging', pending: 2 })] })
    expect(promoteAdvice(lagging, 'fast')).toMatchObject({ suggestForce: true, normalRefused: false, reason: 'fast is lagging (2 pending changes)' })
    const syncing = placement({ members: [member('default', { role: 'primary' }), member('fast', { state: 'syncing' })] })
    expect(promoteAdvice(syncing, 'fast')).toMatchObject({ suggestForce: true, normalRefused: true })
    const primaryDown = placement({ members: [member('default', { role: 'primary', healthy: false, state: 'offline' }), member('fast')] })
    expect(promoteAdvice(primaryDown, 'fast')).toMatchObject({ suggestForce: true, reason: 'the primary default is offline' })
  })

  it('builds bucket list chips and hides them when every bucket sits on one backing', () => {
    expect(placementChips(undefined)).toEqual([])
    expect(placementChips({ mode: 'single', primary: 'default', replicas: [], moving: false }).map(c => c.label)).toEqual(['default'])
    const ap = placementChips({ mode: 'active-passive', primary: 'default', replicas: ['fast'], moving: false })
    expect(ap.map(c => c.label)).toEqual(['default', 'AP ×2'])
    expect(ap[1]!.title).toContain('replicas on fast')
    expect(placementChips({ mode: 'active-active', primary: 'fast', replicas: ['a', 'b'], moving: true }).map(c => c.label)).toEqual(['fast', 'AA ×3', 'moving'])

    const single = { mode: 'single' as const, primary: 'default', replicas: null, moving: false }
    expect(placementWorthShowing([])).toBe(false)
    expect(placementWorthShowing([undefined, single, single])).toBe(false)
    expect(placementWorthShowing([single, { ...single, primary: 'archive' }])).toBe(true)
    expect(placementWorthShowing([single, { ...single, moving: true }])).toBe(true)
    expect(placementWorthShowing([{ ...single, mode: 'active-passive', replicas: ['fast'] }])).toBe(true)
  })

  it('formats copy progress and sorts backings default-first', () => {
    expect(formatProgress({ keys: 1, versions: 2, bytes: 2048 }, n => String(n), b => `${b} B`)).toBe('1 key · 2 versions · 2048 B')
    expect(formatProgress(null, String, String)).toBe('')
    expect(sortBackings([backing('b'), backing('a'), backing('z', { default: true })]).map(b => b.name)).toEqual(['z', 'a', 'b'])
    expect(sortBackings(null)).toEqual([])
  })
})
