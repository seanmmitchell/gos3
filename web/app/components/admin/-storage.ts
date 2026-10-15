// Helpers for storage backings and bucket placement (server Storage page,
// bucket Placement tab, bucket list chips). Pure functions of the admin API
// types (docs/admin-api.md, "Storage backings and bucket placement"), so
// pages and tests share the same rules.
//
// The file name starts with "-" so Nuxt does not register it as a component:
//
//   import { placementBusy } from '~/components/admin/-storage'
import type { Backing, MemberState, Placement, PlacementMember, PlacementSummary, ReplicationMode } from '~/types/admin'

/** Poll interval of the Placement tab while copies run or a move is in progress. */
export const PLACEMENT_POLL_MS = 3000

/** Auto-refresh interval of the server Storage page. */
export const BACKINGS_REFRESH_MS = 10000

type BadgeColor = 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'error' | 'neutral'

export interface ModeOption {
  value: ReplicationMode
  label: string
  /** Short form for chips ("AP", "AA"; "" for single). */
  short: string
  description: string
}

export const MODE_OPTIONS: readonly ModeOption[] = [
  { value: 'single', label: 'Single copy', short: '', description: 'One copy on the primary backing, no replicas.' },
  { value: 'active-passive', label: 'Active-passive', short: 'AP', description: 'Writes go to the primary and are copied to the replicas in the background.' },
  { value: 'active-active', label: 'Active-active', short: 'AA', description: 'Every write goes to the primary and all replicas before it succeeds.' }
]

export function modeOption(mode: ReplicationMode | string | undefined): ModeOption | undefined {
  return MODE_OPTIONS.find(m => m.value === mode)
}

/** Human label of a replication mode (unknown modes are shown as sent). */
export function modeLabel(mode: ReplicationMode | string | undefined): string {
  return modeOption(mode)?.label ?? (mode || 'Unknown')
}

export interface StateMeta {
  label: string
  color: BadgeColor
  icon: string
  description: string
}

export const MEMBER_STATES: Readonly<Record<MemberState, StateMeta>> = {
  'in-sync': { label: 'In sync', color: 'success', icon: 'i-lucide-circle-check', description: 'Has every change.' },
  'syncing': { label: 'Syncing', color: 'info', icon: 'i-lucide-loader-circle', description: 'A full copy is running.' },
  'lagging': { label: 'Lagging', color: 'warning', icon: 'i-lucide-clock', description: 'Changes are pending and copied in the background.' },
  'degraded': { label: 'Degraded', color: 'error', icon: 'i-lucide-triangle-alert', description: 'Copying failed and is retried with back-off.' },
  'offline': { label: 'Offline', color: 'error', icon: 'i-lucide-unplug', description: 'The backing is unhealthy.' }
}

/** Badge metadata of a member state (unknown states are neutral). */
export function memberStateMeta(state: MemberState | string): StateMeta {
  return MEMBER_STATES[state as MemberState] ?? { label: state || 'Unknown', color: 'neutral', icon: 'i-lucide-circle-help', description: '' }
}

export const ROLE_LABELS: Readonly<Record<string, string>> = {
  'primary': 'Primary',
  'replica': 'Replica',
  'move-target': 'Move target'
}

export function roleLabel(role: string): string {
  return ROLE_LABELS[role] ?? role
}

/**
 * True while the placement changes on its own (a full copy running, pending
 * changes, a member retrying, or a move), so the tab keeps polling.
 */
export function placementBusy(p: Placement | null | undefined): boolean {
  if (!p) return false
  if (p.move) return true
  return (p.members ?? []).some(m => m.state === 'syncing' || m.state === 'lagging' || !!m.progress || m.pending > 0)
}

/** The members holding a copy of the bucket (primary and replicas, no move target). */
export function copyMembers(p: Placement | null | undefined): PlacementMember[] {
  return (p?.members ?? []).filter(m => m.role === 'primary' || m.role === 'replica')
}

/** Current replica backing names. */
export function replicaNames(p: Placement | null | undefined): string[] {
  return (p?.members ?? []).filter(m => m.role === 'replica').map(m => m.backing)
}

