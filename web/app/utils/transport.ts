// One-hop, browser-signed relay transport shared by S3, admin and probes.
import type { Connection } from '~/types/connection'
import { AwsClient } from './s3/signing'
import { bytesToBase64, bytesToHex } from './s3/hash'
import { sha256 } from '@noble/hashes/sha2.js'

export type TransportConnection = Pick<Connection, 'id' | 'endpoint' | 'region' | 'accessKeyId' | 'secretAccessKey'> & { proxyConnectionId?: string }
export interface RequestTransport {
  send(request: Request, send?: Sender): Promise<Response>
  presign(destination: string, expiresSeconds: number): Promise<string>
}
type Sender = (request: Request) => Promise<Response>
let resolveConnection: (id: string) => Connection | undefined = () => undefined
let routeChanged: (id: string, relayId?: string) => void = () => {}
const transports = new Map<string, { signature: string, transport: ConnectionTransport }>()

export function configureTransports(resolve: typeof resolveConnection, changed: typeof routeChanged): void {
  resolveConnection = resolve
  routeChanged = changed
}
export function clearTransport(id?: string): void {
  if (id === undefined) transports.clear()
  else transports.delete(id)
}
export function getTransport(c: TransportConnection): ConnectionTransport {
  const relay = c.proxyConnectionId ? resolveConnection(c.proxyConnectionId) : undefined
  const fields = (v: TransportConnection | undefined) => v && [v.id, v.endpoint, v.region, v.accessKeyId, v.secretAccessKey, v.proxyConnectionId]
  const signature = JSON.stringify([fields(c), fields(relay)])
  const cached = transports.get(c.id)
  if (cached?.signature === signature) return cached.transport
  const transport = new ConnectionTransport(c)
  transports.set(c.id, { signature, transport })
  return transport
}
export class TransportError extends Error {
  constructor(readonly code: 'ProxyUnavailable' | 'ProxyFailed' | 'WriteOutcomeUnknown', message: string, readonly status = 0) { super(message) }
}
function signer(c: TransportConnection): AwsClient {
  return new AwsClient({ accessKeyId: c.accessKeyId, secretAccessKey: c.secretAccessKey, region: c.region, service: 's3', retries: 0 })
}
function metadata(request: Request): string {
  const headers: Record<string, string> = {}
  request.headers.forEach((v, k) => {
    if (!['host', 'content-length', 'cookie', 'origin', 'referer'].includes(k)) headers[k] = v
  })
  return bytesToBase64(new TextEncoder().encode(JSON.stringify({ url: request.url, method: request.method, headers })))
    .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}
async function bodyHash(body: Blob): Promise<string> {
  const hash = sha256.create()
  // Bound memory even when a caller explicitly uploads a large single object.
  for (let offset = 0; offset < body.size; offset += 1024 * 1024) {
    hash.update(new Uint8Array(await body.slice(offset, offset + 1024 * 1024).arrayBuffer()))
  }
  return bytesToHex(hash.digest())
}

export class ConnectionTransport {
  private relayActive = false
  private verifying?: Promise<Connection>
  constructor(private readonly connection: TransportConnection, private readonly network: Sender = request => globalThis.fetch(request)) {}
  get proxyConnectionId(): string | undefined { return this.relayActive ? this.connection.proxyConnectionId : undefined }

