import { describe, expect, it } from 'vitest'
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
} from '~/utils/s3/builders'
import {
  parseCors,
  parseEncryption,
  parseLegalHold,
  parseLifecycle,
  parseObjectLockConfiguration,
  parseRetention,
  parseTagging,
  parseVersioning
} from '~/utils/s3/parsers'
import { children, parseXml, text } from '~/utils/s3/xml'
import type { CORSRule, LifecycleRule } from '~/types/s3'

describe('S3 XML builders', () => {
  it('Delete with escaping and versions', () => {
    const xml = buildDelete([{ key: 'a&b<c>.txt' }, { key: 'v.txt', versionId: 'v1' }], true)
    expect(xml).toContain('<Quiet>true</Quiet>')
    expect(xml).toContain('<Object><Key>a&amp;b&lt;c&gt;.txt</Key></Object>')
    expect(xml).toContain('<Object><Key>v.txt</Key><VersionId>v1</VersionId></Object>')
    const root = parseXml(xml)
    expect(root.localName).toBe('Delete')
    expect(children(root, 'Object').map(o => text(o, 'Key'))).toEqual(['a&b<c>.txt', 'v.txt'])
  })

  it('CompleteMultipartUpload sorts parts and quotes ETags', () => {
    const xml = buildCompleteMultipartUpload([{ partNumber: 2, etag: 'b' }, { partNumber: 1, etag: '"a"' }])
    expect(xml).toContain('<Part><PartNumber>1</PartNumber><ETag>&quot;a&quot;</ETag></Part><Part><PartNumber>2</PartNumber>')
  })

  it('CreateBucketConfiguration only outside us-east-1', () => {
    expect(buildCreateBucketConfiguration()).toBe('')
    expect(buildCreateBucketConfiguration('us-east-1')).toBe('')
    expect(buildCreateBucketConfiguration('eu-west-1')).toContain('<LocationConstraint>eu-west-1</LocationConstraint>')
  })

  it('round-trips tagging, versioning, CORS, lifecycle', () => {
    const tags = [{ key: 'env', value: 'prod & test' }, { key: 'empty', value: '' }]
    expect(parseTagging(buildTagging(tags))).toEqual(tags)
    expect(parseVersioning(buildVersioning('Suspended')).status).toBe('Suspended')

    const cors: CORSRule[] = [{ id: 'r1', allowedMethods: ['GET', 'PUT'], allowedOrigins: ['https://a.example'], allowedHeaders: ['*'], exposeHeaders: ['ETag'], maxAgeSeconds: 600 }]
    expect(parseCors(buildCors(cors))).toEqual(cors)

    const rules: LifecycleRule[] = [
      { id: 'one', status: 'Enabled', filter: { prefix: 'logs/' }, expiration: { days: 30 }, abortIncompleteMultipartUpload: { daysAfterInitiation: 7 } },
      { id: 'two', status: 'Disabled', filter: { and: { prefix: 'x/', tags: [{ key: 'k', value: 'v' }] } }, noncurrentVersionExpiration: { noncurrentDays: 10 } },
      { id: 'three', status: 'Enabled', filter: { tag: { key: 'a', value: 'b' } }, expiration: { expiredObjectDeleteMarker: true } }
    ]
    const parsed = parseLifecycle(buildLifecycle(rules))
    expect(parsed[0]).toMatchObject(rules[0]!)
    expect(parsed[1]!.filter?.and).toMatchObject({ prefix: 'x/', tags: [{ key: 'k', value: 'v' }] })
    expect(parsed[1]!.noncurrentVersionExpiration?.noncurrentDays).toBe(10)
    expect(parsed[2]!.filter?.tag).toEqual({ key: 'a', value: 'b' })
    // whole-bucket rule gets an empty-prefix filter
    expect(buildLifecycle([{ status: 'Enabled', expiration: { days: 1 } }])).toContain('<Filter><Prefix></Prefix></Filter>')
  })

  it('round-trips encryption, object lock, retention, legal hold', () => {
    expect(parseEncryption(buildEncryption({ rules: [{ sseAlgorithm: 'AES256', bucketKeyEnabled: true }] })).rules[0]).toMatchObject({ sseAlgorithm: 'AES256', bucketKeyEnabled: true })
    const lock = { enabled: true, rule: { defaultRetention: { mode: 'COMPLIANCE' as const, years: 1 } } }
    expect(parseObjectLockConfiguration(buildObjectLockConfiguration(lock))).toEqual({ enabled: true, rule: { defaultRetention: { mode: 'COMPLIANCE', years: 1, days: undefined } } })
    expect(parseRetention(buildRetention({ mode: 'GOVERNANCE', retainUntilDate: '2027-01-01T00:00:00Z' }))).toEqual({ mode: 'GOVERNANCE', retainUntilDate: '2027-01-01T00:00:00Z' })
    expect(parseLegalHold(buildLegalHold('OFF'))).toBe('OFF')
  })

  it('encodes tags for the x-amz-tagging header', () => {
    expect(tagsToHeader([{ key: 'a b', value: 'c&d' }, { key: 'e', value: '' }])).toBe('a+b=c%26d&e=')
  })
})
