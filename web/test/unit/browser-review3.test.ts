// Review fixes (round 3): metadata edits must not delete what CORS hides,
// overwrite checks must ask the server (and ask when they are incomplete),
// restores keep the ACL, a stale upload attempt must not hijack a retry, and
// new keys keep backslashes and spaces.
import { describe, expect, it } from 'vitest'
import { findExistingKeys, restoreVersion } from '~/composables/useBrowserOps'
import type { DestinationLookupClient } from '~/composables/useBrowserOps'
import { filesFromInput } from '~/composables/useBrowserDrop'
import { folderKey, normalizeRelativePath, uploadKey, validateFolderName } from '~/composables/useBrowserPaths'
import { UploadQueue } from '~/composables/useUploads'
import type { Uploader } from '~/composables/useUploads'
import type { AccessControlPolicy, CannedACL, CopyObjectOptions, HeadResult, ListObjectsV2Params, ListResult, ObjectRef, UploadLargeOptions } from '~/types/s3'
import { contentDispositionHeader, parseHeadHeaders, S3Client } from '~/utils/s3/client'
import { S3Error } from '~/utils/s3/errors'

const tick = () => new Promise(r => setTimeout(r, 0))

function client(responder: (req: Request) => Response) {
  const calls: Request[] = []
  const c = new S3Client({
    endpoint: 'http://localhost:9000',
    region: 'us-east-1',
    accessKeyId: 'AKIDEXAMPLE',
    secretAccessKey: 'secret',
    forcePathStyle: true,
    fetch: async (req: Request) => {
      calls.push(req)
      return responder(req)
    }
  })
  return { client: c, calls }
}

/** A Response as a cross-origin fetch returns it (type 'cors'). */
function corsResponse(headers: Record<string, string>): Response {
  const r = new Response(null, { status: 200, headers })
  Object.defineProperty(r, 'type', { value: 'cors' })
  return r
}

describe('headers hidden by CORS', () => {
  it('flags cross-origin responses that do not expose every header', () => {
    // ExposeHeader ETag only: the browser shows the safelisted headers and ETag.
    const hidden = parseHeadHeaders(new Headers({ 'content-type': 'text/plain', 'etag': '"e"' }), { crossOrigin: true })
    expect(hidden.headersHidden).toBe(true)
    expect(hidden.metadata).toEqual({})
    // ExposeHeader "*": Date (and everything else) is visible.
    const all = parseHeadHeaders(new Headers({ 'date': 'Sat, 10 Oct 2026 10:00:00 GMT', 'x-amz-meta-owner': 'alice' }), { crossOrigin: true })
    expect(all.headersHidden).toBeUndefined()
    expect(all.metadata).toEqual({ owner: 'alice' })
    // Same-origin responses are never filtered.
    expect(parseHeadHeaders(new Headers({ etag: '"e"' })).headersHidden).toBeUndefined()
  })

  it('HeadObject reports hidden headers of a CORS response', async () => {
    const c = client(() => corsResponse({ 'content-type': 'text/plain', 'content-length': '3', 'etag': '"e"' }))
    expect((await c.client.headObject('b', 'k')).headersHidden).toBe(true)
    const d = client(() => corsResponse({ 'content-length': '3', 'date': 'Sat, 10 Oct 2026 10:00:00 GMT' }))
    expect((await d.client.headObject('b', 'k')).headersHidden).toBeUndefined()
  })
})

