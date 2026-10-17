import { describe, expect, it } from 'vitest'
import {
  backingEnvVar,
  copyBackingConfiguration,
  defaultKindValue,
  emptyS3,
  generateEncryptionKey,
  secretPlaceholder,
  setBackend,
  setDefaultKind,
  setHomeBucket,
  toPutBody,
  validateBackingConfiguration,
  validEncryptionKey
} from '~/components/admin/-backing-form'
import type { BackingConfiguration, BackingDefinition, S3BackingDefinition } from '~/types/admin'

const form = (): BackingConfiguration => ({ backings: [{ name: 'archive', backend: 'fs', path: '/mnt/archive' }], defaultBacking: 'archive', revision: 'abc', persistent: true })

const KEY = btoa(String.fromCharCode(...new Uint8Array(32).fill(7)))

const s3Row = (name = 'cold', over: Partial<S3BackingDefinition> = {}): BackingDefinition => ({
  name,
  backend: 's3',
  path: '',
  s3: { ...emptyS3(), endpoint: 'https://s3.example.com', region: 'eu-1', homeBucket: 'gos3-cold', homePrefix: 'data/', accessKey: 'AK', secretKey: 'SK', ...over }
})

describe('backing configuration', () => {
  it('accepts filesystem and memory backings and preserves the revision when copied', () => {
    const f = form()
    f.backings.push({ name: 'cache', backend: 'memory', path: '' })
    expect(validateBackingConfiguration(f)).toBeUndefined()
    const copy = copyBackingConfiguration(f)
    copy.backings[0]!.path = '/new'
    expect(f.backings[0]!.path).toBe('/mnt/archive')
    expect(copy.revision).toBe('abc')
  })

  it.each(['default', 'INVALID', '', '-archive', 'a'.repeat(33)])('rejects name %j', (name) => {
    const f = form()
    f.backings[0]!.name = name
    expect(validateBackingConfiguration(f)).toBeTruthy()
  })

  it('rejects duplicate names, paths, missing paths, and a removed default', () => {
    const f = form()
    f.backings.push({ ...f.backings[0]! })
    expect(validateBackingConfiguration(f)).toContain('already used')
    f.backings[1]!.name = 'spare'
    expect(validateBackingConfiguration(f)).toContain('own directory')
    f.backings.pop()
    f.backings[0]!.path = ' '
    expect(validateBackingConfiguration(f)).toContain('directory')
    f.backings = []
    expect(validateBackingConfiguration(f)).toContain('existing backing')
  })
})

