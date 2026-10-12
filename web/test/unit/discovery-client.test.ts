// Client-side discovery helpers: aggregation, scan form, local discovery client.
import { describe, expect, it } from 'vitest'
import type { Info, Peer } from '~/types/admin'
import type { Connection } from '~/types/connection'
import { aggregatePeers, credentialsSourceFor, findConfigured, findIdClaim, parseUrlList, peerFromInfo, probeUrlFor, serverUrls, sourceLabel } from '~/components/discovery/-aggregate'
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
    // A multi-homed server: it advertises both addresses it is seen at.
    const endpoints = { s3: 'http://10.0.0.5:9000', admin: '', all: ['http://10.0.0.5:9000', 'http://192.168.1.20:9000'] }
    const servers = aggregatePeers([
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [peer({ endpoints }), peer({ id: 'p2', name: 'zeta', verified: false, healthy: false, error: 'timeout' })] },
      { finder: { kind: 'server', label: 'B', connectionId: 'b' }, peers: [peer({ endpoints, sources: ['beacon', 'scan'], addresses: ['http://192.168.1.20:9000'], lastSeen: '2026-10-09T12:05:00Z', name: 'nas-renamed' })] },
      { finder: { kind: 'browser', label: 'Browser' }, peers: [peer({ sources: ['probe'] }), peer({ id: 'p2', name: 'zeta', healthy: true, verified: true })] },
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [peer({ sources: ['static'] })] },
      { finder: { kind: 'local', label: 'UI host' }, peers: null }
    ])
    expect(servers.map(s => s.id)).toEqual(['p1', 'p2'])
    const nas = servers[0]!
    expect(nas.name).toBe('nas-renamed') // most recent verified sighting
    expect(nas.addresses).toEqual(['http://10.0.0.5:9000', 'http://192.168.1.20:9000'])
    expect(nas.lastSeen).toBe('2026-10-09T12:05:00Z')
    expect(nas.conflict).toBeUndefined()
    expect(nas.foundBy).toEqual([
      { kind: 'server', label: 'A', connectionId: 'a', sources: ['mdns', 'static'], addresses: ['http://10.0.0.5:9000'] },
      { kind: 'server', label: 'B', connectionId: 'b', sources: ['beacon', 'scan'], addresses: ['http://192.168.1.20:9000'] },
      { kind: 'browser', label: 'Browser', sources: ['probe'], addresses: ['http://10.0.0.5:9000'] }
    ])
    const zeta = servers[1]!
    expect(zeta.healthy && zeta.verified).toBe(true)
    expect(zeta.error).toBeUndefined()
  })

  it('sorts unverified servers last', () => {
    const s = aggregatePeers([{ finder: { kind: 'local', label: 'L' }, peers: [peer({ id: 'u', name: 'aaa', verified: false }), peer({ id: 'v', name: 'zzz' })] }])
    expect(s.map(x => x.id)).toEqual(['v', 'u'])
  })

  it('finds configured connections by observed address, never by server ID or advertised URL alone', () => {
    const s = aggregatePeers([{ finder: { kind: 'browser', label: 'B' }, peers: [peer({ endpoints: { s3: 'http://10.0.0.5:9000', admin: '', all: ['http://nas:9000'] } })] }])[0]!
    expect(serverUrls(s)).toEqual(['http://10.0.0.5:9000', 'http://nas:9000'])
    // Observed address (the one with the same ID first).
    expect(findConfigured(s, [conn({ id: 'w', endpoint: 'http://10.0.0.5:9000/' }), conn({ id: 'x', endpoint: 'http://10.0.0.5:9000', serverId: 'p1' })])?.id).toBe('x')
    expect(findConfigured(s, [conn({ id: 'y', endpoint: 'http://10.0.0.5:9000' })])?.id).toBe('y')
    // A rogue claiming a configured server's ID from another address is not "configured" (so never
    // hidden or linked as that server); it is reported as an ID claim instead.
    const idOnly = [conn({ id: 'x', serverId: 'p1', endpoint: 'https://prod:9000' })]
    expect(findConfigured(s, idOnly)).toBeUndefined()
    expect(findIdClaim(s, idOnly)?.id).toBe('x')
    // Advertised (self-declared) URLs do not count either.
    expect(findConfigured(s, [conn({ id: 'z', endpoint: 'http://NAS:9000/' })])).toBeUndefined()
    expect(findIdClaim(s, [conn({ id: 'y', endpoint: 'http://10.0.0.5:9000', serverId: 'p1' })])).toBeUndefined()
    expect(findIdClaim(s, [conn({ id: 'z' })])).toBeUndefined()
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
    const seen: { method: string, path: string, ct: string | null, marker: string | null, body: string }[] = []
    const f: FetchLike = async (req) => {
      const u = new URL(req.url)
      seen.push({ method: req.method, path: u.pathname + u.search, ct: req.headers.get('content-type'), marker: req.headers.get('X-Gos3-Discovery'), body: req.method === 'POST' ? await req.text() : '' })
      if (u.pathname.endsWith('/capabilities')) return new Response(JSON.stringify(caps))
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
    await c.capabilities()
    expect(seen[3]).toMatchObject({ method: 'GET', path: '/api/discovery/capabilities' })
    // Every /api/discovery/* request carries the UI marker header.
    expect(seen.map(s => s.marker)).toEqual(['1', '1', '1', '1'])
    expect(seen[0]!.ct).toBeNull()
  })
})

