// Helpers for the remote bucket browser of an s3 backing (backing page,
// "Remote buckets" tab) and its modals: status badges, the actions each
// remote bucket offers, form validation and the admin API request bodies
// (docs/admin-api.md, "Remote buckets and containers (s3 backings)"). Pure
// functions, so the components and the tests share the same rules.
//
// The file name starts with "-" so Nuxt does not register it as a component.
import { AdminError } from '~/utils/admin/client'
import type { ContainerSpec, CreateOnBacking, CreateRemoteBucketRequest, RemoteBucket, RemoteBucketList, RemoteBucketStatus, RemoteBucketUse } from '~/types/admin'
import { validateBucketName } from '~/utils/paths'
import type { RemoteAccess } from './-capabilities'

/** Buckets per page of the remote bucket browser. */
export const REMOTE_PAGE_SIZE = 100

type BadgeColor = 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'error' | 'neutral'

export interface RemoteStatusMeta {
  label: string
  color: BadgeColor
  icon: string
  description: string
}

export const REMOTE_STATUSES: Readonly<Record<RemoteBucketStatus, RemoteStatusMeta>> = {
  native: { label: 'Native bucket', color: 'primary', icon: 'i-lucide-cloud', description: 'A native gos3 bucket: gos3 serves its objects as they are.' },
  home: { label: 'Home', color: 'info', icon: 'i-lucide-house', description: 'This backing\'s home: its records and its shared container of wrapped buckets.' },
  container: { label: 'Container', color: 'info', icon: 'i-lucide-package', description: 'Holds a container of wrapped buckets.' },
  claimed: { label: 'Used elsewhere', color: 'warning', icon: 'i-lucide-link', description: 'Used by another backing of this server.' },
  gos3: { label: 'gos3 data', color: 'warning', icon: 'i-lucide-database', description: 'Holds gos3 data that no backing of this server uses, for example from another server.' },
  inaccessible: { label: 'Inaccessible', color: 'error', icon: 'i-lucide-lock', description: 'These credentials cannot read it.' },
  unmanaged: { label: 'Unmanaged', color: 'neutral', icon: 'i-lucide-circle-dashed', description: 'Not used by gos3.' }
}

/** Badge metadata of a remote bucket status (unknown statuses are neutral). */
export function remoteStatusMeta(status: RemoteBucketStatus | string): RemoteStatusMeta {
  return REMOTE_STATUSES[status as RemoteBucketStatus] ?? { label: status || 'Unknown', color: 'neutral', icon: 'i-lucide-circle-help', description: '' }
}

/** The gos3 bucket a native remote bucket backs, if any. */
export function nativeBucketOf(rb: Pick<RemoteBucket, 'uses'>): string | undefined {
  return rb.uses?.find(u => u.role === 'native' && u.bucket)?.bucket
}

/** One line per use of a remote bucket, for the "Used by" column. */
export function describeUse(u: RemoteBucketUse): string {
  const at = u.prefix ? ` at ${u.prefix}` : ''
  switch (u.role) {
    case 'native': return `gos3 bucket ${u.bucket ?? '?'} (${u.created ? 'created by gos3' : 'adopted'})`
    case 'home': return `Home of this backing${at}`
    case 'container': return `Container ${u.containerId ?? '?'}${at}`
    case 'claimed': return u.backing ? `Used by backing ${u.backing}` : 'Used by another backing'
    default: return u.role
  }
}

/** Actions of the remote bucket browser. */
export type RemoteBucketAction = 'open' | 'release' | 'adopt' | 'use-as-container' | 'attach-container' | 'create-wrapped' | 'containers'

export interface RemoteActionItem {
  action: RemoteBucketAction
  label: string
  icon: string
  color?: 'error' | 'warning'
}

