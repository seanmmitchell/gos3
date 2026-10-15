// Form model of the live backing configuration (GET/PUT /backings/config,
// docs/admin-api.md): validation, copies, the PUT body and the s3 helpers of
// BackingConfiguration.vue. Pure functions so unit tests share the rules.
//
// The file name starts with "-" so Nuxt does not register it as a component.
import type { BackingBackend, BackingConfiguration, BackingDefinition, BucketKind, S3BackingDefinition } from '~/types/admin'

/** Backend choices of the configuration form. */
export const BACKEND_ITEMS: readonly { label: string, value: BackingBackend }[] = [
  { label: 'Filesystem', value: 'fs' },
  { label: 'Memory', value: 'memory' },
  { label: 'S3 service', value: 's3' }
]

/** Addressing choices ('' = automatic). */
export const ADDRESSING_ITEMS = [
  { label: 'Automatic', value: 'auto' },
  { label: 'Path style (endpoint/bucket/key)', value: 'path' },
  { label: 'Virtual hosted (bucket.endpoint/key)', value: 'virtual' }
] as const

/**
 * Default bucket kind choices of an s3 backing ('' = automatic: wrapped with
 * a home, else native). USelect cannot hold '', hence 'auto'.
 */
export const DEFAULT_KIND_ITEMS = [
  { label: 'Automatic (wrapped with a home, else native)', value: 'auto' },
  { label: 'Wrapped', value: 'wrapped' },
  { label: 'Native', value: 'native' }
] as const

/** The form value of S3BackingDefinition.defaultKind ('' → 'auto'). */
export function defaultKindValue(s3: Pick<S3BackingDefinition, 'defaultKind'>): BucketKind | 'auto' {
  return s3.defaultKind || 'auto'
}

/** Sets S3BackingDefinition.defaultKind from a DEFAULT_KIND_ITEMS value. */
export function setDefaultKind(s3: S3BackingDefinition, v: string): void {
  s3.defaultKind = v === 'wrapped' || v === 'native' ? (v as BucketKind) : ''
}

/**
 * Sets the home bucket. Clearing it resets a default kind of "wrapped" that
 * was loaded (`loaded`, the saved definition) rather than chosen in the form
 * back to automatic: without a home buckets cannot be wrapped by default,
 * and the server derives "native" (it also resets an unchanged derived kind).
 */
export function setHomeBucket(s3: S3BackingDefinition, value: string, loaded?: Pick<S3BackingDefinition, 'defaultKind'>): void {
  s3.homeBucket = value
  if (!value.trim() && s3.defaultKind === 'wrapped' && loaded?.defaultKind === 'wrapped') s3.defaultKind = ''
}

/** Help text of the home bucket field. */
export const HOME_HELP = 'Where gos3 keeps this backing\'s records and its shared container of wrapped buckets. Leave empty to keep records on this server (system directory) — then wrapped buckets need a container added later.'

/** Limits of S3BackingDefinition.partSize (bytes), as enforced by the server. */
export const MIN_PART_SIZE = 5 * 1024 * 1024
export const MAX_PART_SIZE = 5 * 1024 * 1024 * 1024

/** Read-only fields of S3BackingDefinition, dropped from the PUT body. */
const READ_ONLY_S3_FIELDS = ['secretKeyConfigured', 'encryptionKeyConfigured', 'secretKeySource', 'encryptionKeySource', 'accessKeySource', 'envFields'] as const

/** Home bucket names the server accepts (remoteBucketRe in internal/config/s3backing.go). */
const HOME_BUCKET_NAME = /^[a-z0-9][a-z0-9._-]{1,254}$/

/** A blank s3 section for a new (or newly switched) s3 backing. */
export function emptyS3(): S3BackingDefinition {
  return { endpoint: '', region: '', homeBucket: '', homePrefix: '', defaultKind: '', addressing: '', accessKey: '', secretKey: '', encryptionKey: '' }
}

/** Switches a row's backend: the directory is reset and the s3 section added or removed. */
export function setBackend(b: BackingDefinition, backend: BackingBackend): void {
  b.backend = backend
  b.path = ''
  if (backend === 's3') b.s3 ??= emptyS3()
  else delete b.s3
}

