// Connections store: the list of S3 servers the UI manages, which one is
// active, their credentials and a cached online/identity status.
//
// Persistence rules
// - Connection definitions (and accessKeyId) live in localStorage
//   ('gos3.connections.v1').
// - secretAccessKey is written to localStorage only when remember = true;
//   otherwise it is kept in sessionStorage ('gos3.secrets.v1') and is gone
//   when the browser session ends.
// - Presets (runtime config NUXT_PUBLIC_SERVERS and `${baseURL}servers.json`)
//   are merged in on every start; removing a preset hides it permanently.
// - In embedded mode the hosting gos3 server is added as 'This server'.
//
// The store is written with explicit imports (no Nuxt auto-imports) so it can
// be unit-tested with plain Vitest + Pinia. Nuxt wiring lives in
// app/plugins/connections.client.ts.
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type { Capabilities, Info } from '~/types/admin'
import type { Connection, ConnectionInput, ConnectionsExport, ConnectionStatus } from '~/types/connection'
import { AdminError, fetchInfo, probeServer, type FetchLike } from '~/utils/admin/client'
import { clearClientCache, getAdminClient, getS3Client } from '~/utils/clients'
import {
  completeConnection,
  EMBEDDED_CONNECTION_ID,
  endpointSlug,
  hasCredentials,
  parseConnectionList,
  sameEndpoint
} from '~/utils/connections'
import { S3Error } from '~/utils/s3/errors'

export const LOCAL_STORAGE_KEY = 'gos3.connections.v1'
export const SESSION_STORAGE_KEY = 'gos3.secrets.v1'

/** Capabilities of identities without admin access (and of generic S3 servers). */
export const NO_CAPABILITIES: Capabilities = Object.freeze({
  admin: false,
  viewInfo: false,
  viewStats: false,
  manageConfig: false,
  manageUsers: false,
  managePolicies: false,
  manageBuckets: false,
  discovery: false,
  scan: false
})

/**
 * ConnectionStatus.errorCode when the server answers its public /_gos3/info
 * but the browser blocks the signed API requests: the server's CORS settings
 * do not allow this console's origin.
 */
export const CORS_BLOCKED = 'CorsBlocked'

/** Explanation for CORS_BLOCKED, naming the origin to allow. */
export function corsBlockedMessage(endpoint: string): string {
  let host = endpoint
  try {
    host = new URL(endpoint).host
  } catch {
    // keep the raw endpoint
  }
  const origin = globalThis.location?.origin
  const allow = origin && origin !== 'null' ? `add ${origin} to its CORS allowed origins` : 'allow this console\'s origin in its CORS settings'
  return `${host} is reachable, but the browser blocked the request (CORS): ${allow}.`
}

interface PersistedState {
  version: 1
  activeId: string | null
  connections: Connection[]
  dismissedPresets: string[]
}

type SecretMap = Record<string, { secretAccessKey: string }>

export interface InitOptions {
  /** runtimeConfig.public.servers (JSON string or already-parsed value). */
  presets?: unknown
  /** app.baseURL, e.g. '/' or '/_gos3/ui/'. */
  baseURL?: string
  /** runtimeConfig.public.embedded */
  embedded?: boolean | string
  /** location.origin (default globalThis.location.origin). */
  origin?: string
  /** Fetch used for servers.json and /_gos3/info (tests). */
  fetch?: FetchLike
  /** Skip loading `${baseURL}servers.json`. */
  skipServersJson?: boolean
}

function storage(kind: 'local' | 'session'): Storage | undefined {
  try {
    const s = kind === 'local' ? globalThis.localStorage : globalThis.sessionStorage
    return s ?? undefined
  } catch {
    return undefined
  }
}

function readJson<T>(s: Storage | undefined, key: string): T | undefined {
  try {
    const v = s?.getItem(key)
    return v ? (JSON.parse(v) as T) : undefined
  } catch {
    return undefined
  }
}

function writeJson(s: Storage | undefined, key: string, value: unknown): void {
  try {
    s?.setItem(key, JSON.stringify(value))
  } catch {
    // quota / privacy mode: ignore
  }
}

