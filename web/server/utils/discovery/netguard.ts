// Outbound HTTP for the standalone UI's discovery probes, with a deny-list
// enforced where the connection is made (contract C6). Probes never reach
// cloud metadata endpoints, unspecified, multicast or broadcast addresses:
//
//   - IP literals are checked before connecting (Node skips the DNS lookup
//     for them);
//   - host names are resolved by our own lookup function, which Node's
//     socket uses to pick the address it connects to: every resolved
//     address is checked there, so a name that resolves to a metadata
//     address (or re-resolves to one: DNS rebinding) is refused at connect
//     time.
//
// General link-local (169.254.0.0/16, fe80::/10), loopback and private
// ranges stay allowed: zeroconf networks and local gos3 servers use them.
import { lookup as dnsLookup, type LookupAddress, type LookupAllOptions } from 'node:dns'
import { request as httpRequest, type IncomingMessage } from 'node:http'
import { request as httpsRequest } from 'node:https'
import { isIP, type LookupFunction } from 'node:net'
import { Readable } from 'node:stream'

/** Error code of a refused destination address. */
export const BLOCKED_ADDRESS_CODE = 'EGOS3BLOCKEDADDRESS'

/** The destination is (or resolves to) an address that is never probed. */
export class BlockedAddressError extends Error {
  readonly code = BLOCKED_ADDRESS_CODE
  constructor(readonly address: string) {
    super(`address ${address} is not allowed`)
    this.name = 'BlockedAddressError'
  }
}

/** Cloud metadata services (AWS IMDS / ECS / EKS pod identity, Alibaba). */
const METADATA_V4 = ['169.254.169.254', '169.254.170.2', '169.254.170.23', '100.100.100.200']
/** AWS IMDS over IPv6 (fd00:ec2::254). */
const METADATA_V6 = 'fd00:0ec2:0000:0000:0000:0000:0000:0254'

function v4ToInt(s: string): number | undefined {
  const parts = s.split('.')
  if (parts.length !== 4) return undefined
  let n = 0
  for (const p of parts) {
    if (!/^\d{1,3}$/.test(p)) return undefined
    const v = Number(p)
    if (v > 255) return undefined
    n = n * 256 + v
  }
  return n
}

const METADATA_V4_INTS = new Set(METADATA_V4.map(a => v4ToInt(a)!))

/** Expands a valid IPv6 literal (zone stripped) into 16 bytes. */
export function ipv6Bytes(addr: string): Uint8Array | undefined {
  let s = addr.split('%')[0]!.toLowerCase()
  if (isIP(s) !== 6) return undefined
  const out = new Uint8Array(16)
  // Trailing dotted quad (::ffff:1.2.3.4, 64:ff9b::1.2.3.4).
  let tail: number[] = []
  const lastColon = s.lastIndexOf(':')
  if (s.slice(lastColon + 1).includes('.')) {
    const v4 = v4ToInt(s.slice(lastColon + 1))
    if (v4 === undefined) return undefined
    tail = [v4 >>> 24, (v4 >>> 16) & 255, (v4 >>> 8) & 255, v4 & 255]
    s = `${s.slice(0, lastColon + 1)}0:0`
  }
  const halves = s.split('::')
  const head = halves[0] ? halves[0].split(':') : []
  const rest = halves.length > 1 && halves[1] ? halves[1].split(':') : []
  const groups = halves.length > 1 ? [...head, ...Array<string>(8 - head.length - rest.length).fill('0'), ...rest] : head
  if (groups.length !== 8) return undefined
  groups.forEach((g, i) => {
    const v = Number.parseInt(g, 16)
    out[i * 2] = v >> 8
    out[i * 2 + 1] = v & 255
  })
  if (tail.length) out.set(tail, 12)
  return out
}

function isBlockedV4(n: number): boolean {
  if (n >>> 24 === 0) return true // 0.0.0.0/8: unspecified ("this network")
  if (n >>> 28 === 0xe) return true // 224.0.0.0/4: multicast
  if (n === 0xffffffff) return true // limited broadcast
  return METADATA_V4_INTS.has(n)
}

/** IPv4 embedded in an IPv6 address that reaches it: mapped, compatible or NAT64. */
function embeddedV4(b: Uint8Array): number | undefined {
  const v4 = ((b[12]! << 24) | (b[13]! << 16) | (b[14]! << 8) | b[15]!) >>> 0
  const zero = (from: number, to: number) => b.slice(from, to).every(x => x === 0)
  // ::ffff:a.b.c.d (IPv4-mapped)
  if (zero(0, 10) && b[10] === 0xff && b[11] === 0xff) return v4
  // ::a.b.c.d (deprecated IPv4-compatible; :: and ::1 are not)
  if (zero(0, 12) && v4 > 1) return v4
  // 64:ff9b::a.b.c.d (NAT64 well-known prefix)
  if (b[0] === 0x00 && b[1] === 0x64 && b[2] === 0xff && b[3] === 0x9b && zero(4, 12)) return v4
  return undefined
}

/**
 * True when ip (an IPv4 or IPv6 literal, IPv6 optionally bracketed) must
 * never be probed: cloud metadata addresses, unspecified, multicast and
 * broadcast, including their IPv4-mapped / NAT64 IPv6 forms. Not an IP → false.
 */
