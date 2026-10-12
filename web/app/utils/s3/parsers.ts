// Parsers for S3 XML responses. Each takes the response body text and returns
// typed data; all lookups are namespace-agnostic.
import type {
  AccessControlPolicy,
  Bucket,
  CannedACL,
  CommonPrefix,
  CompleteMultipartUploadResult,
  CopyObjectResult,
  CORSRule,
  DeleteObjectsResult,
  EncryptionConfiguration,
  Grant,
  InitiateMultipartUploadResult,
  LegalHoldStatus,
  LifecycleFilter,
  LifecycleRule,
  ListBucketsResult,
  ListMultipartUploadsResult,
  ListPartsResult,
  ListResult,
  ListVersionsResult,
  ObjectLockConfiguration,
  ObjectLockMode,
  ObjectRetention,
  ObjectVersion,
  Owner,
  PolicyStatus,
  S3Object,
  Tag,
  VersioningConfiguration,
  VersioningStatus
} from '~/types/s3'
import { bool, child, children, num, parseXml, path, text, texts, unquoteETag } from './xml'

function optional(v: string | undefined): string | undefined {
  return v === undefined || v === '' ? undefined : v
}

function parseOwner(el: Element | undefined): Owner | undefined {
  if (!el) return undefined
  return { id: optional(text(el, 'ID')), displayName: optional(text(el, 'DisplayName')) }
}

function parseCommonPrefixes(root: Element): CommonPrefix[] {
  return children(root, 'CommonPrefixes').map(cp => ({ prefix: text(cp, 'Prefix') ?? '' }))
}

/**
 * Decodes keys when the response used EncodingType=url. Note that S3 URL
 * encoding uses '+' for spaces.
 */
function decoder(root: Element): (s: string | undefined) => string | undefined {
  const enc = text(root, 'EncodingType')
  if (enc !== 'url') return s => s
  return (s) => {
    if (s === undefined) return s
    try {
      return decodeURIComponent(s.replace(/\+/g, ' '))
    } catch {
      return s // malformed escape (not valid UTF-8): keep the raw value
    }
  }
}

export function parseListBuckets(body: string): ListBucketsResult {
  const root = parseXml(body)
  const buckets: Bucket[] = children(child(root, 'Buckets'), 'Bucket').map(b => ({
    name: text(b, 'Name') ?? '',
    creationDate: optional(text(b, 'CreationDate')),
    region: optional(text(b, 'BucketRegion'))
  }))
  return { owner: parseOwner(child(root, 'Owner')), buckets }
}

export function parseLocation(body: string): string {
  const root = parseXml(body)
  const loc = (root.textContent ?? '').trim()
  // Empty LocationConstraint means us-east-1; "EU" is a legacy alias.
  if (!loc) return 'us-east-1'
  if (loc === 'EU') return 'eu-west-1'
  return loc
}

function parseObject(el: Element, dec: (s: string | undefined) => string | undefined): S3Object {
  const algos = texts(el, 'ChecksumAlgorithm')
  return {
    key: dec(text(el, 'Key')) ?? '',
    lastModified: optional(text(el, 'LastModified')),
    etag: unquoteETag(text(el, 'ETag')),
    size: num(el, 'Size') ?? 0,
    storageClass: optional(text(el, 'StorageClass')),
    owner: parseOwner(child(el, 'Owner')),
    checksumAlgorithm: algos.length ? algos : undefined,
    checksumType: optional(text(el, 'ChecksumType'))
  }
}

/** ListBucketResult of ListObjectsV2 (also tolerates V1 responses). */
export function parseListObjectsV2(body: string): ListResult {
  const root = parseXml(body)
  const dec = decoder(root)
  const objects = children(root, 'Contents').map(c => parseObject(c, dec))
  const commonPrefixes = parseCommonPrefixes(root).map(p => ({ prefix: dec(p.prefix) ?? '' }))
  return {
    name: text(root, 'Name') ?? '',
    prefix: dec(text(root, 'Prefix')) ?? '',
    delimiter: optional(dec(text(root, 'Delimiter'))),
    maxKeys: num(root, 'MaxKeys') ?? 1000,
    keyCount: num(root, 'KeyCount') ?? objects.length + commonPrefixes.length,
    isTruncated: bool(root, 'IsTruncated') ?? false,
    continuationToken: optional(text(root, 'ContinuationToken')),
    // V1 compatibility: NextMarker acts as a continuation token for StartAfter.
    nextContinuationToken: optional(text(root, 'NextContinuationToken')),
    startAfter: optional(dec(text(root, 'StartAfter'))),
    objects,
    commonPrefixes
  }
}