export const useConnectionsStore = defineStore('connections', () => {
  const connections = ref<Connection[]>([])
  const activeId = ref<string | null>(null)
  const statuses = ref<Record<string, ConnectionStatus>>({})
  const dismissedPresets = ref<string[]>([])
  /** Info of the gos3 server hosting the UI (embedded mode), if detected. */
  const embeddedInfo = ref<Info | null>(null)
  const initialized = ref(false)
  let initPromise: Promise<void> | null = null
  let hydrated = false
  const inflight = new Map<string, Promise<ConnectionStatus>>()

  // -- getters -------------------------------------------------------------

  const active = computed(() => connections.value.find(c => c.id === activeId.value))
  const sorted = computed(() =>
    [...connections.value].sort((a, b) => {
      if (a.source === 'embedded' && b.source !== 'embedded') return -1
      if (b.source === 'embedded' && a.source !== 'embedded') return 1
      return a.name.localeCompare(b.name)
    })
  )

  function byId(id: string | null | undefined): Connection | undefined {
    if (!id) return undefined
    return connections.value.find(c => c.id === id)
  }

  function findByEndpoint(endpoint: string): Connection | undefined {
    return connections.value.find(c => sameEndpoint(c.endpoint, endpoint))
  }

  function findByServerId(serverId: string): Connection | undefined {
    return connections.value.find(c => c.serverId === serverId)
  }

  function statusOf(id: string): ConnectionStatus {
    return statuses.value[id] ?? { state: 'unknown', info: null, whoami: null }
  }

  /** Capabilities of the active identity on a connection (none for generic S3). */
  function capabilitiesOf(id: string): Capabilities {
    const c = byId(id)
    if (!c || c.kind !== 'gos3') return NO_CAPABILITIES
    return statuses.value[id]?.whoami?.capabilities ?? NO_CAPABILITIES
  }

  // -- persistence -----------------------------------------------------------

  function persist(): void {
    const secrets: SecretMap = {}
    const stored: Connection[] = connections.value.map((c) => {
      if (c.remember) return { ...c }
      if (c.secretAccessKey) secrets[c.id] = { secretAccessKey: c.secretAccessKey }
      return { ...c, secretAccessKey: '' }
    })
    const state: PersistedState = {
      version: 1,
      activeId: activeId.value,
      connections: stored,
      dismissedPresets: dismissedPresets.value
    }
    writeJson(storage('local'), LOCAL_STORAGE_KEY, state)
    writeJson(storage('session'), SESSION_STORAGE_KEY, secrets)
  }

  /** Loads persisted state (idempotent). */
  function hydrate(): void {
    if (hydrated) return
    hydrated = true
    const state = readJson<PersistedState>(storage('local'), LOCAL_STORAGE_KEY)
    const secrets = readJson<SecretMap>(storage('session'), SESSION_STORAGE_KEY) ?? {}
    if (state && Array.isArray(state.connections)) {
      const list: Connection[] = []
      for (const raw of state.connections) {
        try {
          const c = completeConnection(raw)
          if (!c.remember) c.secretAccessKey = secrets[c.id]?.secretAccessKey ?? ''
          list.push(c)
        } catch {
          // skip corrupt entries
        }
      }
      connections.value = list
      activeId.value = state.activeId && list.some(c => c.id === state.activeId) ? state.activeId : null
      dismissedPresets.value = Array.isArray(state.dismissedPresets) ? state.dismissedPresets : []
    }
  }

  // -- CRUD -----------------------------------------------------------------

  /** Adds a connection (endpoint is normalised; id generated if missing). */
  function add(input: ConnectionInput): Connection {
    const c = completeConnection(input)
    if (byId(c.id)) throw new Error(`A connection with id "${c.id}" already exists`)
    connections.value.push(c)
    persist()
    return c
  }

  /** Updates fields of a connection. Changing endpoint/credentials resets its status. */
  function update(id: string, patch: Partial<Omit<Connection, 'id'>>): Connection {
    const idx = connections.value.findIndex(c => c.id === id)
    if (idx < 0) throw new Error(`Unknown connection "${id}"`)
    const prev = connections.value[idx]!
    const next = completeConnection({ ...prev, ...patch, id })
    connections.value[idx] = next
    if (
      prev.endpoint !== next.endpoint
      || prev.accessKeyId !== next.accessKeyId
      || prev.secretAccessKey !== next.secretAccessKey
      || prev.region !== next.region
      || prev.kind !== next.kind
    ) {
      delete statuses.value[id]
      clearClientCache(id)
    }
    persist()
    return next
  }

  function remove(id: string): void {
    const c = byId(id)
    if (!c) return
    connections.value = connections.value.filter(x => x.id !== id)
    if (c.source === 'preset' && !dismissedPresets.value.includes(id)) dismissedPresets.value.push(id)
    delete statuses.value[id]
    clearClientCache(id)
    if (activeId.value === id) activeId.value = null
    persist()
  }

  function setActive(id: string | null): void {
    if (id !== null && !byId(id)) return
    activeId.value = id
    persist()
  }

  function setCredentials(id: string, creds: { accessKeyId: string, secretAccessKey: string, remember?: boolean }): Connection {
    return update(id, {
      accessKeyId: creds.accessKeyId.trim(),
      secretAccessKey: creds.secretAccessKey,
      ...(creds.remember !== undefined ? { remember: creds.remember } : {})
    })
  }

  /** "Disconnect": forgets the secret key (keeps the definition). */
  function clearCredentials(id: string): void {
    if (!byId(id)) return
    update(id, { secretAccessKey: '' })
    if (activeId.value === id) activeId.value = null
    persist()
  }

  // -- import / export ------------------------------------------------------

  function exportData(opts: { includeSecrets?: boolean, ids?: string[] } = {}): ConnectionsExport {
    const list = connections.value.filter(c => !opts.ids || opts.ids.includes(c.id))
    return {
      type: 'gos3-connections',
      version: 1,
      exportedAt: new Date().toISOString(),
      connections: list.map(c => {
        const out: ConnectionInput = { ...c }
        if (!opts.includeSecrets) {
          delete out.secretAccessKey
          delete out.accessKeyId
        }
        // Embedded connections are environment specific; export as user connections.
        if (out.source === 'embedded' || out.source === 'preset') out.source = 'user'
        return out
      })
    }
  }

  function exportJson(opts: { includeSecrets?: boolean, ids?: string[] } = {}): string {
    return JSON.stringify(exportData(opts), null, 2)
  }

  /**
   * Imports connections from JSON (export file, array, or {servers}). Entries
   * matching an existing connection (same id, or same endpoint + access key)
   * update it; others are added. Throws SyntaxError on invalid JSON.
   */
  function importJson(json: string | unknown): { added: number, updated: number, skipped: number } {
    const items = parseConnectionList(json)
    let added = 0
    let updated = 0
    for (const item of items) {
      const existing = (item.id ? byId(item.id) : undefined)
        ?? connections.value.find(c => sameEndpoint(c.endpoint, item.endpoint) && (!item.accessKeyId || c.accessKeyId === item.accessKeyId))
      const clean: ConnectionInput = { ...item, source: item.source === 'discovered' ? 'discovered' : 'user' }
      if (existing) {
        const patch: Partial<Connection> = {}
        for (const [k, v] of Object.entries(clean)) {
          if (k === 'id' || k === 'source' || v === undefined) continue
          ;(patch as Record<string, unknown>)[k] = v
        }
        update(existing.id, patch)
        updated++
      } else {
        connections.value.push(completeConnection(clean))
        added++
      }
    }
    persist()
    return { added, updated, skipped: 0 }
  }

  // -- presets & embedded ---------------------------------------------------

  /** Merges preset connections (from config / servers.json). */
  function applyPresets(list: ConnectionInput[]): number {
    let n = 0
    for (const p of list) {
      const id = p.id ?? `preset-${endpointSlug(p.endpoint)}`
      if (dismissedPresets.value.includes(id)) continue
      const existing = byId(id)
      if (existing) {
        if (existing.source === 'preset') {
          // Definition comes from the preset; credentials the user entered win.
          const idx = connections.value.indexOf(existing)
          connections.value[idx] = completeConnection({
            ...existing,
            name: p.name ?? existing.name,
            endpoint: p.endpoint,
            region: p.region ?? existing.region,
            kind: p.kind ?? existing.kind,
            forcePathStyle: p.forcePathStyle ?? existing.forcePathStyle,
            color: p.color ?? existing.color,
            accessKeyId: existing.accessKeyId || p.accessKeyId,
            secretAccessKey: existing.secretAccessKey || p.secretAccessKey,
            id
          })
        }
      } else {
        connections.value.push(completeConnection({ ...p, id, source: 'preset', remember: p.remember ?? false }))
      }
      n++
    }
    persist()
    return n
  }

  /** Ensures the 'This server' connection for the hosting gos3. */
  function applyEmbedded(info: Info, origin: string): Connection {
    embeddedInfo.value = info
    const existing = byId(EMBEDDED_CONNECTION_ID)
    const def = {
      id: EMBEDDED_CONNECTION_ID,
      name: info.name ? `${info.name} (this server)` : 'This server',
      endpoint: origin,
      region: info.region || 'us-east-1',
      kind: 'gos3' as const,
      forcePathStyle: true,
      source: 'embedded' as const,
      serverId: info.id
    }
    let c: Connection
    if (existing) {
      c = update(EMBEDDED_CONNECTION_ID, { ...def, name: existing.source === 'embedded' && existing.name !== 'This server' && !existing.name.endsWith('(this server)') ? existing.name : def.name })
    } else {
      c = completeConnection(def)
      connections.value.unshift(c)
    }
    statuses.value[c.id] = { ...statusOf(c.id), info }
    if (!activeId.value) activeId.value = c.id
    persist()
    return c
  }

  async function loadServersJson(baseURL: string, doFetch: FetchLike): Promise<ConnectionInput[]> {
    try {
      const base = baseURL.endsWith('/') ? baseURL : `${baseURL}/`
      const res = await doFetch(new Request(new URL(`${base}servers.json`, globalThis.location?.href ?? 'http://localhost/').toString(), { cache: 'no-store' }))
      if (!res.ok) return []
      const ct = res.headers.get('content-type') ?? ''
      const text = await res.text()
      // SPA fallbacks may answer index.html with 200.
      if (ct.includes('text/html') || text.trimStart().startsWith('<')) return []
      return parseConnectionList(text)
    } catch {
      return []
    }
  }

  /**
   * Initialises the store once: hydrate from storage, merge presets, detect
   * the embedding gos3 server. Safe to call repeatedly (returns the same promise).
   */
  function init(opts: InitOptions = {}): Promise<void> {
    if (initPromise) return initPromise
    initPromise = (async () => {
      hydrate()
      const doFetch: FetchLike = opts.fetch ?? (req => globalThis.fetch(req))
      const baseURL = opts.baseURL ?? '/'
      const presets: ConnectionInput[] = []
      if (opts.presets) {
        try {
          presets.push(...parseConnectionList(opts.presets))
        } catch {
          console.warn('[gos3] invalid NUXT_PUBLIC_SERVERS JSON; ignoring')
        }
      }
      const embedded = opts.embedded === true || opts.embedded === 'true' || baseURL.startsWith('/_gos3/ui')
      const origin = opts.origin ?? globalThis.location?.origin
      const [fileList, info] = await Promise.all([
        opts.skipServersJson ? Promise.resolve([]) : loadServersJson(baseURL, doFetch),
        embedded && origin ? fetchInfo(origin, { timeoutMs: 3000, fetch: doFetch }).catch(() => null) : Promise.resolve(null)
      ])
      presets.push(...fileList)
      if (presets.length) applyPresets(presets)
      if (info && origin) applyEmbedded(info, origin)
      initialized.value = true
    })()
    return initPromise
  }

  /** Resolves once init() finished (immediately hydrates when init was never called). */
  function ready(): Promise<void> {
    if (initPromise) return initPromise
    hydrate()
    return Promise.resolve()
  }

  // -- status ---------------------------------------------------------------

  function setStatus(id: string, s: Partial<ConnectionStatus>): ConnectionStatus {
    const next = { ...statusOf(id), ...s }
    statuses.value[id] = next
    return next
  }

  async function check(c: Connection): Promise<ConnectionStatus> {
    const t0 = Date.now()
    const done = (s: Partial<ConnectionStatus>) => setStatus(c.id, { error: undefined, errorCode: undefined, ...s, checkedAt: Date.now(), latencyMs: Date.now() - t0 })

    if (c.kind === 'gos3') {
      const probe = await probeServer(c.endpoint, { timeoutMs: 5000 })
      const info = probe.info ?? null
      if (!probe.ok && probe.error?.isNetworkError) {
        return done({ state: 'offline', info: statusOf(c.id).info, whoami: null, error: probe.error.message, errorCode: probe.error.code })
      }
      if (!hasCredentials(c)) {
        // Public info reachable (or protected by auth): the server is up.
        const up = probe.ok || probe.error?.code === 'Unauthenticated'
        return done({ state: up ? 'online' : 'error', info, whoami: null, error: up ? undefined : probe.error?.message, errorCode: up ? undefined : probe.error?.code })
      }
      try {
        const whoami = await getAdminClient(c).whoami()
        if (whoami.server?.id && c.serverId !== whoami.server.id) update(c.id, { serverId: whoami.server.id })
        return done({ state: 'online', info: whoami.server ?? info, whoami })
      } catch (e) {
        if (e instanceof AdminError) {
          if (e.status === 401) return done({ state: 'unauthorized', info, whoami: null, error: e.message, errorCode: e.code })
          // /_gos3/info (CORS *) answered, so the server is up: a network error
          // on the signed request is the browser blocking it (CORS).
          if (e.isNetworkError && probe.ok) return done({ state: 'error', info, whoami: null, error: corsBlockedMessage(c.endpoint), errorCode: CORS_BLOCKED })
          if (e.isNetworkError) return done({ state: 'offline', info, whoami: null, error: e.message, errorCode: e.code })
          return done({ state: 'error', info, whoami: null, error: e.message, errorCode: e.code })
        }
        return done({ state: 'error', info, whoami: null, error: String(e) })
      }
    }

    // Generic S3: verify credentials with ListBuckets; opportunistically detect gos3.
    const infoP = probeServer(c.endpoint, { timeoutMs: 3000 }).then(p => p.info ?? null)
    if (!hasCredentials(c)) return done({ state: 'unknown', info: await infoP, whoami: null })
    try {
      await getS3Client(c).listBuckets()
      return done({ state: 'online', info: await infoP, whoami: null })
    } catch (e) {
      const info = await infoP
      if (e instanceof S3Error) {
        if (e.code === 'NetworkError' && info) return done({ state: 'error', info, whoami: null, error: corsBlockedMessage(c.endpoint), errorCode: CORS_BLOCKED })
        if (e.code === 'NetworkError') return done({ state: 'offline', info, whoami: null, error: e.message, errorCode: e.code })
        if (['InvalidAccessKeyId', 'SignatureDoesNotMatch', 'InvalidToken', 'ExpiredToken', 'Unauthorized'].includes(e.code)) {
          return done({ state: 'unauthorized', info, whoami: null, error: e.message, errorCode: e.code })
        }
        // AccessDenied on ListBuckets: credentials valid but restricted.
        if (e.code === 'AccessDenied') return done({ state: 'online', info, whoami: null, error: e.message, errorCode: e.code })
        return done({ state: 'error', info, whoami: null, error: e.message, errorCode: e.code })
      }
      return done({ state: 'error', info, whoami: null, error: String(e) })
    }
  }

  /** Re-checks a connection (deduplicated while in flight). */
  function refreshStatus(id: string): Promise<ConnectionStatus> {
    const c = byId(id)
    if (!c) return Promise.resolve({ state: 'unknown', info: null, whoami: null })
    const running = inflight.get(id)
    if (running) return running
    setStatus(id, { state: 'checking' })
    const p = check(c).finally(() => inflight.delete(id))
    inflight.set(id, p)
    return p
  }

  /** Returns the cached status if newer than maxAgeMs, else refreshes. */
  async function ensureStatus(id: string, maxAgeMs = 30000): Promise<ConnectionStatus> {
    const s = statuses.value[id]
    if (s?.checkedAt && Date.now() - s.checkedAt < maxAgeMs && s.state !== 'checking') return s
    return refreshStatus(id)
  }

  function refreshAll(): Promise<ConnectionStatus[]> {
    return Promise.all(connections.value.map(c => refreshStatus(c.id)))
  }

  /** Test helper: resets all state (does not touch storage). */
  function resetState(): void {
    connections.value = []
    activeId.value = null
    statuses.value = {}
    dismissedPresets.value = []
    embeddedInfo.value = null
    initialized.value = false
    initPromise = null
    hydrated = false
    inflight.clear()
  }

  return {
    // state
    connections,
    activeId,
    statuses,
    dismissedPresets,
    embeddedInfo,
    initialized,
    // getters
    active,
    sorted,
    byId,
    findByEndpoint,
    findByServerId,
    statusOf,
    capabilitiesOf,
    hasCredentials,
    // actions
    init,
    ready,
    hydrate,
    persist,
    add,
    update,
    remove,
    setActive,
    setCredentials,
    clearCredentials,
    exportData,
    exportJson,
    importJson,
    applyPresets,
    applyEmbedded,
    refreshStatus,
    ensureStatus,
    refreshAll,
    resetState
  }
})
