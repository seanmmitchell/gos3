// s3 backing account features against a fake gos3 admin API: the backing
// page, its remote bucket browser (statuses, uses, paging, probe, incomplete
// lists, actions, release), the containers list, the adopt / create /
// wrapped-bucket / add-container modals and the Danger zone of a native
// bucket.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, nextTick, type Component } from 'vue'
import { UApp } from '#components'
import type { AccountInfo, Backing, Capabilities, ContainerInfo, Info, RemoteBucket, WhoAmI } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import RemoteBucketsTable from '~/components/admin/RemoteBucketsTable.vue'
import BackingContainers from '~/components/admin/BackingContainers.vue'
import AdoptRemoteBucketModal from '~/components/admin/AdoptRemoteBucketModal.vue'
import CreateWrappedBucketModal from '~/components/admin/CreateWrappedBucketModal.vue'
import AddContainerModal from '~/components/admin/AddContainerModal.vue'
import DangerZoneTab from '~/components/bucket-settings/DangerZoneTab.vue'
import CreateBucketModal from '~/components/browser/CreateBucketModal.vue'
import BackingPage from '~/pages/servers/[id]/backings/[backing].vue'
import type { S3Client } from '~/utils/s3/client'

const INFO: Info = {
  service: 'gos3',
  id: 'b3f1c2d4e5f60718',
  name: 'nas',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.9.9.3:9000', admin: 'http://10.9.9.3:9000/_gos3/admin/v1' },
  features: [],
  startedAt: '2026-10-09T12:00:00Z'
}

const ALL_REMOTE: Partial<Capabilities> = { admin: true, viewStorage: true, browseRemote: true, createRemote: true, adoptRemote: true, manageContainers: true }

const CONTAINERS: ContainerInfo[] = [
  { id: 'home', label: 'shared', location: { bucket: 'acme-gos3', prefix: 'gos3/' }, region: 'eu-central-1', role: 'home', default: true, buckets: 4, state: 'ready', error: '' },
  { id: 'c-7f3a', label: 'archive', location: { bucket: 'acme-archive' }, role: 'dedicated', buckets: 0, state: 'failed', error: 'AccessDenied: no access' }
]

const ACCOUNT: AccountInfo = {
  home: { bucket: 'acme-gos3', prefix: 'gos3/' }, records: 'home', defaultKind: 'wrapped', defaultContainer: 'home',
  kinds: ['native', 'wrapped'], nativeBuckets: 1, wrappedBuckets: 4, containers: CONTAINERS
}

const CLOUD: Backing = {
  name: 'cloud', kind: 's3', path: '', location: 's3://acme-gos3/gos3/', kinds: ['native', 'wrapped'], defaultKind: 'wrapped', account: ACCOUNT,
  default: false, healthy: true, lastError: '', checkedAt: '2026-10-12T10:00:00Z', buckets: 5, replicas: 0, disk: null
}

const PAGE1: RemoteBucket[] = [
  { name: 'acme-gos3', region: 'eu-central-1', status: 'home', uses: [{ role: 'home', containerId: 'home', prefix: 'gos3/' }] },
  { name: 'acme-photos', region: 'eu-central-1', createdAt: '2026-01-01T00:00:00Z', status: 'native', uses: [{ role: 'native', bucket: 'photos', created: false }] },
  { name: 'legacy_bucket', status: 'unmanaged' },
  { name: 'Legacy_Bucket', status: 'unmanaged' },
  { name: 'other-team', status: 'claimed', uses: [{ role: 'claimed', backing: 'other' }] }
]
const PAGE2: RemoteBucket[] = [{ name: 'zz-locked', status: 'inaccessible', note: 'AccessDenied' }]

interface Seen { method: string, path: string, search: string, body: string }

interface FakeOptions {
  incomplete?: boolean
  /** GET remote-buckets answers 403. */
  listDenied?: boolean
  /** POST /backings/cloud/buckets fails after adding container c-kept. */
  keepContainer?: boolean
}

