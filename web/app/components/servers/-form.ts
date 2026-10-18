// Pure model/validation helpers of the add/edit server form
// (ServerFormModal.vue), the connect page and discovery "add as server".
// Plain TS (no auto-imports) so it is unit-testable.
import type { Peer } from '~/types/admin'
import type { Connection, ConnectionInput, ConnectionKind } from '~/types/connection'
import { DEFAULT_REGION, hasCredentials, sameEndpoint } from '~/utils/connections'
import { normalizeEndpoint } from '~/utils/s3/url'

/** Kind selector: 'auto' = detect by probing /_gos3/info. */
export type KindChoice = 'auto' | ConnectionKind

export interface ServerFormModel {
  name: string
  endpoint: string
  region: string
  accessKeyId: string
  secretAccessKey: string
  kind: KindChoice
  forcePathStyle: boolean
  remember: boolean
  proxyConnectionId?: string
  color: string
  /** gos3 server ID when known (discovery prefill / detection). */
  serverId?: string
}

export type ServerFormErrors = Partial<Record<keyof ServerFormModel, string>>

export function emptyServerForm(): ServerFormModel {
  return {
    name: '',
    endpoint: '',
    region: '',
    accessKeyId: '',
    secretAccessKey: '',
    kind: 'auto',
    forcePathStyle: true,
    remember: false,
    color: ''
  }
}

export function formFromConnection(c: Connection): ServerFormModel {
  return {
    name: c.name,
    endpoint: c.endpoint,
    region: c.region,
    accessKeyId: c.accessKeyId,
    secretAccessKey: c.secretAccessKey,
    kind: c.kind,
    forcePathStyle: c.forcePathStyle,
    remember: c.remember,
    proxyConnectionId: c.proxyConnectionId,
    color: c.color ?? '',
    serverId: c.serverId
  }
}

/** Form prefilled from a partial connection (discovery, query string, duplicate). */
export function formFromInput(p: Partial<ConnectionInput>): ServerFormModel {
  return {
    ...emptyServerForm(),
    name: p.name ?? '',
    endpoint: p.endpoint ?? '',
    region: p.region ?? '',
    accessKeyId: p.accessKeyId ?? '',
    secretAccessKey: p.secretAccessKey ?? '',
    kind: p.kind ?? 'auto',
    forcePathStyle: p.forcePathStyle ?? true,
    remember: p.remember ?? false,
    proxyConnectionId: p.proxyConnectionId,
    color: p.color ?? '',
    serverId: p.serverId
  }
}

