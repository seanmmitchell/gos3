// Storage backings and bucket placement components: the backing card of the
// server Storage page and the bucket settings Placement tab, against a fake
// gos3 admin API.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, nextTick, type Component } from 'vue'
import { UApp } from '#components'
import type { Backing, Capabilities, Info, Placement, WhoAmI } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import BackingCard from '~/components/admin/BackingCard.vue'
import BackingConfiguration from '~/components/admin/BackingConfiguration.vue'
import PlacementTab from '~/components/bucket-settings/PlacementTab.vue'

const INFO: Info = {
  service: 'gos3',
  id: 'b3f1c2d4e5f60718',
  name: 'nas',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.9.9.2:9000', admin: 'http://10.9.9.2:9000/_gos3/admin/v1' },
  features: [],
  startedAt: '2026-10-09T12:00:00Z'
}

const backing = (name: string, over: Partial<Backing> = {}): Backing => ({
  name, kind: 'fs', path: `/mnt/${name}`, default: false, healthy: true, lastError: '', checkedAt: '2026-10-12T10:00:00Z', buckets: 1, replicas: 0, disk: null, ...over
})

const BACKINGS: Backing[] = [backing('default', { default: true }), backing('fast'), backing('archive'), backing('broken', { healthy: false, lastError: 'stat /mnt/broken: no such file or directory' })]

function basePlacement(): Placement {
  return {
    bucket: 'photos',
    mode: 'active-passive',
    primary: 'default',
    members: [
      { backing: 'default', role: 'primary', state: 'in-sync', healthy: true, pending: 0, progress: null, lastError: '' },
      { backing: 'fast', role: 'replica', state: 'lagging', healthy: true, pending: 7, progress: null, lastSyncAt: '2026-10-12T09:00:00Z', lastError: 'copy photos/a.jpg: timeout' }
    ],
    move: null
  }
}

interface Seen { method: string, path: string, body: string }

/** Fake gos3 at 10.9.9.2:9000: whoami with `caps`, backings and the placement of "photos". */
function fakeServer(caps: Partial<Capabilities>, placement: Placement = basePlacement()) {
  const seen: Seen[] = []
  let current = placement
  let config = { backings: [{ name: 'fast', backend: 'fs', path: '/mnt/fast' }], defaultBacking: 'default', revision: 'v1', persistent: true }
  const whoami = (): WhoAmI => ({ user: 'alice', accessKey: 'AKALICE', isRoot: false, policies: [], capabilities: { ...NO_CAPABILITIES, ...caps }, server: INFO })
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input))
    const url = new URL(req.url)
    const body = req.method === 'GET' || req.method === 'DELETE' ? '' : await req.text()
    seen.push({ method: req.method, path: url.pathname, body })
    const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })
    if (url.host !== '10.9.9.2:9000') throw new TypeError('Failed to fetch')
    const p = url.pathname.replace('/_gos3/admin/v1', '')
    if (url.pathname === '/_gos3/info') return json(INFO)
    if (p === '/whoami') return json(whoami())
    if (p === '/backings') return json({ defaultBacking: 'default', autoFailover: false, backings: BACKINGS })
    if (p === '/backings/config' && req.method === 'GET') return json(config)
    if (p === '/backings/config' && req.method === 'PUT') {
      config = { ...JSON.parse(body), revision: 'v2' }
      return json(config)
    }
    if (p === '/buckets/photos/placement' && req.method === 'GET') return json(current)
    if (p === '/buckets/photos/placement' && req.method === 'PUT') {
      const r = JSON.parse(body) as { mode: Placement['mode'], replicas: string[] }
      current = {
        ...current,
        mode: r.mode,
        members: [current.members![0]!, ...r.replicas.map(b => ({ backing: b, role: 'replica' as const, state: 'in-sync' as const, healthy: true, pending: 0, progress: null, lastError: '' }))]
      }
      return json(current)
    }
    if (p === '/buckets/photos/placement/resync') return json(current, 202)
    return new Response('', { status: 404 })
  })
  vi.stubGlobal('fetch', fetch)
  return { seen }
}

/** Mounts `comp` inside UApp (tooltips need its provider); `inner` is the component itself. */
async function mountInApp(comp: Component, props: Record<string, unknown>) {
  const Wrapper = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(comp, props) }) })
  const w = await mountSuspended(Wrapper)
  return { w, inner: w.findComponent(comp) }
}