function fakeServer(caps: Partial<Capabilities>, opts: FakeOptions = {}) {
  const seen: Seen[] = []
  const whoami = (): WhoAmI => ({ user: 'alice', accessKey: 'AKALICE', isRoot: false, policies: [], capabilities: { ...NO_CAPABILITIES, ...caps }, server: INFO })
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input))
    const url = new URL(req.url)
    const body = req.method === 'GET' || req.method === 'DELETE' ? '' : await req.text()
    seen.push({ method: req.method, path: url.pathname, search: url.search, body })
    const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })
    if (url.host !== '10.9.9.3:9000') throw new TypeError('Failed to fetch')
    const p = url.pathname.replace('/_gos3/admin/v1', '')
    if (url.pathname === '/_gos3/info') return json(INFO)
    if (p === '/whoami') return json(whoami())
    if (p === '/backings') return json({ defaultBacking: 'default', autoFailover: false, backings: [{ ...CLOUD }, { name: 'default', kind: 'fs', path: '/data', default: true, healthy: true, lastError: '', buckets: 1, replicas: 0, disk: null }] })
    if (p === '/backings/cloud/remote-buckets' && req.method === 'GET') {
      if (opts.listDenied) return json({ error: { code: 'AccessDenied', message: 'admin:ListRemoteBuckets denied' } }, 403)
      if (opts.incomplete) return json({ buckets: [PAGE1[1]], complete: false, listError: 'AccessDenied: s3:ListAllMyBuckets' })
      const token = url.searchParams.get('continuationToken')
      return json(token === 'p2' ? { buckets: PAGE2, complete: true } : { buckets: PAGE1, complete: true, continuationToken: 'p2' })
    }
    if (p === '/backings/cloud/buckets' && req.method === 'POST') {
      const r = JSON.parse(body) as { bucket: string, kind: string, newContainer?: unknown }
      if (opts.keepContainer && r.newContainer) {
        return json({ error: { code: 'Conflict', message: 'the bucket exists (container "c-kept" was added and is kept; retry with "container": "c-kept")' } }, 409)
      }
      return json({ name: r.bucket, kind: r.kind }, 201)
    }
    if (p === '/backings/cloud/containers' && req.method === 'GET') {
      const kept = opts.keepContainer ? [{ id: 'c-kept', location: { bucket: 'acme-media', prefix: 'gos3/' }, role: 'existing', buckets: 0, state: 'ready' }] : []
      return json({ containers: [...CONTAINERS, ...kept] })
    }
    if (p === '/backings/cloud/containers' && req.method === 'POST') return json({ ...CONTAINERS[1], id: 'c-new', state: 'ready' }, 201)
    if (p === '/backings/cloud/containers/default' && req.method === 'PUT') return json({ containers: CONTAINERS })
    if (p.startsWith('/backings/cloud/containers/') && req.method === 'DELETE') return new Response(null, { status: 204 })
    if (p === '/buckets/photos/release' && req.method === 'POST') return new Response(null, { status: 204 })
    return new Response('', { status: 404 })
  }))
  return { seen }
}

async function connect() {
  const store = useConnectionsStore()
  const c = store.add({ id: 'nas', name: 'nas', endpoint: 'http://10.9.9.3:9000', accessKeyId: 'AKALICE', secretAccessKey: 'SK', kind: 'gos3' })
  store.setActive(c.id)
  await store.refreshStatus(c.id)
  return c
}

/** Mounts `comp` inside UApp (tooltips and overlays need its providers). */
async function mountInApp(comp: Component, props: Record<string, unknown>) {
  const Wrapper = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(comp, props) }) })
  const w = await mountSuspended(Wrapper)
  return { w, inner: w.findComponent(comp) }
}

const page = () => document.body.textContent ?? ''

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  useConnectionsStore().resetState()
})
afterEach(() => {
  vi.unstubAllGlobals()
})