export interface BackingChoice {
  value: string
  label: string
  healthy: boolean
  /** Why it cannot be chosen (empty when it can). */
  disabledReason?: string
}

/** Why native buckets can neither be replicated nor moved (Placement tab). */
export const NATIVE_PLACEMENT_NOTE = 'Native buckets keep their data in their remote bucket; copy them with a copy job instead.'

/**
 * Why a backing cannot hold a copy of a bucket, or '' when it can. Copies on
 * s3 backings are always wrapped buckets, so an s3 backing needs a container
 * (it does not offer the wrapped kind without one). A move names the
 * container of its copy; a replica goes to the default container, so the
 * backing needs one (`purpose` replica).
 */
export function copyRefusal(b: Pick<Backing, 'kind' | 'kinds' | 'account'>, purpose: 'replica' | 'move' = 'move'): string {
  if (b.kind !== 's3') return ''
  const kinds = b.kinds ?? b.account?.kinds
  if (kinds && !kinds.includes('wrapped')) return 'no container'
  if (b.account && !b.account.containers?.length) return 'no container'
  if (purpose === 'replica' && b.account && !b.account.defaultContainer) return 'no default container'
  return ''
}

/**
 * Backings offered as replicas: every backing except the primary and s3
 * backings without a (default) container. Unhealthy ones stay selectable
 * (the copy starts once they recover) but are marked.
 */
export function replicaChoices(backings: readonly Backing[], p: Placement): BackingChoice[] {
  const names = new Set(backings.map(b => b.name))
  const current = new Set(replicaNames(p))
  const list: BackingChoice[] = backings
    // Backings that cannot hold a copy are left out (kept when one somehow is a replica).
    .filter(b => b.name !== p.primary && (!copyRefusal(b, 'replica') || current.has(b.name)))
    .map(b => ({ value: b.name, label: b.name, healthy: b.healthy }))
  // Replicas on backings the listing did not return (ListBackings denied, or a backing removed from the config).
  for (const m of p.members ?? []) {
    if (m.role === 'replica' && !names.has(m.backing)) list.push({ value: m.backing, label: m.backing, healthy: m.healthy })
  }
  return list.sort((a, b) => a.label.localeCompare(b.label))
}

/** Validation of the replication form; returns an error message or ''. */
export function validateReplication(mode: ReplicationMode, replicas: readonly string[]): string {
  if (mode === 'single') return replicas.length ? 'A single-copy bucket has no replicas.' : ''
  return replicas.length ? '' : 'Choose at least one replica backing.'
}

/** The replicas a replication change removes (their copy is deleted). */
export function removedReplicas(current: readonly string[], next: readonly string[]): string[] {
  return current.filter(r => !next.includes(r))
}

/** The replicas a replication change adds (each starts a full copy). */
export function addedReplicas(current: readonly string[], next: readonly string[]): string[] {
  return next.filter(r => !current.includes(r))
}

/** True when the form differs from the placement. */
export function replicationChanged(p: Placement, mode: ReplicationMode, replicas: readonly string[]): boolean {
  const cur = replicaNames(p)
  return p.mode !== mode || cur.length !== replicas.length || cur.some(r => !replicas.includes(r))
}

/**
 * Move targets: backings that hold no copy of the bucket (and are not a move
 * target already). Backings that cannot hold a copy (s3 backings without a
 * container) and unhealthy ones are listed but disabled.
 */
export function moveTargets(backings: readonly Backing[], p: Placement): BackingChoice[] {
  const members = new Set((p.members ?? []).map(m => m.backing))
  return backings
    .filter(b => !members.has(b.name))
    .map(b => ({ value: b.name, label: b.name, healthy: b.healthy, disabledReason: copyRefusal(b) || (b.healthy ? undefined : 'unhealthy') }))
    .sort((a, b) => a.label.localeCompare(b.label))
}

export interface PromoteAdvice {
  /** A normal promote is expected to be refused or to block: offer force. */
  suggestForce: boolean
  /** Why force is suggested ('' when it is not). */
  reason: string
  /** A normal promote is refused by the server (initial copy not finished). */
  normalRefused: boolean
}

