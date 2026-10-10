import { describe, expect, it } from 'vitest'
import { UploadQueue, uploadEta } from '~/composables/useUploads'
import type { Uploader } from '~/composables/useUploads'
import type { PutObjectOptions, UploadLargeOptions } from '~/types/s3'
import { S3Error } from '~/utils/s3/errors'

interface Call {
  method: 'put' | 'multipart'
  key: string
  opts: PutObjectOptions | UploadLargeOptions
  resolve: (versionId?: string) => void
  reject: (e: unknown) => void
}

/** Fake client: every call stays pending until the test settles it. */
function fakeClient() {
  const calls: Call[] = []
  let active = 0
  let maxActive = 0
  const start = (method: Call['method'], key: string, opts: PutObjectOptions = {}) =>
    new Promise<{ versionId?: string, etag: string, bucket: string, key: string }>((resolve, reject) => {
      active++
      maxActive = Math.max(maxActive, active)
      const done = () => active--
      const call: Call = {
        method,
        key,
        opts,
        resolve: (v) => {
          done()
          resolve({ versionId: v, etag: 'e', bucket: 'b', key })
        },
        reject: (e) => {
          done()
          reject(e)
        }
      }
      opts.signal?.addEventListener('abort', () => call.reject(new S3Error({ code: 'Aborted', message: 'aborted', status: 0 })))
      calls.push(call)
    })
  const client: Uploader = {
    putObject: (_b, key, _body, opts) => start('put', key, opts),
    uploadLarge: (_b, key, _file, opts) => start('multipart', key, opts)
  }
  return { client, calls, maxActive: () => maxActive, active: () => active }
}

const tick = () => new Promise(r => setTimeout(r, 0))
const file = (name: string, size: number) => new File([new Uint8Array(size)], name, { type: name.endsWith('.txt') ? 'text/plain' : '' })