describe('RemoteBucketsTable', () => {
  it('lists remote buckets with status, uses and links, page by page', async () => {
    const { seen } = fakeServer(ALL_REMOTE)
    await connect()
    const { w } = await mountInApp(RemoteBucketsTable, { backing: CLOUD, serverId: 'nas' })
    await vi.waitFor(() => expect(w.find('[data-testid="remote-acme-photos"]').exists()).toBe(true))
    expect(seen.find(s => s.path.endsWith('/remote-buckets'))!.search).toBe('?maxBuckets=100')
    const native = w.find('[data-testid="remote-acme-photos"]')
    expect(native.find('[data-testid="remote-status"]').text()).toBe('Native bucket')
    expect(native.find('a[href="/servers/nas/buckets/photos"]').text()).toBe('gos3 bucket photos (adopted)')
    expect(native.text()).toContain('eu-central-1')
    expect(w.find('[data-testid="remote-acme-gos3"]').text()).toContain('Home of this backing at gos3/')
    expect(w.find('[data-testid="remote-legacy_bucket"] [data-testid="remote-status"]').text()).toBe('Unmanaged')
    // A name gos3 does not accept: a note instead of actions.
    expect(w.find('[data-testid="remote-Legacy_Bucket"] [data-testid="remote-name-note"]').text()).toContain('gos3 cannot use this bucket')
    expect(w.find('button[aria-label="Actions for remote bucket Legacy_Bucket"]').exists()).toBe(false)
    expect(w.find('[data-testid="remote-other-team"]').text()).toContain('Used by backing other')
    // Toolbar actions with the capabilities.
    expect(w.findAll('button').some(b => b.text() === 'Create remote bucket…')).toBe(true)
    expect(w.findAll('button').some(b => b.text() === 'Adopt by name…')).toBe(true)
    // No actions for claimed buckets.
    expect(w.find('[data-testid="remote-other-team"] button[aria-label="Actions for remote bucket other-team"]').exists()).toBe(false)

    await w.findAll('button').find(b => b.text() === 'Next')!.trigger('click')
    await vi.waitFor(() => expect(w.find('[data-testid="remote-zz-locked"]').exists()).toBe(true))
    expect(seen.filter(s => s.path.endsWith('/remote-buckets')).at(-1)!.search).toBe('?continuationToken=p2&maxBuckets=100')
    expect(w.find('[data-testid="remote-zz-locked"]').text()).toContain('AccessDenied')
    expect(w.text()).toContain('Page 2')
    await w.findAll('button').find(b => b.text() === 'Previous')!.trigger('click')
    await vi.waitFor(() => expect(w.find('[data-testid="remote-acme-photos"]').exists()).toBe(true))
    w.unmount()
  })

  it('filters by prefix, probes for gos3 data and offers row actions by status', async () => {
    const { seen } = fakeServer(ALL_REMOTE)
    await connect()
    const { w, inner } = await mountInApp(RemoteBucketsTable, { backing: CLOUD, serverId: 'nas' })
    await vi.waitFor(() => expect(w.find('[data-testid="remote-acme-photos"]').exists()).toBe(true))
    await w.find('input[aria-label="Filter remote buckets by name prefix"]').setValue('acme-')
    await w.find('form[role="search"]').trigger('submit')
    await vi.waitFor(() => expect(seen.some(s => s.search === '?prefix=acme-&maxBuckets=100')).toBe(true))
    await w.find('[role="switch"]').trigger('click')
    await vi.waitFor(() => expect(seen.some(s => s.search === '?prefix=acme-&maxBuckets=100&probe=true')).toBe(true))

    const vm = inner.vm as unknown as { rowMenu: (rb: RemoteBucket) => { label: string, to?: string }[][], release: { run: (b: string) => Promise<unknown> } }
    expect(vm.rowMenu(PAGE1[1]!)[0]!.map(i => [i.label, i.to])).toEqual([['Open bucket', '/servers/nas/buckets/photos'], ['Release…', undefined]])
    expect(vm.rowMenu(PAGE1[2]!)[0]!.map(i => i.label)).toEqual(['Adopt as native bucket…', 'Use as container…', 'Create wrapped bucket in it…'])
    expect(vm.rowMenu(PAGE1[3]!)[0]!).toEqual([])
    expect(vm.rowMenu(PAGE1[0]!)[0]!.map(i => i.label)).toEqual(['Show containers'])
    await vm.release.run('photos')
    expect(seen.some(s => s.method === 'POST' && s.path.endsWith('/buckets/photos/release'))).toBe(true)
    w.unmount()
  })

  it('explains incomplete lists and offers the actions once the list loads, even without capabilities', async () => {
    // A grant scoped to this backing does not show in whoami's capabilities.
    fakeServer({ admin: true, viewStorage: true }, { incomplete: true })
    await connect()
    const { w, inner } = await mountInApp(RemoteBucketsTable, { backing: CLOUD, serverId: 'nas' })
    await vi.waitFor(() => expect(w.find('[data-testid="remote-incomplete"]').exists()).toBe(true))
    expect(w.find('[data-testid="remote-incomplete"]').text()).toContain('These credentials cannot list the account\'s buckets (AccessDenied: s3:ListAllMyBuckets)')
    expect(w.findAll('button').some(b => b.text() === 'Adopt by name…')).toBe(true)
    expect(w.findAll('button').some(b => b.text() === 'Create remote bucket…')).toBe(true)
    const vm = inner.vm as unknown as { rowMenu: (rb: RemoteBucket) => { label: string }[][] }
    expect(vm.rowMenu(PAGE1[1]!)[0]!.map(i => i.label)).toEqual(['Open bucket', 'Release…'])
    expect(vm.rowMenu(PAGE1[2]!)[0]!.map(i => i.label)).toEqual(['Adopt as native bucket…', 'Use as container…', 'Create wrapped bucket in it…'])
    w.unmount()
  })

  it('says when the identity may not list this backing\'s remote buckets', async () => {
    fakeServer({ admin: true, viewStorage: true }, { listDenied: true })
    await connect()
    const { w } = await mountInApp(RemoteBucketsTable, { backing: CLOUD, serverId: 'nas' })
    await vi.waitFor(() => expect(w.find('[data-testid="remote-denied"]').exists()).toBe(true))
    expect(w.find('[data-testid="remote-denied"]').text()).toContain('No access to this backing\'s remote buckets')
    expect(w.find('[data-testid="remote-denied"]').text()).toContain('admin:ListRemoteBuckets on arn:gos3:admin:::backing/cloud')
    expect(w.text()).not.toContain('Could not list the remote buckets')
    // Without the capabilities nothing is offered; adopting by name stays possible with adoptRemote.
    expect(w.findAll('button').some(b => b.text() === 'Adopt by name…')).toBe(false)
    w.unmount()
    fakeServer({ admin: true, viewStorage: true, adoptRemote: true }, { listDenied: true })
    await useConnectionsStore().refreshStatus('nas')
    const again = await mountInApp(RemoteBucketsTable, { backing: CLOUD, serverId: 'nas' })
    await vi.waitFor(() => expect(again.w.find('[data-testid="remote-denied"]').exists()).toBe(true))
    expect(again.w.find('[data-testid="remote-denied"]').text()).toContain('You can still adopt a remote bucket by name.')
    expect(again.w.findAll('button').some(b => b.text() === 'Adopt by name…')).toBe(true)
    again.w.unmount()
  })
})