describe('Content-Disposition with non-ASCII file names', () => {
  it('uses RFC 6266 / 5987 instead of RFC 2047', () => {
    expect(contentDispositionHeader('inline')).toBe('inline')
    expect(contentDispositionHeader('attachment; filename="résumé.pdf"'))
      .toBe('attachment; filename="r_sum_.pdf"; filename*=UTF-8\'\'r%C3%A9sum%C3%A9.pdf')
    expect(contentDispositionHeader('attachment; filename=日本.txt; size=3'))
      .toBe('attachment; filename="__.txt"; filename*=UTF-8\'\'%E6%97%A5%E6%9C%AC.txt; size=3')
    // An explicit filename* is kept; only the fallback is made ASCII.
    expect(contentDispositionHeader('attachment; filename="é"; filename*=UTF-8\'\'%C3%A9'))
      .toBe('attachment; filename="_"; filename*=UTF-8\'\'%C3%A9')
    expect(() => contentDispositionHeader('pièce-jointe')).toThrow(/only the filename/)
    expect(() => contentDispositionHeader('attachment; title="é"')).toThrow(/only the filename/)
  })

  it('CopyObject REPLACE sends the RFC 5987 form', async () => {
    const c = client(() => new Response('<CopyObjectResult><ETag>"e"</ETag></CopyObjectResult>', { status: 200 }))
    await c.client.copyObject({ bucket: 'b', key: 'k' }, { bucket: 'b', key: 'k' }, { metadataDirective: 'REPLACE', contentDisposition: 'attachment; filename="résumé.pdf"', metadata: { note: 'é' } })
    const h = c.calls[0]!.headers
    expect(h.get('content-disposition')).toBe('attachment; filename="r_sum_.pdf"; filename*=UTF-8\'\'r%C3%A9sum%C3%A9.pdf')
    expect(h.get('x-amz-meta-note')).toBe('=?UTF-8?B?w6k=?=') // metadata keeps the SDK convention
  })

  it('decodes values stored as RFC 2047 by earlier versions', () => {
    const v = parseHeadHeaders(new Headers({ 'content-disposition': '=?UTF-8?B?YXR0YWNobWVudDsgZmlsZW5hbWU9InLDqXN1bcOpLnBkZiI=?=' }))
    expect(v.contentDisposition).toBe('attachment; filename="résumé.pdf"')
  })
})

/** Bucket with sorted keys, listing pages of `pageSize`, HEAD 404 for missing keys. */
function lookupStore(keys: string[], pageSize = 1000) {
  const data = new Set(keys)
  const heads: string[] = []
  let listed = 0
  const c: DestinationLookupClient = {
    async listObjectsV2(_b: string, params: ListObjectsV2Params = {}): Promise<ListResult> {
      const all = [...data].filter(k => k.startsWith(params.prefix ?? '')).sort()
      const start = params.continuationToken ? Number(params.continuationToken) : 0
      const page = all.slice(start, start + pageSize)
      listed += page.length
      const more = start + pageSize < all.length
      return { name: 'b', prefix: params.prefix ?? '', maxKeys: pageSize, keyCount: page.length, isTruncated: more, nextContinuationToken: more ? String(start + pageSize) : undefined, objects: page.map(key => ({ key, size: 1 })), commonPrefixes: [] }
    },
    async headObject(_b: string, key: string): Promise<HeadResult> {
      heads.push(key)
      if (!data.has(key)) throw Object.assign(new Error('Not Found'), { status: 404 })
      return { contentLength: 1, metadata: {}, checksums: {}, headers: {} }
    }
  }
  return { client: c, heads, listed: () => listed }
}

describe('existing keys beyond the listing budget', () => {
  const many = Array.from({ length: 11000 }, (_, i) => `many/obj-${String(i).padStart(5, '0')}.txt`)

  it('checks the keys the listing did not reach with HeadObject', async () => {
    const s = lookupStore(many)
    const wanted = [...Array.from({ length: 20 }, (_, i) => `many/a${String(i).padStart(2, '0')}.txt`), 'many/obj-10999.txt']
    const r = await findExistingKeys(s.client, 'b', wanted)
    expect(r).toEqual({ existing: ['many/obj-10999.txt'], complete: true })
    expect(s.listed()).toBe(10000)
    expect(s.heads).toEqual(['many/obj-10999.txt'])
  })

  it('is incomplete when too many keys remain', async () => {
    const s = lookupStore(many, 10)
    const wanted = many.slice(0, 600).map(k => `${k}.bak`)
    const r = await findExistingKeys(s.client, 'b', wanted, { maxListed: 10 })
    expect(r.complete).toBe(false)
    expect(s.heads).toHaveLength(0)
  })

  it('finds files of a folder upload below the current level', async () => {
    const s = lookupStore(['many/obj-00001.txt', 'many/other.txt', 'top.txt'])
    const r = await findExistingKeys(s.client, 'b', ['many/obj-00001.txt', 'many/new.txt'])
    expect(r).toEqual({ existing: ['many/obj-00001.txt'], complete: true })
  })

  it('is incomplete when HeadObject is denied', async () => {
    const s = lookupStore([])
    s.client.headObject = async () => {
      throw Object.assign(new Error('Forbidden'), { status: 403 })
    }
    expect(await findExistingKeys(s.client, 'b', ['a'])).toEqual({ existing: [], complete: false })
  })
})

