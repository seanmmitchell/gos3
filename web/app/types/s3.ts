// Types for the S3 API as used by the browser S3 client (app/utils/s3/client.ts).
// Names are camelCase versions of the S3 XML element names.

/** ISO 8601 / RFC3339 timestamp string as returned by S3. */
export type S3Timestamp = string

export interface Owner {
  id?: string
  displayName?: string
}

export interface Bucket {
  name: string
  creationDate?: S3Timestamp
  /** BucketRegion (newer ListBuckets responses). */
  region?: string
}

export interface ListBucketsResult {
  owner?: Owner
  buckets: Bucket[]
}

export type ChecksumAlgorithm = 'CRC32' | 'CRC32C' | 'CRC64NVME' | 'SHA1' | 'SHA256'
export type ChecksumType = 'COMPOSITE' | 'FULL_OBJECT'

export interface S3Object {
  key: string
  lastModified?: S3Timestamp
  /** ETag without surrounding quotes. */
  etag?: string
  size: number
  storageClass?: string
  owner?: Owner
  checksumAlgorithm?: string[]
  checksumType?: string
}

export interface CommonPrefix {
  prefix: string
}

export interface ListObjectsV2Params {
  prefix?: string
  delimiter?: string
  continuationToken?: string
  maxKeys?: number
  startAfter?: string
  fetchOwner?: boolean
}

/** ListBucketResult (ListObjectsV2). */
export interface ListResult {
  name: string
  prefix: string
  delimiter?: string
  maxKeys: number
  keyCount: number
  isTruncated: boolean
  continuationToken?: string
  nextContinuationToken?: string
  startAfter?: string
  objects: S3Object[]
  commonPrefixes: CommonPrefix[]
}

export interface ListVersionsParams {
  prefix?: string
  delimiter?: string
  keyMarker?: string
  versionIdMarker?: string
  maxKeys?: number
}

/** An entry of ListVersionsResult: an object version or a delete marker. */
export interface ObjectVersion {
  key: string
  versionId: string
  isLatest: boolean
  lastModified?: S3Timestamp
  /** Not present for delete markers. */
  etag?: string
  /** 0 for delete markers. */
  size: number
  storageClass?: string
  owner?: Owner
  isDeleteMarker: boolean
}

export interface ListVersionsResult {
  name: string
  prefix: string
  delimiter?: string
  keyMarker?: string
  versionIdMarker?: string
  nextKeyMarker?: string
  nextVersionIdMarker?: string
  maxKeys: number
  isTruncated: boolean
  /** Versions and delete markers in response (key, then newest-first) order. */
  versions: ObjectVersion[]
  commonPrefixes: CommonPrefix[]
}

export type ObjectLockMode = 'GOVERNANCE' | 'COMPLIANCE'
export type LegalHoldStatus = 'ON' | 'OFF'

/** Result of HeadObject (also used for GetObject response headers). */
export interface HeadResult {
  contentLength: number
  contentType?: string
  contentEncoding?: string
  contentDisposition?: string
  contentLanguage?: string
  cacheControl?: string
  expires?: string
  /** ETag without quotes. */
  etag?: string
  lastModified?: string
  versionId?: string
  deleteMarker?: boolean
  storageClass?: string
  acceptRanges?: string
  /** User metadata (x-amz-meta-*), keys lower-cased without the prefix. */
  metadata: Record<string, string>
  /** Checksums returned with x-amz-checksum-mode: ENABLED. */
  checksums: Partial<Record<ChecksumAlgorithm, string>>
  checksumType?: string
  objectLockMode?: ObjectLockMode
  objectLockRetainUntilDate?: string
  objectLockLegalHold?: LegalHoldStatus
  serverSideEncryption?: string
  sseKmsKeyId?: string
  /** x-amz-tagging-count */
  tagCount?: number
  /** x-amz-mp-parts-count */
  partsCount?: number
  replicationStatus?: string
  restore?: string
  websiteRedirectLocation?: string
  /**
   * The response was read cross-origin and the server's CORS rule did not
   * expose every header: user metadata, Content-Disposition,
   * Content-Encoding … may exist although they are missing here.
   */
  headersHidden?: boolean
  /** All response headers (lower-case names). */
  headers: Record<string, string>
}

