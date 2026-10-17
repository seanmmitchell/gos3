// The "Copy / Mirror" wizard (CopyBucketModal): its form, what the source
// and target allow, validation and the CopyJobSpec request body
// (docs/admin-api.md, "Jobs: copy and mirror"). Pure functions, so the
// dialog and tests share the same rules.
//
// The file name starts with "-" so Nuxt does not register it as a component.
import type { AvailableBackings, BucketKind, CopyConfigs, CopyJobOptions, CopyJobSpec, CopyOnExisting, CopyTargetCreate, CopyVersions } from '~/types/admin'
import { defaultBacking, defaultBucketKind, defaultContainer, findBacking, kindChoices, containerItems } from '~/utils/backings'
import { formatGoDuration, parseGoDuration } from '~/utils/durations'
import { validateBucketName } from '~/utils/paths'

/** Smallest mirror poll interval the server accepts (seconds). */
export const MIN_POLL_SECONDS = 30

/** Most keys copied in parallel. */
export const MAX_CONCURRENCY = 32

export type CopyTargetMode = 'new' | 'existing'

/** What the wizard knows about the bucket being copied. */
export interface CopySource {
  bucket: string
  /** 'Enabled' | 'Suspended' | '' (never enabled); null when unknown. */
  versioning: string | null
  /** Object lock enabled; null when unknown. */
  objectLock: boolean | null
}

/** What the wizard knows about an existing target bucket (HeadBucket, GetBucketVersioning, GetObjectLockConfiguration). */
export interface ExistingTargetInfo {
  /** native | wrapped (s3 backings), '' elsewhere or unknown. */
  kind?: string
  /** 'Enabled' | 'Suspended' | ''; null when unknown. */
  versioning?: string | null
  /** The bucket's storage cannot keep versions (GetBucketVersioning answered NotImplemented). */
  versioningUnsupported?: boolean
  objectLock?: boolean | null
}

export interface CopyContext {
  source: CopySource
  /** Every bucket name the identity can see (for "existing" and name clashes). */
  buckets: readonly string[]
  /** WhoAmI.backings (backings, kinds, containers); null on servers without backings. */
  backings?: AvailableBackings | null
  /** Facts about the chosen existing target, when loaded. */
  target?: ExistingTargetInfo | null
}

export interface CopyForm {
  mode: 'copy' | 'mirror'
  /** Copy only keys with this prefix ('' = all). */
  sourcePrefix: string
  target: CopyTargetMode
  /** Name of the new bucket, or the existing bucket chosen. */
  targetBucket: string
  /** Prefix in the target the keys are written under ('' = same keys). */
  targetPrefix: string
  // New target bucket:
  /** '' = the server's default backing. */
  backing: string
  /** s3 backings: '' = the backing's default kind. */
  kind: BucketKind | ''
  /** wrapped: '' = the backing's default container. */
  container: string
  /** native: '' = the bucket name. */
  remoteBucket: string
  objectLock: boolean
  // Options:
  /** 'auto' = the server decides (all when the target keeps versions). */
  versions: CopyVersions | 'auto'
  configs: CopyConfigs
  onExisting: CopyOnExisting
  encryption: 'preserve' | 'target'
  allowPlaintext: boolean
  dropRetention: boolean
  compression: 'preserve' | 'target'
  // Mirror:
  propagateDeletes: boolean
  deleteExtraneous: boolean
  pollInterval: string
  verifyInterval: string
  // Both:
  concurrency: number
  /** copy: compare again after the first pass. */
  finalPass: boolean
}

/** A suggested name for the copy: "<source>-copy" when that is a valid bucket name, else ''. */
export function suggestedTargetName(source: string, taken: readonly string[] = []): string {
  for (const suffix of ['-copy', '-copy-2', '-copy-3']) {
    const name = `${source}${suffix}`
    if (!validateBucketName(name) && !taken.includes(name)) return name
  }
  return ''
}

export function emptyCopyForm(ctx: CopyContext, over: Partial<CopyForm> = {}): CopyForm {
  return {
    mode: 'copy',
    sourcePrefix: '',
    target: 'new',
    targetBucket: suggestedTargetName(ctx.source.bucket, ctx.buckets),
    targetPrefix: '',
    backing: '',
    kind: '',
    container: '',
    remoteBucket: '',
    // Locked versions keep their retention on a target with object lock.
    objectLock: !!ctx.source.objectLock,
    versions: 'auto',
    configs: 'safe',
    onExisting: 'fail',
    encryption: 'preserve',
    allowPlaintext: false,
    dropRetention: false,
    compression: 'preserve',
    propagateDeletes: true,
    deleteExtraneous: false,
    pollInterval: '5m',
    verifyInterval: '24h',
    concurrency: 4,
    finalPass: true,
    ...over
  }
}

