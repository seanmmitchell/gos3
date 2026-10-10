// Freshness of the bucket usage figures shown in the bucket list.
//
// The admin API caches per-bucket usage for up to 30 s, so right after an
// upload the list can still show the previous counts. The list says how old
// its figures are instead of silently showing stale numbers.
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { BucketUsage } from '~/types/admin'
import { formatRelativeTime } from '~/utils/format'

/** Oldest computedAt (epoch ms) among the given usages, or undefined. */
export function oldestUsageTime(usages: readonly (BucketUsage | null | undefined)[]): number | undefined {
  let oldest: number | undefined
  for (const u of usages) {
    const t = u?.computedAt ? Date.parse(u.computedAt) : Number.NaN
    if (Number.isFinite(t) && (oldest === undefined || t < oldest)) oldest = t
  }
  return oldest
}

/**
 * " (usage as of 25 seconds ago)" when the oldest figure is at least
 * `minAgeMs` old, else "" (fresh figures need no remark).
 */
export function usageAgeNote(usages: readonly (BucketUsage | null | undefined)[], now: number = Date.now(), minAgeMs = 5000): string {
  const t = oldestUsageTime(usages)
  if (t === undefined || now - t < minAgeMs) return ''
  return ` (usage as of ${formatRelativeTime(t, now)})`
}
