// Aggregation of discovery results from several origins into one list of
// servers, deduplicated by gos3 server ID (docs/discovery.md):
//   - peer tables of connected gos3 servers (GET /discovery on each)
//   - local discovery of the standalone UI host (/api/discovery/local, scan)
//   - direct browser probes (GET <url>/_gos3/info, CORS "*")
// Server IDs are not authenticated (docs/discovery.md "Peer identity"): a
// record only merges into another with the same ID when both describe the
// same host (sameServer); otherwise the server is flagged as `conflict` and
// keeps the identity of the record seen first.
// Plain TS (explicit imports) so it is unit-testable.
import type { DiscoverySource, Info, Peer } from '~/types/admin'
import type { Connection } from '~/types/connection'
import { sameEndpoint } from '~/utils/connections'
import { normalizeEndpoint } from '~/utils/s3/url'

export type FinderKind = 'server' | 'local' | 'browser'

/** Who found a server, and through which mechanisms. */
export interface Finder {
  kind: FinderKind
  /** Display label, e.g. "nas" or "UI host" or "Browser". */
  label: string
  /** Connection whose peer table reported it (kind 'server'). */
  connectionId?: string
  sources: DiscoverySource[]
  /** Addresses this finder observed the server at. */
  addresses?: string[]
}

export interface DiscoveredServer {
  id: string
  name: string
  version: string
  region: string
  endpoints: Peer['endpoints']
  features: string[]
  addresses: string[]
  firstSeen?: string
  lastSeen?: string
  healthy: boolean
  verified: boolean
  error?: string
  foundBy: Finder[]
  /**
   * Another record claimed this server ID from addresses that do not match
   * (one of them is spoofing it, or the server moved). The row keeps the
   * first verified record's identity; the other addresses are in
   * `conflictAddresses`.
   */
  conflict?: boolean
  conflictAddresses?: string[]
}

export interface PeerBatch {
  finder: Omit<Finder, 'sources' | 'addresses'>
  peers: readonly Peer[] | null | undefined
}

function finderKey(f: Pick<Finder, 'kind' | 'connectionId'>): string {
  return `${f.kind}:${f.connectionId ?? ''}`
}

function union<T>(a: readonly T[] | null | undefined, b: readonly T[] | null | undefined): T[] {
  const out = [...(a ?? [])]
  for (const v of b ?? []) if (!out.includes(v)) out.push(v)
  return out
}

function maxTs(a: string | undefined, b: string | undefined): string | undefined {
  if (!a) return b
  if (!b) return a
  return a >= b ? a : b
}

function minTs(a: string | undefined, b: string | undefined): string | undefined {
  if (!a) return b
  if (!b) return a
  return a <= b ? a : b
}

function isHttps(u: string | undefined): boolean {
  return !!u && /^https:/i.test(u)
}

/** True when the two address lists have an endpoint in common. */
function sharesAddress(a: readonly string[], b: readonly string[]): boolean {
  return a.some(x => b.some(y => sameEndpoint(x, y)))
}

function advertised(e: Peer['endpoints'] | undefined): string[] {
  return [typeof e?.s3 === 'string' ? e.s3 : '', ...stringList(e?.all)].filter(u => !!u)
}

/** Strings of a value that should be a string array (server documents may be malformed). */
export function stringList(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string' && x !== '') : []
}

const str = (v: unknown): string => (typeof v === 'string' ? v : '')

/** A peer with every field of the expected type (lists are arrays of strings, endpoints strings). */
function sanitizePeer(p: Peer): Peer {
  const e = (typeof p.endpoints === 'object' && p.endpoints !== null ? p.endpoints : {}) as Partial<Peer['endpoints']>
  const all = stringList(e.all)
  return {
    ...p,
    name: str(p.name),
    version: str(p.version),
    region: str(p.region),
    endpoints: { s3: str(e.s3), admin: str(e.admin), ...(e.ui !== undefined ? { ui: str(e.ui) } : {}), ...(all.length ? { all } : {}) },
    features: stringList(p.features),
    sources: stringList(p.sources) as Peer['sources'],
    addresses: stringList(p.addresses),
    firstSeen: str(p.firstSeen),
    lastSeen: str(p.lastSeen),
    error: typeof p.error === 'string' ? p.error : undefined
  }
}

/**
 * True when record `p` plausibly describes the same server as `prev`: it was
 * observed at an address `prev` was observed at, or (multi-homed hosts) each
 * advertises the address the other was observed at. Advertised endpoints
 * are self-declared, so one side's claim alone is not enough: a rogue
 * listing the real server's address in its own endpoints does not match,
 * because the real server does not advertise the rogue's address. Records
 * without addresses match.
 */