/**
 * The shape the server accepts for an existing remote bucket (to adopt or to
 * hold a container): 2–255 lowercase letters, digits, dots, dashes and
 * underscores, starting with a letter or digit (validRemoteBucketName in
 * internal/admin/remotebuckets.go; the same as an s3 backing's home bucket).
 */
export const REMOTE_BUCKET_NAME_RE = /^[a-z0-9][a-z0-9._-]{1,254}$/

/**
 * Why gos3 cannot use a remote bucket of the list ('' when it can): its name
 * does not have the shape the server accepts (e.g. uppercase letters, which
 * some older buckets have).
 */
export function remoteNameNote(name: string): string {
  return REMOTE_BUCKET_NAME_RE.test(name) ? '' : 'gos3 cannot use this bucket: its name has characters gos3 does not accept (only lowercase letters, digits, dots, dashes and underscores).'
}

/**
 * The actions a remote bucket offers, by status and permission:
 * - native:       open the gos3 bucket; release it (gos3 forgets it, the
 *                 remote bucket and its objects stay)
 * - unmanaged:    adopt it as a native bucket (admin:AdoptRemoteBucket),
 *                 use it as a container, or create a wrapped bucket in a new
 *                 container inside it (both also admin:ManageContainers)
 * - gos3:         attach the gos3 store it holds as a container
 * - home / container: show the containers
 * - claimed, inaccessible: nothing
 * Buckets whose names gos3 does not accept (remoteNameNote) offer no adopt
 * or container actions.
 */
export function remoteBucketActions(rb: Pick<RemoteBucket, 'name' | 'status' | 'uses'>, access: RemoteAccess): RemoteActionItem[] {
  const out: RemoteActionItem[] = []
  const usable = !remoteNameNote(rb.name)
  switch (rb.status) {
    case 'native':
      if (nativeBucketOf(rb)) {
        out.push({ action: 'open', label: 'Open bucket', icon: 'i-lucide-folder-open' })
        if (access.release) out.push({ action: 'release', label: 'Release…', icon: 'i-lucide-unlink', color: 'warning' })
      }
      break
    case 'unmanaged':
      if (!usable) break
      if (access.adopt) out.push({ action: 'adopt', label: 'Adopt as native bucket…', icon: 'i-lucide-cloud-download' })
      if (access.adopt && access.containers) {
        out.push({ action: 'use-as-container', label: 'Use as container…', icon: 'i-lucide-package-plus' })
        out.push({ action: 'create-wrapped', label: 'Create wrapped bucket in it…', icon: 'i-lucide-package' })
      }
      break
    case 'gos3':
      if (usable && access.adopt && access.containers) out.push({ action: 'attach-container', label: 'Attach as container…', icon: 'i-lucide-package-check' })
      break
    case 'home':
    case 'container':
      out.push({ action: 'containers', label: 'Show containers', icon: 'i-lucide-package' })
      break
  }
  return out
}

/**
 * Why the list is incomplete ('' when it is complete): the credentials may
 * not list the account's buckets, so only the locations the backing knows
 * are shown and other buckets must be adopted by name.
 */
export function incompleteListMessage(list: Pick<RemoteBucketList, 'complete' | 'listError'> | null | undefined): string {
  if (!list || list.complete) return ''
  const why = list.listError ? ` (${list.listError})` : ''
  return `These credentials cannot list the account's buckets${why}. Only the remote buckets this backing knows are shown; adopt other buckets by name.`
}

// --- validation ------------------------------------------------------------------

/**
 * Error of a remote bucket name (undefined when valid). Buckets gos3 creates
 * must follow the S3 naming rules (`strict`); existing buckets are checked
 * like the server does (REMOTE_BUCKET_NAME_RE), which also accepts
 * underscores and names up to 255 characters.
 */
