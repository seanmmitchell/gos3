// Pure helpers of the standalone UI's local discovery routes (server/utils/discovery).
import { describe, expect, it, vi } from 'vitest'
import type { Info, Peer } from '~/types/admin'
import { beaconCandidateUrls, BEACON_PROTO, decodeBeacon, encodeQuery } from '~~/server/utils/discovery/beacon'
import { decodeTxt, missingMdnsNames, parseMdnsRecords, type MdnsRecord } from '~~/server/utils/discovery/mdns'
import { isInfo, mergePeer, mergePeers, peerFromInfo, peerFromSighting } from '~~/server/utils/discovery/peers'
import { fetchInfo, runPool } from '~~/server/utils/discovery/probe'
import { runScan } from '~~/server/utils/discovery/scan'
import {
  baseUrl,
  cidrHostCount,
  expandCidr,
  formatGoDuration,
  normalizeBaseUrl,
  parseCidr,
  parseIPv4,
  parseScanTarget,
  planScan,
  ScanRequestError
} from '~~/server/utils/discovery/targets'
import { isAllowedHost, hostnameOf, allowedHostsFromEnv, rejectForeignRequest } from '~~/server/utils/discovery/http'
import { localDiscoveryEnabled, MAX_VERIFY_URLS, verifyCandidates, type Candidate } from '~~/server/utils/discovery/local'

const INFO: Info = {
  service: 'gos3',
  id: 'b3f1c2d4e5f60718',
  name: 'nas',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.0.0.5:9000', admin: 'http://10.0.0.5:9000/_gos3/admin/v1' },
  features: ['admin'],
  startedAt: '2026-10-09T12:00:00Z'
}

describe('IPv4 / CIDR', () => {
  it('parses and rejects addresses', () => {
    expect(parseIPv4('10.0.0.1')).toBe(0x0a000001)
    expect(parseIPv4('255.255.255.255')).toBe(0xffffffff)
    expect(parseIPv4('256.0.0.1')).toBeUndefined()
    expect(parseIPv4('01.0.0.1')).toBeUndefined()
    expect(parseIPv4('1.2.3')).toBeUndefined()
  })

  it('masks CIDRs and counts hosts', () => {
    expect(parseCidr('10.0.0.77/24')).toEqual({ base: 0x0a000000, bits: 24 })
    expect(parseCidr('10.0.0.0/33')).toBeUndefined()
    expect(cidrHostCount(24)).toBe(254)
    expect(cidrHostCount(30)).toBe(2)
    expect(cidrHostCount(31)).toBe(2)
    expect(cidrHostCount(32)).toBe(1)
  })

  it('expands CIDRs skipping network and broadcast up to /30', () => {
    const hosts = expandCidr('192.168.1.0/24')
    expect(hosts).toHaveLength(254)
    expect(hosts[0]).toBe('192.168.1.1')
    expect(hosts.at(-1)).toBe('192.168.1.254')
    expect(expandCidr('10.0.0.4/30')).toEqual(['10.0.0.5', '10.0.0.6'])
    expect(expandCidr('10.0.0.4/31')).toEqual(['10.0.0.4', '10.0.0.5'])
    expect(expandCidr('10.0.0.9/32')).toEqual(['10.0.0.9'])
  })

  it('enforces the host cap', () => {
    expect(expandCidr('10.0.0.0/22')).toHaveLength(1022)
    expect(() => expandCidr('10.0.0.0/21')).toThrow(ScanRequestError)
    expect(() => expandCidr('10.0.0.0/8')).toThrow(/limit is 1024/)
  })
})