export interface GetObjectRangeResult {
  blob: Blob
  contentType?: string
  /** Total object size (from Content-Range), if known. */
  totalSize?: number
  /** Raw Content-Range header. */
  contentRange?: string
  head: HeadResult
}

export interface GetObjectTextResult {
  text: string
  /** True when the object is larger than maxBytes and was cut. */
  truncated: boolean
  contentType?: string
  /** Total object size, if known. */
  size?: number
}

export interface Tag {
  key: string
  value: string
}

/** '' means the bucket has never been versioned (Unversioned). */
export type VersioningStatus = 'Enabled' | 'Suspended' | ''

export interface VersioningConfiguration {
  status: VersioningStatus
  mfaDelete?: 'Enabled' | 'Disabled'
}

export type HTTPMethod = 'GET' | 'PUT' | 'POST' | 'DELETE' | 'HEAD'

export interface CORSRule {
  id?: string
  allowedMethods: string[]
  allowedOrigins: string[]
  allowedHeaders: string[]
  exposeHeaders: string[]
  maxAgeSeconds?: number
}

export interface LifecycleAndOperator {
  prefix?: string
  tags: Tag[]
  objectSizeGreaterThan?: number
  objectSizeLessThan?: number
}

/** Exactly one of prefix / tag / and / objectSize* should be set (or none = whole bucket). */
export interface LifecycleFilter {
  prefix?: string
  tag?: Tag
  and?: LifecycleAndOperator
  objectSizeGreaterThan?: number
  objectSizeLessThan?: number
}

export interface LifecycleExpiration {
  days?: number
  /** ISO date at midnight UTC, e.g. 2027-01-01T00:00:00Z */
  date?: string
  expiredObjectDeleteMarker?: boolean
}

export interface NoncurrentVersionExpiration {
  noncurrentDays?: number
  newerNoncurrentVersions?: number
}

export interface AbortIncompleteMultipartUpload {
  daysAfterInitiation: number
}

export interface LifecycleTransition {
  days?: number
  date?: string
  storageClass: string
}

export interface NoncurrentVersionTransition {
  noncurrentDays?: number
  newerNoncurrentVersions?: number
  storageClass: string
}

export interface LifecycleRule {
  id?: string
  status: 'Enabled' | 'Disabled'
  filter?: LifecycleFilter
  /** Legacy top-level <Prefix> (pre-Filter rules). */
  prefix?: string
  expiration?: LifecycleExpiration
  noncurrentVersionExpiration?: NoncurrentVersionExpiration
  abortIncompleteMultipartUpload?: AbortIncompleteMultipartUpload
  transitions?: LifecycleTransition[]
  noncurrentVersionTransitions?: NoncurrentVersionTransition[]
}

export interface DefaultRetention {
  mode: ObjectLockMode
  days?: number
  years?: number
}

export interface ObjectLockConfiguration {
  /** ObjectLockEnabled = Enabled */
  enabled: boolean
  rule?: { defaultRetention: DefaultRetention }
}

export interface ObjectRetention {
  mode?: ObjectLockMode
  retainUntilDate?: string
}

export type SSEAlgorithm = 'AES256' | 'aws:kms' | 'aws:kms:dsse'

export interface EncryptionRule {
  sseAlgorithm: SSEAlgorithm | string
  kmsMasterKeyId?: string
  bucketKeyEnabled?: boolean
}

export interface EncryptionConfiguration {
  rules: EncryptionRule[]
}

export type CannedACL =
  | 'private'
  | 'public-read'
  | 'public-read-write'
  | 'authenticated-read'
  | 'aws-exec-read'
  | 'bucket-owner-read'
  | 'bucket-owner-full-control'
  | 'log-delivery-write'

export type GrantPermission = 'FULL_CONTROL' | 'READ' | 'WRITE' | 'READ_ACP' | 'WRITE_ACP'

export interface Grantee {
  type: 'CanonicalUser' | 'Group' | 'AmazonCustomerByEmail' | string
  id?: string
  displayName?: string
  uri?: string
  email?: string
}

export interface Grant {
  grantee: Grantee
  permission: GrantPermission | string
}

export interface AccessControlPolicy {
  owner?: Owner
  grants: Grant[]
  /** Best-effort canned ACL matching the grants, or 'custom'. */
  canned: CannedACL | 'custom'
}

