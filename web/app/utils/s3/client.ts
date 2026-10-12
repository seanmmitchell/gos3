// Browser S3 client: SigV4 signing with aws4fetch, XML via DOMParser.
//
// Usage:
//   const s3 = new S3Client({ endpoint, region, accessKeyId, secretAccessKey, forcePathStyle: true })
//   const { buckets } = await s3.listBuckets()
//   const page = await s3.listObjectsV2('photos', { prefix: '2024/', delimiter: '/' })
//
// All methods throw S3Error on failure (see ./errors). Every request is signed
// with `x-amz-content-sha256: UNSIGNED-PAYLOAD` (aws4fetch default for s3), so
// bodies are never hashed in the browser. Bodies that S3 requires a checksum
// for carry Content-MD5.
import { AwsClient } from 'aws4fetch'
import type {
  AccessControlPolicy,
  CannedACL,
  ChecksumAlgorithm,
  CompletedPart,
  CompleteMultipartUploadResult,
  CopyObjectOptions,
  CopyObjectResult,
  CORSRule,
  CreateBucketOptions,
  DeleteObjectResult,
  DeleteObjectsResult,
  EncryptionConfiguration,
  GetObjectRangeResult,
  GetObjectTextResult,
  HeadResult,
  InitiateMultipartUploadResult,
  LegalHoldStatus,
  LifecycleRule,
  ListBucketsResult,
  ListMultipartUploadsParams,
  ListMultipartUploadsResult,
  ListObjectsV2Params,
  ListPartsResult,
  ListResult,
  ListVersionsParams,
  ListVersionsResult,
  ObjectIdentifier,
  ObjectLockConfiguration,
  ObjectLockMode,
  ObjectRef,
  ObjectRetention,
  ObjectVersion,
  PolicyStatus,
  PresignOptions,
  ProgressCallback,
  PutObjectOptions,
  PutObjectResult,
  S3Object,
  Tag,
  UploadLargeOptions,
  UploadOptions,
  VersioningConfiguration,
  VersioningStatus
} from '~/types/s3'
import {
  buildCompleteMultipartUpload,
  buildCors,
  buildCreateBucketConfiguration,
  buildDelete,
  buildEncryption,
  buildLegalHold,
  buildLifecycle,
  buildObjectLockConfiguration,
  buildRetention,
  buildTagging,
  buildVersioning,
  tagsToHeader
} from './builders'
import { isAbortError, networkError, parseS3Error, S3Error } from './errors'
import { md5Base64 } from './hash'
import {
  parseAccessControlPolicy,
  parseCompleteMultipartUpload,
  parseCopyObjectResult,
  parseCors,
  parseDeleteObjectsResult,
  parseEncryption,
  parseInitiateMultipartUpload,
  parseLegalHold,
  parseLifecycle,
  parseListBuckets,
  parseListMultipartUploads,
  parseListObjectsV2,
  parseListParts,
  parseListVersions,
  parseLocation,
  parseObjectLockConfiguration,
  parsePolicyStatus,
  parseRetention,
  parseTagging,
  parseVersioning
} from './parsers'
import { buildS3Url, encodeKey, encodeRfc3986, hasDotSegment, isUnaddressableBucket, normalizeEndpoint, type Query } from './url'
import { tryParseXml, unquoteETag } from './xml'

export { S3Error } from './errors'

const MiB = 1024 * 1024
export const MIN_PART_SIZE = 5 * MiB
export const MAX_PARTS = 10000
export const DEFAULT_PART_SIZE = 8 * MiB
export const DEFAULT_MULTIPART_THRESHOLD = 16 * MiB
export const MAX_DELETE_BATCH = 1000
export const MAX_PRESIGN_SECONDS = 7 * 24 * 3600

export type FetchLike = (input: Request) => Promise<Response>

export interface S3ClientConfig {
  endpoint: string
  /** Signing region (default us-east-1). */
  region?: string
  accessKeyId: string
  secretAccessKey: string
  sessionToken?: string
  /** Path-style (http://host/bucket/key) vs virtual-hosted (http://bucket.host/key). Default true. */
  forcePathStyle?: boolean
  /** Custom fetch (tests). Defaults to globalThis.fetch at call time. */
  fetch?: FetchLike
  /** Fixed signing time (tests), e.g. '20261009T120000Z'. */
  datetime?: string
}

export interface SendOptions {
  method: 'GET' | 'PUT' | 'POST' | 'DELETE' | 'HEAD'
  bucket?: string
  key?: string
  query?: Query
  headers?: Record<string, string | undefined>
  body?: string | Blob | ArrayBuffer | Uint8Array
  signal?: AbortSignal
  /** Add Content-MD5 for a string body. */
  md5?: boolean
  /** Non-2xx statuses that should not throw. */
  okStatus?: number[]
}

export interface SendResult {
  response: Response
  /** Response body as text ('' for HEAD). */
  body: string
}

/** Choose a multipart part size: ≥ 5 MiB, whole MiB, and ≤ 10000 parts. */
export function choosePartSize(size: number, requested?: number): number {
  let ps = Math.max(requested ?? DEFAULT_PART_SIZE, MIN_PART_SIZE, Math.ceil(size / MAX_PARTS))
  ps = Math.ceil(ps / MiB) * MiB
  return ps
}

/** RFC 2047 (base64) encodes non-ASCII header values, as the AWS SDKs do for metadata. */
export function encodeHeaderValue(v: string): string {
  // eslint-disable-next-line no-control-regex
  if (/^[\x20-\x7e]*$/.test(v)) return v
  const bytes = new TextEncoder().encode(v)
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return `=?UTF-8?B?${btoa(bin)}?=`
}

/**
 * Content-Disposition value safe to send as a header. Non-ASCII characters
 * are only allowed in the filename parameter: they are moved into an RFC
 * 5987 `filename*` parameter (with an ASCII `filename` fallback), as RFC
 * 6266 specifies. RFC 2047 encoded-words, which encodeHeaderValue produces
 * for user metadata, are not understood by browsers in this header. Throws
 * an Error for other non-ASCII content (it cannot be represented).
 */
