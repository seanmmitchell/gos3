// Storage backings as offered to bucket creators (auto-imported by Nuxt).
//
// A bucket's primary backing is chosen when it is created (CreateBucket
// header x-gos3-backing) and reported by HeadBucket (x-gos3-backing header)
// and the admin BucketSummary (placement.primary). The choices, with each
// backing's kind and, for kind "s3", the bucket kinds and containers it
// offers, come from WhoAmI.backings, which every authenticated caller gets.
// Generic S3 servers have no backings.
//
// An s3 backing is a connection to a remote S3 service's account. Its
// buckets are either native (objects as-is in a remote bucket of their own,
// x-gos3-bucket-kind: native, x-gos3-remote-bucket) or wrapped (gos3's own
// format inside a container shared by many buckets, x-gos3-bucket-kind:
// wrapped, x-gos3-container).
import type { AvailableBacking, AvailableBackings, BucketKind, ContainerChoice } from '~/types/admin'

/** What the UI needs to know about a backing's kind. */
export interface BackingKindInfo {
  label: string
  description: string
  icon: string
}

const KIND_INFO: Record<string, BackingKindInfo> = {
  fs: { label: 'Disk', description: 'Stored on this server\'s disks.', icon: 'i-lucide-hard-drive' },
  memory: { label: 'Memory', description: 'Kept in this server\'s memory; lost on restart.', icon: 'i-lucide-memory-stick' },
  s3: { label: 'Remote S3', description: 'A remote S3 account: native buckets keep their objects as-is in remote buckets, wrapped buckets use gos3\'s format inside a container.', icon: 'i-lucide-cloud' }
}

/** Label, one-line description and icon of a backing kind. */
export function backingKindInfo(b: Pick<AvailableBacking, 'kind'> | null | undefined): BackingKindInfo {
  if (!b) return { label: 'Unknown', description: 'Storage backing of this server.', icon: 'i-lucide-database' }
  return KIND_INFO[b.kind] ?? { label: b.kind, description: 'Storage backing of this server.', icon: 'i-lucide-database' }
}

/** What the UI needs to know about a bucket kind (s3 backings). */
export interface BucketKindInfo {
  label: string
  description: string
  icon: string
}

const BUCKET_KIND_INFO: Record<BucketKind, BucketKindInfo> = {
  native: {
    label: 'Native',
    description: 'Objects stored as-is in a remote bucket; versioning and object lock follow the remote bucket.',
    icon: 'i-lucide-cloud'
  },
  wrapped: {
    label: 'Wrapped',
    description: 'gos3 format inside a container; every gos3 feature.',
    icon: 'i-lucide-package'
  }
}

/** The known bucket kinds, in the order the UI offers them. */
export const BUCKET_KINDS: readonly BucketKind[] = ['wrapped', 'native']

export function isBucketKind(v: unknown): v is BucketKind {
  return v === 'native' || v === 'wrapped'
}

/** Label, description and icon of a bucket kind (unknown kinds are shown as sent). */
export function bucketKindInfo(kind: string | null | undefined): BucketKindInfo {
  if (isBucketKind(kind)) return BUCKET_KIND_INFO[kind]
  return { label: kind || 'Unknown', description: 'How this bucket is stored on its s3 backing.', icon: 'i-lucide-box' }
}

/**
 * The bucket kinds a backing offers (WhoAmI.backings, GET /backings), known
 * ones only, in BUCKET_KINDS order; [] for backings other than s3 and for
 * servers that do not report kinds (then no kind is sent: the server decides).
 */
export function offeredKinds(b: Pick<AvailableBacking, 'kind' | 'kinds'> | null | undefined): BucketKind[] {
  if (b?.kind !== 's3') return []
  const offered = new Set(b.kinds ?? [])
  return BUCKET_KINDS.filter(k => offered.has(k))
}

/** The kind a new bucket on the backing gets by default ('' when it offers no choice). */
export function defaultBucketKind(b: Pick<AvailableBacking, 'kind' | 'kinds' | 'defaultKind'> | null | undefined): BucketKind | '' {
  const kinds = offeredKinds(b)
  if (!kinds.length) return ''
  return kinds.find(k => k === b?.defaultKind) ?? kinds[0]!
}

export interface KindChoice {
  value: BucketKind
  label: string
  description: string
  icon: string
}

/** URadioGroup items for the bucket kinds a backing offers, the default marked. */
export function kindChoices(b: Pick<AvailableBacking, 'kind' | 'kinds' | 'defaultKind'> | null | undefined): KindChoice[] {
  const def = defaultBucketKind(b)
  return offeredKinds(b).map((k) => {
    const info = bucketKindInfo(k)
    return { value: k, label: k === def ? `${info.label} (default)` : info.label, description: info.description, icon: info.icon }
  })
}

/** The containers a backing offers, or [] (backings other than s3, none added). */
export function backingContainers(b: { containers?: readonly ContainerChoice[] | null } | null | undefined): ContainerChoice[] {
  return [...(b?.containers ?? [])]
}

/** The default container's ID ('' when there is none). */
export function defaultContainer(b: { containers?: readonly ContainerChoice[] | null } | null | undefined): string {
  const list = backingContainers(b)
  return list.find(c => c.default)?.id ?? ''
}

export interface ContainerItem {
  label: string
  value: string
  description?: string
}

/**
 * USelect items for the containers of a backing's wrapped buckets: the
 * label (or the ID when it has none), the default marked, and the ID as
 * description when a label is shown. The home container is the backing's
 * shared container.
 */
export function containerItems(b: { containers?: readonly ContainerChoice[] | null } | null | undefined): ContainerItem[] {
  return backingContainers(b).map((c) => {
    const name = c.label || (c.id === 'home' ? 'shared (home)' : c.id)
    const item: ContainerItem = { label: c.default ? `${name} (default)` : name, value: c.id }
    if (c.label && c.label !== c.id) item.description = c.id
    return item
  })
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
