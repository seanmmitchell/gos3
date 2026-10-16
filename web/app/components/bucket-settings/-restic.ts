// Helpers for the bucket's "Backup & archive" tab (ResticTab): the
// per-version archive toggle, recovery snippets for the restic CLI (bash and
// PowerShell, with every value quoted for its shell), the whole-bucket
// snapshot schedule form (validation, retention presets, request body) and
// the restore dialog (docs/admin-api.md, "Object encryption providers" and
// "Whole-bucket restic snapshots").
//
// The file name starts with "-" so Nuxt does not register it as a component.
import type {
  BucketEncryptionProvider,
  BucketEncryptionProviderInput,
  CopyTargetCreate,
  EncryptionProviderName,
  RepositoryInfo,
  ResticArchiveCopy,
  ResticCredentials,
  ResticTags,
  RestoreSnapshotRequest,
  Snapshot,
  SnapshotConfig,
  SnapshotRetention
} from '~/types/admin'
import { durationText, formatGoDuration, parseGoDuration } from '~/utils/durations'
import { validateBucketName } from '~/utils/paths'

// --- archive toggle ------------------------------------------------------------------

/** Every object version becomes a restic snapshot: shown next to the toggle. */
export const ARCHIVE_COST_WARNING = 'Every object version written becomes a restic snapshot in the bucket’s repository. Writes take longer, the repository grows with every version, and downloads restore from the archive. Snapshots stay after objects are deleted.'

/** True when the bucket archives every write in restic (provider restic, scope all). */
export function archiveEnabled(doc: Pick<BucketEncryptionProvider, 'provider' | 'scope'> | null | undefined): boolean {
  return doc?.provider === 'restic' && doc.scope === 'all'
}

/** Why native buckets offer no encryption provider. */
export const NATIVE_NO_PROVIDER = 'Not available: native buckets of S3 backings store objects as they are, without an encryption provider.'

const ALL_PROVIDERS: readonly EncryptionProviderName[] = ['AES256', 'restic']

/**
 * The providers the bucket can use now. The server always reports
 * `available` (an empty list: none, e.g. for native buckets); only older
 * servers omit it, and then every provider is offered. Native buckets of
 * s3 backings offer none whatever the document says.
 */
export function availableProviders(doc: Pick<BucketEncryptionProvider, 'available'> | null | undefined, kind?: string): readonly EncryptionProviderName[] {
  if (kind === 'native') return []
  return doc?.available ?? ALL_PROVIDERS
}

/** Whether the archive can be turned on (or off: both need restic), and why not. */
export function archiveAvailability(doc: Pick<BucketEncryptionProvider, 'available'> | null | undefined, kind?: string): { available: boolean, reason: string } {
  if (!doc) return { available: false, reason: '' }
  if (kind === 'native') return { available: false, reason: NATIVE_NO_PROVIDER }
  if (availableProviders(doc, kind).includes('restic')) return { available: true, reason: '' }
  return {
    available: false,
    reason: 'Restic is not available for this bucket: it must be installed on the server, and every copy of the bucket must be on a filesystem backing or in a wrapped bucket of an S3 backing with an encryption key.'
  }
}

/**
 * True when the bucket has no restic repository of its own, so
 * whole-bucket snapshots need a backing: native buckets, and buckets whose
 * primary copy reports "no repository" (e.g. on a memory backing). A
 * repository that is merely not created yet (no restic write so far) still
 * counts as the bucket's own.
 */
export function bucketLacksRepository(archive: readonly ResticArchiveCopy[] | null | undefined, kind?: string): boolean {
  if (kind === 'native') return true
  const list = archive ?? []
  const primary = list.find(c => c.role === 'primary') ?? list[0]
  return !!primary && !primary.info && /no repository/i.test(primary.error ?? '')
}

/**
 * PUT encryption-provider body of the archive toggle: on → restic for
 * every write; off → restic for SSE writes only (the provider stays, so
 * objects already archived keep being readable and SSE uploads keep using
 * restic).
 */
export function archiveToggleBody(on: boolean): BucketEncryptionProviderInput {
  return on ? { provider: 'restic', scope: 'all' } : { provider: 'restic', scope: 'sse' }
}

// --- shell quoting --------------------------------------------------------------------

export type Shell = 'bash' | 'powershell'

const BASH_SAFE = /^[A-Za-z0-9_@%+=:,./-]+$/

