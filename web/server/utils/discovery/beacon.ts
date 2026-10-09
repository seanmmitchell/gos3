// UDP multicast beacon messages (docs/discovery.md §3): encoding, decoding
// and turning announces into probe candidates. Pure functions; the socket
// handling lives in ./local.ts.
import { randomBytes } from 'node:crypto'
import { isIP } from 'node:net'
import { baseUrl, DEFAULT_PORT } from './targets'

export const BEACON_PROTO = 'gos3-beacon/1'
export const BEACON_GROUP = '239.255.73.83'
export const BEACON_PORT = 7373
export const MAX_BEACON_SIZE = 1400
const MAX_BEACON_URLS = 16
const MAX_FIELD_LEN = 128
const MAX_NONCE_LEN = 64
const MAX_URL_LEN = 2048

export interface BeaconAnnounce {
  proto: typeof BEACON_PROTO
  type: 'announce'
  id: string
  name?: string
  version?: string
  region?: string
  urls?: string[]
  info?: string
  ts?: number
}

export interface BeaconQuery {
  proto: typeof BEACON_PROTO
  type: 'query'
  nonce?: string
}

export interface BeaconBye {
  proto: typeof BEACON_PROTO
  type: 'bye'
  id: string
}

export type BeaconMessage = BeaconAnnounce | BeaconQuery | BeaconBye

/** Server IDs: short URL-safe tokens (16 hex chars in practice). */
export function isValidPeerId(id: unknown): id is string {
  return typeof id === 'string' && /^[A-Za-z0-9_-]{1,64}$/.test(id)
}

export function newNonce(): string {
  return randomBytes(8).toString('hex')
}

/** Encodes a query datagram. */
export function encodeQuery(nonce: string = newNonce()): Buffer {
  const msg: BeaconQuery = { proto: BEACON_PROTO, type: 'query', nonce }
  return Buffer.from(JSON.stringify(msg), 'utf8')
}

function cleanField(v: unknown): string | undefined {
  if (typeof v !== 'string') return undefined
  // Strip control characters and clip.
  // eslint-disable-next-line no-control-regex
  const s = v.replace(/[\u0000-\u001f\u007f]/g, '').trim()
  return s ? s.slice(0, MAX_FIELD_LEN) : undefined
}

/**
 * Decodes one datagram. Returns null for invalid JSON, other protocols,
 * unknown types and malformed messages (they are silently ignored).
 */
export function decodeBeacon(data: Uint8Array | string): BeaconMessage | null {
  let text: string
  if (typeof data === 'string') {
    text = data
  } else {
    if (data.byteLength === 0 || data.byteLength > MAX_BEACON_SIZE) return null
    try {
      text = new TextDecoder('utf-8', { fatal: true }).decode(data)
    } catch {
      return null
    }
  }
  if (!text || text.length > MAX_BEACON_SIZE) return null
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch {
    return null
  }
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return null
  const o = raw as Record<string, unknown>
  if (o.proto !== BEACON_PROTO) return null
  switch (o.type) {
    case 'announce': {
      if (!isValidPeerId(o.id)) return null
      const urls = Array.isArray(o.urls)
        ? o.urls.filter((u): u is string => typeof u === 'string' && u.length <= MAX_URL_LEN).slice(0, MAX_BEACON_URLS)
        : undefined
      return {
        proto: BEACON_PROTO,
        type: 'announce',
        id: o.id,
        name: cleanField(o.name),
        version: cleanField(o.version),
        region: cleanField(o.region),
        urls,
        info: typeof o.info === 'string' ? o.info : undefined,
        ts: typeof o.ts === 'number' ? o.ts : undefined
      }
    }
    case 'query':
      if (o.nonce !== undefined && (typeof o.nonce !== 'string' || o.nonce.length > MAX_NONCE_LEN)) return null
      return { proto: BEACON_PROTO, type: 'query', nonce: o.nonce as string | undefined }
    case 'bye':
      if (!isValidPeerId(o.id)) return null
      return { proto: BEACON_PROTO, type: 'bye', id: o.id }
    default:
      return null
  }
}

function isWildcardHost(host: string): boolean {
  return host === '' || host === '0.0.0.0' || host === '::' || host === '[::]'
}

function isLoopbackHost(host: string): boolean {
  const h = host.replace(/^\[|\]$/g, '').toLowerCase()
  return h === 'localhost' || h === '::1' || /^127\./.test(h)
}

/**
 * Turns the URLs of an announce received from srcIp into probe candidates.
 * Wildcard hosts, and loopback hosts announced from a non-loopback source,
 * are replaced by the source IP (keeping scheme and port). Without any usable
 * URL, the source IP with the default port is used.
 */
export function beaconCandidateUrls(urls: readonly string[] | undefined, srcIp: string | undefined): string[] {
  const src = srcIp && isIP(srcIp) && !isWildcardHost(srcIp) && !/^2(2[4-9]|3\d)\./.test(srcIp) ? srcIp : undefined
  const out: string[] = []
  const add = (u: string) => {
    if (out.length < MAX_BEACON_URLS && !out.includes(u)) out.push(u)
  }
  for (const raw of urls ?? []) {
    if (typeof raw !== 'string' || raw.length > MAX_URL_LEN) continue
    // "http://:9000" (host omitted) is not a valid WHATWG URL; handle it first.
    const hostless = /^(https?):\/\/:(\d{1,5})\/?$/i.exec(raw.trim())
    let scheme: string
    let host: string
    let port: number
    if (hostless) {
      scheme = hostless[1]!.toLowerCase()
      host = ''
      port = Number(hostless[2])
    } else {
      let u: URL
      try {
        u = new URL(raw)
      } catch {
        continue
      }
      if (u.protocol !== 'http:' && u.protocol !== 'https:') continue
      scheme = u.protocol.slice(0, -1)
      host = u.hostname
      port = u.port ? Number(u.port) : scheme === 'https' ? 443 : 80
      if (!isWildcardHost(host) && !(isLoopbackHost(host) && src && !isLoopbackHost(src))) {
        add(`${scheme}://${u.host}${u.pathname.replace(/\/+$/, '')}`)
        continue
      }
    }
    if (port < 1 || port > 65535) continue
    if (src) add(baseUrl(scheme, src, port))
  }
  if (out.length === 0 && src) add(baseUrl('http', src, DEFAULT_PORT))
  return out
}
