// Security hardening of the standalone UI's /api/discovery/* Nitro routes
// (audit findings web-1 / web-2 and contract C6): local discovery off by
// default, metadata / unspecified / multicast / broadcast destinations
// refused (literally and at connect time), probe concurrency slots, capped
// JSON bodies, and coarse probe error messages.
import { EventEmitter } from 'node:events'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { PassThrough } from 'node:stream'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Info } from '~/types/admin'
import {
  BodyError,
  MAX_CONCURRENT_PROBES,
  MAX_CONCURRENT_SCANS,
  PROBE_BODY_LIMIT,
  probeSlots,
  readCappedBody,
  readJsonBody,
  SCAN_BODY_LIMIT,
  scanSlots
} from '~~/server/utils/discovery/http'
import { localDiscoveryEnabled } from '~~/server/utils/discovery/local'
import { BlockedAddressError, blockedUrlHost, createGuardedFetch, isBlockedAddress, type ResolveFn } from '~~/server/utils/discovery/netguard'
import { fetchInfo, ProbeError } from '~~/server/utils/discovery/probe'
import { assertProbeableUrl, planScan, ScanRequestError } from '~~/server/utils/discovery/targets'

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

// -- fake h3 events -----------------------------------------------------------

type Handler = (event: unknown) => Promise<unknown>
type Envelope = { error: { code: string, message: string } }

interface FakeRes extends EventEmitter {
  writableEnded: boolean
}

interface FakeEvent {
  headers: Record<string, string>
  status: number
  resHeaders: Record<string, string>
  node: { req: PassThrough, res: FakeRes }
}

/** An event whose request body stream stays open unless `body` is given. */
function fakeEvent(headers: Record<string, string>, body?: string | object): FakeEvent {
  const res = Object.assign(new EventEmitter(), { writableEnded: false })
  const req = new PassThrough()
  if (body !== undefined) req.end(typeof body === 'string' ? body : JSON.stringify(body))
  return { headers, status: 200, resHeaders: {}, node: { req, res } }
}

const JSON_HEADERS = { 'host': 'localhost:3000', 'content-type': 'application/json' }

function stubH3() {
  vi.stubGlobal('defineEventHandler', (h: Handler) => h)
  vi.stubGlobal('getRequestHeader', (e: FakeEvent, name: string) => e.headers[name])
  vi.stubGlobal('setResponseStatus', (e: FakeEvent, s: number) => {
    e.status = s
  })
  vi.stubGlobal('setResponseHeader', (e: FakeEvent, k: string, v: string) => {
    e.resHeaders[k] = String(v)
  })
  vi.stubGlobal('getQuery', () => ({}))
}

async function route(name: 'probe.post' | 'scan.post' | 'local.get' | 'capabilities.get'): Promise<Handler> {
  stubH3()
  const mod = await import(`~~/server/api/discovery/${name}.ts`) as { default: Handler }
  return mod.default
}

const settle = () => new Promise(r => setTimeout(r, 0))

/** A local HTTP server; `handler` answers every request. */
async function httpServer(handler: Parameters<typeof createServer>[1]): Promise<{ server: Server, port: number }> {
  const server = createServer(handler)
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  return { server, port: (server.address() as AddressInfo).port }
}

/** A loopback port nothing listens on. */
async function closedPort(): Promise<number> {
  const { server, port } = await httpServer(() => {})
  await new Promise(r => server.close(r))
  return port
}

afterEach(() => {
  vi.unstubAllEnvs()
})

// -- web-1: off by default -----------------------------------------------------