describe('scan targets', () => {
  it('classifies targets', () => {
    expect(parseScanTarget('http://NAS.local:9000/')).toEqual({ kind: 'url', url: 'http://nas.local:9000' })
    expect(parseScanTarget('10.0.0.0/24')).toMatchObject({ kind: 'cidr' })
    expect(parseScanTarget('10.0.0.5')).toEqual({ kind: 'host', host: '10.0.0.5' })
    expect(parseScanTarget('10.0.0.5:9001')).toEqual({ kind: 'host', host: '10.0.0.5', port: 9001 })
    expect(parseScanTarget('fe80::1')).toEqual({ kind: 'host', host: 'fe80::1' })
    expect(parseScanTarget('[::1]:9000')).toEqual({ kind: 'host', host: '::1', port: 9000 })
    expect(parseScanTarget('Nas.Local.')).toEqual({ kind: 'host', host: 'nas.local', port: undefined })
  })

  it('rejects invalid targets', () => {
    expect(() => parseScanTarget('ftp://x')).toThrow(/only http and https/)
    expect(() => parseScanTarget('10.0.0.0/40')).toThrow(/not a valid CIDR/)
    expect(() => parseScanTarget('fd00::/64')).toThrow(/IPv6 CIDRs/)
    expect(() => parseScanTarget('host:99999')).toThrow(/invalid port/)
    expect(() => parseScanTarget('bad host!')).toThrow(/not a host name/)
    expect(() => parseScanTarget('http://user:pw@h')).toThrow(/credentials/)
  })

  it('formats base URLs', () => {
    expect(baseUrl('http', '::1', 9000)).toBe('http://[::1]:9000')
    expect(baseUrl('https', '10.0.0.1', 443)).toBe('https://10.0.0.1:443')
    expect(normalizeBaseUrl('nas:9000/')).toBe('http://nas:9000')
  })

  it('plans scans with defaults and dedup', () => {
    const p = planScan({ targets: ['10.0.0.4/30', '10.0.0.5', 'http://x:1', 'nas:9001'], schemes: ['http', 'HTTPS'] })
    expect(p.urls).toEqual([
      'http://10.0.0.5:9000', 'https://10.0.0.5:9000',
      'http://10.0.0.6:9000', 'https://10.0.0.6:9000',
      'http://x:1',
      'http://nas:9001', 'https://nas:9001'
    ])
    expect(p.hosts).toBe(5)
    expect(p.timeoutMs).toBe(1500)
    expect(p.concurrency).toBe(64)
  })

  it('converts the timeout from nanoseconds and clamps concurrency', () => {
    const p = planScan({ targets: ['h'], ports: [9000, 9000, 80], timeout: 500_000_000, concurrency: 1000 })
    expect(p.timeoutMs).toBe(500)
    expect(p.concurrency).toBe(64)
    expect(p.urls).toEqual(['http://h:9000', 'http://h:80'])
    expect(planScan({ targets: ['h'], timeout: 1e12 }).timeoutMs).toBe(30000)
  })

  it('validates request bodies', () => {
    expect(() => planScan(null)).toThrow(/JSON object/)
    expect(() => planScan({ targets: [] })).toThrow(/no targets/)
    expect(() => planScan({ targets: [1] })).toThrow(/strings/)
    expect(() => planScan({ targets: ['h'], ports: [0] })).toThrow(/invalid port/)
    expect(() => planScan({ targets: ['h'], schemes: ['ftp'] })).toThrow(/unsupported scheme/)
    expect(() => planScan({ targets: ['10.0.0.0/22', '10.0.4.0/23'] })).toThrow(/more than 1024 hosts/)
    expect(() => planScan({ targets: ['10.0.0.0/22'], ports: [1, 2, 3, 4, 5] })).toThrow(/probes/)
  })

  it('formats elapsed like Go durations', () => {
    expect(formatGoDuration(850)).toBe('850ms')
    expect(formatGoDuration(1234)).toBe('1.234s')
    expect(formatGoDuration(2000)).toBe('2s')
  })
})