export function validateRemoteBucketName(name: string, strict: boolean): string | undefined {
  if (!name) return 'Enter the remote bucket name'
  if (strict) return validateBucketName(name)
  if (name.length < 2 || name.length > 255) return 'Remote bucket names are 2–255 characters long'
  if (!REMOTE_BUCKET_NAME_RE.test(name)) return 'Use lowercase letters, digits, dots, dashes and underscores, starting with a letter or digit'
  return undefined
}

/**
 * Error of a key prefix inside a remote bucket (optional): no leading "/",
 * no empty, "." or ".." segments, at most 512 characters.
 */
export function validatePrefix(prefix: string): string | undefined {
  if (!prefix) return undefined
  if (prefix.length > 512) return 'At most 512 characters'
  if (prefix.startsWith('/')) return 'Must not start with "/"'
  // eslint-disable-next-line no-control-regex
  if (/[\x00-\x1f\x7f]/.test(prefix)) return 'Must not contain control characters'
  const segs = prefix.replace(/\/$/, '').split('/')
  if (segs.some(s => s === '' || s === '.' || s === '..')) return 'Must not contain empty, "." or ".." segments'
  return undefined
}

/** Error of an optional region (lowercase letters, digits and dashes). */
export function validateRegion(region: string): string | undefined {
  if (!region) return undefined
  return /^[a-z0-9][a-z0-9-]{0,62}$/.test(region) ? undefined : 'Use lowercase letters, digits and dashes'
}

/** Error of a gos3 bucket name, including names the server already has. */
export function validateGos3BucketName(name: string, existing?: readonly string[]): string | undefined {
  if (!name) return 'Enter a bucket name'
  const e = validateBucketName(name)
  if (e) return e
  if (existing?.includes(name)) return 'There already is a bucket with this name'
  return undefined
}

// --- adopt -----------------------------------------------------------------------

export interface AdoptForm {
  /** The existing remote bucket. */
  remoteBucket: string
  /** The gos3 bucket name (defaults to the remote name). */
  bucket: string
  /** Remote region ("" = discovered). */
  region: string
}

export interface AdoptFormErrors {
  remoteBucket?: string
  bucket?: string
  region?: string
}

export function validateAdoptForm(f: AdoptForm, existing?: readonly string[]): AdoptFormErrors {
  const e: AdoptFormErrors = {}
  const remote = validateRemoteBucketName(f.remoteBucket.trim(), false)
  if (remote) e.remoteBucket = remote
  const bucket = validateGos3BucketName(f.bucket.trim(), existing)
  if (bucket) e.bucket = bucket
  const region = validateRegion(f.region.trim())
  if (region) e.region = region
  return e
}

/** POST /backings/{name}/buckets body that adopts an existing remote bucket. */
export function adoptRequest(f: AdoptForm): CreateOnBacking {
  const req: CreateOnBacking = { bucket: f.bucket.trim(), kind: 'native', remoteBucket: f.remoteBucket.trim(), adopt: true }
  if (f.region.trim()) req.region = f.region.trim()
  return req
}

/**
 * The gos3 name suggested for a remote bucket: the remote name when it is a
 * valid gos3 bucket name, else the remote name with the characters gos3
 * bucket names cannot have (e.g. underscores) replaced (the user can still
 * edit it).
 */
export function suggestedBucketName(remote: string): string {
  if (!validateBucketName(remote)) return remote
  const s = remote.toLowerCase().replace(/[^a-z0-9.-]+/g, '-').replace(/\.{2,}/g, '.').replace(/^[^a-z0-9]+|[^a-z0-9]+$/g, '').slice(0, 63)
  return s.replace(/[^a-z0-9]+$/, '')
}

// --- create remote bucket --------------------------------------------------------

export interface CreateRemoteForm {
  name: string
  region: string
  objectLock: boolean
}

export interface CreateRemoteFormErrors {
  name?: string
  region?: string
}

export function validateCreateRemoteForm(f: CreateRemoteForm): CreateRemoteFormErrors {
  const e: CreateRemoteFormErrors = {}
  const name = validateRemoteBucketName(f.name.trim(), true)
  if (name) e.name = name
  const region = validateRegion(f.region.trim())
  if (region) e.region = region
  return e
}

