// Form model for config.Settings (GET/PUT /config).
//
// The admin API speaks nanoseconds (Go time.Duration) and nullable lists;
// the settings form edits seconds and plain arrays. These helpers convert
// both ways, normalise list editors and validate the form before saving.
// The server still validates (400 InvalidSettings) and its message is shown.
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { Settings } from '~/types/admin'
import { nsToSeconds, secondsToNs } from '~/utils/format'

export const LOG_LEVELS = ['debug', 'info', 'warn', 'error'] as const
export const LOG_FORMATS = ['text', 'json'] as const

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
    lifecycle: { enabled: false, intervalSeconds: 0 }
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
    }
  }
}

/**
 * Form → full settings document for PUT /config. Fields of `base` (the
 * settings last loaded from the server) that the form does not know are
 * preserved, so a newer server never loses settings through this UI.
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
    }
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

const REGION_RE = /^[a-z0-9][a-z0-9-]{0,62}$/
const HOST_RE = /^(?=.{1,253}$)[a-z0-9_](?:[a-z0-9_-]{0,62})(?:\.[a-z0-9_](?:[a-z0-9_-]{0,62}))*\.?$/i
const HOST_PORT_RE = /^([^\s:/]+|\[[0-9a-f:.]+\]):(\d{1,5})$/i

function isHttpUrl(s: string): boolean {
  try {
    const u = new URL(s)
    return (u.protocol === 'http:' || u.protocol === 'https:') && !!u.host
  } catch {
    return false
  }
}

function validPort(p: string | number): boolean {
  const n = Number(p)
  return Number.isInteger(n) && n >= 1 && n <= 65535
}

/** "host" or "host:port" (no scheme), e.g. a virtual-hosted-style base domain. */
export function validateDomain(d: string): string | undefined {
  const [host, port, ...rest] = d.split(':')
  if (rest.length || !host || !HOST_RE.test(host)) return `"${d}" is not a valid domain`
  if (port !== undefined && !validPort(port)) return `"${d}" has an invalid port`
  return undefined
}

/** CORS origin: "*", or scheme://host[:port] where host may contain one "*". */
export function validateOrigin(o: string): string | undefined {
  if (o === '*') return undefined
  const m = /^([a-z][a-z0-9+.-]*):\/\/([^/\s]+)$/i.exec(o)
  if (!m || (m[2] ?? '').split('*').length > 2) return `"${o}" is not a valid origin (use "*" or e.g. https://app.example.com)`
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

/** Static seed: a base URL (http[s]://host:port) or host:port. */
export function validateSeed(s: string): string | undefined {
  if (/^https?:\/\//i.test(s)) return isHttpUrl(s) ? undefined : `"${s}" is not a valid URL`
  const m = HOST_PORT_RE.exec(s)
  if (m && validPort(m[2]!)) return undefined
  if (HOST_RE.test(s)) return undefined
  return `"${s}" is not a valid seed (use http://host:9000 or host:9000)`
}

/** DNS SRV name, e.g. _gos3._tcp.example.com. */
export function validateSrvName(s: string): string | undefined {
  return HOST_RE.test(s) ? undefined : `"${s}" is not a valid DNS name`
}

function firstListError(items: readonly string[], fn: (s: string) => string | undefined): string | undefined {
  for (const it of normalizeList(items)) {
    const e = fn(it)
    if (e) return e
  }
  return undefined
}

function isNonNegInt(n: number): boolean {
  return Number.isInteger(n) && n >= 0
}

/** Client-side validation; returns field path → message (empty = valid). */
export function validateSettingsForm(f: SettingsForm): SettingsErrors {
  const e: SettingsErrors = {}
  const set = (k: string, msg: string | undefined) => {
    if (msg && !e[k]) e[k] = msg
  }

  if (!f.name.trim()) set('name', 'Name is required')
  else if (f.name.trim().length > 128) set('name', 'At most 128 characters')
  if (!REGION_RE.test(f.region.trim())) set('region', 'Use lowercase letters, digits and dashes, e.g. us-east-1')
  if (f.publicUrl.trim() && !isHttpUrl(f.publicUrl.trim())) set('publicUrl', 'Must be an http:// or https:// URL')
  set('domains', firstListError(f.domains, validateDomain))

  set('cors.allowedOrigins', firstListError(f.cors.allowedOrigins, validateOrigin))
  set('cors.allowedHeaders', firstListError(f.cors.allowedHeaders, h => (/^[\w*-]+$/.test(h) ? undefined : `"${h}" is not a valid header name`)))
  set('cors.exposeHeaders', firstListError(f.cors.exposeHeaders, h => (/^[\w-]+$/.test(h) ? undefined : `"${h}" is not a valid header name`)))
  if (!isNonNegInt(f.cors.maxAgeSeconds)) set('cors.maxAgeSeconds', 'Must be a whole number of seconds ≥ 0')

  const d = f.discovery
  if (d.beacon || d.beaconGroup.trim()) set('discovery.beaconGroup', validateBeaconGroup(d.beaconGroup.trim()))
  if (!Number.isFinite(d.beaconIntervalSeconds) || d.beaconIntervalSeconds < 1) set('discovery.beaconIntervalSeconds', 'Must be at least 1 second')
  if (!Number.isFinite(d.peerTtlSeconds) || d.peerTtlSeconds < 1) set('discovery.peerTtlSeconds', 'Must be at least 1 second')
  else if (d.beacon && d.peerTtlSeconds < d.beaconIntervalSeconds * 2) {
    set('discovery.peerTtlSeconds', 'Should be at least twice the beacon interval, otherwise peers flap')
  }
  if (!Number.isInteger(d.maxScanHosts) || d.maxScanHosts < 1) set('discovery.maxScanHosts', 'Must be a whole number ≥ 1')
  set('discovery.seeds', firstListError(d.seeds, validateSeed))
  set('discovery.dnsSrv', firstListError(d.dnsSrv, validateSrvName))

  if (!(LOG_LEVELS as readonly string[]).includes(f.logging.level)) set('logging.level', 'Unknown log level')
  if (!(LOG_FORMATS as readonly string[]).includes(f.logging.format)) set('logging.format', 'Unknown log format')

  if (!Number.isInteger(f.lifecycle.intervalSeconds) || f.lifecycle.intervalSeconds < 1) {
    set('lifecycle.intervalSeconds', 'Must be a whole number of seconds ≥ 1')
  }
  return e
}

/** Which form section a field path belongs to (for error badges on section headers). */
export function sectionOf(path: string): 'general' | 'cors' | 'discovery' | 'logging' | 'lifecycle' {
  const head = path.split('.')[0]
  if (head === 'cors' || head === 'discovery' || head === 'logging' || head === 'lifecycle') return head
  return 'general'
}
