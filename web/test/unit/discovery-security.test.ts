// Security audit fixes of the standalone UI server (server/**): sender
// binding and identity conflicts of local discovery (web-nitro-1), bounded
// identity documents and linear merges (web-nitro-2, -v1, -3), beacon
// candidates that only point at the sender (web-nitro-4), the rebinding
// guard behind proxies and in older browsers (web-nitro-5, -8), the
// metadata deny-list (web-nitro-7), the console Host check, the icon API
// block and the security headers.
import { createHash } from 'node:crypto'
import { EventEmitter } from 'node:events'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { Info, Peer } from '~/types/admin'
import { beaconCandidateUrls } from '~~/server/utils/discovery/beacon'
import { hasConfiguredPresets, isConsoleHost, isPrivateName, rejectForeignRequest, requestHosts } from '~~/server/utils/discovery/http'
import { MAX_BEACON_CANDIDATES_PER_SOURCE, queryBeacon, verifyCandidates, type Candidate } from '~~/server/utils/discovery/local'
import { MDNS_MAX_PER_SOURCE, parseMdnsRecords, type MdnsRecord } from '~~/server/utils/discovery/mdns'
import { blockedUrlHost, isBlockedAddress } from '~~/server/utils/discovery/netguard'
import { canonicalIp, LocalNets } from '~~/server/utils/discovery/netlocal'
import { MAX_ADDRESSES, MAX_FEATURES, mergePeers, parseInfo, peerFromInfo } from '~~/server/utils/discovery/peers'
import { fetchInfo, MAX_INFO_BYTES } from '~~/server/utils/discovery/probe'
import { runScan } from '~~/server/utils/discovery/scan'
import { planScan } from '~~/server/utils/discovery/targets'
import { contentSecurityPolicy, inlineScriptHashes, SECURITY_HEADERS } from '~~/server/utils/csp'
import { presetSecretPaths, presetSecretsAllowed } from '~~/server/utils/preset-secrets'
import { presetsFileResponse } from '~~/server/utils/presets-file'

// queryBeacon's sockets: every bind fails with a raw OS error.
vi.mock('node:dgram', () => ({
  createSocket: () => {
    const s = new EventEmitter() as EventEmitter & Record<string, unknown>
    s.bind = (_port: number) => {
      queueMicrotask(() => s.emit('error', new Error('bind EADDRINUSE 10.0.0.20:7373 (eth0)')))
    }
    s.close = () => {}
    return s
  }
}))

const ID = 'b3f1c2d4e5f60718'
const GENUINE: Info = {
  service: 'gos3',
  id: ID,
  name: 'NAS',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.0.0.5:9000', admin: 'http://10.0.0.5:9000/_gos3/admin/v1', all: ['http://10.0.0.5:9000'] },
  features: ['admin'],
  startedAt: '2026-10-09T12:00:00Z'
}
const EVIL: Info = { ...GENUINE, endpoints: { s3: 'http://10.0.0.66:9000', admin: 'http://10.0.0.66:9000/_gos3/admin/v1', all: ['http://10.0.0.66:9000'] } }

/** A host with 10.0.0.20/24 and loopback. */
const nets = new LocalNets(() => ({
  eth0: [{ address: '10.0.0.20', netmask: '255.255.255.0', family: 'IPv4', mac: '00:00:00:00:00:00', internal: false, cidr: '10.0.0.20/24' }],
  lo: [{ address: '127.0.0.1', netmask: '255.0.0.0', family: 'IPv4', mac: '00:00:00:00:00:00', internal: true, cidr: '127.0.0.1/8' }]
}))

function infoFetch(byHost: Record<string, unknown>) {
  return vi.fn(async (url: string) => {
    const host = new URL(url).host
    if (host in byHost) return new Response(JSON.stringify(byHost[host]))
    throw new TypeError('fetch failed')
  })
}

afterEach(() => {
  vi.unstubAllEnvs()
})

