// Local network discovery from the standalone UI's Node server:
//   1. mDNS browse for `_gos3._tcp.local` (PTR → SRV/TXT/A records)
//   2. UDP beacon query to 239.255.73.83:7373 from an ephemeral socket
//      (unicast announce replies) + listening on the group for announces
//   3. verification of every candidate URL via GET /_gos3/info
// Sockets are always closed when the listen window ends, on errors included.
import { createSocket, type RemoteInfo, type Socket } from 'node:dgram'
import { networkInterfaces } from 'node:os'
import type { DiscoverySource, Peer } from '~/types/admin'
import { BEACON_GROUP, BEACON_PORT, beaconCandidateUrls, decodeBeacon, encodeQuery, MAX_BEACON_URLS, newNonce } from './beacon'
import { MDNS_SERVICE, missingMdnsNames, parseMdnsRecords, type MdnsRecord } from './mdns'
import { blockedUrlHost } from './netguard'
import { hostNets, type LocalNets } from './netlocal'
import { mergePeers, peerFromInfo, peerFromSighting, type Sighting } from './peers'
import { fetchInfo, ProbeError, runPool, VERIFY_TIMEOUT_MS, type FetchFn } from './probe'

export const DEFAULT_WAIT_MS = 3000
export const MIN_WAIT_MS = 500
export const MAX_WAIT_MS = 10000
/**
 * Bounds on what hosts on the LAN can make one discovery collect and probe
 * (multicast answers are unauthenticated and may be spoofed or flooded).
 */
export const MAX_MDNS_RECORDS = 2048
/** Records looked at per mDNS response (as the gos3 server). */
export const MAX_MDNS_RECORDS_PER_RESPONSE = 64
/** Follow-up mDNS questions per browse. */
export const MAX_MDNS_FOLLOW_UPS = 32
export const MAX_BEACON_CANDIDATES = 1024
/** Beacon candidates taken from one sender per discovery. */
export const MAX_BEACON_CANDIDATES_PER_SOURCE = MAX_BEACON_URLS
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

/**
 * Local discovery (the /api/discovery/* probe, scan and local routes) is
 * off unless GOS3_UI_LOCAL_DISCOVERY is explicitly true (`true`, `1`, `on`,
 * `yes` or `enabled`, case-insensitive). The Node server has no
 * authentication, so anyone who can reach it could otherwise make the UI
 * host probe and scan its networks.
 */