function sameServer(prev: Pick<DiscoveredServer, 'addresses' | 'endpoints'>, p: Pick<Peer, 'addresses' | 'endpoints'>): boolean {
  const addrs = p.addresses ?? []
  if (!addrs.length || !prev.addresses.length) return true
  if (sharesAddress(addrs, prev.addresses)) return true
  return sharesAddress(addrs, advertised(prev.endpoints)) && sharesAddress(prev.addresses, advertised(p.endpoints))
}

function addFinder(s: DiscoveredServer, finder: Finder): void {
  const existing = s.foundBy.find(f => finderKey(f) === finderKey(finder))
  if (existing) {
    existing.sources = union(existing.sources, finder.sources)
    existing.addresses = union(existing.addresses, finder.addresses)
  } else s.foundBy.push(finder)
}

/**
 * Merges batches of peers into servers keyed by ID. Records for the same ID
 * merge only when they plausibly describe the same host (sameServer); then
 * verified data wins, else the more recent one, and an https endpoint is
 * never replaced by an http one. Any other record never takes over the
 * row's identity, endpoints or addresses: the row is marked `conflict`
 * instead (a verified record does replace an unverified one, which then
 * counts as the conflicting one). Endpoints a connected gos3 server
 * reported (finder 'server') are never replaced by a record from local
 * discovery or a browser probe. Malformed fields are ignored.
 */
export function aggregatePeers(batches: readonly PeerBatch[]): DiscoveredServer[] {
  const byId = new Map<string, DiscoveredServer>()
  /** IDs whose identity (endpoints) came from a server finder's peer table. */
  const fromServer = new Set<string>()
  for (const batch of batches) {
    for (const raw of batch.peers ?? []) {
      if (!raw || typeof raw.id !== 'string' || !raw.id) continue
      const p = sanitizePeer(raw)
      const finder: Finder = { ...batch.finder, sources: [...p.sources], addresses: [...p.addresses] }
      // A local/browser record may not replace what a server reported.
      const mayReplace = batch.finder.kind === 'server' || !fromServer.has(p.id)
      const prev = byId.get(p.id)
      if (!prev) {
        if (batch.finder.kind === 'server') fromServer.add(p.id)
        byId.set(p.id, {
          id: p.id,
          name: p.name || p.id,
          version: p.version ?? '',
          region: p.region ?? '',
          endpoints: p.endpoints ?? { s3: p.addresses?.[0] ?? '', admin: '' },
          features: [...(p.features ?? [])],
          addresses: [...(p.addresses ?? [])],
          firstSeen: p.firstSeen || undefined,
          lastSeen: p.lastSeen || undefined,
          healthy: !!p.healthy,
          verified: !!p.verified,
          error: p.healthy ? undefined : p.error,
          foundBy: [finder]
        })
        continue
      }
      if (!sameServer(prev, p)) {
        prev.conflict = true
        if (p.verified && !prev.verified && mayReplace) {
          if (batch.finder.kind === 'server') fromServer.add(p.id)
          // The verified record wins; the unverified one becomes the conflict.
          prev.conflictAddresses = union(prev.conflictAddresses, prev.addresses)
          Object.assign(prev, {
            name: p.name || p.id,
            version: p.version ?? '',
            region: p.region ?? '',
            endpoints: p.endpoints ?? { s3: p.addresses?.[0] ?? '', admin: '' },
            features: [...(p.features ?? [])],
            addresses: [...(p.addresses ?? [])],
            firstSeen: p.firstSeen || undefined,
            lastSeen: p.lastSeen || undefined,
            healthy: !!p.healthy,
            verified: true,
            error: p.healthy ? undefined : p.error
          })
        } else {
          prev.conflictAddresses = union(prev.conflictAddresses, p.addresses)
        }
        addFinder(prev, finder)
        continue
      }
      const takeIdentity = (p.verified && !prev.verified) || (p.verified === prev.verified && (p.lastSeen ?? '') > (prev.lastSeen ?? ''))
      if (takeIdentity) {
        prev.name = p.name || prev.name
        prev.version = p.version || prev.version
        prev.region = p.region || prev.region
        const downgrade = isHttps(prev.endpoints?.s3) && !isHttps(p.endpoints?.s3)
        if (p.endpoints?.s3 && !downgrade && mayReplace) {
          prev.endpoints = p.endpoints
          if (batch.finder.kind === 'server') fromServer.add(p.id)
        }
      }
      prev.features = union(prev.features, p.features)
      prev.addresses = union(prev.addresses, p.addresses)
      prev.firstSeen = minTs(prev.firstSeen, p.firstSeen || undefined)
      prev.lastSeen = maxTs(prev.lastSeen, p.lastSeen || undefined)
      prev.healthy = prev.healthy || !!p.healthy
      prev.verified = prev.verified || !!p.verified
      prev.error = prev.healthy ? undefined : (prev.error ?? p.error)
      addFinder(prev, finder)
    }
  }
  return [...byId.values()].sort((a, b) => {
    if (a.verified !== b.verified) return a.verified ? -1 : 1
    return a.name.localeCompare(b.name) || a.id.localeCompare(b.id)
  })
}