/** POST /backings/{name}/remote-buckets body. */
export function createRemoteRequest(f: CreateRemoteForm): CreateRemoteBucketRequest {
  const req: CreateRemoteBucketRequest = { name: f.name.trim() }
  if (f.region.trim()) req.region = f.region.trim()
  if (f.objectLock) req.objectLock = true
  return req
}

// --- wrapped bucket --------------------------------------------------------------

export interface WrappedForm {
  bucket: string
  /** existing: a container of the backing; new: a new container in `remoteBucket`. */
  target: 'existing' | 'new'
  /** target existing: the container's ID. */
  container: string
  /** target new: the existing remote bucket holding the new container. */
  remoteBucket: string
  /** target new: the new container's prefix in the remote bucket. */
  prefix: string
  /** target new: an optional label for the new container. */
  label: string
  /** target new: the remote bucket's region, when known ("" = discovered). */
  region: string
}

export interface WrappedFormErrors {
  bucket?: string
  container?: string
  remoteBucket?: string
  prefix?: string
  label?: string
}

/** Container labels are limited to 128 bytes (UTF-8) by the server. */
export const MAX_LABEL_BYTES = 128

/** Error of an optional container label (shown to every caller). */
export function validateLabel(label: string): string | undefined {
  if (new TextEncoder().encode(label).length > MAX_LABEL_BYTES) return `At most ${MAX_LABEL_BYTES} bytes`
  // eslint-disable-next-line no-control-regex
  if (/[\x00-\x1f\x7f]/.test(label)) return 'Must not contain control characters'
  return undefined
}

export function validateWrappedForm(f: WrappedForm, existing?: readonly string[]): WrappedFormErrors {
  const e: WrappedFormErrors = {}
  const bucket = validateGos3BucketName(f.bucket.trim(), existing)
  if (bucket) e.bucket = bucket
  if (f.target === 'existing') {
    if (!f.container) e.container = 'Choose a container'
  } else {
    const remote = validateRemoteBucketName(f.remoteBucket.trim(), false)
    if (remote) e.remoteBucket = remote
    const prefix = validatePrefix(f.prefix.trim())
    if (prefix) e.prefix = prefix
    const label = validateLabel(f.label.trim())
    if (label) e.label = label
  }
  return e
}

/** POST /backings/{name}/buckets body that creates a wrapped bucket. */
export function wrappedRequest(f: WrappedForm): CreateOnBacking {
  const bucket = f.bucket.trim()
  if (f.target === 'existing') return { bucket, kind: 'wrapped', container: f.container }
  const newContainer: ContainerSpec = { bucket: f.remoteBucket.trim() }
  if (f.prefix.trim()) newContainer.prefix = f.prefix.trim()
  if (f.label.trim()) newContainer.label = f.label.trim()
  if (f.region.trim()) newContainer.region = f.region.trim()
  return { bucket, kind: 'wrapped', newContainer }
}

/**
 * Whether a failed request may have reached the server (and changed
 * something): an HTTP error, or a timeout after it was sent. Errors raised
 * before sending (no connection, invalid path, network failures) did not.
 */
export function requestReachedServer(e: unknown): boolean {
  return e instanceof AdminError && (e.status > 0 || e.code === 'Timeout')
}

/**
 * The container a failed "create a wrapped bucket in a new container"
 * request added anyway: the server keeps it and says so ("container "c-1"
 * was added and is kept; retry with "container": "c-1"").
 */
export function addedContainerFromError(e: unknown): string | undefined {
  const message = e instanceof Error ? e.message : ''
  return /container "([^"]+)" was added/.exec(message)?.[1]
}

/** True when an error object has no message. */
export function noErrors(e: object): boolean {
  return Object.values(e).every(v => !v)
}
