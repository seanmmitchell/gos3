// Form model for config.Settings (GET/PUT /config).
//
// The admin API speaks nanoseconds (Go time.Duration) and nullable lists;
// the settings form edits seconds and plain arrays. These helpers convert
// both ways, normalise list editors and validate the form before saving.
// The server still validates (400 InvalidSettings) and its message is shown.
// Sections a server may not have (compression, on servers that predate it)
// are null in the form and left out of the saved document.
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { CompressionMode, Settings } from '~/types/admin'
import { nsToSeconds, secondsToNs } from '~/utils/format'

export const LOG_LEVELS = ['debug', 'info', 'warn', 'error'] as const
export const LOG_FORMATS = ['text', 'json'] as const
export const COMPRESSION_MODES = ['optional', 'enforced', 'disabled'] as const satisfies readonly CompressionMode[]

export interface SettingsForm {
  name: string
  region: string
  publicUrl: string
  domains: string[]
  publicInfo: boolean
  uiRedirect: boolean
  metricsPublic: boolean
  cors: {
    allowedOrigins: string[]
    allowedHeaders: string[]
    exposeHeaders: string[]
    maxAgeSeconds: number
  }
  discovery: {
    enabled: boolean
    advertise: boolean
    mdns: boolean
    beacon: boolean
    beaconGroup: string
    /** Seconds (API: nanoseconds). */
    beaconIntervalSeconds: number
    seeds: string[]
    dnsSrv: string[]
    /** Seconds (API: nanoseconds). */
    peerTtlSeconds: number
    allowScan: boolean
    maxScanHosts: number
  }
  logging: {
    level: string
    format: string
    accessLog: boolean
  }
  lifecycle: {
    enabled: boolean
    intervalSeconds: number
  }
  /** null when the server has no compression settings (older gos3). */
  compression: {
    mode: string
    level: number
    /** Bytes. */
    minSize: number
    skipContentTypes: string[]
  } | null
}

/** Field path → error message. Paths match the form (e.g. "discovery.beaconGroup"). */
export type SettingsErrors = Partial<Record<string, string>>

/** Trims entries, drops empty ones and removes duplicates (first wins). */
export function normalizeList(items: readonly (string | null | undefined)[] | null | undefined): string[] {
  const out: string[] = []
  const seen = new Set<string>()
  for (const raw of items ?? []) {
    const v = (raw ?? '').trim()
    if (!v || seen.has(v)) continue
    seen.add(v)
    out.push(v)
  }
  return out
}

/** Parses a textarea / pasted list (newline, comma or whitespace separated). */
export function parseList(text: string): string[] {
  return normalizeList(text.split(/[\s,]+/))
}

/** Joins a list for a textarea (one item per line). */
export function formatList(items: readonly string[] | null | undefined): string {
  return (items ?? []).join('\n')
}

/** Rounds a seconds value from nanoseconds to at most 3 decimals (ms precision). */
function nsToFormSeconds(ns: number | null | undefined): number {
  if (!ns || !Number.isFinite(ns)) return 0
  return Math.round(nsToSeconds(ns) * 1000) / 1000
}

/** A blank form (placeholder until the settings are loaded). */
export function emptySettingsForm(): SettingsForm {
  return {
    name: '',
    region: '',
    publicUrl: '',
    domains: [],
    publicInfo: false,
    uiRedirect: false,
    metricsPublic: false,
    cors: { allowedOrigins: [], allowedHeaders: [], exposeHeaders: [], maxAgeSeconds: 0 },
    discovery: {
      enabled: false,
      advertise: false,
      mdns: false,
      beacon: false,
      beaconGroup: '',
      beaconIntervalSeconds: 0,
      seeds: [],
      dnsSrv: [],
      peerTtlSeconds: 0,
      allowScan: false,
      maxScanHosts: 0
    },
    logging: { level: 'info', format: 'text', accessLog: false },
    lifecycle: { enabled: false, intervalSeconds: 0 },
    compression: null
  }
}

