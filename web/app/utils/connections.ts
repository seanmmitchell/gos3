// Pure helpers for connection definitions (auto-imported by Nuxt).
import type { Connection, ConnectionInput, ConnectionKind, ConnectionSource } from '~/types/connection'
import { normalizeEndpoint } from './s3/url'

export const EMBEDDED_CONNECTION_ID = 'this-server'
export const DEFAULT_REGION = 'us-east-1'

/** Short random ID for connections (URL friendly). */
export function newConnectionId(): string {
  const bytes = new Uint8Array(8)
  globalThis.crypto.getRandomValues(bytes)
  return Array.from(bytes, b => (b % 36).toString(36)).join('')
}

/** Slug of an endpoint, used for deterministic preset IDs: http://a.b:9000 → a-b-9000. */
export function endpointSlug(endpoint: string): string {
  return endpoint
    .replace(/^https?:\/\//, '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 48)
}

/** Default display name from an endpoint: host[:port]. */
export function defaultConnectionName(endpoint: string): string {
  try {
    return new URL(endpoint).host
  } catch {
    return endpoint
  }
}

export function hasCredentials(c: Pick<Connection, 'accessKeyId' | 'secretAccessKey'> | undefined | null): boolean {
  return !!c && !!c.accessKeyId && !!c.secretAccessKey
}

function str(v: unknown): string | undefined {
  return typeof v === 'string' && v.trim() !== '' ? v.trim() : undefined
}

/**
 * Leniently converts an unknown JSON value (servers.json entry, import file
 * entry) into a ConnectionInput. Accepts aliases: url → endpoint,
 * accessKey → accessKeyId, secretKey → secretAccessKey, pathStyle → forcePathStyle.
 * Returns undefined when there is no usable endpoint.
 */
export function toConnectionInput(raw: unknown): ConnectionInput | undefined {
  if (typeof raw !== 'object' || raw === null) return undefined
  const o = raw as Record<string, unknown>
  const endpointRaw = str(o.endpoint) ?? str(o.url)
  if (!endpointRaw) return undefined
  let endpoint: string
  try {
    endpoint = normalizeEndpoint(endpointRaw)
  } catch {
    return undefined
  }
  const kind = o.kind === 's3' || o.kind === 'gos3' ? (o.kind as ConnectionKind) : undefined
  const source = ['user', 'preset', 'embedded', 'discovered'].includes(o.source as string) ? (o.source as ConnectionSource) : undefined
  const fps = typeof o.forcePathStyle === 'boolean' ? o.forcePathStyle : typeof o.pathStyle === 'boolean' ? o.pathStyle : undefined
  return {
    id: str(o.id),
    name: str(o.name),
    endpoint,
    region: str(o.region),
    accessKeyId: str(o.accessKeyId) ?? str(o.accessKey),
    secretAccessKey: str(o.secretAccessKey) ?? str(o.secretKey),
    kind,
    forcePathStyle: fps,
    remember: typeof o.remember === 'boolean' ? o.remember : undefined,
    source,
    serverId: str(o.serverId),
    proxyConnectionId: str(o.proxyConnectionId),
    color: str(o.color)
  }
}

/**
 * Parses a list of connections from JSON text: an array, {servers: [...]},
 * or an export file {type:'gos3-connections', connections: [...]}.
 * Invalid entries are skipped. Throws SyntaxError on invalid JSON.
 */
export function parseConnectionList(json: string | unknown): ConnectionInput[] {
  const data: unknown = typeof json === 'string' ? (json.trim() ? JSON.parse(json) : []) : json
  let list: unknown[] = []
  if (Array.isArray(data)) list = data
  else if (typeof data === 'object' && data !== null) {
    const o = data as Record<string, unknown>
    if (Array.isArray(o.connections)) list = o.connections
    else if (Array.isArray(o.servers)) list = o.servers
    else if (o.endpoint || o.url) list = [o]
  }
  return list.map(toConnectionInput).filter((c): c is ConnectionInput => !!c)
}

/** Fills defaults to produce a complete Connection. */
export function completeConnection(input: ConnectionInput, defaults: Partial<Connection> = {}): Connection {
  const endpoint = normalizeEndpoint(input.endpoint)
  return {
    id: input.id ?? defaults.id ?? newConnectionId(),
    name: input.name ?? defaults.name ?? defaultConnectionName(endpoint),
    endpoint,
    region: input.region ?? defaults.region ?? DEFAULT_REGION,
    accessKeyId: input.accessKeyId ?? defaults.accessKeyId ?? '',
    secretAccessKey: input.secretAccessKey ?? defaults.secretAccessKey ?? '',
    kind: input.kind ?? defaults.kind ?? 'gos3',
    forcePathStyle: input.forcePathStyle ?? defaults.forcePathStyle ?? true,
    remember: input.remember ?? defaults.remember ?? false,
    source: input.source ?? defaults.source ?? 'user',
    serverId: input.serverId ?? defaults.serverId,
    proxyConnectionId: input.proxyConnectionId ?? defaults.proxyConnectionId,
    color: input.color ?? defaults.color
  }
}

/** Two endpoints point to the same server (scheme/host/port/path equal). */
export function sameEndpoint(a: string, b: string): boolean {
  try {
    return normalizeEndpoint(a).toLowerCase() === normalizeEndpoint(b).toLowerCase()
  } catch {
    return false
  }
}

// -- import planning ----------------------------------------------------------

/** Connections an import may never change (they come from the environment). */
export function isProtectedConnection(c: Pick<Connection, 'source'>): boolean {
  return c.source === 'embedded' || c.source === 'preset'
}

/** IDs an imported entry may not claim: the embedded server and deterministic preset IDs. */
function isReservedId(id: string): boolean {
  return id === EMBEDDED_CONNECTION_ID || id.startsWith('preset-')
}

/** Fields an import may change on an existing connection (endpoint and kind never). */
export type ImportField = 'name' | 'region' | 'accessKeyId' | 'secretAccessKey' | 'forcePathStyle' | 'serverId' | 'color' | 'remember'

const IMPORT_FIELDS: readonly ImportField[] = ['name', 'region', 'accessKeyId', 'secretAccessKey', 'forcePathStyle', 'serverId', 'color']

/** One field an import changes. Secret values are never included (only 'set' / 'none'). */
export interface ImportFieldChange {
  field: ImportField
  from: string
  to: string
}

/** Why an entry that matched an existing connection was added as a new one. */
export type ImportAddReason = 'retarget' | 'protected'

/**
 * What importing one entry does:
 * - add: a new connection. `instead` is set when the entry matched an
 *   existing connection that is left untouched because the entry points
 *   elsewhere (other endpoint, kind or fallback relay): 'retarget' for a user connection,
 *   'protected' for an embedded/preset one. The new connection gets a fresh
 *   ID and only the credentials the file itself holds.
 * - update: applies `patch` (described by `changes`) to `target`, whose
 *   endpoint and kind stay the same.
 * - skip: `target` is an embedded/preset connection ('protected'), or the
 *   entry would not change anything ('unchanged').
 */
export type ImportStep =
  | { action: 'add', input: ConnectionInput & { id: string }, instead?: { of: Connection, reason: ImportAddReason } }
  | { action: 'update', target: Connection, patch: Partial<Omit<Connection, 'id'>>, changes: ImportFieldChange[] }
  | { action: 'skip', target: Connection, reason: 'protected' | 'unchanged' }

export interface ImportPlan {
  steps: ImportStep[]
  added: number
  updated: number
  skipped: number
}

function displayValue(field: ImportField, v: unknown): string {
  if (field === 'secretAccessKey') return v ? 'set' : 'none'
  if (field === 'remember') return v ? 'on' : 'off'
  if (v === undefined || v === '') return '—'
  return String(v)
}

/**
 * Plans importing `items` into `existing` without changing anything (the
 * import preview and the store's importJson share it). An entry matches an
 * existing connection by ID, or by endpoint when the access keys agree.
 * Entries are planned in order, each against the result of the previous ones.
 *
 * Imported data must never redirect credentials the user already stored, so:
 * - an entry that would change a match's normalized endpoint, kind or fallback relay is added
 *   as a separate connection (fresh ID, only the file's own credentials); the
 *   existing connection keeps its endpoint and secret. Updating it with the
 *   credentials cleared was the alternative; a new entry is clearer because
 *   the connection the user trusts stays exactly as it was, and a familiar
 *   name never asks for credentials on behalf of an unfamiliar host.
 * - embedded and preset connections are never modified: a matching entry for
 *   another endpoint or kind is added as a new connection, otherwise it is
 *   skipped. New entries never claim their reserved IDs.
 * - when an entry changes the access key ID without giving a secret, the
 *   stored secret (which belongs to the old key) is cleared.
 * - `remember` is ignored: only the user decides whether a secret is written
 *   to localStorage (imported connections start session-only). An import
 *   that replaces the secret of a remembered connection turns "remember"
 *   off, so the imported secret is kept in sessionStorage too.
 * Region and path style may change: requests still go to the same host
 * (virtual-hosted style only prefixes it with the bucket name) and SigV4
 * signatures are bound to that host.
 */
export function planConnectionImport(existing: readonly Connection[], items: readonly ConnectionInput[]): ImportPlan {
  const work = [...existing]
  const steps: ImportStep[] = []
  const freshId = () => {
    let id = newConnectionId()
    while (work.some(c => c.id === id)) id = newConnectionId()
    return id
  }
  const add = (input: ConnectionInput, instead?: { of: Connection, reason: ImportAddReason }) => {
    const id = !instead && input.id && !isReservedId(input.id) ? input.id : freshId()
    const step: ImportStep = { action: 'add', input: { ...input, id }, ...(instead ? { instead } : {}) }
    steps.push(step)
    work.push(completeConnection(step.input))
  }

  for (const item of items) {
    const { remember: _remember, ...rest } = item
    const clean: ConnectionInput = { ...rest, source: item.source === 'discovered' ? 'discovered' : 'user' }
    const byEndpoint = work.filter(c => sameEndpoint(c.endpoint, item.endpoint) && (!item.accessKeyId || c.accessKeyId === item.accessKeyId))
    const target = (item.id ? work.find(c => c.id === item.id) : undefined)
      ?? byEndpoint.find(c => !item.kind || c.kind === item.kind)
      ?? byEndpoint[0]
    if (!target) {
      add(clean)
      continue
    }
    const prot = isProtectedConnection(target)
    if (!sameEndpoint(target.endpoint, item.endpoint) || (item.kind !== undefined && item.kind !== target.kind) || item.proxyConnectionId !== target.proxyConnectionId) {
      add(clean, { of: target, reason: prot ? 'protected' : 'retarget' })
      continue
    }
    if (prot) {
      steps.push({ action: 'skip', target, reason: 'protected' })
      continue
    }
    const patch: Partial<Omit<Connection, 'id'>> = {}
    const changes: ImportFieldChange[] = []
    const set = (field: ImportField, v: unknown) => {
      ;(patch as Record<string, unknown>)[field] = v
      changes.push({ field, from: displayValue(field, target[field]), to: displayValue(field, v) })
    }
    for (const field of IMPORT_FIELDS) {
      const v = clean[field]
      if (v !== undefined && v !== target[field]) set(field, v)
    }
    if (patch.accessKeyId !== undefined && clean.secretAccessKey === undefined && target.secretAccessKey) set('secretAccessKey', '')
    if (patch.secretAccessKey && target.remember) set('remember', false)
    if (!changes.length) {
      steps.push({ action: 'skip', target, reason: 'unchanged' })
      continue
    }
    steps.push({ action: 'update', target, patch, changes })
    work[work.indexOf(target)] = completeConnection({ ...target, ...patch, id: target.id })
  }
  const count = (a: ImportStep['action']) => steps.filter(s => s.action === a).length
  return { steps, added: count('add'), updated: count('update'), skipped: count('skip') }
}
