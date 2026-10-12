// XML request body builders for S3 PUT/POST operations. All text is escaped.
import type {
  CompletedPart,
  CORSRule,
  EncryptionConfiguration,
  LegalHoldStatus,
  LifecycleFilter,
  LifecycleRule,
  ObjectIdentifier,
  ObjectLockConfiguration,
  ObjectRetention,
  Tag,
  VersioningStatus
} from '~/types/s3'
import { t, x, xmlDocument } from './xml'

function tagXml(tag: Tag): string {
  return x('Tag', t('Key', tag.key), t('Value', tag.value))
}

export function buildTagging(tags: Tag[]): string {
  return xmlDocument('Tagging', x('TagSet', tags.map(tagXml)))
}

/** URL-encoded tag set for the x-amz-tagging header. */
export function tagsToHeader(tags: Tag[]): string {
  const p = new URLSearchParams()
  for (const tg of tags) p.append(tg.key, tg.value)
  return p.toString()
}

export function buildCreateBucketConfiguration(region?: string): string {
  if (!region || region === 'us-east-1') return ''
  return xmlDocument('CreateBucketConfiguration', t('LocationConstraint', region))
}

export function buildVersioning(status: Exclude<VersioningStatus, ''>): string {
  return xmlDocument('VersioningConfiguration', t('Status', status))
}

export function buildDelete(objects: ObjectIdentifier[], quiet = false): string {
  return xmlDocument(
    'Delete',
    t('Quiet', quiet),
    objects.map(o => x('Object', t('Key', o.key), o.versionId ? t('VersionId', o.versionId) : ''))
  )
}

export function buildCompleteMultipartUpload(parts: CompletedPart[]): string {
  const sorted = [...parts].sort((a, b) => a.partNumber - b.partNumber)
  return xmlDocument(
    'CompleteMultipartUpload',
    sorted.map(p => x('Part', t('PartNumber', p.partNumber), t('ETag', `"${p.etag.replace(/^"+|"+$/g, '')}"`)))
  )
}

export function buildCors(rules: CORSRule[]): string {
  return xmlDocument(
    'CORSConfiguration',
    rules.map(r =>
      x(
        'CORSRule',
        r.id ? t('ID', r.id) : '',
        r.allowedHeaders.map(h => t('AllowedHeader', h)),
        r.allowedMethods.map(m => t('AllowedMethod', m)),
        r.allowedOrigins.map(o => t('AllowedOrigin', o)),
        r.exposeHeaders.map(h => t('ExposeHeader', h)),
        r.maxAgeSeconds !== undefined ? t('MaxAgeSeconds', r.maxAgeSeconds) : ''
      )
    )
  )
}

function filterXml(f: LifecycleFilter | undefined): string {
  if (!f) return x('Filter', t('Prefix', ''))
  if (f.and) {
    return x(
      'Filter',
      x(
        'And',
        f.and.prefix !== undefined ? t('Prefix', f.and.prefix) : '',
        f.and.tags.map(tagXml),
        t('ObjectSizeGreaterThan', f.and.objectSizeGreaterThan),
        t('ObjectSizeLessThan', f.and.objectSizeLessThan)
      )
    )
  }
  if (f.tag) return x('Filter', tagXml(f.tag))
  if (f.objectSizeGreaterThan !== undefined || f.objectSizeLessThan !== undefined) {
    // S3 requires And when combining; a single size condition is allowed bare.
    if (f.objectSizeGreaterThan !== undefined && f.objectSizeLessThan !== undefined) {
      return x('Filter', x('And', t('ObjectSizeGreaterThan', f.objectSizeGreaterThan), t('ObjectSizeLessThan', f.objectSizeLessThan)))
    }
    return x('Filter', t('ObjectSizeGreaterThan', f.objectSizeGreaterThan), t('ObjectSizeLessThan', f.objectSizeLessThan))
  }
  return x('Filter', t('Prefix', f.prefix ?? ''))
}

export function buildLifecycle(rules: LifecycleRule[]): string {
  return xmlDocument(
    'LifecycleConfiguration',
    rules.map(r =>
      x(
        'Rule',
        r.id ? t('ID', r.id) : '',
        // Legacy top-level Prefix is only emitted when no Filter is given.
        r.filter === undefined && r.prefix !== undefined ? t('Prefix', r.prefix) : filterXml(r.filter),
        t('Status', r.status),
        r.transitions?.map(tr => x('Transition', t('Date', tr.date), t('Days', tr.days), t('StorageClass', tr.storageClass))),
        r.expiration
          ? x(
              'Expiration',
              t('Date', r.expiration.date),
              t('Days', r.expiration.days),
              t('ExpiredObjectDeleteMarker', r.expiration.expiredObjectDeleteMarker)
            )
          : '',
        r.noncurrentVersionTransitions?.map(tr =>
          x(
            'NoncurrentVersionTransition',
            t('NoncurrentDays', tr.noncurrentDays),
            t('NewerNoncurrentVersions', tr.newerNoncurrentVersions),
            t('StorageClass', tr.storageClass)
          )
        ),
        r.noncurrentVersionExpiration
          ? x(
              'NoncurrentVersionExpiration',
              t('NoncurrentDays', r.noncurrentVersionExpiration.noncurrentDays),
              t('NewerNoncurrentVersions', r.noncurrentVersionExpiration.newerNoncurrentVersions)
            )
          : '',
        r.abortIncompleteMultipartUpload
          ? x('AbortIncompleteMultipartUpload', t('DaysAfterInitiation', r.abortIncompleteMultipartUpload.daysAfterInitiation))
          : ''
      )
    )
  )
}

export function buildEncryption(cfg: EncryptionConfiguration): string {
  return xmlDocument(
    'ServerSideEncryptionConfiguration',
    cfg.rules.map(r =>
      x(
        'Rule',
        x('ApplyServerSideEncryptionByDefault', t('SSEAlgorithm', r.sseAlgorithm), r.kmsMasterKeyId ? t('KMSMasterKeyID', r.kmsMasterKeyId) : ''),
        t('BucketKeyEnabled', r.bucketKeyEnabled)
      )
    )
  )
}

export function buildObjectLockConfiguration(cfg: ObjectLockConfiguration): string {
  const dr = cfg.rule?.defaultRetention
  return xmlDocument(
    'ObjectLockConfiguration',
    cfg.enabled ? t('ObjectLockEnabled', 'Enabled') : '',
    dr ? x('Rule', x('DefaultRetention', t('Mode', dr.mode), t('Days', dr.days), t('Years', dr.years))) : ''
  )
}

export function buildRetention(r: ObjectRetention): string {
  return xmlDocument('Retention', t('Mode', r.mode), t('RetainUntilDate', r.retainUntilDate))
}

export function buildLegalHold(status: LegalHoldStatus): string {
  return xmlDocument('LegalHold', t('Status', status))
}