describe('BackingContainers', () => {
  it('lists containers with state and errors; sets the default and removes empty ones', async () => {
    const { seen } = fakeServer(ALL_REMOTE)
    await connect()
    const { w, inner } = await mountInApp(BackingContainers, { backing: CLOUD })
    const home = w.find('[data-testid="container-home"]')
    expect(home.text()).toContain('shared')
    expect(home.text()).toContain('Default')
    expect(home.text()).toContain('Home')
    expect(home.text()).toContain('s3://acme-gos3/gos3/')
    expect(home.text()).toContain('Ready')
    const failed = w.find('[data-testid="container-c-7f3a"]')
    expect(failed.text()).toContain('Failed')
    expect(failed.find('[data-testid="container-error"]').text()).toBe('AccessDenied: no access')
    expect(w.findAll('button').some(b => b.text() === 'Add container…')).toBe(true)

    const vm = inner.vm as unknown as { rowMenu: (c: ContainerInfo) => { label: string, disabled?: boolean, description?: string }[][], setDefault: { run: (id: string) => Promise<unknown> }, remove: { run: (id: string) => Promise<unknown> } }
    expect(vm.rowMenu(CONTAINERS[0]!)[0]!.map(i => [i.label, i.disabled])).toEqual([['Make default', true], ['Remove…', true]])
    expect(vm.rowMenu(CONTAINERS[0]!)[0]![1]!.description).toMatch(/home container/)
    // The default container cannot be removed either.
    expect(vm.rowMenu({ ...CONTAINERS[1]!, default: true })[0]![1]).toMatchObject({ disabled: true, description: 'It is the default container; make another one the default first.' })
    // Failed containers cannot become the default; empty ones can be removed.
    expect(vm.rowMenu(CONTAINERS[1]!)[0]!.map(i => [i.label, i.disabled])).toEqual([['Make default', true], ['Remove…', false]])
    await vm.setDefault.run('home')
    await vm.remove.run('c-7f3a')
    expect(seen.filter(s => s.path.includes('/containers')).map(s => [s.method, s.path.replace('/_gos3/admin/v1', ''), s.body])).toEqual([
      ['PUT', '/backings/cloud/containers/default', '{"id":"home"}'],
      ['DELETE', '/backings/cloud/containers/c-7f3a', '']
    ])
    w.unmount()
  })

  it('explains a backing without containers', async () => {
    fakeServer({ admin: true, viewStorage: true })
    await connect()
    const { w } = await mountInApp(BackingContainers, { backing: { ...CLOUD, account: { ...ACCOUNT, home: null, records: 'local', containers: [] } } })
    expect(w.text()).toContain('cloud has no home and no container, so it cannot hold wrapped buckets (only native ones). An administrator can add one.')
    expect(w.findAll('button').some(b => b.text() === 'Add container…')).toBe(false)
    w.unmount()
  })
})

