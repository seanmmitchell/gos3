// Scan target parsing and expansion for the standalone UI's local network
// scan (POST /api/discovery/scan). Mirrors the gos3 server's semantics
// (internal/discovery/scan.go, docs/discovery.md §6):
//
//   targets: base URLs ("http://nas:9000"), IPv4 CIDRs ("10.0.0.0/24"),
//            IP literals or host names, optionally with ":port"
//   × ports (default [9000]) × schemes (default ["http"])
//
// CIDRs skip the network and broadcast addresses for /0–/30 and the total
// number of hosts is capped (MAX_SCAN_HOSTS). Pure functions: unit-tested in
// test/unit/discovery-server.test.ts.
import { isIP } from 'node:net'
import { blockedUrlHost, isBlockedAddress } from './netguard'

export const DEFAULT_PORT = 9000
/** Hard cap of hosts a single scan may expand to. */
export const MAX_SCAN_HOSTS = 1024
export const MAX_SCAN_TARGETS = 1024
export const MAX_SCAN_PORTS = 64
/** Cap of probe URLs (hosts × ports × schemes) per scan. */
export const MAX_SCAN_PROBES = 4096
export const DEFAULT_SCAN_TIMEOUT_MS = 1500
export const MIN_SCAN_TIMEOUT_MS = 10
export const MAX_SCAN_TIMEOUT_MS = 30000
export const DEFAULT_SCAN_CONCURRENCY = 64
export const MAX_SCAN_CONCURRENCY = 64
const MAX_URL_LEN = 2048

/** Invalid scan request (→ HTTP 400). */
export class ScanRequestError extends Error {
  readonly status = 400
  constructor(message: string) {
    super(message)
    this.name = 'ScanRequestError'
  }
}

function clip(s: string, n = 64): string {
  return s.length > n ? `${s.slice(0, n)}…` : s
}

// -- IPv4 ---------------------------------------------------------------------

/** Parses a dotted-quad IPv4 address into an unsigned 32-bit integer. */
export function parseIPv4(s: string): number | undefined {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s)
  if (!m) return undefined
  let n = 0
  for (let i = 1; i <= 4; i++) {
    const part = m[i]!
    if (part.length > 1 && part.startsWith('0')) return undefined // no octal-looking octets
    const v = Number(part)
    if (v > 255) return undefined
    n = n * 256 + v
  }
  return n
}

