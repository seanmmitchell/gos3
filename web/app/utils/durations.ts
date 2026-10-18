// Go duration strings (auto-imported by Nuxt from app/utils/).
//
// Some admin API fields are Go durations written as strings ("24h",
// "1h30m", "5m": copy job poll intervals, snapshot schedules), unlike the
// integer-nanosecond durations of the server settings. Forms accept the Go
// units plus "d" (days) for convenience and always send Go's own units.

const UNIT_SECONDS: Record<string, number> = {
  ns: 1e-9,
  us: 1e-6,
  µs: 1e-6,
  μs: 1e-6,
  ms: 1e-3,
  s: 1,
  m: 60,
  h: 3600,
  d: 86400
}

const DURATION_RE = /^(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h|d)/

/**
 * Seconds of a Go duration string ("1h30m", "90s", "0s", "1.5h"; "d" for
 * days is accepted too), or undefined when it is not one. "0" alone is 0.
 * Negative durations are refused.
 */
export function parseGoDuration(input: string | null | undefined): number | undefined {
  const s = (input ?? '').trim()
  if (!s) return undefined
  if (s === '0') return 0
  let rest = s
  let total = 0
  while (rest) {
    const m = DURATION_RE.exec(rest)
    if (!m) return undefined
    const unit = /(ns|us|µs|μs|ms|s|m|h|d)$/.exec(m[0])![1]!
    total += Number.parseFloat(m[0].slice(0, -unit.length)) * UNIT_SECONDS[unit]!
    rest = rest.slice(m[0].length)
  }
  return Number.isFinite(total) ? total : undefined
}

/**
 * A duration in seconds as a Go duration string with the units Go accepts
 * (h, m, s, ms): 86400 → "24h", 5400 → "1h30m", 0 → "0s".
 */
export function formatGoDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0s'
  let ms = Math.round(seconds * 1000)
  const h = Math.floor(ms / 3600000)
  ms -= h * 3600000
  const m = Math.floor(ms / 60000)
  ms -= m * 60000
  const s = Math.floor(ms / 1000)
  ms -= s * 1000
  let out = ''
  if (h) out += `${h}h`
  if (m) out += `${m}m`
  if (s) out += `${s}s`
  if (ms) out += `${ms}ms`
  return out || '0s'
}

/**
 * A duration field as received from the server, as a form value: Go
 * duration strings are normalized ("24h0m0s" → "24h"), numbers are taken
 * as nanoseconds (time.Duration), anything else gives `fallback`.
 */
export function durationText(v: unknown, fallback = ''): string {
  if (typeof v === 'number' && Number.isFinite(v)) return formatGoDuration(v / 1e9)
  if (typeof v === 'string') {
    const s = parseGoDuration(v)
    return s === undefined ? v : formatGoDuration(s)
  }
  return fallback
}