describe('restoring a version', () => {
  function aclStore(acl: CannedACL | 'custom') {
    const copies: { src: ObjectRef, opts?: CopyObjectOptions }[] = []
    const aclReads: (string | undefined)[] = []
    const c = {
      async copyObject(src: ObjectRef, _dst: { bucket: string, key: string }, opts?: CopyObjectOptions) {
        copies.push({ src, opts })
        return { etag: 'e' }
      },
      async getObjectAcl(_b: string, _k: string, versionId?: string): Promise<AccessControlPolicy> {
        aclReads.push(versionId)
        return { grants: [], canned: acl }
      }
    }
    return { client: c, copies, aclReads }
  }

  it('keeps the canned ACL of the restored version', async () => {
    const s = aclStore('public-read')
    const r = await restoreVersion(s.client, 'b', 'pub.txt', 'v1')
    expect(r.aclReset).toBe(false)
    expect(s.aclReads).toEqual(['v1'])
    expect(s.copies[0]!.opts?.acl).toBe('public-read')
  })

  it('reports custom grants it cannot carry over', async () => {
    const s = aclStore('custom')
    const r = await restoreVersion(s.client, 'b', 'k', 'v1')
    expect(r.aclReset).toBe(true)
    expect(s.copies[0]!.opts?.acl).toBeUndefined()
  })
})

describe('upload cancel then retry', () => {
  it('a late cancellation of the old attempt leaves the new attempt alone', async () => {
    // Multipart: the canceled attempt only settles after AbortMultipartUpload.
    const attempts: { opts: UploadLargeOptions, finishAbort: () => void, resolve: () => void }[] = []
    const c: Uploader = {
      putObject: () => Promise.reject(new Error('unexpected')),
      uploadLarge: (_b, key, _f, opts = {}) => new Promise((resolve, reject) => {
        const a = {
          opts,
          finishAbort: () => reject(new S3Error({ code: 'Aborted', message: 'aborted', status: 0 })),
          resolve: () => resolve({ bucket: 'b', key, etag: 'e', versionId: undefined })
        }
        attempts.push(a)
      })
    }
    const q = new UploadQueue({ multipartThreshold: 1 })
    const [item] = q.enqueue({ client: c, connectionId: 'c', bucket: 'b', files: [{ file: new File([new Uint8Array(10)], 'big.bin'), key: 'big.bin' }] })
    await tick()
    q.cancel(item!.id)
    expect(item!.status).toBe('canceled')
    q.retry(item!.id)
    await tick()
    expect(attempts).toHaveLength(2)
    expect(item!.status).toBe('uploading')

    attempts[0]!.finishAbort() // the first attempt's abort returns now
    await tick()
    expect(item!.status).toBe('uploading')
    expect(q.stats.value.active).toBe(1)

    // The new attempt can still be canceled.
    q.cancel(item!.id)
    expect(attempts[1]!.opts.signal?.aborted).toBe(true)
    attempts[1]!.finishAbort()
    await tick()
    expect(item!.status).toBe('canceled')
  })

  it('a stale attempt that completes does not mark the retry done', async () => {
    const attempts: { resolve: () => void, reject: (e: unknown) => void }[] = []
    const c: Uploader = {
      putObject: (_b, key) => new Promise((resolve, reject) => attempts.push({ resolve: () => resolve({ etag: 'e' }), reject })),
      uploadLarge: () => Promise.reject(new Error('unexpected'))
    }
    const q = new UploadQueue()
    const [item] = q.enqueue({ client: c, connectionId: 'c', bucket: 'b', files: [{ file: new File(['x'], 'a.txt'), key: 'a.txt' }] })
    await tick()
    q.cancel(item!.id)
    q.retry(item!.id)
    await tick()
    attempts[0]!.resolve()
    await tick()
    expect(item!.status).toBe('uploading')
    attempts[1]!.resolve()
    await tick()
    expect(item!.status).toBe('done')
  })
})

describe('keys of new objects', () => {
  it('keeps backslashes in file names', () => {
    expect(normalizeRelativePath('report\\2026.txt')).toBe('report\\2026.txt')
    expect(uploadKey('docs/', 'a\\b.txt')).toBe('docs/a\\b.txt')
    const f = new File(['x'], 'report\\2026.txt')
    expect(filesFromInput([f])[0]!.relativePath).toBe('report\\2026.txt')
  })

  it('keeps leading and trailing spaces in folder names', () => {
    expect(folderKey('', 'trailingspacefolder ')).toBe('trailingspacefolder /')
    expect(folderKey('a/', ' lead')).toBe('a/ lead/')
    expect(validateFolderName('trailingspacefolder ')).toBeUndefined()
    expect(validateFolderName('   ')).toBeDefined()
  })
})