describe('beacon messages', () => {
  it('encodes queries', () => {
    const q = JSON.parse(encodeQuery('abc').toString('utf8'))
    expect(q).toEqual({ proto: BEACON_PROTO, type: 'query', nonce: 'abc' })
  })

  it('decodes announces and ignores junk', () => {
    const a = decodeBeacon(Buffer.from(JSON.stringify({ proto: BEACON_PROTO, type: 'announce', id: 'b3f1c2d4e5f60718', name: 'nas\u0007', urls: ['http://10.0.0.5:9000', 5], ts: 1 })))
    expect(a).toMatchObject({ type: 'announce', id: 'b3f1c2d4e5f60718', name: 'nas', urls: ['http://10.0.0.5:9000'] })
    expect(decodeBeacon('{"proto":"gos3-beacon/1","type":"bye","id":"abc"}')).toEqual({ proto: BEACON_PROTO, type: 'bye', id: 'abc' })
    expect(decodeBeacon('{"proto":"other/1","type":"announce","id":"x"}')).toBeNull()
    expect(decodeBeacon('{"proto":"gos3-beacon/1","type":"announce"}')).toBeNull()
    expect(decodeBeacon('{"proto":"gos3-beacon/1","type":"wat","id":"x"}')).toBeNull()
    expect(decodeBeacon('not json')).toBeNull()
    expect(decodeBeacon(new Uint8Array([0xff, 0xfe]))).toBeNull()
    expect(decodeBeacon(new Uint8Array(2000))).toBeNull()
  })

  it('derives candidate URLs from announces', () => {
    expect(beaconCandidateUrls(['http://10.0.0.5:9000/'], '10.0.0.5')).toEqual(['http://10.0.0.5:9000'])
    // wildcard host → source IP
    expect(beaconCandidateUrls(['http://0.0.0.0:9000'], '10.0.0.7')).toEqual(['http://10.0.0.7:9000'])
    expect(beaconCandidateUrls(['https://:9443'], '10.0.0.7')).toEqual(['https://10.0.0.7:9443'])
    // loopback announced from a remote host → source IP
    expect(beaconCandidateUrls(['http://127.0.0.1:9000'], '10.0.0.8')).toEqual(['http://10.0.0.8:9000'])
    // loopback from loopback stays
    expect(beaconCandidateUrls(['http://localhost:9000'], '127.0.0.1')).toEqual(['http://localhost:9000'])
    // nothing usable → source IP + default port
    expect(beaconCandidateUrls([], '10.0.0.9')).toEqual(['http://10.0.0.9:9000'])
    expect(beaconCandidateUrls(undefined, undefined)).toEqual([])
  })
})

describe('mDNS answers', () => {
  const records: MdnsRecord[] = [
    { name: '_gos3._tcp.local', type: 'PTR', data: 'nas-b3f1c2._gos3._tcp.local' },
    { name: '_gos3._tcp.local', type: 'PTR', data: 'other-service._http._tcp.local' },
    { name: 'nas-b3f1c2._gos3._tcp.local', type: 'SRV', data: { target: 'nas.local', port: 9000, priority: 0, weight: 0 } },
    { name: 'nas-b3f1c2._gos3._tcp.local', type: 'TXT', data: [Buffer.from('id=b3f1c2d4e5f60718'), Buffer.from('name=nas'), Buffer.from('scheme=https'), 'v=0.1.0'] },
    { name: 'nas.local', type: 'AAAA', data: 'fe80::1' },
    { name: 'nas.local', type: 'AAAA', data: 'fd00::5' },
    { name: 'nas.local', type: 'A', data: '10.0.0.5' }
  ]

  it('decodes TXT data', () => {
    expect(decodeTxt([Buffer.from('a=1'), 'B=two=2', 'flag', 'a=dup'])).toEqual({ a: '1', b: 'two=2', flag: '' })
    expect(decodeTxt(undefined)).toEqual({})
  })

  it('builds candidates from PTR → SRV/TXT/A', () => {
    const c = parseMdnsRecords(records)
    expect(c).toHaveLength(1)
    expect(c[0]).toMatchObject({
      instance: 'nas-b3f1c2._gos3._tcp.local',
      host: 'nas.local',
      port: 9000,
      scheme: 'https',
      addresses: ['10.0.0.5', 'fd00::5'],
      urls: ['https://10.0.0.5:9000', 'https://[fd00::5]:9000']
    })
    expect(c[0]!.txt.id).toBe('b3f1c2d4e5f60718')
  })

  it('falls back to the target host and reports missing records', () => {
    const partial = records.slice(0, 3)
    expect(parseMdnsRecords(partial)[0]!.urls).toEqual(['http://nas.local:9000'])
    expect(missingMdnsNames(partial)).toEqual({ srv: [], a: ['nas.local'] })
    expect(missingMdnsNames(records.slice(0, 1))).toEqual({ srv: ['nas-b3f1c2._gos3._tcp.local'], a: [] })
    expect(parseMdnsRecords(records.slice(0, 1))).toEqual([])
  })
})