/** Validation message for a user-entered endpoint, or undefined when valid. */
export function validateEndpoint(raw: string): string | undefined {
  const s = raw.trim()
  if (!s) return 'Enter the server URL, e.g. http://10.0.0.5:9000'
  if (/\s/.test(s)) return 'The URL must not contain spaces'
  let url: URL
  try {
    normalizeEndpoint(s)
    url = new URL(/^[a-z][a-z0-9+.-]*:\/\//i.test(s) ? s : `http://${s}`)
  } catch (e) {
    return e instanceof TypeError && /http or https/.test(e.message) ? 'Only http:// and https:// URLs are supported' : 'This is not a valid URL'
  }
  if (!url.hostname) return 'The URL needs a host name'
  if (url.username || url.password) return 'Do not put credentials in the URL; use the access key fields'
  if (url.search || url.hash) return 'The URL must not contain a query string or fragment'
  return undefined
}

/** Region: empty (default) or an S3 region-like token. */
export function validateRegion(raw: string): string | undefined {
  const s = raw.trim()
  if (!s) return undefined
  if (!/^[a-z0-9][a-z0-9-]{0,62}$/i.test(s)) return 'Use letters, digits and hyphens only (e.g. us-east-1)'
  return undefined
}

export function validateServerForm(m: ServerFormModel, opts: { requireCredentials?: boolean } = {}): ServerFormErrors {
  const errors: ServerFormErrors = {}
  const ep = validateEndpoint(m.endpoint)
  if (ep) errors.endpoint = ep
  const region = validateRegion(m.region)
  if (region) errors.region = region
  if (m.name.length > 80) errors.name = 'At most 80 characters'
  const ak = m.accessKeyId.trim()
  const sk = m.secretAccessKey
  if (opts.requireCredentials && !ak) errors.accessKeyId = 'Enter the access key'
  if (opts.requireCredentials && !sk) errors.secretAccessKey = 'Enter the secret key'
  if (ak && !sk && !opts.requireCredentials) errors.secretAccessKey = 'Enter the secret key too (or leave both empty)'
  if (!ak && sk) errors.accessKeyId = 'Enter the access key too (or leave both empty)'
  if (/\s/.test(ak)) errors.accessKeyId = 'The access key must not contain spaces'
  return errors
}

/** Effective kind: the explicit choice, else what probing detected, else generic S3. */
export function resolveKind(choice: KindChoice, detected?: ConnectionKind): ConnectionKind {
  if (choice !== 'auto') return choice
  return detected ?? 's3'
}

/** Converts the form into a ConnectionInput for store.add / store.update. */
export function formToInput(m: ServerFormModel, detected?: ConnectionKind): ConnectionInput {
  const endpoint = normalizeEndpoint(m.endpoint)
  return {
    name: m.name.trim() || undefined,
    endpoint,
    region: m.region.trim() || DEFAULT_REGION,
    accessKeyId: m.accessKeyId.trim(),
    secretAccessKey: m.secretAccessKey,
    kind: resolveKind(m.kind, detected),
    forcePathStyle: m.forcePathStyle,
    remember: m.remember,
    proxyConnectionId: m.proxyConnectionId,
    color: m.color || undefined,
    serverId: m.serverId
  }
}

/** Presets and the embedded server get their definition from configuration. */
export function isDefinitionLocked(c: Pick<Connection, 'source'> | undefined): boolean {
  return c?.source === 'preset' || c?.source === 'embedded'
}

/** Name for a copy: "X (copy)", "X (copy 2)", … unique among existing names. */
export function copyName(name: string, existing: readonly string[]): string {
  const base = name.replace(/ \(copy(?: \d+)?\)$/, '')
  let candidate = `${base} (copy)`
  for (let i = 2; existing.includes(candidate); i++) candidate = `${base} (copy ${i})`
  return candidate
}

/** Input for duplicating a connection (new id, user source, same credentials). */
export function duplicateInput(c: Connection, existingNames: readonly string[]): ConnectionInput {
  return {
    name: copyName(c.name, existingNames),
    endpoint: c.endpoint,
    region: c.region,
    accessKeyId: c.accessKeyId,
    secretAccessKey: c.secretAccessKey,
    kind: c.kind,
    forcePathStyle: c.forcePathStyle,
    remember: c.remember,
    source: 'user',
    serverId: c.serverId,
    proxyConnectionId: c.proxyConnectionId,
    color: c.color
  }
}

/** Accent colors offered in the form (stored as hex in Connection.color). */
export const CONNECTION_COLORS: readonly { label: string, value: string }[] = [
  { label: 'Teal', value: '#14b8a6' },
  { label: 'Blue', value: '#3b82f6' },
  { label: 'Violet', value: '#8b5cf6' },
  { label: 'Pink', value: '#ec4899' },
  { label: 'Red', value: '#ef4444' },
  { label: 'Orange', value: '#f97316' },
  { label: 'Amber', value: '#f59e0b' },
  { label: 'Green', value: '#22c55e' },
  { label: 'Slate', value: '#64748b' }
]

const UI_COLORS = ['primary', 'secondary', 'success', 'info', 'warning', 'error', 'neutral']

/** CSS color for Connection.color (hex or a Nuxt UI color name), else undefined. */
export function connectionColor(color: string | undefined): string | undefined {
  if (!color) return undefined
  if (/^#(?:[0-9a-f]{3}|[0-9a-f]{6}|[0-9a-f]{8})$/i.test(color)) return color
  if (UI_COLORS.includes(color)) return `var(--ui-${color})`
  return undefined
}

/** An https page cannot call an http endpoint (except localhost). */
export function isMixedContent(endpoint: string, pageProtocol: string | undefined): boolean {
  if (pageProtocol !== 'https:') return false
  try {
    const u = new URL(normalizeEndpoint(endpoint))
    if (u.protocol !== 'http:') return false
    return !(u.hostname === 'localhost' || u.hostname === '127.0.0.1' || u.hostname === '[::1]' || u.hostname.endsWith('.localhost'))
  } catch {
    return false
  }
}

/**
 * Base URL of a discovered peer for "Add as server": the first address the
 * peer was observed and verified at (`addresses`). The peer's self-declared
 * `endpoints` come from its own /_gos3/info document, which any host can
 * fill in (discovery is unauthenticated, docs/discovery.md "Peer identity"),
 * so they are never prefilled; see peerAdvertisedEndpoint.
 */
export function peerEndpoint(p: Pick<Peer, 'addresses'>): string | undefined {
  for (const c of Array.isArray(p.addresses) ? p.addresses : []) {
    if (!c || typeof c !== 'string') continue
    try {
      return normalizeEndpoint(c)
    } catch {
      // try next
    }
  }
  return undefined
}

/**
 * The S3 endpoint a peer advertises about itself when it differs from
 * `chosen` (the prefilled, observed address), else undefined. Shown as an
 * unverified hint only.
 */
export function peerAdvertisedEndpoint(p: Pick<Peer, 'endpoints'>, chosen: string | undefined): string | undefined {
  const all: unknown[] = Array.isArray(p.endpoints?.all) ? p.endpoints.all : []
  for (const c of [p.endpoints?.s3, ...all]) {
    if (!c || typeof c !== 'string') continue
    try {
      const ep = normalizeEndpoint(c)
      return chosen && sameEndpoint(ep, chosen) ? undefined : ep
    } catch {
      // try next
    }
  }
  return undefined
}

/**
 * Connection prefill for "Add as server" from a discovered peer (endpoint
 * from peerEndpoint). It never carries credentials: reusing another
 * connection's keys is a separate, explicit choice in the form.
 */
export function prefillFromPeer(p: Pick<Peer, 'id' | 'name' | 'region' | 'addresses'>): ConnectionInput | undefined {
  const endpoint = peerEndpoint(p)
  if (!endpoint) return undefined
  return {
    name: typeof p.name === 'string' && p.name ? p.name : undefined,
    endpoint,
    region: typeof p.region === 'string' && p.region && !validateRegion(p.region) ? p.region : undefined,
    kind: 'gos3',
    forcePathStyle: true,
    serverId: p.id,
    source: 'discovered'
  }
}

/** Lowercased host[:port] of an endpoint, or undefined when it is not a valid URL. */
export function endpointHost(endpoint: string): string | undefined {
  try {
    return new URL(normalizeEndpoint(endpoint)).host.toLowerCase()
  } catch {
    return undefined
  }
}

/**
 * Host that would receive requests signed with `from`'s credentials when they
 * are reused for `endpoint`, or undefined when that is `from`'s own host
 * (or the endpoint is not valid yet). Discovered servers are not
 * authenticated, so the form names this host and asks before signing for it.
 */
export function credentialReuseHost(endpoint: string, from: Pick<Connection, 'endpoint'>): string | undefined {
  const host = endpointHost(endpoint)
  if (!host || host === endpointHost(from.endpoint)) return undefined
  return host
}

/**
 * The stored connection that /connect may put credentials on after they were
 * verified against `input.endpoint`: one with the same endpoint (the one
 * with the same server ID first) that has no credentials or the same access
 * key. Server IDs are not authenticated (a peer, import file or rogue
 * /_gos3/info can claim any ID), so a connection that only shares the ID but
 * points elsewhere is never reused: the verified credentials would otherwise
 * be signed for that other host.
 */
export function reusableConnection(
  list: readonly Connection[],
  input: Pick<ConnectionInput, 'endpoint' | 'serverId' | 'accessKeyId'>
): Connection | undefined {
  const same = list.filter(c => sameEndpoint(c.endpoint, input.endpoint))
  const candidates = [...same.filter(c => !!input.serverId && c.serverId === input.serverId), ...same]
  return candidates.find(c => c.accessKeyId === (input.accessKeyId ?? '') || !hasCredentials(c))
}

/** Lowercased origin (scheme://host[:port], default ports dropped) of an endpoint, or undefined. */
export function endpointOrigin(endpoint: string): string | undefined {
  try {
    return new URL(normalizeEndpoint(endpoint)).origin.toLowerCase()
  } catch {
    return undefined
  }
}

/**
 * Warning for a /connect form prefilled from link parameters (?endpoint=):
 * `origin` is the prefilled endpoint's scheme://host[:port]. `unknown` is
 * true when that origin is neither this page's nor a configured
 * connection's: the user must confirm before keys are signed for it.
 * `downgrade` is true when it is plain http for a host this page or a
 * configured connection reaches over https (also `unknown`). Undefined
 * when the endpoint is not a valid URL.
 */
export function linkPrefillCheck(
  endpoint: string,
  pageOrigin: string | undefined,
  known: readonly Pick<Connection, 'endpoint'>[]
): { origin: string, unknown: boolean, downgrade: boolean } | undefined {
  const origin = endpointOrigin(endpoint)
  if (!origin) return undefined
  const origins = [pageOrigin && pageOrigin !== 'null' ? pageOrigin.toLowerCase() : undefined, ...known.map(c => endpointOrigin(c.endpoint))]
    .filter((o): o is string => !!o)
  const hostname = (o: string) => new URL(o).hostname
  const downgrade = origin.startsWith('http:') && origins.some(o => o.startsWith('https:') && hostname(o) === hostname(origin))
  const unknown = downgrade || !origins.includes(origin)
  return { origin, unknown, downgrade }
}

/**
 * Internal redirect target from ?redirect=, or undefined. Rejects external /
 * protocol-relative URLs, the connect page itself, and paths with '.' / '..'
 * segments, encoded slashes or backslashes, or control characters, in any
 * level of percent-encoding: vue-router decodes route params, so
 * '/servers/x/buckets/%2E%2E/…' would open a bucket named '..' and the S3
 * request URL would resolve to another path.
 */
export function safeRedirect(v: unknown): string | undefined {
  const s = Array.isArray(v) ? v[0] : v
  if (typeof s !== 'string' || !s.startsWith('/') || s.startsWith('//')) return undefined
  if (s.startsWith('/connect')) return undefined
  // Backslashes ('\' counts as '/' in http URLs) and control characters.
  // eslint-disable-next-line no-control-regex
  if (/[\\\x00-\x1f\x7f]/.test(s)) return undefined
  const path = s.split(/[?#]/, 1)[0]!
  for (const raw of path.split('/')) {
    // The raw segment, what the router decodes it to, and one more level
    // ('%252E%252E' → '%2E%2E' → '..').
    let seg = raw
    for (let level = 0; level < 3; level++) {
      // eslint-disable-next-line no-control-regex
      if (seg === '.' || seg === '..' || /[\\\x00-\x1f\x7f]/.test(seg)) return undefined
      if (level === 1 && seg.includes('/')) return undefined // an encoded slash
      let next: string
      try {
        next = decodeURIComponent(seg)
      } catch {
        break // a literal '%' (e.g. in an object key)
      }
      if (next === seg) break
      seg = next
    }
  }
  return s
}
