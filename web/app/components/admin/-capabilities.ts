// Capability gating helpers for the admin pages and bucket settings.
//
// The file name starts with "-" so Nuxt's component scanner ignores it
// (`ignorePrefix`); import it explicitly:
//
//   import { usersPageMode } from '~/components/admin/-capabilities'
//
// Every helper is a pure function of the connection kind and the
// capabilities reported by GET /whoami, so pages and tests share the exact
// same rules. Generic S3 connections never have admin features.
import type { Capabilities, CapabilityName } from '~/types/admin'
import type { ConnectionKind } from '~/types/connection'

/** Capabilities with every flag false (generic S3 / unknown identity). */
export const NO_CAPS: Readonly<Capabilities> = Object.freeze({
  admin: false,
  viewInfo: false,
  viewStats: false,
  manageConfig: false,
  manageUsers: false,
  listUsers: false,
  managePolicies: false,
  manageBuckets: false,
  viewStorage: false,
  manageStorage: false,
  discovery: false,
  scan: false,
  probe: false,
  createOwnKeys: false,
  manageCompression: false
})

/** True when the connection is gos3 and the identity has `cap`. */
export function hasCapability(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined, cap: CapabilityName): boolean {
  return kind === 'gos3' && !!caps?.[cap]
}

/**
 * What the server overview can show:
 * - stats:  full metrics dashboard (admin:GetStats)
 * - info:   server info only (admin:ServerInfo)
 * - public: public identity document only (gos3 without admin rights)
 * - s3:     generic S3 server, bucket summary only
 */
export type OverviewMode = 'stats' | 'info' | 'public' | 's3'

export function overviewMode(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): OverviewMode {
  if (kind !== 'gos3') return 's3'
  if (caps?.viewStats) return 'stats'
  if (caps?.viewInfo) return 'info'
  return 'public'
}

/**
 * Whether the caller may list IAM users (admin:ListUsers). Servers that
 * predate the `listUsers` capability only reported manageUsers.
 */
export function canListUsers(caps: Capabilities | null | undefined): boolean {
  return !!(caps?.listUsers ?? caps?.manageUsers)
}

/**
 * Users page:
 * - manage:      IAM user management (admin:CreateUser, or admin:ListUsers
 *                for a delegated manager whose user actions are scoped to
 *                some users; the server refuses the rest per user)
 * - self:        "My access keys" only (/whoami/keys)
 * - unavailable: generic S3 (no admin API)
 */
export type UsersPageMode = 'manage' | 'self' | 'unavailable'

export function usersPageMode(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): UsersPageMode {
  if (kind !== 'gos3') return 'unavailable'
  return caps?.manageUsers || canListUsers(caps) ? 'manage' : 'self'
}

/**
 * Root has no IAM access keys (its credentials come from the server config),
 * so the self-service key list only makes sense for IAM users.
 */
export function canManageOwnKeys(kind: ConnectionKind | undefined, isRoot: boolean, hasIdentity: boolean): boolean {
  return kind === 'gos3' && hasIdentity && !isRoot
}

/**
 * Creating a key for oneself (POST /whoami/keys) needs an explicit Allow of
 * admin:CreateOwnAccessKey; listing and deleting one's own keys do not.
 *
 * Servers that predate the `createOwnKeys` capability allowed self-service
 * creation unless it was explicitly denied, so a missing field counts as
 * allowed: the button then matches what that server enforces (and a 403
 * from it is shown as an error). Root never has IAM keys.
 */
export function canCreateOwnKeys(kind: ConnectionKind | undefined, isRoot: boolean, caps: Capabilities | null | undefined): boolean {
  if (kind !== 'gos3' || isRoot || !caps) return false
  return caps.createOwnKeys ?? true
}

/**
 * Probing a URL from the server (POST /discovery/probe) needs
 * admin:ProbeNetwork. Servers that predate the `probe` capability gated it
 * on admin:GetDiscovery, so a missing field falls back to `discovery`.
 */
export function canProbe(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): boolean {
  if (kind !== 'gos3' || !caps) return false
  return caps.probe ?? caps.discovery
}

/**
 * Policies / settings pages:
 * - manage:      editable (admin:PutPolicy / admin:UpdateConfig)
 * - readonly:    the identity has some admin rights; reading may be allowed
 *                (ListPolicies / GetConfig have no dedicated capability, so
 *                the page tries and shows AccessDenied if it is not)
 * - unavailable: generic S3, or no admin rights at all
 */
export type ManagePageMode = 'manage' | 'readonly' | 'unavailable'

function managePageMode(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined, cap: CapabilityName): ManagePageMode {
  if (kind !== 'gos3') return 'unavailable'
  if (caps?.[cap]) return 'manage'
  return caps?.admin ? 'readonly' : 'unavailable'
}

export function policiesPageMode(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): ManagePageMode {
  return managePageMode(kind, caps, 'managePolicies')
}

export function settingsPageMode(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): ManagePageMode {
  return managePageMode(kind, caps, 'manageConfig')
}

/** The authorization simulator (admin:SimulatePolicy) is offered to IAM managers. */
export function canSimulate(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): boolean {
  return hasCapability(kind, caps, 'managePolicies') || hasCapability(kind, caps, 'manageUsers')
}

/** Force delete (all versions, uploads and configs) needs admin:ForceDeleteBucket. */
export function canForceDeleteBucket(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): boolean {
  return hasCapability(kind, caps, 'manageBuckets')
}