/** A word for bash (POSIX sh): left as is when it holds only safe characters, else single-quoted ('\'' for quotes). */
export function bashQuote(s: string): string {
  if (s && BASH_SAFE.test(s)) return s
  return `'${s.replace(/'/g, `'\\''`)}'`
}

/**
 * A single-quoted PowerShell string: nothing inside is expanded ($, `,
 * "). PowerShell also treats the typographic single quotes ‘ ’ ‚ ‛ as
 * quotes, so they are doubled like '.
 */
export function powershellQuote(s: string): string {
  return `'${s.replace(/['‘’‚‛]/g, m => m + m)}'`
}

/** A command-line word for the shell. PowerShell always quotes (commas, @, < and $ are special there). */
export function shellQuote(shell: Shell, s: string): string {
  return shell === 'bash' ? bashQuote(s) : powershellQuote(s)
}

const ENV_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/

/** A line setting an environment variable, or a comment for a name the shells cannot set safely. */
export function envLine(shell: Shell, name: string, value: string): string {
  if (!ENV_NAME.test(name)) return `# skipped invalid variable name ${JSON.stringify(name)}`
  return shell === 'bash' ? `export ${name}=${bashQuote(value)}` : `$env:${name} = ${powershellQuote(value)}`
}

/** A comment line (# in both shells); line breaks are folded so a value cannot start a command. */
function comment(text: string): string {
  return `# ${text.replace(/[\r\n]+/g, ' ')}`
}

export interface SnippetInput {
  shell: Shell
  /** The bucket's name, for the gos3 CLI hint. */
  bucket: string
  /** archive: per-version snapshots (tag object); snapshots: whole-bucket snapshots. */
  purpose: 'archive' | 'snapshots'
  repository: string
  /** Revealed password; when absent a comment says how to get it. */
  password?: string
  /** Values of the further variables (revealed), else only their names are listed. */
  env?: Record<string, string> | null
  /** Names of the further variables (RepositoryInfo.envNames). */
  envNames?: readonly string[] | null
  options?: readonly string[] | null
  tags: Pick<ResticTags, 'object' | 'bucket' | 'snapshot'>
  /** This server's URL (the connection's endpoint), for the gos3 CLI's --server. */
  server?: string
}

/** The usual config file of a gos3 server, for the CLI's offline source (--config). */
export const GOS3_CONFIG_PATH = '/etc/gos3/gos3.yaml'

/** Placeholder of --server when the server's URL is not known. */
export const SERVER_PLACEHOLDER = 'SERVER_URL'

/**
 * A source of the `gos3 restic` CLI: online, `--server URL` (the gos3
 * access key from --access-key/--secret-key or AWS_ACCESS_KEY_ID /
 * AWS_SECRET_ACCESS_KEY); offline, `--config FILE` on the server.
 */
export type CliSource = { server?: string } | { config: string }

/**
 * A `gos3 restic <sub>` command line, values quoted for the shell, ending
 * in its required source. `args` are flags with an optional value.
 */
export function gos3ResticCommand(shell: Shell, sub: 'env' | 'exec' | 'objects' | 'recover', args: readonly (readonly [string, string?])[], source: CliSource): string {
  const src: [string, string] = 'config' in source ? ['--config', source.config] : ['--server', source.server || SERVER_PLACEHOLDER]
  return ['gos3', 'restic', sub, ...[...args, src].map(([flag, value]) => (value === undefined ? flag : `${flag} ${shellQuote(shell, value)}`))].join(' ')
}

/** The `gos3 restic env` arguments that print the repository's variables for `shell`. */
function envArgs(input: Pick<SnippetInput, 'bucket' | 'purpose' | 'shell'>): [string, string?][] {
  const args: [string, string?][] = [['--bucket', input.bucket]]
  if (input.purpose === 'snapshots') args.push(['--snapshots'])
  args.push(['--shell', input.shell])
  return args
}

/** "-o opt" arguments for every restic option, quoted for the shell. */
function optionArgs(shell: Shell, options: readonly string[] | null | undefined): string {
  return (options ?? []).map(o => ` -o ${shellQuote(shell, o)}`).join('')
}

/**
 * Commands that list (and hint how to restore) the bucket's restic
 * snapshots with the restic CLI alone, for bash or PowerShell. Every
 * value is quoted for the shell, so repository paths, passwords and
 * options containing spaces, quotes or $ are passed literally. Without a
 * revealed password, comments show how to get it with the gos3 CLI, online
 * (--server, this server's URL) or offline on the server (--config).
 */
export function resticSnippet(input: SnippetInput): string {
  const { shell } = input
  const lines: string[] = []
  lines.push(envLine(shell, 'RESTIC_REPOSITORY', input.repository))
  if (input.password !== undefined) {
    lines.push(envLine(shell, 'RESTIC_PASSWORD', input.password))
  } else {
    lines.push(comment('RESTIC_PASSWORD: reveal the credentials, or print every variable with the gos3 CLI, online (your gos3 key in --access-key/--secret-key or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY):'))
    lines.push(comment(`  ${gos3ResticCommand(shell, 'env', envArgs(input), { server: input.server })}`))
    lines.push(comment('or offline, on the server:'))
    lines.push(comment(`  ${gos3ResticCommand(shell, 'env', envArgs(input), { config: GOS3_CONFIG_PATH })}`))
  }
  const names = new Set<string>([...(input.envNames ?? []), ...Object.keys(input.env ?? {})])
  for (const name of [...names].sort()) {
    const value = input.env?.[name]
    lines.push(value !== undefined ? envLine(shell, name, value) : comment(`${name}: set it to the value from the credentials`))
  }
  const opts = optionArgs(shell, input.options)
  const tag = shellQuote(shell, `${input.purpose === 'snapshots' ? input.tags.snapshot : input.tags.object},${input.tags.bucket}`)
  lines.push(`restic snapshots${opts} --tag ${tag}`)
  if (input.purpose === 'snapshots') {
    lines.push(comment('Restore the newest whole-bucket snapshot: objects/ holds the files, gos3-manifest.jsonl their keys and versions'))
    lines.push(`restic restore latest${opts} --tag ${tag} --host gos3-snapshot --target ${shellQuote(shell, './gos3-restore')}`)
  } else {
    lines.push(comment('Each snapshot holds one object version; restore one by its ID from the list above:'))
    lines.push(comment(`restic restore SNAPSHOT_ID${opts} --target ./gos3-restore`))
    lines.push(comment(`or let gos3 decode the tags: ${gos3ResticCommand(shell, 'recover', [['--bucket', input.bucket], ['--key', 'KEY']], { server: input.server })}`))
  }
  return lines.join('\n')
}

/** Snippet input from a repository's (secret-free) info. */
export function snippetFromInfo(shell: Shell, bucket: string, purpose: 'archive' | 'snapshots', info: RepositoryInfo, tags: ResticTags, server?: string): SnippetInput {
  return { shell, bucket, purpose, repository: info.repository, envNames: info.envNames, options: info.options, tags, server }
}

/** Environment variables holding secrets (masked until shown). */
export function isSecretEnv(name: string): boolean {
  return /secret|password|token/i.test(name)
}

/** What masked secrets look like on screen. */
export const MASK = '••••••••'

/** The credentials with the password and secret variables masked, for display (copying uses the real ones). */
export function maskCredentials(creds: ResticCredentials): ResticCredentials {
  const env = Object.fromEntries(Object.entries(creds.env ?? {}).map(([k, v]) => [k, isSecretEnv(k) ? MASK : v]))
  return { ...creds, password: MASK, env }
}

/** Snippet input from revealed credentials (shown once, never stored). */
export function snippetFromCredentials(shell: Shell, bucket: string, creds: ResticCredentials, tags: ResticTags, server?: string): SnippetInput {
  return {
    server,
    shell,
    bucket,
    purpose: creds.purpose === 'snapshots' ? 'snapshots' : 'archive',
    repository: creds.repository,
    password: creds.password,
    env: creds.env ?? {},
    options: creds.options,
    tags
  }
}

// --- snapshot schedule -----------------------------------------------------------------

export type RetentionPresetName = 'standard' | 'short' | 'long' | 'custom'

export interface RetentionPreset {
  value: Exclude<RetentionPresetName, 'custom'>
  label: string
  description: string
  retention: SnapshotRetention
}

const R = (over: Partial<SnapshotRetention>): SnapshotRetention => ({
  keepLast: 0, keepHourly: 0, keepDaily: 0, keepWeekly: 0, keepMonthly: 0, keepYearly: 0, keepWithin: '0s', ...over
})

export const RETENTION_PRESETS: readonly RetentionPreset[] = [
  { value: 'standard', label: 'Standard', description: 'The last 7, plus 7 daily, 4 weekly and 12 monthly snapshots.', retention: R({ keepLast: 7, keepDaily: 7, keepWeekly: 4, keepMonthly: 12 }) },
  { value: 'short', label: 'Short', description: 'The last 3 and 7 daily snapshots.', retention: R({ keepLast: 3, keepDaily: 7 }) },
  { value: 'long', label: 'Long', description: '14 daily, 8 weekly, 24 monthly and 5 yearly snapshots.', retention: R({ keepLast: 3, keepDaily: 14, keepWeekly: 8, keepMonthly: 24, keepYearly: 5 }) }
]

export const RETENTION_FIELDS: readonly { key: Exclude<keyof SnapshotRetention, 'keepWithin'>, label: string }[] = [
  { key: 'keepLast', label: 'Last' },
  { key: 'keepHourly', label: 'Hourly' },
  { key: 'keepDaily', label: 'Daily' },
  { key: 'keepWeekly', label: 'Weekly' },
  { key: 'keepMonthly', label: 'Monthly' },
  { key: 'keepYearly', label: 'Yearly' }
]

function sameRetention(a: SnapshotRetention, b: SnapshotRetention): boolean {
  return RETENTION_FIELDS.every(f => a[f.key] === b[f.key]) && (parseGoDuration(a.keepWithin) ?? -1) === (parseGoDuration(b.keepWithin) ?? -2)
}

/** The preset a retention equals, else "custom". */
export function retentionPreset(r: SnapshotRetention): RetentionPresetName {
  return RETENTION_PRESETS.find(p => sameRetention(p.retention, r))?.value ?? 'custom'
}

/** A copy of a preset's retention. */
export function presetRetention(name: Exclude<RetentionPresetName, 'custom'>): SnapshotRetention {
  return { ...(RETENTION_PRESETS.find(p => p.value === name) ?? RETENTION_PRESETS[0]!).retention }
}

/** "7 last, 7 daily, 4 weekly, 12 monthly, within 30d" ('' when nothing is kept by rule). */
export function describeRetention(r: SnapshotRetention): string {
  const parts = RETENTION_FIELDS.filter(f => r[f.key] > 0).map(f => `${r[f.key]} ${f.label.toLowerCase()}`)
  const within = parseGoDuration(r.keepWithin) ?? 0
  if (within > 0) parts.push(`all within ${r.keepWithin}`)
  return parts.join(', ')
}

/** The snapshot schedule form (SnapshotConfig with editable strings). */
export interface SnapshotForm {
  enabled: boolean
  /** Go duration ("24h"; "d" accepted). */
  interval: string
  /** "HH:MM" UTC, '' = any time. */
  at: string
  consistent: boolean
  prefix: string
  retention: SnapshotRetention
  /** auto: the server chooses (new schedules); bucket: the bucket's own repository; backing: an aux repository there. */
  targetKind: 'auto' | 'bucket' | 'backing'
  backing: string
  /** Read-only: the aux repository the server generated. */
  repo: string
  pruneEnabled: boolean
  pruneInterval: string
  /** prune.enabled was not chosen (the server follows the archive scope); cleared when the user changes it. */
  pruneDefault: boolean
}

/** Minimum and maximum snapshot interval (seconds): 1h–30d. */
export const MIN_INTERVAL_SECONDS = 3600
export const MAX_INTERVAL_SECONDS = 30 * 86400

/**
 * The form for a new schedule: daily at 03:00 UTC, the standard retention,
 * the repository chosen by the server (the bucket's own for filesystem and
 * wrapped buckets, else one on the default backing), prune weekly — but
 * off while the archive is on, because prune takes restic's exclusive lock
 * and archive writes would wait.
 */
export function defaultSnapshotForm(opts: { archiveOn?: boolean, targetKind?: SnapshotForm['targetKind'], backing?: string } = {}): SnapshotForm {
  return {
    enabled: true,
    interval: '24h',
    at: '03:00',
    consistent: true,
    prefix: '',
    retention: presetRetention('standard'),
    targetKind: opts.targetKind ?? 'auto',
    backing: opts.backing ?? '',
    repo: '',
    pruneEnabled: !opts.archiveOn,
    pruneInterval: '168h',
    // Not chosen: the server decides from the archive scope.
    pruneDefault: true
  }
}

function count(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) && v >= 0 ? Math.floor(v) : 0
}