describe('web-1: local discovery is off by default', () => {
  it('is enabled only by an explicit true value', () => {
    for (const v of [undefined, '', 'false', '0', 'off', 'no', 'disabled', 'maybe', 'tru']) {
      expect(localDiscoveryEnabled({ GOS3_UI_LOCAL_DISCOVERY: v }), String(v)).toBe(false)
    }
    for (const v of ['true', 'TRUE', ' true ', '1', 'on', 'yes', 'enabled']) {
      expect(localDiscoveryEnabled({ GOS3_UI_LOCAL_DISCOVERY: v }), v).toBe(true)
    }
    expect(localDiscoveryEnabled({})).toBe(false)
  })

  it('probe, scan and local answer 403 "disabled" when the variable is unset', async () => {
    vi.stubEnv('GOS3_UI_LOCAL_DISCOVERY', undefined)
    for (const [name, body] of [['probe.post', { url: 'http://127.0.0.1:9' }], ['scan.post', { targets: ['127.0.0.1'] }], ['local.get', undefined]] as const) {
      const handler = await route(name)
      const e = fakeEvent(JSON_HEADERS, body)
      const res = await handler(e) as Envelope
      expect(e.status, name).toBe(403)
      expect(res.error.code).toBe('AccessDenied')
      expect(res.error.message).toMatch(/disabled.*GOS3_UI_LOCAL_DISCOVERY=true/)
    }
  })

  it('capabilities still answers and reports localDiscovery: false', async () => {
    vi.stubEnv('GOS3_UI_LOCAL_DISCOVERY', undefined)
    const handler = await route('capabilities.get')
    const e = fakeEvent({ host: 'localhost:3000' })
    expect(await handler(e)).toMatchObject({ localDiscovery: false, mdns: false, beacon: false, scan: false })
    expect(e.status).toBe(200)

    vi.stubEnv('GOS3_UI_LOCAL_DISCOVERY', 'true')
    expect(await handler(fakeEvent({ host: 'localhost:3000' }))).toMatchObject({ localDiscovery: true, beacon: true, scan: true })
  })
})

// -- C6: blocked destinations ---------------------------------------------------

describe('C6: metadata, unspecified, multicast and broadcast addresses', () => {
  it('blocks IPv4 literals', () => {
    for (const ip of ['169.254.169.254', '169.254.170.2', '169.254.170.23', '100.100.100.200', '0.0.0.0', '0.1.2.3', '224.0.0.251', '239.255.73.83', '255.255.255.255']) {
      expect(isBlockedAddress(ip), ip).toBe(true)
    }
    // General link-local, loopback and private ranges stay allowed.
    for (const ip of ['127.0.0.1', '10.0.0.5', '192.168.1.20', '169.254.1.1', '169.254.169.253', '169.254.170.3', '100.100.100.201', '223.255.255.255']) {
      expect(isBlockedAddress(ip), ip).toBe(false)
    }
  })

  it('blocks IPv6 forms', () => {
    for (const ip of ['fd00:ec2::254', 'FD00:0EC2::0254', 'fd00:ec2:0:0:0:0:0:254', '[fd00:ec2::254]', '::', '0:0:0:0:0:0:0:0', '::0', 'ff02::1', 'ff05::fb', 'ff02::1%eth0']) {
      expect(isBlockedAddress(ip), ip).toBe(true)
    }
    for (const ip of ['::1', 'fe80::1', 'fe80::1%eth0', 'fd00::5', 'fd00:ec2::253', '2001:db8::1']) {
      expect(isBlockedAddress(ip), ip).toBe(false)
    }
  })

  it('blocks IPv4-mapped, IPv4-compatible and NAT64 forms of blocked IPv4 addresses', () => {
    for (const ip of ['::ffff:169.254.169.254', '::ffff:a9fe:a9fe', '::FFFF:A9FE:A9FE', '0:0:0:0:0:ffff:a9fe:a9fe', '::ffff:100.100.100.200', '::ffff:0.0.0.0', '::ffff:224.0.0.1', '::ffff:255.255.255.255', '::a9fe:a9fe', '64:ff9b::169.254.169.254', '64:ff9b::a9fe:a9fe']) {
      expect(isBlockedAddress(ip), ip).toBe(true)
    }
    for (const ip of ['::ffff:127.0.0.1', '::ffff:10.0.0.5', '::ffff:169.254.1.1', '64:ff9b::10.0.0.5']) {
      expect(isBlockedAddress(ip), ip).toBe(false)
    }
  })

  it('normalises URL host literals before checking them', () => {
    for (const u of ['http://169.254.169.254', 'http://169.254.169.254:80/latest', 'http://2852039166', 'http://0xa9fea9fe:80', 'http://0251.0376.0251.0376', 'http://169.254.43518', 'https://[::ffff:169.254.169.254]', 'http://[fd00:ec2::254]:80', 'http://0.0.0.0:9000', 'http://[::]:9000']) {
      expect(blockedUrlHost(u), u).toBeDefined()
    }
    for (const u of ['http://nas:9000', 'http://127.0.0.1:9000', 'http://[::1]:9000', 'http://169.254.1.1:9000', 'not a url']) {
      expect(blockedUrlHost(u), u).toBeUndefined()
    }
    expect(() => assertProbeableUrl('http://169.254.169.254')).toThrow(ScanRequestError)
    expect(() => assertProbeableUrl('http://127.0.0.1:9000')).not.toThrow()
  })

  it('refuses explicit blocked scan targets and skips them inside CIDRs', () => {
    for (const t of ['169.254.169.254', '169.254.169.254:80', 'http://169.254.169.254', '[fd00:ec2::254]:80', '::ffff:a9fe:a9fe', '0.0.0.0', '255.255.255.255', '224.0.0.251', '2852039166']) {
      expect(() => planScan({ targets: [t] }), t).toThrow(/never probed/)
    }
    const p = planScan({ targets: ['169.254.169.0/24'] })
    expect(p.urls).toHaveLength(253)
    expect(p.urls).not.toContain('http://169.254.169.254:9000')
    expect(p.urls).toContain('http://169.254.169.253:9000')
    expect(planScan({ targets: ['224.0.0.0/30'] }).urls).toEqual([])
    // Loopback stays scannable (the standalone e2e scans 127.0.0.1).
    expect(planScan({ targets: ['127.0.0.1'] }).urls).toEqual(['http://127.0.0.1:9000'])
  })
})

