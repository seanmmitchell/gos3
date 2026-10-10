// Local network discovery from the standalone UI's Node server:
//   1. mDNS browse for `_gos3._tcp.local` (PTR → SRV/TXT/A records)
//   2. UDP beacon query to 239.255.73.83:7373 from an ephemeral socket
//      (unicast announce replies) + listening on the group for announces
//   3. verification of every candidate URL via GET /_gos3/info
// Sockets are always closed when the listen window ends, on errors included.
import { createSocket, type RemoteInfo, type Socket } from 'node:dgram'
import { networkInterfaces } from 'node:os'
import type { DiscoverySource, Peer } from '~/types/admin'
import { BEACON_GROUP, BEACON_PORT, beaconCandidateUrls, decodeBeacon, encodeQuery, newNonce } from './beacon'
import { MDNS_SERVICE, missingMdnsNames, parseMdnsRecords, type MdnsRecord } from './mdns'
import { mergePeers, peerFromInfo, peerFromSighting, type Sighting } from './peers'
import { fetchInfo, runPool, VERIFY_TIMEOUT_MS } from './probe'

export const DEFAULT_WAIT_MS = 3000
export const MIN_WAIT_MS = 500
export const MAX_WAIT_MS = 10000
/**
 * Bounds on what hosts on the LAN can make one discovery collect and probe
 * (multicast answers are unauthenticated and may be spoofed or flooded).
 */
export const MAX_MDNS_RECORDS = 2048
export const MAX_BEACON_CANDIDATES = 1024
export const MAX_VERIFY_URLS = 256

/** A candidate base URL with what we know about it before verification. */
export interface Candidate {
  url: string
  source: DiscoverySource
  /** Server ID / name announced by the mechanism (unverified). */
  id?: string
  name?: string
  version?: string
  region?: string
}

export interface MechanismResult {
  candidates: Candidate[]
  errors: string[]
}

/** Local discovery can be turned off with GOS3_UI_LOCAL_DISCOVERY=false. */
export function localDiscoveryEnabled(env: Record<string, string | undefined> = process.env): boolean {
  const v = (env.GOS3_UI_LOCAL_DISCOVERY ?? '').trim().toLowerCase()
  return !['0', 'false', 'off', 'no', 'disabled'].includes(v)
}

type MdnsFactory = typeof import('multicast-dns')

let mdnsModule: Promise<MdnsFactory | null> | undefined

/** Loads multicast-dns lazily; null when unavailable. */
export function loadMdns(): Promise<MdnsFactory | null> {
  mdnsModule ??= import('multicast-dns')
    .then(m => ((m as unknown as { default?: MdnsFactory }).default ?? (m as unknown as MdnsFactory)))
    .catch(() => null)
  return mdnsModule
}

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/** Non-internal IPv4 addresses of this host (multicast send/join interfaces). */
function ipv4Interfaces(): string[] {
  const out: string[] = []
  for (const list of Object.values(networkInterfaces())) {
    for (const a of list ?? []) {
      if (a.family === 'IPv4' && !a.internal) out.push(a.address)
    }
  }
  return out
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal?.aborted) return resolve()
    const t = setTimeout(done, ms)
    function done() {
      clearTimeout(t)
      signal?.removeEventListener('abort', done)
      resolve()
    }
    signal?.addEventListener('abort', done, { once: true })
  })
}