describe('aggregatePeers with spoofed server IDs (web-content-1, discovery-4)', () => {
  const legit = peer({ name: 'prod-nas', endpoints: { s3: 'https://nas.lan:9000', admin: '' }, addresses: ['https://nas.lan:9000'], lastSeen: '2026-10-09T12:00:00Z' })
  const rogueInfo: Info = { service: 'gos3', id: 'p1', name: 'prod-nas', version: '0.1.0', apiVersion: 'v1', region: 'us-east-1', endpoints: { s3: 'http://10.0.0.66:9000', admin: '', all: ['https://nas.lan:9000'] }, features: [], startedAt: '' }

  it('a fresher probe of another host claiming the ID does not take over the row', () => {
    const rogue = peerFromInfo(rogueInfo, 'http://10.0.0.66:9000', 'scan', new Date('2026-10-09T13:00:00Z'))
    const [s] = aggregatePeers([
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [legit] },
      { finder: { kind: 'browser', label: 'Browser' }, peers: [rogue] }
    ])
    expect(s!.endpoints.s3).toBe('https://nas.lan:9000')
    expect(s!.addresses).toEqual(['https://nas.lan:9000'])
    expect(s!.conflict).toBe(true)
    expect(s!.conflictAddresses).toEqual(['http://10.0.0.66:9000'])
    // No credentials are offered for a conflicting row, whatever the endpoint.
    expect(credentialsSourceFor(s!, 'https://nas.lan:9000', () => true)).toBeUndefined()
  })

  it('a rogue seen first keeps its row, but the real server is reported as a conflict', () => {
    const rogue = peer({ name: 'prod-nas', endpoints: rogueInfo.endpoints, addresses: ['http://10.0.0.66:9000'] })
    const [s] = aggregatePeers([
      { finder: { kind: 'server', label: 'C', connectionId: 'c' }, peers: [rogue] },
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [{ ...legit, lastSeen: '2026-10-09T14:00:00Z' }] }
    ])
    // The rogue's claim of the real address alone is not enough to merge.
    expect(s!.conflict).toBe(true)
    expect(s!.conflictAddresses).toEqual(['https://nas.lan:9000'])
  })

  it('a verified record replaces an unverified one at another address, flagged as a conflict', () => {
    const [s] = aggregatePeers([
      { finder: { kind: 'local', label: 'UI host' }, peers: [peer({ addresses: ['http://10.0.0.66:9000'], verified: false, healthy: false })] },
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [legit] }
    ])
    expect(s!.addresses).toEqual(['https://nas.lan:9000'])
    expect(s!.verified).toBe(true)
    expect(s!.conflict).toBe(true)
    expect(s!.conflictAddresses).toEqual(['http://10.0.0.66:9000'])
  })

  it('never downgrades an https endpoint to http', () => {
    const [s] = aggregatePeers([
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [legit] },
      { finder: { kind: 'browser', label: 'Browser' }, peers: [peer({ endpoints: { s3: 'http://nas.lan:9000', admin: '' }, addresses: ['https://nas.lan:9000'], lastSeen: '2026-10-09T15:00:00Z', name: 'renamed' })] }
    ])
    expect(s!.conflict).toBeUndefined()
    expect(s!.name).toBe('renamed')
    expect(s!.endpoints.s3).toBe('https://nas.lan:9000')
  })

  it('offers credentials only from a server whose own record lists the prefilled address', () => {
    const [s] = aggregatePeers([
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [legit] },
      { finder: { kind: 'browser', label: 'Browser' }, peers: [legit] }
    ])
    expect(credentialsSourceFor(s!, 'https://nas.lan:9000/', () => true)).toBe('a')
    expect(credentialsSourceFor(s!, 'http://10.0.0.66:9000', () => true)).toBeUndefined()
    expect(credentialsSourceFor(s!, 'https://nas.lan:9000', () => false)).toBeUndefined()
  })
})

