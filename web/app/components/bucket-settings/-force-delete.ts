// Force delete of a bucket (admin API DELETE /buckets/{name}?force=true)
// and object lock: the server answers 409 before deleting anything when
// versions are under legal hold or COMPLIANCE retention (never deletable),
// or under GOVERNANCE retention without bypassGovernanceRetention=true
// (deletable with s3:BypassGovernanceRetention). See docs/admin-api.md.
//
// File name starts with "-" so Nuxt does not register it as a component.
import { AdminError } from '~/utils/admin/client'

export interface ForceDeleteBlock {
  /** Only GOVERNANCE retention blocks the delete: retrying with bypass may work. */
  governance: boolean
  /** The server's explanation (names the bucket, counts and example keys). */
  message: string
}

/** Why a force delete was refused by object lock, or undefined for any other error. */
export function forceDeleteBlock(e: unknown): ForceDeleteBlock | undefined {
  if (!(e instanceof AdminError) || e.status !== 409) return undefined
  if (!/legal hold|COMPLIANCE|GOVERNANCE/.test(e.message)) return undefined
  return { governance: /GOVERNANCE retention/.test(e.message) && !/legal hold|COMPLIANCE/.test(e.message), message: e.message }
}

/** Title of the error shown for a force delete that object lock refused. */
export const FORCE_DELETE_LOCKED_TITLE = 'Object lock prevents deleting this bucket'

/** Description of the "bypass governance retention" option. */
export const BYPASS_GOVERNANCE_HELP = 'Also delete versions under GOVERNANCE retention. Needs s3:BypassGovernanceRetention on those objects. Legal holds and COMPLIANCE retention can never be bypassed.'