  private async relay(): Promise<Connection> {
    const c = this.connection
    const relay = c.proxyConnectionId ? resolveConnection(c.proxyConnectionId) : undefined
    if (!relay || relay.id === c.id || relay.endpoint === c.endpoint || relay.kind !== 'gos3' || relay.proxyConnectionId || !relay.accessKeyId || !relay.secretAccessKey) {
      throw new TransportError('ProxyUnavailable', 'The fallback relay must be a different, directly reachable gos3 connection with credentials.')
    }
    if (!this.verifying) {
      this.verifying = (async () => {
        const req = await signer(relay).sign(relay.endpoint + '/_gos3/admin/v1/whoami', { signal: AbortSignal.timeout(5000) })
        const res = await this.network(req)
        const identity = await res.json() as { capabilities?: { proxy?: boolean } }
        if (!res.ok || identity.capabilities?.proxy !== true) throw new Error('ProxyRequest permission or relay support is missing')
        return relay
      })().catch(() => { this.verifying = undefined; throw new TransportError('ProxyUnavailable', `Cannot use ${relay.name} as a relay. Check its reachability, credentials, and admin:ProxyRequest permission.`) })
    }
    return this.verifying
  }
  private activate(): void {
    this.relayActive = true
    // An old request may finish after edits or a direct-access retest. Only
    // the currently registered transport may publish its route to the store.
    if (transports.get(this.connection.id)?.transport === this) {
      routeChanged(this.connection.id, this.connection.proxyConnectionId)
    }
  }
  private async prepare(request: Request): Promise<Request> {
    const relay = await this.relay()
    const url = new URL(relay.endpoint + '/_gos3/admin/v1/proxy')
    url.searchParams.set('request', metadata(request))
    const body = await request.blob()
    const digest = await bodyHash(body)
    if (request.signal.aborted) throw new DOMException('Aborted', 'AbortError')
    return signer(relay).sign(url.toString(), {
      method: request.method === 'HEAD' ? 'HEAD' : 'POST', body: body.size ? body : undefined,
      headers: { 'X-Amz-Content-Sha256': digest }, signal: request.signal
    })
  }
  private async through(request: Request, send: Sender): Promise<Response> {
    let response: Response
    try { response = await send(await this.prepare(request)) }
    catch (error) {
      if (request.signal.aborted) throw error
      if (error instanceof TransportError) throw error
      if (request.method !== 'GET' && request.method !== 'HEAD') throw new TransportError('WriteOutcomeUnknown', 'The relayed write may already have succeeded. Check the destination before explicitly retrying it.')
      throw new TransportError('ProxyFailed', 'The relay request failed. Check the relay connection.')
    }
    if (response.headers.get('X-Gos3-Proxy') !== 'upstream') {
      throw new TransportError('ProxyFailed', 'The relay refused the request or could not reach the destination.', response.status)
    }
    this.activate()
    // Network responses stay uncacheable, but object metadata describes the
    // destination's Cache-Control rather than the relay's no-store policy.
    const headers = new Headers(response.headers)
    const cacheControl = headers.get('X-Gos3-Proxy-Cache-Control')
    if (cacheControl) headers.set('Cache-Control', cacheControl)
    else headers.delete('Cache-Control')
    return new Response(response.body, { status: response.status, statusText: response.statusText, headers })
  }
  async send(request: Request, send: Sender = this.network): Promise<Response> {
    if (this.relayActive) return this.through(request, send)
    if (!this.connection.proxyConnectionId) return send(request)
    const read = request.method === 'GET' || request.method === 'HEAD'
    // Retain only reads for replay. A failed write is never sent a second time.
    const retry = read ? request.clone() : undefined
    try {
      if (!read) return await send(request)
      const controller = new AbortController()
      const timer = setTimeout(() => controller.abort(), 5000)
      try {
        return await send(new Request(request, { signal: AbortSignal.any([request.signal, controller.signal]) }))
      } finally { clearTimeout(timer) }
    } catch (error) {
      if (request.signal.aborted) throw error
      if (retry) return this.through(retry, send)
      // Establish the route with a safe HEAD; leave the failed write uncertain.
      try {
        const probe = await signer(this.connection).sign(this.connection.endpoint + '/', { method: 'HEAD', signal: AbortSignal.timeout(5000) })
        await this.through(probe, this.network)
      } catch { /* The original write still has an uncertain outcome. */ }
      throw new TransportError('WriteOutcomeUnknown', 'The write may already have succeeded. Check the destination before explicitly retrying it. Automatic fallback did not replay this write.')
    }
  }
  async presign(destination: string, expiresSeconds: number): Promise<string> {
    if (!this.connection.proxyConnectionId) return destination
    if (!this.relayActive) {
      const probe = await signer(this.connection).sign(this.connection.endpoint + '/', { method: 'HEAD' })
      await this.send(probe)
    }
    if (!this.relayActive) return destination
    const relay = await this.relay()
    const target = new URL(destination)
    const timestamp = target.searchParams.get('X-Amz-Date')!
    const date = Date.UTC(+timestamp.slice(0, 4), +timestamp.slice(4, 6) - 1, +timestamp.slice(6, 8), +timestamp.slice(9, 11), +timestamp.slice(11, 13), +timestamp.slice(13, 15))
    const remaining = Math.floor((date + Number(target.searchParams.get('X-Amz-Expires')) * 1000 - Date.now()) / 1000)
    if (!Number.isFinite(remaining) || remaining < 1) throw new TransportError('ProxyFailed', 'Destination URL has expired.')
    const url = new URL(relay.endpoint + '/_gos3/admin/v1/proxy')
    url.searchParams.set('request', metadata(new Request(destination)))
    url.searchParams.set('X-Amz-Expires', String(Math.max(1, Math.min(expiresSeconds, remaining, 604800))))
    return (await signer(relay).sign(url.toString(), { aws: { signQuery: true } } as RequestInit)).url
  }
}
