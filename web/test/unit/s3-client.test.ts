import { beforeEach, describe, expect, it, vi } from 'vitest'
import { choosePartSize, copySourceHeader, decodeHeaderValue, encodeHeaderValue, S3Client, S3Error } from '~/utils/s3/client'
import { md5Base64 } from '~/utils/s3/hash'
import { buildS3Url, encodeKey, normalizeEndpoint } from '~/utils/s3/url'

const MiB = 1024 * 1024
const NS = 'xmlns="http://s3.amazonaws.com/doc/2006-03-01/"'

interface Captured {
  method: string
  url: string
  headers: Record<string, string>
  body: string
}

type Responder = (req: Captured) => Response | Promise<Response>

function makeClient(responder: Responder, opts: Partial<ConstructorParameters<typeof S3Client>[0]> = {}) {
  const calls: Captured[] = []
  const fetch = vi.fn(async (req: Request) => {
    const headers: Record<string, string> = {}
    req.headers.forEach((v, k) => (headers[k] = v))
    const body = req.method === 'GET' || req.method === 'HEAD' ? '' : await req.text()
    const c = { method: req.method, url: req.url, headers, body }
    calls.push(c)
    return responder(c)
  })
  const client = new S3Client({
    endpoint: 'http://localhost:9000',
    region: 'us-east-1',
    accessKeyId: 'AKIDEXAMPLE',
    secretAccessKey: 'secret',
    forcePathStyle: true,
    datetime: '20261009T120000Z',
    fetch,
    ...opts
  })
  return { client, calls, fetch }
}

const xml = (body: string, status = 200, headers: Record<string, string> = {}) =>
  new Response(body, { status, headers: { 'content-type': 'application/xml', ...headers } })

describe('URL construction', () => {
  it('normalizes endpoints', () => {
    expect(normalizeEndpoint('localhost:9000/')).toBe('http://localhost:9000')
    expect(normalizeEndpoint(' https://s3.example.com/base/ ')).toBe('https://s3.example.com/base')
    expect(() => normalizeEndpoint('ftp://x')).toThrow()
    expect(() => normalizeEndpoint('')).toThrow()
  })

  it('encodes keys per segment (RFC 3986)', () => {
    expect(encodeKey('dir/ä+b?#.txt')).toBe('dir/%C3%A4%2Bb%3F%23.txt')
    expect(encodeKey("a (1)!'*.txt")).toBe('a%20%281%29%21%27%2A.txt')
    expect(encodeKey('a//b/')).toBe('a//b/')
  })

  it('builds path-style and virtual-hosted URLs', () => {
    const ps = { endpoint: 'http://localhost:9000', forcePathStyle: true }
    expect(buildS3Url(ps).toString()).toBe('http://localhost:9000/')
    expect(buildS3Url(ps, 'photos', 'a b/c.jpg', { versionId: 'x/y', tagging: '' }).toString())
      .toBe('http://localhost:9000/photos/a%20b/c.jpg?versionId=x%2Fy&tagging=')
    const vh = { endpoint: 'https://s3.example.com', forcePathStyle: false }
    expect(buildS3Url(vh, 'photos', 'k.txt').toString()).toBe('https://photos.s3.example.com/k.txt')
    expect(buildS3Url(vh, 'photos').toString()).toBe('https://photos.s3.example.com/')
    // dotted buckets and IP endpoints fall back to path style
    expect(buildS3Url(vh, 'my.bucket', 'k').toString()).toBe('https://s3.example.com/my.bucket/k')
    expect(buildS3Url({ endpoint: 'http://10.0.0.5:9000', forcePathStyle: false }, 'b', 'k').toString()).toBe('http://10.0.0.5:9000/b/k')
    // endpoint with base path
    expect(buildS3Url({ endpoint: 'https://proxy.example/s3', forcePathStyle: true }, 'b', 'k').toString()).toBe('https://proxy.example/s3/b/k')
  })

  it('builds copy source headers', () => {
    expect(copySourceHeader({ bucket: 'b', key: 'a b/ü.txt', versionId: 'v 1' })).toBe('b/a%20b/%C3%BC.txt?versionId=v%201')
  })

  it('chooses part sizes within S3 limits', () => {
    expect(choosePartSize(1)).toBe(8 * MiB)
    expect(choosePartSize(1, 1)).toBe(5 * MiB)
    const huge = 100 * 1024 * MiB * 1024 // 100 TiB
    const ps = choosePartSize(huge)
    expect(Math.ceil(huge / ps)).toBeLessThanOrEqual(10000)
    expect(ps % MiB).toBe(0)
  })

  it('RFC 2047 encodes non-ASCII metadata', () => {
    expect(encodeHeaderValue('plain')).toBe('plain')
    const enc = encodeHeaderValue('héllo wörld')
    expect(enc).toMatch(/^=\?UTF-8\?B\?.+\?=$/)
    expect(decodeHeaderValue(enc)).toBe('héllo wörld')
    expect(decodeHeaderValue('=?UTF-8?Q?caf=C3=A9?=')).toBe('café')
  })
})