describe('aggregatePeers: server-reported data and malformed documents', () => {
  it('never lets a local or browser record replace endpoints a connected server reported', () => {
    const fromA = peer({ endpoints: { s3: 'http://10.0.0.5:9000', admin: '', all: ['http://10.0.0.5:9000'] } })
    const probe = peer({ endpoints: { s3: 'http://lookalike.example:9000', admin: '' }, lastSeen: '2026-10-09T13:00:00Z', name: 'renamed' })
    const [s] = aggregatePeers([
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [fromA] },
      { finder: { kind: 'browser', label: 'Browser' }, peers: [probe] },
      { finder: { kind: 'local', label: 'UI host' }, peers: [{ ...probe, lastSeen: '2026-10-09T14:00:00Z' }] }
    ])
    expect(s!.endpoints.s3).toBe('http://10.0.0.5:9000')
    expect(s!.conflict).toBeUndefined()
    // Without a server record, the fresher probe's document is used as before.
    const [t] = aggregatePeers([
      { finder: { kind: 'local', label: 'UI host' }, peers: [fromA] },
      { finder: { kind: 'browser', label: 'Browser' }, peers: [probe] }
    ])
    expect(t!.endpoints.s3).toBe('http://lookalike.example:9000')
  })

  it('keeps an unverified server-reported record over a verified probe from another address', () => {
    const [s] = aggregatePeers([
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [peer({ verified: false, healthy: false })] },
      { finder: { kind: 'browser', label: 'Browser' }, peers: [peer({ endpoints: { s3: 'http://10.0.0.66:9000', admin: '' }, addresses: ['http://10.0.0.66:9000'] })] }
    ])
    expect(s!.addresses).toEqual(['http://10.0.0.5:9000'])
    expect(s!.conflict).toBe(true)
    expect(s!.conflictAddresses).toEqual(['http://10.0.0.66:9000'])
  })

  it('ignores malformed fields instead of throwing', () => {
    const bad = {
      ...peer({}),
      endpoints: { s3: 42, admin: null, all: 'http://x:9000' },
      features: null,
      sources: 'mdns',
      addresses: { 0: 'http://10.0.0.5:9000' },
      name: ['x']
    } as unknown as Peer
    const servers = aggregatePeers([
      { finder: { kind: 'server', label: 'A', connectionId: 'a' }, peers: [peer({}), bad] },
      { finder: { kind: 'local', label: 'UI host' }, peers: [bad, null as unknown as Peer, { id: 7 } as unknown as Peer] }
    ])
    expect(servers).toHaveLength(1)
    expect(servers[0]!.addresses).toEqual(['http://10.0.0.5:9000'])
    expect(serverUrls({ endpoints: { s3: '', admin: '', all: 'x' as unknown as string[] }, addresses: null as unknown as string[] })).toEqual([])
    const [only] = aggregatePeers([{ finder: { kind: 'local', label: 'L' }, peers: [bad] }])
    expect(only).toMatchObject({ name: 'p1', features: [], addresses: [], endpoints: { s3: '', admin: '' } })
  })
})
