// HTTP verification of candidates (GET <base>/_gos3/info) and a small
// bounded-concurrency worker pool, used by local discovery, probe and scan.
import type { Info } from '~/types/admin'
import { isInfo } from './peers'

export const INFO_PATH = '/_gos3/info'
export const VERIFY_TIMEOUT_MS = 1500
const MAX_INFO_BYTES = 64 * 1024

export type FetchFn = (input: string, init?: RequestInit) => Promise<Response>

export class ProbeError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'ProbeError'
  }
}

function causeMessage(e: unknown): string {
  const cause = (e as { cause?: { code?: string, message?: string } })?.cause
  if (cause?.code) return cause.code
  if (cause?.message) return cause.message
  return e instanceof Error ? e.message : String(e)
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
      throw new ProbeError('identity document too large')
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
 * Fetches and validates the identity document of baseUrl. Throws ProbeError
 * with a human readable message (timeout, connection error, not gos3 …).
 */
export async function fetchInfo(baseUrl: string, opts: { timeoutMs?: number, signal?: AbortSignal, fetch?: FetchFn } = {}): Promise<Info> {
  const doFetch: FetchFn = opts.fetch ?? ((u, i) => globalThis.fetch(u, i))
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
      if (timedOut) throw new ProbeError(`timed out after ${timeoutMs} ms`)
      if (opts.signal?.aborted) throw new ProbeError('aborted')
      throw new ProbeError(`connection failed (${causeMessage(e)})`)
    }
    if (res.status === 401 || res.status === 403) {
      await res.body?.cancel().catch(() => {})
      throw new ProbeError(`identity document requires authentication (HTTP ${res.status})`)
    }
    if (!res.ok) {
      await res.body?.cancel().catch(() => {})
      throw new ProbeError(res.status === 404 ? 'not a gos3 server (no /_gos3/info)' : `HTTP ${res.status}`)
    }
    const len = Number(res.headers.get('content-length') ?? '0')
    if (len > MAX_INFO_BYTES) {
      await res.body?.cancel().catch(() => {})
      throw new ProbeError('identity document too large')
    }
    let body: string
    try {
      body = await readLimited(res, MAX_INFO_BYTES)
    } catch (e) {
      if (e instanceof ProbeError) throw e
      throw new ProbeError(timedOut ? `timed out after ${timeoutMs} ms` : `read failed (${causeMessage(e)})`)
    }
    let json: unknown
    try {
      json = JSON.parse(body)
    } catch {
      throw new ProbeError('not a gos3 server (invalid identity document)')
    }
    if (!isInfo(json)) throw new ProbeError('not a gos3 server')
    return json
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
