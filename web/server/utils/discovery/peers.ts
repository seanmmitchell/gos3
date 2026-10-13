// Peer bookkeeping for local discovery: identity documents are validated and
// bounded (parseInfo), then observations are merged by server ID like the
// gos3 peer table (docs/discovery.md "Peer lifecycle", "Peer identity").
//
// Nothing a discovered host sends is authenticated: server IDs can be
// claimed by anyone. A verified observation therefore never takes over the
// identity or endpoints of a server already verified at another address in
// the same result; the claim is reported in `error` instead
// (mergePeer). Every list is capped and merged in linear time, so a hostile
// document cannot make one discovery or scan slow or large.
import type { DiscoverySource, Endpoints, Info, Peer } from '~/types/admin'
import { isValidPeerId } from './beacon'

/** Bounds of a sanitised identity document and of a merged peer (as the gos3 server). */
export const MAX_FIELD_LEN = 256
export const MAX_URL_LEN = 2048
export const MAX_FEATURES = 64
export const MAX_FEATURE_LEN = 64
export const MAX_ENDPOINTS_ALL = 64
export const MAX_ADDRESSES = 32
const MAX_SOURCES = 8
const MAX_ERROR_LEN = 512

/**
 * Makes an untrusted string safe to keep and display: no C0/C1 control
 * characters, bidi overrides/isolates or line/paragraph separators (they
 * would let a peer spoof names), trimmed and at most max UTF-16 units.
 * Anything but a string yields ''.
 */
export function cleanText(v: unknown, max: number = MAX_FIELD_LEN): string {
  if (typeof v !== 'string') return ''
  // eslint-disable-next-line no-control-regex
  let s = v.replace(/[\u0000-\u001f\u007f-\u009f\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069\u2028\u2029]/g, '').trim()
  if (s.length > max) {
    s = s.slice(0, max)
    if (/[\ud800-\udbff]$/.test(s)) s = s.slice(0, -1) // no lone high surrogate
  }
  return s
}

/**
 * An endpoint from an identity document: an absolute http(s) URL or an
 * absolute path ("/_gos3/admin/v1"), at most MAX_URL_LEN long; else ''.
 */
function cleanEndpoint(v: unknown): string {
  const s = cleanText(v, MAX_URL_LEN + 1)
  if (!s || s.length > MAX_URL_LEN) return ''
  if (s.startsWith('/')) return s.startsWith('//') ? '' : s
  try {
    const u = new URL(s)
    return u.protocol === 'http:' || u.protocol === 'https:' ? s : ''
  } catch {
    return ''
  }
}

function record(v: unknown): Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v) ? v as Record<string, unknown> : {}
}

function cleanList(v: unknown, maxItems: number, clean: (x: unknown) => string): string[] {
  if (!Array.isArray(v)) return []
  const out = new Set<string>()
  for (const x of v) {
    if (out.size >= maxItems) break
    const s = clean(x)
    if (s) out.add(s)
  }
  return [...out]
}

/**
 * Bounds and cleans every field of an identity document received from the
 * network (service and id must have been validated): strings are cleaned
 * and clipped, endpoints must be http(s) URLs or paths, `endpoints.all` and
 * `features` are lists of strings with at most MAX_ENDPOINTS_ALL /
 * MAX_FEATURES entries. Unknown fields are dropped.
 */
function sanitizeInfo(o: Record<string, unknown>): Info {
  const ep = record(o.endpoints)
  const endpoints: Endpoints = { s3: cleanEndpoint(ep.s3), admin: cleanEndpoint(ep.admin) }
  const ui = cleanEndpoint(ep.ui)
  if (ui) endpoints.ui = ui
  const all = cleanList(ep.all, MAX_ENDPOINTS_ALL, cleanEndpoint)
  if (all.length) endpoints.all = all
  return {
    service: 'gos3',
    id: o.id as string,
    name: cleanText(o.name),
    version: cleanText(o.version),
    apiVersion: cleanText(o.apiVersion, 32),
    region: cleanText(o.region),
    endpoints,
    features: cleanList(o.features, MAX_FEATURES, f => cleanText(f, MAX_FEATURE_LEN)),
    startedAt: cleanText(o.startedAt, 64)
  }
}

