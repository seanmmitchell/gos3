import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { LOCAL_STORAGE_KEY, NO_CAPABILITIES, SESSION_STORAGE_KEY, useConnectionsStore } from '~/stores/connections'
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

describe('connections store: presets & embedded', () => {
  const noFetch = () => Promise.reject(new TypeError('offline'))

  it('loads presets from runtime config JSON and keeps user credentials', async () => {
    const s = useConnectionsStore()
    await s.init({ presets: JSON.stringify([{ id: 'nas', name: 'NAS', endpoint: 'http://nas:9000', region: 'eu-west-1' }]), skipServersJson: true, fetch: noFetch })
    expect(s.byId('nas')).toMatchObject({ name: 'NAS', source: 'preset', region: 'eu-west-1', remember: false })
    s.setCredentials('nas', { accessKeyId: 'AK', secretAccessKey: 'SK' })

    // restart with a changed preset
    setActivePinia(createPinia())
    const s2 = useConnectionsStore()
    await s2.init({ presets: [{ id: 'nas', name: 'NAS 2', endpoint: 'http://nas:9001' }], skipServersJson: true, fetch: noFetch })
    expect(s2.byId('nas')).toMatchObject({ name: 'NAS 2', endpoint: 'http://nas:9001', accessKeyId: 'AK', secretAccessKey: 'SK' })
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
