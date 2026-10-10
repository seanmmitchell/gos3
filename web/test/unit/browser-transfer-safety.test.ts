// Review fixes: transfers must not destroy data (destination inside the
// source, silent overwrites, lost ACLs) and keys with '.' / '..' segments
// must never be put into URLs.
import { describe, expect, it } from 'vitest'
import { aclToKeep, findExistingDestinations, findTransferOverlap, planTransfer, runTransfer } from '~/composables/useBrowserOps'
import type { BrowserOpsClient, DestinationLookupClient } from '~/composables/useBrowserOps'
import { browserObjectRoute, browserRoute, parseBrowserPath } from '~/composables/useBrowserPaths'
import type { AccessControlPolicy, CannedACL, CopyObjectOptions, HeadResult, ListObjectsV2Params, ListResult, ObjectIdentifier, ObjectRef } from '~/types/s3'
import { validateObjectKey } from '~/utils/paths'
import { appRoutes } from '~/utils/routes'

/** In-memory bucket with object contents and canned ACLs. */
function contentStore(objects: Record<string, string>, acls: Record<string, CannedACL | 'custom'> = {}) {
  const data = new Map(Object.entries(objects))
  const acl = new Map(Object.entries(acls))
  const heads: string[] = []
  const client: BrowserOpsClient & DestinationLookupClient = {
    async listObjectsV2(_b: string, params: ListObjectsV2Params = {}): Promise<ListResult> {
      const all = [...data.keys()].filter(k => k.startsWith(params.prefix ?? '')).sort()
      const start = params.continuationToken ? Number(params.continuationToken) : 0
      const page = all.slice(start, start + 2)
      const more = start + 2 < all.length
      return { name: 'b', prefix: params.prefix ?? '', maxKeys: 2, keyCount: page.length, isTruncated: more, nextContinuationToken: more ? String(start + 2) : undefined, objects: page.map(key => ({ key, size: 1 })), commonPrefixes: [] }
    },
    async deleteObjects(_b: string, ids: ObjectIdentifier[]) {
      for (const o of ids) data.delete(o.key)
      return { deleted: ids.map(o => ({ key: o.key })), errors: [] }
    },
    async deletePrefix() {
      return { deleted: [], errors: [] }
    },
    async copyObject(src: ObjectRef, dst: { bucket: string, key: string }, opts?: CopyObjectOptions) {
      data.set(dst.key, data.get(src.key)!)
      acl.set(dst.key, opts?.acl ?? 'private')
      return { etag: 'e' }
    },
    async getObjectAcl(_b: string, key: string): Promise<AccessControlPolicy> {
      return { grants: [], canned: acl.get(key) ?? 'private' }
    },
    async headObject(_b: string, key: string): Promise<HeadResult> {
      heads.push(key)
      if (!data.has(key)) throw Object.assign(new Error('Not Found'), { status: 404 })
      return { contentLength: 1, metadata: {}, checksums: {}, headers: {} }
    }
  }
  return { client, data, acl, heads }
}

describe('transfers into the source folder', () => {
  it('refuses a folder rename into its own subfolder before copying anything', async () => {
    const s = contentStore({ 'x/f': 'AAA', 'x/y/f': 'BBB' })
    const plan = await planTransfer(s.client, 'b', [{ key: 'x/', isFolder: true }], { fromPrefix: '', toPrefix: '', renameTo: 'x/y/' })
    expect(findTransferOverlap(plan, 'b')?.dstKey).toBe('x/y/f')
    await expect(runTransfer(s.client, plan, 'b', { move: true })).rejects.toThrow(/inside the source/)
    expect(Object.fromEntries(s.data)).toEqual({ 'x/f': 'AAA', 'x/y/f': 'BBB' })
  })

  it('refuses a folder copy whose destinations are other sources', async () => {
    const s = contentStore({ 'x/f': 'AAA', 'x/y/x/f': 'BBB' })
    const plan = await planTransfer(s.client, 'b', [{ key: 'x/', isFolder: true }], { fromPrefix: '', toPrefix: 'x/y/' })
    await expect(runTransfer(s.client, plan, 'b')).rejects.toThrow(/inside the source/)
    expect(s.data.get('x/y/x/f')).toBe('BBB')
  })

  it('allows the same keys in another bucket and versions copied onto their key', () => {
    const plan = [{ src: { bucket: 'b', key: 'x/f' }, dstKey: 'x/y/f' }, { src: { bucket: 'b', key: 'x/y/f' }, dstKey: 'x/y/y/f' }]
    expect(findTransferOverlap(plan, 'other')).toBeUndefined()
    expect(findTransferOverlap([{ src: { bucket: 'b', key: 'k', versionId: 'v1' }, dstKey: 'k' }], 'b')).toBeUndefined()
  })
})