/** API settings → editable form (deep copy; null lists → []; ns → s). */
export function settingsToForm(s: Settings): SettingsForm {
  return {
    name: s.name ?? '',
    region: s.region ?? '',
    publicUrl: s.publicUrl ?? '',
    domains: [...(s.domains ?? [])],
    publicInfo: !!s.publicInfo,
    uiRedirect: !!s.uiRedirect,
    metricsPublic: !!s.metricsPublic,
    cors: {
      allowedOrigins: [...(s.cors?.allowedOrigins ?? [])],
      allowedHeaders: [...(s.cors?.allowedHeaders ?? [])],
      exposeHeaders: [...(s.cors?.exposeHeaders ?? [])],
      maxAgeSeconds: s.cors?.maxAgeSeconds ?? 0
    },
    discovery: {
      enabled: !!s.discovery?.enabled,
      advertise: !!s.discovery?.advertise,
      mdns: !!s.discovery?.mdns,
      beacon: !!s.discovery?.beacon,
      beaconGroup: s.discovery?.beaconGroup ?? '',
      beaconIntervalSeconds: nsToFormSeconds(s.discovery?.beaconInterval),
      seeds: [...(s.discovery?.seeds ?? [])],
      dnsSrv: [...(s.discovery?.dnsSrv ?? [])],
      peerTtlSeconds: nsToFormSeconds(s.discovery?.peerTtl),
      allowScan: !!s.discovery?.allowScan,
      maxScanHosts: s.discovery?.maxScanHosts ?? 0
    },
    logging: {
      level: s.logging?.level ?? 'info',
      format: s.logging?.format ?? 'text',
      accessLog: !!s.logging?.accessLog
    },
    lifecycle: {
      enabled: !!s.lifecycle?.enabled,
      intervalSeconds: s.lifecycle?.intervalSeconds ?? 0
    },
    compression: s.compression
      ? {
          mode: s.compression.mode ?? 'optional',
          level: s.compression.level ?? 1,
          minSize: s.compression.minSize ?? 0,
          skipContentTypes: [...(s.compression.skipContentTypes ?? [])]
        }
      : null
  }
}

/**
 * Form → full settings document for PUT /config. Fields of `base` (the
 * settings last loaded from the server) that the form does not know are
 * preserved, so a newer server never loses settings through this UI. A
 * null form section (the server did not send it) is not added: an older
 * server gets no compression object, and a newer one keeps its current
 * compression settings when the document has none.
 */
export function formToSettings(f: SettingsForm, base?: Settings): Settings {
  return {
    ...(base ?? {}),
    name: f.name.trim(),
    region: f.region.trim(),
    publicUrl: f.publicUrl.trim().replace(/\/+$/, ''),
    domains: normalizeList(f.domains),
    publicInfo: f.publicInfo,
    uiRedirect: f.uiRedirect,
    metricsPublic: f.metricsPublic,
    cors: {
      ...(base?.cors ?? {}),
      allowedOrigins: normalizeList(f.cors.allowedOrigins),
      allowedHeaders: normalizeList(f.cors.allowedHeaders),
      exposeHeaders: normalizeList(f.cors.exposeHeaders),
      maxAgeSeconds: Math.round(f.cors.maxAgeSeconds)
    },
    discovery: {
      ...(base?.discovery ?? {}),
      enabled: f.discovery.enabled,
      advertise: f.discovery.advertise,
      mdns: f.discovery.mdns,
      beacon: f.discovery.beacon,
      beaconGroup: f.discovery.beaconGroup.trim(),
      beaconInterval: secondsToNs(f.discovery.beaconIntervalSeconds),
      seeds: normalizeList(f.discovery.seeds),
      dnsSrv: normalizeList(f.discovery.dnsSrv),
      peerTtl: secondsToNs(f.discovery.peerTtlSeconds),
      allowScan: f.discovery.allowScan,
      maxScanHosts: Math.round(f.discovery.maxScanHosts)
    },
    logging: {
      ...(base?.logging ?? {}),
      level: f.logging.level,
      format: f.logging.format,
      accessLog: f.logging.accessLog
    },
    lifecycle: {
      ...(base?.lifecycle ?? {}),
      enabled: f.lifecycle.enabled,
      intervalSeconds: Math.round(f.lifecycle.intervalSeconds)
    },
    ...(f.compression
      ? {
          compression: {
            ...(base?.compression ?? {}),
            mode: f.compression.mode,
            level: Math.round(f.compression.level),
            minSize: Math.round(f.compression.minSize),
            skipContentTypes: normalizeList(f.compression.skipContentTypes)
          }
        }
      : {})
  }
}

/** Canonical JSON of a form (normalised lists) for dirty checks. */
export function formSignature(f: SettingsForm): string {
  return JSON.stringify(formToSettings(f))
}

/** True when two forms would produce different settings documents. */
export function isFormDirty(current: SettingsForm, original: SettingsForm | null | undefined): boolean {
  if (!original) return false
  return formSignature(current) !== formSignature(original)
}

