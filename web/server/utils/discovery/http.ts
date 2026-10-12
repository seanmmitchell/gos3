// Shared HTTP helpers for the /api/discovery/* Nitro routes. Errors use the
// admin API envelope ({"error": {"code", "message"}}) so the UI parses them
// like admin errors.
import { isIP } from 'node:net'

/** The h3 event type (h3 is not a direct dependency; take it from an auto-import). */
type H3Event = Parameters<typeof getQuery>[0]

export interface ErrorEnvelope {
  error: { code: string, message: string }
}

/** Sets the status and returns the JSON error envelope. */
export function errorResponse(event: H3Event, status: number, code: string, message: string): ErrorEnvelope {
  setResponseStatus(event, status)
  setResponseHeader(event, 'cache-control', 'no-store')
  return { error: { code, message } }
}

/**
 * Requires `Content-Type: application/json` on POSTs. Cross-site pages
 * cannot send that without a CORS preflight (which this server never
 * approves). That alone does not stop DNS rebinding (the attacker's page
 * becomes same-origin), so every route also calls rejectForeignRequest.
 */
export function isJsonRequest(event: H3Event): boolean {
  const ct = getRequestHeader(event, 'content-type') ?? ''
  return /^application\/json\b/i.test(ct.trim())
}

/** AbortSignal that fires when the client goes away before the response ends. */
export function clientAbortSignal(event: H3Event): AbortSignal {
  const ctrl = new AbortController()
  const res = event.node.res
  res.once('close', () => {
    if (!res.writableEnded) ctrl.abort()
  })
  return ctrl.signal
}

/** 403 answer of the probe, scan and local routes while local discovery is off. */
export function discoveryDisabled(event: H3Event): ErrorEnvelope {
  return errorResponse(event, 403, 'AccessDenied', 'Local discovery is disabled on this UI server (enable it with GOS3_UI_LOCAL_DISCOVERY=true).')
}

/** Body caps of the POST routes. */
export const PROBE_BODY_LIMIT = 8 * 1024
export const SCAN_BODY_LIMIT = 64 * 1024
/** Time a client has to send the whole (small) request body. */
export const BODY_TIMEOUT_MS = 10_000

/** A request body that cannot be accepted, with its HTTP status. */
export class BodyError extends Error {
  constructor(readonly status: 400 | 408 | 413, message: string) {
    super(message)
    this.name = 'BodyError'
  }
}

/** The request stream readCappedBody needs (an IncomingMessage). */
export type BodyStream = NodeJS.ReadableStream & { readableEnded?: boolean, destroy?: (err?: Error) => unknown }

/**
 * Reads a request body of at most `limit` bytes. Rejects with BodyError 413
 * when Content-Length announces more (without reading anything) or as soon
 * as the streamed size passes the limit, 408 when the body does not arrive
 * within timeoutMs, and 400 when the client aborts. On rejection the stream
 * is paused and detached, and what was buffered is dropped; the caller
 * disposes of the connection (see readJsonBody).
 */
export function readCappedBody(req: BodyStream, limit: number, opts: { contentLength?: string, timeoutMs?: number } = {}): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const declared = opts.contentLength?.trim()
    if (declared && /^\d+$/.test(declared) && Number(declared) > limit) {
      return reject(new BodyError(413, `The request body exceeds ${limit} bytes.`))
    }
    if (req.readableEnded) return resolve(Buffer.alloc(0))
    let chunks: Buffer[] = []
    let size = 0
    let done = false
    const finish = (err?: BodyError) => {
      if (done) return
      done = true
      clearTimeout(timer)
      req.removeListener('data', onData)
      req.removeListener('end', onEnd)
      req.removeListener('error', onError)
      req.removeListener('close', onClose)
      if (err) {
        chunks = []
        req.pause()
        req.on('error', () => {}) // late errors of a discarded stream
        reject(err)
      } else {
        resolve(Buffer.concat(chunks, size))
      }
    }
    const onData = (c: Buffer | string) => {
      const b = typeof c === 'string' ? Buffer.from(c) : c
      size += b.length
      if (size > limit) return finish(new BodyError(413, `The request body exceeds ${limit} bytes.`))
      chunks.push(b)
    }
    const onEnd = () => finish()
    const onError = () => finish(new BodyError(400, 'The request body could not be read.'))
    const onClose = () => finish(new BodyError(400, 'The request body was not received completely.'))
    const timer = setTimeout(() => finish(new BodyError(408, 'The request body was not received in time.')), opts.timeoutMs ?? BODY_TIMEOUT_MS)
    req.on('data', onData)
    req.on('end', onEnd)
    req.on('error', onError)
    req.on('close', onClose)
  })
}

