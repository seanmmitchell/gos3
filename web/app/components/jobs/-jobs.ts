// Helpers for background jobs (Jobs page, JobsTable, JobProgress, the
// bucket overview's Jobs card): state badges, what a job copies, progress,
// ETA / mirror lag, the actions each state allows and the poll interval.
// Pure functions of the admin API types (docs/admin-api.md, "Jobs: copy and
// mirror"), so components and tests share the same rules.
//
// The file name starts with "-" so Nuxt does not register it as a component:
//
//   import { jobStateMeta } from '~/components/jobs/-jobs'
import type { CopyJobSpec, CopyStatus, Job, JobAction, JobKind, JobState, SnapshotJobStatus } from '~/types/admin'
import { formatSeconds } from '~/utils/format'

/** Poll interval of job lists while a job is busy (queued, running, or a mirror with pending changes). */
export const JOB_POLL_FAST_MS = 3000

/** Poll interval of job lists otherwise (mirrors wake up, retries start). */
export const JOB_POLL_SLOW_MS = 15000

type BadgeColor = 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'error' | 'neutral'

export interface JobStateMeta {
  label: string
  color: BadgeColor
  icon: string
  description: string
}

export const JOB_STATES: Readonly<Record<JobState, JobStateMeta>> = {
  queued: { label: 'Queued', color: 'neutral', icon: 'i-lucide-clock', description: 'Waiting for a free worker.' },
  running: { label: 'Running', color: 'info', icon: 'i-lucide-loader-circle', description: 'Working now.' },
  waiting: { label: 'Waiting', color: 'secondary', icon: 'i-lucide-hourglass', description: 'Between runs: a mirror waits for its next pass, a failed run for its retry.' },
  paused: { label: 'Paused', color: 'warning', icon: 'i-lucide-pause', description: 'Stopped; resume continues from the last checkpoint.' },
  succeeded: { label: 'Succeeded', color: 'success', icon: 'i-lucide-circle-check', description: 'Finished.' },
  failed: { label: 'Failed', color: 'error', icon: 'i-lucide-circle-x', description: 'Stopped by an error; resume retries from the last checkpoint.' },
  canceled: { label: 'Canceled', color: 'neutral', icon: 'i-lucide-ban', description: 'Canceled; what was copied stays.' }
}

/** Badge metadata of a job state (unknown states are neutral). */
export function jobStateMeta(state: JobState | string): JobStateMeta {
  return JOB_STATES[state as JobState] ?? { label: state || 'Unknown', color: 'neutral', icon: 'i-lucide-circle-help', description: '' }
}

/** States in the order the filter offers them. */
export const JOB_STATE_ORDER: readonly JobState[] = ['running', 'queued', 'waiting', 'paused', 'failed', 'succeeded', 'canceled']

export const JOB_KINDS: Readonly<Record<JobKind, { label: string, icon: string }>> = {
  'copy': { label: 'Copy', icon: 'i-lucide-copy' },
  'mirror': { label: 'Mirror', icon: 'i-lucide-refresh-cw' },
  'restic-snapshot': { label: 'Snapshot', icon: 'i-lucide-camera' },
  'restic-restore': { label: 'Restore', icon: 'i-lucide-archive-restore' },
  'restic-prune': { label: 'Prune', icon: 'i-lucide-scissors' }
}

/** Label and icon of a job kind (unknown kinds are shown as sent). */
export function jobKindMeta(kind: JobKind | string): { label: string, icon: string } {
  return JOB_KINDS[kind as JobKind] ?? { label: kind || 'Job', icon: 'i-lucide-briefcase' }
}

/** True for succeeded, failed and canceled jobs (only those can be deleted). */
export function jobFinished(state: JobState | string): boolean {
  return state === 'succeeded' || state === 'failed' || state === 'canceled'
}

/** True for copy and mirror jobs. */
export function isCopyJob(job: Pick<Job, 'kind'>): boolean {
  return job.kind === 'copy' || job.kind === 'mirror'
}

function num(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : 0
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : ''
}

function obj(v: unknown): Record<string, unknown> | undefined {
  return typeof v === 'object' && v !== null && !Array.isArray(v) ? v as Record<string, unknown> : undefined
}

/** The copy status of a copy or mirror job (missing counters are 0), else undefined. */
export function copyStatusOf(job: Job): CopyStatus | undefined {
  if (!isCopyJob(job)) return undefined
  const s = obj(job.status) ?? {}
  return {
    ...(s as Partial<CopyStatus>),
    keysScanned: num(s.keysScanned),
    keysCopied: num(s.keysCopied),
    versionsCopied: num(s.versionsCopied),
    bytesCopied: num(s.bytesCopied),
    bytesDone: num(s.bytesDone),
    keysDeleted: num(s.keysDeleted),
    conflicts: num(s.conflicts),
    totalKeys: num(s.totalKeys),
    totalBytes: num(s.totalBytes),
    bytesPerSecond: num(s.bytesPerSecond),
    etaSeconds: num(s.etaSeconds)
  }
}