describe('web-nitro-1: local discovery binds announcements to the sender', () => {
  it('keeps only addresses the sender may advertise and never resolves the SRV target', () => {
    const records: MdnsRecord[] = [
      { name: '_gos3._tcp.local', type: 'PTR', data: 'nas._gos3._tcp.local' },
      { name: 'nas._gos3._tcp.local', type: 'SRV', data: { target: 'nas.local', port: 9000 } },
      { name: 'nas._gos3._tcp.local', type: 'TXT', data: [`id=${ID}`, 'name=NAS'] },
      // an injected record for another host's name, and loopback
      { name: 'nas.local', type: 'A', data: '10.0.0.66' },
      { name: 'nas.local', type: 'A', data: '127.0.0.1' },
      { name: 'nas.local', type: 'A', data: '10.0.0.5' }
    ]
    expect(parseMdnsRecords(records, '10.0.0.5', nets).map(c => c.urls)).toEqual([['http://10.0.0.5:9000']])
    // the attacker's own response only ever yields its own address
    expect(parseMdnsRecords(records, '10.0.0.66', nets).map(c => c.urls)).toEqual([['http://10.0.0.66:9000']])
    // a sender on this host may advertise loopback
    expect(parseMdnsRecords(records, '10.0.0.20', nets).map(c => c.urls)).toEqual([['http://127.0.0.1:9000']])
    // no accepted address: no candidate (not the SRV target name)
    const srvOnly = records.slice(0, 3).concat({ name: 'x._gos3._tcp.local', type: 'SRV', data: { target: 'internal-db.corp', port: 6379 } }, { name: 'x._gos3._tcp.local', type: 'TXT', data: ['id=x'] })
    expect(parseMdnsRecords(srvOnly, '10.0.0.66', nets)).toEqual([])
  })

  it('drops instances with an invalid TXT id and bounds instances per sender', () => {
    const bad: MdnsRecord[] = [
      { name: 'x._gos3._tcp.local', type: 'SRV', data: { target: 'x.local', port: 9000 } },
      { name: 'x._gos3._tcp.local', type: 'TXT', data: ['id=../../x y'] },
      { name: 'x.local', type: 'A', data: '10.0.0.66' }
    ]
    expect(parseMdnsRecords(bad, '10.0.0.66', nets)).toEqual([])
    const many: MdnsRecord[] = [{ name: 'h.local', type: 'A', data: '10.0.0.66' }]
    for (let i = 0; i < 100; i++) {
      many.push({ name: `i${i}._gos3._tcp.local`, type: 'SRV', data: { target: 'h.local', port: 9000 + i } }, { name: `i${i}._gos3._tcp.local`, type: 'TXT', data: [`id=i${i}`] })
    }
    expect(parseMdnsRecords(many, '10.0.0.66', nets)).toHaveLength(MDNS_MAX_PER_SOURCE)
  })

  it('accepts datagrams only from on-link senders and checks advertised addresses', () => {
    expect(nets.onLink('10.0.0.66')).toBe(true)
    expect(nets.onLink('127.0.0.1')).toBe(true)
    expect(nets.onLink('192.168.1.5')).toBe(false) // routed
    expect(nets.onLink('10.0.0.255')).toBe(false) // directed broadcast
    expect(nets.onLink('224.0.0.251')).toBe(false)
    expect(nets.isLocal('10.0.0.20')).toBe(true)
    expect(nets.isLocal('::ffff:10.0.0.20')).toBe(true)
    expect(nets.advertisedBy('10.0.0.5', '10.0.0.5')).toBe(true)
    expect(nets.advertisedBy('10.0.0.5', '10.0.0.66')).toBe(false)
    expect(nets.advertisedBy('127.0.0.1', '10.0.0.66')).toBe(false)
    expect(nets.advertisedBy('127.0.0.1', '10.0.0.20')).toBe(true)
    // fail closed when the interfaces cannot be listed: only loopback
    const blind = new LocalNets(() => {
      throw new Error('no interfaces')
    })
    expect(blind.onLink('10.0.0.66')).toBe(false)
    expect(blind.onLink('127.0.0.1')).toBe(true)
    expect(canonicalIp('[::FFFF:7f00:1]')).toBe('127.0.0.1')
    expect(canonicalIp('FD00:0:0::5')).toBe('fd00::5')
  })

  it('never lets a later claimant take over a verified server (merge)', () => {
    const now = new Date('2026-10-12T12:00:00Z')
    const genuine = peerFromInfo(GENUINE, 'http://10.0.0.5:9000', 'mdns', now)
    const evil = peerFromInfo(EVIL, 'http://10.0.0.66:9000', 'beacon', now)
    const [m] = mergePeers([genuine, evil])
    expect(m!.endpoints.s3).toBe('http://10.0.0.5:9000')
    expect(m!.name).toBe('NAS')
    expect(m!.addresses).toEqual(['http://10.0.0.5:9000'])
    expect(m!.sources).toEqual(['mdns'])
    expect(m!.error).toMatch(/ID claimed by another address: http:\/\/10\.0\.0\.66:9000/)
    // a copy of the genuine document from another address changes nothing
    const copy = peerFromInfo(GENUINE, 'http://10.0.0.66:9000', 'beacon', now)
    expect(mergePeers([genuine, copy])[0]).toEqual(genuine)
  })

  it('merges only on a shared address or a mutual claim, never a one-sided one', () => {
    const now = new Date('2026-10-12T12:00:00Z')
    const both = ['http://10.0.0.66:9000', 'http://10.0.0.5:9000']
    // a rogue that answered first and advertises the real server's address
    const rogue = peerFromInfo({ ...EVIL, endpoints: { ...EVIL.endpoints, all: both } }, 'http://10.0.0.66:9000', 'mdns', now)
    const genuine = peerFromInfo(GENUINE, 'http://10.0.0.5:9000', 'beacon', now)
    const [m] = mergePeers([rogue, genuine])
    expect(m!.addresses).toEqual(['http://10.0.0.66:9000'])
    expect(m!.error).toMatch(/claimed by another address: http:\/\/10\.0\.0\.5:9000/)
    // a multi-homed server whose documents advertise each other's address
    const multi: Info = { ...GENUINE, endpoints: { ...GENUINE.endpoints, all: ['http://10.0.0.5:9000', 'http://10.0.0.6:9000'] } }
    const a = peerFromInfo(multi, 'http://10.0.0.5:9000', 'mdns', now)
    const b = peerFromInfo(multi, 'http://10.0.0.6:9000', 'beacon', now)
    expect(mergePeers([a, b])[0]).toMatchObject({ addresses: ['http://10.0.0.5:9000', 'http://10.0.0.6:9000'], sources: ['mdns', 'beacon'] })
    expect(mergePeers([a, b])[0]!.error).toBeUndefined()
    // the same address found twice (mDNS and beacon)
    expect(mergePeers([genuine, { ...genuine, sources: ['scan'] }])[0]!.sources).toEqual(['beacon', 'scan'])
  })

  it('verifyCandidates keeps the first verified server when a beacon sender claims its ID', async () => {
    const fetch = infoFetch({ '10.0.0.5:9000': GENUINE, '10.0.0.66:9000': EVIL })
    const candidates: Candidate[] = [
      { url: 'http://10.0.0.5:9000', source: 'mdns', id: ID },
      { url: 'http://10.0.0.5:9000', source: 'beacon', id: ID },
      ...beaconCandidateUrls(['http://10.0.0.5:9000', 'http://:9000'], '10.0.0.66', ip => nets.isLocal(ip)).map(url => ({ url, source: 'beacon' as const, id: ID }))
    ]
    const peers = await verifyCandidates(candidates, { fetch })
    expect(peers).toHaveLength(1)
    expect(peers[0]).toMatchObject({ id: ID, name: 'NAS', endpoints: { s3: 'http://10.0.0.5:9000' }, addresses: ['http://10.0.0.5:9000'], sources: ['mdns', 'beacon'] })
    expect(peers[0]!.error).toMatch(/claimed by another address/)
  })
})

