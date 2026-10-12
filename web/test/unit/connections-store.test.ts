import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CORS_BLOCKED, LEGACY_DEFAULT_SERVER_KEY, LOCAL_STORAGE_KEY, NO_CAPABILITIES, SESSION_STORAGE_KEY, useConnectionsStore } from '~/stores/connections'
import { EMBEDDED_CONNECTION_ID } from '~/utils/connections'
import { clearClientCache } from '~/utils/clients'
import type { Info } from '~/types/admin'

const INFO: Info = {
  service: 'gos3',
  id: 'abcdef0123456789',
  name: 'nas',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'eu-west-1',
  endpoints: { s3: 'http://nas:9000', admin: 'http://nas:9000/_gos3/admin/v1' },
  features: [],
  startedAt: '2026-10-09T12:00:00Z'
}

function local() {
  return JSON.parse(localStorage.getItem(LOCAL_STORAGE_KEY) ?? 'null')
}
function session() {
  return JSON.parse(sessionStorage.getItem(SESSION_STORAGE_KEY) ?? 'null')
}

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  setActivePinia(createPinia())
})

describe('connections store: CRUD & persistence', () => {
  it('adds connections with defaults and normalised endpoints', () => {
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'localhost:9000/', accessKeyId: 'AK', secretAccessKey: 'SK' })
    expect(c).toMatchObject({ endpoint: 'http://localhost:9000', name: 'localhost:9000', region: 'us-east-1', kind: 'gos3', forcePathStyle: true, remember: false, source: 'user' })
    expect(c.id).toMatch(/^[a-z0-9]{8}$/)
    expect(s.byId(c.id)).toEqual(c)
    expect(s.findByEndpoint('http://LOCALHOST:9000')).toEqual(c)
  })

  it('never writes the secret to localStorage when remember = false', () => {
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'http://a:9000', accessKeyId: 'AK', secretAccessKey: 'TOPSECRET', remember: false })
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).not.toContain('TOPSECRET')
    expect(local().connections[0].accessKeyId).toBe('AK')
    expect(session()[c.id].secretAccessKey).toBe('TOPSECRET')
  })

  it('stores the secret in localStorage when remember = true', () => {
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'http://a:9000', accessKeyId: 'AK', secretAccessKey: 'KEEP', remember: true })
    expect(local().connections[0].secretAccessKey).toBe('KEEP')
    expect(session()[c.id]).toBeUndefined()
    // switching remember off moves it to the session store
    s.update(c.id, { remember: false })
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).not.toContain('KEEP')
    expect(session()[c.id].secretAccessKey).toBe('KEEP')
  })

  it('hydrates from storage (secrets from session for remember=false)', () => {
    const s1 = useConnectionsStore()
    const a = s1.add({ endpoint: 'http://a:9000', accessKeyId: 'AK', secretAccessKey: 'S1', remember: false })
    const b = s1.add({ endpoint: 'http://b:9000', accessKeyId: 'BK', secretAccessKey: 'S2', remember: true })
    s1.setActive(b.id)

    setActivePinia(createPinia())
    const s2 = useConnectionsStore()
    s2.hydrate()
    expect(s2.connections).toHaveLength(2)
    expect(s2.byId(a.id)?.secretAccessKey).toBe('S1')
    expect(s2.byId(b.id)?.secretAccessKey).toBe('S2')
    expect(s2.activeId).toBe(b.id)

    // new browser session: session storage gone
    sessionStorage.clear()
    setActivePinia(createPinia())
    const s3 = useConnectionsStore()
    s3.hydrate()
    expect(s3.byId(a.id)?.secretAccessKey).toBe('')
    expect(s3.hasCredentials(s3.byId(a.id))).toBe(false)
    expect(s3.byId(b.id)?.secretAccessKey).toBe('S2')
  })

  it('clearCredentials forgets the secret and the active selection', () => {
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'http://a:9000', accessKeyId: 'AK', secretAccessKey: 'S', remember: true })
    s.setActive(c.id)
    s.clearCredentials(c.id)
    expect(s.byId(c.id)?.secretAccessKey).toBe('')
    expect(s.byId(c.id)?.accessKeyId).toBe('AK')
    expect(s.activeId).toBeNull()
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).not.toContain('"S"')
  })

  it('removes connections and clears active', () => {
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'http://a:9000' })
    s.setActive(c.id)
    s.remove(c.id)
    expect(s.connections).toHaveLength(0)
    expect(s.activeId).toBeNull()
    expect(local().connections).toEqual([])
  })

  it('reports no capabilities for generic S3 or unknown status', () => {
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'http://a:9000', kind: 's3' })
    expect(s.capabilitiesOf(c.id)).toEqual(NO_CAPABILITIES)
    expect(s.statusOf(c.id).state).toBe('unknown')
  })
})

