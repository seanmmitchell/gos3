// Storage backings as offered to bucket creators (auto-imported by Nuxt).
//
// A bucket's primary backing is chosen when it is created (CreateBucket
// header x-gos3-backing) and reported by HeadBucket (x-gos3-backing header)
// and the admin BucketSummary (placement.primary). The choices, with each
// backing's kind and (kind "s3") mode, come from WhoAmI.backings, which every
// authenticated caller gets. Generic S3 servers have no backings.
import type { AvailableBacking, AvailableBackings } from '~/types/admin'

/** What the UI needs to know about a backing's kind / mode. */
export interface BackingKindInfo {
  label: string
  description: string
  icon: string
}

const KIND_INFO: Record<string, BackingKindInfo> = {
  'fs': { label: 'Disk', description: 'Stored on this server\'s disks.', icon: 'i-lucide-hard-drive' },
  'memory': { label: 'Memory', description: 'Kept in this server\'s memory; lost on restart.', icon: 'i-lucide-memory-stick' },
  's3-blob': { label: 'Remote S3 (blob)', description: 'Data stored in a remote S3 bucket; all features available.', icon: 'i-lucide-cloud' },
  's3-gateway': { label: 'Remote S3 (gateway)', description: 'Objects stored directly in a remote S3 service; no versioning or object lock.', icon: 'i-lucide-cloud-cog' }
}

/** Label, one-line description and icon of a backing kind (+ mode for kind "s3"). */
export function backingKindInfo(b: Pick<AvailableBacking, 'kind' | 'mode'> | null | undefined): BackingKindInfo {
  if (!b) return { label: 'Unknown', description: 'Storage backing of this server.', icon: 'i-lucide-database' }
  const known = KIND_INFO[b.kind === 's3' ? `s3-${b.mode ?? ''}` : b.kind]
  if (known) return known
  const label = b.mode ? `${b.kind} (${b.mode})` : b.kind
  return { label, description: 'Storage backing of this server.', icon: b.kind === 's3' ? 'i-lucide-cloud' : 'i-lucide-database' }
}

/** Gateway backings (kind "s3", mode "gateway") store objects natively: no versioning, no object lock. */
export function isGatewayBacking(b: Pick<AvailableBacking, 'kind' | 'mode'> | null | undefined): boolean {
  return b?.kind === 's3' && b.mode === 'gateway'
}

/**
 * The backings a user can choose from when creating a bucket, or [] when
 * there is nothing to choose (older server, or a single backing).
 */
export function backingChoices(b: AvailableBackings | null | undefined): AvailableBacking[] {
  const list = b?.available ?? []
  return list.length > 1 ? list : []
}

/** The server's default backing if it is one of the available ones, else the first one ('' when none). */
export function defaultBacking(b: AvailableBackings | null | undefined): string {
  const list = b?.available ?? []
  return list.find(x => x.name === b?.default)?.name ?? list[0]?.name ?? ''
}

/** The available backing called `name`, if WhoAmI.backings lists it. */
export function findBacking(name: string | undefined, b: AvailableBackings | null | undefined): AvailableBacking | undefined {
  return name ? b?.available?.find(x => x.name === name) : undefined
}

export interface BackingSelectItem {
  label: string
  value: string
  description: string
  icon: string
}

/** USelect items for the create-bucket backing selector, the default marked. */
export function backingSelectItems(b: AvailableBackings | null | undefined): BackingSelectItem[] {
  const def = defaultBacking(b)
  return backingChoices(b).map((x) => {
    const info = backingKindInfo(x)
    return {
      label: x.name === def ? `${x.name} (default)` : x.name,
      value: x.name,
      description: `${info.label}: ${info.description}`,
      icon: info.icon
    }
  })
}
