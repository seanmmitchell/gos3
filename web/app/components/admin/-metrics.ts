// Helpers for the server overview: per-minute traffic series, SVG chart
// geometry (no chart dependency) and per-operation table sorting.
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { MinuteBucket, OpStats } from '~/types/admin'

export const CHART_MINUTES = 60

export type SeriesColor = 'primary' | 'error' | 'info' | 'warning' | 'success' | 'secondary'

/** One line of AdminTrafficChart. */
export interface TrafficSeries {
  key: 'requests' | 'errors' | 'bytesIn' | 'bytesOut'
  label: string
  color: SeriesColor
}

/**
 * Expands the server's `recent` buckets (oldest first, possibly sparse or
 * null) into exactly `minutes` consecutive per-minute points ending at the
 * newest bucket (or `now`), filling gaps with zeros.
 */
export function fillMinutes(recent: readonly MinuteBucket[] | null | undefined, now: number = Date.now(), minutes = CHART_MINUTES): MinuteBucket[] {
  const byMinute = new Map<number, MinuteBucket>()
  let newest = Math.floor(now / 60000)
  for (const b of recent ?? []) {
    const t = Date.parse(b.minute)
    if (Number.isNaN(t)) continue
    const m = Math.floor(t / 60000)
    byMinute.set(m, b)
  }
  if (byMinute.size) newest = Math.max(...byMinute.keys())
  const out: MinuteBucket[] = []
  for (let m = newest - minutes + 1; m <= newest; m++) {
    const b = byMinute.get(m)
    out.push({
      minute: new Date(m * 60000).toISOString(),
      requests: b?.requests ?? 0,
      errors: b?.errors ?? 0,
      bytesIn: b?.bytesIn ?? 0,
      bytesOut: b?.bytesOut ?? 0
    })
  }
  return out
}

/** Rounds a maximum up to a "nice" axis value (1, 2, 2.5, 5 × 10^n); 0 → 1. */
export function niceMax(v: number): number {
  if (!Number.isFinite(v) || v <= 0) return 1
  const exp = Math.floor(Math.log10(v))
  const base = 10 ** exp
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (v <= m * base) return m * base
  }
  return 10 * base
}

/** Nice maximum for byte values (powers of 1024 × 1, 2, 5). */
export function niceBytesMax(v: number): number {
  if (!Number.isFinite(v) || v <= 0) return 1024
  const exp = Math.floor(Math.log(v) / Math.log(1024))
  const base = 1024 ** exp
  for (const m of [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000]) {
    if (v <= m * base) return m * base
  }
  return 1024 * base
}

export interface ChartBox {
  width: number
  height: number
  /** Inner padding (axis labels live in it). */
  padLeft: number
  padRight: number
  padTop: number
  padBottom: number
}

/** x coordinate of point i of n. */
export function xAt(i: number, n: number, box: ChartBox): number {
  const w = box.width - box.padLeft - box.padRight
  return box.padLeft + (n <= 1 ? w : (i / (n - 1)) * w)
}

/** y coordinate of value v on a 0..max axis. */
export function yAt(v: number, max: number, box: ChartBox): number {
  const h = box.height - box.padTop - box.padBottom
  const clamped = Math.max(0, Math.min(v, max))
  return box.padTop + h - (max > 0 ? (clamped / max) * h : 0)
}

const r1 = (n: number) => Math.round(n * 10) / 10

/** SVG path ("M x y L …") of a polyline through the values. */
export function linePath(values: readonly number[], max: number, box: ChartBox): string {
  if (!values.length) return ''
  return values.map((v, i) => `${i ? 'L' : 'M'}${r1(xAt(i, values.length, box))} ${r1(yAt(v, max, box))}`).join(' ')
}

/** Closed SVG path of the area under the line (for a translucent fill). */
export function areaPath(values: readonly number[], max: number, box: ChartBox): string {
  if (!values.length) return ''
  const base = r1(box.height - box.padBottom)
  const first = r1(xAt(0, values.length, box))
  const last = r1(xAt(values.length - 1, values.length, box))
  return `${linePath(values, max, box)} L${last} ${base} L${first} ${base} Z`
}

/** Index of the point nearest to an x coordinate (hover crosshair). */
export function indexAtX(x: number, n: number, box: ChartBox): number {
  if (n <= 1) return 0
  const w = box.width - box.padLeft - box.padRight
  const i = Math.round(((x - box.padLeft) / w) * (n - 1))
  return Math.max(0, Math.min(n - 1, i))
}

// ---------------------------------------------------------------------------
// Per-operation table
// ---------------------------------------------------------------------------

export type OpSortKey = 'op' | 'requests' | 'errors' | 'errorRate' | 'bytesIn' | 'bytesOut' | 'avgLatencyMs' | 'p50LatencyMs' | 'p99LatencyMs'
export type SortDir = 'asc' | 'desc'

export function opErrors(o: OpStats): number {
  return (o.errors4xx ?? 0) + (o.errors5xx ?? 0)
}

export function opErrorRate(o: OpStats): number {
  return o.requests ? opErrors(o) / o.requests : 0
}

function opSortValue(o: OpStats, key: OpSortKey): number | string {
  switch (key) {
    case 'op': return o.op
    case 'errors': return opErrors(o)
    case 'errorRate': return opErrorRate(o)
    default: return o[key] ?? 0
  }
}

/** Returns a sorted copy (ties broken by op name). */
export function sortOps(ops: readonly OpStats[] | null | undefined, key: OpSortKey, dir: SortDir): OpStats[] {
  const sign = dir === 'asc' ? 1 : -1
  return [...(ops ?? [])].sort((a, b) => {
    const va = opSortValue(a, key)
    const vb = opSortValue(b, key)
    let c = typeof va === 'string' || typeof vb === 'string' ? String(va).localeCompare(String(vb)) : va - vb
    if (c === 0 && key !== 'op') c = a.op.localeCompare(b.op) * sign
    return c * sign
  })
}

/** Next sort state when a column header is activated (new column → desc, except op → asc). */
export function nextSort(current: { key: OpSortKey, dir: SortDir }, key: OpSortKey): { key: OpSortKey, dir: SortDir } {
  if (current.key === key) return { key, dir: current.dir === 'asc' ? 'desc' : 'asc' }
  return { key, dir: key === 'op' ? 'asc' : 'desc' }
}

/** Formats a latency in milliseconds ("0.42 ms", "12 ms", "1.5 s"). */
export function formatLatencyMs(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return '—'
  if (ms >= 1000) return `${+(ms / 1000).toFixed(2)} s`
  if (ms >= 10) return `${Math.round(ms)} ms`
  return `${+ms.toFixed(2)} ms`
}

/** Disk usage percentage (0..100) from DiskStats-like totals. */
export function diskUsedPercent(used: number, total: number): number {
  if (!total || total <= 0) return 0
  return Math.max(0, Math.min(100, (used / total) * 100))
}
