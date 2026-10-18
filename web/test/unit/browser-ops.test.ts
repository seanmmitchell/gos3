import { describe, expect, it, vi } from 'vitest'
import {
  deleteTargets,
  downloadSequentially,
  listKeysUnder,
  planTransfer,
  restoreVersion,
  runTransfer
} from '~/composables/useBrowserOps'
import type { BrowserOpsClient } from '~/composables/useBrowserOps'
import type { CopyObjectOptions, ListObjectsV2Params, ListResult, ObjectIdentifier, ObjectRef, PresignOptions } from '~/types/s3'

/** In-memory bucket store implementing the client subset. */
function fakeStore(initial: Record<string, string[]>) {
  const buckets = new Map(Object.entries(initial).map(([b, keys]) => [b, new Set(keys)]))
  const copies: { src: ObjectRef, dst: { bucket: string, key: string } }[] = []
  const client: BrowserOpsClient = {
    async headObject() {
      return { contentLength: 1, etag: 'e', metadata: {}, checksums: {}, headers: {} }
    },
    async deleteObject(bucket: string, key: string) {
      buckets.get(bucket)!.delete(key)
      return {}
    },
    async listObjectsV2(bucket: string, params: ListObjectsV2Params = {}): Promise<ListResult> {
      const all = [...(buckets.get(bucket) ?? [])].filter(k => k.startsWith(params.prefix ?? '')).sort()
      // Two keys per page to exercise pagination.
      const start = params.continuationToken ? Number(params.continuationToken) : 0
      const page = all.slice(start, start + 2)
      const more = start + 2 < all.length
      return {
        name: bucket,
        prefix: params.prefix ?? '',
        maxKeys: 2,
        keyCount: page.length,
        isTruncated: more,
        nextContinuationToken: more ? String(start + 2) : undefined,
        objects: page.map(key => ({ key, size: 1 })),
        commonPrefixes: []
      }
    },
    async deleteObjects(bucket: string, objects: ObjectIdentifier[]) {
      const set = buckets.get(bucket)!
      const deleted = []
      const errors = []
      for (const o of objects) {
        if (o.key.includes('locked')) errors.push({ key: o.key, code: 'AccessDenied', message: 'locked' })
        else {
          set.delete(o.key)
          deleted.push({ key: o.key, versionId: o.versionId })
        }
      }
      return { deleted, errors }
    },
    async deletePrefix(bucket: string, prefix: string, opts = {}) {
      const keys = await listKeysUnder(client, bucket, prefix)
      const r = await client.deleteObjects(bucket, keys.map(key => ({ key })))
      opts.onProgress?.(r.deleted.length)
      return r
    },
    async copyObject(src: ObjectRef, dst: { bucket: string, key: string }, _opts?: CopyObjectOptions) {
      if (src.key.includes('fail')) throw new Error('copy failed')
      copies.push({ src, dst })
      buckets.get(dst.bucket)!.add(dst.key)
      return { etag: 'e' }
    }
  }
  return { client, buckets, copies, keys: (b: string) => [...buckets.get(b)!].sort() }
}