describe('web-nitro-4: beacon candidates only ever point at the sender', () => {
  it('replaces every announced host (names, loopback spellings, other IPs) and drops paths', () => {
    const isLocal = (ip: string) => nets.isLocal(ip)
    expect(beaconCandidateUrls([
      'http://localhost.:6379',
      'http://[::ffff:127.0.0.1]:2375',
      'http://10.9.9.9:8080/admin',
      'http://lvh.me:8500',
      'http://10.0.0.66:9000/p1',
      'http://10.0.0.66:9000/p2'
    ], '10.0.0.66', isLocal)).toEqual(['http://10.0.0.66:6379', 'http://10.0.0.66:2375', 'http://10.0.0.66:8080', 'http://10.0.0.66:8500', 'http://10.0.0.66:9000'])
    // this host may announce its loopback and local addresses
    expect(beaconCandidateUrls(['http://[::ffff:127.0.0.1]:9000', 'http://10.0.0.20:9001'], '10.0.0.20', isLocal)).toEqual(['http://127.0.0.1:9000', 'http://10.0.0.20:9001'])
    // blocked or unusable sources yield nothing
    expect(beaconCandidateUrls(['http://x:9000'], '0.0.0.0', isLocal)).toEqual([])
    expect(beaconCandidateUrls(['http://x:9000'], '169.254.169.254', isLocal)).toEqual([])
  })

  it('caps the candidates one sender can add per discovery', () => {
    expect(MAX_BEACON_CANDIDATES_PER_SOURCE).toBeLessThanOrEqual(16)
    const urls = Array.from({ length: 64 }, (_, i) => `http://h:${9000 + i}`)
    expect(beaconCandidateUrls(urls, '10.0.0.66', () => false)).toHaveLength(16)
  })
})

