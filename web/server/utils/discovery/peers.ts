// Peer bookkeeping for local discovery: verified identity documents and
// unverified sightings are merged by server ID (sources = union, addresses =
// union), like the gos3 peer table (docs/discovery.md "Peer lifecycle").
import type { DiscoverySource, Info, Peer } from '~/types/admin'

/** Validates a /_gos3/info document. */
export function isInfo(v: unknown): v is Info {
  if (typeof v !== 'object' || v === null) return false
  const o = v as Record<string, unknown>
  return o.service === 'gos3' && typeof o.id === 'string' && o.id !== '' && typeof o.name === 'string'
}

function union<T>(a: readonly T[] | null | undefined, b: readonly T[] | null | undefined): T[] {
  const out = [...(a ?? [])]
  for (const v of b ?? []) if (!out.includes(v)) out.push(v)
  return out
}

/** A verified peer built from an identity document fetched at baseUrl. */
export function peerFromInfo(info: Info, baseUrl: string, source: DiscoverySource, now: Date = new Date()): Peer {
  const ts = now.toISOString()
  return {
    id: info.id,
    name: info.name,
    version: info.version ?? '',
    region: info.region ?? '',
    endpoints: info.endpoints ?? { s3: baseUrl, admin: `${baseUrl}/_gos3/admin/v1` },
    features: info.features ?? [],
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
  return {
    id: s.id,
    name: s.name ?? s.id,
    version: s.version ?? '',
    region: s.region ?? '',
    endpoints: { s3: s.urls[0] ?? '', admin: s.urls[0] ? `${s.urls[0]}/_gos3/admin/v1` : '' },
    sources: [s.source],
    addresses: [...s.urls],
    firstSeen: ts,
    lastSeen: ts,
    healthy: false,
    verified: false,
    error: s.error
  }
}

/**
 * Merges two observations of the same server. Identity fields come from the
 * verified (else most recent) observation; health/verification are OR-ed.
 */
export function mergePeer(a: Peer, b: Peer): Peer {
  const primary = b.verified && !a.verified ? b : a.verified && !b.verified ? a : (b.lastSeen >= a.lastSeen ? b : a)
  const healthy = a.healthy || b.healthy
  return {
    ...primary,
    endpoints: primary.endpoints,
    features: union(a.features, b.features),
    sources: union(a.sources, b.sources),
    addresses: union(a.addresses, b.addresses),
    firstSeen: a.firstSeen <= b.firstSeen ? a.firstSeen : b.firstSeen,
    lastSeen: a.lastSeen >= b.lastSeen ? a.lastSeen : b.lastSeen,
    healthy,
    verified: a.verified || b.verified,
    error: healthy ? undefined : (primary.error ?? a.error ?? b.error)
  }
}

/** Merges peers by ID, keeping first-seen order. */
export function mergePeers(list: readonly Peer[]): Peer[] {
  const byId = new Map<string, Peer>()
  for (const p of list) {
    const prev = byId.get(p.id)
    byId.set(p.id, prev ? mergePeer(prev, p) : p)
  }
  return [...byId.values()]
}