export function localDiscoveryEnabled(env: Record<string, string | undefined> = process.env): boolean {
  const v = (env.GOS3_UI_LOCAL_DISCOVERY ?? '').trim().toLowerCase()
  return ['1', 'true', 'on', 'yes', 'enabled'].includes(v)
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

/**
 * Reports a socket error: the response carries only the fixed text `what`
 * (raw OS errors name bind addresses and interfaces); the details go to
 * the server log.
 */
function socketError(errors: string[], what: string, e: unknown): void {
  errors.push(what)
  console.warn(`[gos3-ui] local discovery: ${what}: ${errText(e)}`)
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

/**
 * mDNS browse for waitMs. Never throws. Responses are accepted only from
 * senders on this host or a directly connected subnet, and every sender's
 * records are kept apart (see parseMdnsRecords): a host can only make the
 * UI host probe its own address.
 */
export async function browseMdns(waitMs: number, signal?: AbortSignal, nets: LocalNets = hostNets): Promise<MechanismResult> {
  const errors: string[] = []
  const make = await loadMdns()
  if (!make) return { candidates: [], errors: ['mDNS: the multicast-dns module is not available'] }
  let m: ReturnType<MdnsFactory> | undefined
  const bySender = new Map<string, MdnsRecord[]>()
  let total = 0
  const asked = new Set<string>()
  try {
    m = make({ multicast: true, reuseAddr: true, loopback: true })
    m.on('error', (e: unknown) => socketError(errors, 'mDNS: socket error', e))
    m.on('warning', () => {
      // Non-fatal (e.g. one interface could not join the group).
    })
    m.on('response', (packet, rinfo: RemoteInfo) => {
      const src = rinfo?.address
      if (!src || !nets.onLink(src)) return
      const list = [...(packet.answers ?? []), ...(packet.additionals ?? [])].slice(0, MAX_MDNS_RECORDS_PER_RESPONSE) as unknown as MdnsRecord[]
      if (!list.length || total >= MAX_MDNS_RECORDS) return
      const records = bySender.get(src) ?? []
      const take = list.slice(0, MAX_MDNS_RECORDS - total)
      records.push(...take)
      total += take.length
      bySender.set(src, records)
      // Ask for what this sender has not sent yet (SRV/TXT of instances, A of targets).
      const missing = missingMdnsNames(records)
      const questions: { name: string, type: 'SRV' | 'TXT' | 'A' }[] = []
      for (const n of missing.srv) {
        if (!asked.has(`srv:${n}`) && asked.size < MAX_MDNS_FOLLOW_UPS) {
          asked.add(`srv:${n}`)
          questions.push({ name: n, type: 'SRV' }, { name: n, type: 'TXT' })
        }
      }
      for (const n of missing.a) {
        if (!asked.has(`a:${n}`) && asked.size < MAX_MDNS_FOLLOW_UPS) {
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
        socketError(errors, 'mDNS: the query could not be sent', e)
      }
    }
    query()
    // A second query catches responders that missed the first one.
    await sleep(Math.min(1000, waitMs / 3), signal)
    query()
    await sleep(waitMs - Math.min(1000, waitMs / 3), signal)
  } catch (e) {
    socketError(errors, 'mDNS: socket error', e)
  } finally {
    try {
      m?.destroy()
    } catch {
      // ignore
    }
  }
  const candidates: Candidate[] = []
  for (const [src, records] of bySender) {
    for (const c of parseMdnsRecords(records, src, nets)) {
      for (const url of c.urls) {
        candidates.push({ url, source: 'mdns', id: c.txt.id || undefined, name: c.txt.name, version: c.txt.v, region: c.txt.region })
      }
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
 * to the group during waitMs. Never throws. Datagrams are accepted only
 * from senders on this host or a directly connected subnet, their URLs only
 * ever point at the sender (beaconCandidateUrls), and one sender yields at
 * most MAX_BEACON_CANDIDATES_PER_SOURCE candidates.
 */
export async function queryBeacon(waitMs: number, signal?: AbortSignal, group = BEACON_GROUP, port = BEACON_PORT, nets: LocalNets = hostNets): Promise<MechanismResult> {
  const errors: string[] = []
  const candidates: Candidate[] = []
  const seen = new Set<string>()
  const perSender = new Map<string, number>()
  const onMessage = (msg: Buffer, rinfo: RemoteInfo) => {
    if (candidates.length >= MAX_BEACON_CANDIDATES || !nets.onLink(rinfo.address)) return
    const m = decodeBeacon(msg)
    if (!m || m.type !== 'announce') return
    for (const url of beaconCandidateUrls(m.urls, rinfo.address, ip => nets.isLocal(ip))) {
      // Servers announce periodically: keep each (server, URL) once.
      const k = `${m.id}\n${url}`
      const n = perSender.get(rinfo.address) ?? 0
      if (seen.has(k) || candidates.length >= MAX_BEACON_CANDIDATES || n >= MAX_BEACON_CANDIDATES_PER_SOURCE) continue
      seen.add(k)
      perSender.set(rinfo.address, n + 1)
      candidates.push({ url, source: 'beacon', id: m.id, name: m.name, version: m.version, region: m.region })
    }
  }

  let uconn: Socket | undefined
  let mconn: Socket | undefined
  try {
    uconn = createSocket({ type: 'udp4' })
    uconn.on('error', e => socketError(errors, 'beacon: socket error', e))
    uconn.on('message', onMessage)
    await bindSocket(uconn, 0)
    try {
      uconn.setMulticastTTL(1)
      uconn.setMulticastLoopback(true)
    } catch (e) {
      socketError(errors, 'beacon: cannot set multicast options', e)
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
      socketError(errors, 'beacon: cannot listen on the group port; relying on query replies', e)
      closeSocket(mconn)
      mconn = undefined
    }

    const query = encodeQuery(newNonce())
    const send = () => {
      const ifaces = ipv4Interfaces()
      const sendOne = () => new Promise<void>((resolve) => {
        uconn!.send(query, port, group, (e) => {
          if (e) socketError(errors, 'beacon: the query could not be sent', e)
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
    socketError(errors, 'beacon: socket error', e)
  } finally {
    closeSocket(uconn)
    closeSocket(mconn)
  }
  return { candidates, errors: [...new Set(errors)] }
}

/**
 * Verifies candidates (each URL once, at most MAX_VERIFY_URLS URLs, bounded
 * concurrency; blocked IP literals are never probed). Verified identities
 * become peers; candidates that announced an ID but could not be verified
 * are reported as unverified peers. Observations of one ID are merged with
 * mergePeers, so a host claiming the ID of a server verified earlier in the
 * list (mDNS before beacon, in arrival order) cannot take over its entry.
 */
export async function verifyCandidates(candidates: readonly Candidate[], opts: { timeoutMs?: number, signal?: AbortSignal, concurrency?: number, maxUrls?: number, fetch?: FetchFn } = {}): Promise<Peer[]> {
  const byUrl = new Map<string, Candidate[]>()
  for (const c of candidates) {
    if (blockedUrlHost(c.url)) continue
    const list = byUrl.get(c.url)
    if (list) list.push(c)
    else if (byUrl.size < (opts.maxUrls ?? MAX_VERIFY_URLS)) byUrl.set(c.url, [c])
  }
  const urls = [...byUrl.keys()]
  const now = new Date()
  const results = await runPool(urls, opts.concurrency ?? 16, async (url) => {
    try {
      const info = await fetchInfo(url, { timeoutMs: opts.timeoutMs ?? VERIFY_TIMEOUT_MS, signal: opts.signal, fetch: opts.fetch })
      return { url, info }
    } catch (e) {
      return { url, error: e instanceof ProbeError ? e.message : 'unreachable' }
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