/** A peer for a server the browser probed directly. */
export function peerFromInfo(info: Info, url: string, source: DiscoverySource = 'probe', now: Date = new Date()): Peer {
  let base = url
  try {
    base = normalizeEndpoint(url)
  } catch {
    // keep as is
  }
  const ts = now.toISOString()
  return {
    id: info.id,
    name: info.name,
    version: info.version,
    region: info.region,
    endpoints: info.endpoints,
    features: info.features ?? [],
    sources: [source],
    addresses: [base],
    firstSeen: ts,
    lastSeen: ts,
    healthy: true,
    verified: true
  }
}

/** Every base URL a discovered server is known under. */
export function serverUrls(s: Pick<DiscoveredServer, 'endpoints' | 'addresses'>): string[] {
  return union(advertised(s.endpoints), stringList(s.addresses))
}

/**
 * The configured connection for a discovered server: one whose endpoint is
 * an address the server was observed at (the one with the same server ID
 * first). Server IDs and advertised endpoints are self-declared, so neither
 * alone makes a server "configured"; see findIdClaim.
 */
export function findConfigured(s: Pick<DiscoveredServer, 'id' | 'addresses'>, connections: readonly Connection[]): Connection | undefined {
  const addrs = stringList(s.addresses)
  const atAddress = connections.filter(c => addrs.some(a => sameEndpoint(c.endpoint, a)))
  return atAddress.find(c => !!c.serverId && c.serverId === s.id) ?? atAddress[0]
}

/**
 * A configured connection with the same server ID as `s` although `s` was
 * not observed at its endpoint: the same server under another address, or
 * a host spoofing the ID. Shown as a warning; never treated as configured.
 */
export function findIdClaim(s: Pick<DiscoveredServer, 'id' | 'addresses'>, connections: readonly Connection[]): Connection | undefined {
  if (findConfigured(s, connections)) return undefined
  return connections.find(c => !!c.serverId && c.serverId === s.id)
}

/**
 * Connection whose credentials "Add as server" may offer for `s` at
 * `endpoint`: a server finder (whose connection `usable` accepts) whose own
 * peer record lists `endpoint`. None for a `conflict` row, where it is
 * unclear which record is the real server.
 */
export function credentialsSourceFor(
  s: Pick<DiscoveredServer, 'foundBy' | 'conflict'>,
  endpoint: string,
  usable: (connectionId: string) => boolean
): string | undefined {
  if (s.conflict) return undefined
  const f = s.foundBy.find(f => f.kind === 'server' && !!f.connectionId && (f.addresses ?? []).some(a => sameEndpoint(a, endpoint)) && usable(f.connectionId))
  return f?.connectionId
}

/** Splits user input (newlines, commas, whitespace) into URL/host entries. */
export function parseUrlList(text: string): string[] {
  const out: string[] = []
  for (const part of text.split(/[\s,;]+/)) {
    const s = part.trim()
    if (s && !out.includes(s)) out.push(s)
  }
  return out
}

/** Human label of a discovery source. */
export const SOURCE_LABELS: Record<DiscoverySource, string> = {
  mdns: 'mDNS',
  beacon: 'Beacon',
  static: 'Seed',
  dns: 'DNS SRV',
  scan: 'Scan',
  probe: 'Probe'
}

export function sourceLabel(s: string): string {
  return (SOURCE_LABELS as Record<string, string>)[s] ?? s
}

/**
 * Base URL to probe for a user entry: URLs are kept; bare hosts get http://
 * and gos3's default port 9000 unless a port is given.
 */
export function probeUrlFor(entry: string, defaultPort = 9000): string {
  const s = entry.trim().replace(/\/+$/, '')
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(s)) return s
  if (/^\[[^\]]+\]:\d+$/.test(s) || /^[^:]+:\d+$/.test(s)) return `http://${s}`
  if (s.includes(':') && !s.startsWith('[')) return `http://[${s}]:${defaultPort}` // bare IPv6
  return `http://${s}:${defaultPort}`
}