export function parseListVersions(body: string): ListVersionsResult {
  const root = parseXml(body)
  const dec = decoder(root)
  const versions: ObjectVersion[] = []
  for (let n = root.firstElementChild; n; n = n.nextElementSibling) {
    if (n.localName !== 'Version' && n.localName !== 'DeleteMarker') continue
    const isDeleteMarker = n.localName === 'DeleteMarker'
    versions.push({
      key: dec(text(n, 'Key')) ?? '',
      versionId: text(n, 'VersionId') || 'null',
      isLatest: bool(n, 'IsLatest') ?? false,
      lastModified: optional(text(n, 'LastModified')),
      etag: isDeleteMarker ? undefined : unquoteETag(text(n, 'ETag')),
      size: isDeleteMarker ? 0 : num(n, 'Size') ?? 0,
      storageClass: optional(text(n, 'StorageClass')),
      owner: parseOwner(child(n, 'Owner')),
      isDeleteMarker
    })
  }
  return {
    name: text(root, 'Name') ?? '',
    prefix: dec(text(root, 'Prefix')) ?? '',
    delimiter: optional(dec(text(root, 'Delimiter'))),
    keyMarker: optional(dec(text(root, 'KeyMarker'))),
    versionIdMarker: optional(text(root, 'VersionIdMarker')),
    nextKeyMarker: optional(dec(text(root, 'NextKeyMarker'))),
    nextVersionIdMarker: optional(text(root, 'NextVersionIdMarker')),
    maxKeys: num(root, 'MaxKeys') ?? 1000,
    isTruncated: bool(root, 'IsTruncated') ?? false,
    versions,
    commonPrefixes: parseCommonPrefixes(root).map(p => ({ prefix: dec(p.prefix) ?? '' }))
  }
}

export function parseInitiateMultipartUpload(body: string): InitiateMultipartUploadResult {
  const root = parseXml(body)
  return {
    bucket: text(root, 'Bucket') ?? '',
    key: text(root, 'Key') ?? '',
    uploadId: text(root, 'UploadId') ?? ''
  }
}

export function parseCompleteMultipartUpload(body: string, headers?: Headers): CompleteMultipartUploadResult {
  const root = parseXml(body)
  return {
    location: optional(text(root, 'Location')),
    bucket: text(root, 'Bucket') ?? '',
    key: text(root, 'Key') ?? '',
    etag: unquoteETag(text(root, 'ETag')) ?? '',
    versionId: headers?.get('x-amz-version-id') ?? undefined
  }
}

export function parseCopyObjectResult(body: string, headers?: Headers): CopyObjectResult {
  const root = parseXml(body)
  return {
    etag: unquoteETag(text(root, 'ETag')) ?? '',
    lastModified: optional(text(root, 'LastModified')),
    versionId: headers?.get('x-amz-version-id') ?? undefined,
    copySourceVersionId: headers?.get('x-amz-copy-source-version-id') ?? undefined
  }
}

export function parseListMultipartUploads(body: string): ListMultipartUploadsResult {
  const root = parseXml(body)
  const dec = decoder(root)
  return {
    bucket: text(root, 'Bucket') ?? '',
    prefix: optional(dec(text(root, 'Prefix'))),
    delimiter: optional(dec(text(root, 'Delimiter'))),
    keyMarker: optional(dec(text(root, 'KeyMarker'))),
    uploadIdMarker: optional(text(root, 'UploadIdMarker')),
    nextKeyMarker: optional(dec(text(root, 'NextKeyMarker'))),
    nextUploadIdMarker: optional(text(root, 'NextUploadIdMarker')),
    maxUploads: num(root, 'MaxUploads') ?? 1000,
    isTruncated: bool(root, 'IsTruncated') ?? false,
    uploads: children(root, 'Upload').map(u => ({
      key: dec(text(u, 'Key')) ?? '',
      uploadId: text(u, 'UploadId') ?? '',
      initiated: optional(text(u, 'Initiated')),
      storageClass: optional(text(u, 'StorageClass')),
      initiator: parseOwner(child(u, 'Initiator')),
      owner: parseOwner(child(u, 'Owner')),
      checksumAlgorithm: optional(text(u, 'ChecksumAlgorithm'))
    })),
    commonPrefixes: parseCommonPrefixes(root).map(p => ({ prefix: dec(p.prefix) ?? '' }))
  }
}

export function parseListParts(body: string): ListPartsResult {
  const root = parseXml(body)
  return {
    bucket: text(root, 'Bucket') ?? '',
    key: text(root, 'Key') ?? '',
    uploadId: text(root, 'UploadId') ?? '',
    partNumberMarker: num(root, 'PartNumberMarker'),
    nextPartNumberMarker: num(root, 'NextPartNumberMarker'),
    maxParts: num(root, 'MaxParts') ?? 1000,
    isTruncated: bool(root, 'IsTruncated') ?? false,
    parts: children(root, 'Part').map(p => ({
      partNumber: num(p, 'PartNumber') ?? 0,
      etag: unquoteETag(text(p, 'ETag')) ?? '',
      size: num(p, 'Size'),
      lastModified: optional(text(p, 'LastModified'))
    }))
  }
}