/** mDNS browse for waitMs. Never throws. */
export async function browseMdns(waitMs: number, signal?: AbortSignal): Promise<MechanismResult> {
  const errors: string[] = []
  const make = await loadMdns()
  if (!make) return { candidates: [], errors: ['mDNS: the multicast-dns module is not available'] }
  let m: ReturnType<MdnsFactory> | undefined
  const records: MdnsRecord[] = []
  const asked = new Set<string>()
  try {
    m = make({ multicast: true, reuseAddr: true, loopback: true })
    m.on('error', (e: unknown) => {
      errors.push(`mDNS: ${errText(e)}`)
    })
    m.on('warning', () => {
      // Non-fatal (e.g. one interface could not join the group).
    })
    m.on('response', (packet) => {
      const list = [...(packet.answers ?? []), ...(packet.additionals ?? [])] as unknown as MdnsRecord[]
      if (!list.length || records.length >= MAX_MDNS_RECORDS) return
      records.push(...list.slice(0, MAX_MDNS_RECORDS - records.length))
      // Ask for what is still missing (SRV/TXT of instances, A of targets).
      const missing = missingMdnsNames(records)
      const questions: { name: string, type: 'SRV' | 'TXT' | 'A' }[] = []
      for (const n of missing.srv) {
        if (!asked.has(`srv:${n}`)) {
          asked.add(`srv:${n}`)
          questions.push({ name: n, type: 'SRV' }, { name: n, type: 'TXT' })
        }
      }
      for (const n of missing.a) {
        if (!asked.has(`a:${n}`)) {
          asked.add(`a:${n}`)
          questions.push({ name: n, type: 'A' })
        }
      }
      if (questions.length && m) {
        try {
          m.query({ questions })
        } catch {
          // ignore
        }
      }
    })
    const query = () => {
      try {
        m?.query({ questions: [{ name: MDNS_SERVICE, type: 'PTR' }] })
      } catch (e) {
        errors.push(`mDNS: ${errText(e)}`)
      }
    }
    query()
    // A second query catches responders that missed the first one.
    await sleep(Math.min(1000, waitMs / 3), signal)
    query()
    await sleep(waitMs - Math.min(1000, waitMs / 3), signal)
  } catch (e) {
    errors.push(`mDNS: ${errText(e)}`)
  } finally {
    try {
      m?.destroy()
    } catch {
      // ignore
    }
  }
  const candidates: Candidate[] = []
  for (const c of parseMdnsRecords(records)) {
    for (const url of c.urls) {
      candidates.push({ url, source: 'mdns', id: c.txt.id, name: c.txt.name, version: c.txt.v, region: c.txt.region })
    }
  }
  return { candidates, errors: [...new Set(errors)] }
}

function bindSocket(sock: Socket, port: number): Promise<void> {
  return new Promise((resolve, reject) => {
    const onError = (e: Error) => reject(e)
    sock.once('error', onError)
    sock.bind(port, () => {
      sock.off('error', onError)
      resolve()
    })
  })
}

function closeSocket(sock: Socket | undefined): void {
  if (!sock) return
  try {
    sock.close()
  } catch {
    // already closed
  }
}

/**
 * Beacon query: sends a query from an ephemeral socket on every IPv4
 * interface and collects unicast announce replies plus announces multicast
 * to the group during waitMs. Never throws.
 */
export async function queryBeacon(waitMs: number, signal?: AbortSignal, group = BEACON_GROUP, port = BEACON_PORT): Promise<MechanismResult> {
  const errors: string[] = []
  const candidates: Candidate[] = []
  const seen = new Set<string>()
  const onMessage = (msg: Buffer, rinfo: RemoteInfo) => {
    if (candidates.length >= MAX_BEACON_CANDIDATES) return
    const m = decodeBeacon(msg)
    if (!m || m.type !== 'announce') return
    for (const url of beaconCandidateUrls(m.urls, rinfo.address)) {
      // Servers announce periodically: keep each (server, URL) once.
      const k = `${m.id ?? ''}\n${url}`
      if (seen.has(k) || candidates.length >= MAX_BEACON_CANDIDATES) continue
      seen.add(k)
      candidates.push({ url, source: 'beacon', id: m.id, name: m.name, version: m.version, region: m.region })
    }
  }

  let uconn: Socket | undefined
  let mconn: Socket | undefined
  try {
    uconn = createSocket({ type: 'udp4' })
    uconn.on('error', e => errors.push(`beacon: ${errText(e)}`))
    uconn.on('message', onMessage)
    await bindSocket(uconn, 0)
    try {
      uconn.setMulticastTTL(1)
      uconn.setMulticastLoopback(true)
    } catch (e) {
      errors.push(`beacon: ${errText(e)}`)
    }

    // Also listen on the group port for periodic announces (best effort:
    // another process, e.g. a local gos3, may hold the port without reuse).
    try {
      mconn = createSocket({ type: 'udp4', reuseAddr: true })
      mconn.on('error', () => {})
      mconn.on('message', onMessage)
      await bindSocket(mconn, port)
      const ifaces = ipv4Interfaces()
      let joined = false
      for (const iface of ifaces) {
        try {
          mconn.addMembership(group, iface)
          joined = true
        } catch {
          // interface without multicast
        }
      }
      if (!joined) mconn.addMembership(group)
    } catch (e) {
      errors.push(`beacon: cannot listen on ${group}:${port} (${errText(e)}); relying on query replies`)
      closeSocket(mconn)
      mconn = undefined
    }

    const query = encodeQuery(newNonce())
    const send = () => {
      const ifaces = ipv4Interfaces()
      const sendOne = () => new Promise<void>((resolve) => {
        uconn!.send(query, port, group, (e) => {
          if (e) errors.push(`beacon: send failed (${errText(e)})`)
          resolve()
        })
      })
      const tasks: Promise<void>[] = []
      if (ifaces.length === 0) {
        tasks.push(sendOne())
      } else {
        for (const iface of ifaces) {
          try {
            uconn!.setMulticastInterface(iface)
            tasks.push(sendOne())
          } catch {
            // skip interface
          }
        }
      }
      return Promise.all(tasks)
    }
    await send()
    await sleep(Math.min(1000, waitMs / 3), signal)
    await send() // repeat once: UDP is lossy
    await sleep(waitMs - Math.min(1000, waitMs / 3), signal)
  } catch (e) {
    errors.push(`beacon: ${errText(e)}`)
  } finally {
    closeSocket(uconn)
    closeSocket(mconn)
  }
  return { candidates, errors: [...new Set(errors)] }
}