describe('connections store: default server', () => {
  it('persists the default, orders it first and ignores unknown ids', () => {
    const s = useConnectionsStore()
    const b = s.add({ name: 'B', endpoint: 'http://b:9000' })
    const a = s.add({ name: 'A', endpoint: 'http://a:9000' })
    expect(s.ordered.map(c => c.name)).toEqual(['A', 'B'])
    s.setDefault(b.id)
    expect(s.defaultConnection?.id).toBe(b.id)
    expect(s.ordered.map(c => c.name)).toEqual(['B', 'A'])
    expect(local().defaultId).toBe(b.id)
    s.setDefault('nope')
    expect(s.defaultId).toBe(b.id)

    setActivePinia(createPinia())
    const s2 = useConnectionsStore()
    s2.hydrate()
    expect(s2.defaultId).toBe(b.id)
    s2.remove(b.id)
    expect(s2.defaultId).toBeNull()
    expect(s2.ordered.map(c => c.id)).toEqual([a.id])
  })

  it('migrates the former separate localStorage key', () => {
    const s = useConnectionsStore()
    const a = s.add({ name: 'A', endpoint: 'http://a:9000' })
    const state = local()
    delete state.defaultId
    localStorage.setItem(LOCAL_STORAGE_KEY, JSON.stringify(state))
    localStorage.setItem(LEGACY_DEFAULT_SERVER_KEY, a.id)

    setActivePinia(createPinia())
    const s2 = useConnectionsStore()
    s2.hydrate()
    expect(s2.defaultId).toBe(a.id)
    expect(localStorage.getItem(LEGACY_DEFAULT_SERVER_KEY)).toBeNull()
    expect(local().defaultId).toBe(a.id)
  })

  it('keeps a default that names a preset merged in by init()', async () => {
    localStorage.setItem(LEGACY_DEFAULT_SERVER_KEY, 'nas')
    const s = useConnectionsStore()
    await s.init({ presets: [{ id: 'nas', name: 'NAS', endpoint: 'http://nas:9000' }], skipServersJson: true, fetch: async () => new Response('', { status: 404 }) })
    expect(s.defaultConnection?.name).toBe('NAS')
  })
})

describe('connections store: import / export', () => {
  it('exports without secrets by default and with secrets on request', () => {
    const s = useConnectionsStore()
    s.add({ endpoint: 'http://a:9000', name: 'A', accessKeyId: 'AK', secretAccessKey: 'SK' })
    const noSecrets = JSON.parse(s.exportJson())
    expect(noSecrets.type).toBe('gos3-connections')
    expect(noSecrets.connections[0].secretAccessKey).toBeUndefined()
    expect(noSecrets.connections[0].accessKeyId).toBeUndefined()
    const withSecrets = JSON.parse(s.exportJson({ includeSecrets: true }))
    expect(withSecrets.connections[0]).toMatchObject({ name: 'A', accessKeyId: 'AK', secretAccessKey: 'SK' })
  })

  it('imports export files, arrays and aliases; updates matches', () => {
    const s = useConnectionsStore()
    const existing = s.add({ endpoint: 'http://a:9000', name: 'Old', accessKeyId: 'AK' })
    const r = s.importJson(JSON.stringify({
      type: 'gos3-connections',
      version: 1,
      connections: [
        { endpoint: 'http://a:9000/', name: 'Renamed', accessKeyId: 'AK', secretAccessKey: 'NEW' },
        { url: 'https://s3.example.com', accessKey: 'X', secretKey: 'Y', kind: 's3', pathStyle: false },
        { name: 'no endpoint' }
      ]
    }))
    expect(r).toEqual({ added: 1, updated: 1, skipped: 0 })
    expect(s.byId(existing.id)).toMatchObject({ name: 'Renamed', secretAccessKey: 'NEW' })
    const added = s.findByEndpoint('https://s3.example.com')!
    expect(added).toMatchObject({ accessKeyId: 'X', secretAccessKey: 'Y', kind: 's3', forcePathStyle: false, source: 'user' })
    expect(s.importJson('[{"endpoint":"http://c:1"}]').added).toBe(1)
    expect(() => s.importJson('{not json')).toThrow(SyntaxError)
  })

  it('round-trips export → import into an empty store', () => {
    const s = useConnectionsStore()
    s.add({ endpoint: 'http://a:9000', name: 'A', accessKeyId: 'AK', secretAccessKey: 'SK', color: 'sky' })
    const dump = s.exportJson({ includeSecrets: true })
    localStorage.clear()
    setActivePinia(createPinia())
    const s2 = useConnectionsStore()
    s2.importJson(dump)
    expect(s2.connections[0]).toMatchObject({ name: 'A', endpoint: 'http://a:9000', accessKeyId: 'AK', secretAccessKey: 'SK', color: 'sky' })
  })
})