/**
 * The server Storage page and the bucket Placement tab need
 * admin:ListBackings. Servers without storage backings do not report
 * `viewStorage`, so a missing field means false.
 */
export function canViewStorage(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): boolean {
  return hasCapability(kind, caps, 'viewStorage')
}

/** Changing replication, moving, promoting and resyncing need admin:UpdateBucketPlacement. */
export function canManageStorage(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): boolean {
  return hasCapability(kind, caps, 'manageStorage')
}

/**
 * Bucket compression settings (admin API GET/PUT/DELETE
 * /buckets/{name}/compression):
 * - manage:      admin:PutBucketCompression allowed at service level
 * - try:         not allowed server-wide. A grant scoped to the bucket
 *                (arn:gos3:admin:::bucket/<name>) does not show in the
 *                capabilities, so the controls stay usable and the
 *                server's 403 is shown, like the other bucket tabs do
 * - unavailable: generic S3, or a gos3 server without compression support
 *                (its /whoami has no manageCompression)
 */
export type BucketCompressionAccess = 'manage' | 'try' | 'unavailable'

export function bucketCompressionAccess(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): BucketCompressionAccess {
  if (kind !== 'gos3' || !caps || caps.manageCompression === undefined) return 'unavailable'
  return caps.manageCompression ? 'manage' : 'try'
}

/** Changing bucket compression is known to be allowed (admin:PutBucketCompression). */
export function canManageCompression(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): boolean {
  return bucketCompressionAccess(kind, caps) === 'manage'
}

/** Bucket usage / owner / public flag come from the gos3 admin API. */
export function canShowBucketUsage(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): boolean {
  return kind === 'gos3' && !!caps?.admin
}

export type BucketSettingsTab =
  | 'overview'
  | 'versioning'
  | 'policy'
  | 'cors'
  | 'lifecycle'
  | 'tags'
  | 'encryption'
  | 'compression'
  | 'object-lock'
  | 'access'
  | 'placement'
  | 'danger'

export interface BucketTabDef {
  value: BucketSettingsTab
  label: string
  icon: string
}

/**
 * Bucket settings tabs. Every tab but "compression" maps to plain S3 bucket
 * sub-resources, so they are shown for gos3 and generic S3 alike;
 * "compression" is a gos3 admin API feature (see bucketTabs). Per-bucket
 * permissions are not knowable up front and each tab handles AccessDenied.
 */
export const BUCKET_TABS: readonly BucketTabDef[] = [
  { value: 'overview', label: 'Overview', icon: 'i-lucide-info' },
  { value: 'versioning', label: 'Versioning', icon: 'i-lucide-history' },
  { value: 'policy', label: 'Policy', icon: 'i-lucide-file-lock' },
  { value: 'cors', label: 'CORS', icon: 'i-lucide-globe' },
  { value: 'lifecycle', label: 'Lifecycle', icon: 'i-lucide-timer' },
  { value: 'tags', label: 'Tags', icon: 'i-lucide-tags' },
  { value: 'encryption', label: 'Encryption', icon: 'i-lucide-lock' },
  { value: 'compression', label: 'Compression', icon: 'i-lucide-file-archive' },
  { value: 'object-lock', label: 'Object lock', icon: 'i-lucide-shield-alert' },
  { value: 'access', label: 'Access', icon: 'i-lucide-key-round' },
  { value: 'danger', label: 'Danger zone', icon: 'i-lucide-triangle-alert' }
]

/** Tabs that only some servers offer, with the rule that decides. */
const TAB_AVAILABLE: Partial<Record<BucketSettingsTab, (kind: ConnectionKind | undefined, caps: Capabilities | null | undefined) => boolean>> = {
  compression: (kind, caps) => bucketCompressionAccess(kind, caps) !== 'unavailable'
}

/** The bucket settings tabs a connection offers (BUCKET_TABS minus gos3-only ones it lacks). */
export function bucketTabs(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): readonly BucketTabDef[] {
  return BUCKET_TABS.filter(t => TAB_AVAILABLE[t.value]?.(kind, caps) ?? true)
}

/** gos3 storage placement (admin:ListBackings), shown before the danger zone. */
export const PLACEMENT_TAB: Readonly<BucketTabDef> = Object.freeze({ value: 'placement', label: 'Placement', icon: 'i-lucide-hard-drive' })

/**
 * The bucket settings tabs for a connection: bucketTabs, plus Placement
 * for gos3 identities with viewStorage.
 */
export function bucketSettingsTabs(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): readonly BucketTabDef[] {
  const tabs = bucketTabs(kind, caps)
  if (!canViewStorage(kind, caps)) return tabs
  const i = tabs.findIndex(t => t.value === 'danger')
  return [...tabs.slice(0, i), PLACEMENT_TAB, ...tabs.slice(i)]
}

/** Normalises a `?tab=` query value to one of `tabs` (default overview). */
export function resolveBucketTab(v: unknown, tabs: readonly BucketTabDef[] = BUCKET_TABS): BucketSettingsTab {
  const s = Array.isArray(v) ? v[0] : v
  return tabs.find(t => t.value === s)?.value ?? 'overview'
}

/**
 * True while the identity of a gos3 connection is not known yet (status not
 * checked or being checked). Pages show a skeleton instead of a premature
 * "not allowed" state during that time.
 */
export function identityPending(kind: ConnectionKind | undefined, status: { state: string, whoami: unknown } | null | undefined): boolean {
  if (kind !== 'gos3') return false
  if (status?.whoami) return false
  return !status || status.state === 'unknown' || status.state === 'checking'
}