describe('bulk operations', () => {
  it('lists every key under a prefix across pages', async () => {
    const s = fakeStore({ b: ['a/1', 'a/2', 'a/3', 'a/x/4', 'b/5'] })
    expect(await listKeysUnder(s.client, 'b', 'a/')).toEqual(['a/1', 'a/2', 'a/3', 'a/x/4'])
  })

  it('deletes objects in one batch and folders by prefix, collecting failures', async () => {
    const s = fakeStore({ b: ['x.txt', 'locked.txt', 'f/', 'f/1', 'f/2/3', 'keep'] })
    const progress = vi.fn()
    const r = await deleteTargets(s.client, 'b', [
      { key: 'x.txt', isFolder: false },
      { key: 'locked.txt', isFolder: false },
      { key: 'f/', isFolder: true }
    ], { onProgress: progress })
    expect(r.done).toBe(4)
    expect(r.failures).toEqual([{ key: 'locked.txt', versionId: undefined, message: 'AccessDenied: locked' }])
    expect(s.keys('b')).toEqual(['keep', 'locked.txt'])
    expect(progress).toHaveBeenCalled()
  })

  it('plans copies of objects and folders relative to the listed prefix', async () => {
    const s = fakeStore({ b: ['docs/a.txt', 'docs/sub/', 'docs/sub/b.txt', 'docs/sub/c/d.txt'] })
    const plan = await planTransfer(s.client, 'b', [
      { key: 'docs/a.txt', isFolder: false, versionId: 'v1' },
      { key: 'docs/sub/', isFolder: true }
    ], { fromPrefix: 'docs/', toPrefix: 'backup/' })
    expect(plan).toEqual([
      { src: { bucket: 'b', key: 'docs/a.txt', versionId: 'v1' }, dstKey: 'backup/a.txt' },
      { src: { bucket: 'b', key: 'docs/sub/' }, dstKey: 'backup/sub/' },
      { src: { bucket: 'b', key: 'docs/sub/b.txt' }, dstKey: 'backup/sub/b.txt' },
      { src: { bucket: 'b', key: 'docs/sub/c/d.txt' }, dstKey: 'backup/sub/c/d.txt' }
    ])
  })

  it('plans renames of a single object or a whole folder', async () => {
    const s = fakeStore({ b: ['docs/old/1', 'docs/old/2/3', 'docs/file'] })
    expect(await planTransfer(s.client, 'b', [{ key: 'docs/file', isFolder: false }], { fromPrefix: 'docs/', toPrefix: 'docs/', renameTo: 'docs/renamed' }))
      .toEqual([{ src: { bucket: 'b', key: 'docs/file', versionId: undefined }, dstKey: 'docs/renamed' }])
    const plan = await planTransfer(s.client, 'b', [{ key: 'docs/old/', isFolder: true }], { fromPrefix: 'docs/', toPrefix: 'docs/', renameTo: 'docs/new' })
    expect(plan.map(p => p.dstKey)).toEqual(['docs/new/1', 'docs/new/2/3'])
  })

  it('moves: copies then deletes successfully copied sources only', async () => {
    const s = fakeStore({ b: ['a/1', 'a/fail', 'a/2'], other: [] })
    const plan = await planTransfer(s.client, 'b', [{ key: 'a/', isFolder: true }], { fromPrefix: '', toPrefix: 'moved/' })
    const progress: number[] = []
    const r = await runTransfer(s.client, plan, 'other', { move: true, onProgress: p => progress.push(p.done) })
    expect(r.done).toBe(2)
    expect(r.failures).toEqual([{ key: 'a/fail', versionId: undefined, message: 'copy failed' }])
    expect(s.keys('other')).toEqual(['moved/a/1', 'moved/a/2'])
    expect(s.keys('b')).toEqual(['a/fail'])
    expect(progress.at(-1)).toBe(2)
  })

  it('refuses to copy an object onto itself and stops when aborted', async () => {
    const s = fakeStore({ b: ['k', 'l'] })
    const r = await runTransfer(s.client, [{ src: { bucket: 'b', key: 'k' }, dstKey: 'k' }], 'b')
    expect(r.failures[0]!.message).toMatch(/same/)
    const ctrl = new AbortController()
    ctrl.abort()
    const r2 = await runTransfer(s.client, [{ src: { bucket: 'b', key: 'l' }, dstKey: 'm' }], 'b', { signal: ctrl.signal })
    expect(r2.done).toBe(0)
    expect(s.copies).toHaveLength(0)
  })

  it('restores a version by copying it onto its own key', async () => {
    const s = fakeStore({ b: ['k'] })
    await restoreVersion(s.client, 'b', 'k', 'v1')
    expect(s.copies).toEqual([{ src: { bucket: 'b', key: 'k', versionId: 'v1' }, dst: { bucket: 'b', key: 'k' } }])
  })

  it('downloads sequentially through presigned URLs with attachment disposition', async () => {
    const presigned: { key: string, opts?: PresignOptions }[] = []
    const presigner = {
      async presignGetUrl(_b: string, key: string, opts?: PresignOptions) {
        if (key === 'bad') throw new Error('nope')
        presigned.push({ key, opts })
        return `https://s3/${key}`
      }
    }
    const urls: string[] = []
    const r = await downloadSequentially(presigner, 'b', [{ key: 'dir/a b.txt' }, { key: 'bad' }, { key: 'c', versionId: 'v9' }], { delayMs: 0, trigger: u => urls.push(u) })
    expect(urls).toEqual(['https://s3/dir/a b.txt', 'https://s3/c'])
    expect(r).toEqual({ done: 2, failures: [{ key: 'bad', versionId: undefined, message: 'nope' }] })
    expect(presigned[0]!.opts?.responseContentDisposition).toContain('filename="a b.txt"')
    expect(presigned[1]!.opts?.versionId).toBe('v9')
  })
})