describe('UploadQueue', () => {
  it('limits concurrency to 3 and starts queued files as others finish', async () => {
    const f = fakeClient()
    const q = new UploadQueue()
    q.enqueue({ client: f.client, connectionId: 'c', bucket: 'b', files: [1, 2, 3, 4, 5].map(i => ({ file: file(`${i}.bin`, 10), key: `k${i}` })) })
    await tick()
    expect(f.calls.map(c => c.key)).toEqual(['k1', 'k2', 'k3'])
    expect(q.items.map(i => i.status)).toEqual(['uploading', 'uploading', 'uploading', 'queued', 'queued'])
    expect(q.stats.value).toMatchObject({ active: 3, queued: 2, pending: 5 })

    f.calls[1]!.resolve('v2')
    await tick()
    expect(f.calls.map(c => c.key)).toEqual(['k1', 'k2', 'k3', 'k4'])
    expect(q.items[1]).toMatchObject({ status: 'done', loaded: 10, versionId: 'v2' })

    for (const c of f.calls.slice()) c.resolve()
    await tick()
    for (const c of f.calls.slice(4)) c.resolve()
    await tick()
    expect(f.calls).toHaveLength(5)
    expect(f.maxActive()).toBe(3)
    expect(q.items.every(i => i.status === 'done')).toBe(true)
    expect(q.stats.value).toMatchObject({ done: 5, pending: 0, loaded: 50, total: 50 })
  })

  it('uses multipart above the threshold and passes the content type', async () => {
    const f = fakeClient()
    const q = new UploadQueue({ multipartThreshold: 100 })
    const [small, big] = q.enqueue({ client: f.client, connectionId: 'c', bucket: 'b', files: [{ file: file('a.txt', 100), key: 'a.txt' }, { file: file('b.md', 101), key: 'b.md' }] })
    await tick()
    expect(f.calls.map(c => c.method)).toEqual(['put', 'multipart'])
    expect(small!.multipart).toBe(false)
    expect(big!.multipart).toBe(true)
    expect(f.calls[0]!.opts.contentType).toBe('text/plain')
    expect(f.calls[1]!.opts.contentType).toBe('text/markdown') // guessed from the extension
  })

  it('reports progress and a smoothed speed', async () => {
    let now = 1000
    const f = fakeClient()
    const q = new UploadQueue({ now: () => now })
    const [it] = q.enqueue({ client: f.client, connectionId: 'c', bucket: 'b', files: [{ file: file('a.bin', 4000), key: 'a' }] })
    await tick()
    const onProgress = f.calls[0]!.opts.onProgress!
    now += 500
    onProgress({ loaded: 1000, total: 4000 })
    expect(it!.loaded).toBe(1000)
    expect(it!.speed).toBe(2000) // 1000 B in 0.5 s
    now += 500
    onProgress({ loaded: 3000, total: 4000 })
    expect(it!.speed).toBeCloseTo(2000 * 0.7 + 4000 * 0.3)
    now += 1000
    f.calls[0]!.resolve()
    await tick()
    expect(it!.status).toBe('done')
    expect(it!.speed).toBe(2000) // 4000 B in 2 s average
  })

  it('cancels running and queued uploads, aborting the request', async () => {
    const f = fakeClient()
    const q = new UploadQueue({ concurrency: 1 })
    const settled: string[] = []
    q.onSettled(i => settled.push(`${i.key}:${i.status}`))
    const [a, b] = q.enqueue({ client: f.client, connectionId: 'c', bucket: 'b', files: [{ file: file('a', 1), key: 'a' }, { file: file('b', 1), key: 'b' }] })
    await tick()
    q.cancel(b!.id) // queued
    expect(b!.status).toBe('canceled')
    q.cancel(a!.id) // running
    await tick()
    expect(a!.status).toBe('canceled')
    expect(f.calls[0]!.opts.signal!.aborted).toBe(true)
    expect(f.calls).toHaveLength(1)
    expect(settled.sort()).toEqual(['a:canceled', 'b:canceled'])
    expect(q.stats.value.pending).toBe(0)
  })

  it('records errors and retries them', async () => {
    const f = fakeClient()
    const q = new UploadQueue()
    const settled: string[] = []
    q.onSettled(i => settled.push(i.status))
    const [it] = q.enqueue({ client: f.client, connectionId: 'c', bucket: 'b', files: [{ file: file('a', 5), key: 'a' }] })
    await tick()
    f.calls[0]!.reject(new S3Error({ code: 'AccessDenied', message: 'Access Denied', status: 403 }))
    await tick()
    expect(it!.status).toBe('error')
    expect(it!.error).toContain('Access Denied')
    expect(q.stats.value.failed).toBe(1)

    q.retryFailed()
    await tick()
    expect(it!.status).toBe('uploading')
    expect(it!.attempts).toBe(2)
    f.calls[1]!.resolve()
    await tick()
    expect(it!.status).toBe('done')
    expect(settled).toEqual(['error', 'done'])
  })

  it('clears finished items but keeps failed ones', async () => {
    const f = fakeClient()
    const q = new UploadQueue()
    q.enqueue({ client: f.client, connectionId: 'c', bucket: 'b', files: ['a', 'b', 'c'].map(k => ({ file: file(k, 1), key: k })) })
    await tick()
    f.calls[0]!.resolve()
    f.calls[1]!.reject(new Error('boom'))
    q.cancel(q.items[2]!.id)
    await tick()
    q.clearFinished()
    expect(q.items.map(i => `${i.key}:${i.status}`)).toEqual(['b:error'])
    q.remove(q.items[0]!.id)
    expect(q.items).toHaveLength(0)
  })

  it('clearSettled also dismisses failed items but keeps running ones', async () => {
    const f = fakeClient()
    const q = new UploadQueue()
    q.enqueue({ client: f.client, connectionId: 'c', bucket: 'b', files: ['a', 'b'].map(k => ({ file: file(k, 1), key: k })) })
    await tick()
    f.calls[0]!.reject(new Error('boom'))
    await tick()
    q.clearSettled()
    expect(q.items.map(i => `${i.key}:${i.status}`)).toEqual(['b:uploading'])
    f.calls[1]!.resolve()
    await tick()
  })

  it('formats ETAs', () => {
    expect(uploadEta(0, 100)).toBe('')
    expect(uploadEta(100, 0)).toBe('')
    expect(uploadEta(500, 100)).toBe('5s')
    expect(uploadEta(100 * 125, 100)).toBe('2m 5s')
    expect(uploadEta(100 * 3700, 100)).toBe('1h 1m')
  })
})
