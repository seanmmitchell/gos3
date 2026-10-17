// Which addresses belong to this host or to one of its directly connected
// subnets (like internal/discovery/netlocal.go). Beacon and mDNS datagrams
// are link-local by design (multicast TTL 1 / 255), and nothing in them is
// authenticated: local discovery only accepts datagrams from on-link
// senders, and an announced address only when it is the sender's own
// (or, for a sender on this host, a loopback or local address).
import { isIP } from 'node:net'
import { networkInterfaces, type NetworkInterfaceInfo } from 'node:os'
import { ipv6Bytes } from './netguard'

/** How long the interface list is cached. */
const LOCAL_NETS_TTL_MS = 10_000

/** An address as 16 bytes (IPv4 as ::ffff:a.b.c.d), or undefined. */
function ipBytes(ip: string): Uint8Array | undefined {
  const s = ip.trim().replace(/^\[(.*)\]$/, '$1').split('%')[0]!
  const family = isIP(s)
  if (family === 4) {
    const out = new Uint8Array(16)
    out[10] = 0xff
    out[11] = 0xff
    s.split('.').forEach((p, i) => {
      out[12 + i] = Number(p)
    })
    return out
  }
  return family === 6 ? ipv6Bytes(s) : undefined
}

function isV4(b: Uint8Array): boolean {
  return b.slice(0, 10).every(x => x === 0) && b[10] === 0xff && b[11] === 0xff
}

/**
 * Canonical text form of an IP literal (brackets and zone dropped,
 * IPv4-mapped IPv6 unmapped, IPv6 compressed), or undefined when ip is
 * not one: "10.0.0.5", "fd00::5". Two spellings of one address compare
 * equal.
 */
export function canonicalIp(ip: string): string | undefined {
  const b = ipBytes(ip)
  if (!b) return undefined
  if (isV4(b)) return `${b[12]}.${b[13]}.${b[14]}.${b[15]}`
  const full = Array.from({ length: 8 }, (_, i) => ((b[i * 2]! << 8) | b[i * 2 + 1]!).toString(16)).join(':')
  // The WHATWG URL serialiser yields the canonical compressed form.
  return new URL(`http://[${full}]/`).hostname.slice(1, -1)
}

function isLoopbackBytes(b: Uint8Array): boolean {
  if (isV4(b)) return b[12] === 127
  return b.slice(0, 15).every(x => x === 0) && b[15] === 1
}

/** True for 127.0.0.0/8, ::1 and their IPv4-mapped forms. */
export function isLoopbackIp(ip: string): boolean {
  const b = ipBytes(ip)
  return !!b && isLoopbackBytes(b)
}

interface Prefix {
  addr: Uint8Array
  /** Prefix length in bits of the 16-byte form. */
  bits: number
}

function contains(p: Prefix, b: Uint8Array): boolean {
  for (let bit = 0; bit < p.bits; bit += 8) {
    const n = Math.min(8, p.bits - bit)
    const mask = (0xff << (8 - n)) & 0xff
    if ((p.addr[bit / 8]! & mask) !== (b[bit / 8]! & mask)) return false
  }
  return true
}

function prefixesOf(list: NodeJS.Dict<NetworkInterfaceInfo[]>): Prefix[] {
  const out: Prefix[] = []
  for (const infos of Object.values(list)) {
    for (const a of infos ?? []) {
      const addr = ipBytes(a.address)
      if (!addr) continue
      const len = Number((a.cidr ?? '').split('/')[1] ?? (a.family === 'IPv4' ? 32 : 128))
      if (!Number.isInteger(len)) continue
      out.push({ addr, bits: isV4(addr) ? 96 + Math.min(len, 32) : Math.min(len, 128) })
    }
  }
  return out
}

/** Answers for this host's interfaces (cached for 10 s). */
export class LocalNets {
  private loadedAt = 0
  private prefixes: Prefix[] = []

  /** load: the interface list (default os.networkInterfaces). */
  constructor(private readonly load: () => NodeJS.Dict<NetworkInterfaceInfo[]> = networkInterfaces, private readonly now: () => number = Date.now) {}

  private get(): Prefix[] {
    const t = this.now()
    if (!this.loadedAt || t - this.loadedAt > LOCAL_NETS_TTL_MS) {
      try {
        this.prefixes = prefixesOf(this.load())
      } catch {
        this.prefixes = []
      }
      this.loadedAt = t
    }
    return this.prefixes
  }

  /** ip is a loopback address or one of this host's interface addresses. */
  isLocal(ip: string): boolean {
    const b = ipBytes(ip)
    if (!b) return false
    if (isLoopbackBytes(b)) return true
    return this.get().some(p => p.addr.every((x, i) => x === b[i]))
  }

  /**
   * ip is this host or inside a directly connected subnet, and is neither
   * unspecified, multicast nor a broadcast address. When the interfaces
   * cannot be listed only loopback is accepted (fail closed).
   */
  onLink(ip: string): boolean {
    const b = ipBytes(ip)
    if (!b) return false
    if (isV4(b) ? (b[12] === 0 || b[12]! >= 224) : (b.every(x => x === 0) || b[0] === 0xff)) return false
    if (isLoopbackBytes(b)) return true
    const prefixes = this.get()
    if (!prefixes.length) return false
    if (isV4(b) && prefixes.some(p => isV4(p.addr) && p.bits <= 126 && contains(p, b) && b.slice(12).every((x, i) => {
      // directed broadcast: every host bit set
      const bit = 96 + i * 8
      const hostBits = Math.max(0, Math.min(8, bit + 8 - p.bits))
      const mask = hostBits ? (1 << hostBits) - 1 : 0
      return (x & mask) === mask
    }))) return false
    return prefixes.some(p => contains(p, b))
  }

  /**
   * A host at src may advertise ip as its own address: ip is src itself
   * or, when src is this host, a loopback or local address. Unauthenticated
   * announcements therefore only ever make discovery probe the sender.
   */
  advertisedBy(ip: string, src: string): boolean {
    const a = canonicalIp(ip)
    const s = canonicalIp(src)
    if (!a || !s) return false
    if (a === s) return true
    return this.isLocal(s) && (isLoopbackIp(a) || this.isLocal(a))
  }
}

/** The shared cache of this host's interfaces. */
export const hostNets = new LocalNets()