describe('C6: enforcement at connect time', () => {
  let srv: { server: Server, port: number } | undefined
  beforeEach(async () => {
    srv = await httpServer((_req, res) => {
      res.setHeader('content-type', 'application/json')
      res.end(JSON.stringify(INFO))
    })
  })
  afterEach(async () => {
    await new Promise(r => srv?.server.close(r))
  })

  const fixed = (...addresses: string[]): ResolveFn => vi.fn(async () => addresses.map(address => ({ address, family: address.includes(':') ? 6 : 4 })))

  it('connects to the address the guarded lookup resolved', async () => {
    const resolve = fixed('127.0.0.1')
    const info = await fetchInfo(`http://gos3.test:${srv!.port}`, { fetch: createGuardedFetch({ resolve }) })
    expect(info.id).toBe(INFO.id)
    expect(resolve).toHaveBeenCalledWith('gos3.test', expect.anything())
  })

  it('refuses a host name that resolves to a metadata address', async () => {
    for (const addr of ['169.254.169.254', 'fd00:ec2::254', '::ffff:169.254.169.254', '100.100.100.200']) {
      const resolve = fixed(addr)
      const err = await fetchInfo(`http://metadata.test:${srv!.port}`, { fetch: createGuardedFetch({ resolve }), timeoutMs: 2000 }).catch((e: unknown) => e)
      expect(err, addr).toBeInstanceOf(ProbeError)
      expect((err as ProbeError).kind).toBe('blocked')
      expect((err as ProbeError).message).toBe('address not allowed')
      expect(resolve).toHaveBeenCalled()
    }
    // Any blocked address among the answers refuses the name.
    const mixed = await fetchInfo(`http://mixed.test:${srv!.port}`, { fetch: createGuardedFetch({ resolve: fixed('127.0.0.1', '169.254.169.254') }) }).catch((e: unknown) => e)
    expect((mixed as ProbeError).kind).toBe('blocked')
  })

  it('checks every connection, so DNS rebinding cannot switch to a metadata address', async () => {
    let calls = 0
    const rebinding: ResolveFn = async () => [{ address: calls++ === 0 ? '127.0.0.1' : '169.254.169.254', family: 4 }]
    const fetch = createGuardedFetch({ resolve: rebinding })
    await expect(fetchInfo(`http://rebind.test:${srv!.port}`, { fetch })).resolves.toMatchObject({ id: INFO.id })
    await expect(fetchInfo(`http://rebind.test:${srv!.port}`, { fetch })).rejects.toThrow('address not allowed')
    expect(calls).toBe(2)
  })

  it('refuses blocked IP literals without resolving or connecting', async () => {
    const resolve = fixed('127.0.0.1')
    const fetch = createGuardedFetch({ resolve })
    for (const u of ['http://169.254.169.254', 'http://[::ffff:a9fe:a9fe]', 'http://0.0.0.0', 'http://[fd00:ec2::254]']) {
      await expect(fetch(`${u}/_gos3/info`), u).rejects.toBeInstanceOf(BlockedAddressError)
    }
    expect(resolve).not.toHaveBeenCalled()
  })

  it('does not follow redirects', async () => {
    const redirect = await httpServer((_req, res) => {
      res.writeHead(302, { location: 'http://169.254.169.254/latest/meta-data' })
      res.end()
    })
    try {
      await expect(fetchInfo(`http://127.0.0.1:${redirect.port}`)).rejects.toThrow('HTTP 302')
    } finally {
      await new Promise(r => redirect.server.close(r))
    }
  })
})

