// Route-level tests for the standalone /api/discovery/* Nitro handlers (review
// round 2): reverse-proxy Origin handling and global concurrency limits.
import { EventEmitter } from 'node:events'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { isListedOrigin, localSlots, MAX_CONCURRENT_LOCAL, MAX_CONCURRENT_SCANS, rejectForeignRequest, scanSlots, Slots } from '~~/server/utils/discovery/http'

const scanGate = vi.hoisted(() => ({ waiters: [] as Array<() => void> }))

vi.mock('~~/server/utils/discovery/scan', () => ({
  runScan: vi.fn(() => new Promise((resolve) => {
    scanGate.waiters.push(() => resolve({ found: [], probed: 0, elapsed: '0s' }))
  }))
}))

vi.mock('~~/server/utils/discovery/local', async (orig) => {
  const actual = await orig<typeof import('~~/server/utils/discovery/local')>()
  return {
    ...actual,
    loadMdns: vi.fn(async () => null),
    discoverLocal: vi.fn(() => new Promise((resolve) => {
      scanGate.waiters.push(() => resolve({ peers: [], errors: [], candidates: 0, elapsedMs: 0 }))
    }))
  }
})

type Handler = (event: unknown) => Promise<unknown>

interface FakeEvent {
  headers: Record<string, string>
  status: number
  resHeaders: Record<string, string>
  node: { res: EventEmitter & { writableEnded: boolean } }
}

function fakeEvent(headers: Record<string, string>): FakeEvent {
  const res = Object.assign(new EventEmitter(), { writableEnded: false })
  return { headers, status: 200, resHeaders: {}, node: { res } }
}

function stubH3(body?: unknown) {
  vi.stubGlobal('defineEventHandler', (h: Handler) => h)
  vi.stubGlobal('getRequestHeader', (e: FakeEvent, name: string) => e.headers[name])
  vi.stubGlobal('setResponseStatus', (e: FakeEvent, s: number) => {
    e.status = s
  })
  vi.stubGlobal('setResponseHeader', (e: FakeEvent, k: string, v: string) => {
    e.resHeaders[k] = v
  })
  vi.stubGlobal('readBody', async () => body)
  vi.stubGlobal('getQuery', () => ({}))
}

/** Lets pending microtasks (handler awaits) run. */
const settle = () => new Promise(r => setTimeout(r, 0))

describe('review round 2: Origin behind a Host-rewriting reverse proxy', () => {
  afterEach(() => {
    vi.unstubAllEnvs()
  })

  it('accepts an Origin whose host name is listed explicitly', () => {
    expect(isListedOrigin('https://console.example.com', ['console.example.com'])).toBe(true)
    expect(isListedOrigin('https://Console.Example.com:8443', ['console.example.com'])).toBe(true)
    expect(isListedOrigin('https://evil.example', ['console.example.com'])).toBe(false)
    expect(isListedOrigin('https://evil.example', ['*'])).toBe(false)
    expect(isListedOrigin('null', ['null'])).toBe(false)
    expect(isListedOrigin('ftp://console.example.com', ['console.example.com'])).toBe(false)
  })

  it('lets a proxied same-origin POST through when the public name is listed', () => {
    stubH3()
    // nginx `proxy_pass http://127.0.0.1:3000` sends Host: 127.0.0.1:3000.
    const proxied = () => fakeEvent({ 'host': '127.0.0.1:3000', 'origin': 'https://console.example.com', 'sec-fetch-site': 'same-origin' })

    vi.stubEnv('GOS3_UI_ALLOWED_HOSTS', 'console.example.com')
    expect(rejectForeignRequest(proxied() as never)).toBeUndefined()

    // Unlisted public name: still refused, with a hint.
    vi.stubEnv('GOS3_UI_ALLOWED_HOSTS', 'other.example.com')
    const e = proxied()
    const env = rejectForeignRequest(e as never)
    expect(e.status).toBe(403)
    expect(env?.error.message).toMatch(/GOS3_UI_ALLOWED_HOSTS/)

    // "*" relaxes only the Host check, never Origin.
    vi.stubEnv('GOS3_UI_ALLOWED_HOSTS', '*')
    expect(rejectForeignRequest(proxied() as never)?.error.code).toBe('AccessDenied')
    // A listed name does not let a foreign Origin in either.
    vi.stubEnv('GOS3_UI_ALLOWED_HOSTS', 'console.example.com')
    expect(rejectForeignRequest(fakeEvent({ host: '127.0.0.1:3000', origin: 'https://evil.example' }) as never)?.error.code).toBe('AccessDenied')
  })
})

describe('review round 2: global concurrency limits', () => {
  beforeEach(() => {
    scanGate.waiters.length = 0
  })

  afterEach(() => {
    for (const w of scanGate.waiters.splice(0)) w()
  })

  it('Slots hands out at most size slots and releases idempotently', () => {
    const s = new Slots(2)
    const a = s.tryAcquire()
    const b = s.tryAcquire()
    expect(a && b).toBeTruthy()
    expect(s.tryAcquire()).toBeUndefined()
    a!()
    a!()
    expect(s.inUse).toBe(1)
    expect(s.tryAcquire()).toBeTypeOf('function')
    expect(s.tryAcquire()).toBeUndefined()
  })

  it('POST /api/discovery/scan answers 429 beyond MAX_CONCURRENT_SCANS and frees slots when scans end', async () => {
    stubH3({ targets: ['10.0.0.1'], ports: [9000] })
    const handler = (await import('~~/server/api/discovery/scan.post')).default as unknown as Handler
    const req = () => fakeEvent({ 'host': 'localhost:3000', 'content-type': 'application/json' })

    const running = Array.from({ length: MAX_CONCURRENT_SCANS }, () => handler(req()))
    await settle()
    expect(scanSlots.inUse).toBe(MAX_CONCURRENT_SCANS)

    const over = req()
    const res = await handler(over) as { error: { code: string } }
    expect(over.status).toBe(429)
    expect(res.error.code).toBe('TooManyRequests')
    expect(over.resHeaders['retry-after']).toBeDefined()

    for (const w of scanGate.waiters.splice(0)) w()
    await Promise.all(running)
    expect(scanSlots.inUse).toBe(0)

    // A slot is free again.
    const again = handler(req())
    await settle()
    expect(scanSlots.inUse).toBe(1)
    for (const w of scanGate.waiters.splice(0)) w()
    await again
    expect(scanSlots.inUse).toBe(0)
  })

  it('GET /api/discovery/local answers 429 beyond MAX_CONCURRENT_LOCAL', async () => {
    stubH3()
    const handler = (await import('~~/server/api/discovery/local.get')).default as unknown as Handler
    const req = () => fakeEvent({ host: 'localhost:3000' })

    const running = Array.from({ length: MAX_CONCURRENT_LOCAL }, () => handler(req()))
    await settle()
    expect(localSlots.inUse).toBe(MAX_CONCURRENT_LOCAL)

    const over = req()
    const res = await handler(over) as { error: { code: string } }
    expect(over.status).toBe(429)
    expect(res.error.code).toBe('TooManyRequests')

    for (const w of scanGate.waiters.splice(0)) w()
    await Promise.all(running)
    expect(localSlots.inUse).toBe(0)
  })
})