export function contentDispositionHeader(v: string): string {
  // eslint-disable-next-line no-control-regex
  if (/^[\x20-\x7e\t]*$/.test(v)) return v
  // Split on ';' outside quoted strings.
  const parts: string[] = []
  let cur = ''
  let quoted = false
  for (let i = 0; i < v.length; i++) {
    const c = v[i]!
    if (quoted && c === '\\' && i + 1 < v.length) {
      cur += c + v[++i]
      continue
    }
    if (c === '"') quoted = !quoted
    if (c === ';' && !quoted) {
      parts.push(cur)
      cur = ''
    } else {
      cur += c
    }
  }
  parts.push(cur)
  const [type, ...params] = parts.map(p => p.trim()).filter(Boolean)
  const nonAscii = (s: string) => /[^\x20-\x7e\t]/.test(s)
  if (!type || nonAscii(type)) throw new Error('Content-Disposition: only the filename may contain non-ASCII characters (e.g. attachment; filename="résumé.pdf").')
  const out = [type]
  const hasExtended = params.some(p => /^filename\*\s*=/i.test(p))
  for (const p of params) {
    if (!nonAscii(p)) {
      out.push(p)
      continue
    }
    const m = p.match(/^filename\s*=\s*(.*)$/is)
    if (!m) throw new Error('Content-Disposition: only the filename may contain non-ASCII characters (e.g. attachment; filename="résumé.pdf").')
    let name = m[1]!.trim()
    if (name.length >= 2 && name.startsWith('"') && name.endsWith('"')) name = name.slice(1, -1).replace(/\\(.)/g, '$1')
    const ascii = name.replace(/[^\x20-\x7e]/g, '_').replace(/["\\]/g, '_')
    out.push(`filename="${ascii}"`)
    if (!hasExtended) {
      out.push(`filename*=UTF-8''${encodeURIComponent(name).replace(/['()*]/g, c => `%${c.charCodeAt(0).toString(16).toUpperCase()}`)}`)
    }
  }
  return out.join('; ')
}

/** Decodes RFC 2047 encoded-words (B and Q encodings, UTF-8 / ISO-8859-1). */
export function decodeHeaderValue(v: string): string {
  return v.replace(/=\?([^?]+)\?([BbQq])\?([^?]*)\?=/g, (whole, charset: string, enc: string, data: string) => {
    try {
      let bytes: Uint8Array
      if (enc.toUpperCase() === 'B') {
        const bin = atob(data)
        bytes = Uint8Array.from(bin, c => c.charCodeAt(0))
      } else {
        const s = data.replace(/_/g, ' ').replace(/=([0-9A-Fa-f]{2})/g, (_m, h: string) => String.fromCharCode(parseInt(h, 16)))
        bytes = Uint8Array.from(s, c => c.charCodeAt(0))
      }
      return new TextDecoder(charset.toLowerCase() === 'utf-8' ? 'utf-8' : 'latin1').decode(bytes)
    } catch {
      return whole
    }
  })
}

const CHECKSUM_ALGOS: ChecksumAlgorithm[] = ['CRC32', 'CRC32C', 'CRC64NVME', 'SHA1', 'SHA256']

/**
 * Parses object response headers into a HeadResult. Pass `crossOrigin` for
 * responses read cross-origin (Response.type 'cors'): the browser then only
 * shows the headers the server's CORS rule exposes, and `headersHidden` is
 * set unless the Date header (sent by every S3 server, never safelisted and
 * practically never listed explicitly) is visible, which means that every
 * header is exposed ("*").
 */
export function parseHeadHeaders(headers: Headers, opts: { crossOrigin?: boolean } = {}): HeadResult {
  const all: Record<string, string> = {}
  const metadata: Record<string, string> = {}
  headers.forEach((value, name) => {
    const n = name.toLowerCase()
    all[n] = value
    if (n.startsWith('x-amz-meta-')) metadata[n.slice('x-amz-meta-'.length)] = decodeHeaderValue(value)
  })
  const checksums: HeadResult['checksums'] = {}
  for (const a of CHECKSUM_ALGOS) {
    const v = all[`x-amz-checksum-${a.toLowerCase()}`]
    if (v) checksums[a] = v
  }
  const int = (v: string | undefined) => (v === undefined || v === '' ? undefined : Number(v))
  const mode = all['x-amz-object-lock-mode']
  const hold = all['x-amz-object-lock-legal-hold']
  return {
    contentLength: int(all['content-length']) ?? 0,
    contentType: all['content-type'],
    contentEncoding: all['content-encoding'],
    // Decoded: earlier UI versions stored non-ASCII values as RFC 2047 words.
    contentDisposition: all['content-disposition'] === undefined ? undefined : decodeHeaderValue(all['content-disposition']),
    contentLanguage: all['content-language'],
    cacheControl: all['cache-control'],
    expires: all['expires'],
    etag: unquoteETag(all['etag']),
    lastModified: all['last-modified'],
    versionId: all['x-amz-version-id'],
    deleteMarker: all['x-amz-delete-marker'] === 'true' ? true : undefined,
    storageClass: all['x-amz-storage-class'],
    acceptRanges: all['accept-ranges'],
    metadata,
    checksums,
    checksumType: all['x-amz-checksum-type'],
    objectLockMode: mode === 'GOVERNANCE' || mode === 'COMPLIANCE' ? (mode as ObjectLockMode) : undefined,
    objectLockRetainUntilDate: all['x-amz-object-lock-retain-until-date'],
    objectLockLegalHold: hold === 'ON' || hold === 'OFF' ? hold : undefined,
    serverSideEncryption: all['x-amz-server-side-encryption'],
    sseKmsKeyId: all['x-amz-server-side-encryption-aws-kms-key-id'],
    tagCount: int(all['x-amz-tagging-count']),
    partsCount: int(all['x-amz-mp-parts-count']),
    replicationStatus: all['x-amz-replication-status'],
    restore: all['x-amz-restore'],
    websiteRedirectLocation: all['x-amz-website-redirect-location'],
    headersHidden: opts.crossOrigin && all.date === undefined ? true : undefined,
    headers: all
  }
}

function parseXhrHeaders(raw: string): Headers {
  const h = new Headers()
  for (const line of raw.trim().split(/[\r\n]+/)) {
    const i = line.indexOf(':')
    if (i <= 0) continue
    try {
      h.append(line.slice(0, i).trim(), line.slice(i + 1).trim())
    } catch {
      // ignore invalid header lines
    }
  }
  return h
}

function abortedError(): S3Error {
  return new S3Error({ code: 'Aborted', message: 'The request was aborted.', status: 0 })
}

/** Client error code for keys a browser cannot put into a request URL (see hasDotSegment). */
export const UNADDRESSABLE_KEY = 'UnaddressableKey'

/** The error thrown for path-addressed requests on keys with '.' / '..' segments. */
export function unaddressableKeyError(key: string): S3Error {
  return new S3Error({
    code: UNADDRESSABLE_KEY,
    message: `The key “${key}” contains a “.” or “..” path segment. Browsers resolve such segments in request URLs, so the request would address a different object; it was not sent. Use an S3 client (e.g. the AWS CLI) for this object, or rename it.`,
    status: 0,
    details: { Key: key }
  })
}

/**
 * The error thrown for bucket names that would change the request path
 * ('.', '..', names with '/' or '\'; see isUnaddressableBucket). The request
 * is not sent.
 */
export function invalidBucketError(bucket: string): S3Error {
  return new S3Error({
    code: 'InvalidBucketName',
    message: `“${bucket}” is not a valid bucket name; the request was not sent.`,
    status: 0,
    details: { BucketName: bucket }
  })
}

function isRetryable(e: unknown): boolean {
  if (!(e instanceof S3Error)) return false
  if (e.code === 'Aborted') return false
  return e.code === 'NetworkError' || e.status >= 500 || e.status === 429 || e.code === 'RequestTimeout'
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(abortedError())
    const t = setTimeout(resolve, ms)
    signal?.addEventListener('abort', () => {
      clearTimeout(t)
      reject(abortedError())
    }, { once: true })
  })
}