/**
 * The environment variable a backing's secret comes from when it is not
 * configured: GOS3_BACKING_<NAME>_<SUFFIX>, name upper-cased with "-" as "_".
 */
export function backingEnvVar(name: string, suffix: 'SECRET_KEY' | 'ENCRYPTION_KEY'): string {
  return `GOS3_BACKING_${(name || '<NAME>').toUpperCase().replace(/-/g, '_')}_${suffix}`
}

/** Placeholder of a write-only secret input: where the current value comes from. */
export function secretPlaceholder(name: string, s3: S3BackingDefinition, which: 'secret' | 'encryption'): string {
  const source = which === 'secret' ? s3.secretKeySource : s3.encryptionKeySource
  const configured = which === 'secret' ? s3.secretKeyConfigured : s3.encryptionKeyConfigured
  if (source === 'env') return `From ${backingEnvVar(name, which === 'secret' ? 'SECRET_KEY' : 'ENCRYPTION_KEY')}`
  if (configured) return 'Saved – leave blank to keep'
  return ''
}

/** A new random encryption key: 32 bytes, base64. */
export function generateEncryptionKey(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32))
  return btoa(String.fromCharCode(...bytes))
}

/** True when `key` is base64 of exactly 32 bytes. */
export function validEncryptionKey(key: string): boolean {
  try {
    return atob(key.trim()).length === 32
  } catch {
    return false
  }
}

