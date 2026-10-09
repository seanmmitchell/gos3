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