describe('web-nitro-2 / v1 / 3: identity documents are validated, bounded and merged linearly', () => {
  it('parseInfo drops malformed fields and bounds strings and lists', () => {
    const info = parseInfo({
      service: 'gos3',
      id: ID,
      name: `x‮${'n'.repeat(1000)}`,
      features: [{}, 1, 'versioning', 'versioning', 'f'.repeat(100), ...Array.from({ length: 200 }, (_, i) => `f${i}`)],
      endpoints: { s3: 'javascript:alert(1)', admin: '/_gos3/admin/v1', ui: 5, all: ['http://a:1', { x: 1 }, '//evil'] },
      extra: { big: 'x'.repeat(1000) }
    })!
    expect(info.name).toHaveLength(256)
    expect(info.name.startsWith('xn')).toBe(true)
    expect(info.features).toHaveLength(MAX_FEATURES)
    expect(info.features[0]).toBe('versioning')
    expect(info.features[1]).toHaveLength(64)
    expect(info.endpoints).toEqual({ s3: '', admin: '/_gos3/admin/v1', all: ['http://a:1'] })
    expect(info).not.toHaveProperty('extra')
    for (const bad of [{ service: 'gos3', id: '../../x y', name: 'n' }, { service: 'gos3', id: ID }, { service: 'minio', id: ID, name: 'n' }, [], 'x']) {
      expect(parseInfo(bad), JSON.stringify(bad)).toBeUndefined()
    }
    // a missing or invalid s3 endpoint falls back to the probed address
    expect(peerFromInfo(info, 'http://10.0.0.5:9000', 'scan').endpoints).toMatchObject({ s3: 'http://10.0.0.5:9000', admin: '/_gos3/admin/v1' })
  })

  it('refuses documents over MAX_INFO_BYTES', async () => {
    const big = JSON.stringify({ ...GENUINE, features: Array.from({ length: 6000 }, () => ({})) })
    expect(big.length).toBeGreaterThan(MAX_INFO_BYTES)
    await expect(fetchInfo('http://h', { fetch: async () => new Response(big) })).rejects.toThrow('not a gos3 server')
  })

  it('a malformed document on several ports does not fail the scan', async () => {
    const odd = { service: 'gos3', id: 'x', name: 'n', features: 1, endpoints: { all: 7 } }
    const fetch = infoFetch({ '10.0.0.7:9000': odd, '10.0.0.7:9001': odd, '10.0.0.5:9000': GENUINE })
    const r = await runScan(planScan({ targets: ['10.0.0.7', '10.0.0.5'], ports: [9000, 9001] }), { fetch })
    expect(r.found!.map(p => p.id).sort()).toEqual([ID, 'x'])
    expect(r.found!.find(p => p.id === 'x')!.features).toEqual([])
  })

  it('merging many observations of one ID is fast and bounded', async () => {
    // 256 path-distinct URLs on one host, each serving a document full of features.
    const doc = { ...GENUINE, endpoints: { s3: 'http://10.0.0.66:9000', admin: '', all: Array.from({ length: 64 }, (_, i) => `http://10.0.0.66:${9000 + i}`) }, features: Array.from({ length: 1400 }, (_, i) => `f${i}`) }
    const body = JSON.stringify(doc)
    expect(body.length).toBeLessThan(MAX_INFO_BYTES)
    const fetch = vi.fn(async () => new Response(body))
    const candidates: Candidate[] = Array.from({ length: 256 }, (_, i) => ({ url: `http://10.0.0.66:${9000 + (i % 64)}`, source: i % 2 ? 'beacon' : 'mdns', id: ID }))
    const t0 = Date.now()
    const peers = await verifyCandidates(candidates, { fetch })
    expect(Date.now() - t0).toBeLessThan(2000)
    expect(peers).toHaveLength(1)
    expect(peers[0]!.features!.length).toBeLessThanOrEqual(MAX_FEATURES)
    expect(peers[0]!.addresses.length).toBeLessThanOrEqual(MAX_ADDRESSES)
    // non-array inputs from older callers do not throw
    const p = peerFromInfo(GENUINE, 'http://10.0.0.5:9000', 'mdns')
    expect(() => mergePeers([{ ...p, features: 1 as unknown as string[] }, p] as Peer[])).not.toThrow()
  })
})