/** The form for a stored schedule (durations normalized, missing fields defaulted). */
export function snapshotFormFrom(cfg: SnapshotConfig): SnapshotForm {
  const r = (cfg.retention ?? {}) as Partial<SnapshotRetention>
  const targetKind = cfg.target?.kind === 'backing' ? 'backing' : cfg.target?.kind === 'bucket' ? 'bucket' : 'auto'
  return {
    enabled: !!cfg.enabled,
    interval: durationText(cfg.interval, '24h'),
    at: cfg.at ?? '',
    consistent: cfg.consistent !== false,
    prefix: cfg.prefix ?? '',
    retention: {
      keepLast: count(r.keepLast),
      keepHourly: count(r.keepHourly),
      keepDaily: count(r.keepDaily),
      keepWeekly: count(r.keepWeekly),
      keepMonthly: count(r.keepMonthly),
      keepYearly: count(r.keepYearly),
      keepWithin: durationText(r.keepWithin, '0s')
    },
    targetKind,
    backing: targetKind === 'backing' ? cfg.target?.backing ?? '' : '',
    repo: cfg.target?.repo ?? '',
    pruneEnabled: !!cfg.prune?.enabled,
    pruneInterval: durationText(cfg.prune?.interval, '168h'),
    pruneDefault: cfg.prune?.default === true
  }
}