describe('peers', () => {
  const now = new Date('2026-10-09T12:00:00Z')

  it('validates identity documents', () => {
    expect(isInfo(INFO)).toBe(true)
    expect(isInfo({ ...INFO, service: 'minio' })).toBe(false)
    expect(isInfo(null)).toBe(false)
  })

  it('merges sightings of the same server', () => {
    const a = peerFromInfo(INFO, 'http://10.0.0.5:9000', 'mdns', now)
    const b = peerFromInfo(INFO, 'http://192.168.1.20:9000', 'beacon', new Date('2026-10-09T12:00:05Z'))
    const u = peerFromSighting({ id: INFO.id, name: 'nas?', urls: ['http://10.9.9.9:9000'], source: 'beacon', error: 'timeout' }, new Date('2026-10-09T12:00:09Z'))
    const m = mergePeer(mergePeer(a, b), u)
    expect(m.sources).toEqual(['mdns', 'beacon'])
    expect(m.addresses).toEqual(['http://10.0.0.5:9000', 'http://192.168.1.20:9000', 'http://10.9.9.9:9000'])
    expect(m.name).toBe('nas') // verified identity wins over the unverified hint
    expect(m.verified && m.healthy).toBe(true)
    expect(m.error).toBeUndefined()
    expect(m.firstSeen).toBe(now.toISOString())
    expect(m.lastSeen).toBe('2026-10-09T12:00:09.000Z')
    const list: Peer[] = mergePeers([a, { ...a, id: 'other' }, b])
    expect(list.map(p => p.id)).toEqual([INFO.id, 'other'])
  })
})

describe('probing', () => {
  it('fetches and validates /_gos3/info', async () => {
    const fetch = vi.fn(async (url: string) => {
      if (url.startsWith('http://ok')) return new Response(JSON.stringify(INFO), { status: 200 })
      if (url.startsWith('http://html')) return new Response('<html>', { status: 200 })
      if (url.startsWith('http://auth')) return new Response('{}', { status: 401 })
      if (url.startsWith('http://missing')) return new Response('nope', { status: 404 })
      throw new TypeError('fetch failed', { cause: { code: 'ECONNREFUSED' } })
    })
    await expect(fetchInfo('http://ok:9000', { fetch })).resolves.toMatchObject({ id: INFO.id })
    expect(fetch).toHaveBeenCalledWith('http://ok:9000/_gos3/info', expect.anything())
    await expect(fetchInfo('http://html', { fetch })).rejects.toThrow('not a gos3 server')
    await expect(fetchInfo('http://auth', { fetch })).rejects.toThrow('authentication required')
    await expect(fetchInfo('http://missing', { fetch })).rejects.toThrow('not a gos3 server')
    await expect(fetchInfo('http://down', { fetch })).rejects.toThrow('unreachable')
  })

  it('times out', async () => {
    const fetch = (_u: string, init?: RequestInit) => new Promise<Response>((_, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
    })
    await expect(fetchInfo('http://slow', { fetch, timeoutMs: 20 })).rejects.toThrow('timed out')
  })

  it('runs a bounded pool in order', async () => {
    let inFlight = 0
    let peak = 0
    const out = await runPool([1, 2, 3, 4, 5, 6], 2, async (n) => {
      inFlight++
      peak = Math.max(peak, inFlight)
      await new Promise(r => setTimeout(r, 5))
      inFlight--
      return n * 10
    })
    expect(out).toEqual([10, 20, 30, 40, 50, 60])
    expect(peak).toBe(2)
  })

  it('scans and reports found peers', async () => {
    const fetch = async (url: string) => {
      if (url === 'http://10.0.0.5:9000/_gos3/info') return new Response(JSON.stringify(INFO))
      throw new TypeError('fetch failed')
    }
    const r = await runScan(planScan({ targets: ['10.0.0.4/30', '10.0.0.5'] }), { fetch })
    expect(r.probed).toBe(2)
    expect(r.found).toHaveLength(1)
    expect(r.found![0]).toMatchObject({ id: INFO.id, sources: ['scan'], addresses: ['http://10.0.0.5:9000'] })
    expect(r.elapsed).toMatch(/^\d+(\.\d+)?m?s$/)
  })

  it('is off unless enabled by environment', () => {
    expect(localDiscoveryEnabled({})).toBe(false)
    expect(localDiscoveryEnabled({ GOS3_UI_LOCAL_DISCOVERY: 'true' })).toBe(true)
    expect(localDiscoveryEnabled({ GOS3_UI_LOCAL_DISCOVERY: 'false' })).toBe(false)
    expect(localDiscoveryEnabled({ GOS3_UI_LOCAL_DISCOVERY: 'off' })).toBe(false)
  })
})