export interface PolicyStatus {
  isPublic: boolean
}

export interface MultipartUpload {
  key: string
  uploadId: string
  initiated?: S3Timestamp
  storageClass?: string
  initiator?: Owner
  owner?: Owner
  checksumAlgorithm?: string
}

export interface ListMultipartUploadsParams {
  prefix?: string
  delimiter?: string
  keyMarker?: string
  uploadIdMarker?: string
  maxUploads?: number
}

export interface ListMultipartUploadsResult {
  bucket: string
  prefix?: string
  delimiter?: string
  keyMarker?: string
  uploadIdMarker?: string
  nextKeyMarker?: string
  nextUploadIdMarker?: string
  maxUploads: number
  isTruncated: boolean
  uploads: MultipartUpload[]
  commonPrefixes: CommonPrefix[]
}

export interface Part {
  partNumber: number
  etag: string
  size?: number
  lastModified?: S3Timestamp
}

export interface ListPartsResult {
  bucket: string
  key: string
  uploadId: string
  partNumberMarker?: number
  nextPartNumberMarker?: number
  maxParts: number
  isTruncated: boolean
  parts: Part[]
}

export interface InitiateMultipartUploadResult {
  bucket: string
  key: string
  uploadId: string
}

export interface CompletedPart {
  partNumber: number
  etag: string
}

export interface CompleteMultipartUploadResult {
  location?: string
  bucket: string
  key: string
  etag: string
  versionId?: string
}

export interface CopyObjectResult {
  etag: string
  lastModified?: S3Timestamp
  versionId?: string
  copySourceVersionId?: string
}

export interface PutObjectResult {
  etag?: string
  versionId?: string
}

export interface ObjectIdentifier {
  key: string
  versionId?: string
}

export interface DeletedObject {
  key: string
  versionId?: string
  deleteMarker?: boolean
  deleteMarkerVersionId?: string
}

export interface DeleteError {
  key: string
  versionId?: string
  code: string
  message: string
}

export interface DeleteObjectsResult {
  deleted: DeletedObject[]
  errors: DeleteError[]
}

export interface DeleteObjectResult {
  versionId?: string
  deleteMarker?: boolean
}

export interface UploadProgress {
  /** Bytes sent so far. */
  loaded: number
  /** Total bytes to send. */
  total: number
}

export type ProgressCallback = (p: UploadProgress) => void

export interface PutObjectOptions {
  contentType?: string
  cacheControl?: string
  contentDisposition?: string
  contentEncoding?: string
  contentLanguage?: string
  /** User metadata (without the x-amz-meta- prefix). */
  metadata?: Record<string, string>
  tags?: Tag[]
  acl?: CannedACL
  storageClass?: string
  /** Extra raw request headers (e.g. object lock). */
  headers?: Record<string, string>
  onProgress?: ProgressCallback
  signal?: AbortSignal
}

export interface UploadLargeOptions extends PutObjectOptions {
  /** Part size in bytes (auto: ≥ 5 MiB and ≤ 10000 parts). */
  partSize?: number
  /** Parallel part uploads (default 4). */
  concurrency?: number
  /** Retries per part on network/5xx errors (default 2). */
  partRetries?: number
}

export interface UploadOptions extends UploadLargeOptions {
  /** Files larger than this use multipart (default 16 MiB). */
  multipartThreshold?: number
}

export type MetadataDirective = 'COPY' | 'REPLACE'

export interface CopyObjectOptions {
  metadataDirective?: MetadataDirective
  metadata?: Record<string, string>
  contentType?: string
  cacheControl?: string
  contentDisposition?: string
  taggingDirective?: MetadataDirective
  tags?: Tag[]
  acl?: CannedACL
  storageClass?: string
  headers?: Record<string, string>
  signal?: AbortSignal
}

export interface CreateBucketOptions {
  /** LocationConstraint (omitted for us-east-1). */
  region?: string
  /** x-amz-bucket-object-lock-enabled: true */
  objectLock?: boolean
  acl?: CannedACL
}

export interface PresignOptions {
  /** Default 3600, max 604800 (7 days). */
  expiresSeconds?: number
  versionId?: string
  responseContentDisposition?: string
  responseContentType?: string
}

export interface ObjectRef {
  bucket: string
  key: string
  versionId?: string
}