describe('connections store: import never redirects stored credentials', () => {
  const noFetch = () => Promise.reject(new TypeError('offline'))
  const importOf = (...connections: unknown[]) => JSON.stringify({ type: 'gos3-connections', version: 1, connections })

  it('adds an entry that would move a connection to another endpoint as a separate server', () => {
    const s = useConnectionsStore()
    const nas = s.add({ id: 'nas', name: 'NAS', endpoint: 'http://10.0.0.5:9000', accessKeyId: 'AK', secretAccessKey: 'SECRET', color: 'sky' })
    const plan = s.planImport(importOf({ id: 'nas', endpoint: 'http://attacker:9000' }))
    expect(plan).toMatchObject({ added: 1, updated: 0, skipped: 0 })
    expect(plan.steps[0]).toMatchObject({ action: 'add', instead: { of: { id: 'nas' }, reason: 'retarget' } })

    expect(s.importJson(importOf({ id: 'nas', endpoint: 'http://attacker:9000' }))).toEqual({ added: 1, updated: 0, skipped: 0 })
    expect(s.byId('nas')).toEqual(nas)
    const added = s.findByEndpoint('http://attacker:9000')!
    expect(added.id).not.toBe('nas')
    expect(added).toMatchObject({ accessKeyId: '', secretAccessKey: '', source: 'user' })
    expect(s.hasCredentials(added)).toBe(false)
  })

  it('treats a kind change like an endpoint change', () => {
    const s = useConnectionsStore()
    s.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas:9000', kind: 'gos3', accessKeyId: 'AK', secretAccessKey: 'SK' })
    // matched by id
    expect(s.importJson(importOf({ id: 'nas', endpoint: 'http://nas:9000', kind: 's3' }))).toMatchObject({ added: 1, updated: 0 })
    // matched by endpoint (no access key in the file)
    expect(s.importJson(importOf({ endpoint: 'http://nas:9000', kind: 's3', name: 'other' }))).toMatchObject({ added: 0, updated: 1 })
    expect(s.byId('nas')).toMatchObject({ kind: 'gos3', accessKeyId: 'AK', secretAccessKey: 'SK', name: 'NAS' })
    expect(s.connections.filter(c => c.kind === 's3')).toHaveLength(1)
    expect(s.connections.find(c => c.kind === 's3')).toMatchObject({ name: 'other', secretAccessKey: '' })
  })

  it('keeps the file\'s own credentials on a connection added instead of a retarget', () => {
    const s = useConnectionsStore()
    s.add({ id: 'nas', endpoint: 'http://nas:9000', accessKeyId: 'AK', secretAccessKey: 'SK' })
    s.importJson(importOf({ id: 'nas', endpoint: 'http://nas2:9000', accessKeyId: 'AK2', secretAccessKey: 'SK2' }))
    expect(s.findByEndpoint('http://nas2:9000')).toMatchObject({ accessKeyId: 'AK2', secretAccessKey: 'SK2' })
    expect(s.byId('nas')).toMatchObject({ endpoint: 'http://nas:9000', secretAccessKey: 'SK' })
  })

  it('never changes the embedded connection', async () => {
    const fetch = vi.fn(async (req: Request) => req.url === 'http://nas:9000/_gos3/info'
      ? new Response(JSON.stringify(INFO), { headers: { 'content-type': 'application/json' } })
      : new Response('', { status: 404 }))
    const s = useConnectionsStore()
    await s.init({ baseURL: '/_gos3/ui/', origin: 'http://nas:9000', fetch })
    s.setCredentials(EMBEDDED_CONNECTION_ID, { accessKeyId: 'AK', secretAccessKey: 'SK' })
    const before = { ...s.byId(EMBEDDED_CONNECTION_ID)! }

    // same endpoint: skipped, even with other credentials or names
    const same = importOf({ id: EMBEDDED_CONNECTION_ID, endpoint: 'http://nas:9000', name: 'Renamed', accessKeyId: 'X', secretAccessKey: 'Y', region: 'ap-south-1' })
    expect(s.planImport(same).steps[0]).toMatchObject({ action: 'skip', reason: 'protected' })
    expect(s.importJson(same)).toEqual({ added: 0, updated: 0, skipped: 1 })
    // matched by endpoint + access key
    expect(s.importJson(importOf({ endpoint: 'http://NAS:9000/', accessKeyId: 'AK', name: 'Renamed' }))).toEqual({ added: 0, updated: 0, skipped: 1 })
    expect(s.byId(EMBEDDED_CONNECTION_ID)).toEqual(before)

    // another endpoint: added as a new user connection with a fresh id
    const other = importOf({ id: EMBEDDED_CONNECTION_ID, source: 'embedded', endpoint: 'http://elsewhere:9000' })
    expect(s.planImport(other).steps[0]).toMatchObject({ action: 'add', instead: { reason: 'protected' } })
    expect(s.importJson(other)).toMatchObject({ added: 1 })
    expect(s.byId(EMBEDDED_CONNECTION_ID)).toEqual(before)
    const added = s.findByEndpoint('http://elsewhere:9000')!
    expect(added).toMatchObject({ source: 'user', secretAccessKey: '' })
    expect(added.id).not.toBe(EMBEDDED_CONNECTION_ID)
  })

  it('never changes preset connections', async () => {
    const s = useConnectionsStore()
    await s.init({ presets: [{ endpoint: 'http://10.0.0.5:9000', name: 'NAS' }], skipServersJson: true, fetch: noFetch })
    s.setCredentials('preset-10-0-0-5-9000', { accessKeyId: 'AK', secretAccessKey: 'SK' })
    const before = { ...s.byId('preset-10-0-0-5-9000')! }
    expect(s.importJson(importOf({ id: 'preset-10-0-0-5-9000', endpoint: 'http://attacker:9000', remember: true }))).toMatchObject({ added: 1, updated: 0 })
    expect(s.importJson(importOf({ endpoint: 'http://10.0.0.5:9000', name: 'Mine', forcePathStyle: false }))).toEqual({ added: 0, updated: 0, skipped: 1 })
    expect(s.byId('preset-10-0-0-5-9000')).toEqual(before)
    expect(s.findByEndpoint('http://attacker:9000')).toMatchObject({ source: 'user', remember: false, secretAccessKey: '' })
  })

  it('does not let new entries claim reserved ids', () => {
    const s = useConnectionsStore()
    s.importJson(importOf({ id: EMBEDDED_CONNECTION_ID, endpoint: 'http://a:1' }, { id: 'preset-b-2', endpoint: 'http://b:2' }, { id: 'mine', endpoint: 'http://c:3' }))
    expect(s.byId(EMBEDDED_CONNECTION_ID)).toBeUndefined()
    expect(s.byId('preset-b-2')).toBeUndefined()
    expect(s.byId('mine')?.endpoint).toBe('http://c:3')
    expect(s.connections).toHaveLength(3)
  })

  it('keeps a secret replaced by an import out of localStorage', () => {
    const s = useConnectionsStore()
    s.add({ id: 'nas', endpoint: 'http://nas:9000', accessKeyId: 'AK', secretAccessKey: 'MINE', remember: true })
    const json = importOf({ id: 'nas', endpoint: 'http://nas:9000', secretAccessKey: 'IMPORTED', remember: true })
    const step = s.planImport(json).steps[0]!
    expect(step.action).toBe('update')
    if (step.action !== 'update') return
    expect(step.changes).toContainEqual({ field: 'remember', from: 'on', to: 'off' })
    s.importJson(json)
    expect(s.byId('nas')).toMatchObject({ secretAccessKey: 'IMPORTED', remember: false })
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).not.toContain('IMPORTED')
    expect(session()).toEqual({ nas: { secretAccessKey: 'IMPORTED' } })
    // An import that leaves the secret alone keeps "remember".
    s.update('nas', { remember: true })
    s.importJson(importOf({ id: 'nas', endpoint: 'http://nas:9000', name: 'Renamed' }))
    expect(s.byId('nas')).toMatchObject({ name: 'Renamed', remember: true, secretAccessKey: 'IMPORTED' })
  })

  it('updates fields of a match on the same endpoint and describes the changes without secrets', () => {
    const s = useConnectionsStore()
    s.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas:9000', region: 'us-east-1', accessKeyId: 'AK', secretAccessKey: 'OLDSECRET' })
    const json = importOf({ id: 'nas', endpoint: 'http://NAS:9000/', name: 'NAS 2', region: 'eu-west-1', secretAccessKey: 'NEWSECRET', forcePathStyle: false })
    const step = s.planImport(json).steps[0]!
    expect(step.action).toBe('update')
    if (step.action !== 'update') return
    expect(step.changes).toEqual([
      { field: 'name', from: 'NAS', to: 'NAS 2' },
      { field: 'region', from: 'us-east-1', to: 'eu-west-1' },
      { field: 'secretAccessKey', from: 'set', to: 'set' },
      { field: 'forcePathStyle', from: 'true', to: 'false' }
    ])
    expect(JSON.stringify(step.changes)).not.toContain('SECRET')
    expect(s.importJson(json)).toEqual({ added: 0, updated: 1, skipped: 0 })
    expect(s.byId('nas')).toMatchObject({ name: 'NAS 2', endpoint: 'http://nas:9000', region: 'eu-west-1', secretAccessKey: 'NEWSECRET', forcePathStyle: false, accessKeyId: 'AK' })
  })

  it('clears the stored secret when the access key changes without a new secret', () => {
    const s = useConnectionsStore()
    s.add({ id: 'nas', endpoint: 'http://nas:9000', accessKeyId: 'AK', secretAccessKey: 'SK' })
    const plan = s.planImport(importOf({ id: 'nas', endpoint: 'http://nas:9000', accessKeyId: 'AK2' }))
    expect(plan.steps[0]).toMatchObject({ action: 'update', changes: [{ field: 'accessKeyId', from: 'AK', to: 'AK2' }, { field: 'secretAccessKey', from: 'set', to: 'none' }] })
    s.importJson(importOf({ id: 'nas', endpoint: 'http://nas:9000', accessKeyId: 'AK2' }))
    expect(s.byId('nas')).toMatchObject({ accessKeyId: 'AK2', secretAccessKey: '' })
  })

  it('ignores remember from imported data', () => {
    const s = useConnectionsStore()
    s.add({ id: 'nas', endpoint: 'http://nas:9000', accessKeyId: 'AK', secretAccessKey: 'SESSIONONLY' })
    expect(s.importJson(importOf({ id: 'nas', endpoint: 'http://nas:9000', remember: true }))).toEqual({ added: 0, updated: 0, skipped: 1 })
    s.importJson(importOf({ endpoint: 'http://new:9000', accessKeyId: 'X', secretAccessKey: 'FROMFILE', remember: true }))
    expect(s.byId('nas')?.remember).toBe(false)
    expect(s.findByEndpoint('http://new:9000')?.remember).toBe(false)
    const stored = localStorage.getItem(LOCAL_STORAGE_KEY)!
    expect(stored).not.toContain('SESSIONONLY')
    expect(stored).not.toContain('FROMFILE')
  })

  it('skips entries that change nothing and plans entries in order', () => {
    const s = useConnectionsStore()
    s.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas:9000' })
    const plan = s.planImport(importOf(
      { id: 'nas', name: 'NAS', endpoint: 'http://nas:9000' },
      { id: 'x', name: 'X', endpoint: 'http://x:1', source: 'discovered' },
      { id: 'x', name: 'X 2', endpoint: 'http://x:1' }
    ))
    expect(plan.steps.map(st => st.action)).toEqual(['skip', 'add', 'update'])
    expect(plan.steps[0]).toMatchObject({ reason: 'unchanged' })
    expect(s.connections).toHaveLength(1) // planning changes nothing
    expect(s.importJson(importOf(
      { id: 'x', name: 'X', endpoint: 'http://x:1', source: 'discovered' },
      { id: 'x', name: 'X 2', endpoint: 'http://x:1' }
    ))).toEqual({ added: 1, updated: 1, skipped: 0 })
    expect(s.byId('x')).toMatchObject({ name: 'X 2', source: 'discovered' })
  })
})

