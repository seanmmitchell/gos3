// Client-side discovery helpers: aggregation, scan form, local discovery client.
import { describe, expect, it } from 'vitest'
import type { Info, Peer } from '~/types/admin'
import type { Connection } from '~/types/connection'
import { aggregatePeers, findConfigured, parseUrlList, peerFromInfo, probeUrlFor, serverUrls, sourceLabel } from '~/components/discovery/-aggregate'
import { LocalDiscoveryClient } from '~/components/discovery/-local-client'
import {
  defaultScanForm,
  estimateHosts,
  estimateProbes,
  parsePorts,
  scanFormErrors,
  suggestCidr,
  targetError,
  toScanRequest
} from '~/components/discovery/-scan-form'
import type { FetchLike } from '~/utils/admin/client'

const peer = (over: Partial<Peer>): Peer => ({
  id: 'p1',
  name: 'nas',
  version: '0.1.0',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.0.0.5:9000', admin: 'http://10.0.0.5:9000/_gos3/admin/v1' },
  features: [],
  sources: ['mdns'],
  addresses: ['http://10.0.0.5:9000'],
  firstSeen: '2026-10-09T12:00:00Z',
  lastSeen: '2026-10-09T12:00:00Z',
  healthy: true,
  verified: true,
  ...over
})

const conn = (over: Partial<Connection>): Connection => ({
  id: 'c', name: 'c', endpoint: 'http://x:9000', region: 'us-east-1', accessKeyId: '', secretAccessKey: '', kind: 'gos3', forcePathStyle: true, remember: false, source: 'user', ...over
})

describe('aggregatePeers', () => {
  it('deduplicates by id and records who found each server', () => {
    const servers = aggregatePeers([
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [peer({}), peer({ id: 'p2', name: 'zeta', verified: false, healthy: false, error: 'timeout' })] },
      { finder: { kind: 'server', label: 'B', connectionId: 'b' }, peers: [peer({ sources: ['beacon', 'scan'], addresses: ['http://192.168.1.20:9000'], lastSeen: '2026-10-09T12:05:00Z', name: 'nas-renamed' })] },
      { finder: { kind: 'browser', label: 'Browser' }, peers: [peer({ sources: ['probe'] }), peer({ id: 'p2', name: 'zeta', healthy: true, verified: true })] },
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [peer({ sources: ['static'] })] },
      { finder: { kind: 'local', label: 'UI host' }, peers: null }
    ])
    expect(servers.map(s => s.id)).toEqual(['p1', 'p2'])
    const nas = servers[0]!
    expect(nas.name).toBe('nas-renamed') // most recent verified sighting
    expect(nas.addresses).toEqual(['http://10.0.0.5:9000', 'http://192.168.1.20:9000'])
    expect(nas.lastSeen).toBe('2026-10-09T12:05:00Z')
    expect(nas.foundBy).toEqual([
      { kind: 'server', label: 'A', connectionId: 'a', sources: ['mdns', 'static'] },
      { kind: 'server', label: 'B', connectionId: 'b', sources: ['beacon', 'scan'] },
      { kind: 'browser', label: 'Browser', sources: ['probe'] }
    ])
    const zeta = servers[1]!
    expect(zeta.healthy && zeta.verified).toBe(true)
    expect(zeta.error).toBeUndefined()
  })

  it('sorts unverified servers last', () => {
    const s = aggregatePeers([{ finder: { kind: 'local', label: 'L' }, peers: [peer({ id: 'u', name: 'aaa', verified: false }), peer({ id: 'v', name: 'zzz' })] }])
    expect(s.map(x => x.id)).toEqual(['v', 'u'])
  })

  it('finds configured connections by server id or URL', () => {
    const s = aggregatePeers([{ finder: { kind: 'browser', label: 'B' }, peers: [peer({ endpoints: { s3: 'http://10.0.0.5:9000', admin: '', all: ['http://nas:9000'] } })] }])[0]!
    expect(serverUrls(s)).toEqual(['http://10.0.0.5:9000', 'http://nas:9000'])
    expect(findConfigured(s, [conn({ id: 'x', serverId: 'p1' })])?.id).toBe('x')
    expect(findConfigured(s, [conn({ id: 'y', endpoint: 'http://NAS:9000/' })])?.id).toBe('y')
    expect(findConfigured(s, [conn({ id: 'z' })])).toBeUndefined()
  })

  it('builds peers from identity documents', () => {
    const info: Info = { service: 'gos3', id: 'i', name: 'n', version: 'v', apiVersion: 'v1', region: 'r', endpoints: { s3: 'http://h:1', admin: '' }, features: ['ui'], startedAt: '' }
    const p = peerFromInfo(info, 'h:1/', 'probe', new Date('2026-01-01T00:00:00Z'))
    expect(p).toMatchObject({ id: 'i', sources: ['probe'], addresses: ['http://h:1'], verified: true, lastSeen: '2026-01-01T00:00:00.000Z' })
  })

  it('parses probe input', () => {
    expect(parseUrlList(' http://a:1 , b\nc;  b ')).toEqual(['http://a:1', 'b', 'c'])
    expect(probeUrlFor('nas.local')).toBe('http://nas.local:9000')
    expect(probeUrlFor('nas.local:9001')).toBe('http://nas.local:9001')
    expect(probeUrlFor('https://x/')).toBe('https://x')
    expect(probeUrlFor('fd00::5')).toBe('http://[fd00::5]:9000')
    expect(probeUrlFor('[fd00::5]:9001')).toBe('http://[fd00::5]:9001')
    expect(sourceLabel('mdns')).toBe('mDNS')
    expect(sourceLabel('other')).toBe('other')
  })
})

