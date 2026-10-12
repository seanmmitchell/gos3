// Client for the standalone UI server's local discovery routes
// (server/api/discovery/*). They only exist when the UI runs as a Node
// server (`pnpm build`); the static/embedded build has no server routes, so
// `fetchLocalCapabilities` returns null there (404, HTML SPA fallback, …).
import type { Info, Peer, ScanRequest, ScanResult } from '~/types/admin'
import { AdminError, isGos3Info, parseAdminError, type FetchLike } from '~/utils/admin/client'

export interface LocalCapabilities {
  localDiscovery: boolean
  mdns: boolean
  beacon: boolean
  scan: boolean
  maxScanHosts: number
}

export interface LocalDiscoveryResponse {
  peers: Peer[]
  errors: string[]
  candidates?: number
  elapsedMs?: number
}

function joinBase(baseURL: string, path: string): string {
  return `${baseURL.replace(/\/+$/, '')}/${path.replace(/^\/+/, '')}`
}

export function isLocalCapabilities(v: unknown): v is LocalCapabilities {
  if (typeof v !== 'object' || v === null) return false
  const o = v as Record<string, unknown>
  return typeof o.localDiscovery === 'boolean' && typeof o.mdns === 'boolean' && typeof o.beacon === 'boolean'
}

export class LocalDiscoveryClient {
  private readonly fetchFn: FetchLike
  private readonly baseURL: string

  constructor(baseURL: string, opts: { fetch?: FetchLike } = {}) {
    this.baseURL = baseURL
    this.fetchFn = opts.fetch ?? (req => globalThis.fetch(req))
  }

  url(path: string): string {
    return new URL(joinBase(this.baseURL, path), globalThis.location?.href ?? 'http://localhost/').toString()
  }

  private async request<T>(method: 'GET' | 'POST', path: string, body?: unknown, opts: { signal?: AbortSignal, timeoutMs?: number } = {}): Promise<T> {
    const ctrl = new AbortController()
    let timedOut = false
    const timer = opts.timeoutMs
      ? setTimeout(() => {
          timedOut = true
          ctrl.abort()
        }, opts.timeoutMs)
      : undefined
    const onAbort = () => ctrl.abort()
    opts.signal?.addEventListener('abort', onAbort, { once: true })
    let res: Response
    let text: string
    try {
      res = await this.fetchFn(new Request(this.url(path), {
        method,
        headers: body !== undefined ? { 'content-type': 'application/json', accept: 'application/json' } : { accept: 'application/json' },
        body: body !== undefined ? JSON.stringify(body) : undefined,
        signal: ctrl.signal,
        cache: 'no-store'
      }))
      text = await res.text()
    } catch (e) {
      if (timedOut) throw new AdminError('Timeout', 'The UI server did not answer in time.', 0, e)
      if (opts.signal?.aborted) throw new AdminError('Aborted', 'The request was aborted.', 0, e)
      throw new AdminError('NetworkError', 'Could not reach the UI server.', 0, e)
    } finally {
      if (timer) clearTimeout(timer)
      opts.signal?.removeEventListener('abort', onAbort)
    }
    if (!res.ok) throw parseAdminError(res.status, text)
    try {
      return JSON.parse(text) as T
    } catch (e) {
      throw new AdminError('InvalidResponse', 'Invalid JSON from the UI server.', res.status, e)
    }
  }

  /**
   * Capabilities, or null when the routes do not exist (static build). When
   * local discovery is off on the UI server (the default; it is enabled with
   * GOS3_UI_LOCAL_DISCOVERY=true) the result has `localDiscovery: false`.
   * Never throws.
   */
  async capabilities(timeoutMs = 3000): Promise<LocalCapabilities | null> {
    try {
      const caps = await this.request<unknown>('GET', 'api/discovery/capabilities', undefined, { timeoutMs })
      return isLocalCapabilities(caps) ? caps : null
    } catch {
      return null
    }
  }

  /** mDNS + beacon from the UI host; waitMs 500–10000. */
  async discover(waitMs = 3000, signal?: AbortSignal): Promise<LocalDiscoveryResponse> {
    const r = await this.request<LocalDiscoveryResponse>('GET', `api/discovery/local?wait=${Math.round(waitMs)}`, undefined, { signal, timeoutMs: waitMs + 20000 })
    return { ...r, peers: r.peers ?? [], errors: r.errors ?? [] }
  }

  async scan(req: ScanRequest, signal?: AbortSignal): Promise<ScanResult> {
    return this.request<ScanResult>('POST', 'api/discovery/scan', req, { signal, timeoutMs: 300000 })
  }

  async probe(url: string, signal?: AbortSignal): Promise<Info> {
    const info = await this.request<unknown>('POST', 'api/discovery/probe', { url }, { signal, timeoutMs: 10000 })
    if (!isGos3Info(info)) throw new AdminError('InvalidResponse', 'The UI server returned an invalid identity document.', 200)
    return info
  }
}