describe('connections store: presets & embedded', () => {
  const noFetch = () => Promise.reject(new TypeError('offline'))

  it('loads presets from runtime config JSON and keeps user credentials', async () => {
    const s = useConnectionsStore()
    await s.init({ presets: JSON.stringify([{ id: 'nas', name: 'NAS', endpoint: 'http://nas:9000', region: 'eu-west-1' }]), skipServersJson: true, fetch: noFetch })
    expect(s.byId('nas')).toMatchObject({ name: 'NAS', source: 'preset', region: 'eu-west-1', remember: false })
    s.setCredentials('nas', { accessKeyId: 'AK', secretAccessKey: 'SK' })

    // restart with a changed preset (same endpoint): the user's keys stay
    setActivePinia(createPinia())
    const s2 = useConnectionsStore()
    await s2.init({ presets: [{ id: 'nas', name: 'NAS 2', endpoint: 'http://NAS:9000/', region: 'eu-west-2' }], skipServersJson: true, fetch: noFetch })
    expect(s2.byId('nas')).toMatchObject({ name: 'NAS 2', region: 'eu-west-2', accessKeyId: 'AK', secretAccessKey: 'SK' })
  })

  it('never signs a user-entered secret for a preset endpoint that changed', async () => {
    const s = useConnectionsStore()
    await s.init({ presets: [{ id: 'nas', name: 'NAS', endpoint: 'http://nas:9000' }], skipServersJson: true, fetch: noFetch })
    s.setCredentials('nas', { accessKeyId: 'AK', secretAccessKey: 'SK', remember: true })

    setActivePinia(createPinia())
    const s2 = useConnectionsStore()
    await s2.init({ presets: [{ id: 'nas', name: 'NAS', endpoint: 'http://evil:9000' }], skipServersJson: true, fetch: noFetch })
    const c = s2.byId('nas')!
    expect(c.endpoint).toBe('http://evil:9000')
    expect(c.secretAccessKey).toBe('')
    expect(c.accessKeyId).toBe('AK')
    expect(s2.hasCredentials(c)).toBe(false)
    // ...and not in storage either (it was remembered).
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).not.toContain('"SK"')
  })

  it('ignores secret keys in presets (they are public) with a warning', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const s = useConnectionsStore()
    await s.init({ presets: [{ id: 'nas', endpoint: 'http://nas:9000', accessKeyId: 'AK', secretAccessKey: 'PUBLIC-SECRET' }], skipServersJson: true, fetch: noFetch })
    expect(s.byId('nas')).toMatchObject({ accessKeyId: 'AK', secretAccessKey: '' })
    expect(warn).toHaveBeenCalledWith(expect.stringMatching(/preset "nas" contains a secret key/))
    expect(JSON.stringify(warn.mock.calls)).not.toContain('PUBLIC-SECRET')
  })

  it('derives preset ids from endpoints and remembers dismissed presets', async () => {
    const s = useConnectionsStore()
    await s.init({ presets: [{ endpoint: 'http://nas:9000' }], skipServersJson: true, fetch: noFetch })
    const id = s.connections[0]!.id
    expect(id).toBe('preset-nas-9000')
    s.remove(id)
    setActivePinia(createPinia())
    const s2 = useConnectionsStore()
    await s2.init({ presets: [{ endpoint: 'http://nas:9000' }], skipServersJson: true, fetch: noFetch })
    expect(s2.connections).toHaveLength(0)
  })

  it('loads servers.json and ignores SPA HTML fallbacks / missing files', async () => {
    const make = (res: Response) => vi.fn(async (req: Request) => {
      expect(req.url).toBe('http://localhost/_gos3/ui/servers.json')
      return res
    })
    let s = useConnectionsStore()
    await s.init({ baseURL: '/_gos3/ui/', embedded: false, fetch: make(new Response(JSON.stringify({ servers: [{ endpoint: 'http://x:9000', name: 'X' }] }), { headers: { 'content-type': 'application/json' } })) })
    expect(s.connections.map(c => c.name)).toEqual(['X'])

    for (const res of [new Response('<!doctype html><html></html>', { headers: { 'content-type': 'text/html' } }), new Response('nope', { status: 404 })]) {
      localStorage.clear()
      setActivePinia(createPinia())
      s = useConnectionsStore()
      await s.init({ baseURL: '/_gos3/ui/', embedded: false, fetch: make(res) })
      expect(s.connections).toHaveLength(0)
    }
  })

  it('detects the embedding gos3 server', async () => {
    const fetch = vi.fn(async (req: Request) => {
      if (req.url === 'http://nas:9000/_gos3/info') return new Response(JSON.stringify(INFO), { headers: { 'content-type': 'application/json' } })
      return new Response('', { status: 404 })
    })
    const s = useConnectionsStore()
    await s.init({ baseURL: '/_gos3/ui/', origin: 'http://nas:9000', fetch })
    const c = s.byId(EMBEDDED_CONNECTION_ID)!
    expect(c).toMatchObject({ endpoint: 'http://nas:9000', region: 'eu-west-1', kind: 'gos3', source: 'embedded', serverId: INFO.id, name: 'nas (this server)' })
    expect(s.hasCredentials(c)).toBe(false)
    expect(s.activeId).toBe(EMBEDDED_CONNECTION_ID)
    expect(s.embeddedInfo?.id).toBe(INFO.id)
    expect(s.statusOf(c.id).info?.id).toBe(INFO.id)
  })

  it('does not add an embedded connection for non-gos3 origins', async () => {
    const s = useConnectionsStore()
    await s.init({ embedded: 'true', origin: 'http://web:3000', fetch: async () => new Response('<html>', { status: 200 }) })
    expect(s.connections).toHaveLength(0)
  })

  it('init is idempotent and ready() resolves', async () => {
    const s = useConnectionsStore()
    const fetch = vi.fn(async () => new Response('', { status: 404 }))
    await Promise.all([s.init({ fetch }), s.init({ fetch })])
    expect(fetch).toHaveBeenCalledTimes(1)
    await s.ready()
    expect(s.initialized).toBe(true)
  })
})