export function parseDeleteObjectsResult(body: string): DeleteObjectsResult {
  const root = parseXml(body)
  return {
    deleted: children(root, 'Deleted').map(d => ({
      key: text(d, 'Key') ?? '',
      versionId: optional(text(d, 'VersionId')),
      deleteMarker: bool(d, 'DeleteMarker'),
      deleteMarkerVersionId: optional(text(d, 'DeleteMarkerVersionId'))
    })),
    errors: children(root, 'Error').map(e => ({
      key: text(e, 'Key') ?? '',
      versionId: optional(text(e, 'VersionId')),
      code: text(e, 'Code') ?? '',
      message: text(e, 'Message') ?? ''
    }))
  }
}

/** <Tagging><TagSet><Tag><Key/><Value/></Tag></TagSet></Tagging> */
export function parseTagging(body: string): Tag[] {
  const root = parseXml(body)
  return children(child(root, 'TagSet'), 'Tag').map(tg => ({
    key: text(tg, 'Key') ?? '',
    value: text(tg, 'Value') ?? ''
  }))
}

export function parseVersioning(body: string): VersioningConfiguration {
  const root = parseXml(body)
  const status = (text(root, 'Status') ?? '') as VersioningStatus
  const mfa = text(root, 'MfaDelete') ?? text(root, 'MFADelete')
  return {
    status: status === 'Enabled' || status === 'Suspended' ? status : '',
    mfaDelete: mfa === 'Enabled' || mfa === 'Disabled' ? mfa : undefined
  }
}

export function parseCors(body: string): CORSRule[] {
  const root = parseXml(body)
  return children(root, 'CORSRule').map(r => ({
    id: optional(text(r, 'ID')),
    allowedMethods: texts(r, 'AllowedMethod'),
    allowedOrigins: texts(r, 'AllowedOrigin'),
    allowedHeaders: texts(r, 'AllowedHeader'),
    exposeHeaders: texts(r, 'ExposeHeader'),
    maxAgeSeconds: num(r, 'MaxAgeSeconds')
  }))
}

function parseTag(el: Element | undefined): Tag | undefined {
  if (!el) return undefined
  return { key: text(el, 'Key') ?? '', value: text(el, 'Value') ?? '' }
}

function parseLifecycleFilter(el: Element | undefined): LifecycleFilter | undefined {
  if (!el) return undefined
  const f: LifecycleFilter = {}
  const prefix = text(el, 'Prefix')
  if (prefix !== undefined) f.prefix = prefix
  const tg = parseTag(child(el, 'Tag'))
  if (tg) f.tag = tg
  const gt = num(el, 'ObjectSizeGreaterThan')
  if (gt !== undefined) f.objectSizeGreaterThan = gt
  const lt = num(el, 'ObjectSizeLessThan')
  if (lt !== undefined) f.objectSizeLessThan = lt
  const and = child(el, 'And')
  if (and) {
    f.and = {
      prefix: text(and, 'Prefix'),
      tags: children(and, 'Tag').map(tgEl => parseTag(tgEl)!),
      objectSizeGreaterThan: num(and, 'ObjectSizeGreaterThan'),
      objectSizeLessThan: num(and, 'ObjectSizeLessThan')
    }
  }
  return f
}

export function parseLifecycle(body: string): LifecycleRule[] {
  const root = parseXml(body)
  return children(root, 'Rule').map(r => {
    const rule: LifecycleRule = {
      id: optional(text(r, 'ID')),
      status: text(r, 'Status') === 'Enabled' ? 'Enabled' : 'Disabled'
    }
    const filter = parseLifecycleFilter(child(r, 'Filter'))
    if (filter) rule.filter = filter
    const legacyPrefix = text(r, 'Prefix')
    if (legacyPrefix !== undefined) rule.prefix = legacyPrefix
    const exp = child(r, 'Expiration')
    if (exp) {
      rule.expiration = {
        days: num(exp, 'Days'),
        date: optional(text(exp, 'Date')),
        expiredObjectDeleteMarker: bool(exp, 'ExpiredObjectDeleteMarker')
      }
    }
    const nve = child(r, 'NoncurrentVersionExpiration')
    if (nve) {
      rule.noncurrentVersionExpiration = {
        noncurrentDays: num(nve, 'NoncurrentDays'),
        newerNoncurrentVersions: num(nve, 'NewerNoncurrentVersions')
      }
    }
    const abort = child(r, 'AbortIncompleteMultipartUpload')
    if (abort) rule.abortIncompleteMultipartUpload = { daysAfterInitiation: num(abort, 'DaysAfterInitiation') ?? 0 }
    const transitions = children(r, 'Transition')
    if (transitions.length) {
      rule.transitions = transitions.map(tr => ({
        days: num(tr, 'Days'),
        date: optional(text(tr, 'Date')),
        storageClass: text(tr, 'StorageClass') ?? ''
      }))
    }
    const nvt = children(r, 'NoncurrentVersionTransition')
    if (nvt.length) {
      rule.noncurrentVersionTransitions = nvt.map(tr => ({
        noncurrentDays: num(tr, 'NoncurrentDays'),
        newerNoncurrentVersions: num(tr, 'NewerNoncurrentVersions'),
        storageClass: text(tr, 'StorageClass') ?? ''
      }))
    }
    return rule
  })
}

