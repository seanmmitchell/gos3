// HTTP verification of candidates (GET <base>/_gos3/info) and a small
// bounded-concurrency worker pool, used by local discovery, probe and scan.
import type { Info } from '~/types/admin'
import { BLOCKED_ADDRESS_CODE, BlockedAddressError, guardedFetch } from './netguard'
import { parseInfo } from './peers'

export const INFO_PATH = '/_gos3/info'
export const VERIFY_TIMEOUT_MS = 1500
/**
 * Largest identity document read. A real one is well under 2 KiB; the cap
 * bounds what a hostile host can make a scan or discovery hold in memory
 * (the parsed document is bounded further by parseInfo).
 */
export const MAX_INFO_BYTES = 16 * 1024

export type FetchFn = (input: string, init?: RequestInit) => Promise<Response>

/**
 * Coarse failure categories. Probe errors are returned to unauthenticated
 * clients of the UI server, so they never carry low-level details (socket
 * error codes, resolver messages, timings): only these categories.
 */
export type ProbeErrorKind = 'unreachable' | 'timeout' | 'http' | 'auth' | 'not-gos3' | 'blocked' | 'aborted'

const KIND_MESSAGE: Record<Exclude<ProbeErrorKind, 'http'>, string> = {
  'unreachable': 'unreachable',
  'timeout': 'timed out',
  'auth': 'authentication required',
  'not-gos3': 'not a gos3 server',
  'blocked': 'address not allowed',
  'aborted': 'aborted'
}

export class ProbeError extends Error {
  constructor(readonly kind: ProbeErrorKind, readonly status?: number) {
    super(kind === 'http' ? `HTTP ${status ?? 0}` : KIND_MESSAGE[kind])
    this.name = 'ProbeError'
  }
}

function isBlocked(e: unknown): boolean {
  for (let cur: unknown = e, i = 0; cur && i < 4; cur = (cur as { cause?: unknown }).cause, i++) {
    if (cur instanceof BlockedAddressError || (cur as { code?: unknown }).code === BLOCKED_ADDRESS_CODE) return true
  }
  return false
}

/**
 * Reads a response body as UTF-8 text, at most `limit` bytes: a body without
 * (or with a false) Content-Length cannot make the server buffer more.
 */
export async function readLimited(res: Response, limit: number): Promise<string> {
  if (!res.body) return ''
  const reader = res.body.getReader()
  const chunks: Uint8Array[] = []
  let size = 0
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    size += value.byteLength
    if (size > limit) {
      await reader.cancel().catch(() => {})
      throw new ProbeError('not-gos3')
    }
    chunks.push(value)
  }
  const buf = new Uint8Array(size)
  let off = 0
  for (const c of chunks) {
    buf.set(c, off)
    off += c.byteLength
  }
  return new TextDecoder().decode(buf)
}

/**
 * Fetches, validates and sanitises (parseInfo) the identity document of
 * baseUrl. Throws ProbeError
 * with a coarse category (see ProbeErrorKind). The default fetch refuses
 * blocked destinations at connect time (./netguard.ts).
 */
export async function fetchInfo(baseUrl: string, opts: { timeoutMs?: number, signal?: AbortSignal, fetch?: FetchFn } = {}): Promise<Info> {
  const doFetch: FetchFn = opts.fetch ?? guardedFetch
  const timeoutMs = opts.timeoutMs ?? VERIFY_TIMEOUT_MS
  const ctrl = new AbortController()
  let timedOut = false
  const timer = setTimeout(() => {
    timedOut = true
    ctrl.abort()
  }, timeoutMs)
  const onAbort = () => ctrl.abort()
  if (opts.signal?.aborted) ctrl.abort()
  opts.signal?.addEventListener('abort', onAbort, { once: true })
  try {
    let res: Response
    try {
      res = await doFetch(baseUrl + INFO_PATH, { headers: { accept: 'application/json' }, signal: ctrl.signal, redirect: 'manual' })
    } catch (e) {
      if (e instanceof ProbeError) throw e
      if (isBlocked(e)) throw new ProbeError('blocked')
      if (timedOut) throw new ProbeError('timeout')
      if (opts.signal?.aborted) throw new ProbeError('aborted')
      throw new ProbeError('unreachable')
    }
    if (res.status === 401 || res.status === 403) {
      await res.body?.cancel().catch(() => {})
      throw new ProbeError('auth')
    }
    if (!res.ok) {
      await res.body?.cancel().catch(() => {})
      throw res.status === 404 ? new ProbeError('not-gos3') : new ProbeError('http', res.status)
    }
    const len = Number(res.headers.get('content-length') ?? '0')
    if (len > MAX_INFO_BYTES) {
      await res.body?.cancel().catch(() => {})
      throw new ProbeError('not-gos3')
    }
    let body: string
    try {
      body = await readLimited(res, MAX_INFO_BYTES)
    } catch (e) {
      if (e instanceof ProbeError) throw e
      throw new ProbeError(timedOut ? 'timeout' : opts.signal?.aborted ? 'aborted' : 'unreachable')
    }
    let json: unknown
    try {
      json = JSON.parse(body)
    } catch {
      throw new ProbeError('not-gos3')
    }
    const info = parseInfo(json)
    if (!info) throw new ProbeError('not-gos3')
    return info
  } finally {
    clearTimeout(timer)
    opts.signal?.removeEventListener('abort', onAbort)
  }
}

/**
 * Runs fn over items with at most `concurrency` calls in flight. Results keep
 * the item order; fn must not throw (wrap errors). Stops starting new work
 * once signal aborts (remaining results are undefined).
 */
export async function runPool<T, R>(items: readonly T[], concurrency: number, fn: (item: T, index: number) => Promise<R>, signal?: AbortSignal): Promise<(R | undefined)[]> {
  const results: (R | undefined)[] = new Array(items.length)
  let next = 0
  const worker = async () => {
    while (next < items.length && !signal?.aborted) {
      const i = next++
      results[i] = await fn(items[i]!, i)
    }
  }
  const n = Math.max(1, Math.min(concurrency, items.length))
  await Promise.all(Array.from({ length: n }, worker))
  return results
}
