import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { completeConnection, planConnectionImport } from '~/utils/connections'
import { ConnectionTransport, configureTransports, getTransport } from '~/utils/transport'
import { useConnectionsStore } from '~/stores/connections'
import { getAdminClient, getS3Client } from '~/utils/clients'
import { AwsClient } from '~/utils/s3/signing'

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
})