/** Error of an s3 endpoint ('' when it is an http(s) URL without a path). */
function endpointError(endpoint: string): string {
  let u: URL
  try {
    u = new URL(endpoint.trim())
  } catch {
    return 'must be an http:// or https:// URL'
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return 'must be an http:// or https:// URL'
  if ((u.pathname && u.pathname !== '/') || u.search || u.hash || u.username || u.password) return 'must not have a path, query or credentials'
  return ''
}

/** Normalized endpoint + home bucket of an s3 backing with a home, for overlap detection. */
interface HomeLocation {
  key: string
  /** Home prefix without trailing slashes, as path segments ('' = the whole bucket). */
  prefix: string
}

function homeLocation(s3: S3BackingDefinition): HomeLocation | undefined {
  if (!s3.homeBucket.trim()) return undefined
  let endpoint = s3.endpoint.trim().toLowerCase()
  try {
    endpoint = new URL(s3.endpoint.trim()).origin
  } catch { /* reported by endpointError */ }
  return { key: `${endpoint}|${s3.homeBucket.trim().toLowerCase()}`, prefix: s3.homePrefix.trim().replace(/\/+$/, '') }
}

/** True when one home contains the other (same bucket, nested or equal prefixes). */
function homesOverlap(a: HomeLocation, b: HomeLocation): boolean {
  if (a.key !== b.key) return false
  if (!a.prefix || !b.prefix || a.prefix === b.prefix) return true
  return a.prefix.startsWith(`${b.prefix}/`) || b.prefix.startsWith(`${a.prefix}/`)
}

function validateS3(name: string, s3: S3BackingDefinition | undefined, persistent: boolean, loaded?: S3BackingDefinition): string | undefined {
  if (!s3) return `Enter the S3 service of "${name}".`
  if (!s3.endpoint.trim()) return `Enter the S3 endpoint of "${name}".`
  const ep = endpointError(s3.endpoint)
  if (ep) return `The S3 endpoint of "${name}" ${ep}.`
  const home = s3.homeBucket.trim()
  if (home && !HOME_BUCKET_NAME.test(home)) return `Enter a valid home bucket name for "${name}" (2–255 lowercase letters, digits, dots, dashes or underscores, starting with a letter or digit).`
  if (!home && s3.homePrefix.trim()) return `The home prefix of "${name}" needs a home bucket.`
  if (s3.homePrefix.trim().startsWith('/')) return `The home prefix of "${name}" must not start with "/".`
  // A loaded, unchanged kind is reset by the server when the home goes (see setHomeBucket).
  if (!home && s3.defaultKind === 'wrapped' && loaded?.defaultKind !== 'wrapped') return `"${name}" needs a home bucket to create wrapped buckets by default (or choose native).`
  if (!home && !persistent) return `"${name}" needs a home bucket: this server has no data directory to keep the backing's records in.`
  if (!s3.accessKey.trim()) return `Enter the access key of "${name}".`
  if (!s3.secretKey?.trim() && !s3.secretKeyConfigured && s3.secretKeySource !== 'env') return `Enter the secret key of "${name}".`
  if (s3.partSize && (!Number.isInteger(s3.partSize) || s3.partSize < MIN_PART_SIZE || s3.partSize > MAX_PART_SIZE)) return `The part size of "${name}" must be between 5 MiB and 5 GiB.`
  if (s3.encryptionKey?.trim() && !validEncryptionKey(s3.encryptionKey)) return `The encryption key of "${name}" must be base64 of 32 bytes (use Generate).`
}

/**
 * Validates the form; `loaded` is the configuration it was loaded from (a
 * default kind kept from it is not the user's choice).
 */
export function validateBackingConfiguration(form: BackingConfiguration, loaded?: BackingConfiguration): string | undefined {
  if (form.backings.length > 64) return 'At most 64 additional backings are allowed.'
  const names = new Set(['default'])
  const paths = new Set<string>()
  const homes: HomeLocation[] = []
  for (const b of form.backings) {
    if (!/^[a-z0-9][a-z0-9_-]{0,31}$/.test(b.name)) return 'Backing names need 1–32 lowercase letters, digits, underscores or dashes, starting with a letter or digit.'
    if (names.has(b.name)) return `Backing name "${b.name}" is already used. The name "default" is reserved.`
    names.add(b.name)
    if (b.backend === 'fs') {
      if (!b.path.trim()) return `Enter a server directory for "${b.name}".`
      if (paths.has(b.path)) return 'Every filesystem backing needs its own directory.'
      paths.add(b.path)
    } else if (b.backend === 's3') {
      if (b.path) return 'S3 backings cannot have a directory.'
      const err = validateS3(b.name, b.s3, form.persistent, loaded?.backings.find(x => x.name === b.name)?.s3)
      if (err) return err
      const home = homeLocation(b.s3!)
      if (home) {
        if (homes.some(h => homesOverlap(h, home))) return 'Every S3 backing needs its own home: home buckets and prefixes must not overlap.'
        homes.push(home)
      }
    } else if (b.backend !== 'memory' || b.path) return 'Memory backings cannot have a directory.'
  }
  if (!names.has(form.defaultBacking)) return 'Choose an existing backing for new buckets.'
}

/** A deep copy (rows and their s3 sections). */
export function copyBackingConfiguration(form: BackingConfiguration): BackingConfiguration {
  return { ...form, backings: form.backings.map(b => (b.s3 ? { ...b, s3: { ...b.s3 } } : { ...b })) }
}

/**
 * The PUT body of a form: read-only s3 fields dropped, blank secrets left
 * out (the server keeps the stored ones), no s3 section on other backends.
 */
export function toPutBody(form: BackingConfiguration): BackingConfiguration {
  return {
    ...form,
    backings: form.backings.map((b): BackingDefinition => {
      if (b.backend !== 's3' || !b.s3) return { name: b.name, backend: b.backend, path: b.path }
      const s3: S3BackingDefinition = { ...b.s3 }
      for (const f of READ_ONLY_S3_FIELDS) delete s3[f]
      for (const f of ['endpoint', 'region', 'homeBucket', 'homePrefix', 'accessKey'] as const) s3[f] = s3[f].trim()
      if (s3.secretKey) s3.secretKey = s3.secretKey.trim()
      if (s3.encryptionKey) s3.encryptionKey = s3.encryptionKey.trim()
      if (!s3.secretKey) delete s3.secretKey
      if (!s3.encryptionKey) delete s3.encryptionKey
      if (!s3.partSize) delete s3.partSize
      return { name: b.name, backend: 's3', path: '', s3 }
    })
  }
}