/** The backing a new target goes to: the chosen one, else the server's default. */
export function newTargetBacking(form: Pick<CopyForm, 'backing'>, ctx: Pick<CopyContext, 'backings'>) {
  return findBacking(form.backing || defaultBacking(ctx.backings), ctx.backings)
}

/** The kind of the target: chosen (or the backing's default) for a new bucket, reported for an existing one ('' when not an s3 bucket or unknown). */
export function targetKind(form: CopyForm, ctx: CopyContext): BucketKind | '' {
  if (form.target === 'existing') return ctx.target?.kind === 'native' || ctx.target?.kind === 'wrapped' ? ctx.target.kind : ''
  const b = newTargetBacking(form, ctx)
  const kinds = kindChoices(b).map(k => k.value)
  if (form.kind && kinds.includes(form.kind)) return form.kind
  return defaultBucketKind(b)
}

/** The target will have object lock: chosen for a new bucket, reported for an existing one (null when unknown). */
export function targetObjectLock(form: CopyForm, ctx: CopyContext): boolean | null {
  if (form.target === 'new') return form.objectLock
  return ctx.target?.objectLock ?? null
}

/**
 * Why `versions: "all"` cannot work for this target, or '' when it can.
 * Every version is kept by targets in gos3's format; a native bucket keeps
 * versions only with versioning on its remote bucket.
 */
export function allVersionsRefusal(form: CopyForm, ctx: CopyContext): string {
  const name = form.targetBucket.trim() || 'The target'
  if (form.target === 'existing') {
    const t = ctx.target
    if (t?.versioningUnsupported) return `${name} cannot keep versions (its storage has no versioning).`
    if (t?.kind === 'native' && t.versioning !== 'Enabled' && t.versioning !== null && t.versioning !== undefined) {
      return `${name} is a native bucket without versioning; enable versioning on it first.`
    }
  }
  return ''
}

export interface VersionsItem {
  value: CopyVersions | 'auto'
  label: string
  description: string
  disabled?: boolean
}

/** URadioGroup items for the versions option, "all" disabled with the reason when it cannot work. */
export function versionsItems(form: CopyForm, ctx: CopyContext): VersionsItem[] {
  const refusal = allVersionsRefusal(form, ctx)
  const unversioned = ctx.source.versioning === ''
  const nativeNew = form.target === 'new' && targetKind(form, ctx) === 'native'
  return [
    { value: 'auto', label: 'Automatic', description: 'Every version when the target can keep them, else only the current ones.' },
    {
      value: 'all',
      label: 'All versions',
      disabled: !!refusal,
      description: refusal
        || (unversioned
          ? `${ctx.source.bucket} is not versioned: it has one version per key.`
          : nativeNew
            ? 'Versioning is enabled on the new remote bucket; its service must support it.'
            : 'Keeps every version, delete marker and version ID.')
    },
    { value: 'current', label: 'Current versions only', description: 'The latest version of each key; history and delete markers are left out.' }
  ]
}

export interface CopyFormErrors {
  targetBucket?: string
  remoteBucket?: string
  container?: string
  sourcePrefix?: string
  targetPrefix?: string
  versions?: string
  dropRetention?: string
  onExisting?: string
  pollInterval?: string
  verifyInterval?: string
  concurrency?: string
}

/** Error of a key prefix ('' is fine): at most 1024 characters, no control characters. */
export function validateKeyPrefix(prefix: string): string | undefined {
  if (!prefix) return undefined
  if (prefix.length > 1024) return 'At most 1024 characters'
  // eslint-disable-next-line no-control-regex
  if (/[\x00-\x1f\x7f]/.test(prefix)) return 'Must not contain control characters'
  return undefined
}

