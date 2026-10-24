import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import type { Connection } from '~/types/connection'
import { completeConnection, EMBEDDED_CONNECTION_ID, planConnectionImport } from '~/utils/connections'
import { ConnectionTransport, configureTransports, getTransport } from '~/utils/transport'
import { LOCAL_STORAGE_KEY, useConnectionsStore } from '~/stores/connections'
import { getAdminClient, getS3Client } from '~/utils/clients'
import { AwsClient } from '~/utils/s3/signing'
import { sha256Hex } from '~/utils/s3/hash'

const relay = completeConnection({ id: 'relay', endpoint: 'https://relay.test', accessKeyId: 'relay-key', secretAccessKey: 'relay-secret' })
const target = completeConnection({ id: 'target', endpoint: 'https://target.test', accessKeyId: 'target-key', secretAccessKey: 'target-secret', proxyConnectionId: relay.id })
const upstream = (body = 'ok', status = 200) => new Response(body, { status, headers: { 'X-Gos3-Proxy': 'upstream' } })
const route = vi.fn()
const meta = (request: Request) => JSON.parse(Buffer.from(new URL(request.url).searchParams.get('request')!, 'base64url').toString())
function network(destination: (r: Request) => Response | Promise<Response> = () => { throw new TypeError('unreachable') }) {
  return vi.fn(async (request: Request) => {
    if (request.url.startsWith(target.endpoint)) return destination(request)
    if (request.url.endsWith('/whoami')) return Response.json({ capabilities: { proxy: true } })
    return upstream()
  })
}
beforeEach(() => {
  route.mockClear()
  configureTransports(id => id === relay.id ? relay : undefined, route)
})
describe('one-hop automatic relay transport', () => {
  it('uses direct responses, including destination authentication errors', async () => {
    const fetch = network(() => new Response('denied', { status: 403 }))
    const transport = new ConnectionTransport(target, fetch)
    expect((await transport.send(new Request(target.endpoint))).status).toBe(403)
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(route).not.toHaveBeenCalled()
  })
  it('retries reads once, binds metadata and credentials, and sticks to the relay', async () => {
    const fetch = network()
    const transport = new ConnectionTransport(target, fetch)
    const req = await new AwsClient({ accessKeyId: target.accessKeyId, secretAccessKey: target.secretAccessKey, service: 's3', region: 'us-east-1' }).sign(target.endpoint + '/bucket/a%2Fb?versionId=x%2By', { headers: { Range: 'bytes=0-1' } })
    expect(await (await transport.send(req)).text()).toBe('ok')
    const outer = fetch.mock.calls.at(-1)![0]
    expect(meta(outer)).toMatchObject({ url: req.url, method: 'GET', headers: { range: 'bytes=0-1' } })
    expect(meta(outer).headers.authorization).toContain('target-key')
    expect(outer.headers.get('authorization')).toContain('relay-key')
    expect(outer.url).not.toContain('target-secret')
    await transport.send(new Request(target.endpoint + '/second'))
    expect(fetch.mock.calls.filter(([r]) => r.url.startsWith(target.endpoint))).toHaveLength(1)
    expect(transport.proxyConnectionId).toBe('relay')
  })
  it.each(['PUT', 'POST', 'PATCH', 'DELETE'])('never replays a failed %s; selects the relay using HEAD', async method => {
    const fetch = network()
    const transport = new ConnectionTransport(target, fetch)
    await expect(transport.send(new Request(target.endpoint + '/write', { method, body: 'data' }))).rejects.toMatchObject({ code: 'WriteOutcomeUnknown' })
    const forwarded = fetch.mock.calls.filter(([r]) => new URL(r.url).pathname.endsWith('/proxy')).map(([r]) => meta(r).method)
    expect(forwarded).toEqual(['HEAD'])
    await transport.send(new Request(target.endpoint + '/write', { method, body: 'retry' }))
    expect(meta(fetch.mock.calls.at(-1)![0]).method).toBe(method)
  })
  it('distinguishes relay errors and refuses old relays without capability', async () => {
    const fetch = network()
    fetch.mockImplementation(async r => r.url.endsWith('/whoami') ? Response.json({ capabilities: {} }) : Promise.reject(new TypeError('offline')))
    await expect(new ConnectionTransport(target, fetch).send(new Request(target.endpoint))).rejects.toMatchObject({ code: 'ProxyUnavailable' })
    const failed = network()
    failed.mockImplementation(async r => {
      if (r.url.endsWith('/whoami')) return Response.json({ capabilities: { proxy: true } })
      if (r.url.startsWith(relay.endpoint)) return Response.json({ error: {} }, { status: 502, headers: { 'X-Gos3-Proxy': 'error' } })
      throw new TypeError('offline')
    })
    await expect(new ConnectionTransport(target, failed).send(new Request(target.endpoint))).rejects.toMatchObject({ code: 'ProxyFailed', status: 502 })
  })
  it('reports a relay 502 after a write as an unknown outcome', async () => {
    const fetch = network()
    const transport = new ConnectionTransport(target, fetch)
    await transport.send(new Request(target.endpoint)) // activate the relay with a safe read
    fetch.mockImplementation(async r => r.url.endsWith('/whoami')
      ? Response.json({ capabilities: { proxy: true } })
      : Response.json({ error: {} }, { status: 502, headers: { 'X-Gos3-Proxy': 'error' } }))
    await expect(transport.send(new Request(target.endpoint + '/bucket/key', { method: 'PUT', body: 'data' })))
      .rejects.toMatchObject({ code: 'WriteOutcomeUnknown', status: 502 })
    expect(fetch.mock.calls.filter(([r]) => new URL(r.url).pathname.endsWith('/proxy')).map(([r]) => meta(r).method))
      .toEqual(['GET', 'PUT'])
  })
  it('pins a delayed S3 upload to a long-lived, signed payload hash', async () => {
    const fetch = network()
    const transport = new ConnectionTransport(target, fetch)
    await transport.send(new Request(target.endpoint))
    const hash = await sha256Hex('upload bytes')
    const oldDate = '20200101T000000Z'
    const signed = await new AwsClient({ accessKeyId: target.accessKeyId, secretAccessKey: target.secretAccessKey, region: target.region, service: 's3' })
      .sign(target.endpoint + '/bucket/key', { method: 'PUT', body: 'upload bytes', headers: { 'X-Amz-Content-Sha256': hash }, aws: { datetime: oldDate } } as RequestInit)
    await transport.send(signed, fetch, true)
    const outer = fetch.mock.calls.at(-1)![0]
    const destination = meta(outer)
    const url = new URL(destination.url)
    expect(destination.method).toBe('PUT')
    expect(Number(url.searchParams.get('X-Amz-Expires'))).toBeGreaterThan(3600)
    expect(Number(url.searchParams.get('X-Amz-Expires'))).toBeLessThan(604800)
    expect(url.searchParams.get('X-Amz-Date')).not.toBe(oldDate)
    expect(url.searchParams.get('X-Amz-Content-Sha256')).toBe(hash)
    expect(url.searchParams.get('X-Amz-SignedHeaders')).toContain('x-amz-content-sha256')
    expect(destination.headers['x-amz-content-sha256']).toBe(hash)
    expect(destination.headers.authorization).toBeUndefined()
    expect(await outer.text()).toBe('upload bytes')
  })
  it('does not fallback after user cancellation', async () => {
    const controller = new AbortController(); controller.abort()
    const fetch = network(() => { throw new DOMException('abort', 'AbortError') })
    await expect(new ConnectionTransport(target, fetch).send(new Request(target.endpoint, { signal: controller.signal }))).rejects.toHaveProperty('name', 'AbortError')
    expect(fetch).toHaveBeenCalledTimes(1)
  })
  it('wraps presigned GET URLs with destination-bound expiry', async () => {
    const fetch = network()
    const transport = new ConnectionTransport(target, fetch)
    await transport.send(new Request(target.endpoint))
    const aws = new AwsClient({ accessKeyId: target.accessKeyId, secretAccessKey: target.secretAccessKey, region: target.region, service: 's3' })
    const url = new URL(target.endpoint + '/bucket/file'); url.searchParams.set('X-Amz-Expires', '60')
    const destination = (await aws.sign(url.toString(), { aws: { signQuery: true } } as RequestInit)).url
    const wrapped = new URL(await transport.presign(destination, 900))
    expect(wrapped.origin).toBe(relay.endpoint)
    expect(Number(wrapped.searchParams.get('X-Amz-Expires'))).toBeLessThanOrEqual(60)
    expect(meta(new Request(wrapped)).url).toBe(destination)
  })
})
describe('relay connection persistence and invalidation', () => {
  beforeEach(() => { localStorage.clear(); sessionStorage.clear(); setActivePinia(createPinia()) })
  it('preserves settings and never imports a changed relay onto stored credentials', () => {
    const store = useConnectionsStore(); store.add(relay); store.add(target)
    expect(store.exportData().connections.find(c => c.id === target.id)?.proxyConnectionId).toBe(relay.id)
    const plan = planConnectionImport(store.connections, [{ ...target, proxyConnectionId: 'other', secretAccessKey: undefined }])
    expect(plan.steps[0]?.action).toBe('add')
    store.resetState(); store.hydrate()
    expect(store.byId(target.id)?.proxyConnectionId).toBe(relay.id)
  })
  it('invalidates dependent clients and statuses when relay credentials change', () => {
    const store = useConnectionsStore(); store.add(relay); store.add(target)
    const client = getS3Client(target)
    const transport = getTransport(target)
    store.statuses[target.id] = { state: 'online', info: null, whoami: null, proxyConnectionId: relay.id }
    store.setCredentials(relay.id, { accessKeyId: 'changed', secretAccessKey: 'changed' })
    expect(store.statusOf(target.id).state).toBe('unknown')
    expect(getS3Client(target)).not.toBe(client)
    expect(getTransport(target)).not.toBe(transport)
  })
  it('rejects self references and relay chains', () => {
    const store = useConnectionsStore(); store.add(relay); store.add(target)
    expect(() => store.update(relay.id, { proxyConnectionId: target.id })).toThrow()
    expect(() => store.update(target.id, { proxyConnectionId: target.id })).toThrow()
  })
  it('preserves relay errors through the S3 and admin clients', async () => {
    const store = useConnectionsStore(); store.add(relay); store.add(target)
    vi.stubGlobal('fetch', network())
    // A failed direct write is surfaced as uncertain, not as a generic network error.
    await expect(getS3Client(target).createBucket('bucket')).rejects.toMatchObject({ code: 'WriteOutcomeUnknown' })
    const fresh = completeConnection({ ...target, id: 'second' }); store.add(fresh)
    await expect(getAdminClient(fresh).createUser({ name: 'new-user', policies: [], disabled: false })).rejects.toMatchObject({ code: 'WriteOutcomeUnknown' })
  })
  it('never makes an imported connection the relay of a deleted relay\'s dependents (N-13)', async () => {
    const store = useConnectionsStore()
    store.add({ id: 'k3x9a1qz', endpoint: 'https://office-gw.test', kind: 'gos3', accessKeyId: 'gw-key', secretAccessKey: 'gw-secret' })
    store.add({ id: 'nas', endpoint: 'https://nas.test', accessKeyId: 'nas-key', secretAccessKey: 'nas-secret', proxyConnectionId: 'k3x9a1qz' })
    const held = store.byId('nas')!
    store.remove('k3x9a1qz')
    store.importJson({ type: 'gos3-connections', connections: [{ id: 'k3x9a1qz', endpoint: 'https://evil.example', kind: 'gos3', accessKeyId: 'atk', secretAccessKey: 'atk' }] })
    const fetch = vi.fn(async (request: Request) => {
      if (request.url.startsWith('https://nas.test')) throw new TypeError('unreachable')
      if (request.url.endsWith('/whoami')) return Response.json({ capabilities: { proxy: true } })
      return upstream()
    })
    vi.stubGlobal('fetch', fetch)
    for (const nas of [store.byId('nas')!, held]) {
      await getS3Client(nas).listBuckets().catch(() => {})
      const aws = new AwsClient({ accessKeyId: nas.accessKeyId, secretAccessKey: nas.secretAccessKey, region: nas.region, service: 's3' })
      const destination = (await aws.sign(nas.endpoint + '/bucket/file?X-Amz-Expires=60', { aws: { signQuery: true } } as RequestInit)).url
      expect(new URL(await getTransport(nas).presign(destination, 900).catch(() => destination)).origin).toBe('https://nas.test')
    }
    expect(fetch.mock.calls.length).toBeGreaterThan(0)
    expect(fetch.mock.calls.filter(([r]) => new URL(r.url).hostname === 'evil.example')).toEqual([])
  })
  it('clears and persists the relay of a removed relay\'s dependents', () => {
    const store = useConnectionsStore(); store.add(relay); store.add(target)
    const held = store.byId(target.id)!
    store.remove(relay.id)
    expect(store.byId(target.id)?.proxyConnectionId).toBeUndefined()
    expect(held.proxyConnectionId).toBeUndefined()
    expect(JSON.parse(localStorage.getItem(LOCAL_STORAGE_KEY)!).connections.find((c: Connection) => c.id === target.id)).not.toHaveProperty('proxyConnectionId')
    store.resetState(); store.hydrate()
    expect(store.byId(target.id)?.proxyConnectionId).toBeUndefined()
  })
  it('drops dangling relay IDs on hydrate and persists the result', () => {
    const state = { version: 1, activeId: null, dismissedPresets: [], connections: [
      { id: 'nas', endpoint: 'https://nas.test', proxyConnectionId: 'k3x9a1qz' },
      { id: 'peer', endpoint: 'https://peer.test', proxyConnectionId: EMBEDDED_CONNECTION_ID },
      relay, target
    ] }
    localStorage.setItem(LOCAL_STORAGE_KEY, JSON.stringify(state))
    const store = useConnectionsStore(); store.hydrate()
    expect(store.byId('nas')?.proxyConnectionId).toBeUndefined()
    // Reserved IDs can never be taken by an import; the embedded server is merged in later.
    expect(store.byId('peer')?.proxyConnectionId).toBe(EMBEDDED_CONNECTION_ID)
    expect(store.byId(target.id)?.proxyConnectionId).toBe(relay.id)
    const stored = JSON.parse(localStorage.getItem(LOCAL_STORAGE_KEY)!).connections as Connection[]
    expect(stored.map(c => c.proxyConnectionId)).toEqual([undefined, EMBEDDED_CONNECTION_ID, undefined, relay.id])
    expect(store.importJson([{ id: 'k3x9a1qz', endpoint: 'https://evil.example', accessKeyId: 'atk', secretAccessKey: 'atk' }]).added).toBe(1)
    expect(store.byId('nas')?.proxyConnectionId).toBeUndefined()
  })
  it('drops dangling relay IDs another tab stored, without writing back', () => {
    const tabA = useConnectionsStore(createPinia()); tabA.hydrate(); tabA.add(relay); tabA.add(target)
    const held = tabA.byId(target.id)!
    // Another tab removes the relay.
    const tabB = useConnectionsStore(createPinia()); tabB.hydrate(); tabB.remove(relay.id)
    tabA.syncFromStorage(localStorage.getItem(LOCAL_STORAGE_KEY))
    expect(tabA.byId(relay.id)).toBeUndefined()
    expect(tabA.byId(target.id)?.proxyConnectionId).toBeUndefined()
    expect(held.proxyConnectionId).toBeUndefined()
    // A list written by an older version still names the deleted relay.
    const raw = JSON.stringify({ version: 1, activeId: null, dismissedPresets: [], connections: [{ ...target, secretAccessKey: '' }] })
    localStorage.setItem(LOCAL_STORAGE_KEY, raw)
    tabA.syncFromStorage(raw)
    expect(tabA.byId(target.id)?.proxyConnectionId).toBeUndefined()
    expect(tabA.byId(target.id)?.secretAccessKey).toBe(target.secretAccessKey)
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).toBe(raw)
  })
  it('gives an imported entry a fresh ID when a stored connection names that ID as its relay', () => {
    const nas = completeConnection({ id: 'nas', endpoint: 'https://nas.test', proxyConnectionId: 'k3x9a1qz' })
    const plan = planConnectionImport([nas], [{ id: 'k3x9a1qz', endpoint: 'https://evil.example', kind: 'gos3' }])
    expect(plan.steps).toHaveLength(1)
    expect(plan.steps[0]).toMatchObject({ action: 'add', input: { endpoint: 'https://evil.example' } })
    expect(plan.steps[0]?.action === 'add' && plan.steps[0].input.id).not.toBe('k3x9a1qz')
    // A relay imported together with its dependents keeps its ID, in either order.
    const pair = planConnectionImport([], [{ id: 'dep', endpoint: 'https://dep.test', proxyConnectionId: 'gw' }, { id: 'gw', endpoint: 'https://gw.test' }])
    expect(pair.steps.map(s => s.action === 'add' && s.input.id)).toEqual(['dep', 'gw'])
  })
  it('validates the relays of imported connections like add()', () => {
    const store = useConnectionsStore(); store.add(relay); store.add(target)
    const before = localStorage.getItem(LOCAL_STORAGE_KEY)
    // A chain (the relay has a relay itself) aborts the import before anything changes.
    expect(() => store.importJson([{ id: 'new', endpoint: 'https://new.test' }, { id: 'chained', endpoint: 'https://chained.test', proxyConnectionId: target.id }])).toThrow(/relay/)
    expect(store.byId('new')).toBeUndefined()
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).toBe(before)
    // A relay that follows its dependent in the file is accepted; one that names nothing is dropped.
    store.importJson([{ id: 'dep', endpoint: 'https://dep.test', proxyConnectionId: 'gw' }, { id: 'gw', endpoint: 'https://gw.test' }, { id: 'lone', endpoint: 'https://lone.test', proxyConnectionId: 'nowhere' }])
    expect(store.byId('dep')?.proxyConnectionId).toBe('gw')
    expect(store.byId('lone')?.proxyConnectionId).toBeUndefined()
  })
  it('never restores a preset relay that the user removed', () => {
    const store = useConnectionsStore()
    const presets = [{ id: 'p', endpoint: 'https://p.test', proxyConnectionId: 'gw' }, { id: 'gw', endpoint: 'https://gw.test' }]
    store.applyPresets(presets)
    expect(store.byId('p')?.proxyConnectionId).toBe('gw')
    store.setCredentials('p', { accessKeyId: 'pk', secretAccessKey: 'ps' })
    store.remove('gw')
    store.importJson([{ id: 'gw', endpoint: 'https://evil.example', accessKeyId: 'atk', secretAccessKey: 'atk' }])
    store.applyPresets(presets) // the next start
    expect(store.byId('p')?.proxyConnectionId).toBeUndefined()
    expect(store.byId('p')?.secretAccessKey).toBe('ps')
  })
})