describe('S3Client requests', () => {
  beforeEach(() => {
    vi.stubGlobal('XMLHttpRequest', undefined)
  })

  it('signs requests with SigV4 headers', async () => {
    const { client, calls } = makeClient(() => xml(`<ListAllMyBucketsResult ${NS}><Buckets><Bucket><Name>a</Name></Bucket></Buckets></ListAllMyBucketsResult>`))
    const r = await client.listBuckets()
    expect(r.buckets.map(b => b.name)).toEqual(['a'])
    const c = calls[0]!
    expect(c.method).toBe('GET')
    expect(c.url).toBe('http://localhost:9000/')
    expect(c.headers['x-amz-date']).toBe('20261009T120000Z')
    expect(c.headers['x-amz-content-sha256']).toBe('UNSIGNED-PAYLOAD')
    expect(c.headers.authorization).toMatch(/^AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE\/20261009\/us-east-1\/s3\/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=[0-9a-f]{64}$/)
  })

  it('uses the connection region in the credential scope', async () => {
    const { client, calls } = makeClient(() => xml(`<ListAllMyBucketsResult ${NS}><Buckets/></ListAllMyBucketsResult>`), { region: 'eu-west-3' })
    await client.listBuckets()
    expect(calls[0]!.headers.authorization).toContain('/20261009/eu-west-3/s3/aws4_request')
  })

  it('lists objects with query parameters (path style)', async () => {
    const { client, calls } = makeClient(() => xml(`<ListBucketResult ${NS}><Name>photos</Name><Prefix>a b/</Prefix><KeyCount>0</KeyCount><MaxKeys>100</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`))
    const r = await client.listObjectsV2('photos', { prefix: 'a b/', delimiter: '/', maxKeys: 100, continuationToken: 'x+y=' })
    expect(r.prefix).toBe('a b/')
    expect(calls[0]!.url).toBe('http://localhost:9000/photos/?list-type=2&prefix=a%20b%2F&delimiter=%2F&continuation-token=x%2By%3D&max-keys=100')
  })

  it('paginates with listObjectsV2Pages / listAllObjects', async () => {
    let n = 0
    const { client, calls } = makeClient(() => {
      n++
      return xml(`<ListBucketResult ${NS}><Name>b</Name><KeyCount>1</KeyCount><MaxKeys>1</MaxKeys><IsTruncated>${n < 3}</IsTruncated>${n < 3 ? `<NextContinuationToken>t${n}</NextContinuationToken>` : ''}<Contents><Key>k${n}</Key><Size>${n}</Size></Contents></ListBucketResult>`)
    })
    const keys: string[] = []
    for await (const o of client.listAllObjects('b')) keys.push(o.key)
    expect(keys).toEqual(['k1', 'k2', 'k3'])
    expect(calls[1]!.url).toContain('continuation-token=t1')
    expect(calls[2]!.url).toContain('continuation-token=t2')
  })

  it('uses virtual-hosted URLs when not forcing path style', async () => {
    const { client, calls } = makeClient(() => new Response(null, { status: 200, headers: { 'content-length': '3', 'etag': '"abc"', 'x-amz-meta-color': 'blue', 'content-type': 'text/plain', 'x-amz-checksum-crc32': 'y/Q5Jg==', 'x-amz-object-lock-legal-hold': 'ON' } }), { endpoint: 'https://s3.example.com', forcePathStyle: false })
    const h = await client.headObject('photos', 'dir/ä.txt', { versionId: 'v1' })
    expect(calls[0]!.method).toBe('HEAD')
    expect(calls[0]!.url).toBe('https://photos.s3.example.com/dir/%C3%A4.txt?versionId=v1')
    expect(calls[0]!.headers['x-amz-checksum-mode']).toBe('ENABLED')
    expect(h).toMatchObject({ contentLength: 3, etag: 'abc', contentType: 'text/plain', metadata: { color: 'blue' }, checksums: { CRC32: 'y/Q5Jg==' }, objectLockLegalHold: 'ON' })
  })

  it('maps HEAD errors (no body) to status-based codes', async () => {
    const { client } = makeClient(() => new Response(null, { status: 404, headers: { 'x-amz-request-id': 'RID' } }))
    const err = await client.headObject('b', 'missing').catch(e => e)
    expect(err).toBeInstanceOf(S3Error)
    expect(err).toMatchObject({ code: 'NotFound', status: 404, requestId: 'RID' })
  })

  it('parses XML error bodies', async () => {
    const { client } = makeClient(() => xml('<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message><BucketName>nope</BucketName></Error>', 404))
    await expect(client.listObjectsV2('nope')).rejects.toMatchObject({ code: 'NoSuchBucket', status: 404, message: 'The specified bucket does not exist' })
  })

  it('turns fetch failures into NetworkError', async () => {
    const client = new S3Client({ endpoint: 'http://unreachable:1', accessKeyId: 'a', secretAccessKey: 'b', fetch: () => Promise.reject(new TypeError('Failed to fetch')) })
    await expect(client.listBuckets()).rejects.toMatchObject({ code: 'NetworkError', status: 0 })
  })

  it('creates buckets with region and object lock', async () => {
    const { client, calls } = makeClient(() => new Response(null, { status: 200 }))
    await client.createBucket('new-bucket', { region: 'eu-west-1', objectLock: true })
    expect(calls[0]!.method).toBe('PUT')
    expect(calls[0]!.url).toBe('http://localhost:9000/new-bucket/')
    expect(calls[0]!.headers['x-amz-bucket-object-lock-enabled']).toBe('true')
    expect(calls[0]!.body).toContain('<LocationConstraint>eu-west-1</LocationConstraint>')
  })

  it('deleteObjects batches by 1000 with Content-MD5', async () => {
    const { client, calls } = makeClient(req => {
      const keys = [...req.body.matchAll(/<Key>(.*?)<\/Key>/g)].map(m => m[1])
      return xml(`<DeleteResult ${NS}>${keys.map(k => `<Deleted><Key>${k}</Key></Deleted>`).join('')}</DeleteResult>`)
    })
    const objects = Array.from({ length: 2500 }, (_, i) => ({ key: `k${i}` }))
    const r = await client.deleteObjects('b', objects)
    expect(calls).toHaveLength(3)
    for (const c of calls) {
      expect(c.method).toBe('POST')
      expect(c.url).toBe('http://localhost:9000/b/?delete=')
      expect(c.headers['content-md5']).toBe(md5Base64(c.body))
    }
    expect(r.deleted).toHaveLength(2500)
    expect(r.errors).toHaveLength(0)
  })

  it('returns null / [] for missing configurations but rethrows other 404s', async () => {
    let body = '<Error><Code>NoSuchBucketPolicy</Code><Message>none</Message></Error>'
    const { client } = makeClient(() => xml(body, 404))
    expect(await client.getBucketPolicy('b')).toBeNull()
    body = '<Error><Code>NoSuchCORSConfiguration</Code><Message>none</Message></Error>'
    expect(await client.getBucketCors('b')).toEqual([])
    body = '<Error><Code>NoSuchTagSet</Code><Message>none</Message></Error>'
    expect(await client.getBucketTagging('b')).toEqual([])
    body = '<Error><Code>NoSuchLifecycleConfiguration</Code><Message>none</Message></Error>'
    expect(await client.getBucketLifecycle('b')).toEqual([])
    body = '<Error><Code>ServerSideEncryptionConfigurationNotFoundError</Code><Message>none</Message></Error>'
    expect(await client.getBucketEncryption('b')).toBeNull()
    body = '<Error><Code>NoSuchBucket</Code><Message>gone</Message></Error>'
    await expect(client.getBucketPolicy('b')).rejects.toMatchObject({ code: 'NoSuchBucket' })
  })

  it('puts configurations with Content-MD5', async () => {
    const { client, calls } = makeClient(() => new Response(null, { status: 200 }))
    await client.putBucketVersioning('b', 'Enabled')
    await client.putBucketPolicy('b', '{"Version":"2012-10-17","Statement":[]}')
    await client.putObjectTagging('b', 'k', [{ key: 'a', value: '1' }], 'v1')
    expect(calls[0]!.url).toBe('http://localhost:9000/b/?versioning=')
    expect(calls[1]!.headers['content-type']).toBe('application/json')
    expect(calls[2]!.url).toBe('http://localhost:9000/b/k?tagging=&versionId=v1')
    for (const c of calls) expect(c.headers['content-md5']).toBe(md5Base64(c.body))
  })

  it('copies objects and detects 200-with-Error responses', async () => {
    let fail = false
    const { client, calls } = makeClient(() => fail
      ? xml('<Error><Code>InternalError</Code><Message>boom</Message></Error>', 200)
      : xml(`<CopyObjectResult><ETag>"e"</ETag><LastModified>2026-10-09T12:00:00.000Z</LastModified></CopyObjectResult>`, 200, { 'x-amz-version-id': 'v2' }))
    const r = await client.copyObject({ bucket: 'src', key: 'a b.txt' }, { bucket: 'dst', key: 'c.txt' }, { metadataDirective: 'REPLACE', metadata: { Owner: 'me' }, contentType: 'text/plain' })
    expect(r).toMatchObject({ etag: 'e', versionId: 'v2' })
    const h = calls[0]!.headers
    expect(h['x-amz-copy-source']).toBe('src/a%20b.txt')
    expect(h['x-amz-metadata-directive']).toBe('REPLACE')
    expect(h['x-amz-meta-owner']).toBe('me')
    fail = true
    await expect(client.copyObject({ bucket: 'src', key: 'a' }, { bucket: 'dst', key: 'b' })).rejects.toMatchObject({ code: 'InternalError' })
  })

  it('puts objects with metadata and tags', async () => {
    const { client, calls } = makeClient(() => new Response(null, { status: 200, headers: { etag: '"x"', 'x-amz-version-id': 'v9' } }))
    const progress = vi.fn()
    const r = await client.putObject('b', 'k.txt', 'hello', { contentType: 'text/plain', metadata: { a: 'ü' }, tags: [{ key: 't', value: 'v' }], onProgress: progress })
    expect(r).toEqual({ etag: 'x', versionId: 'v9' })
    expect(calls[0]!.body).toBe('hello')
    expect(calls[0]!.headers['x-amz-meta-a']).toMatch(/^=\?UTF-8\?B\?/)
    expect(calls[0]!.headers['x-amz-tagging']).toBe('t=v')
    expect(progress).toHaveBeenCalledWith({ loaded: 5, total: 5 })
  })

  it('uploads large files with multipart (parallel parts, ordered completion)', async () => {
    const { client, calls } = makeClient((req) => {
      const u = new URL(req.url)
      if (req.method === 'POST' && u.searchParams.has('uploads')) return xml(`<InitiateMultipartUploadResult ${NS}><Bucket>b</Bucket><Key>big.bin</Key><UploadId>UP1</UploadId></InitiateMultipartUploadResult>`)
      if (req.method === 'PUT') return new Response(null, { status: 200, headers: { etag: `"etag-${u.searchParams.get('partNumber')}"` } })
      if (req.method === 'POST') return xml(`<CompleteMultipartUploadResult ${NS}><Bucket>b</Bucket><Key>big.bin</Key><ETag>"final-3"</ETag></CompleteMultipartUploadResult>`)
      return new Response(null, { status: 500 })
    })
    const file = new Blob([new Uint8Array(12 * MiB)])
    const progress = vi.fn()
    const r = await client.uploadLarge('b', 'big.bin', file, { partSize: 5 * MiB, onProgress: progress })
    expect(r.etag).toBe('final-3')
    const parts = calls.filter(c => c.method === 'PUT')
    expect(parts).toHaveLength(3)
    expect(parts.map(p => p.body.length).sort((a, b) => a - b)).toEqual([2 * MiB, 5 * MiB, 5 * MiB])
    const complete = calls.at(-1)!
    expect(complete.url).toBe('http://localhost:9000/b/big.bin?uploadId=UP1')
    expect(complete.body.indexOf('<PartNumber>1</PartNumber>')).toBeLessThan(complete.body.indexOf('<PartNumber>2</PartNumber>'))
    expect(complete.body).toContain('&quot;etag-3&quot;')
    expect(progress).toHaveBeenLastCalledWith({ loaded: 12 * MiB, total: 12 * MiB })
  })

  it('aborts the multipart upload when a part fails', async () => {
    const { client, calls } = makeClient((req) => {
      const u = new URL(req.url)
      if (req.method === 'POST' && u.searchParams.has('uploads')) return xml(`<InitiateMultipartUploadResult ${NS}><Bucket>b</Bucket><Key>k</Key><UploadId>UP2</UploadId></InitiateMultipartUploadResult>`)
      if (req.method === 'PUT' && u.searchParams.get('partNumber') === '2') return xml('<Error><Code>AccessDenied</Code><Message>no</Message></Error>', 403)
      if (req.method === 'PUT') return new Response(null, { status: 200, headers: { etag: '"p"' } })
      if (req.method === 'DELETE') return new Response(null, { status: 204 })
      return new Response(null, { status: 500 })
    })
    const file = new Blob([new Uint8Array(11 * MiB)])
    await expect(client.uploadLarge('b', 'k', file, { partSize: 5 * MiB, concurrency: 1 })).rejects.toMatchObject({ code: 'AccessDenied' })
    const abort = calls.find(c => c.method === 'DELETE')
    expect(abort?.url).toBe('http://localhost:9000/b/k?uploadId=UP2')
    expect(calls.some(c => c.method === 'POST' && c.url.includes('uploadId=UP2'))).toBe(false)
  })

  it('cancels uploads via AbortSignal', async () => {
    const ctrl = new AbortController()
    const { client, calls } = makeClient(async (req) => {
      const u = new URL(req.url)
      if (req.method === 'POST' && u.searchParams.has('uploads')) return xml(`<InitiateMultipartUploadResult ${NS}><UploadId>UP3</UploadId></InitiateMultipartUploadResult>`)
      if (req.method === 'PUT') {
        ctrl.abort()
        throw new DOMException('aborted', 'AbortError')
      }
      return new Response(null, { status: 204 })
    })
    const file = new Blob([new Uint8Array(6 * MiB)])
    await expect(client.uploadLarge('b', 'k', file, { partSize: 5 * MiB, signal: ctrl.signal })).rejects.toMatchObject({ code: 'Aborted' })
    expect(calls.some(c => c.method === 'DELETE' && c.url.includes('uploadId=UP3'))).toBe(true)
  })

  it('reads text previews with a size cap', async () => {
    const { client, calls } = makeClient(() => new Response('hello', { status: 206, headers: { 'content-range': 'bytes 0-4/100', 'content-type': 'text/plain' } }))
    const r = await client.getObjectText('b', 'k.txt', { maxBytes: 5 })
    expect(calls[0]!.headers.range).toBe('bytes=0-4')
    expect(r).toEqual({ text: 'hello', truncated: true, contentType: 'text/plain', size: 100 })
  })

  it('treats 416 on empty objects as empty text', async () => {
    const { client } = makeClient(() => xml('<Error><Code>InvalidRange</Code><Message>x</Message></Error>', 416))
    expect(await client.getObjectText('b', 'empty')).toEqual({ text: '', truncated: false, size: 0 })
  })
})