export interface SnapshotFormErrors {
  interval?: string
  at?: string
  prefix?: string
  retention?: string
  keepWithin?: string
  backing?: string
  pruneInterval?: string
}

const AT_RE = /^([01]\d|2[0-3]):[0-5]\d$/

/** Validates the schedule form; returns the errors per field (empty when valid). */
export function validateSnapshotForm(f: SnapshotForm): SnapshotFormErrors {
  const e: SnapshotFormErrors = {}
  const interval = parseGoDuration(f.interval)
  if (interval === undefined) e.interval = 'Enter a duration such as 24h, 12h or 7d'
  else if (interval < MIN_INTERVAL_SECONDS || interval > MAX_INTERVAL_SECONDS) e.interval = 'Between 1h and 30d'
  if (f.at) {
    if (!AT_RE.test(f.at)) e.at = 'Use HH:MM (UTC), e.g. 03:00'
    else if (interval !== undefined && interval < 86400) e.at = 'A time of day applies to intervals of 24h or more; clear it for shorter intervals'
  }
  // eslint-disable-next-line no-control-regex
  if (f.prefix.length > 1024 || /[\x00-\x1f\x7f]/.test(f.prefix)) e.prefix = 'At most 1024 characters, no control characters'
  const within = parseGoDuration(f.retention.keepWithin || '0s')
  if (within === undefined) e.keepWithin = 'Enter a duration such as 720h or 30d (0s = none)'
  const counts = RETENTION_FIELDS.map(x => f.retention[x.key])
  if (counts.some(n => !Number.isInteger(n) || n < 0 || n > 100000)) e.retention = 'Use whole numbers from 0'
  else if (!counts.some(n => n > 0) && !(within && within > 0)) e.retention = 'Keep at least one snapshot (a count above 0, or a time span)'
  if (f.targetKind === 'backing' && !f.backing) e.backing = 'Choose the backing that holds the snapshots'
  if (f.pruneEnabled) {
    const prune = parseGoDuration(f.pruneInterval)
    if (prune === undefined) e.pruneInterval = 'Enter a duration such as 168h or 7d'
    else if (prune < MIN_INTERVAL_SECONDS) e.pruneInterval = 'At least 1h'
  }
  return e
}