export function parseEncryption(body: string): EncryptionConfiguration {
  const root = parseXml(body)
  return {
    rules: children(root, 'Rule').map(r => {
      const d = child(r, 'ApplyServerSideEncryptionByDefault')
      return {
        sseAlgorithm: text(d, 'SSEAlgorithm') ?? 'AES256',
        kmsMasterKeyId: optional(text(d, 'KMSMasterKeyID')),
        bucketKeyEnabled: bool(r, 'BucketKeyEnabled')
      }
    })
  }
}

function lockMode(v: string | undefined): ObjectLockMode | undefined {
  return v === 'GOVERNANCE' || v === 'COMPLIANCE' ? v : undefined
}

export function parseObjectLockConfiguration(body: string): ObjectLockConfiguration {
  const root = parseXml(body)
  const cfg: ObjectLockConfiguration = { enabled: text(root, 'ObjectLockEnabled') === 'Enabled' }
  const dr = path(root, 'Rule', 'DefaultRetention')
  const mode = lockMode(text(dr, 'Mode'))
  if (dr && mode) {
    cfg.rule = { defaultRetention: { mode, days: num(dr, 'Days'), years: num(dr, 'Years') } }
  }
  return cfg
}

export function parseRetention(body: string): ObjectRetention {
  const root = parseXml(body)
  return { mode: lockMode(text(root, 'Mode')), retainUntilDate: optional(text(root, 'RetainUntilDate')) }
}

export function parseLegalHold(body: string): LegalHoldStatus {
  const root = parseXml(body)
  return text(root, 'Status') === 'ON' ? 'ON' : 'OFF'
}

export function parsePolicyStatus(body: string): PolicyStatus {
  const root = parseXml(body)
  return { isPublic: bool(root, 'IsPublic') ?? false }
}

const ALL_USERS = 'http://acs.amazonaws.com/groups/global/AllUsers'
const AUTH_USERS = 'http://acs.amazonaws.com/groups/global/AuthenticatedUsers'

/** Best-effort mapping of grants to a canned ACL. */
export function cannedFromGrants(grants: Grant[], ownerId?: string): CannedACL | 'custom' {
  const others = grants.filter(g => !(g.grantee.type === 'CanonicalUser' && (!ownerId || g.grantee.id === ownerId) && g.permission === 'FULL_CONTROL'))
  const perms = (uri: string) => others.filter(g => g.grantee.uri === uri).map(g => g.permission).sort().join(',')
  if (others.length === 0) return 'private'
  const all = perms(ALL_USERS)
  const auth = perms(AUTH_USERS)
  const rest = others.filter(g => g.grantee.uri !== ALL_USERS && g.grantee.uri !== AUTH_USERS)
  if (rest.length) return 'custom'
  if (all === 'READ' && !auth) return 'public-read'
  if (all === 'READ,WRITE' && !auth) return 'public-read-write'
  if (auth === 'READ' && !all) return 'authenticated-read'
  return 'custom'
}

export function parseAccessControlPolicy(body: string): AccessControlPolicy {
  const root = parseXml(body)
  const owner = parseOwner(child(root, 'Owner'))
  const grants: Grant[] = children(child(root, 'AccessControlList'), 'Grant').map(g => {
    const ge = child(g, 'Grantee')
    const type = ge?.getAttributeNS('http://www.w3.org/2001/XMLSchema-instance', 'type')
      || ge?.getAttribute('xsi:type')
      || (text(ge, 'URI') ? 'Group' : 'CanonicalUser')
    return {
      grantee: {
        type,
        id: optional(text(ge, 'ID')),
        displayName: optional(text(ge, 'DisplayName')),
        uri: optional(text(ge, 'URI')),
        email: optional(text(ge, 'EmailAddress'))
      },
      permission: text(g, 'Permission') ?? ''
    }
  })
  return { owner, grants, canned: cannedFromGrants(grants, owner?.id) }
}
