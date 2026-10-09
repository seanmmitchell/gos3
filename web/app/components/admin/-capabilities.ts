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
  managePolicies: false,
  manageBuckets: false,
  discovery: false,
  scan: false
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
 * Users page:
 * - manage:      full IAM user management (admin:CreateUser)
 * - self:        "My access keys" only (/whoami/keys)
 * - unavailable: generic S3 (no admin API)
 */
export type UsersPageMode = 'manage' | 'self' | 'unavailable'

export function usersPageMode(kind: ConnectionKind | undefined, caps: Capabilities | null | undefined): UsersPageMode {
  if (kind !== 'gos3') return 'unavailable'
  return caps?.manageUsers ? 'manage' : 'self'
}

/**
 * Root has no IAM access keys (its credentials come from the server config),
 * so the self-service key list only makes sense for IAM users.
 */
export function canManageOwnKeys(kind: ConnectionKind | undefined, isRoot: boolean, hasIdentity: boolean): boolean {
  return kind === 'gos3' && hasIdentity && !isRoot
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
  | 'object-lock'
  | 'access'
  | 'danger'

export interface BucketTabDef {
  value: BucketSettingsTab
  label: string
  icon: string
}

/**
 * Bucket settings tabs. Every tab maps to plain S3 bucket sub-resources, so
 * all of them are shown for gos3 and generic S3 alike; per-bucket
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
  { value: 'object-lock', label: 'Object lock', icon: 'i-lucide-shield-alert' },
  { value: 'access', label: 'Access', icon: 'i-lucide-key-round' },
  { value: 'danger', label: 'Danger zone', icon: 'i-lucide-triangle-alert' }
]

/** Normalises a `?tab=` query value to a known tab (default overview). */
export function resolveBucketTab(v: unknown): BucketSettingsTab {
  const s = Array.isArray(v) ? v[0] : v
  return BUCKET_TABS.find(t => t.value === s)?.value ?? 'overview'
}