/** Validates the wizard; returns the errors per field (empty when valid). */
export function validateCopyForm(form: CopyForm, ctx: CopyContext): CopyFormErrors {
  const e: CopyFormErrors = {}
  const target = form.targetBucket.trim()
  if (!target) {
    e.targetBucket = form.target === 'new' ? 'Enter a name for the new bucket' : 'Choose a bucket'
  } else if (target === ctx.source.bucket) {
    e.targetBucket = 'Choose another bucket than the source'
  } else if (form.target === 'new') {
    const nameError = validateBucketName(target)
    if (nameError) e.targetBucket = nameError
    else if (ctx.buckets.includes(target)) e.targetBucket = 'A bucket with this name exists; choose “Existing bucket” to copy into it'
  } else if (ctx.buckets.length && !ctx.buckets.includes(target)) {
    e.targetBucket = 'There is no bucket with this name'
  }

  const sp = validateKeyPrefix(form.sourcePrefix)
  if (sp) e.sourcePrefix = sp
  const tp = validateKeyPrefix(form.targetPrefix)
  if (tp) e.targetPrefix = tp

  if (form.target === 'new') {
    const kind = targetKind(form, ctx)
    const b = newTargetBacking(form, ctx)
    if (kind === 'native' && form.remoteBucket.trim()) {
      const re = validateBucketName(form.remoteBucket.trim())
      if (re) e.remoteBucket = re
    }
    if (kind === 'wrapped' && b && !containerItems(b).length) e.container = 'This backing has no container for wrapped buckets; choose native or another backing'
  } else if (form.onExisting === 'adopt' && form.mode !== 'mirror') {
    e.onExisting = 'Only a mirror can take over an existing bucket'
  }

  if (form.versions === 'all') {
    const refusal = allVersionsRefusal(form, ctx)
    if (refusal) e.versions = refusal
  }

  if (ctx.source.objectLock && targetObjectLock(form, ctx) === false && !form.dropRetention) {
    e.dropRetention = `${ctx.source.bucket} has object lock and the target does not: enable object lock on the new bucket, or allow dropping retention`
  }

  if (form.mode === 'mirror') {
    const poll = parseGoDuration(form.pollInterval)
    if (poll === undefined) e.pollInterval = 'Enter a duration such as 5m or 1h'
    else if (poll < MIN_POLL_SECONDS) e.pollInterval = `At least ${MIN_POLL_SECONDS}s`
    const verify = parseGoDuration(form.verifyInterval)
    if (verify === undefined) e.verifyInterval = 'Enter a duration such as 24h'
    else if (verify < 60) e.verifyInterval = 'At least 1m'
  }

  if (!Number.isInteger(form.concurrency) || form.concurrency < 1 || form.concurrency > MAX_CONCURRENCY) {
    e.concurrency = `A whole number from 1 to ${MAX_CONCURRENCY}`
  }
  return e
}

/** The fields of the wizard's first step (target), for "Next". */
export const TARGET_STEP_FIELDS: readonly (keyof CopyFormErrors)[] = ['targetBucket', 'remoteBucket', 'container', 'sourcePrefix', 'targetPrefix']

/** True when none of `fields` (default all) has an error. */
export function stepValid(errors: CopyFormErrors, fields?: readonly (keyof CopyFormErrors)[]): boolean {
  return (fields ?? (Object.keys(errors) as (keyof CopyFormErrors)[])).every(f => !errors[f])
}

/** The create part of a new target (what differs from the server's defaults, and always objectLock). */
export function targetCreate(form: CopyForm, ctx: CopyContext): CopyTargetCreate {
  const create: CopyTargetCreate = {}
  if (form.backing) create.backing = form.backing
  const kind = targetKind(form, ctx)
  const b = newTargetBacking(form, ctx)
  // The kind is sent whenever the backing offers a choice, so the bucket gets the kind shown.
  if (kind && kindChoices(b).length) create.kind = kind
  if (kind === 'wrapped') {
    const container = form.container || defaultContainer(b)
    if (container) create.container = container
  }
  const remote = form.remoteBucket.trim()
  if (kind === 'native' && remote && remote !== form.targetBucket.trim()) create.remoteBucket = remote
  // Always explicit: when left out, the server follows the source's object
  // lock, which would make the new bucket WORM irreversibly.
  create.objectLock = form.objectLock
  return create
}

/** The CopyJobSpec of the wizard (POST /jobs). */
export function copyJobSpec(form: CopyForm, ctx: CopyContext): CopyJobSpec {
  const options: CopyJobOptions = {
    configs: form.configs,
    encryption: form.encryption,
    compression: form.compression,
    concurrency: form.concurrency
  }
  if (form.versions !== 'auto') options.versions = form.versions
  if (form.target === 'existing') options.onExisting = form.onExisting
  if (form.allowPlaintext) options.allowPlaintext = true
  if (form.dropRetention) options.dropRetention = true
  if (form.mode === 'mirror') {
    options.propagateDeletes = form.propagateDeletes
    options.deleteExtraneous = form.deleteExtraneous
    options.pollInterval = formatGoDuration(parseGoDuration(form.pollInterval) ?? 300)
    options.verifyInterval = formatGoDuration(parseGoDuration(form.verifyInterval) ?? 86400)
  } else {
    options.finalPass = form.finalPass
  }
  const spec: CopyJobSpec = {
    kind: form.mode,
    source: { bucket: ctx.source.bucket },
    target: { bucket: form.targetBucket.trim() },
    options
  }
  if (form.sourcePrefix) spec.source.prefix = form.sourcePrefix
  if (form.targetPrefix) spec.target.prefix = form.targetPrefix
  if (form.target === 'new') spec.target.create = targetCreate(form, ctx)
  return spec
}

