// mDNS / DNS-SD answer parsing for `_gos3._tcp.local` (docs/discovery.md §2).
// Pure functions over the record shape produced by `multicast-dns`
// (dns-packet); the socket handling lives in ./local.ts.
import { isIP } from 'node:net'
import { baseUrl } from './targets'

export const MDNS_SERVICE = '_gos3._tcp.local'

/** Minimal structural view of a dns-packet answer. */
export interface MdnsRecord {
  name: string
  type: string
  data?: unknown
  ttl?: number
}

export interface MdnsCandidate {
  /** Instance name, e.g. "nas-gos3-b3f1c2._gos3._tcp.local". */
  instance: string
  /** SRV target host, e.g. "nas.local". */
  host: string
  port: number
  scheme: 'http' | 'https'
  /** Decoded TXT key/values (id, name, v, api, region, scheme, info). */
  txt: Record<string, string>
  /** A/AAAA addresses of the target (IPv4 first). */
  addresses: string[]
  /** Candidate base URLs: one per address, else the target host name. */
  urls: string[]
}

/** Lower-case and strip the trailing dot of a DNS name. */
export function dnsName(name: string): string {
  return name.toLowerCase().replace(/\.$/, '')
}

function bufToString(v: unknown): string | undefined {
  if (typeof v === 'string') return v
  if (v instanceof Uint8Array) return new TextDecoder().decode(v)
  return undefined
}

/** Decodes TXT record data (string | Buffer | Array<string | Buffer>) into key/values. */
export function decodeTxt(data: unknown): Record<string, string> {
  const out: Record<string, string> = {}
  const items = Array.isArray(data) ? data : data === undefined || data === null ? [] : [data]
  for (const item of items) {
    const s = bufToString(item)
    if (!s) continue
    const eq = s.indexOf('=')
    const key = (eq < 0 ? s : s.slice(0, eq)).trim().toLowerCase()
    if (!key || key in out) continue // RFC 6763 §6.4: first occurrence wins
    out[key] = eq < 0 ? '' : s.slice(eq + 1)
  }
  return out
}

interface SrvData {
  target: string
  port: number
}

function isSrvData(v: unknown): v is SrvData {
  return typeof v === 'object' && v !== null && typeof (v as SrvData).target === 'string' && typeof (v as SrvData).port === 'number'
}

/** True for instance names of our service type. */
export function isGos3Instance(name: string): boolean {
  return dnsName(name).endsWith(`.${MDNS_SERVICE}`)
}

/**
 * Builds candidates from all records collected during a browse window
 * (answers + additionals of every response). Instances come from PTR
 * records of the service type and from SRV/TXT records named like an
 * instance; each needs an SRV record to become a candidate.
 */
export function parseMdnsRecords(records: readonly MdnsRecord[]): MdnsCandidate[] {
  const instances = new Set<string>()
  const srv = new Map<string, SrvData>()
  const txt = new Map<string, Record<string, string>>()
  const addrs = new Map<string, string[]>()

  for (const r of records) {
    if (!r || typeof r.name !== 'string') continue
    const name = dnsName(r.name)
    switch (r.type) {
      case 'PTR':
        if (name === MDNS_SERVICE && typeof r.data === 'string' && isGos3Instance(r.data)) instances.add(dnsName(r.data))
        break
      case 'SRV':
        if (isSrvData(r.data) && isGos3Instance(name)) {
          instances.add(name)
          srv.set(name, { target: dnsName(r.data.target), port: r.data.port })
        }
        break
      case 'TXT':
        if (isGos3Instance(name)) {
          instances.add(name)
          txt.set(name, { ...decodeTxt(r.data), ...txt.get(name) })
        }
        break
      case 'A':
      case 'AAAA':
        if (typeof r.data === 'string' && isIP(r.data)) {
          const list = addrs.get(name) ?? []
          if (!list.includes(r.data)) list.push(r.data)
          addrs.set(name, list)
        }
        break
    }
  }

  const out: MdnsCandidate[] = []
  for (const instance of instances) {
    const s = srv.get(instance)
    if (!s || s.port < 1 || s.port > 65535) continue
    const t = txt.get(instance) ?? {}
    const scheme = t.scheme?.toLowerCase() === 'https' ? 'https' : 'http'
    const all = addrs.get(s.target) ?? []
    // IPv4 first; IPv6 link-local addresses need a zone id and are not usable in URLs.
    const v4 = all.filter(a => isIP(a) === 4)
    const v6 = all.filter(a => isIP(a) === 6 && !/^fe80:/i.test(a))
    const addresses = [...v4, ...v6]
    const urls = addresses.length ? addresses.map(a => baseUrl(scheme, a, s.port)) : s.target ? [baseUrl(scheme, s.target, s.port)] : []
    out.push({ instance, host: s.target, port: s.port, scheme, txt: t, addresses, urls })
  }
  return out
}

/**
 * Names still missing to resolve the instances seen so far: instances
 * without SRV/TXT and SRV targets without addresses (asked in follow-up queries).
 */
export function missingMdnsNames(records: readonly MdnsRecord[]): { srv: string[], a: string[] } {
  const instances = new Set<string>()
  const haveSrv = new Set<string>()
  const targets = new Set<string>()
  const haveAddr = new Set<string>()
  for (const r of records) {
    if (!r || typeof r.name !== 'string') continue
    const name = dnsName(r.name)
    if (r.type === 'PTR' && name === MDNS_SERVICE && typeof r.data === 'string' && isGos3Instance(r.data)) instances.add(dnsName(r.data))
    if (r.type === 'SRV' && isSrvData(r.data)) {
      haveSrv.add(name)
      targets.add(dnsName(r.data.target))
    }
    if ((r.type === 'A' || r.type === 'AAAA') && typeof r.data === 'string') haveAddr.add(name)
  }
  return {
    srv: [...instances].filter(i => !haveSrv.has(i)),
    a: [...targets].filter(t => t && !haveAddr.has(t))
  }
}