describe('remote bucket modals', () => {
  it('adopts a remote bucket under a valid gos3 name', async () => {
    const { seen } = fakeServer(ALL_REMOTE)
    await connect()
    const w = await mountSuspended(AdoptRemoteBucketModal, { props: { backing: 'cloud', remoteBucket: 'legacy_bucket', fixed: true }, attrs: { open: true } })
    await flushPromises()
    const remote = document.body.querySelector<HTMLInputElement>('input[aria-label="Remote bucket"]')!
    expect(remote.disabled).toBe(true)
    expect(remote.value).toBe('legacy_bucket')
    const name = document.body.querySelector<HTMLInputElement>('input[aria-label="gos3 bucket name"]')!
    expect(name.value).toBe('legacy-bucket')
    name.value = 'Bad Name'
    name.dispatchEvent(new Event('input'))
    await nextTick()
    const form = () => name.closest('form')!
    form().dispatchEvent(new Event('submit', { cancelable: true }))
    await flushPromises()
    expect(page()).toContain('Use only lowercase letters')
    expect(seen.some(s => s.method === 'POST')).toBe(false)
    name.value = 'legacy'
    name.dispatchEvent(new Event('input'))
    await nextTick()
    form().dispatchEvent(new Event('submit', { cancelable: true }))
    await vi.waitFor(() => expect(w.emitted('close')).toEqual([['legacy']]))
    w.unmount()
    const post = seen.find(s => s.method === 'POST')!
    expect(post.path).toBe('/_gos3/admin/v1/backings/cloud/buckets')
    expect(JSON.parse(post.body)).toEqual({ bucket: 'legacy', kind: 'native', remoteBucket: 'legacy_bucket', adopt: true })
  })

  it('creates a wrapped bucket in a new container inside a remote bucket', async () => {
    const { seen } = fakeServer(ALL_REMOTE)
    await connect()
    const w = await mountSuspended(CreateWrappedBucketModal, {
      props: { backing: 'cloud', containers: CONTAINERS, defaultContainer: 'home', remoteBucket: 'acme-media', region: 'eu-central-1' },
      attrs: { open: true }
    })
    await flushPromises()
    expect(page()).toContain('New container in acme-media')
    expect(document.body.querySelector<HTMLInputElement>('input[aria-label="Prefix"]')!.value).toBe('gos3/')
    const name = document.body.querySelector<HTMLInputElement>('input[aria-label="Bucket name"]')!
    name.value = 'media'
    name.dispatchEvent(new Event('input'))
    await nextTick()
    name.closest('form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await vi.waitFor(() => expect(w.emitted('close')).toEqual([['media']]))
    w.unmount()
    expect(JSON.parse(seen.find(s => s.method === 'POST')!.body)).toEqual({ bucket: 'media', kind: 'wrapped', newContainer: { bucket: 'acme-media', prefix: 'gos3/', region: 'eu-central-1' } })
  })

  it('offers the container a failed wrapped-bucket request kept, and retries in it', async () => {
    const { seen } = fakeServer(ALL_REMOTE, { keepContainer: true })
    await connect()
    const w = await mountSuspended(CreateWrappedBucketModal, {
      props: { backing: 'cloud', containers: CONTAINERS, defaultContainer: 'home', remoteBucket: 'acme-media' },
      attrs: { open: true }
    })
    await flushPromises()
    const name = document.body.querySelector<HTMLInputElement>('input[aria-label="Bucket name"]')!
    name.value = 'media'
    name.dispatchEvent(new Event('input'))
    await nextTick()
    name.closest('form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await vi.waitFor(() => expect(w.find('[data-testid="kept-container"]').exists() || !!document.body.querySelector('[data-testid="kept-container"]')).toBe(true))
    expect(w.emitted('changed')).toHaveLength(1)
    expect(w.emitted('close')).toBeUndefined()
    expect(page()).toContain('Container c-kept was added before the bucket failed')
    expect(seen.some(s => s.method === 'GET' && s.path.endsWith('/backings/cloud/containers'))).toBe(true)

    name.closest('form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await vi.waitFor(() => expect(w.emitted('close')).toEqual([['media']]))
    w.unmount()
    const posts = seen.filter(s => s.method === 'POST' && s.path.endsWith('/backings/cloud/buckets')).map(s => JSON.parse(s.body))
    expect(posts).toEqual([
      { bucket: 'media', kind: 'wrapped', newContainer: { bucket: 'acme-media', prefix: 'gos3/' } },
      { bucket: 'media', kind: 'wrapped', container: 'c-kept' }
    ])
  })

  it('adds a dedicated container in a new remote bucket', async () => {
    const { seen } = fakeServer(ALL_REMOTE)
    await connect()
    const w = await mountSuspended(AddContainerModal, {
      props: { backing: 'cloud', access: { browse: true, create: true, adopt: true, containers: true, release: true } },
      attrs: { open: true }
    })
    await flushPromises()
    const bucket = document.body.querySelector<HTMLInputElement>('input[aria-label="Remote bucket"]')!
    bucket.value = 'acme-gos3-archive'
    bucket.dispatchEvent(new Event('input'))
    const label = document.body.querySelector<HTMLInputElement>('input[aria-label="Label"]')!
    label.value = 'archive'
    label.dispatchEvent(new Event('input'))
    await nextTick()
    bucket.closest('form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await vi.waitFor(() => expect(w.emitted('close')).toEqual([['c-new']]))
    w.unmount()
    expect(JSON.parse(seen.find(s => s.method === 'POST')!.body)).toEqual({ bucket: 'acme-gos3-archive', createBucket: true, label: 'archive' })
  })
})

describe('DangerZoneTab of a native bucket', () => {
  it('offers release and warns that force delete deletes the remote objects', async () => {
    const { seen } = fakeServer({ admin: true, manageBuckets: true })
    await connect()
    const w = await mountSuspended(DangerZoneTab, { props: { bucket: 'photos', kind: 'native', location: 's3://acme-photos/' } })
    expect(w.find('[data-testid="release-bucket"]').text()).toContain('the remote bucket s3://acme-photos/ and its objects are not touched')
    expect(w.find('[data-testid="force-delete-native"]').text()).toContain('its objects are deleted in the remote bucket s3://acme-photos/')
    expect(w.text()).toContain('an adopted one is kept')
    const vm = w.vm as unknown as { release: { run: () => Promise<unknown> } }
    await vm.release.run()
    expect(seen.some(s => s.method === 'POST' && s.path === '/_gos3/admin/v1/buckets/photos/release')).toBe(true)
    w.unmount()
  })

  it('has no release for wrapped buckets', async () => {
    fakeServer({ admin: true, manageBuckets: true })
    await connect()
    const w = await mountSuspended(DangerZoneTab, { props: { bucket: 'docs', kind: 'wrapped' } })
    expect(w.find('[data-testid="release-bucket"]').exists()).toBe(false)
    expect(w.find('[data-testid="force-delete-native"]').exists()).toBe(false)
    w.unmount()
  })
})

describe('backing page', () => {
  it('shows the account overview and the sections the identity may use', async () => {
    fakeServer(ALL_REMOTE)
    await connect()
    const w = await mountSuspended(BackingPage, { route: '/servers/nas/backings/cloud' })
    await vi.waitFor(() => expect(w.find('[data-testid="backing-account"]').exists()).toBe(true))
    const account = w.find('[data-testid="backing-account"]').text()
    expect(account).toContain('s3://acme-gos3/gos3/')
    expect(account).toContain('Wrapped')
    expect(account).toContain('shared')
    expect(w.text()).toContain('1 native · 4 wrapped')
    expect(w.findAll('[role="tab"]').map(t => t.text())).toEqual(['Overview', 'Remote buckets', 'Containers'])
    w.unmount()
  })

  it('shows the remote buckets without browseRemote (grants may be scoped to the backing) and explains other backings', async () => {
    fakeServer({ admin: true, viewStorage: true })
    await connect()
    const w = await mountSuspended(BackingPage, { route: '/servers/nas/backings/cloud' })
    await vi.waitFor(() => expect(w.find('[data-testid="backing-account"]').exists()).toBe(true))
    expect(w.findAll('[role="tab"]').map(t => t.text())).toEqual(['Overview', 'Remote buckets', 'Containers'])
    w.unmount()
    const fs = await mountSuspended(BackingPage, { route: '/servers/nas/backings/default' })
    await vi.waitFor(() => expect(fs.text()).toContain('Nothing to manage here'))
    fs.unmount()
  })

  it('opens the prefilled adopt dialog from the create-bucket link on any tab', async () => {
    const { seen } = fakeServer({ admin: true, viewStorage: true, adoptRemote: true })
    await connect()
    const Wrapper = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(BackingPage) }) })
    const w = await mountSuspended(Wrapper, { route: '/servers/nas/backings/cloud?adopt=acme-photos2&bucket=photos2' })
    await vi.waitFor(() => expect(document.body.querySelector('input[aria-label="gos3 bucket name"]')).not.toBeNull())
    const remote = document.body.querySelector<HTMLInputElement>('input[aria-label="Remote bucket"]')!
    expect(remote.value).toBe('acme-photos2')
    expect(remote.disabled).toBe(false)
    expect(document.body.querySelector<HTMLInputElement>('input[aria-label="gos3 bucket name"]')!.value).toBe('photos2')
    // The overview stays the active tab; the remote buckets were not listed.
    expect(seen.some(s => s.path.endsWith('/remote-buckets'))).toBe(false)
    remote.closest('form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    await vi.waitFor(() => expect(seen.some(s => s.method === 'POST' && s.path.endsWith('/backings/cloud/buckets'))).toBe(true))
    expect(JSON.parse(seen.find(s => s.method === 'POST')!.body)).toEqual({ bucket: 'photos2', kind: 'native', remoteBucket: 'acme-photos2', adopt: true })
    // Adopting reloads the backing and whoami.
    await vi.waitFor(() => expect(seen.filter(s => s.path.endsWith('/whoami')).length).toBeGreaterThan(1))
    w.unmount()
  })
})

describe('CreateBucketModal on a connected server', () => {
  it('refreshes a whoami older than a few seconds when it opens', async () => {
    const { seen } = fakeServer({ admin: true, viewStorage: true })
    await connect()
    const whoamis = () => seen.filter(s => s.path.endsWith('/whoami')).length
    const before = whoamis()
    const client = { createBucket: async () => {} } as unknown as S3Client
    const backings = { default: 'cloud', available: [{ name: 'cloud', kind: 's3', kinds: ['wrapped'], defaultKind: 'wrapped', containers: [{ id: 'home', default: true }] }] }
    // Fresh (just checked): not fetched again.
    let w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings } as never })
    await flushPromises()
    expect(whoamis()).toBe(before)
    w.unmount()
    // Older than a few seconds: refreshed.
    const store = useConnectionsStore()
    store.statusOf('nas').checkedAt = Date.now() - 10000
    w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings } as never })
    await vi.waitFor(() => expect(whoamis()).toBe(before + 1))
    w.unmount()
  })
})