export function isBlockedAddress(ip: string): boolean {
  const s = ip.trim().replace(/^\[(.*)\]$/, '$1')
  const family = isIP(s.split('%')[0]!)
  if (family === 4) return isBlockedV4(v4ToInt(s)!)
  if (family !== 6) return false
  const b = ipv6Bytes(s)
  if (!b) return true
  if (b.every(x => x === 0)) return true // ::
  if (b[0] === 0xff) return true // ff00::/8 multicast
  const hex = Array.from({ length: 8 }, (_, i) => ((b[i * 2]! << 8) | b[i * 2 + 1]!).toString(16).padStart(4, '0')).join(':')
  if (hex === METADATA_V6) return true
  const v4 = embeddedV4(b)
  return v4 !== undefined && isBlockedV4(v4)
}

/**
 * The host name of an http(s) URL when it is a blocked IP literal (after
 * WHATWG URL normalisation, so "http://2852039166" counts as
 * 169.254.169.254), else undefined.
 */
export function blockedUrlHost(url: string): string | undefined {
  let host: string
  try {
    host = new URL(url).hostname
  } catch {
    return undefined
  }
  const bare = host.replace(/^\[(.*)\]$/, '$1')
  return isIP(bare) && isBlockedAddress(bare) ? bare : undefined
}

/** Resolves a host name to all of its addresses (dns.lookup semantics). */
export type ResolveFn = (hostname: string, options: { family?: number, hints?: number }) => Promise<LookupAddress[]>

const systemResolve: ResolveFn = (hostname, options) => new Promise((resolve, reject) => {
  const opts: LookupAllOptions = { all: true, family: options.family as 0 | 4 | 6 | undefined, hints: options.hints }
  dnsLookup(hostname, opts, (err, addresses) => (err ? reject(err) : resolve(addresses)))
})

/**
 * A `lookup` for net/tls connect options that refuses the connection when
 * any address the name resolves to is blocked. It answers in the shape Node
 * asked for (`all: true` → an address list, else one address + family).
 */
export function guardedLookup(resolve: ResolveFn = systemResolve): LookupFunction {
  return (hostname, options, callback) => {
    const family = options.family === 'IPv4' ? 4 : options.family === 'IPv6' ? 6 : options.family
    resolve(hostname, { family, hints: options.hints }).then((addresses) => {
      const bad = addresses.find(a => isBlockedAddress(a.address))
      if (bad) return callback(new BlockedAddressError(bad.address), options.all ? [] : '', 0)
      if (!addresses.length) {
        const e: NodeJS.ErrnoException = Object.assign(new Error(`getaddrinfo ENOTFOUND ${hostname}`), { code: 'ENOTFOUND' })
        return callback(e, options.all ? [] : '', 0)
      }
      if (options.all) return callback(null, addresses)
      callback(null, addresses[0]!.address, addresses[0]!.family)
    }, (err: NodeJS.ErrnoException) => callback(err, options.all ? [] : '', 0))
  }
}

/** Statuses whose Response must not carry a body. */
const NULL_BODY = new Set([101, 103, 204, 205, 304])

function toResponse(res: IncomingMessage): Response {
  const status = res.statusCode ?? 0
  const headers = new Headers()
  const raw = res.rawHeaders
  for (let i = 0; i + 1 < raw.length; i += 2) {
    try {
      headers.append(raw[i]!, raw[i + 1]!)
    } catch {
      // header value a Headers object cannot hold: irrelevant for probes
    }
  }
  if (status < 200 || status > 599 || NULL_BODY.has(status)) {
    res.destroy()
    // A Response cannot carry a body with these statuses (or cannot hold
    // the status at all): answer bodyless, out-of-range statuses as 599.
    return new Response(null, { status: status >= 200 && status <= 599 ? status : 599, headers })
  }
  const body = Readable.toWeb(res) as unknown as ReadableStream<Uint8Array>
  return new Response(body, { status, headers })
}

/**
 * A fetch-like GET over node:http(s) that enforces the deny-list at connect
 * time (see guardedLookup). It never follows redirects, never uses proxies
 * and opens one connection per request (no pooled sockets).
 */
export function createGuardedFetch(opts: { resolve?: ResolveFn } = {}) {
  const lookup = guardedLookup(opts.resolve)
  return (input: string, init: RequestInit = {}): Promise<Response> => new Promise((resolve, reject) => {
    let url: URL
    try {
      url = new URL(input)
    } catch (e) {
      return reject(e)
    }
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return reject(new TypeError(`unsupported protocol ${url.protocol}`))
    const literal = url.hostname.replace(/^\[(.*)\]$/, '$1')
    if (isIP(literal) && isBlockedAddress(literal)) return reject(new BlockedAddressError(literal))
    if (init.signal?.aborted) return reject(init.signal.reason ?? new DOMException('aborted', 'AbortError'))
    const headers: Record<string, string> = {}
    new Headers(init.headers).forEach((v, k) => {
      headers[k] = v
    })
    const send = url.protocol === 'https:' ? httpsRequest : httpRequest
    const req = send(url, {
      method: 'GET',
      headers,
      agent: false,
      lookup,
      signal: init.signal ?? undefined
    }, res => resolve(toResponse(res)))
    req.once('error', reject)
    req.end()
  })
}

/** Default fetch of the discovery probes. */
export const guardedFetch = createGuardedFetch()