/**
 * Verifies candidates (each URL once, at most MAX_VERIFY_URLS URLs, bounded
 * concurrency). Verified identities become peers; candidates that announced
 * an ID but could not be verified are reported as unverified peers.
 */
export async function verifyCandidates(candidates: readonly Candidate[], opts: { timeoutMs?: number, signal?: AbortSignal, concurrency?: number, maxUrls?: number } = {}): Promise<Peer[]> {
  const byUrl = new Map<string, Candidate[]>()
  for (const c of candidates) {
    const list = byUrl.get(c.url)
    if (list) list.push(c)
    else if (byUrl.size < (opts.maxUrls ?? MAX_VERIFY_URLS)) byUrl.set(c.url, [c])
  }
  const urls = [...byUrl.keys()]
  const now = new Date()
  const results = await runPool(urls, opts.concurrency ?? 16, async (url) => {
    try {
      const info = await fetchInfo(url, { timeoutMs: opts.timeoutMs ?? VERIFY_TIMEOUT_MS, signal: opts.signal })
      return { url, info }
    } catch (e) {
      return { url, error: errText(e) }
    }
  }, opts.signal)

  const peers: Peer[] = []
  for (const r of results) {
    if (!r) continue
    const seen = byUrl.get(r.url) ?? []
    const sources = [...new Set(seen.map(c => c.source))]
    if ('info' in r && r.info) {
      for (const source of sources) peers.push(peerFromInfo(r.info, r.url, source, now))
    } else {
      for (const c of seen) {
        if (!c.id) continue
        const s: Sighting = { id: c.id, name: c.name, version: c.version, region: c.region, urls: [c.url], source: c.source, error: `${r.url}: ${'error' in r ? r.error : 'unreachable'}` }
        peers.push(peerFromSighting(s, now))
      }
    }
  }
  return mergePeers(peers)
}

export interface LocalDiscoveryResult {
  peers: Peer[]
  errors: string[]
  /** Candidate URLs seen before verification. */
  candidates: number
  elapsedMs: number
}

/** Runs mDNS + beacon concurrently for waitMs, then verifies. Never throws. */
export async function discoverLocal(waitMs: number, opts: { mdns?: boolean, beacon?: boolean, signal?: AbortSignal } = {}): Promise<LocalDiscoveryResult> {
  const t0 = Date.now()
  const [m, b] = await Promise.all([
    opts.mdns === false ? Promise.resolve<MechanismResult>({ candidates: [], errors: [] }) : browseMdns(waitMs, opts.signal),
    opts.beacon === false ? Promise.resolve<MechanismResult>({ candidates: [], errors: [] }) : queryBeacon(waitMs, opts.signal)
  ])
  const candidates = [...m.candidates, ...b.candidates]
  const peers = opts.signal?.aborted ? [] : await verifyCandidates(candidates, { signal: opts.signal })
  return { peers, errors: [...m.errors, ...b.errors], candidates: new Set(candidates.map(c => c.url)).size, elapsedMs: Date.now() - t0 }
}