export function formatIPv4(n: number): string {
  return [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join('.')
}

export interface Cidr {
  /** Network address (masked) as uint32. */
  base: number
  bits: number
}

/** Parses an IPv4 CIDR ("10.0.0.0/24"); the address is masked. */
export function parseCidr(s: string): Cidr | undefined {
  const m = /^([^/]+)\/(\d{1,2})$/.exec(s.trim())
  if (!m) return undefined
  const ip = parseIPv4(m[1]!)
  const bits = Number(m[2])
  if (ip === undefined || bits > 32) return undefined
  const size = 2 ** (32 - bits)
  return { base: Math.floor(ip / size) * size, bits }
}

/** Number of scanned hosts of a CIDR (network/broadcast skipped for /0–/30). */
export function cidrHostCount(bits: number): number {
  const n = 2 ** (32 - bits)
  return bits <= 30 ? n - 2 : n
}

/** Expands an IPv4 CIDR into host addresses. Throws ScanRequestError above limit. */
export function expandCidr(cidr: string | Cidr, limit = MAX_SCAN_HOSTS): string[] {
  const c = typeof cidr === 'string' ? parseCidr(cidr) : cidr
  if (!c) throw new ScanRequestError(`"${clip(String(cidr))}" is not a valid IPv4 CIDR`)
  const count = cidrHostCount(c.bits)
  if (count > limit) throw new ScanRequestError(`CIDR /${c.bits} has ${count} hosts; the limit is ${limit}`)
  const first = c.bits <= 30 ? c.base + 1 : c.base
  const out: string[] = []
  for (let i = 0; i < count; i++) out.push(formatIPv4(first + i))
  return out
}

// -- hosts & URLs -------------------------------------------------------------

/** DNS host name: labels of letters, digits, '-' and '_' (≤ 63 each, ≤ 253 total). */
export function isValidHostname(h: string): boolean {
  const s = h.replace(/\.$/, '')
  if (!s || s.length > 253) return false
  return s.split('.').every(l => l.length > 0 && l.length <= 63 && /^[A-Za-z0-9_-]+$/.test(l))
}

/** "<scheme>://<host>:<port>" with IPv6 literals bracketed. */
export function baseUrl(scheme: string, host: string, port: number): string {
  const h = host.includes(':') && !host.startsWith('[') ? `[${host}]` : host
  return `${scheme}://${h}:${port}`
}

/**
 * Normalises a base URL: http/https only, no credentials/query/fragment,
 * lower-case scheme/host, no trailing slash. Throws ScanRequestError.
 */
export function normalizeBaseUrl(raw: string): string {
  const s = raw.trim()
  if (!s || s.length > MAX_URL_LEN) throw new ScanRequestError('empty or oversized URL')
  let u: URL
  try {
    u = new URL(/^[a-z][a-z0-9+.-]*:\/\//i.test(s) ? s : `http://${s}`)
  } catch {
    throw new ScanRequestError(`"${clip(s)}" is not a valid URL`)
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new ScanRequestError(`"${clip(s)}": only http and https are supported`)
  if (!u.hostname) throw new ScanRequestError(`"${clip(s)}" has no host`)
  if (u.username || u.password) throw new ScanRequestError(`"${clip(s)}": URLs with credentials are not allowed`)
  const path = u.pathname.replace(/\/+$/, '')
  return `${u.protocol}//${u.host}${path}`
}

/**
 * Throws ScanRequestError when url's host is an IP literal that is never
 * probed (cloud metadata, unspecified, multicast, broadcast; contract C6).
 */
export function assertProbeableUrl(url: string, raw: string = url): void {
  const blocked = blockedUrlHost(url)
  if (blocked) throw new ScanRequestError(`"${clip(raw.trim())}": ${blocked} is a cloud metadata, unspecified, multicast or broadcast address, which is never probed`)
}

export type ScanTarget =
  | { kind: 'url', url: string }
  | { kind: 'cidr', cidr: Cidr }
  | { kind: 'host', host: string, port?: number }

function parsePort(s: string): number | undefined {
  if (!/^\d{1,5}$/.test(s)) return undefined
  const n = Number(s)
  return n >= 1 && n <= 65535 ? n : undefined
}

/**
 * Classifies one target: base URL (contains "://"), CIDR (contains "/"), or
 * host[:port] (IP literal or host name; bracket IPv6 literals with a port).
 */
export function parseScanTarget(raw: string): ScanTarget {
  const s = raw.trim()
  if (!s || s.length > MAX_URL_LEN) throw new ScanRequestError('empty or oversized target')
  if (s.includes('://')) return { kind: 'url', url: normalizeBaseUrl(s) }
  if (s.includes('/')) {
    const [addr] = s.split('/')
    if (addr && isIP(addr) === 6) throw new ScanRequestError(`"${clip(s)}": IPv6 CIDRs are not supported; list hosts instead`)
    const cidr = parseCidr(s)
    if (!cidr) throw new ScanRequestError(`"${clip(s)}" is not a valid CIDR`)
    return { kind: 'cidr', cidr }
  }
  if (isIP(s)) return { kind: 'host', host: s.toLowerCase() } // bare IPv4/IPv6 literal
  let host = s
  let port: number | undefined
  const bracket = /^\[([^\]]+)\](?::(\d+))?$/.exec(s)
  if (bracket) {
    host = bracket[1]!
    if (isIP(host) !== 6) throw new ScanRequestError(`"${clip(s)}" is not a valid IPv6 literal`)
    if (bracket[2] !== undefined) {
      port = parsePort(bracket[2])
      if (port === undefined) throw new ScanRequestError(`"${clip(s)}" has an invalid port`)
    }
    return { kind: 'host', host: host.toLowerCase(), port }
  }
  const colon = s.lastIndexOf(':')
  if (colon >= 0) {
    host = s.slice(0, colon)
    port = parsePort(s.slice(colon + 1))
    if (port === undefined) throw new ScanRequestError(`"${clip(s)}" has an invalid port`)
  }
  host = host.replace(/\.$/, '')
  if (isIP(host)) return { kind: 'host', host, port }
  if (!isValidHostname(host)) throw new ScanRequestError(`"${clip(s)}" is not a host name, IP, CIDR or URL`)
  return { kind: 'host', host: host.toLowerCase(), port }
}

// -- plan -----------------------------------------------------------------------

export interface ScanPlan {
  /** Probe base URLs, deduplicated, in target order. */
  urls: string[]
  hosts: number
  timeoutMs: number
  concurrency: number
}

export interface PlanOptions {
  maxHosts?: number
  maxProbes?: number
}

function asRecord(v: unknown): Record<string, unknown> {
  if (typeof v !== 'object' || v === null || Array.isArray(v)) throw new ScanRequestError('the request body must be a JSON object')
  return v as Record<string, unknown>
}

/**
 * Validates a ScanRequest body ({targets, ports?, schemes?, timeout? (ns),
 * concurrency?}) and expands it into probe URLs. Throws ScanRequestError.
 */
export function planScan(body: unknown, opts: PlanOptions = {}): ScanPlan {
  const maxHosts = opts.maxHosts ?? MAX_SCAN_HOSTS
  const maxProbes = opts.maxProbes ?? MAX_SCAN_PROBES
  const req = asRecord(body)

  if (!Array.isArray(req.targets) || req.targets.length === 0) throw new ScanRequestError('no targets')
  if (req.targets.length > MAX_SCAN_TARGETS) throw new ScanRequestError(`at most ${MAX_SCAN_TARGETS} targets`)
  if (!req.targets.every(t => typeof t === 'string')) throw new ScanRequestError('targets must be strings')

  const ports: number[] = []
  if (req.ports !== undefined && req.ports !== null) {
    if (!Array.isArray(req.ports)) throw new ScanRequestError('ports must be an array of numbers')
    if (req.ports.length > MAX_SCAN_PORTS) throw new ScanRequestError(`at most ${MAX_SCAN_PORTS} ports`)
    for (const p of req.ports) {
      if (typeof p !== 'number' || !Number.isInteger(p) || p < 1 || p > 65535) throw new ScanRequestError(`invalid port ${clip(String(p), 16)}`)
      if (!ports.includes(p)) ports.push(p)
    }
  }
  if (!ports.length) ports.push(DEFAULT_PORT)

  const schemes: string[] = []
  if (req.schemes !== undefined && req.schemes !== null) {
    if (!Array.isArray(req.schemes)) throw new ScanRequestError('schemes must be an array of strings')
    for (const raw of req.schemes) {
      const s = typeof raw === 'string' ? raw.trim().toLowerCase() : ''
      if (s !== 'http' && s !== 'https') throw new ScanRequestError(`unsupported scheme "${clip(String(raw), 16)}"`)
      if (!schemes.includes(s)) schemes.push(s)
    }
  }
  if (!schemes.length) schemes.push('http')

  let timeoutMs = DEFAULT_SCAN_TIMEOUT_MS
  if (req.timeout !== undefined && req.timeout !== null) {
    if (typeof req.timeout !== 'number' || !Number.isFinite(req.timeout)) throw new ScanRequestError('timeout must be a number of nanoseconds')
    if (req.timeout > 0) timeoutMs = Math.min(Math.max(req.timeout / 1e6, MIN_SCAN_TIMEOUT_MS), MAX_SCAN_TIMEOUT_MS)
  }

  let concurrency = DEFAULT_SCAN_CONCURRENCY
  if (req.concurrency !== undefined && req.concurrency !== null) {
    if (typeof req.concurrency !== 'number' || !Number.isFinite(req.concurrency)) throw new ScanRequestError('concurrency must be a number')
    if (req.concurrency > 0) concurrency = Math.min(Math.floor(req.concurrency), MAX_SCAN_CONCURRENCY) || 1
  }

  const urls: string[] = []
  const seen = new Set<string>()
  let hosts = 0
  const push = (u: string) => {
    if (seen.has(u)) return
    if (urls.length >= maxProbes) throw new ScanRequestError(`the scan expands to more than ${maxProbes} probes; narrow the targets, ports or schemes`)
    seen.add(u)
    urls.push(u)
  }
  const addHosts = (n: number) => {
    hosts += n
    if (hosts > maxHosts) throw new ScanRequestError(`the scan expands to more than ${maxHosts} hosts`)
  }
  for (const raw of req.targets as string[]) {
    const t = parseScanTarget(raw)
    // An explicit target that is a blocked address is an error; blocked
    // addresses inside a CIDR are skipped. Host names are checked when the
    // probe connects (./netguard.ts).
    const explicit = (u: string) => {
      assertProbeableUrl(u, raw)
      push(u)
    }
    if (t.kind === 'url') {
      addHosts(1)
      explicit(t.url)
    } else if (t.kind === 'host') {
      addHosts(1)
      for (const port of t.port ? [t.port] : ports) for (const scheme of schemes) explicit(baseUrl(scheme, t.host, port))
    } else {
      const n = cidrHostCount(t.cidr.bits)
      addHosts(n)
      for (const ip of expandCidr(t.cidr, maxHosts)) {
        if (isBlockedAddress(ip)) continue
        for (const port of ports) for (const scheme of schemes) push(baseUrl(scheme, ip, port))
      }
    }
  }
  return { urls, hosts, timeoutMs, concurrency }
}

/** Formats elapsed milliseconds like a Go duration string ("850ms", "1.234s"). */
export function formatGoDuration(ms: number): string {
  if (ms < 1000) return `${Math.max(0, Math.round(ms))}ms`
  return `${(ms / 1000).toFixed(3).replace(/\.?0+$/, '')}s`
}