// -- coarse error messages -------------------------------------------------------

describe('web-1: probe errors are coarse categories', () => {
  const cases: Array<[string, () => Promise<Response>, string]> = [
    ['refused', async () => {
      throw new TypeError('fetch failed', { cause: Object.assign(new Error('connect ECONNREFUSED 10.0.0.5:22'), { code: 'ECONNREFUSED' }) })
    }, 'unreachable'],
    ['dns', async () => {
      throw Object.assign(new Error('getaddrinfo ENOTFOUND kubernetes.default.svc'), { code: 'ENOTFOUND' })
    }, 'unreachable'],
    ['blocked', async () => {
      throw new BlockedAddressError('169.254.169.254')
    }, 'address not allowed'],
    ['500', async () => new Response('Internal error: stack trace', { status: 500 }), 'HTTP 500'],
    ['401', async () => new Response('{}', { status: 401 }), 'authentication required'],
    ['403', async () => new Response('{}', { status: 403 }), 'authentication required'],
    ['404', async () => new Response('nope', { status: 404 }), 'not a gos3 server'],
    ['html', async () => new Response('<html>', { status: 200 }), 'not a gos3 server'],
    ['other json', async () => new Response('{"service":"minio"}', { status: 200 }), 'not a gos3 server'],
    ['too large', async () => new Response('x', { headers: { 'content-length': String(1 << 20) } }), 'not a gos3 server']
  ]

  for (const [name, fetch, message] of cases) {
    it(`${name} → "${message}"`, async () => {
      const err = await fetchInfo('http://target:9000', { fetch }).catch((e: unknown) => e)
      expect(err).toBeInstanceOf(ProbeError)
      expect((err as Error).message).toBe(message)
    })
  }

  it('timeout → "timed out" (no duration)', async () => {
    const fetch = (_u: string, init?: RequestInit) => new Promise<Response>((_, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
    })
    await expect(fetchInfo('http://slow', { fetch, timeoutMs: 20 })).rejects.toThrow(/^timed out$/)
  })

  it('the probe route answers 502 with the URL and the category only', async () => {
    vi.stubEnv('GOS3_UI_LOCAL_DISCOVERY', 'true')
    const handler = await route('probe.post')
    const port = await closedPort()
    const e = fakeEvent(JSON_HEADERS, { url: `http://127.0.0.1:${port}/` })
    const res = await handler(e) as Envelope
    expect(e.status).toBe(502)
    expect(res.error).toEqual({ code: 'ProbeFailed', message: `http://127.0.0.1:${port}: unreachable` })
    expect(probeSlots.inUse).toBe(0)
  })

  it('the probe route refuses blocked literals with 400', async () => {
    vi.stubEnv('GOS3_UI_LOCAL_DISCOVERY', 'true')
    const handler = await route('probe.post')
    for (const url of ['http://169.254.169.254/latest/meta-data', '[::ffff:a9fe:a9fe]:80', 'http://2852039166', 'http://[fd00:ec2::254]']) {
      const e = fakeEvent(JSON_HEADERS, { url })
      const res = await handler(e) as Envelope
      expect(e.status, url).toBe(400)
      expect(res.error.message).toMatch(/never probed/)
    }
    expect(probeSlots.inUse).toBe(0)
  })
})