describe('s3 backings', () => {
  it('switching the backend resets the path and adds or removes the s3 section', () => {
    const b: BackingDefinition = { name: 'x', backend: 'fs', path: '/mnt/x' }
    setBackend(b, 's3')
    expect(b).toEqual({ name: 'x', backend: 's3', path: '', s3: emptyS3() })
    b.s3!.homeBucket = 'kept'
    setBackend(b, 's3')
    expect(b.s3!.homeBucket).toBe('kept')
    setBackend(b, 'memory')
    expect(b).toEqual({ name: 'x', backend: 'memory', path: '' })
  })

  it('accepts a complete s3 backing and deep-copies its s3 section', () => {
    const f = form()
    f.backings.push(s3Row())
    expect(validateBackingConfiguration(f)).toBeUndefined()
    const copy = copyBackingConfiguration(f)
    copy.backings[1]!.s3!.homeBucket = 'other'
    expect(f.backings[1]!.s3!.homeBucket).toBe('gos3-cold')
  })

  it.each<[Partial<S3BackingDefinition>, string]>([
    [{ endpoint: '' }, 'endpoint'],
    [{ endpoint: 's3.example.com' }, 'http:// or https://'],
    [{ endpoint: 'ftp://s3.example.com' }, 'http:// or https://'],
    [{ endpoint: 'https://s3.example.com/bucket' }, 'path'],
    [{ homeBucket: 'Bad_Bucket' }, 'home bucket name'],
    [{ homeBucket: 'a' }, 'home bucket name'],
    [{ homeBucket: '_gos3' }, 'home bucket name'],
    [{ homePrefix: '/data' }, 'must not start with "/"'],
    [{ homeBucket: '', homePrefix: 'data/' }, 'needs a home bucket'],
    [{ homeBucket: '', homePrefix: '', defaultKind: 'wrapped' }, 'to create wrapped buckets by default'],
    [{ accessKey: ' ' }, 'access key'],
    [{ secretKey: '' }, 'secret key'],
    [{ partSize: 1024 }, 'part size'],
    [{ encryptionKey: 'not base64!' }, 'encryption key'],
    [{ encryptionKey: btoa('short') }, '32 bytes']
  ])('rejects %j', (over, message) => {
    const f = form()
    f.backings.push(s3Row('cold', over))
    expect(validateBackingConfiguration(f)).toContain(message)
  })

  it('accepts a stored or env-sourced secret, an endpoint with a trailing slash and a valid key', () => {
    const f = form()
    f.backings.push(s3Row('a', { secretKey: '', secretKeyConfigured: true, endpoint: 'http://minio:9000/', encryptionKey: KEY, partSize: 16 * 1024 * 1024 }))
    f.backings.push(s3Row('b', { secretKey: undefined, secretKeySource: 'env', homePrefix: 'other/' }))
    expect(validateBackingConfiguration(f)).toBeUndefined()
  })

  it('accepts backings without a home (records kept locally), native by default or not', () => {
    const f = form()
    f.backings.push(s3Row('a', { homeBucket: '', homePrefix: '' }), s3Row('b', { homeBucket: '', homePrefix: '', defaultKind: 'native' }))
    expect(validateBackingConfiguration(f)).toBeUndefined()
    // Without a system directory the records need a home.
    expect(validateBackingConfiguration({ ...f, persistent: false })).toContain('needs a home bucket: this server has no data directory')
    f.backings[1]!.s3!.homeBucket = 'gos3-b'
    f.backings[1]!.s3!.defaultKind = 'wrapped'
    expect(validateBackingConfiguration(f)).toBeUndefined()
  })

  it('rejects two s3 backings with overlapping homes at the same endpoint', () => {
    const f = form()
    f.backings.push(s3Row('a'), s3Row('b', { endpoint: 'https://S3.example.com/', homePrefix: 'data' }))
    expect(validateBackingConfiguration(f)).toContain('own home')
    f.backings[2]!.s3!.homePrefix = 'data/nested/'
    expect(validateBackingConfiguration(f)).toContain('own home')
    f.backings[2]!.s3!.homePrefix = ''
    expect(validateBackingConfiguration(f)).toContain('own home')
    f.backings[2]!.s3!.homePrefix = 'data2/'
    expect(validateBackingConfiguration(f)).toBeUndefined()
    // Other endpoints, other buckets and backings without a home never overlap.
    f.backings[2]!.s3!.homePrefix = 'data/'
    f.backings[2]!.s3!.endpoint = 'https://other.example.com'
    expect(validateBackingConfiguration(f)).toBeUndefined()
    f.backings.push(s3Row('c', { homeBucket: '', homePrefix: '' }), s3Row('d', { homeBucket: '', homePrefix: '' }))
    expect(validateBackingConfiguration(f)).toBeUndefined()
  })

  it('accepts the home bucket names the server accepts', () => {
    const f = form()
    f.backings.push(s3Row('a', { homeBucket: 'legacy_bucket' }), s3Row('b', { homeBucket: 'ab', homePrefix: '' }))
    expect(validateBackingConfiguration(f)).toBeUndefined()
  })

  it('keeps a loaded wrapped default kind from blocking the removal of the home', () => {
    const loaded = form()
    loaded.backings.push(s3Row('a', { defaultKind: 'wrapped' }))
    const f = copyBackingConfiguration(loaded)
    const s3 = f.backings[1]!.s3!
    // Clearing the home resets the loaded (not chosen) kind to automatic.
    setHomeBucket(s3, '', loaded.backings[1]!.s3)
    s3.homePrefix = ''
    expect(s3.defaultKind).toBe('')
    expect(validateBackingConfiguration(f, loaded)).toBeUndefined()
    // Unchanged from what was loaded: not refused (the server resets a derived kind).
    s3.defaultKind = 'wrapped'
    expect(validateBackingConfiguration(f, loaded)).toBeUndefined()
    // Chosen in the form (or a new backing): refused.
    expect(validateBackingConfiguration(f)).toContain('to create wrapped buckets by default')
    const native = form()
    native.backings.push(s3Row('a', { defaultKind: 'native' }))
    expect(validateBackingConfiguration(f, native)).toContain('to create wrapped buckets by default')
    // Setting a home keeps the kind; a kind chosen in the form is kept too.
    setHomeBucket(s3, 'gos3-a', loaded.backings[1]!.s3)
    expect(s3.defaultKind).toBe('wrapped')
    const chosen = { ...emptyS3(), homeBucket: 'x1', defaultKind: 'wrapped' as const }
    setHomeBucket(chosen, '', { defaultKind: 'native' })
    expect(chosen.defaultKind).toBe('wrapped')
  })

  it('maps the default kind to and from the select ("auto" = server default)', () => {
    const s3 = emptyS3()
    expect(defaultKindValue(s3)).toBe('auto')
    setDefaultKind(s3, 'native')
    expect(s3.defaultKind).toBe('native')
    expect(defaultKindValue(s3)).toBe('native')
    setDefaultKind(s3, 'auto')
    expect(s3.defaultKind).toBe('')
    setDefaultKind(s3, 'gateway')
    expect(s3.defaultKind).toBe('')
  })

  it('rejects a directory on an s3 backing', () => {
    const f = form()
    f.backings.push({ ...s3Row(), path: '/mnt/x' })
    expect(validateBackingConfiguration(f)).toContain('cannot have a directory')
  })

  it('builds a PUT body without read-only fields and blank secrets', () => {
    const f = form()
    f.backings.push(s3Row('a', { secretKey: '', encryptionKey: '', secretKeyConfigured: true, encryptionKeyConfigured: true, secretKeySource: 'env', encryptionKeySource: 'env', accessKeySource: 'env', envFields: ['secret_key'], homeBucket: ' gos3-cold ', defaultKind: 'native' }))
    f.backings.push(s3Row('b', { encryptionKey: KEY, partSize: 8 * 1024 * 1024 }))
    f.backings.push({ name: 'mem', backend: 'memory', path: '', s3: emptyS3() })
    const body = toPutBody(f)
    expect(body.revision).toBe('abc')
    expect(body.backings[0]).toEqual({ name: 'archive', backend: 'fs', path: '/mnt/archive' })
    expect(body.backings[1]).toEqual({
      name: 'a', backend: 's3', path: '',
      s3: { endpoint: 'https://s3.example.com', region: 'eu-1', homeBucket: 'gos3-cold', homePrefix: 'data/', defaultKind: 'native', addressing: '', accessKey: 'AK' }
    })
    expect(body.backings[2]!.s3).toMatchObject({ secretKey: 'SK', encryptionKey: KEY, partSize: 8 * 1024 * 1024 })
    expect(body.backings[3]).toEqual({ name: 'mem', backend: 'memory', path: '' })
    // The form itself is unchanged.
    expect(f.backings[1]!.s3!.secretKeyConfigured).toBe(true)
  })

  it('names the secret source in placeholders', () => {
    expect(backingEnvVar('cold-tier', 'SECRET_KEY')).toBe('GOS3_BACKING_COLD_TIER_SECRET_KEY')
    expect(secretPlaceholder('cold', emptyS3(), 'secret')).toBe('')
    expect(secretPlaceholder('cold', { ...emptyS3(), secretKeyConfigured: true }, 'secret')).toBe('Saved – leave blank to keep')
    expect(secretPlaceholder('cold', { ...emptyS3(), secretKeyConfigured: true, secretKeySource: 'env' }, 'secret')).toBe('From GOS3_BACKING_COLD_SECRET_KEY')
    expect(secretPlaceholder('cold', { ...emptyS3(), encryptionKeyConfigured: true, encryptionKeySource: 'env' }, 'encryption')).toBe('From GOS3_BACKING_COLD_ENCRYPTION_KEY')
    expect(secretPlaceholder('cold', { ...emptyS3(), secretKeyConfigured: true }, 'encryption')).toBe('')
  })

  it('generates random 32-byte base64 keys', () => {
    const a = generateEncryptionKey()
    expect(validEncryptionKey(a)).toBe(true)
    expect(atob(a)).toHaveLength(32)
    expect(generateEncryptionKey()).not.toBe(a)
    expect(validEncryptionKey('')).toBe(false)
  })
})