describe('existing destination objects', () => {
  it('finds them with HeadObject for small plans', async () => {
    const s = contentStore({ 'a.txt': '1', 'b.txt': '2' })
    const r = await findExistingDestinations(s.client, 'b', [{ src: { bucket: 'b', key: 'a.txt' }, dstKey: 'b.txt' }])
    expect(r).toEqual({ existing: ['b.txt'], complete: true })
  })

  it('lists the common destination prefix for large plans', async () => {
    const objects: Record<string, string> = {}
    for (let i = 0; i < 30; i++) objects[`src/${i}`] = String(i)
    objects['dst/3'] = 'old'
    objects['dst/17'] = 'old'
    objects['zzz'] = 'other'
    const s = contentStore(objects)
    const plan = Array.from({ length: 30 }, (_, i) => ({ src: { bucket: 'b', key: `src/${i}` }, dstKey: `dst/${i}` }))
    const r = await findExistingDestinations(s.client, 'b', plan)
    expect(r.existing.sort()).toEqual(['dst/17', 'dst/3'])
    expect(r.complete).toBe(true)
    expect(s.heads).toHaveLength(0)
    // Listing capped: the keys it did not reach are checked with HeadObject.
    const capped = await findExistingDestinations(s.client, 'b', plan.map(p => ({ ...p, dstKey: `${p.src.key}c` })), { maxListed: 4 })
    expect(capped).toEqual({ existing: [], complete: true })
    expect(s.heads.length).toBeGreaterThan(0)
    s.client.headObject = async () => {
      throw Object.assign(new Error('Forbidden'), { status: 403 })
    }
    const denied = await findExistingDestinations(s.client, 'b', plan.map(p => ({ ...p, dstKey: `${p.src.key}c` })), { maxListed: 4 })
    expect(denied.complete).toBe(false)
  })

  it('does not report unknown objects when HeadObject is not allowed', async () => {
    const s = contentStore({})
    s.client.headObject = async () => {
      throw Object.assign(new Error('Forbidden'), { status: 403 })
    }
    expect(await findExistingDestinations(s.client, 'b', [{ src: { bucket: 'b', key: 'a' }, dstKey: 'c' }])).toEqual({ existing: [], complete: false })
  })
})

describe('ACLs of moved objects', () => {
  it('keeps canned ACLs on move / rename, not on copy', async () => {
    const s = contentStore({ 'pub.txt': 'p', 'priv.txt': 'q' }, { 'pub.txt': 'public-read' })
    await runTransfer(s.client, [{ src: { bucket: 'b', key: 'pub.txt' }, dstKey: 'moved/pub.txt' }, { src: { bucket: 'b', key: 'priv.txt' }, dstKey: 'moved/priv.txt' }], 'b', { move: true })
    expect(s.acl.get('moved/pub.txt')).toBe('public-read')
    expect(s.acl.get('moved/priv.txt')).toBe('private')
    await runTransfer(s.client, [{ src: { bucket: 'b', key: 'moved/pub.txt' }, dstKey: 'copy.txt' }], 'b')
    expect(s.acl.get('copy.txt')).toBe('private')
  })

  it('warns about custom grants it cannot carry over', async () => {
    const s = contentStore({ 'c.txt': 'c' }, { 'c.txt': 'custom' })
    const r = await runTransfer(s.client, [{ src: { bucket: 'b', key: 'c.txt' }, dstKey: 'd.txt' }], 'b', { move: true })
    expect(r.failures).toEqual([])
    expect(r.warnings?.[0]?.message).toMatch(/custom ACL/)
  })

  it('ignores servers without ACL support', async () => {
    expect(await aclToKeep({ getObjectAcl: async () => {
      throw new Error('NotImplemented')
    } }, { bucket: 'b', key: 'k' })).toBeUndefined()
    expect(await aclToKeep({}, { bucket: 'b', key: 'k' })).toBeUndefined()
  })
})

describe('keys with "." / ".." segments', () => {
  it('get routes that keep the key out of the URL path', () => {
    const r = browserObjectRoute('s', 'b', 'x/../y', 'v1')
    expect(r).toBe('/servers/s/buckets/b/objects/?key=x%2F..%2Fy&versionId=v1')
    expect(appRoutes.object('s', 'b', 'x/../y', 'v1')).toBe(r)
    const u = new URL(r, 'http://ui.local')
    expect(u.pathname).toBe('/servers/s/buckets/b/objects/')
    expect(parseBrowserPath(u.pathname, Object.fromEntries(u.searchParams))).toEqual({ kind: 'object', key: 'x/../y', prefix: 'x/../' })

    const f = browserRoute('s', 'b', 'x/../')
    expect(f).toBe('/servers/s/buckets/b/objects/?prefix=x%2F..%2F')
    const fu = new URL(f, 'http://ui.local')
    expect(parseBrowserPath(fu.pathname, Object.fromEntries(fu.searchParams))).toEqual({ kind: 'folder', prefix: 'x/../' })

    // Ordinary keys keep their path routes.
    expect(browserObjectRoute('s', 'b', 'a/..b')).toBe('/servers/s/buckets/b/objects/a/..b')
    expect(parseBrowserPath('/servers/s/buckets/b/objects/', {})).toEqual({ kind: 'folder', prefix: '' })
  })

  it('are rejected for new objects, renames and copies', () => {
    expect(validateObjectKey('x/../y')).toMatch(/"\." or "\.\."/)
    expect(validateObjectKey('a/./b')).toBeDefined()
    expect(validateObjectKey('..')).toBeDefined()
    expect(validateObjectKey('a/..b/c..')).toBeUndefined()
  })
})