/**
 * Hints shown before the dry run (the server's dry run adds its own
 * warnings): plaintext copies to native buckets, mirrors deleting target
 * keys, and so on.
 */
export function copyHints(form: CopyForm, ctx: CopyContext): string[] {
  const hints: string[] = []
  const kind = targetKind(form, ctx)
  if (kind === 'native' && !form.allowPlaintext) {
    hints.push('Native buckets store objects as they are: if the source holds encrypted objects, allow plaintext copies or the job fails.')
  }
  if (form.mode === 'mirror' && form.deleteExtraneous) {
    hints.push('The mirror deletes keys in the target that the source does not have.')
  }
  if (form.target === 'existing' && form.onExisting === 'adopt') {
    hints.push('The mirror takes over the target: keys it copies are kept in step with the source from now on.')
  }
  if (form.versions === 'current' && ctx.source.versioning === 'Enabled') {
    hints.push('Only current versions are copied: older versions and delete markers stay behind.')
  }
  return hints
}

export interface SpecLine {
  label: string
  value: string
}

const CONFIG_LABELS: Record<string, string> = {
  safe: 'Safe settings (CORS, tags, policy with rewritten ARNs, …)',
  all: 'All settings',
  none: 'None'
}

const ON_EXISTING_LABELS: Record<string, string> = {
  fail: 'Refuse unless empty',
  merge: 'Copy without deleting',
  adopt: 'Take over (mirror)'
}

/** Summary lines of a (normalized) spec for the review step. */
export function describeSpec(spec: CopyJobSpec): SpecLine[] {
  const o = spec.options ?? {}
  const end = (e: { bucket: string, prefix?: string }) => (e.prefix ? `${e.bucket}/${e.prefix}` : e.bucket)
  const lines: SpecLine[] = [
    { label: 'Job', value: spec.kind === 'mirror' ? 'Mirror (continuous)' : 'Copy (once)' },
    { label: 'Source', value: end(spec.source) },
    { label: 'Target', value: end(spec.target) + (spec.target.create ? ' (new bucket)' : '') }
  ]
  const c = spec.target.create
  if (c) {
    const parts = [c.backing ? `backing ${c.backing}` : 'default backing']
    if (c.kind) parts.push(c.kind)
    if (c.container) parts.push(`container ${c.container}`)
    if (c.remoteBucket) parts.push(`remote bucket ${c.remoteBucket}`)
    if (c.objectLock !== undefined) parts.push(c.objectLock ? 'object lock' : 'no object lock')
    lines.push({ label: 'New bucket', value: parts.join(', ') })
  }
  lines.push({ label: 'Versions', value: o.versions === 'all' ? 'All versions' : o.versions === 'current' ? 'Current versions only' : 'Automatic' })
  if (o.configs) lines.push({ label: 'Settings', value: CONFIG_LABELS[o.configs] ?? o.configs })
  if (o.onExisting && !spec.target.create) lines.push({ label: 'Existing target', value: ON_EXISTING_LABELS[o.onExisting] ?? o.onExisting })
  const enc = [o.encryption === 'target' ? 'target settings' : 'preserved']
  if (o.allowPlaintext) enc.push('plaintext allowed')
  lines.push({ label: 'Encryption', value: enc.join(', ') })
  if (o.dropRetention) lines.push({ label: 'Retention', value: 'Dropped where the target has no object lock' })
  if (o.compression) lines.push({ label: 'Compression', value: o.compression === 'target' ? 'Target settings' : 'Preserved' })
  if (spec.kind === 'mirror') {
    const m = [o.propagateDeletes === false ? 'deletes not propagated' : 'deletes propagated']
    if (o.deleteExtraneous) m.push('extra target keys deleted')
    if (o.pollInterval) m.push(`poll ${o.pollInterval}`)
    if (o.verifyInterval) m.push(`verify ${o.verifyInterval}`)
    lines.push({ label: 'Mirror', value: m.join(', ') })
  } else if (o.finalPass !== undefined) {
    lines.push({ label: 'Final pass', value: o.finalPass ? 'Yes' : 'No' })
  }
  if (o.concurrency) lines.push({ label: 'Parallel keys', value: String(o.concurrency) })
  return lines
}