describe('web-nitro-7: metadata deny-list', () => {
  it('blocks GCP IPv6, Tencent, Azure WireServer, OCI and local-use NAT64 forms', () => {
    for (const a of ['fd00:ec2::23', 'fd20:ce::254', '169.254.0.23', '168.63.129.16', '192.0.0.192', '64:ff9b:1::a9fe:a9fe', 'fd00:ec2::254', '169.254.170.2', '100.100.100.200']) {
      expect(isBlockedAddress(a), a).toBe(true)
    }
    expect(blockedUrlHost('http://[fd20:ce::254]')).toBe('fd20:ce::254')
    expect(blockedUrlHost('http://[64:ff9b:1::a9fe:a9fe]:80/x')).toBeDefined()
    expect(isBlockedAddress('64:ff9b:1::a00:5')).toBe(false)
    expect(isBlockedAddress('169.254.10.10')).toBe(false)
  })
})

describe('web-nitro-5 / 8: rebinding guard behind proxies and in older browsers', () => {
  const status: number[] = []
  const event = (headers: Record<string, string>) => {
    vi.stubGlobal('getRequestHeader', (_e: unknown, name: string) => headers[name])
    vi.stubGlobal('setResponseStatus', (_e: unknown, s: number) => status.push(s))
    vi.stubGlobal('setResponseHeader', () => {})
    return {} as Parameters<typeof rejectForeignRequest>[0]
  }

  it('checks X-Forwarded-Host and Forwarded host= as well as Host', () => {
    expect(requestHosts(event({ 'host': '127.0.0.1:3000', 'x-forwarded-host': 'a.example, b.example', 'forwarded': 'for=1.2.3.4;host="c.example:8080", host=d.example' })))
      .toEqual(['127.0.0.1:3000', 'a.example', 'b.example', 'c.example:8080', 'd.example'])
    // DNS rebinding through a proxy that rewrites Host but forwards the name
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'x-forwarded-host': 'attacker.example:3000', 'sec-fetch-site': 'same-origin' }))?.error.code).toBe('AccessDenied')
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'forwarded': 'host=attacker.example', 'sec-fetch-site': 'same-origin' }))?.error.code).toBe('AccessDenied')
    // a proxied same-origin POST whose Origin matches the (allowed) forwarded host
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'x-forwarded-host': '10.0.0.20:8080', 'origin': 'http://10.0.0.20:8080', 'sec-fetch-site': 'same-origin' }))).toBeUndefined()
    // a spoofed X-Forwarded-Host does not make a foreign Origin same-origin
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'x-forwarded-host': '127.0.0.1:3000', 'origin': 'http://attacker.example:3000' }))?.error.code).toBe('AccessDenied')
  })

  it('GET /local refuses browser requests without Sec-Fetch-Site unless they carry X-Gos3-Discovery', () => {
    const ua = 'Mozilla/5.0 (Macintosh) AppleWebKit/605.1.15 Version/15.6 Safari/605.1.15'
    const opts = { browserSignal: true }
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'user-agent': ua }), opts)?.error.code).toBe('AccessDenied')
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'user-agent': ua, 'x-gos3-discovery': '1' }), opts)).toBeUndefined()
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'user-agent': ua, 'sec-fetch-site': 'same-origin' }), opts)).toBeUndefined()
    // non-browser clients (health check, curl) are unaffected, and so are the POST routes
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'user-agent': 'curl/8.0' }), opts)).toBeUndefined()
    expect(rejectForeignRequest(event({ host: '127.0.0.1:3000' }), opts)).toBeUndefined()
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'user-agent': ua }))).toBeUndefined()
  })

  it('reports socket errors with fixed texts only', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const r = await queryBeacon(500, AbortSignal.abort(), '239.255.73.83', 7373, nets)
    expect(r.errors).toEqual(['beacon: socket error'])
    expect(r.errors.join()).not.toMatch(/EADDRINUSE|10\.0\.0\.20|eth0/)
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('EADDRINUSE'))
  })
})