// -- slots ---------------------------------------------------------------------------

describe('web-1 / web-2: concurrency slots are taken before the body is read', () => {
  beforeEach(() => {
    vi.stubEnv('GOS3_UI_LOCAL_DISCOVERY', 'true')
  })

  it('probe answers 429 beyond MAX_CONCURRENT_PROBES and frees its slots', async () => {
    const handler = await route('probe.post')
    // Bodies that have not arrived yet hold their slot.
    const pending = Array.from({ length: MAX_CONCURRENT_PROBES }, () => fakeEvent(JSON_HEADERS))
    const running = pending.map(e => handler(e))
    await settle()
    expect(probeSlots.inUse).toBe(MAX_CONCURRENT_PROBES)

    const over = fakeEvent(JSON_HEADERS, { url: 'http://127.0.0.1:9' })
    const res = await handler(over) as Envelope
    expect(over.status).toBe(429)
    expect(res.error.code).toBe('TooManyRequests')
    expect(over.resHeaders['retry-after']).toBeDefined()

    for (const e of pending) e.node.req.end('not json')
    await Promise.all(running)
    expect(pending.every(e => e.status === 400)).toBe(true)
    expect(probeSlots.inUse).toBe(0)
  })

  it('scan holds its slot while the body is still arriving', async () => {
    const handler = await route('scan.post')
    const pending = Array.from({ length: MAX_CONCURRENT_SCANS }, () => fakeEvent(JSON_HEADERS))
    const running = pending.map(e => handler(e))
    await settle()
    expect(scanSlots.inUse).toBe(MAX_CONCURRENT_SCANS)

    const over = fakeEvent(JSON_HEADERS, { targets: ['127.0.0.1'] })
    expect(((await handler(over)) as Envelope).error.code).toBe('TooManyRequests')
    expect(over.status).toBe(429)

    for (const e of pending) e.node.req.end('{')
    await Promise.all(running)
    expect(scanSlots.inUse).toBe(0)
  })
})

// -- web-2: capped bodies ---------------------------------------------------------