/**
 * Reads a JSON request body of at most `limit` bytes (see readCappedBody).
 * Never throws: on failure the error envelope (status set) is returned.
 * When the body was not consumed completely, the response asks the client
 * to close the connection and the request is destroyed once the response
 * is sent, so the rest of an oversized upload is never read.
 */
export async function readJsonBody(event: H3Event, limit: number, opts: { timeoutMs?: number } = {}): Promise<{ ok: true, body: unknown } | { ok: false, error: ErrorEnvelope }> {
  const req = event.node.req as unknown as BodyStream
  let raw: Buffer
  try {
    raw = await readCappedBody(req, limit, { contentLength: getRequestHeader(event, 'content-length'), timeoutMs: opts.timeoutMs })
  } catch (e) {
    const err = e instanceof BodyError ? e : new BodyError(400, 'The request body could not be read.')
    setResponseHeader(event, 'connection', 'close')
    const res = event.node.res
    const dispose = () => req.destroy?.()
    res.once('finish', dispose)
    res.once('close', dispose)
    return { ok: false, error: errorResponse(event, err.status, 'InvalidRequest', err.message) }
  }
  try {
    return { ok: true, body: JSON.parse(raw.toString('utf8')) as unknown }
  } catch {
    return { ok: false, error: errorResponse(event, 400, 'InvalidRequest', 'The request body is not valid JSON.') }
  }
}

/**
 * Extra Host names the discovery routes accept, from GOS3_UI_ALLOWED_HOSTS
 * (comma or space separated, case-insensitive; "*" accepts any host).
 */
export function allowedHostsFromEnv(env: Record<string, string | undefined> = process.env): string[] {
  return (env.GOS3_UI_ALLOWED_HOSTS ?? '').split(/[\s,]+/).map(h => h.trim().toLowerCase().replace(/\.$/, '')).filter(Boolean)
}

/** Host name of a Host header value ('[::1]:3000' → '::1', 'a.b:80' → 'a.b'). */
export function hostnameOf(host: string): string {
  let h = host.trim().toLowerCase()
  if (h.startsWith('[')) h = h.slice(1, h.indexOf(']') < 0 ? undefined : h.indexOf(']'))
  else if (h.split(':').length === 2) h = h.slice(0, h.indexOf(':'))
  return h.replace(/\.$/, '')
}

/**
 * True when a request with this Host header may use the discovery routes.
 * DNS rebinding always arrives with the attacker's host name, so IP literals,
 * localhost and *.localhost are accepted, plus the configured names.
 */
export function isAllowedHost(host: string | undefined, allowed: string[] = allowedHostsFromEnv()): boolean {
  if (!host) return true // HTTP/1.0 without Host: not a browser
  if (allowed.includes('*')) return true
  const name = hostnameOf(host)
  if (!name) return false
  if (isIP(name)) return true
  if (name === 'localhost' || name.endsWith('.localhost')) return true
  return allowed.includes(name)
}

/**
 * True when origin is an http(s) origin whose host name is listed
 * explicitly in GOS3_UI_ALLOWED_HOSTS (the "*" wildcard does not count).
 */