describe('console Host check (presets) and the icon API', () => {
  interface Ev { path: string, headers: Record<string, string>, status: number }
  async function middleware(name: '00-allowed-hosts' | '01-no-icon-api', servers: unknown = '') {
    vi.stubGlobal('defineEventHandler', (h: unknown) => h)
    vi.stubGlobal('getRequestHeader', (e: Ev, n: string) => e.headers[n])
    vi.stubGlobal('setResponseStatus', (e: Ev, s: number) => {
      e.status = s
    })
    vi.stubGlobal('setResponseHeader', () => {})
    vi.stubGlobal('useRuntimeConfig', () => ({ public: { servers } }))
    vi.spyOn(console, 'warn').mockImplementation(() => {})
    return (await import(`~~/server/middleware/${name}.ts`)).default as (e: Ev) => unknown
  }
  const ev = (path: string, headers: Record<string, string>): Ev => ({ path, headers, status: 200 })

  it('classifies host names', () => {
    for (const h of ['10.0.0.20:3000', '[::1]:3000', 'localhost', 'nas', 'nas.local', 'ui.home.arpa', 'console.lan:3000', 'x.internal', undefined]) expect(isConsoleHost(h, []), String(h)).toBe(true)
    for (const h of ['attacker.example', 'nas.local.attacker.example', 'ui.example.com:3000']) expect(isConsoleHost(h, []), h).toBe(false)
    expect(isConsoleHost('ui.example.com', ['ui.example.com'])).toBe(true)
    expect(isPrivateName('a.lan.')).toBe(true)
    expect(hasConfiguredPresets('')).toBe(false)
    expect(hasConfiguredPresets(' [] ')).toBe(false)
    expect(hasConfiguredPresets('[{"endpoint":"http://10.0.0.5:9000"}]')).toBe(true)
    expect(hasConfiguredPresets([{ endpoint: 'x' }])).toBe(true)
  })

  it('answers 421 for a rebinding Host when presets are configured', async () => {
    const mw = await middleware('00-allowed-hosts', '[{"name":"NAS","endpoint":"http://10.0.0.5:9000"}]')
    const bad = ev('/', { host: 'attacker.example:3000' })
    expect(mw(bad)).toMatch(/GOS3_UI_ALLOWED_HOSTS/)
    expect(bad.status).toBe(421)
    const proxied = ev('/', { 'host': '127.0.0.1:3000', 'x-forwarded-host': 'attacker.example' })
    mw(proxied)
    expect(proxied.status).toBe(421)
    for (const host of ['10.0.0.20:3000', 'localhost:3000', 'nas.lan:3000']) {
      const ok = ev('/servers/x', { host })
      expect(mw(ok), host).toBeUndefined()
      expect(ok.status).toBe(200)
    }
    vi.stubEnv('GOS3_UI_ALLOWED_HOSTS', 'attacker.example')
    expect(mw(ev('/', { host: 'attacker.example:3000' }))).toBeUndefined()
  })

  it('without presets serves any Host, except servers.json', async () => {
    const mw = await middleware('00-allowed-hosts', '')
    expect(mw(ev('/', { host: 'ui.example.com' }))).toBeUndefined()
    const file = ev('/servers.json', { host: 'attacker.example' })
    mw(file)
    expect(file.status).toBe(421)
  })

  it('refuses the @nuxt/icon server endpoint', async () => {
    const mw = await middleware('01-no-icon-api')
    const e = ev('/api/_nuxt_icon/lucide.json?icons=trash,x1', { host: 'localhost' })
    expect(mw(e)).toBe('')
    expect(e.status).toBe(404)
    expect(mw(ev('/api/discovery/capabilities', { host: 'localhost' }))).toBeUndefined()
  })
})