/** Builds request headers for object writes (PutObject, CreateMultipartUpload, CopyObject REPLACE). */
function objectWriteHeaders(opts: Pick<PutObjectOptions, 'contentType' | 'cacheControl' | 'contentDisposition' | 'contentEncoding' | 'contentLanguage' | 'metadata' | 'tags' | 'acl' | 'storageClass' | 'headers'>): Record<string, string> {
  const h: Record<string, string> = {}
  if (opts.contentType) h['Content-Type'] = opts.contentType
  if (opts.cacheControl) h['Cache-Control'] = opts.cacheControl
  if (opts.contentDisposition) h['Content-Disposition'] = contentDispositionHeader(opts.contentDisposition)
  if (opts.contentEncoding) h['Content-Encoding'] = opts.contentEncoding
  if (opts.contentLanguage) h['Content-Language'] = opts.contentLanguage
  if (opts.acl) h['x-amz-acl'] = opts.acl
  if (opts.storageClass) h['x-amz-storage-class'] = opts.storageClass
  for (const [k, v] of Object.entries(opts.metadata ?? {})) {
    const name = k.trim().toLowerCase()
    if (name) h[`x-amz-meta-${name}`] = encodeHeaderValue(v)
  }
  if (opts.tags?.length) h['x-amz-tagging'] = tagsToHeader(opts.tags)
  Object.assign(h, opts.headers ?? {})
  return h
}

/** x-amz-copy-source header value for an object (URL-encoded). */
export function copySourceHeader(src: ObjectRef): string {
  let v = `${encodeRfc3986(src.bucket)}/${encodeKey(src.key)}`
  if (src.versionId) v += `?versionId=${encodeRfc3986(src.versionId)}`
  return v
}

/** Returns null when err is a 404 with one of the given codes, else rethrows. */
function nullOn404<T>(codes: string[]): (e: unknown) => T | null {
  return (e: unknown) => {
    if (e instanceof S3Error && e.status === 404 && codes.includes(e.code)) return null
    throw e
  }
}

export class S3Client {
  readonly endpoint: string
  readonly region: string
  readonly forcePathStyle: boolean
  readonly accessKeyId: string
  private readonly aws: AwsClient
  private readonly fetchFn: FetchLike
  private readonly datetime?: string

  constructor(cfg: S3ClientConfig) {
    this.endpoint = normalizeEndpoint(cfg.endpoint)
    this.region = cfg.region || 'us-east-1'
    this.forcePathStyle = cfg.forcePathStyle ?? true
    this.accessKeyId = cfg.accessKeyId
    this.datetime = cfg.datetime
    this.aws = new AwsClient({
      accessKeyId: cfg.accessKeyId,
      secretAccessKey: cfg.secretAccessKey,
      sessionToken: cfg.sessionToken,
      service: 's3',
      region: this.region,
      retries: 0
    })
    this.fetchFn = cfg.fetch ?? (req => globalThis.fetch(req))
  }

  // -------------------------------------------------------------------------
  // Low level
  // -------------------------------------------------------------------------

  /**
   * Unsigned request URL for bucket/key (+ query). Throws UNADDRESSABLE_KEY
   * for keys with '.' / '..' segments: the URL would address another object.
   */
  url(bucket?: string, key?: string, query?: Query): URL {
    if (bucket !== undefined && isUnaddressableBucket(bucket)) throw invalidBucketError(bucket)
    if (key && hasDotSegment(key)) throw unaddressableKeyError(key)
    return buildS3Url({ endpoint: this.endpoint, forcePathStyle: this.forcePathStyle }, bucket, key, query)
  }

  /** Signs a request (Authorization header) and returns it without sending. */
  async sign(url: URL | string, init: { method: string, headers?: HeadersInit, body?: BodyInit | null, signal?: AbortSignal }): Promise<Request> {
    return this.aws.sign(url.toString(), {
      method: init.method,
      headers: init.headers,
      body: init.body ?? undefined,
      signal: init.signal,
      aws: { datetime: this.datetime }
    } as RequestInit)
  }

  /** Signs and sends a request; throws S3Error on non-2xx (except okStatus). */
  async send(opts: SendOptions): Promise<SendResult> {
    const url = this.url(opts.bucket, opts.key, opts.query)
    const headers = new Headers()
    for (const [k, v] of Object.entries(opts.headers ?? {})) if (v !== undefined) headers.set(k, v)
    if (opts.md5 && typeof opts.body === 'string') headers.set('Content-MD5', md5Base64(opts.body))
    if (opts.signal?.aborted) throw abortedError()
    let response: Response
    try {
      const req = await this.sign(url, { method: opts.method, headers, body: opts.body as BodyInit | undefined, signal: opts.signal })
      response = await this.fetchFn(req)
    } catch (e) {
      throw networkError(e, url.toString())
    }
    let body = ''
    if (opts.method !== 'HEAD') {
      try {
        body = await response.text()
      } catch (e) {
        throw networkError(e, url.toString())
      }
    }
    if (!response.ok && !(opts.okStatus ?? []).includes(response.status)) {
      throw parseS3Error(response.status, body, response.headers)
    }
    return { response, body }
  }