describe('scan form', () => {
  it('parses ports and estimates hosts', () => {
    expect(parsePorts('9000, 9001 9000')).toEqual({ ports: [9000, 9001] })
    expect(parsePorts('70000').error).toMatch(/out of range/)
    expect(parsePorts('abc').error).toMatch(/not a port/)
    expect(estimateHosts(['10.0.0.0/24', 'nas', '10.0.0.1/32'])).toBe(256)
    expect(estimateProbes({ ...defaultScanForm(), targets: '10.0.0.0/30\nhttp://x:1\nnas:9001', ports: '9000,9001', https: true })).toBe(2 * 2 * 2 + 1 + 2)
  })

  it('validates targets', () => {
    expect(targetError('10.0.0.0/24')).toBeUndefined()
    expect(targetError('10.0.0.0/33')).toMatch(/CIDR/)
    expect(targetError('300.0.0.0/24')).toMatch(/CIDR/)
    expect(targetError('ftp://x')).toMatch(/only http/)
    expect(targetError('nas.local:9000')).toBeUndefined()
    expect(targetError('[::1]:9000')).toBeUndefined()
    expect(targetError('bad host')).toBeDefined()
  })

  it('validates and converts the model', () => {
    const m = defaultScanForm()
    expect(scanFormErrors(m)).toEqual({ targets: 'Enter at least one target' })
    expect(scanFormErrors({ ...m, targets: '10.0.0.0/21' }).targets).toMatch(/limit is 1024/)
    expect(scanFormErrors({ ...m, targets: 'nas', http: false })).toEqual({ schemes: 'Select at least one scheme' })
    expect(scanFormErrors({ ...m, targets: 'nas', timeoutSeconds: 0, concurrency: 0 })).toMatchObject({ timeoutSeconds: expect.any(String), concurrency: expect.any(String) })
    expect(toScanRequest({ ...m, targets: '10.0.0.0/24, nas', ports: '9000 9443', https: true, timeoutSeconds: 2.5, concurrency: 32 })).toEqual({
      targets: ['10.0.0.0/24', 'nas'],
      ports: [9000, 9443],
      schemes: ['http', 'https'],
      timeout: 2_500_000_000,
      concurrency: 32
    })
  })

  it('suggests a /24 from known URLs', () => {
    expect(suggestCidr([undefined, 'http://127.0.0.1:9000', 'http://192.168.1.20:9000'])).toBe('192.168.1.0/24')
    expect(suggestCidr(['http://nas.local:9000'])).toBeUndefined()
  })
})

describe('LocalDiscoveryClient', () => {
  const caps = { localDiscovery: true, mdns: true, beacon: true, scan: true, maxScanHosts: 1024 }

  it('detects the Nitro routes and ignores SPA fallbacks', async () => {
    const ok: FetchLike = async req => new Response(JSON.stringify(caps), { status: new URL(req.url).pathname === '/ui/api/discovery/capabilities' ? 200 : 404 })
    expect(await new LocalDiscoveryClient('/ui/', { fetch: ok }).capabilities()).toEqual(caps)
    const html: FetchLike = async () => new Response('<!doctype html><html></html>', { status: 200, headers: { 'content-type': 'text/html' } })
    expect(await new LocalDiscoveryClient('/', { fetch: html }).capabilities()).toBeNull()
    const missing: FetchLike = async () => new Response('not found', { status: 404 })
    expect(await new LocalDiscoveryClient('/', { fetch: missing }).capabilities()).toBeNull()
    const offline: FetchLike = async () => {
      throw new TypeError('Failed to fetch')
    }
    expect(await new LocalDiscoveryClient('/', { fetch: offline }).capabilities()).toBeNull()
    const disabled: FetchLike = async () => new Response(JSON.stringify({ ...caps, localDiscovery: false }))
    // Turned off on the UI server: reported (so the UI can say so), not null.
    expect(await new LocalDiscoveryClient('/', { fetch: disabled }).capabilities()).toEqual({ ...caps, localDiscovery: false })
  })

  it('calls the discovery routes and parses error envelopes', async () => {
    const seen: { method: string, path: string, ct: string | null, body: string }[] = []
    const f: FetchLike = async (req) => {
      const u = new URL(req.url)
      seen.push({ method: req.method, path: u.pathname + u.search, ct: req.headers.get('content-type'), body: req.method === 'POST' ? await req.text() : '' })
      if (u.pathname.endsWith('/local')) return new Response(JSON.stringify({ peers: [peer({})], errors: ['mDNS: x'] }))
      if (u.pathname.endsWith('/scan')) return new Response(JSON.stringify({ error: { code: 'InvalidRequest', message: 'no targets' } }), { status: 400 })
      return new Response(JSON.stringify({ error: { code: 'ProbeFailed', message: 'http://x: timed out' } }), { status: 502 })
    }
    const c = new LocalDiscoveryClient('/', { fetch: f })
    const r = await c.discover(1500)
    expect(r.peers).toHaveLength(1)
    expect(r.errors).toEqual(['mDNS: x'])
    await expect(c.scan({ targets: [] })).rejects.toMatchObject({ code: 'InvalidRequest', status: 400, message: 'no targets' })
    await expect(c.probe('http://x')).rejects.toMatchObject({ code: 'ProbeFailed', status: 502 })
    expect(seen[0]).toMatchObject({ method: 'GET', path: '/api/discovery/local?wait=1500' })
    expect(seen[1]).toMatchObject({ method: 'POST', path: '/api/discovery/scan', ct: 'application/json', body: '{"targets":[]}' })
    expect(seen[2]).toMatchObject({ method: 'POST', path: '/api/discovery/probe', body: '{"url":"http://x"}' })
  })
})
