// Formatting helpers (auto-imported by Nuxt from app/utils/).

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB']

/** Human readable binary size: 1536 → "1.5 KiB". */
export function formatBytes(bytes: number | null | undefined, decimals = 1): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes)) return '—'
  if (bytes < 0) return `-${formatBytes(-bytes, decimals)}`
  if (bytes < 1024) return `${bytes} B`
  let i = 0
  let v = bytes
  while (v >= 1024 && i < BYTE_UNITS.length - 1) {
    v /= 1024
    i++
  }
  const s = v.toFixed(v >= 100 ? 0 : decimals)
  return `${s.replace(/\.0+$/, '')} ${BYTE_UNITS[i]}`
}

/** Locale number formatting: 1234567 → "1,234,567". */
export function formatNumber(n: number | null | undefined, opts?: Intl.NumberFormatOptions): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return '—'
  return new Intl.NumberFormat(undefined, opts).format(n)
}

/** Compact number: 12345 → "12K". */
export function formatCompact(n: number | null | undefined): string {
  return formatNumber(n, { notation: 'compact', maximumFractionDigits: 1 })
}

function toDate(v: string | number | Date | null | undefined): Date | undefined {
  if (v === null || v === undefined || v === '') return undefined
  const d = v instanceof Date ? v : new Date(v)
  return Number.isNaN(d.getTime()) ? undefined : d
}

/** Locale date+time (medium date, short time). */
export function formatDate(v: string | number | Date | null | undefined, opts: Intl.DateTimeFormatOptions = { dateStyle: 'medium', timeStyle: 'short' }): string {
  const d = toDate(v)
  if (!d) return '—'
  return new Intl.DateTimeFormat(undefined, opts).format(d)
}

/** ISO 8601 in UTC, e.g. for tooltips. */
export function formatISO(v: string | number | Date | null | undefined): string {
  const d = toDate(v)
  return d ? d.toISOString() : ''
}

const RELATIVE_UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 365 * 24 * 3600],
  ['month', 30 * 24 * 3600],
  ['week', 7 * 24 * 3600],
  ['day', 24 * 3600],
  ['hour', 3600],
  ['minute', 60],
  ['second', 1]
]

/** Relative time: "3 minutes ago", "in 2 days". `now` is injectable for tests. */
export function formatRelativeTime(v: string | number | Date | null | undefined, now: number = Date.now()): string {
  const d = toDate(v)
  if (!d) return '—'
  const diffSec = Math.round((d.getTime() - now) / 1000)
  const abs = Math.abs(diffSec)
  if (abs < 5) return 'just now'
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })
  for (const [unit, secs] of RELATIVE_UNITS) {
    if (abs >= secs || unit === 'second') return rtf.format(Math.round(diffSec / secs), unit)
  }
  return rtf.format(diffSec, 'second')
}

/** Formats a duration given in seconds: 3725 → "1h 2m 5s". */
export function formatSeconds(totalSeconds: number | null | undefined): string {
  if (totalSeconds === null || totalSeconds === undefined || !Number.isFinite(totalSeconds)) return '—'
  if (totalSeconds < 1) {
    if (totalSeconds === 0) return '0s'
    const ms = totalSeconds * 1000
    if (ms >= 1) return `${+ms.toFixed(ms < 10 ? 2 : 0)}ms`
    const us = ms * 1000
    if (us >= 1) return `${+us.toFixed(us < 10 ? 2 : 0)}µs`
    return `${Math.round(us * 1000)}ns`
  }
  let s = Math.floor(totalSeconds)
  const d = Math.floor(s / 86400)
  s -= d * 86400
  const h = Math.floor(s / 3600)
  s -= h * 3600
  const m = Math.floor(s / 60)
  s -= m * 60
  const parts: string[] = []
  if (d) parts.push(`${d}d`)
  if (h) parts.push(`${h}h`)
  if (m) parts.push(`${m}m`)
  if (s || parts.length === 0) parts.push(`${s}s`)
  return parts.slice(0, 3).join(' ')
}

/** Formats a Go time.Duration in nanoseconds: 1_500_000_000 → "1.5s", 60e9 → "1m". */
export function formatDuration(ns: number | null | undefined): string {
  if (ns === null || ns === undefined || !Number.isFinite(ns)) return '—'
  if (ns < 0) return `-${formatDuration(-ns)}`
  if (ns === 0) return '0s'
  if (ns < 1e3) return `${ns}ns`
  if (ns < 1e6) return `${+(ns / 1e3).toFixed(2)}µs`
  if (ns < 1e9) return `${+(ns / 1e6).toFixed(2)}ms`
  if (ns < 60e9) return `${+(ns / 1e9).toFixed(3)}s`
  return formatSeconds(ns / 1e9)
}

/** Nanoseconds ↔ seconds helpers for duration form fields. */
export const NS_PER_SECOND = 1e9
export function secondsToNs(seconds: number): number {
  return Math.round(seconds * NS_PER_SECOND)
}
export function nsToSeconds(ns: number): number {
  return ns / NS_PER_SECOND
}

/** Percentage string with one decimal: (25, 200) → "12.5%". */
export function formatPercent(part: number, total: number): string {
  if (!total) return '0%'
  return `${+((part / total) * 100).toFixed(1)}%`
}

/** Truncates the middle of long strings: "averylongname…suffix". */
export function truncateMiddle(s: string, max = 48): string {
  if (s.length <= max) return s
  const keep = max - 1
  const head = Math.ceil(keep / 2)
  const tail = Math.floor(keep / 2)
  return `${s.slice(0, head)}…${s.slice(s.length - tail)}`
}