export function isListedOrigin(origin: string, allowed: string[] = allowedHostsFromEnv()): boolean {
  let u: URL
  try {
    u = new URL(origin)
  } catch {
    return false
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return false
  const name = hostnameOf(u.host)
  return name !== '' && name !== '*' && allowed.includes(name)
}

/**
 * Rejects requests a browser made on behalf of another site: a Host header
 * that is not allowed (DNS rebinding), an Origin that differs from the Host
 * and is not listed in GOS3_UI_ALLOWED_HOSTS (cross-origin), or Sec-Fetch-Site cross-site / same-site. Returns the
 * error envelope to send, or undefined when the request may proceed.
 */
export function rejectForeignRequest(event: H3Event): ErrorEnvelope | undefined {
  const host = getRequestHeader(event, 'host')
  if (!isAllowedHost(host)) {
    return errorResponse(event, 403, 'AccessDenied', `Host "${host}" is not allowed for discovery requests: open the console via localhost or an IP address, or add the name to GOS3_UI_ALLOWED_HOSTS.`)
  }
  const origin = getRequestHeader(event, 'origin')
  if (origin !== undefined) {
    let sameOrigin = false
    try {
      sameOrigin = origin !== 'null' && !!host && new URL(origin).host === new URL(`http://${host}`).host
    } catch {
      sameOrigin = false
    }
    // Behind a reverse proxy that rewrites Host (nginx's default), the
    // browser's Origin carries the public name while Host is the upstream
    // address; accept an Origin whose name the operator listed explicitly.
    // "*" only relaxes the Host check: it does not trust every Origin.
    if (!sameOrigin) sameOrigin = isListedOrigin(origin)
    if (!sameOrigin) return errorResponse(event, 403, 'AccessDenied', 'Cross-origin discovery requests are not allowed. Behind a reverse proxy, forward the original Host header or add the console\'s host name to GOS3_UI_ALLOWED_HOSTS.')
  }
  const site = getRequestHeader(event, 'sec-fetch-site')
  if (site === 'cross-site' || site === 'same-site') return errorResponse(event, 403, 'AccessDenied', 'Cross-site discovery requests are not allowed.')
  return undefined
}

/**
 * A fixed number of slots for expensive routes. The Node server has no
 * authentication, so without a global bound any client that can reach it
 * could run hundreds of scans or multicast listeners at once.
 */
export class Slots {
  private used = 0

  constructor(readonly size: number) {}

  /** Takes a slot and returns its release function (idempotent), or undefined when all are in use. */
  tryAcquire(): (() => void) | undefined {
    if (this.used >= this.size) return undefined
    this.used++
    let released = false
    return () => {
      if (released) return
      released = true
      this.used--
    }
  }

  /** Slots currently in use. */
  get inUse(): number {
    return this.used
  }
}

/** At most this many POST /api/discovery/scan run at once (as the admin API). */
export const MAX_CONCURRENT_SCANS = 2
/** At most this many GET /api/discovery/local run at once. */
export const MAX_CONCURRENT_LOCAL = 4
/**
 * At most this many POST /api/discovery/probe run at once (the Discover
 * page probes a list of URLs with 6 requests in flight).
 */
export const MAX_CONCURRENT_PROBES = 8

/** Process-wide slots for /api/discovery/scan. */
export const scanSlots = new Slots(MAX_CONCURRENT_SCANS)
/** Process-wide slots for /api/discovery/local. */
export const localSlots = new Slots(MAX_CONCURRENT_LOCAL)
/** Process-wide slots for /api/discovery/probe. */
export const probeSlots = new Slots(MAX_CONCURRENT_PROBES)

/** The 429 TooManyRequests envelope sent when slots are full. */
export function tooManyRequests(event: H3Event, what: string, max: number): ErrorEnvelope {
  setResponseHeader(event, 'retry-after', 5)
  return errorResponse(event, 429, 'TooManyRequests', `${max} ${what} are already running on this UI server; retry when one has finished.`)
}