describe('connections store: status', () => {
  it('marks gos3 connections online with whoami and records the server id', async () => {
    const whoami = { user: 'root', accessKey: 'AK', isRoot: true, policies: ['admin'], capabilities: { ...NO_CAPABILITIES, admin: true, manageUsers: true }, server: INFO }
    vi.stubGlobal('fetch', vi.fn(async (req: Request) => {
      if (req.url.endsWith('/_gos3/info')) return new Response(JSON.stringify(INFO))
      if (req.url.endsWith('/_gos3/admin/v1/whoami')) return new Response(JSON.stringify(whoami))
      return new Response('', { status: 404 })
    }))
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'http://nas:9000', accessKeyId: 'AK', secretAccessKey: 'SK' })
    const st = await s.refreshStatus(c.id)
    expect(st.state).toBe('online')
    expect(st.whoami?.user).toBe('root')
    expect(s.capabilitiesOf(c.id).manageUsers).toBe(true)
    expect(s.byId(c.id)?.serverId).toBe(INFO.id)
  })

  it('detects rejected credentials and offline servers', async () => {
    vi.stubGlobal('fetch', vi.fn(async (req: Request) => {
      if (req.url.endsWith('/_gos3/info')) return new Response(JSON.stringify(INFO))
      return new Response(JSON.stringify({ error: { code: 'Unauthenticated', message: 'InvalidAccessKeyId' } }), { status: 401 })
    }))
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'http://nas:9000', accessKeyId: 'AK', secretAccessKey: 'bad' })
    expect((await s.refreshStatus(c.id)).state).toBe('unauthorized')

    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))))
    expect((await s.refreshStatus(c.id)).state).toBe('offline')
  })

  it('reports a reachable server whose CORS blocks signed requests as an error, not offline', async () => {
    vi.stubGlobal('fetch', vi.fn(async (req: Request) => {
      // /_gos3/info sends CORS * and answers; the signed whoami is blocked.
      if (req.url.endsWith('/_gos3/info')) return new Response(JSON.stringify(INFO))
      throw new TypeError('Failed to fetch')
    }))
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'http://nas:9000', accessKeyId: 'AK', secretAccessKey: 'SK' })
    const st = await s.refreshStatus(c.id)
    expect(st.state).toBe('error')
    expect(st.errorCode).toBe(CORS_BLOCKED)
    expect(st.error).toMatch(/nas:9000 is reachable, but the browser blocked the request \(CORS\)/)
    expect(st.info?.id).toBe(INFO.id)

    // The same for a gos3 server configured as generic S3.
    const g = s.add({ endpoint: 'http://nas2:9000', kind: 's3', accessKeyId: 'AK', secretAccessKey: 'SK' })
    const sg = await s.refreshStatus(g.id)
    expect(sg.state).toBe('error')
    expect(sg.errorCode).toBe(CORS_BLOCKED)
  })

  it('checks generic S3 servers with ListBuckets', async () => {
    vi.stubGlobal('fetch', vi.fn(async (req: Request) => {
      if (req.url.endsWith('/_gos3/info')) return new Response('', { status: 404 })
      return new Response('<Error><Code>SignatureDoesNotMatch</Code><Message>bad</Message></Error>', { status: 403 })
    }))
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'https://s3.example.com', kind: 's3', accessKeyId: 'AK', secretAccessKey: 'SK' })
    const st = await s.refreshStatus(c.id)
    expect(st.state).toBe('unauthorized')
    expect(st.whoami).toBeNull()
    expect(s.capabilitiesOf(c.id)).toEqual(NO_CAPABILITIES)
  })

  it('dedupes concurrent refreshes and caches with ensureStatus', async () => {
    const fetch = vi.fn(async () => new Response(JSON.stringify(INFO)))
    vi.stubGlobal('fetch', fetch)
    const s = useConnectionsStore()
    const c = s.add({ endpoint: 'http://nas:9000' })
    const [a, b] = await Promise.all([s.refreshStatus(c.id), s.refreshStatus(c.id)])
    expect(a).toBe(b)
    expect(a.state).toBe('online')
    const calls = fetch.mock.calls.length
    await s.ensureStatus(c.id)
    expect(fetch.mock.calls.length).toBe(calls)
  })
})