// Limits mirror the server's validator (internal/config ValidateSettings),
// so that a form that passes here is not rejected with a 400 on save.
export const SETTINGS_LIMITS = {
  maxNameLength: 128,
  maxListEntries: 256,
  maxDnsSrv: 64,
  maxCorsMaxAgeSeconds: 7 * 24 * 3600,
  minBeaconIntervalSeconds: 1,
  maxBeaconIntervalSeconds: 3600,
  minPeerTtlSeconds: 1,
  maxPeerTtlSeconds: 7 * 24 * 3600,
  maxScanHosts: 65536,
  minLifecycleSeconds: 60,
  maxLifecycleSeconds: 365 * 24 * 3600,
  minCompressionLevel: 1,
  maxCompressionLevel: 9,
  /** 5 TiB, the S3 object size limit. */
  maxCompressionMinSize: 5 * 1024 ** 4
} as const

const REGION_RE = /^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$/
// DNS name: dot-separated labels of letters, digits, '-' and '_' (no
// leading/trailing '-'); a trailing dot only where noted.
const LABEL_RE = /^(?!-)[a-z0-9_-]{1,63}(?<!-)$/i
const HOST_PORT_RE = /^([^\s:/]+|\[[0-9a-f:.]+\]):(\d{1,5})$/i
// RFC 7230 header field token (also allows '*' patterns for allowed headers).
const HEADER_TOKEN_RE = /^[!#$%&'*+.^_`|~0-9a-z-]+$/i
// compression.skipContentTypes pattern (internal/config mediaTypePatternRe):
// "type/subtype", "type/*" or a prefix ending in "*".
const MEDIA_TYPE_PATTERN_RE = /^[a-z0-9][a-z0-9!#$&^_.+-]*\/(\*|[a-z0-9][a-z0-9!#$&^_.+-]*\*?)$/i

function isHostname(s: string): boolean {
  return s.length >= 1 && s.length <= 253 && s.split('.').every(l => LABEL_RE.test(l))
}

/** Absolute http(s) URL without credentials, query or fragment (and no path unless allowPath). */
function baseUrlError(s: string, allowPath: boolean): string | undefined {
  let u: URL
  try {
    u = new URL(s)
  } catch {
    return `"${s}" is not a valid URL`
  }
  if ((u.protocol !== 'http:' && u.protocol !== 'https:') || !u.hostname) return `"${s}" must be an http:// or https:// URL`
  if (u.username || u.password || u.search || u.hash || s.includes('?') || s.includes('#')) return `"${s}" must not contain credentials, a query or a fragment`
  if (!allowPath && u.pathname !== '/' && u.pathname !== '') return `"${s}" must not contain a path`
  return undefined
}

function validPort(p: string | number): boolean {
  const n = Number(p)
  return Number.isInteger(n) && n >= 1 && n <= 65535
}

/** Virtual-host base domain: a DNS name without scheme, port or path (e.g. s3.example.com). */
export function validateDomain(d: string): string | undefined {
  if (d.includes(':') || d.includes('/')) return `"${d}" must be a DNS name without scheme, port or path`
  if (!isHostname(d)) return `"${d}" is not a valid domain`
  return undefined
}

/** CORS origin: "*", or scheme://host[:port] where host may contain one "*". */
export function validateOrigin(o: string): string | undefined {
  if (o === '*') return undefined
  const m = /^([a-z][a-z0-9+.-]*):\/\/([^/\s?#@]+)$/i.exec(o)
  if (!m || (m[2] ?? '').split('*').length > 2) return `"${o}" is not a valid origin (use "*" or e.g. https://app.example.com)`
  return undefined
}

/** Allowed request header: a header name or a "*" pattern (e.g. x-amz-*). */
export function validateAllowedHeader(h: string): string | undefined {
  return HEADER_TOKEN_RE.test(h) ? undefined : `"${h}" is not a valid header name or pattern`
}

/** Exposed response header: a header name, or a lone "*" (every header). */
export function validateExposeHeader(h: string): string | undefined {
  if (h === '*') return undefined
  if (!HEADER_TOKEN_RE.test(h) || h.includes('*')) return `"${h}" is not a valid header name (only a lone "*" wildcard is allowed)`
  return undefined
}

/** UDP multicast group "a.b.c.d:port" in 224.0.0.0/4. */
export function validateBeaconGroup(g: string): string | undefined {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3}):(\d{1,5})$/.exec(g)
  if (!m) return 'Use an IPv4 multicast address and port, e.g. 239.255.73.83:7373'
  const octets = m.slice(1, 5).map(Number)
  if (octets.some(o => o > 255)) return 'Invalid IPv4 address'
  if (octets[0]! < 224 || octets[0]! > 239) return 'The address must be a multicast address (224.0.0.0 – 239.255.255.255)'
  if (!validPort(m[5]!)) return 'Invalid port'
  return undefined
}

/** Media type pattern stored uncompressed: "image/jpeg", "video/*" or "application/vnd.oasis.opendocument.*". */
export function validateMediaTypePattern(t: string): string | undefined {
  return MEDIA_TYPE_PATTERN_RE.test(t) ? undefined : `"${t}" is not a media type pattern (use type/subtype, type/* or a prefix ending in *)`
}

/** Static seed: a base URL without path (http[s]://host:port) or host[:port]. */
export function validateSeed(s: string): string | undefined {
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(s)) return baseUrlError(s, false)
  const m = HOST_PORT_RE.exec(s)
  if (m && validPort(m[2]!)) return undefined
  if (isHostname(s)) return undefined
  return `"${s}" is not a valid seed (use http://host:9000 or host:9000)`
}

/** DNS SRV domain, e.g. example.com (queried as _gos3._tcp.<domain>); a trailing dot is fine. */
export function validateSrvName(s: string): string | undefined {
  return isHostname(s.replace(/\.$/, '')) ? undefined : `"${s}" is not a valid DNS name`
}

function firstListError(items: readonly string[], fn: (s: string) => string | undefined, max: number = SETTINGS_LIMITS.maxListEntries): string | undefined {
  const list = normalizeList(items)
  if (list.length > max) return `At most ${max} entries`
  for (const it of list) {
    const e = fn(it)
    if (e) return e
  }
  return undefined
}

function inRange(n: number, min: number, max: number): boolean {
  return Number.isFinite(n) && n >= min && n <= max
}

/** Client-side validation; returns field path → message (empty = valid). */
export function validateSettingsForm(f: SettingsForm): SettingsErrors {
  const L = SETTINGS_LIMITS
  const e: SettingsErrors = {}
  const set = (k: string, msg: string | undefined) => {
    if (msg && !e[k]) e[k] = msg
  }

  if (!f.name.trim()) set('name', 'Name is required')
  else if (f.name.trim().length > L.maxNameLength) set('name', `At most ${L.maxNameLength} characters`)
  // eslint-disable-next-line no-control-regex
  else if (/[\u0000-\u001f\u007f]/.test(f.name)) set('name', 'Control characters are not allowed')
  if (!REGION_RE.test(f.region.trim())) set('region', 'Use lowercase letters, digits and dashes, e.g. us-east-1')
  if (f.publicUrl.trim()) set('publicUrl', baseUrlError(f.publicUrl.trim().replace(/\/+$/, ''), true))
  set('domains', firstListError(f.domains, validateDomain))

  set('cors.allowedOrigins', firstListError(f.cors.allowedOrigins, validateOrigin))
  set('cors.allowedHeaders', firstListError(f.cors.allowedHeaders, validateAllowedHeader))
  set('cors.exposeHeaders', firstListError(f.cors.exposeHeaders, validateExposeHeader))
  if (!Number.isInteger(f.cors.maxAgeSeconds) || !inRange(f.cors.maxAgeSeconds, 0, L.maxCorsMaxAgeSeconds)) {
    set('cors.maxAgeSeconds', `Must be a whole number of seconds between 0 and ${L.maxCorsMaxAgeSeconds} (7 days)`)
  }

  const d = f.discovery
  // The server validates the group even while the beacon is off.
  set('discovery.beaconGroup', validateBeaconGroup(d.beaconGroup.trim()))
  if (!inRange(d.beaconIntervalSeconds, L.minBeaconIntervalSeconds, L.maxBeaconIntervalSeconds)) {
    set('discovery.beaconIntervalSeconds', `Must be between ${L.minBeaconIntervalSeconds} and ${L.maxBeaconIntervalSeconds} seconds`)
  }
  if (!inRange(d.peerTtlSeconds, L.minPeerTtlSeconds, L.maxPeerTtlSeconds)) {
    set('discovery.peerTtlSeconds', `Must be between ${L.minPeerTtlSeconds} second and ${L.maxPeerTtlSeconds} seconds (7 days)`)
  } else if (d.beacon && d.peerTtlSeconds < d.beaconIntervalSeconds * 2) {
    set('discovery.peerTtlSeconds', 'Should be at least twice the beacon interval, otherwise peers flap')
  }
  if (!Number.isInteger(d.maxScanHosts) || !inRange(d.maxScanHosts, 1, L.maxScanHosts)) {
    set('discovery.maxScanHosts', `Must be a whole number between 1 and ${L.maxScanHosts}`)
  }
  set('discovery.seeds', firstListError(d.seeds, validateSeed))
  set('discovery.dnsSrv', firstListError(d.dnsSrv, validateSrvName, L.maxDnsSrv))

  if (!(LOG_LEVELS as readonly string[]).includes(f.logging.level)) set('logging.level', 'Unknown log level')
  if (!(LOG_FORMATS as readonly string[]).includes(f.logging.format)) set('logging.format', 'Unknown log format')

  if (!Number.isInteger(f.lifecycle.intervalSeconds) || !inRange(f.lifecycle.intervalSeconds, L.minLifecycleSeconds, L.maxLifecycleSeconds)) {
    set('lifecycle.intervalSeconds', `Must be a whole number of seconds between ${L.minLifecycleSeconds} and ${L.maxLifecycleSeconds} (1 year)`)
  }

  const c = f.compression
  if (c) {
    if (!(COMPRESSION_MODES as readonly string[]).includes(c.mode)) set('compression.mode', 'Unknown compression mode')
    if (!Number.isInteger(c.level) || !inRange(c.level, L.minCompressionLevel, L.maxCompressionLevel)) {
      set('compression.level', `Must be a whole number between ${L.minCompressionLevel} and ${L.maxCompressionLevel}`)
    }
    if (!Number.isInteger(c.minSize) || !inRange(c.minSize, 0, L.maxCompressionMinSize)) {
      set('compression.minSize', `Must be a whole number of bytes between 0 and ${L.maxCompressionMinSize} (5 TiB)`)
    }
    set('compression.skipContentTypes', firstListError(c.skipContentTypes, validateMediaTypePattern))
  }
  return e
}

/** A section of the server settings page (one chip each). */
export type SettingsSection = 'general' | 'cors' | 'discovery' | 'logging' | 'lifecycle' | 'compression'

export interface SettingsSectionDef {
  value: SettingsSection
  label: string
  icon: string
}

/** Settings page sections in display order (see settingsSections). */
export const SETTINGS_SECTIONS: readonly SettingsSectionDef[] = [
  { value: 'general', label: 'General', icon: 'i-lucide-server' },
  { value: 'cors', label: 'CORS', icon: 'i-lucide-globe' },
  { value: 'discovery', label: 'Discovery', icon: 'i-lucide-radar' },
  { value: 'logging', label: 'Logging', icon: 'i-lucide-scroll-text' },
  { value: 'lifecycle', label: 'Lifecycle worker', icon: 'i-lucide-timer' },
  { value: 'compression', label: 'Compression', icon: 'i-lucide-file-archive' }
]

/** The sections a form has: compression only when the server reported it. */
export function settingsSections(f: SettingsForm | null | undefined): readonly SettingsSectionDef[] {
  return SETTINGS_SECTIONS.filter(s => s.value !== 'compression' || !!f?.compression)
}

/** Normalises a `?tab=` query value to one of `sections` (default general). */
export function resolveSettingsSection(v: unknown, sections: readonly SettingsSectionDef[] = SETTINGS_SECTIONS): SettingsSection {
  const s = Array.isArray(v) ? v[0] : v
  return sections.find(t => t.value === s)?.value ?? 'general'
}

/** Which form section a field path belongs to (for error badges on section chips). */
export function sectionOf(path: string): SettingsSection {
  const head = path.split('.')[0]
  if (head === 'cors' || head === 'discovery' || head === 'logging' || head === 'lifecycle' || head === 'compression') return head
  return 'general'
}

/**
 * The sections whose saved document would change, compared like
 * isFormDirty (normalised lists), so the chips can mark unsaved sections.
 */
export function dirtySections(current: SettingsForm, original: SettingsForm | null | undefined): Set<SettingsSection> {
  const out = new Set<SettingsSection>()
  if (!original) return out
  const a = formToSettings(current) as unknown as Record<string, unknown>
  const b = formToSettings(original) as unknown as Record<string, unknown>
  for (const k of new Set([...Object.keys(a), ...Object.keys(b)])) {
    if (JSON.stringify(a[k]) !== JSON.stringify(b[k])) out.add(sectionOf(k))
  }
  return out
}