/** The SnapshotConfig of a valid form (durations in Go's units). */
export function snapshotConfigFrom(f: SnapshotForm): SnapshotConfig {
  const cfg: SnapshotConfig = {
    enabled: f.enabled,
    interval: formatGoDuration(parseGoDuration(f.interval) ?? 86400),
    consistent: f.consistent,
    retention: {
      ...f.retention,
      keepWithin: formatGoDuration(parseGoDuration(f.retention.keepWithin || '0s') ?? 0)
    },
    prune: {
      enabled: f.pruneEnabled,
      interval: formatGoDuration(parseGoDuration(f.pruneInterval) ?? 604800),
      // Untouched: sent back as the server's default, which follows the archive scope.
      ...(f.pruneDefault ? { default: true } : {})
    }
  }
  // "auto" leaves the target out: the server chooses and the response tells which.
  if (f.targetKind === 'backing') cfg.target = { kind: 'backing', backing: f.backing, ...(f.repo ? { repo: f.repo } : {}) }
  else if (f.targetKind === 'bucket') cfg.target = { kind: 'bucket' }
  if (f.at) cfg.at = f.at
  if (f.prefix) cfg.prefix = f.prefix
  return cfg
}

/** "Every 24h at 03:00 UTC", "Every 6h". */
export function describeSchedule(cfg: Pick<SnapshotConfig, 'interval' | 'at' | 'enabled'>): string {
  if (!cfg.enabled) return 'Paused'
  const seconds = parseGoDuration(durationText(cfg.interval))
  const every = seconds === 86400 ? 'Daily' : seconds === 604800 ? 'Weekly' : `Every ${durationText(cfg.interval)}`
  return cfg.at ? `${every} at ${cfg.at} UTC` : every
}

