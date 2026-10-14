// Storage backings: where a gos3 bucket's data lives (auto-imported by Nuxt).
//
// A bucket's backing is chosen when it is created (CreateBucket header
// x-gos3-backing) and reported by the admin BucketSummary
// (backing/backingMode) and by HeadBucket (x-gos3-backing header). The
// choices come from WhoAmI.backings. Generic S3 servers have no backings.
import type { BackingChoices, BackingMode, BackingRef } from '~/types/admin'

/** Name of the server's own storage. */
export const LOCAL_BACKING = 'local'

export interface BackingModeInfo {
  label: string
  description: string
  icon: string
}

const MODE_INFO: Record<BackingMode, BackingModeInfo> = {
  local: { label: 'Local', description: 'Stored on this server\'s own disks.', icon: 'i-lucide-hard-drive' },
  blob: { label: 'Remote (blob)', description: 'Data stored in a remote S3 bucket; all features available.', icon: 'i-lucide-cloud' },
  gateway: { label: 'Remote (gateway)', description: 'Objects stored directly in a remote S3 service; no versioning or object lock.', icon: 'i-lucide-cloud-cog' }
}

/** Label, one-line description and icon of a backing mode (unknown modes get a generic entry). */
export function backingModeInfo(mode: string | undefined): BackingModeInfo {
  return (mode && MODE_INFO[mode as BackingMode]) || { label: mode || 'Unknown', description: 'Stored on a remote backing.', icon: 'i-lucide-cloud' }
}

/**
 * The backings a user can choose from when creating a bucket, or [] when
 * there is nothing to choose (older server, or only local storage).
 */
export function backingChoices(b: BackingChoices | null | undefined): BackingRef[] {
  const list = b?.available ?? []
  return list.length > 1 ? list : []
}

/** The server's default backing, if it is one of the available ones (else "local"). */
export function defaultBacking(b: BackingChoices | null | undefined): string {
  const d = b?.default
  return d && b?.available.some(x => x.name === d) ? d : LOCAL_BACKING
}

/** Mode of a backing name according to WhoAmI.backings ("local" is always local). */
export function backingModeOf(name: string | undefined, b: BackingChoices | null | undefined): BackingMode | undefined {
  if (!name) return undefined
  if (name === LOCAL_BACKING) return 'local'
  return b?.available.find(x => x.name === name)?.mode
}

/** Gateway backings store objects natively: no versioning, no object lock. */
export function backingSupportsVersioning(mode: string | undefined): boolean {
  return mode !== 'gateway'
}

export interface BackingSelectItem {
  label: string
  value: string
  description: string
  icon: string
}

/** USelect items for the create-bucket backing selector, the default marked. */
export function backingSelectItems(b: BackingChoices | null | undefined): BackingSelectItem[] {
  const def = defaultBacking(b)
  return backingChoices(b).map((x) => {
    const info = backingModeInfo(x.mode)
    return {
      label: x.name === def ? `${x.name} (default)` : x.name,
      value: x.name,
      description: x.mode === 'local' ? info.description : `${info.label}: ${info.description}`,
      icon: info.icon
    }
  })
}