describe('web-auth-3 / web-nitro-6 / web-content-3: security headers', () => {
  it('match the embedded UI and forbid framing', () => {
    expect(SECURITY_HEADERS).toMatchObject({ 'X-Frame-Options': 'DENY', 'Referrer-Policy': 'no-referrer', 'X-Content-Type-Options': 'nosniff' })
    expect(SECURITY_HEADERS['Content-Security-Policy']).toContain("frame-ancestors 'none'")
    expect(SECURITY_HEADERS['Content-Security-Policy']).toContain("script-src 'self';")
    expect(SECURITY_HEADERS['Permissions-Policy']).toContain('camera=()')
  })

  it('hashes exactly the executable inline scripts of a page', () => {
    const html = '<script>window.__NUXT__={}</script><script type="module" src="/_nuxt/a.js"></script>'
      + '<script type="application/json" id="d">{}</script><script type=module>import "x"</script>'
    const hashes = inlineScriptHashes(html)
    expect(hashes).toHaveLength(2)
    expect(hashes).toContain(`sha256-${createHash('sha256').update('window.__NUXT__={}').digest('base64')}`)
    // the HTML parser normalises CRLF and CR to LF before hashing
    const lf = `sha256-${createHash('sha256').update('a()\nb()\nc()').digest('base64')}`
    expect(inlineScriptHashes('<script>a()\r\nb()\rc()</script>')).toEqual([lf])
    const csp = contentSecurityPolicy(hashes)
    expect(csp).toMatch(/^default-src 'self'; script-src 'self' 'sha256-[^']+' 'sha256-[^']+'; style-src/)
    expect(csp).not.toContain('unsafe-eval')
  })
})

describe('servers.json of the standalone build', () => {
  it('serves the build-time file as JSON, or 404 without one', () => {
    const e = { status: 200, headers: {} as Record<string, string> }
    vi.stubGlobal('setResponseStatus', (ev: typeof e, s: number) => {
      ev.status = s
    })
    vi.stubGlobal('setResponseHeader', (ev: typeof e, k: string, v: string) => {
      ev.headers[k] = v
    })
    expect(presetsFileResponse(e as never, '{"servers":[]}')).toBe('{"servers":[]}')
    expect(e.headers['content-type']).toMatch(/^application\/json/)
    expect(presetsFileResponse(e as never, null)).toBe('')
    expect(e.status).toBe(404)
  })
})

describe('preset secret keys', () => {
  it('finds non-empty secret key fields in NUXT_PUBLIC_SERVERS and servers.json', () => {
    expect(presetSecretPaths('[{"name":"NAS","endpoint":"http://10.0.0.5:9000","accessKeyId":"AK"}]')).toEqual([])
    expect(presetSecretPaths('[{"name":"NAS","secretAccessKey":"s"}]')).toEqual(['[0].secretAccessKey'])
    expect(presetSecretPaths('{"servers":[{"secretKey":"s"},{"secretKey":""}]}')).toEqual(['servers[0].secretKey'])
    expect(presetSecretPaths([{ secretAccessKey: 'x' }])).toEqual(['[0].secretAccessKey'])
    expect(presetSecretPaths('{"secretKey": "x", broken')).toEqual(['secretKey'])
    expect(presetSecretPaths('')).toEqual([])
    expect(presetSecretsAllowed({})).toBe(false)
    expect(presetSecretsAllowed({ GOS3_UI_ALLOW_PRESET_SECRETS: 'true' })).toBe(true)
  })
})