/** "The bucket's own repository", "Repository snap-1 on cold". */
export function describeTarget(target: SnapshotConfig['target']): string {
  if (!target) return 'Chosen by the server'
  if (target.kind === 'bucket') return 'The bucket’s own repository'
  if (target.kind === 'backing') return `${target.repo ? `Repository ${target.repo}` : 'A repository'} on ${target.backing || 'a backing'}`
  return target.kind
}

/** Restic's tag of partial snapshots (parts of a chain; restore refuses them). */
export const PARTIAL_TAG = 'gos3-partial'

/** A partial snapshot: flagged by the server, or tagged gos3-partial. */
export function isPartialSnapshot(s: Pick<Snapshot, 'partial' | 'tags'>): boolean {
  return !!s.partial || !!s.tags?.includes(PARTIAL_TAG)
}

/** Snapshots newest first. */
export function sortSnapshots(list: readonly Snapshot[] | null | undefined): Snapshot[] {
  return [...(list ?? [])].sort((a, b) => (Date.parse(b.time) || 0) - (Date.parse(a.time) || 0))
}

// --- restore dialog ------------------------------------------------------------------------

export interface RestoreForm {
  target: 'new' | 'existing'
  bucket: string
  /** New bucket: '' = the server's default backing. */
  backing: string
  prefix: string
  onExisting: 'fail' | 'merge'
  configs: 'safe' | 'none'
}

export function emptyRestoreForm(source: string, taken: readonly string[] = []): RestoreForm {
  let bucket = ''
  for (const suffix of ['-restored', '-restored-2', '-restored-3']) {
    const name = `${source}${suffix}`
    if (!validateBucketName(name) && !taken.includes(name)) {
      bucket = name
      break
    }
  }
  return { target: 'new', bucket, backing: '', prefix: '', onExisting: 'fail', configs: 'safe' }
}

export interface RestoreFormErrors {
  bucket?: string
  prefix?: string
}

/** Validates the restore dialog. `buckets` lists the visible bucket names ([] when unknown). */
export function validateRestoreForm(f: RestoreForm, buckets: readonly string[]): RestoreFormErrors {
  const e: RestoreFormErrors = {}
  const name = f.bucket.trim()
  if (!name) e.bucket = f.target === 'new' ? 'Enter a name for the new bucket' : 'Choose a bucket'
  else if (f.target === 'new') {
    const err = validateBucketName(name)
    if (err) e.bucket = err
    else if (buckets.includes(name)) e.bucket = 'A bucket with this name exists; choose “Existing bucket” to restore into it'
  } else if (buckets.length && !buckets.includes(name)) {
    e.bucket = 'There is no bucket with this name'
  }
  // eslint-disable-next-line no-control-regex
  if (f.prefix.length > 1024 || /[\x00-\x1f\x7f]/.test(f.prefix)) e.prefix = 'At most 1024 characters, no control characters'
  return e
}

/** POST …/restic/snapshots/{id}/restore body of the dialog. */
export function restoreRequest(f: RestoreForm): RestoreSnapshotRequest {
  const req: RestoreSnapshotRequest = { target: { bucket: f.bucket.trim() }, configs: f.configs }
  if (f.target === 'new') {
    const create: CopyTargetCreate = {}
    if (f.backing) create.backing = f.backing
    req.target.create = create
  } else {
    req.onExisting = f.onExisting
  }
  if (f.prefix) req.prefix = f.prefix
  return req
}

/** Poll interval of the snapshots card while a snapshot job runs. */
export const SNAPSHOT_POLL_MS = 5000

/** True when the form differs from the stored config (or there is none). */
export function snapshotFormDirty(f: SnapshotForm, stored: SnapshotConfig | null | undefined): boolean {
  if (!stored) return true
  const a = snapshotFormFrom(stored)
  const keys = Object.keys(a) as (keyof SnapshotForm)[]
  return keys.some(k => JSON.stringify(f[k]) !== JSON.stringify(a[k]))
}