async function connect() {
  const store = useConnectionsStore()
  const c = store.add({ name: 'nas', endpoint: 'http://10.9.9.2:9000', accessKeyId: 'AKALICE', secretAccessKey: 'SK', kind: 'gos3' })
  store.setActive(c.id)
  await store.refreshStatus(c.id)
  return c
}

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  useConnectionsStore().resetState()
})
afterEach(() => {
  vi.unstubAllGlobals()
})

describe('BackingCard', () => {
  it('shows kind, path, default badge, counts and a labelled disk meter', async () => {
    const { w } = await mountInApp(BackingCard, { backing: backing('fast', { default: true, buckets: 3, replicas: 1, disk: { path: '/mnt/fast', totalBytes: 1000, usedBytes: 400, freeBytes: 600 } }) })
    expect(w.find('h2').text()).toBe('fast')
    expect(w.text()).toContain('Default')
    expect(w.text()).toContain('/mnt/fast')
    expect(w.text()).toContain('3 buckets · 1 replica')
    const meter = w.find('[role="meter"]')
    expect(meter.attributes('aria-label')).toBe('Disk usage of fast')
    expect(meter.attributes('aria-valuenow')).toBe('40')
    expect(w.text()).toContain('Healthy')
  })

  it('shows an unhealthy backing with its last error and no disk for memory', async () => {
    const { w } = await mountInApp(BackingCard, { backing: backing('mem', { kind: 'memory', path: '', healthy: false, lastError: 'boom' }) })
    expect(w.text()).toContain('Unhealthy')
    expect(w.text()).toContain('boom')
    expect(w.text()).toContain('In memory')
    expect(w.find('[role="meter"]').exists()).toBe(false)
  })
})

describe('BackingConfiguration', () => {
  it('loads, validates and saves a new backing without reloading the server', async () => {
    const { seen } = fakeServer({ admin: true, manageConfig: true, viewStorage: true })
    await connect()
    const { w, inner } = await mountInApp(BackingConfiguration, { backings: BACKINGS })
    const button = (label: string) => w.findAll('button').find(b => b.text() === label)!
    await button('Configure backings').trigger('click')
    await vi.waitFor(() => expect(w.find('form').exists()).toBe(true))
    expect(w.find('input[aria-label="Backing name"]').attributes('disabled')).toBeDefined()
    expect(w.find('input[aria-label="Directory on the server"]').attributes('disabled')).toBeDefined()
    expect(button('Remove').attributes('disabled')).toBeDefined()
    await button('Add backing').trigger('click')
    await w.find('form').trigger('submit')
    expect(seen.some(s => s.method === 'PUT')).toBe(false)
    const inputs = w.findAll('input[aria-label="Backing name"]')
    await inputs[1]!.setValue('fast')
    // A duplicate typed into a new row must remain editable.
    expect(inputs[1]!.attributes('disabled')).toBeUndefined()
    await inputs[1]!.setValue('cold')
    await w.findAll('input[aria-label="Directory on the server"]')[1]!.setValue('/mnt/archive')
    await w.find('form').trigger('submit')
    await vi.waitFor(() => expect(inner.emitted('saved')).toHaveLength(1))
    const request = JSON.parse(seen.find(s => s.method === 'PUT')!.body)
    expect(request.revision).toBe('v1')
    expect(request.backings[1]).toEqual({ name: 'cold', backend: 'fs', path: '/mnt/archive' })
    expect(button('Save backing configuration').attributes('disabled')).toBeDefined()
    w.unmount()
  })

  it('removes an empty backing and saves the remaining configuration', async () => {
    const { seen } = fakeServer({ admin: true, manageConfig: true })
    await connect()
    const { w, inner } = await mountInApp(BackingConfiguration, { backings: [backing('fast', { buckets: 0 })] })
    await w.findAll('button').find(b => b.text() === 'Configure backings')!.trigger('click')
    await vi.waitFor(() => expect(w.find('form').exists()).toBe(true))
    await w.findAll('button').find(b => b.text() === 'Remove')!.trigger('click')
    await w.find('form').trigger('submit')
    await vi.waitFor(() => expect(inner.emitted('saved')).toHaveLength(1))
    expect(JSON.parse(seen.find(s => s.method === 'PUT')!.body).backings).toEqual([])
    w.unmount()
  })
})