describe('connections store: several tabs (review fix)', () => {
  it('merges what another tab persisted instead of overwriting it', () => {
    const tabA = useConnectionsStore(createPinia())
    tabA.hydrate()
    const one = tabA.add({ id: 'one', endpoint: 'http://one:9000', accessKeyId: 'AK1', secretAccessKey: 'S1' })
    const tabB = useConnectionsStore(createPinia())
    tabB.hydrate()
    expect(tabB.byId(one.id)).toBeDefined()

    tabA.add({ id: 'two', endpoint: 'http://two:9000', accessKeyId: 'AK2', secretAccessKey: 'S2', remember: true })
    // The browser delivers a 'storage' event to tab B.
    tabB.syncFromStorage(localStorage.getItem(LOCAL_STORAGE_KEY))
    tabB.setActive('one')
    expect(local().connections.map((c: { id: string }) => c.id)).toEqual(['one', 'two'])
    expect(local().connections[1].secretAccessKey).toBe('S2')
    expect(tabB.byId('two')?.secretAccessKey).toBe('S2')

    // Removal in B reaches A; A keeps its own active server and session secret.
    tabA.setActive('one')
    tabB.remove('two')
    tabA.syncFromStorage(localStorage.getItem(LOCAL_STORAGE_KEY))
    expect(tabA.byId('two')).toBeUndefined()
    expect(tabA.activeId).toBe('one')
    expect(tabA.byId('one')?.secretAccessKey).toBe('S1')
  })

  it('listens for storage events after init', async () => {
    let listener: ((e: { key: string | null, newValue: string | null }) => void) | undefined
    vi.stubGlobal('addEventListener', (type: string, fn: typeof listener) => {
      if (type === 'storage') listener = fn
    })
    const tab = useConnectionsStore(createPinia())
    await tab.init({ skipServersJson: true })
    expect(listener).toBeTypeOf('function')
    const state = { version: 1, activeId: null, connections: [{ id: 'x', endpoint: 'http://x:9000', name: 'x' }], dismissedPresets: [] }
    listener!({ key: LOCAL_STORAGE_KEY, newValue: JSON.stringify(state) })
    expect(tab.byId('x')?.endpoint).toBe('http://x:9000')
  })
})