describe('presigned URLs', () => {
  it('matches the AWS SigV4 documentation example', async () => {
    // https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html
    const client = new S3Client({
      endpoint: 'https://s3.amazonaws.com',
      region: 'us-east-1',
      accessKeyId: 'AKIAIOSFODNN7EXAMPLE',
      secretAccessKey: 'wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY',
      forcePathStyle: false,
      datetime: '20130524T000000Z'
    })
    const url = new URL(await client.presign('GET', 'examplebucket', 'test.txt', 86400))
    expect(url.host).toBe('examplebucket.s3.amazonaws.com')
    expect(url.pathname).toBe('/test.txt')
    expect(url.searchParams.get('X-Amz-Algorithm')).toBe('AWS4-HMAC-SHA256')
    expect(url.searchParams.get('X-Amz-Credential')).toBe('AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request')
    expect(url.searchParams.get('X-Amz-Expires')).toBe('86400')
    expect(url.searchParams.get('X-Amz-SignedHeaders')).toBe('host')
    expect(url.searchParams.get('X-Amz-Signature')).toBe('aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404')
  })

  it('includes version and response overrides and clamps expiry', async () => {
    const client = new S3Client({ endpoint: 'http://localhost:9000', accessKeyId: 'a', secretAccessKey: 'b' })
    const url = new URL(await client.presignGetUrl('b', 'dir/a b.txt', { versionId: 'v1', responseContentDisposition: 'attachment; filename="a b.txt"', expiresSeconds: 10 ** 9 }))
    expect(url.pathname).toBe('/b/dir/a%20b.txt')
    expect(url.searchParams.get('versionId')).toBe('v1')
    expect(url.searchParams.get('response-content-disposition')).toBe('attachment; filename="a b.txt"')
    expect(url.searchParams.get('X-Amz-Expires')).toBe('604800')
    expect(url.searchParams.get('X-Amz-Signature')).toMatch(/^[0-9a-f]{64}$/)
  })
})