/**
 * Whether promoting `backing` should offer `force`: when the replica is not
 * in sync (its pending changes would be lost) or the primary is offline (a
 * normal promote copies pending changes from it first).
 */
export function promoteAdvice(p: Placement, backing: string): PromoteAdvice {
  const members = p.members ?? []
  const replica = members.find(m => m.backing === backing && m.role === 'replica')
  const primary = members.find(m => m.role === 'primary')
  const primaryDown = !!primary && (!primary.healthy || primary.state === 'offline')
  const normalRefused = !!replica && replica.state === 'syncing'
  const reasons: string[] = []
  if (primaryDown) reasons.push(`the primary ${primary!.backing} is offline`)
  if (replica && replica.state !== 'in-sync') {
    reasons.push(replica.state === 'syncing'
      ? `${backing} has not finished its initial copy`
      : `${backing} is ${memberStateMeta(replica.state).label.toLowerCase()}${replica.pending ? ` (${replica.pending} pending change${replica.pending === 1 ? '' : 's'})` : ''}`)
  }
  return { suggestForce: reasons.length > 0, reason: reasons.join(' and '), normalRefused }
}

export interface PlacementChip {
  label: string
  color: BadgeColor
  icon: string
  /** Tooltip / accessible description. */
  title: string
}

/**
 * Whether the bucket list should show placement at all: only when some
 * bucket is replicated, moving or on another backing than the others (a
 * server with one backing would show "default" on every row).
 */
export function placementWorthShowing(list: readonly (PlacementSummary | null | undefined)[]): boolean {
  const present = list.filter((p): p is PlacementSummary => !!p)
  if (!present.length) return false
  const first = present[0]!.primary
  return present.some(p => p.mode !== 'single' || p.moving || (p.replicas?.length ?? 0) > 0 || p.primary !== first)
}

/**
 * Chips for a bucket's placement summary: primary backing, mode × copies,
 * moving. `describe` may give the primary backing's kind (label + icon, e.g.
 * from WhoAmI.backings via backingKindInfo).
 */
export function placementChips(p: PlacementSummary | null | undefined, describe?: (backing: string) => { label: string, icon: string } | undefined): PlacementChip[] {
  if (!p) return []
  const replicas = p.replicas ?? []
  const kind = describe?.(p.primary)
  const chips: PlacementChip[] = [{
    label: p.primary,
    color: 'neutral',
    icon: kind?.icon ?? 'i-lucide-hard-drive',
    title: kind ? `Primary copy on ${p.primary} · ${kind.label}` : `Primary copy on ${p.primary}`
  }]
  const opt = modeOption(p.mode)
  if (p.mode !== 'single') {
    const copies = replicas.length + 1
    chips.push({
      label: `${opt?.short || p.mode} ×${copies}`,
      color: p.mode === 'active-active' ? 'primary' : 'info',
      icon: 'i-lucide-copy',
      title: `${opt?.label ?? p.mode}: ${copies} copies (replicas on ${replicas.join(', ') || 'none'})`
    })
  }
  if (p.moving) chips.push({ label: 'moving', color: 'warning', icon: 'i-lucide-arrow-right-left', title: 'A move to another backing is in progress' })
  return chips
}

/** "120 keys · 130 versions · 1.0 MiB" for a running copy. */
export function formatProgress(pr: { keys: number, versions: number, bytes: number } | null | undefined, fmtNumber: (n: number) => string, fmtBytes: (n: number) => string): string {
  if (!pr) return ''
  return `${fmtNumber(pr.keys)} key${pr.keys === 1 ? '' : 's'} · ${fmtNumber(pr.versions)} version${pr.versions === 1 ? '' : 's'} · ${fmtBytes(pr.bytes)}`
}

/** Backings sorted for display: default first, then by name. */
export function sortBackings(list: readonly Backing[] | null | undefined): Backing[] {
  return [...(list ?? [])].sort((a, b) => Number(b.default) - Number(a.default) || a.name.localeCompare(b.name))
}
