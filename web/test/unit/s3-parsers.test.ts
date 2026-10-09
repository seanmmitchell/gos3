import { describe, expect, it } from 'vitest'
import {
  parseAccessControlPolicy,
  parseCompleteMultipartUpload,
  parseCors,
  parseDeleteObjectsResult,
  parseEncryption,
  parseInitiateMultipartUpload,
  parseLifecycle,
  parseListBuckets,
  parseListMultipartUploads,
  parseListObjectsV2,
  parseListVersions,
  parseLocation,
  parseObjectLockConfiguration,
  parseRetention,
  parseLegalHold,
  parseTagging,
  parseVersioning,
  parseCopyObjectResult
} from '~/utils/s3/parsers'
import { parseS3Error, statusError } from '~/utils/s3/errors'

const NS = 'xmlns="http://s3.amazonaws.com/doc/2006-03-01/"'

describe('S3 response parsers', () => {
  it('ListAllMyBucketsResult', () => {
    const r = parseListBuckets(`<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult ${NS}>
  <Owner><ID>bcaf1ffd86f461ca5fb16fd081034f</ID><DisplayName>webfile</DisplayName></Owner>
  <Buckets>
    <Bucket><Name>amzn-s3-demo-bucket</Name><CreationDate>2006-02-03T16:45:09.000Z</CreationDate></Bucket>
    <Bucket><Name>photos</Name><CreationDate>2026-10-01T10:00:00.000Z</CreationDate><BucketRegion>eu-west-1</BucketRegion></Bucket>
  </Buckets>
</ListAllMyBucketsResult>`)
    expect(r.owner).toEqual({ id: 'bcaf1ffd86f461ca5fb16fd081034f', displayName: 'webfile' })
    expect(r.buckets).toEqual([
      { name: 'amzn-s3-demo-bucket', creationDate: '2006-02-03T16:45:09.000Z', region: undefined },
      { name: 'photos', creationDate: '2026-10-01T10:00:00.000Z', region: 'eu-west-1' }
    ])
  })

  it('empty ListAllMyBucketsResult', () => {
    expect(parseListBuckets(`<ListAllMyBucketsResult ${NS}><Owner><ID>x</ID></Owner><Buckets/></ListAllMyBucketsResult>`).buckets).toEqual([])
  })

  it('ListBucketResult (V2) with CommonPrefixes', () => {
    const r = parseListObjectsV2(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult ${NS}>
  <Name>photos</Name>
  <Prefix>2024/</Prefix>
  <KeyCount>3</KeyCount>
  <MaxKeys>1000</MaxKeys>
  <Delimiter>/</Delimiter>
  <IsTruncated>true</IsTruncated>
  <ContinuationToken>tok1</ContinuationToken>
  <NextContinuationToken>1ueGcxLPRx1Tr/XYExHnhbYLgveDs2J/wm36Hy4vbOwM=</NextContinuationToken>
  <Contents>
    <Key>2024/a &amp; b.jpg</Key>
    <LastModified>2026-10-09T12:00:00.000Z</LastModified>
    <ETag>"599bab3ed2c697f1d26842727561fd94"</ETag>
    <Size>142863</Size>
    <StorageClass>STANDARD</StorageClass>
    <ChecksumAlgorithm>CRC32</ChecksumAlgorithm>
  </Contents>
  <CommonPrefixes><Prefix>2024/jan/</Prefix></CommonPrefixes>
  <CommonPrefixes><Prefix>2024/feb/</Prefix></CommonPrefixes>
</ListBucketResult>`)
    expect(r.name).toBe('photos')
    expect(r.prefix).toBe('2024/')
    expect(r.delimiter).toBe('/')
    expect(r.keyCount).toBe(3)
    expect(r.isTruncated).toBe(true)
    expect(r.continuationToken).toBe('tok1')
    expect(r.nextContinuationToken).toBe('1ueGcxLPRx1Tr/XYExHnhbYLgveDs2J/wm36Hy4vbOwM=')
    expect(r.objects).toHaveLength(1)
    expect(r.objects[0]).toMatchObject({ key: '2024/a & b.jpg', etag: '599bab3ed2c697f1d26842727561fd94', size: 142863, storageClass: 'STANDARD', checksumAlgorithm: ['CRC32'] })
    expect(r.commonPrefixes).toEqual([{ prefix: '2024/jan/' }, { prefix: '2024/feb/' }])
  })

  it('decodes keys when EncodingType=url', () => {
    const r = parseListObjectsV2(`<ListBucketResult ${NS}><Name>b</Name><Prefix>a%2F</Prefix><EncodingType>url</EncodingType><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
      <Contents><Key>a%2Fhello+world%21.txt</Key><Size>1</Size></Contents></ListBucketResult>`)
    expect(r.prefix).toBe('a/')
    expect(r.objects[0]!.key).toBe('a/hello world!.txt')
  })

  it('ListVersionsResult with delete markers', () => {
    const r = parseListVersions(`<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult ${NS}>
  <Name>bucket</Name><Prefix>my</Prefix><KeyMarker/><VersionIdMarker/><MaxKeys>5</MaxKeys><IsTruncated>true</IsTruncated>
  <NextKeyMarker>my-second-image.jpg</NextKeyMarker><NextVersionIdMarker>03jpff543dhffds434rfdsFDN943fdsFkdmqnh892</NextVersionIdMarker>
  <DeleteMarker>
    <Key>my-image.jpg</Key><VersionId>03jpff543dhffds434rfdsFDN943fdsFkdmqnh892</VersionId><IsLatest>true</IsLatest>
    <LastModified>2009-10-15T17:50:30.000Z</LastModified><Owner><ID>75aa57f09aa0c8caeab4f8c24e99d10f8e7faeebf76c078efc7c6caea54ba06a</ID><DisplayName>mtd@amazon.com</DisplayName></Owner>
  </DeleteMarker>
  <Version>
    <Key>my-image.jpg</Key><VersionId>3/L4kqtJl40Nr8X8gdRQBpUMLUo</VersionId><IsLatest>false</IsLatest>
    <LastModified>2009-10-12T17:50:30.000Z</LastModified><ETag>"fba9dede5f27731c9771645a39863328"</ETag><Size>434234</Size><StorageClass>STANDARD</StorageClass>
  </Version>
  <Version>
    <Key>my-image.jpg</Key><VersionId>null</VersionId><IsLatest>false</IsLatest><Size>5</Size><ETag>"x"</ETag>
  </Version>
</ListVersionsResult>`)
    expect(r.isTruncated).toBe(true)
    expect(r.nextKeyMarker).toBe('my-second-image.jpg')
    expect(r.keyMarker).toBeUndefined()
    expect(r.versions).toHaveLength(3)
    expect(r.versions[0]).toMatchObject({ isDeleteMarker: true, isLatest: true, size: 0, etag: undefined, versionId: '03jpff543dhffds434rfdsFDN943fdsFkdmqnh892' })
    expect(r.versions[0]!.owner?.displayName).toBe('mtd@amazon.com')
    expect(r.versions[1]).toMatchObject({ isDeleteMarker: false, isLatest: false, size: 434234, etag: 'fba9dede5f27731c9771645a39863328' })
    expect(r.versions[2]!.versionId).toBe('null')
  })

  it('Error', () => {
    const headers = new Headers({ 'x-amz-request-id': 'HDR' })
    const e = parseS3Error(404, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>The resource you requested does not exist</Message><Resource>/mybucket/myfoto.jpg</Resource><RequestId>4442587FB7D0A2F9</RequestId><Key>myfoto.jpg</Key></Error>`, headers)
    expect(e.code).toBe('NoSuchKey')
    expect(e.status).toBe(404)
    expect(e.message).toBe('The resource you requested does not exist')
    expect(e.resource).toBe('/mybucket/myfoto.jpg')
    expect(e.requestId).toBe('4442587FB7D0A2F9')
    expect(e.details.Key).toBe('myfoto.jpg')
    expect(e.isNotFound).toBe(true)
  })

  it('Error without body falls back to status codes', () => {
    const e = parseS3Error(403, '', new Headers({ 'x-amz-request-id': 'ABC' }))
    expect(e.code).toBe('AccessDenied')
    expect(e.requestId).toBe('ABC')
    expect(parseS3Error(404, '').code).toBe('NotFound')
    expect(parseS3Error(412, 'garbage').code).toBe('PreconditionFailed')
    expect(statusError(599).code).toBe('InternalError')
  })

  it('InitiateMultipartUploadResult', () => {
    expect(parseInitiateMultipartUpload(`<InitiateMultipartUploadResult ${NS}><Bucket>example-bucket</Bucket><Key>example-object</Key><UploadId>VXBsb2FkIElEIGZvciA2aWWpbmcncyBteS1tb3ZpZS5tMnRzIHVwbG9hZA</UploadId></InitiateMultipartUploadResult>`))
      .toEqual({ bucket: 'example-bucket', key: 'example-object', uploadId: 'VXBsb2FkIElEIGZvciA2aWWpbmcncyBteS1tb3ZpZS5tMnRzIHVwbG9hZA' })
  })

  it('CompleteMultipartUploadResult', () => {
    const r = parseCompleteMultipartUpload(`<CompleteMultipartUploadResult ${NS}><Location>http://Example-Bucket.s3.region.amazonaws.com/Example-Object</Location><Bucket>Example-Bucket</Bucket><Key>Example-Object</Key><ETag>"3858f62230ac3c915f300c664312c11f-9"</ETag></CompleteMultipartUploadResult>`, new Headers({ 'x-amz-version-id': 'v1' }))
    expect(r).toEqual({ location: 'http://Example-Bucket.s3.region.amazonaws.com/Example-Object', bucket: 'Example-Bucket', key: 'Example-Object', etag: '3858f62230ac3c915f300c664312c11f-9', versionId: 'v1' })
  })

  it('CopyObjectResult', () => {
    expect(parseCopyObjectResult(`<CopyObjectResult><LastModified>2009-10-28T22:32:00</LastModified><ETag>"9b2cf535f27731c974343645a3985328"</ETag></CopyObjectResult>`))
      .toMatchObject({ etag: '9b2cf535f27731c974343645a3985328', lastModified: '2009-10-28T22:32:00' })
  })

  it('Tagging', () => {
    expect(parseTagging(`<Tagging ${NS}><TagSet><Tag><Key>tag1</Key><Value>val1</Value></Tag><Tag><Key>tag2</Key><Value></Value></Tag></TagSet></Tagging>`))
      .toEqual([{ key: 'tag1', value: 'val1' }, { key: 'tag2', value: '' }])
  })

  it('VersioningConfiguration', () => {
    expect(parseVersioning(`<VersioningConfiguration ${NS}><Status>Enabled</Status></VersioningConfiguration>`)).toEqual({ status: 'Enabled', mfaDelete: undefined })
    expect(parseVersioning(`<VersioningConfiguration ${NS}/>`)).toEqual({ status: '', mfaDelete: undefined })
    expect(parseVersioning(`<VersioningConfiguration ${NS}><Status>Suspended</Status><MfaDelete>Disabled</MfaDelete></VersioningConfiguration>`)).toEqual({ status: 'Suspended', mfaDelete: 'Disabled' })
  })

  it('CORSConfiguration', () => {
    const rules = parseCors(`<CORSConfiguration ${NS}>
 <CORSRule>
   <AllowedOrigin>http://www.example.com</AllowedOrigin>
   <AllowedMethod>PUT</AllowedMethod><AllowedMethod>POST</AllowedMethod><AllowedMethod>DELETE</AllowedMethod>
   <AllowedHeader>*</AllowedHeader>
   <MaxAgeSeconds>3000</MaxAgeSeconds>
   <ExposeHeader>x-amz-server-side-encryption</ExposeHeader><ExposeHeader>ETag</ExposeHeader>
 </CORSRule>
 <CORSRule><ID>r2</ID><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule>
</CORSConfiguration>`)
    expect(rules).toEqual([
      { id: undefined, allowedOrigins: ['http://www.example.com'], allowedMethods: ['PUT', 'POST', 'DELETE'], allowedHeaders: ['*'], exposeHeaders: ['x-amz-server-side-encryption', 'ETag'], maxAgeSeconds: 3000 },
      { id: 'r2', allowedOrigins: ['*'], allowedMethods: ['GET'], allowedHeaders: [], exposeHeaders: [], maxAgeSeconds: undefined }
    ])
  })

  it('LifecycleConfiguration', () => {
    const rules = parseLifecycle(`<LifecycleConfiguration ${NS}>
  <Rule>
    <ID>logs</ID>
    <Filter><Prefix>logs/</Prefix></Filter>
    <Status>Enabled</Status>
    <Expiration><Days>365</Days></Expiration>
    <NoncurrentVersionExpiration><NoncurrentDays>30</NoncurrentDays><NewerNoncurrentVersions>2</NewerNoncurrentVersions></NoncurrentVersionExpiration>
    <AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload>
  </Rule>
  <Rule>
    <ID>tagged</ID>
    <Filter><And><Prefix>tmp/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag><ObjectSizeGreaterThan>1024</ObjectSizeGreaterThan></And></Filter>
    <Status>Disabled</Status>
    <Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>
    <Transition><Days>30</Days><StorageClass>GLACIER</StorageClass></Transition>
  </Rule>
  <Rule><Prefix>legacy/</Prefix><Status>Enabled</Status><Expiration><Date>2027-01-01T00:00:00Z</Date></Expiration></Rule>
</LifecycleConfiguration>`)
    expect(rules).toHaveLength(3)
    expect(rules[0]).toMatchObject({
      id: 'logs',
      status: 'Enabled',
      filter: { prefix: 'logs/' },
      expiration: { days: 365 },
      noncurrentVersionExpiration: { noncurrentDays: 30, newerNoncurrentVersions: 2 },
      abortIncompleteMultipartUpload: { daysAfterInitiation: 7 }
    })
    expect(rules[1]!.status).toBe('Disabled')
    expect(rules[1]!.filter?.and).toEqual({ prefix: 'tmp/', tags: [{ key: 'k', value: 'v' }], objectSizeGreaterThan: 1024, objectSizeLessThan: undefined })
    expect(rules[1]!.expiration?.expiredObjectDeleteMarker).toBe(true)
    expect(rules[1]!.transitions).toEqual([{ days: 30, date: undefined, storageClass: 'GLACIER' }])
    expect(rules[2]).toMatchObject({ prefix: 'legacy/', expiration: { date: '2027-01-01T00:00:00Z' } })
    expect(rules[2]!.filter).toBeUndefined()
  })

  it('location, encryption, object lock, retention, legal hold', () => {
    expect(parseLocation(`<LocationConstraint ${NS}/>`)).toBe('us-east-1')
    expect(parseLocation(`<LocationConstraint ${NS}>eu-central-1</LocationConstraint>`)).toBe('eu-central-1')
    expect(parseEncryption(`<ServerSideEncryptionConfiguration ${NS}><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>false</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`))
      .toEqual({ rules: [{ sseAlgorithm: 'AES256', kmsMasterKeyId: undefined, bucketKeyEnabled: false }] })
    expect(parseObjectLockConfiguration(`<ObjectLockConfiguration ${NS}><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>`))
      .toEqual({ enabled: true, rule: { defaultRetention: { mode: 'GOVERNANCE', days: 30, years: undefined } } })
    expect(parseObjectLockConfiguration(`<ObjectLockConfiguration ${NS}><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)).toEqual({ enabled: true })
    expect(parseRetention(`<Retention ${NS}><Mode>COMPLIANCE</Mode><RetainUntilDate>2027-01-01T00:00:00.000Z</RetainUntilDate></Retention>`))
      .toEqual({ mode: 'COMPLIANCE', retainUntilDate: '2027-01-01T00:00:00.000Z' })
    expect(parseLegalHold(`<LegalHold ${NS}><Status>ON</Status></LegalHold>`)).toBe('ON')
  })

  it('DeleteResult, ListMultipartUploadsResult, AccessControlPolicy', () => {
    const d = parseDeleteObjectsResult(`<DeleteResult ${NS}><Deleted><Key>sample1.txt</Key></Deleted><Deleted><Key>v.txt</Key><VersionId>v1</VersionId><DeleteMarker>true</DeleteMarker><DeleteMarkerVersionId>dm1</DeleteMarkerVersionId></Deleted><Error><Key>sample2.txt</Key><Code>AccessDenied</Code><Message>Access Denied</Message></Error></DeleteResult>`)
    expect(d.deleted).toEqual([
      { key: 'sample1.txt', versionId: undefined, deleteMarker: undefined, deleteMarkerVersionId: undefined },
      { key: 'v.txt', versionId: 'v1', deleteMarker: true, deleteMarkerVersionId: 'dm1' }
    ])
    expect(d.errors).toEqual([{ key: 'sample2.txt', versionId: undefined, code: 'AccessDenied', message: 'Access Denied' }])

    const u = parseListMultipartUploads(`<ListMultipartUploadsResult ${NS}><Bucket>b</Bucket><KeyMarker></KeyMarker><UploadIdMarker></UploadIdMarker><NextKeyMarker>my-movie.m2ts</NextKeyMarker><MaxUploads>3</MaxUploads><IsTruncated>true</IsTruncated>
      <Upload><Key>my-divisor</Key><UploadId>XMgbGlrZSBlbHZpbmcncyBub3QgaGF2aW5nIG11Y2ggbHVjaw</UploadId><Initiator><ID>arn:aws:iam::111122223333:user/user1</ID><DisplayName>user1</DisplayName></Initiator><StorageClass>STANDARD</StorageClass><Initiated>2010-11-10T20:48:33.000Z</Initiated></Upload></ListMultipartUploadsResult>`)
    expect(u.isTruncated).toBe(true)
    expect(u.uploads[0]).toMatchObject({ key: 'my-divisor', uploadId: 'XMgbGlrZSBlbHZpbmcncyBub3QgaGF2aW5nIG11Y2ggbHVjaw', initiated: '2010-11-10T20:48:33.000Z' })

    const acl = parseAccessControlPolicy(`<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <Owner><ID>owner-id</ID><DisplayName>me</DisplayName></Owner>
  <AccessControlList>
    <Grant><Grantee xsi:type="CanonicalUser"><ID>owner-id</ID></Grantee><Permission>FULL_CONTROL</Permission></Grant>
    <Grant><Grantee xsi:type="Group"><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>READ</Permission></Grant>
  </AccessControlList>
</AccessControlPolicy>`)
    expect(acl.owner?.id).toBe('owner-id')
    expect(acl.grants).toHaveLength(2)
    expect(acl.grants[1]!.grantee.type).toBe('Group')
    expect(acl.canned).toBe('public-read')
  })
})