describe('connections store: stale status checks (review fix)', () => {
  it('drops the result of a check started with the previous credentials', async () => {
    let release!: () => void
    const gate = new Promise<void>(r => (release = r))
    const whoami = { user: 'root', accessKey: 'good', isRoot: true, policies: ['admin'], capabilities: { ...NO_CAPABILITIES, admin: true }, server: INFO }
    vi.stubGlobal('fetch', vi.fn(async (req: Request) => {
      if (req.url.endsWith('/_gos3/info')) return new Response(JSON.stringify(INFO))
      if ((req.headers.get('authorization') ?? '').includes('Credential=bad/')) {
        await gate
        return new Response(JSON.stringify({ error: { code: 'InvalidAccessKeyId', message: 'unknown key' } }), { status: 401 })
      }
      return new Response(JSON.stringify(whoami))
    }))
    const s = useConnectionsStore()
    const c = s.add({ id: 'c', endpoint: 'http://nas:9000', accessKeyId: 'bad', secretAccessKey: 'SK' })
    const old = s.refreshStatus(c.id)
    await new Promise(r => setTimeout(r, 10)) // the old check is waiting for whoami
    s.setCredentials(c.id, { accessKeyId: 'good', secretAccessKey: 'SK' })
    const fresh = await s.refreshStatus(c.id)
    expect(fresh.state).toBe('online')
    release()
    await old
    expect(s.statusOf('c').state).toBe('online')
    expect(s.statusOf('c').whoami?.accessKey).toBe('good')
  })
})
