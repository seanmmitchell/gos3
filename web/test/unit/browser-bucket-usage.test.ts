import { describe, expect, it } from 'vitest'
import { oldestUsageTime, usageAgeNote } from '~/components/browser/-bucket-usage'
import type { BucketUsage } from '~/types/admin'

const usage = (computedAt?: string): BucketUsage => ({ objects: 1, bytes: 1, versions: 1, versionBytes: 1, deleteMarkers: 0, pendingUploads: 0, computedAt })

describe('bucket usage freshness', () => {
  const now = Date.parse('2026-10-10T12:00:30Z')

  it('finds the oldest computedAt and ignores missing or invalid ones', () => {
    expect(oldestUsageTime([])).toBeUndefined()
    expect(oldestUsageTime([null, undefined, usage(), usage('nope')])).toBeUndefined()
    expect(oldestUsageTime([usage('2026-10-10T12:00:20Z'), null, usage('2026-10-10T12:00:05Z')])).toBe(Date.parse('2026-10-10T12:00:05Z'))
  })

  it('mentions the age only when the figures are not fresh', () => {
    expect(usageAgeNote([usage('2026-10-10T12:00:28Z')], now)).toBe('')
    expect(usageAgeNote([usage('2026-10-10T12:00:28Z'), usage('2026-10-10T12:00:05Z')], now)).toBe(' (usage as of 25 seconds ago)')
    expect(usageAgeNote([null], now)).toBe('')
  })
})