describe('PlacementTab', () => {
  it('lists members with role, state, pending changes and errors; read-only without manageStorage', async () => {
    fakeServer({ admin: true, viewStorage: true })
    await connect()
    const { w, inner } = await mountInApp(PlacementTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="member-fast"]').exists()).toBe(true))
    expect(w.find('[data-testid="placement-mode"]').text()).toBe('Active-passive')
    const primary = w.find('[data-testid="member-default"]')
    expect(primary.text()).toContain('Primary')
    expect(primary.text()).toContain('In sync')
    const replica = w.find('[data-testid="member-fast"]')
    expect(replica.text()).toContain('Replica')
    expect(replica.text()).toContain('Lagging')
    expect(replica.text()).toContain('7')
    expect(replica.find('[data-testid="member-error"]').text()).toBe('copy photos/a.jpg: timeout')
    expect(w.find('[data-testid="placement-readonly"]').text()).toContain('admin:UpdateBucketPlacement')
    expect(w.find('[data-testid="replication-form"]').exists()).toBe(false)
    expect(w.find('button[aria-label="Actions for replica fast"]').exists()).toBe(false)
  })

  it('adds a replica with PUT placement and offers only backings without a copy as move targets', async () => {
    const { seen } = fakeServer({ admin: true, viewStorage: true, manageStorage: true })
    await connect()
    const { w, inner } = await mountInApp(PlacementTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="replication-form"]').exists()).toBe(true))
    await vi.waitFor(() => expect(w.find('[data-testid="replica-archive"]').exists()).toBe(true))
    // The primary is never offered as a replica; unhealthy backings are marked.
    expect(w.find('[data-testid="replica-default"]').exists()).toBe(false)
    expect(w.find('[data-testid="replica-broken"]').text()).toContain('Unhealthy')

    const save = () => w.findAll('button').find(b => b.text() === 'Save replication')!
    expect(save().attributes('disabled')).toBeDefined()
    await w.find('[data-testid="replica-archive"] button[role="checkbox"]').trigger('click')
    await nextTick()
    expect(w.text()).toContain('A full copy to archive starts right away')
    expect(save().attributes('disabled')).toBeUndefined()
    await w.find('[data-testid="replication-form"]').trigger('submit')
    await vi.waitFor(() => expect(seen.some(s => s.method === 'PUT')).toBe(true))
    expect(JSON.parse(seen.find(s => s.method === 'PUT')!.body)).toEqual({ mode: 'active-passive', replicas: ['archive', 'fast'] })
    await vi.waitFor(() => expect(w.find('[data-testid="member-archive"]').exists()).toBe(true))
    expect(inner.emitted('changed')).toHaveLength(1)

    // Switching to single clears the replicas (removing them asks first).
    const vm = inner.vm as unknown as { mode: string, replicas: string[], targets: { value: string }[] }
    vm.mode = 'single'
    await nextTick()
    expect(vm.replicas).toEqual([])
    expect(vm.targets.map(t => t.value)).toEqual(['broken'])
  })

  it('resyncs every replica and polls while a move is in progress', async () => {
    const moving = basePlacement()
    moving.members!.push({ backing: 'archive', role: 'move-target', state: 'syncing', healthy: true, pending: 0, progress: { keys: 120, versions: 130, bytes: 1048576 }, lastError: '' })
    moving.move = { from: 'default', to: 'archive', startedAt: '2026-10-12T10:00:00Z', phase: 'copying' }
    const { seen } = fakeServer({ admin: true, viewStorage: true, manageStorage: true }, moving)
    await connect()
    const { w, inner } = await mountInApp(PlacementTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="move-banner"]').exists()).toBe(true))
    const banner = w.find('[data-testid="move-banner"]')
    expect(banner.text()).toContain('Moving from default to archive')
    expect(banner.findAll('button').some(b => b.text() === 'Cancel move')).toBe(true)
    expect(w.find('[data-testid="member-archive"]').text()).toContain('Move target')
    expect(w.find('[data-testid="member-archive"]').text()).toContain('120 keys · 130 versions · 1 MiB')
    // Moving: no second move, and replication is frozen.
    expect(w.find('[data-testid="move-form"]').exists()).toBe(false)
    expect(w.text()).toContain('Replication cannot change while a move is in progress.')

    await w.findAll('button').find(b => b.text() === 'Resync all')!.trigger('click')
    await vi.waitFor(() => expect(seen.some(s => s.path.endsWith('/placement/resync'))).toBe(true))
    expect(seen.find(s => s.path.endsWith('/placement/resync'))!.body).toBe('{}')

    const gets = () => seen.filter(s => s.method === 'GET' && s.path.endsWith('/buckets/photos/placement')).length
    const before = gets()
    await vi.waitFor(() => expect(gets()).toBeGreaterThan(before), { timeout: 6000, interval: 100 })
    w.unmount()
  }, 15000)
})
