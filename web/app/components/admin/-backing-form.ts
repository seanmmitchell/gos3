// Form model of the live backing configuration (GET/PUT /backings/config,
// docs/admin-api.md): validation, copies, the PUT body and the s3 helpers of
// BackingConfiguration.vue. Pure functions so unit tests share the rules.
//
// The file name starts with "-" so Nuxt does not register it as a component.
import type { BackingBackend, BackingConfiguration, BackingDefinition, S3BackingDefinition } from '~/types/admin'

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

/** Limits of S3BackingDefinition.partSize (bytes), as enforced by the server. */
export const MIN_PART_SIZE = 5 * 1024 * 1024
export const MAX_PART_SIZE = 5 * 1024 * 1024 * 1024

/** Read-only fields of S3BackingDefinition, dropped from the PUT body. */
const READ_ONLY_S3_FIELDS = ['secretKeyConfigured', 'encryptionKeyConfigured', 'secretKeySource', 'encryptionKeySource'] as const

const BUCKET_NAME = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/

/** A blank s3 section for a new (or newly switched) s3 backing. */
export function emptyS3(): S3BackingDefinition {
  return { endpoint: '', region: '', bucket: '', prefix: '', addressing: '', gateway: false, accessKey: '', secretKey: '', encryptionKey: '' }
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

/** Normalized endpoint + bucket + prefix of an s3 backing, for duplicate detection. */
function s3Location(s3: S3BackingDefinition): string {
  let endpoint = s3.endpoint.trim().toLowerCase()
  try {
    endpoint = new URL(s3.endpoint.trim()).origin
  } catch { /* reported by endpointError */ }
  return `${endpoint}|${s3.bucket.trim()}|${s3.prefix.trim().replace(/\/+$/, '')}`
}

function validateS3(name: string, s3: S3BackingDefinition | undefined): string | undefined {
  if (!s3) return `Enter the S3 service of "${name}".`
  if (!s3.endpoint.trim()) return `Enter the S3 endpoint of "${name}".`
  const ep = endpointError(s3.endpoint)
  if (ep) return `The S3 endpoint of "${name}" ${ep}.`
  if (!BUCKET_NAME.test(s3.bucket.trim())) return `Enter a valid bucket name for "${name}" (3–63 lowercase letters, digits, dots or dashes).`
  if (s3.prefix.trim().startsWith('/')) return `The prefix of "${name}" must not start with "/".`
  if (!s3.accessKey.trim()) return `Enter the access key of "${name}".`
  if (!s3.secretKey?.trim() && !s3.secretKeyConfigured && s3.secretKeySource !== 'env') return `Enter the secret key of "${name}".`
  if (s3.partSize && (!Number.isInteger(s3.partSize) || s3.partSize < MIN_PART_SIZE || s3.partSize > MAX_PART_SIZE)) return `The part size of "${name}" must be between 5 MiB and 5 GiB.`
  if (s3.encryptionKey?.trim() && !validEncryptionKey(s3.encryptionKey)) return `The encryption key of "${name}" must be base64 of 32 bytes (use Generate).`
}

export function validateBackingConfiguration(form: BackingConfiguration): string | undefined {
  if (form.backings.length > 64) return 'At most 64 additional backings are allowed.'
  const names = new Set(['default'])
  const paths = new Set<string>()
  const locations = new Set<string>()
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
      const err = validateS3(b.name, b.s3)
      if (err) return err
      const loc = s3Location(b.s3!)
      if (locations.has(loc)) return 'Every S3 backing needs its own bucket or prefix.'
      locations.add(loc)
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
      for (const f of ['endpoint', 'region', 'bucket', 'prefix', 'accessKey'] as const) s3[f] = s3[f].trim()
      if (s3.secretKey) s3.secretKey = s3.secretKey.trim()
      if (s3.encryptionKey) s3.encryptionKey = s3.encryptionKey.trim()
      if (!s3.secretKey) delete s3.secretKey
      if (!s3.encryptionKey) delete s3.encryptionKey
      if (!s3.partSize) delete s3.partSize
      return { name: b.name, backend: 's3', path: '', s3 }
    })
  }
}
