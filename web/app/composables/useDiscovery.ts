import type { Peer, ScanRequest, ScanResult } from '~/types/admin'
import type { Connection } from '~/types/connection'
import { aggregatePeers, peerFromInfo, type FinderKind, type PeerBatch } from '~/components/discovery/-aggregate'
import { fetchInfo } from '~/utils/admin/client'
import { useConnectionsStore } from '~/stores/connections'

export type DiscoverySourceStateName = 'idle' | 'loading' | 'ok' | 'error' | 'skipped'

/** One origin of discovery results shown on the Discover page. */
export interface DiscoverySourceState {
  key: string
  kind: FinderKind
  label: string
  connectionId?: string
  state: DiscoverySourceStateName
  /** Short status line ("3 peers", "No permission", …). */
  message?: string
  /** Non-fatal problems (e.g. mDNS unavailable on the UI host). */
  warnings?: string[]
  error?: unknown
  peers: Peer[]
  updatedAt?: number
}

export interface ProbeFailure {
  url: string
  error: unknown
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

/**
 * Aggregated discovery for the global Discover page: peer tables of every
 * connected gos3 server, local discovery from the UI host (standalone mode)
 * and direct browser probes, deduplicated by server ID.
 *
 *   const d = useDiscovery()
 *   await d.refreshAll()
 *   d.servers.value   // DiscoveredServer[]
 */
export function useDiscovery() {
  const store = useConnectionsStore()
  const local = useDiscoveryLocal()
  const sources = ref<Record<string, DiscoverySourceState>>({})
  const probeFailures = ref<ProbeFailure[]>([])
  const probing = ref(false)

  function setSource(key: string, init: Omit<DiscoverySourceState, 'key' | 'peers' | 'state'>, patch: Partial<DiscoverySourceState>): void {
    const prev = sources.value[key]
    sources.value[key] = { key, peers: [], state: 'idle', ...prev, ...init, ...patch }
  }

  const sourceList = computed(() => Object.values(sources.value))
  const batches = computed<PeerBatch[]>(() => sourceList.value
    .filter(s => s.peers.length)
    .map(s => ({ finder: { kind: s.kind, label: s.label, connectionId: s.connectionId }, peers: s.peers })))
  const servers = computed(() => aggregatePeers(batches.value))
  const loading = computed(() => probing.value || sourceList.value.some(s => s.state === 'loading'))

  /** GET /discovery on one gos3 connection (skipped without credentials/permission). */
  async function refreshServer(c: Connection): Promise<void> {
    const key = `server:${c.id}`
    const init = { kind: 'server' as const, label: c.name, connectionId: c.id }
    if (!hasCredentials(c)) {
      setSource(key, init, { state: 'skipped', message: 'Sign in to include its peer table.', peers: [] })
      return
    }
    setSource(key, init, { state: 'loading', error: undefined })
    const status = await store.ensureStatus(c.id)
    if (!status.whoami) {
      const why = status.state === 'offline' ? 'Server offline.' : status.state === 'unauthorized' ? 'Credentials rejected.' : (status.error ?? 'Not available.')
      setSource(key, init, { state: status.state === 'offline' || status.state === 'unauthorized' ? 'error' : 'skipped', message: why, peers: [] })
      return
    }
    if (!status.whoami.capabilities.discovery) {
      setSource(key, init, { state: 'skipped', message: 'No permission to read discovery (admin:GetDiscovery).', peers: [] })
      return
    }
    try {
      const d = await getAdminClient(c).discovery()
      const peers = d.peers ?? []
      setSource(key, init, {
        state: 'ok',
        peers,
        message: d.enabled ? plural(peers.length, 'peer') : `Discovery is disabled on this server${peers.length ? ` (${plural(peers.length, 'known peer')})` : ''}.`,
        updatedAt: Date.now()
      })
    } catch (e) {
      setSource(key, init, { state: 'error', error: e, message: errorMessage(e), peers: [] })
    }
  }

  /** Peer tables of every gos3 connection. */
  async function refreshServers(): Promise<void> {
    const gos3 = store.connections.filter(c => c.kind === 'gos3')
    // Drop sources of removed connections / kinds that changed.
    for (const key of Object.keys(sources.value)) {
      if (key.startsWith('server:') && !gos3.some(c => `server:${c.id}` === key)) delete sources.value[key]
    }
    await Promise.all(gos3.map(c => refreshServer(c)))
  }

  /** mDNS + beacon from the UI host (no-op when unavailable). */
  async function refreshLocal(waitMs = 3000): Promise<void> {
    const init = { kind: 'local' as const, label: 'UI host (mDNS + beacon)' }
    await local.detect()
    if (!local.available.value) {
      if (sources.value.local) delete sources.value.local
      return
    }
    setSource('local', init, { state: 'loading', error: undefined })
    try {
      const r = await local.client.discover(waitMs)
      setSource('local', init, {
        state: 'ok',
        peers: r.peers,
        warnings: r.errors,
        message: `${plural(r.peers.length, 'server')} found${r.candidates !== undefined ? ` from ${plural(r.candidates, 'candidate')}` : ''}.`,
        updatedAt: Date.now()
      })
    } catch (e) {
      setSource('local', init, { state: 'error', error: e, message: errorMessage(e), peers: [] })
    }
  }

  /** Network scan from the UI host; results join the aggregate. */
  async function scanLocal(req: ScanRequest, signal?: AbortSignal): Promise<ScanResult> {
    const init = { kind: 'local' as const, label: 'UI host (scan)' }
    setSource('local-scan', init, { state: 'loading', error: undefined })
    try {
      const r = await local.client.scan(req, signal)
      const found = r.found ?? []
      setSource('local-scan', init, { state: 'ok', peers: found, message: `${plural(found.length, 'server')} found, ${plural(r.probed, 'URL')} probed in ${r.elapsed}.`, updatedAt: Date.now() })
      return r
    } catch (e) {
      setSource('local-scan', init, { state: 'error', error: e, message: errorMessage(e) })
      throw e
    }
  }

  /**
   * Probes URLs for gos3 servers: directly from the browser (GET
   * <url>/_gos3/info, CORS "*") or, in standalone mode, from the UI host.
   */
  async function probeUrls(urls: readonly string[], via: 'browser' | 'host' = 'browser'): Promise<{ found: Peer[], failed: ProbeFailure[] }> {
    const key = via === 'host' ? 'local-probe' : 'browser'
    const init = via === 'host'
      ? { kind: 'local' as const, label: 'UI host (probe)' }
      : { kind: 'browser' as const, label: 'This browser (direct probe)' }
    probing.value = true
    const found: Peer[] = []
    const failed: ProbeFailure[] = []
    try {
      const queue = [...urls]
      const worker = async () => {
        for (let url = queue.shift(); url !== undefined; url = queue.shift()) {
          try {
            const info = via === 'host' ? await local.client.probe(url) : await fetchInfo(url, { timeoutMs: 5000 })
            found.push(peerFromInfo(info, url, 'probe'))
          } catch (e) {
            failed.push({ url, error: e })
          }
        }
      }
      await Promise.all(Array.from({ length: Math.min(6, urls.length) }, worker))
      const prev = sources.value[key]?.peers ?? []
      const peers = [...prev.filter(p => !found.some(f => f.id === p.id && f.addresses[0] === p.addresses[0])), ...found]
      if (peers.length) setSource(key, init, { state: 'ok', peers, message: plural(new Set(peers.map(p => p.id)).size, 'server'), updatedAt: Date.now() })
      probeFailures.value = [...probeFailures.value.filter(f => !urls.includes(f.url)), ...failed]
      return { found, failed }
    } finally {
      probing.value = false
    }
  }

  function clearProbes(): void {
    delete sources.value.browser
    delete sources.value['local-probe']
    probeFailures.value = []
  }

  async function refreshAll(waitMs = 3000): Promise<void> {
    await Promise.all([refreshServers(), refreshLocal(waitMs)])
  }

  return {
    sources: sourceList,
    servers,
    loading,
    probing,
    probeFailures,
    local,
    refreshServer,
    refreshServers,
    refreshLocal,
    scanLocal,
    probeUrls,
    clearProbes,
    refreshAll
  }
}
