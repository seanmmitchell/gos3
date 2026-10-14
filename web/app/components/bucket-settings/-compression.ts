// Bucket compression and the administrator's server-wide mode
// (config compression.mode, admin API /buckets/{name}/compression). The
// server refuses (409) a bucket setting that contradicts the mode: enabling
// while "disabled", disabling while "enforced". See docs/admin-api.md.
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { CompressionMode } from '~/types/admin'

/** Label and explanation of each compression mode (server settings and bucket tab). */
export const COMPRESSION_MODE_TEXT: Readonly<Record<CompressionMode, { label: string, description: string }>> = {
  optional: { label: 'Optional', description: 'Each bucket decides; compression is off unless enabled for the bucket.' },
  enforced: { label: 'Enforced', description: 'Every bucket compresses new objects. Buckets cannot opt out.' },
  disabled: { label: 'Disabled', description: 'Nothing new is compressed and buckets cannot enable it. Objects stored compressed stay readable.' }
}

/** Text of a server mode; an unknown mode (newer server) is shown as is. */
export function compressionModeText(mode: string | null | undefined): { label: string, description: string } {
  const known = COMPRESSION_MODE_TEXT[mode as CompressionMode] as { label: string, description: string } | undefined
  return known ?? { label: mode || 'Unknown', description: 'Set by the server administrator.' }
}

/**
 * Why flipping the bucket's setting (currently `enabled`) would be refused
 * under `serverMode`, or undefined when the server accepts the change.
 */
export function compressionConflict(serverMode: string | null | undefined, enabled: boolean | null | undefined): string | undefined {
  if (enabled === null || enabled === undefined) return undefined
  if (serverMode === 'enforced' && enabled) return 'The administrator enforces compression, so it cannot be turned off for this bucket.'
  if (serverMode === 'disabled' && !enabled) return 'The administrator has disabled compression, so it cannot be turned on for this bucket.'
  return undefined
}