/** The status of a restic job (missing counters are 0), else undefined. */
export function snapshotStatusOf(job: Job): SnapshotJobStatus | undefined {
  if (isCopyJob(job) || !job.kind.startsWith('restic-')) return undefined
  const s = obj(job.status) ?? {}
  return {
    keys: num(s.keys),
    bytes: num(s.bytes),
    materialized: num(s.materialized),
    partials: num(s.partials),
    forgotten: num(s.forgotten),
    snapshot: str(s.snapshot) || undefined,
    repository: str(s.repository) || undefined
  }
}

/** One end of a job: a bucket and an optional prefix ("photos/2024/"). */
export interface JobEnd {
  bucket: string
  prefix: string
}

/** "photos" or "photos/2024/" ('' for no bucket). */
export function formatJobEnd(e: JobEnd | undefined): string {
  if (!e?.bucket) return ''
  return e.prefix ? `${e.bucket}/${e.prefix.replace(/^\/+/, '')}` : e.bucket
}

/** What a job reads and writes, for the table and its filters. */
export interface JobDescription {
  kindLabel: string
  icon: string
  source?: JobEnd
  target?: JobEnd
  /** "photos → photos-bak", "Snapshot of photos", … */
  title: string
  /** Every bucket the job touches (for the bucket filter). */
  buckets: string[]
}

function end(v: unknown): JobEnd | undefined {
  const o = obj(v)
  const bucket = str(o?.bucket)
  return bucket ? { bucket, prefix: str(o?.prefix) } : undefined
}

/**
 * Source and target of a job, from its normalized spec, falling back to its
 * labels ("source" / "target"; restic jobs may only name a "bucket").
 */
export function describeJob(job: Job): JobDescription {
  const kind = jobKindMeta(job.kind)
  const spec = obj(job.spec) ?? {}
  const labels = job.labels ?? {}
  const labelEnd = (name: string): JobEnd | undefined => (labels[name] ? { bucket: labels[name]!, prefix: '' } : undefined)
  const source = end(spec.source) ?? labelEnd('source') ?? labelEnd('bucket') ?? (str(spec.bucket) ? { bucket: str(spec.bucket), prefix: '' } : undefined)
  let target = end(spec.target) ?? labelEnd('target')
  let title: string
  switch (job.kind) {
    case 'restic-snapshot':
      title = source ? `Snapshot of ${formatJobEnd(source)}` : 'Snapshot'
      target = undefined
      break
    case 'restic-prune':
      title = source ? `Prune snapshots of ${source.bucket}` : 'Prune snapshots'
      target = undefined
      break
    case 'restic-restore': {
      // A restore reads a snapshot of its bucket and writes the target.
      title = target ? `Restore ${source ? `${source.bucket} ` : ''}snapshot → ${formatJobEnd(target)}` : 'Restore'
      break
    }
    default:
      title = !source && !target ? kind.label : `${formatJobEnd(source) || '?'} → ${formatJobEnd(target) || '?'}`
  }
  const buckets = [...new Set([source?.bucket, target?.bucket, labels.source, labels.target, labels.bucket].filter((b): b is string => !!b))]
  return { kindLabel: kind.label, icon: kind.icon, source, target, title, buckets }
}

/** The copy or mirror spec of a job, when it has one. */
export function copySpecOf(job: Job): CopyJobSpec | undefined {
  if (!isCopyJob(job)) return undefined
  const spec = obj(job.spec)
  return spec && end(spec.source) && end(spec.target) ? spec as unknown as CopyJobSpec : undefined
}

export interface JobProgress {
  /** 0–100, or null when the total is unknown (indeterminate). */
  percent: number | null
  /** Source bytes examined in the full pass. */
  done: number
  /** Total bytes (0 = unknown). */
  total: number
}

/**
 * Byte progress of a copy or mirror job from bytesDone / totalBytes, or
 * null for other jobs. A completed full pass shows 100%, independently
 * of unresolved work and of current mirror freshness.
 */
export function jobProgress(job: Job): JobProgress | null {
  const s = copyStatusOf(job)
  if (!s) return null
  if (job.kind === 'mirror' && job.state === 'running' && job.phase === 'watching') return { percent: null, done: 0, total: 0 }
  const done = s.bytesDone
  const total = s.totalsKnown === true && done <= s.totalBytes ? s.totalBytes : 0
  if (s.passComplete) return { percent: 100, done, total }
  if (s.totalsKnown !== true || done > s.totalBytes) return { percent: null, done, total: 0 }
  // Zero-byte objects still have key work; an empty pass completes only
  // when the backend says so. Legacy servers cannot qualify their totals.
  if (!total) {
    const keys = num(s.sourceKeysDone)
    return { percent: s.totalKeys > 0 && keys <= s.totalKeys ? Math.round(keys / s.totalKeys * 1000) / 10 : null, done, total }
  }
  return { percent: Math.max(0, Math.min(100, Math.round((done / total) * 1000) / 10)), done, total }
}

/**
 * A remaining time for humans: 3725 → "1h 2m", 42 → "42s", under a second
 * → "< 1s"; '' when unknown (0, negative, not a number).
 */