  /**
   * PUT/POST a Blob with XMLHttpRequest so upload progress can be reported.
   * Headers are signed by aws4fetch first (UNSIGNED-PAYLOAD).
   */
  private async xhrSend(method: 'PUT' | 'POST', url: URL, headers: Record<string, string>, body: Blob, onProgress?: ProgressCallback, signal?: AbortSignal): Promise<{ status: number, headers: Headers, body: string }> {
    if (signal?.aborted) throw abortedError()
    const req = await this.sign(url, { method, headers })
    // The abort listener is attached below: an abort during signing would be missed.
    if (signal?.aborted) throw abortedError()
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest()
      xhr.open(method, req.url)
      req.headers.forEach((value, name) => {
        if (name === 'host' || name === 'content-length') return
        xhr.setRequestHeader(name, value)
      })
      const onAbort = () => xhr.abort()
      signal?.addEventListener('abort', onAbort, { once: true })
      const cleanup = () => signal?.removeEventListener('abort', onAbort)
      xhr.upload.onprogress = (e: ProgressEvent) => {
        onProgress?.({ loaded: e.loaded, total: e.lengthComputable ? e.total : body.size })
      }
      xhr.onload = () => {
        cleanup()
        resolve({ status: xhr.status, headers: parseXhrHeaders(xhr.getAllResponseHeaders()), body: xhr.responseText ?? '' })
      }
      xhr.onerror = () => {
        cleanup()
        reject(networkError(new TypeError('XMLHttpRequest failed'), url.toString()))
      }
      xhr.ontimeout = xhr.onerror
      xhr.onabort = () => {
        cleanup()
        reject(abortedError())
      }
      xhr.send(body)
    })
  }

  /** Presigned URL for any method (query-string SigV4). */
  async presign(method: 'GET' | 'PUT' | 'HEAD' | 'DELETE', bucket: string, key: string, expiresSeconds = 3600, query: Query = {}): Promise<string> {
    const exp = Math.max(1, Math.min(MAX_PRESIGN_SECONDS, Math.floor(expiresSeconds)))
    const url = this.url(bucket, key, { ...query, 'X-Amz-Expires': exp })
    const req = await this.aws.sign(url.toString(), { method, aws: { signQuery: true, datetime: this.datetime } } as RequestInit)
    return req.url
  }

  // -------------------------------------------------------------------------
  // Service / buckets
  // -------------------------------------------------------------------------

  async listBuckets(signal?: AbortSignal): Promise<ListBucketsResult> {
    const { body } = await this.send({ method: 'GET', signal })
    return parseListBuckets(body)
  }

  async createBucket(name: string, opts: CreateBucketOptions = {}): Promise<void> {
    const body = buildCreateBucketConfiguration(opts.region)
    await this.send({
      method: 'PUT',
      bucket: name,
      body: body || undefined,
      headers: {
        'Content-Type': body ? 'application/xml' : undefined,
        'x-amz-bucket-object-lock-enabled': opts.objectLock ? 'true' : undefined,
        'x-amz-acl': opts.acl
      }
    })
  }

  async deleteBucket(name: string): Promise<void> {
    await this.send({ method: 'DELETE', bucket: name })
  }

  /** HeadBucket; resolves with the bucket region when the server reports it. */
  async headBucket(name: string, signal?: AbortSignal): Promise<{ region?: string }> {
    const { response } = await this.send({ method: 'HEAD', bucket: name, signal })
    return { region: response.headers.get('x-amz-bucket-region') ?? undefined }
  }

  /** True if the bucket exists (and is accessible); false on 404. */
  async bucketExists(name: string): Promise<boolean> {
    try {
      await this.headBucket(name)
      return true
    } catch (e) {
      if (e instanceof S3Error && e.status === 404) return false
      throw e
    }
  }

  async getBucketLocation(name: string): Promise<string> {
    const { body } = await this.send({ method: 'GET', bucket: name, query: { location: '' } })
    return parseLocation(body)
  }

  // -------------------------------------------------------------------------
  // Listing
  // -------------------------------------------------------------------------

  async listObjectsV2(bucket: string, params: ListObjectsV2Params = {}, signal?: AbortSignal): Promise<ListResult> {
    const { body } = await this.send({
      method: 'GET',
      bucket,
      signal,
      query: {
        'list-type': 2,
        // Keys may contain characters XML 1.0 cannot carry (e.g. U+0001):
        // ask for URL-encoded keys, as the AWS SDKs do (parsers decode them).
        'encoding-type': 'url',
        'prefix': params.prefix,
        'delimiter': params.delimiter,
        'continuation-token': params.continuationToken,
        'max-keys': params.maxKeys,
        'start-after': params.startAfter,
        'fetch-owner': params.fetchOwner ? 'true' : undefined
      }
    })
    return parseListObjectsV2(body)
  }

  /** Iterates over every page of ListObjectsV2. */
  async *listObjectsV2Pages(bucket: string, params: ListObjectsV2Params = {}, signal?: AbortSignal): AsyncGenerator<ListResult> {
    let token = params.continuationToken
    for (;;) {
      const page = await this.listObjectsV2(bucket, { ...params, continuationToken: token }, signal)
      yield page
      if (!page.isTruncated || !page.nextContinuationToken) return
      token = page.nextContinuationToken
    }
  }

  /** Iterates over every object (no common prefixes unless you pass a delimiter and use pages). */
  async *listAllObjects(bucket: string, params: Omit<ListObjectsV2Params, 'continuationToken'> = {}, signal?: AbortSignal): AsyncGenerator<S3Object> {
    for await (const page of this.listObjectsV2Pages(bucket, params, signal)) {
      yield * page.objects
    }
  }

  async listObjectVersions(bucket: string, params: ListVersionsParams = {}, signal?: AbortSignal): Promise<ListVersionsResult> {
    const { body } = await this.send({
      method: 'GET',
      bucket,
      signal,
      query: {
        'versions': '',
        'encoding-type': 'url',
        'prefix': params.prefix,
        'delimiter': params.delimiter,
        'key-marker': params.keyMarker,
        'version-id-marker': params.versionIdMarker,
        'max-keys': params.maxKeys
      }
    })
    return parseListVersions(body)
  }

  async *listObjectVersionsPages(bucket: string, params: ListVersionsParams = {}, signal?: AbortSignal): AsyncGenerator<ListVersionsResult> {
    let keyMarker = params.keyMarker
    let versionIdMarker = params.versionIdMarker
    for (;;) {
      const page = await this.listObjectVersions(bucket, { ...params, keyMarker, versionIdMarker }, signal)
      yield page
      if (!page.isTruncated || (!page.nextKeyMarker && !page.nextVersionIdMarker)) return
      keyMarker = page.nextKeyMarker
      versionIdMarker = page.nextVersionIdMarker
    }
  }

  /** All versions (and delete markers) of exactly one key. */
  async listVersionsOfKey(bucket: string, key: string, signal?: AbortSignal): Promise<ObjectVersion[]> {
    const out: ObjectVersion[] = []
    for await (const page of this.listObjectVersionsPages(bucket, { prefix: key }, signal)) {
      for (const v of page.versions) if (v.key === key) out.push(v)
      // Versions are sorted by key; once we pass our key we can stop.
      if (page.versions.some(v => v.key > key)) break
    }
    return out
  }

  // -------------------------------------------------------------------------
  // Objects
  // -------------------------------------------------------------------------

  async headObject(bucket: string, key: string, opts: { versionId?: string, checksumMode?: boolean, signal?: AbortSignal } = {}): Promise<HeadResult> {
    const { response } = await this.send({
      method: 'HEAD',
      bucket,
      key,
      signal: opts.signal,
      query: { versionId: opts.versionId },
      headers: { 'x-amz-checksum-mode': opts.checksumMode === false ? undefined : 'ENABLED' }
    })
    return parseHeadHeaders(response.headers, { crossOrigin: response.type === 'cors' })
  }

  /** GET with a byte range (inclusive end). */
  async getObjectRange(bucket: string, key: string, opts: { start?: number, end?: number, versionId?: string, signal?: AbortSignal } = {}): Promise<GetObjectRangeResult> {
    const start = opts.start ?? 0
    const range = opts.end !== undefined ? `bytes=${start}-${opts.end}` : `bytes=${start}-`
    const url = this.url(bucket, key, { versionId: opts.versionId })
    if (opts.signal?.aborted) throw abortedError()
    let response: Response
    try {
      const req = await this.sign(url, { method: 'GET', headers: { Range: range }, signal: opts.signal })
      response = await this.fetchFn(req)
    } catch (e) {
      throw networkError(e, url.toString())
    }
    if (!response.ok) {
      const body = await response.text().catch(() => '')
      throw parseS3Error(response.status, body, response.headers)
    }
    const blob = await response.blob()
    const contentRange = response.headers.get('content-range') ?? undefined
    const m = contentRange?.match(/\/(\d+)\s*$/)
    const head = parseHeadHeaders(response.headers)
    const totalSize = m ? Number(m[1]) : response.status === 200 ? blob.size : undefined
    return { blob, contentType: head.contentType, totalSize, contentRange, head }
  }

  /** Whole object as a Blob (prefer presigned URLs for downloads). */
  async getObjectBlob(bucket: string, key: string, opts: { versionId?: string, signal?: AbortSignal } = {}): Promise<Blob> {
    const url = this.url(bucket, key, { versionId: opts.versionId })
    let response: Response
    try {
      response = await this.fetchFn(await this.sign(url, { method: 'GET', signal: opts.signal }))
    } catch (e) {
      throw networkError(e, url.toString())
    }
    if (!response.ok) throw parseS3Error(response.status, await response.text().catch(() => ''), response.headers)
    return response.blob()
  }

  /** First maxBytes (default 256 KiB) of an object decoded as UTF-8, for previews. */
  async getObjectText(bucket: string, key: string, opts: { maxBytes?: number, versionId?: string, signal?: AbortSignal } = {}): Promise<GetObjectTextResult> {
    const maxBytes = opts.maxBytes ?? 256 * 1024
    try {
      const r = await this.getObjectRange(bucket, key, { start: 0, end: maxBytes - 1, versionId: opts.versionId, signal: opts.signal })
      let blob = r.blob
      let truncated = (r.totalSize ?? blob.size) > maxBytes
      if (blob.size > maxBytes) {
        blob = blob.slice(0, maxBytes)
        truncated = true
      }
      const buf = await blob.arrayBuffer()
      const text = new TextDecoder('utf-8', { fatal: false }).decode(buf)
      return { text, truncated, contentType: r.contentType, size: r.totalSize }
    } catch (e) {
      // Range requests on empty objects answer 416.
      if (e instanceof S3Error && e.status === 416) return { text: '', truncated: false, size: 0 }
      throw e
    }
  }

  /** Presigned GET URL (downloads / share links). */
  presignGetUrl(bucket: string, key: string, opts: PresignOptions = {}): Promise<string> {
    return this.presign('GET', bucket, key, opts.expiresSeconds ?? 3600, {
      'versionId': opts.versionId,
      'response-content-disposition': opts.responseContentDisposition,
      'response-content-type': opts.responseContentType
    })
  }

  /** Presigned PUT URL (uploads by third parties). */
  presignPutUrl(bucket: string, key: string, opts: { expiresSeconds?: number } = {}): Promise<string> {
    return this.presign('PUT', bucket, key, opts.expiresSeconds ?? 3600)
  }

  /**
   * PutObject. With onProgress (and XMLHttpRequest available) the body is
   * sent via XHR to report upload progress; otherwise via fetch.
   */
  async putObject(bucket: string, key: string, body: Blob | string | ArrayBuffer | Uint8Array, opts: PutObjectOptions = {}): Promise<PutObjectResult> {
    const headers = objectWriteHeaders({
      ...opts,
      contentType: opts.contentType || (body instanceof Blob && body.type) || 'application/octet-stream'
    })
    if (opts.onProgress && typeof XMLHttpRequest !== 'undefined') {
      const blob = body instanceof Blob ? body : new Blob([body as BlobPart])
      const url = this.url(bucket, key)
      const r = await this.xhrSend('PUT', url, headers, blob, opts.onProgress, opts.signal)
      if (r.status < 200 || r.status >= 300) throw parseS3Error(r.status, r.body, r.headers)
      return { etag: unquoteETag(r.headers.get('etag')), versionId: r.headers.get('x-amz-version-id') ?? undefined }
    }
    const { response } = await this.send({ method: 'PUT', bucket, key, body, headers, signal: opts.signal })
    const size = body instanceof Blob ? body.size : typeof body === 'string' ? new TextEncoder().encode(body).length : body.byteLength
    opts.onProgress?.({ loaded: size, total: size })
    return { etag: unquoteETag(response.headers.get('etag')), versionId: response.headers.get('x-amz-version-id') ?? undefined }
  }

  /** Uploads a Blob/File: PutObject when small, multipart otherwise. */
  async upload(bucket: string, key: string, file: Blob, opts: UploadOptions = {}): Promise<PutObjectResult> {
    const threshold = opts.multipartThreshold ?? DEFAULT_MULTIPART_THRESHOLD
    if (file.size <= threshold) return this.putObject(bucket, key, file, opts)
    const r = await this.uploadLarge(bucket, key, file, opts)
    return { etag: r.etag, versionId: r.versionId }
  }

  /**
   * DeleteObject. Keys with '.' / '..' segments cannot be put into a URL;
   * they are deleted with DeleteObjects (key in the request body) instead.
   */
  async deleteObject(bucket: string, key: string, versionId?: string, opts: { bypassGovernanceRetention?: boolean, signal?: AbortSignal } = {}): Promise<DeleteObjectResult> {
    if (hasDotSegment(key)) {
      const r = await this.deleteObjects(bucket, [{ key, versionId }], { quiet: false, ...opts })
      const err = r.errors[0]
      if (err) throw new S3Error({ code: err.code, message: err.message, status: 0, details: { Key: err.key } })
      const d = r.deleted[0]
      return { versionId: d?.deleteMarkerVersionId ?? d?.versionId, deleteMarker: d?.deleteMarker || undefined }
    }
    const { response } = await this.send({
      method: 'DELETE',
      bucket,
      key,
      signal: opts.signal,
      query: { versionId },
      headers: { 'x-amz-bypass-governance-retention': opts.bypassGovernanceRetention ? 'true' : undefined }
    })
    return {
      versionId: response.headers.get('x-amz-version-id') ?? undefined,
      deleteMarker: response.headers.get('x-amz-delete-marker') === 'true' ? true : undefined
    }
  }

  /** DeleteObjects in batches of 1000; per-key failures are returned in errors. */
  async deleteObjects(bucket: string, objects: ObjectIdentifier[], opts: { quiet?: boolean, bypassGovernanceRetention?: boolean, signal?: AbortSignal } = {}): Promise<DeleteObjectsResult> {
    const result: DeleteObjectsResult = { deleted: [], errors: [] }
    for (let i = 0; i < objects.length; i += MAX_DELETE_BATCH) {
      const batch = objects.slice(i, i + MAX_DELETE_BATCH)
      const { body } = await this.send({
        method: 'POST',
        bucket,
        query: { delete: '' },
        body: buildDelete(batch, opts.quiet),
        md5: true,
        signal: opts.signal,
        headers: {
          'Content-Type': 'application/xml',
          'x-amz-bypass-governance-retention': opts.bypassGovernanceRetention ? 'true' : undefined
        }
      })
      const r = parseDeleteObjectsResult(body)
      result.deleted.push(...r.deleted)
      result.errors.push(...r.errors)
    }
    return result
  }

  /**
   * Deletes every object under prefix (a "folder"). With allVersions, every
   * version and delete marker is removed (permanently).
   */
  async deletePrefix(bucket: string, prefix: string, opts: { allVersions?: boolean, signal?: AbortSignal, onProgress?: (deleted: number) => void } = {}): Promise<DeleteObjectsResult> {
    const total: DeleteObjectsResult = { deleted: [], errors: [] }
    const flush = async (ids: ObjectIdentifier[]) => {
      if (!ids.length) return
      const r = await this.deleteObjects(bucket, ids, { quiet: false, signal: opts.signal })
      total.deleted.push(...r.deleted)
      total.errors.push(...r.errors)
      opts.onProgress?.(total.deleted.length)
    }
    if (opts.allVersions) {
      for await (const page of this.listObjectVersionsPages(bucket, { prefix }, opts.signal)) {
        await flush(page.versions.map(v => ({ key: v.key, versionId: v.versionId })))
      }
    } else {
      for await (const page of this.listObjectsV2Pages(bucket, { prefix }, opts.signal)) {
        await flush(page.objects.map(o => ({ key: o.key })))
      }
    }
    return total
  }

  /** CopyObject (≤ 5 GiB). metadataDirective REPLACE replaces metadata/content headers. */
  async copyObject(src: ObjectRef, dst: { bucket: string, key: string }, opts: CopyObjectOptions = {}): Promise<CopyObjectResult> {
    const replace = opts.metadataDirective === 'REPLACE'
    const headers: Record<string, string> = {
      'x-amz-copy-source': copySourceHeader(src),
      ...(replace
        ? objectWriteHeaders({ contentType: opts.contentType, cacheControl: opts.cacheControl, contentDisposition: opts.contentDisposition, metadata: opts.metadata })
        : {})
    }
    if (opts.metadataDirective) headers['x-amz-metadata-directive'] = opts.metadataDirective
    if (opts.taggingDirective) headers['x-amz-tagging-directive'] = opts.taggingDirective
    if (opts.tags && opts.taggingDirective === 'REPLACE') headers['x-amz-tagging'] = tagsToHeader(opts.tags)
    if (opts.acl) headers['x-amz-acl'] = opts.acl
    if (opts.storageClass) headers['x-amz-storage-class'] = opts.storageClass
    Object.assign(headers, opts.headers ?? {})
    const { response, body } = await this.send({ method: 'PUT', bucket: dst.bucket, key: dst.key, headers, signal: opts.signal })
    // S3 may answer 200 with an <Error> body.
    if (tryParseXml(body)?.localName === 'Error') throw parseS3Error(500, body, response.headers)
    return parseCopyObjectResult(body, response.headers)
  }

  /** Copy then delete the source (rename). */
  async moveObject(src: ObjectRef, dst: { bucket: string, key: string }, opts: CopyObjectOptions = {}): Promise<CopyObjectResult> {
    const r = await this.copyObject(src, dst, opts)
    await this.deleteObject(src.bucket, src.key, src.versionId)
    return r
  }

  // -------------------------------------------------------------------------
  // Multipart
  // -------------------------------------------------------------------------

  async createMultipartUpload(bucket: string, key: string, opts: PutObjectOptions = {}): Promise<InitiateMultipartUploadResult> {
    const headers = objectWriteHeaders({ ...opts, contentType: opts.contentType || 'application/octet-stream' })
    const { body } = await this.send({ method: 'POST', bucket, key, query: { uploads: '' }, headers, signal: opts.signal })
    return parseInitiateMultipartUpload(body)
  }

  /** Uploads one part; resolves with its ETag (unquoted). */
  async uploadPart(bucket: string, key: string, uploadId: string, partNumber: number, body: Blob, opts: { onProgress?: ProgressCallback, signal?: AbortSignal } = {}): Promise<string> {
    const query = { partNumber, uploadId }
    let etag: string | undefined
    if (opts.onProgress && typeof XMLHttpRequest !== 'undefined') {
      const r = await this.xhrSend('PUT', this.url(bucket, key, query), {}, body, opts.onProgress, opts.signal)
      if (r.status < 200 || r.status >= 300) throw parseS3Error(r.status, r.body, r.headers)
      etag = unquoteETag(r.headers.get('etag'))
    } else {
      const { response } = await this.send({ method: 'PUT', bucket, key, query, body, signal: opts.signal })
      etag = unquoteETag(response.headers.get('etag'))
      opts.onProgress?.({ loaded: body.size, total: body.size })
    }
    if (!etag) {
      throw new S3Error({
        code: 'MissingETag',
        message: 'The server did not expose the ETag header. Add "ETag" to the CORS ExposeHeaders setting.',
        status: 0
      })
    }
    return etag
  }

  async completeMultipartUpload(bucket: string, key: string, uploadId: string, parts: CompletedPart[], signal?: AbortSignal): Promise<CompleteMultipartUploadResult> {
    const { response, body } = await this.send({
      method: 'POST',
      bucket,
      key,
      query: { uploadId },
      body: buildCompleteMultipartUpload(parts),
      headers: { 'Content-Type': 'application/xml' },
      signal
    })
    if (tryParseXml(body)?.localName === 'Error') throw parseS3Error(500, body, response.headers)
    return parseCompleteMultipartUpload(body, response.headers)
  }

  async abortMultipartUpload(bucket: string, key: string, uploadId: string): Promise<void> {
    await this.send({ method: 'DELETE', bucket, key, query: { uploadId } })
  }

  async listParts(bucket: string, key: string, uploadId: string, opts: { partNumberMarker?: number, maxParts?: number } = {}): Promise<ListPartsResult> {
    const { body } = await this.send({
      method: 'GET',
      bucket,
      key,
      query: { 'uploadId': uploadId, 'part-number-marker': opts.partNumberMarker, 'max-parts': opts.maxParts }
    })
    return parseListParts(body)
  }

  async listMultipartUploads(bucket: string, params: ListMultipartUploadsParams = {}): Promise<ListMultipartUploadsResult> {
    const { body } = await this.send({
      method: 'GET',
      bucket,
      query: {
        'uploads': '',
        'encoding-type': 'url',
        'prefix': params.prefix,
        'delimiter': params.delimiter,
        'key-marker': params.keyMarker,
        'upload-id-marker': params.uploadIdMarker,
        'max-uploads': params.maxUploads
      }
    })
    return parseListMultipartUploads(body)
  }

  /**
   * Multipart upload of a Blob with parallel parts, progress, cancellation
   * (opts.signal) and per-part retries. The upload is aborted server-side on
   * failure or cancel.
   */
  async uploadLarge(bucket: string, key: string, file: Blob, opts: UploadLargeOptions = {}): Promise<CompleteMultipartUploadResult> {
    const size = file.size
    const partSize = choosePartSize(size, opts.partSize)
    const partCount = Math.max(1, Math.ceil(size / partSize))
    const concurrency = Math.max(1, Math.min(opts.concurrency ?? 4, partCount))
    const retries = opts.partRetries ?? 2

    if (opts.signal?.aborted) throw abortedError()
    const { uploadId } = await this.createMultipartUpload(bucket, key, { ...opts, contentType: opts.contentType || file.type || undefined, signal: opts.signal })

    const internal = new AbortController()
    const onOuterAbort = () => internal.abort()
    opts.signal?.addEventListener('abort', onOuterAbort, { once: true })
    // Canceled while the upload was being created (before the listener existed).
    if (opts.signal?.aborted) {
      opts.signal.removeEventListener('abort', onOuterAbort)
      await this.abortMultipartUpload(bucket, key, uploadId).catch(() => undefined)
      throw abortedError()
    }

    const loaded = new Array<number>(partCount).fill(0)
    const report = () => opts.onProgress?.({ loaded: loaded.reduce((a, b) => a + b, 0), total: size })
    const parts: CompletedPart[] = []
    let next = 0

    const worker = async () => {
      for (;;) {
        if (internal.signal.aborted) throw abortedError()
        const idx = next++
        if (idx >= partCount) return
        const blob = file.slice(idx * partSize, Math.min(size, (idx + 1) * partSize))
        for (let attempt = 0; ; attempt++) {
          try {
            const etag = await this.uploadPart(bucket, key, uploadId, idx + 1, blob, {
              signal: internal.signal,
              onProgress: (p) => {
                loaded[idx] = p.loaded
                report()
              }
            })
            loaded[idx] = blob.size
            report()
            parts.push({ partNumber: idx + 1, etag })
            break
          } catch (e) {
            loaded[idx] = 0
            if (attempt >= retries || !isRetryable(e)) throw e
            await sleep(250 * 2 ** attempt, internal.signal)
          }
        }
      }
    }

    try {
      await Promise.all(Array.from({ length: concurrency }, worker))
      return await this.completeMultipartUpload(bucket, key, uploadId, parts, internal.signal)
    } catch (e) {
      internal.abort()
      await this.abortMultipartUpload(bucket, key, uploadId).catch(() => undefined)
      if (opts.signal?.aborted || isAbortError(e)) throw abortedError()
      throw e
    } finally {
      opts.signal?.removeEventListener('abort', onOuterAbort)
    }
  }

  // -------------------------------------------------------------------------
  // Tagging
  // -------------------------------------------------------------------------

  async getObjectTagging(bucket: string, key: string, versionId?: string): Promise<Tag[]> {
    const { body } = await this.send({ method: 'GET', bucket, key, query: { tagging: '', versionId } })
    return parseTagging(body)
  }

  async putObjectTagging(bucket: string, key: string, tags: Tag[], versionId?: string): Promise<void> {
    await this.send({ method: 'PUT', bucket, key, query: { tagging: '', versionId }, body: buildTagging(tags), md5: true, headers: { 'Content-Type': 'application/xml' } })
  }

  async deleteObjectTagging(bucket: string, key: string, versionId?: string): Promise<void> {
    await this.send({ method: 'DELETE', bucket, key, query: { tagging: '', versionId } })
  }

  async getBucketTagging(bucket: string): Promise<Tag[]> {
    return this.send({ method: 'GET', bucket, query: { tagging: '' } })
      .then(r => parseTagging(r.body))
      .catch(nullOn404<Tag[]>(['NoSuchTagSet', 'NoSuchTagSetError']))
      .then(r => r ?? [])
  }

  async putBucketTagging(bucket: string, tags: Tag[]): Promise<void> {
    await this.send({ method: 'PUT', bucket, query: { tagging: '' }, body: buildTagging(tags), md5: true, headers: { 'Content-Type': 'application/xml' } })
  }

  async deleteBucketTagging(bucket: string): Promise<void> {
    await this.send({ method: 'DELETE', bucket, query: { tagging: '' } })
  }

  // -------------------------------------------------------------------------
  // Bucket configuration
  // -------------------------------------------------------------------------

  async getBucketVersioning(bucket: string): Promise<VersioningConfiguration> {
    const { body } = await this.send({ method: 'GET', bucket, query: { versioning: '' } })
    return parseVersioning(body)
  }

  async putBucketVersioning(bucket: string, status: Exclude<VersioningStatus, ''>): Promise<void> {
    await this.send({ method: 'PUT', bucket, query: { versioning: '' }, body: buildVersioning(status), md5: true, headers: { 'Content-Type': 'application/xml' } })
  }

  /** Bucket policy JSON, or null when the bucket has none. */
  async getBucketPolicy(bucket: string): Promise<string | null> {
    return this.send({ method: 'GET', bucket, query: { policy: '' } })
      .then(r => r.body)
      .catch(nullOn404<string>(['NoSuchBucketPolicy']))
  }

  async putBucketPolicy(bucket: string, policy: string): Promise<void> {
    await this.send({ method: 'PUT', bucket, query: { policy: '' }, body: policy, md5: true, headers: { 'Content-Type': 'application/json' } })
  }

  async deleteBucketPolicy(bucket: string): Promise<void> {
    await this.send({ method: 'DELETE', bucket, query: { policy: '' } })
  }

  async getBucketPolicyStatus(bucket: string): Promise<PolicyStatus> {
    return this.send({ method: 'GET', bucket, query: { policyStatus: '' } })
      .then(r => parsePolicyStatus(r.body))
      .catch(nullOn404<PolicyStatus>(['NoSuchBucketPolicy']))
      .then(r => r ?? { isPublic: false })
  }

  async getBucketCors(bucket: string): Promise<CORSRule[]> {
    return this.send({ method: 'GET', bucket, query: { cors: '' } })
      .then(r => parseCors(r.body))
      .catch(nullOn404<CORSRule[]>(['NoSuchCORSConfiguration']))
      .then(r => r ?? [])
  }

  async putBucketCors(bucket: string, rules: CORSRule[]): Promise<void> {
    await this.send({ method: 'PUT', bucket, query: { cors: '' }, body: buildCors(rules), md5: true, headers: { 'Content-Type': 'application/xml' } })
  }

  async deleteBucketCors(bucket: string): Promise<void> {
    await this.send({ method: 'DELETE', bucket, query: { cors: '' } })
  }

  async getBucketLifecycle(bucket: string): Promise<LifecycleRule[]> {
    return this.send({ method: 'GET', bucket, query: { lifecycle: '' } })
      .then(r => parseLifecycle(r.body))
      .catch(nullOn404<LifecycleRule[]>(['NoSuchLifecycleConfiguration']))
      .then(r => r ?? [])
  }

  async putBucketLifecycle(bucket: string, rules: LifecycleRule[]): Promise<void> {
    await this.send({ method: 'PUT', bucket, query: { lifecycle: '' }, body: buildLifecycle(rules), md5: true, headers: { 'Content-Type': 'application/xml' } })
  }

  async deleteBucketLifecycle(bucket: string): Promise<void> {
    await this.send({ method: 'DELETE', bucket, query: { lifecycle: '' } })
  }

  async getBucketEncryption(bucket: string): Promise<EncryptionConfiguration | null> {
    return this.send({ method: 'GET', bucket, query: { encryption: '' } })
      .then(r => parseEncryption(r.body))
      .catch(nullOn404<EncryptionConfiguration>(['ServerSideEncryptionConfigurationNotFoundError']))
  }

  async putBucketEncryption(bucket: string, cfg: EncryptionConfiguration): Promise<void> {
    await this.send({ method: 'PUT', bucket, query: { encryption: '' }, body: buildEncryption(cfg), md5: true, headers: { 'Content-Type': 'application/xml' } })
  }

  async deleteBucketEncryption(bucket: string): Promise<void> {
    await this.send({ method: 'DELETE', bucket, query: { encryption: '' } })
  }

  async getObjectLockConfiguration(bucket: string): Promise<ObjectLockConfiguration | null> {
    return this.send({ method: 'GET', bucket, query: { 'object-lock': '' } })
      .then(r => parseObjectLockConfiguration(r.body))
      .catch(nullOn404<ObjectLockConfiguration>(['ObjectLockConfigurationNotFoundError']))
  }

  async putObjectLockConfiguration(bucket: string, cfg: ObjectLockConfiguration): Promise<void> {
    await this.send({ method: 'PUT', bucket, query: { 'object-lock': '' }, body: buildObjectLockConfiguration(cfg), md5: true, headers: { 'Content-Type': 'application/xml' } })
  }

  async getObjectRetention(bucket: string, key: string, versionId?: string): Promise<ObjectRetention | null> {
    return this.send({ method: 'GET', bucket, key, query: { retention: '', versionId } })
      .then(r => parseRetention(r.body))
      .catch(nullOn404<ObjectRetention>(['NoSuchObjectLockConfiguration']))
  }

  async putObjectRetention(bucket: string, key: string, retention: ObjectRetention, opts: { versionId?: string, bypassGovernanceRetention?: boolean } = {}): Promise<void> {
    await this.send({
      method: 'PUT',
      bucket,
      key,
      query: { retention: '', versionId: opts.versionId },
      body: buildRetention(retention),
      md5: true,
      headers: {
        'Content-Type': 'application/xml',
        'x-amz-bypass-governance-retention': opts.bypassGovernanceRetention ? 'true' : undefined
      }
    })
  }

  async getObjectLegalHold(bucket: string, key: string, versionId?: string): Promise<LegalHoldStatus | null> {
    return this.send({ method: 'GET', bucket, key, query: { 'legal-hold': '', versionId } })
      .then(r => parseLegalHold(r.body))
      .catch(nullOn404<LegalHoldStatus>(['NoSuchObjectLockConfiguration']))
  }

  async putObjectLegalHold(bucket: string, key: string, status: LegalHoldStatus, versionId?: string): Promise<void> {
    await this.send({ method: 'PUT', bucket, key, query: { 'legal-hold': '', versionId }, body: buildLegalHold(status), md5: true, headers: { 'Content-Type': 'application/xml' } })
  }

  async getBucketAcl(bucket: string): Promise<AccessControlPolicy> {
    const { body } = await this.send({ method: 'GET', bucket, query: { acl: '' } })
    return parseAccessControlPolicy(body)
  }

  async putBucketAcl(bucket: string, acl: CannedACL): Promise<void> {
    await this.send({ method: 'PUT', bucket, query: { acl: '' }, headers: { 'x-amz-acl': acl } })
  }

  async getObjectAcl(bucket: string, key: string, versionId?: string): Promise<AccessControlPolicy> {
    const { body } = await this.send({ method: 'GET', bucket, key, query: { acl: '', versionId } })
    return parseAccessControlPolicy(body)
  }

  async putObjectAcl(bucket: string, key: string, acl: CannedACL, versionId?: string): Promise<void> {
    await this.send({ method: 'PUT', bucket, key, query: { acl: '', versionId }, headers: { 'x-amz-acl': acl } })
  }
}