/**
 * Validates and sanitises a /_gos3/info document: a JSON object with
 * `"service": "gos3"`, a valid server ID and a string `name`. Returns the
 * bounded document (see sanitizeInfo), or undefined when it is not one.
 */
export function parseInfo(v: unknown): Info | undefined {
  if (typeof v !== 'object' || v === null || Array.isArray(v)) return undefined
  const o = v as Record<string, unknown>
  if (o.service !== 'gos3' || !isValidPeerId(o.id) || typeof o.name !== 'string') return undefined
  return sanitizeInfo(o)
}

/** Validates a /_gos3/info document (see parseInfo). */
export function isInfo(v: unknown): v is Info {
  return parseInfo(v) !== undefined
}

/**
 * Union of two lists, in order, at most max entries, in linear time
 * (non-array inputs count as empty).
 */
function union<T>(a: unknown, b: unknown, max: number): T[] {
  const out = new Set<T>()
  for (const list of [a, b]) {
    if (!Array.isArray(list)) continue
    for (const v of list as T[]) {
      if (out.size >= max) return [...out]
      out.add(v)
    }
  }
  return [...out]
}

/** A verified peer built from an identity document fetched at baseUrl. */
export function peerFromInfo(info: Info, baseUrl: string, source: DiscoverySource, now: Date = new Date()): Peer {
  const ts = now.toISOString()
  // Sanitised again: callers may pass a document that did not come from parseInfo.
  const i = sanitizeInfo(info as unknown as Record<string, unknown>)
  const s3 = i.endpoints.s3 || baseUrl
  return {
    id: i.id,
    name: i.name,
    version: i.version,
    region: i.region,
    endpoints: { ...i.endpoints, s3, admin: i.endpoints.admin || `${s3.replace(/\/+$/, '')}/_gos3/admin/v1` },
    features: i.features,
    sources: [source],
    addresses: [baseUrl],
    firstSeen: ts,
    lastSeen: ts,
    healthy: true,
    verified: true
  }
}

/** Hint about a peer that was seen (beacon/mDNS) but could not be verified. */
export interface Sighting {
  id: string
  name?: string
  version?: string
  region?: string
  urls: string[]
  source: DiscoverySource
  error?: string
}

/** An unverified peer from a sighting (healthy = verified = false). */
export function peerFromSighting(s: Sighting, now: Date = new Date()): Peer {
  const ts = now.toISOString()
  const urls = s.urls.slice(0, MAX_ADDRESSES)
  return {
    id: s.id,
    name: cleanText(s.name) || s.id,
    version: cleanText(s.version),
    region: cleanText(s.region),
    endpoints: { s3: urls[0] ?? '', admin: urls[0] ? `${urls[0]}/_gos3/admin/v1` : '' },
    sources: [s.source],
    addresses: urls,
    firstSeen: ts,
    lastSeen: ts,
    healthy: false,
    verified: false,
    error: s.error === undefined ? undefined : cleanText(s.error, MAX_ERROR_LEN)
  }
}

/** Comparable form of a base URL ("http://H:80/" → "http://h"), or the input. */
function baseKey(url: string): string {
  try {
    const u = new URL(url)
    return `${u.protocol}//${u.host}${u.pathname.replace(/\/+$/, '')}`
  } catch {
    return url
  }
}

/** Comparable forms of the base URLs a verified document advertises (endpoints.s3, endpoints.all). */
function advertised(p: Peer): Set<string> {
  return new Set([p.endpoints.s3, ...(Array.isArray(p.endpoints.all) ? p.endpoints.all : [])].map(baseKey))
}