export function formatEta(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds) || seconds <= 0) return ''
  if (seconds < 1) return '< 1s'
  // Two units are precise enough for an estimate.
  return formatSeconds(Math.round(seconds)).split(' ').slice(0, 2).join(' ')
}

/**
 * The timing line of a job: the ETA of a running copy, the lag and pending
 * changes of a mirror, or '' when there is nothing to say.
 */
export function jobTiming(job: Job): string {
  const s = copyStatusOf(job)
  if (!s) return ''
  if (job.kind === 'mirror') {
    const pending = num(s.pending)
    const lag = formatEta(s.lagSeconds)
    const parts: string[] = []
    if (pending > 0) parts.push(`${pending.toLocaleString()} pending${lag ? ` · lag ${lag}` : ''}`)
    if (num(s.unresolved) > 0) parts.push(`${num(s.unresolved).toLocaleString()} unresolved keys`)
    // Older servers have only pass conflict counters. They cannot assert
    // recovery, so qualify those reports instead of calling them current.
    if (s.unresolved === undefined && s.conflicts > 0) parts.push(`${s.conflicts.toLocaleString()} conflicts reported`)
    if (job.lastError) parts.push('Last run failed')
    if (parts.length) return parts.join(' · ')
    if (job.state === 'running') return job.phase === 'watching' ? 'Processing source changes' : 'Full comparison in progress'
    if (s.checkRequired) return 'Full comparison required'
    if (job.state === 'queued') return 'Comparison queued'
    if (job.state === 'paused') return 'Mirror paused'
    if (job.state === 'failed' || job.state === 'canceled') return ''
    if (s.lastPassAt) return `No known pending changes · last full comparison ${s.lastPassAt}`
    return 'No full comparison reported'
  }
  if (job.state === 'running') {
    const eta = formatEta(s.etaSeconds)
    if (eta) return `about ${eta} left`
  }
  return ''
}

/** The actions a job's state allows, in menu order. */
export function jobActions(job: Pick<Job, 'state'>): (JobAction | 'delete')[] {
  switch (job.state) {
    case 'queued': return ['pause', 'cancel']
    case 'running': return ['pause', 'cancel']
    case 'waiting': return ['run', 'pause', 'cancel']
    case 'paused': return ['resume', 'cancel']
    case 'failed': return ['resume', 'delete']
    case 'succeeded':
    case 'canceled': return ['delete']
    default: return []
  }
}

export const JOB_ACTIONS: Readonly<Record<JobAction | 'delete', { label: string, icon: string, done: string }>> = {
  run: { label: 'Run now', icon: 'i-lucide-play', done: 'queued' },
  pause: { label: 'Pause', icon: 'i-lucide-pause', done: 'paused' },
  resume: { label: 'Resume', icon: 'i-lucide-play', done: 'resumed' },
  cancel: { label: 'Cancel', icon: 'i-lucide-circle-x', done: 'canceled' },
  delete: { label: 'Delete', icon: 'i-lucide-trash-2', done: 'deleted' }
}

/** A job counts as busy (fast polling) while queued, running, or a mirror waiting with pending changes. */
export function jobBusy(job: Job): boolean {
  if (job.state === 'queued' || job.state === 'running') return true
  return job.state === 'waiting' && num(copyStatusOf(job)?.pending) > 0
}

/** Poll interval of a job list: JOB_POLL_FAST_MS while any job is busy, else JOB_POLL_SLOW_MS. */
export function jobPollInterval(jobs: readonly Job[] | null | undefined): number {
  return (jobs ?? []).some(jobBusy) ? JOB_POLL_FAST_MS : JOB_POLL_SLOW_MS
}

export interface JobFilter {
  /** '' = any. */
  kind?: string
  /** '' = any; "active" = not finished. */
  state?: string
  /** Bucket name (substring of a source or target bucket), '' = any. */
  bucket?: string
}

/** Filters a job list (kind, state or "active", bucket substring). */
export function filterJobs(jobs: readonly Job[], f: JobFilter): Job[] {
  const bucket = (f.bucket ?? '').trim().toLowerCase()
  return jobs.filter((j) => {
    if (f.kind && j.kind !== f.kind) return false
    if (f.state === 'active' ? jobFinished(j.state) : f.state && j.state !== f.state) return false
    if (bucket && !describeJob(j).buckets.some(b => b.toLowerCase().includes(bucket))) return false
    return true
  })
}

/** Jobs sorted newest first (by creation time, then ID). */
export function sortJobs(jobs: readonly Job[]): Job[] {
  return [...jobs].sort((a, b) => (Date.parse(b.createdAt) || 0) - (Date.parse(a.createdAt) || 0) || b.id.localeCompare(a.id))
}

/** Replaces a job in a list by ID (adds it in front when it is new). */
export function upsertJob(jobs: readonly Job[], job: Job): Job[] {
  const i = jobs.findIndex(j => j.id === job.id)
  if (i < 0) return [job, ...jobs]
  const out = [...jobs]
  out[i] = job
  return out
}
