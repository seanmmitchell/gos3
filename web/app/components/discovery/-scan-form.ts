// Model and validation of the network scan form (ScanForm.vue), shared by the
// per-server scan (admin API POST /discovery/scan) and the UI host's local
// scan (POST /api/discovery/scan). Plain TS, unit-tested.
import type { ScanRequest } from '~/types/admin'

export interface ScanFormModel {
  /** One target per line (or comma separated): CIDR, host[:port] or URL. */
  targets: string
  /** Comma/space separated ports. */
  ports: string
  http: boolean
  https: boolean
  timeoutSeconds: number
  concurrency: number
}

export interface ScanFormErrors {
  targets?: string
  ports?: string
  schemes?: string
  timeoutSeconds?: string
  concurrency?: string
}

export const DEFAULT_SCAN_HOST_LIMIT = 1024

export function defaultScanForm(): ScanFormModel {
  return { targets: '', ports: '9000', http: true, https: false, timeoutSeconds: 1.5, concurrency: 64 }
}

export function splitList(text: string): string[] {
  return text.split(/[\s,;]+/).map(s => s.trim()).filter(Boolean)
}

export function parsePorts(text: string): { ports: number[], error?: string } {
  const ports: number[] = []
  for (const raw of splitList(text)) {
    if (!/^\d{1,5}$/.test(raw)) return { ports, error: `"${raw}" is not a port number` }
    const n = Number(raw)
    if (n < 1 || n > 65535) return { ports, error: `${n} is out of range (1–65535)` }
    if (!ports.includes(n)) ports.push(n)
  }
  return { ports }
}

/** Approximate number of hosts targets expand to (IPv4 CIDRs; others count 1). */
export function estimateHosts(targets: readonly string[]): number {
  let n = 0
  for (const t of targets) {
    const m = /^\d{1,3}(?:\.\d{1,3}){3}\/(\d{1,2})$/.exec(t)
    if (m) {
      const bits = Number(m[1])
      if (bits > 32) continue
      const size = 2 ** (32 - bits)
      n += bits <= 30 ? size - 2 : size
    } else {
      n++
    }
  }
  return n
}

/** Light client-side validation of one target (the server validates for real). */
export function targetError(t: string): string | undefined {
  if (t.includes('://')) {
    try {
      const u = new URL(t)
      if (u.protocol !== 'http:' && u.protocol !== 'https:') return `${t}: only http and https URLs`
      return undefined
    } catch {
      return `${t}: invalid URL`
    }
  }
  if (t.includes('/')) {
    const m = /^(\d{1,3}(?:\.\d{1,3}){3})\/(\d{1,2})$/.exec(t)
    if (!m || Number(m[2]) > 32 || m[1]!.split('.').some(o => Number(o) > 255)) return `${t}: invalid IPv4 CIDR`
    return undefined
  }
  if (/^\[[0-9a-f:.]+\](?::\d{1,5})?$/i.test(t) || /^[0-9a-f:]+$/i.test(t)) return undefined // IPv6 literal
  if (!/^[A-Za-z0-9._-]+(?::\d{1,5})?$/.test(t)) return `${t}: not a host, CIDR or URL`
  return undefined
}

export function scanFormErrors(m: ScanFormModel, opts: { maxHosts?: number } = {}): ScanFormErrors {
  const errors: ScanFormErrors = {}
  const targets = splitList(m.targets)
  const maxHosts = opts.maxHosts ?? DEFAULT_SCAN_HOST_LIMIT
  if (!targets.length) errors.targets = 'Enter at least one target'
  else {
    const bad = targets.map(targetError).find(Boolean)
    if (bad) errors.targets = bad
    else if (estimateHosts(targets) > maxHosts) errors.targets = `The targets expand to ${estimateHosts(targets)} hosts; the limit is ${maxHosts}`
  }
  const p = parsePorts(m.ports)
  if (p.error) errors.ports = p.error
  if (!m.http && !m.https) errors.schemes = 'Select at least one scheme'
  if (!Number.isFinite(m.timeoutSeconds) || m.timeoutSeconds < 0.01 || m.timeoutSeconds > 30) errors.timeoutSeconds = 'Between 0.01 and 30 seconds'
  if (!Number.isInteger(m.concurrency) || m.concurrency < 1 || m.concurrency > 512) errors.concurrency = 'Between 1 and 512'
  return errors
}

export function toScanRequest(m: ScanFormModel): ScanRequest {
  const ports = parsePorts(m.ports).ports
  const schemes = [m.http ? 'http' : '', m.https ? 'https' : ''].filter(Boolean)
  return {
    targets: splitList(m.targets),
    ports: ports.length ? ports : undefined,
    schemes,
    timeout: Math.round(m.timeoutSeconds * 1e9),
    concurrency: m.concurrency
  }
}

/** Number of probe URLs (hosts × ports × schemes; URL targets count once). */
export function estimateProbes(m: ScanFormModel): number {
  const targets = splitList(m.targets)
  const ports = Math.max(1, parsePorts(m.ports).ports.length)
  const schemes = Math.max(1, Number(m.http) + Number(m.https))
  let n = 0
  for (const t of targets) {
    if (t.includes('://')) n += 1
    else if (/:\d{1,5}$/.test(t) && !/^[0-9a-f:]+$/i.test(t)) n += schemes
    else n += estimateHosts([t]) * ports * schemes
  }
  return n
}

/** A /24 around the first IPv4 host of the given URLs (scan placeholder). */
export function suggestCidr(urls: readonly (string | undefined)[]): string | undefined {
  for (const u of urls) {
    if (!u) continue
    try {
      const host = new URL(u).hostname
      const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.\d{1,3}$/.exec(host)
      if (m && m[1] !== '127') return `${m[1]}.${m[2]}.${m[3]}.0/24`
    } catch {
      // skip
    }
  }
  return undefined
}