/**
 * True when claim is verified peer known reached at another address: the
 * two were observed at a shared address, or each one's own document
 * advertises an address the other was observed at. A one-sided claim is
 * not enough: a host that answered first could otherwise advertise the
 * real server's address and absorb its entry.
 */
function sameServer(known: Peer, claim: Peer): boolean {
  const seen = new Set(known.addresses.map(baseKey))
  if (claim.addresses.some(a => seen.has(baseKey(a)))) return true
  const byKnown = advertised(known)
  const byClaim = advertised(claim)
  return claim.addresses.some(a => byKnown.has(baseKey(a))) && known.addresses.some(a => byClaim.has(baseKey(a)))
}

function sameEndpoints(a: Endpoints, b: Endpoints): boolean {
  const all = (e: Endpoints) => (e.all ?? []).join('\n')
  return a.s3 === b.s3 && a.admin === b.admin && (a.ui ?? '') === (b.ui ?? '') && all(a) === all(b)
}

/** True when two verified observations carry the same identity document. */
function sameIdentity(a: Peer, b: Peer): boolean {
  return a.name === b.name && a.version === b.version && a.region === b.region &&
    sameEndpoints(a.endpoints, b.endpoints) && (a.features ?? []).join('\n') === (b.features ?? []).join('\n')
}

function conflictText(addr: string): string {
  return cleanText(`ID claimed by another address: ${addr} served this server ID with a different identity document; keeping the first verified address`, MAX_ERROR_LEN)
}

/**
 * Merges two observations of the same server ID; a is the earlier one.
 *
 *   - verified + unverified: the verified observation's identity, endpoints
 *     and addresses win (the sighting's addresses are unauthenticated); only
 *     its source is added.
 *   - verified + verified: when b was seen at an address a was seen at, or
 *     each document advertises an address the other was seen at, they are
 *     the same server and addresses and sources are merged (sameServer).
 *     Otherwise b is another host claiming a's ID: a is
 *     kept unchanged and, unless b served an identical document, the claim
 *     is recorded in `error` (as the gos3 server does; a's addresses are
 *     never extended with the claimant's).
 *   - unverified + unverified: union of addresses, most recent hint wins.
 */
export function mergePeer(a: Peer, b: Peer): Peer {
  const firstSeen = a.firstSeen <= b.firstSeen ? a.firstSeen : b.firstSeen
  const lastSeen = a.lastSeen >= b.lastSeen ? a.lastSeen : b.lastSeen
  if (a.verified !== b.verified) {
    const v = a.verified ? a : b
    const u = a.verified ? b : a
    return { ...v, sources: union(v.sources, u.sources, MAX_SOURCES), firstSeen, lastSeen }
  }
  if (a.verified) {
    if (!sameServer(a, b)) {
      if (sameIdentity(a, b) || a.error) return a
      return { ...a, error: conflictText(b.addresses[0] ?? '') }
    }
    return {
      ...a,
      features: union(a.features, b.features, MAX_FEATURES),
      sources: union(a.sources, b.sources, MAX_SOURCES),
      addresses: union(a.addresses, b.addresses, MAX_ADDRESSES),
      firstSeen,
      lastSeen
    }
  }
  const primary = b.lastSeen >= a.lastSeen ? b : a
  return {
    ...primary,
    sources: union(a.sources, b.sources, MAX_SOURCES),
    addresses: union(a.addresses, b.addresses, MAX_ADDRESSES),
    firstSeen,
    lastSeen,
    healthy: false,
    verified: false,
    error: primary.error ?? a.error ?? b.error
  }
}

/** Merges peers by ID, keeping first-seen order (see mergePeer). */
export function mergePeers(list: readonly Peer[]): Peer[] {
  const byId = new Map<string, Peer>()
  for (const p of list) {
    const prev = byId.get(p.id)
    byId.set(p.id, prev ? mergePeer(prev, p) : p)
  }
  return [...byId.values()]
}
