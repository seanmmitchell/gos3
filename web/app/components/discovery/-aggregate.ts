// Aggregation of discovery results from several origins into one list of
// servers, deduplicated by gos3 server ID (docs/discovery.md):
//   - peer tables of connected gos3 servers (GET /discovery on each)
//   - local discovery of the standalone UI host (/api/discovery/local, scan)
//   - direct browser probes (GET <url>/_gos3/info, CORS "*")
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
}

export interface PeerBatch {
  finder: Omit<Finder, 'sources'>
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

/** Merges batches of peers into servers keyed by ID (verified data wins). */
export function aggregatePeers(batches: readonly PeerBatch[]): DiscoveredServer[] {
  const byId = new Map<string, DiscoveredServer>()
  for (const batch of batches) {
    for (const p of batch.peers ?? []) {
      if (!p?.id) continue
      const finder: Finder = { ...batch.finder, sources: [...(p.sources ?? [])] }
      const prev = byId.get(p.id)
      if (!prev) {
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
      const takeIdentity = (p.verified && !prev.verified) || (p.verified === prev.verified && (p.lastSeen ?? '') > (prev.lastSeen ?? ''))
      if (takeIdentity) {
        prev.name = p.name || prev.name
        prev.version = p.version || prev.version
        prev.region = p.region || prev.region
        if (p.endpoints?.s3) prev.endpoints = p.endpoints
      }
      prev.features = union(prev.features, p.features)
      prev.addresses = union(prev.addresses, p.addresses)
      prev.firstSeen = minTs(prev.firstSeen, p.firstSeen || undefined)
      prev.lastSeen = maxTs(prev.lastSeen, p.lastSeen || undefined)
      prev.healthy = prev.healthy || !!p.healthy
      prev.verified = prev.verified || !!p.verified
      prev.error = prev.healthy ? undefined : (prev.error ?? p.error)
      const existing = prev.foundBy.find(f => finderKey(f) === finderKey(finder))
      if (existing) existing.sources = union(existing.sources, finder.sources)
      else prev.foundBy.push(finder)
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
  return union([s.endpoints?.s3, ...(s.endpoints?.all ?? [])].filter((u): u is string => !!u), s.addresses)
}

/** The configured connection for a discovered server (by server ID, else by URL). */
export function findConfigured(s: Pick<DiscoveredServer, 'id' | 'endpoints' | 'addresses'>, connections: readonly Connection[]): Connection | undefined {
  const byId = connections.find(c => c.serverId && c.serverId === s.id)
  if (byId) return byId
  const urls = serverUrls(s)
  return connections.find(c => urls.some(u => sameEndpoint(c.endpoint, u)))
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