describe('web-2: capped JSON request bodies', () => {
  it('rejects by Content-Length without reading the body', async () => {
    const req = new PassThrough()
    const read = vi.spyOn(req, 'on')
    const err = await readCappedBody(req, 1024, { contentLength: '1025' }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(BodyError)
    expect((err as BodyError).status).toBe(413)
    expect(read).not.toHaveBeenCalledWith('data', expect.anything())
    // Exactly the limit is fine.
    const ok = new PassThrough()
    ok.end(Buffer.alloc(1024, 0x20))
    await expect(readCappedBody(ok, 1024, { contentLength: '1024' })).resolves.toHaveLength(1024)
  })

  it('aborts a streamed body once it passes the cap and detaches from the stream', async () => {
    const req = new PassThrough()
    const p = readCappedBody(req, 8 * 1024)
    let written = 0
    // Chunked upload without Content-Length that would never end.
    for (let i = 0; i < 4; i++) {
      req.write(Buffer.alloc(4096, 0x20))
      written += 4096
    }
    const err = await p.catch((e: unknown) => e)
    expect((err as BodyError).status).toBe(413)
    expect(written).toBeGreaterThan(8 * 1024)
    expect(req.listenerCount('data')).toBe(0)
    expect(req.isPaused()).toBe(true)
  })

  it('times out a body that does not arrive', async () => {
    const req = new PassThrough()
    req.write('{"url":')
    const err = await readCappedBody(req, 1024, { timeoutMs: 20 }).catch((e: unknown) => e)
    expect((err as BodyError).status).toBe(408)
    expect(req.listenerCount('data')).toBe(0)
  })

  it('reports a client that goes away mid-body', async () => {
    const req = new PassThrough()
    const p = readCappedBody(req, 1024)
    req.write('{"url":')
    req.destroy()
    expect(((await p.catch((e: unknown) => e)) as BodyError).status).toBe(400)
  })

  it('readJsonBody: 413 by Content-Length and by streamed size, then disposes of the request', async () => {
    stubH3()
    const byLength = fakeEvent({ ...JSON_HEADERS, 'content-length': String(PROBE_BODY_LIMIT + 1) })
    const r1 = await readJsonBody(byLength as never, PROBE_BODY_LIMIT)
    expect(r1.ok).toBe(false)
    expect(byLength.status).toBe(413)
    expect(byLength.resHeaders.connection).toBe('close')
    expect(byLength.node.req.destroyed).toBe(false)
    byLength.node.res.emit('finish')
    expect(byLength.node.req.destroyed).toBe(true)

    const streamed = fakeEvent(JSON_HEADERS)
    const p = readJsonBody(streamed as never, SCAN_BODY_LIMIT)
    streamed.node.req.write(`{"targets":["${'a'.repeat(SCAN_BODY_LIMIT)}"]}`)
    const r2 = await p
    expect(r2.ok).toBe(false)
    expect(streamed.status).toBe(413)
    if (!r2.ok) expect(r2.error.error.message).toMatch(new RegExp(`exceeds ${SCAN_BODY_LIMIT} bytes`))
    streamed.node.res.emit('close')
    expect(streamed.node.req.destroyed).toBe(true)
  })

  it('readJsonBody: 400 on invalid or empty JSON, parsed body otherwise', async () => {
    stubH3()
    for (const body of ['{"url":', 'not json', '']) {
      const e = fakeEvent(JSON_HEADERS, body)
      const r = await readJsonBody(e as never, PROBE_BODY_LIMIT)
      expect(r.ok, body).toBe(false)
      expect(e.status).toBe(400)
      if (!r.ok) expect(r.error.error.message).toBe('The request body is not valid JSON.')
    }
    const ok = fakeEvent(JSON_HEADERS, { url: 'http://nas:9000' })
    expect(await readJsonBody(ok as never, PROBE_BODY_LIMIT)).toEqual({ ok: true, body: { url: 'http://nas:9000' } })
    expect(ok.status).toBe(200)
  })

  it('the probe and scan routes answer 413 for oversized bodies', async () => {
    vi.stubEnv('GOS3_UI_LOCAL_DISCOVERY', 'true')
    for (const [name, limit] of [['probe.post', PROBE_BODY_LIMIT], ['scan.post', SCAN_BODY_LIMIT]] as const) {
      const handler = await route(name)
      const e = fakeEvent(JSON_HEADERS, { url: 'x'.repeat(limit), targets: ['x'.repeat(limit)] })
      const res = await handler(e) as Envelope
      expect(e.status, name).toBe(413)
      expect(res.error.code).toBe('InvalidRequest')
    }
    expect(probeSlots.inUse + scanSlots.inUse).toBe(0)
  })
})