describe('review fixes: bounded probes', () => {
  it('stops reading an identity document beyond 64 KiB without Content-Length', async () => {
    let pulled = 0
    const fetch = async () => new Response(new ReadableStream<Uint8Array>({
      pull(ctrl) {
        pulled++
        ctrl.enqueue(new Uint8Array(16 * 1024).fill(0x20))
        if (pulled > 1000) ctrl.close()
      }
    }))
    await expect(fetchInfo('http://stream', { fetch, timeoutMs: 2000 })).rejects.toThrow('not a gos3 server')
    expect(pulled).toBeLessThan(10)
  })

  it('still accepts a chunked identity document within the limit', async () => {
    const body = new TextEncoder().encode(JSON.stringify(INFO))
    const fetch = async () => new Response(new ReadableStream<Uint8Array>({
      start(ctrl) {
        ctrl.enqueue(body.slice(0, 10))
        ctrl.enqueue(body.slice(10))
        ctrl.close()
      }
    }))
    await expect(fetchInfo('http://chunked', { fetch })).resolves.toMatchObject({ id: INFO.id })
  })

  it('verifies at most MAX_VERIFY_URLS candidate URLs', async () => {
    const fetch = vi.fn(async () => new Response('nope', { status: 404 }))
    const candidates: Candidate[] = Array.from({ length: 2000 }, (_, i) => ({ url: `http://10.${i >> 8}.${i & 255}.1:9000`, source: 'beacon', id: `id${i}` }))
    await verifyCandidates(candidates, { timeoutMs: 100, fetch })
    expect(fetch).toHaveBeenCalledTimes(MAX_VERIFY_URLS)
  })
})

describe('review fixes: DNS rebinding guard', () => {
  it('accepts IP literals, localhost and configured names only', () => {
    expect(hostnameOf('[::1]:3000')).toBe('::1')
    expect(hostnameOf('UI.Example.lan.:80')).toBe('ui.example.lan')
    for (const h of ['localhost:3000', '127.0.0.1:3000', '[::1]:3000', '192.168.1.20', 'app.localhost:3000', undefined]) expect(isAllowedHost(h, []), String(h)).toBe(true)
    for (const h of ['attacker.example:3000', 'rebind.attacker', 'localhost.attacker.example']) expect(isAllowedHost(h, []), h).toBe(false)
    expect(isAllowedHost('ui.example.lan:3000', ['ui.example.lan'])).toBe(true)
    expect(isAllowedHost('anything', ['*'])).toBe(true)
    expect(allowedHostsFromEnv({ GOS3_UI_ALLOWED_HOSTS: 'UI.example.lan, nas.local' })).toEqual(['ui.example.lan', 'nas.local'])
  })

  it('rejects foreign Host, Origin and Sec-Fetch-Site headers', () => {
    const status: number[] = []
    vi.stubGlobal('setResponseStatus', (_e: unknown, s: number) => status.push(s))
    vi.stubGlobal('setResponseHeader', () => {})
    const event = (headers: Record<string, string>) => {
      vi.stubGlobal('getRequestHeader', (_e: unknown, name: string) => headers[name])
      return {} as Parameters<typeof rejectForeignRequest>[0]
    }
    expect(rejectForeignRequest(event({ host: 'localhost:3000' }))).toBeUndefined()
    expect(rejectForeignRequest(event({ 'host': '127.0.0.1:3000', 'origin': 'http://127.0.0.1:3000', 'sec-fetch-site': 'same-origin' }))).toBeUndefined()
    expect(rejectForeignRequest(event({ host: 'attacker.example:3000', origin: 'http://attacker.example:3000' }))?.error.code).toBe('AccessDenied')
    expect(rejectForeignRequest(event({ host: 'localhost:3000', origin: 'http://evil.example' }))?.error.code).toBe('AccessDenied')
    expect(rejectForeignRequest(event({ host: 'localhost:3000', origin: 'null' }))?.error.code).toBe('AccessDenied')
    expect(rejectForeignRequest(event({ 'host': 'localhost:3000', 'sec-fetch-site': 'cross-site' }))?.error.code).toBe('AccessDenied')
    expect(status).toEqual([403, 403, 403, 403])
  })
})
