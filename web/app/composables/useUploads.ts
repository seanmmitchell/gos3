// Upload queue shared by the object browser (one per app, survives folder
// navigation): per-file progress and speed, cancel, retry, a concurrency
// limit (3 files at a time) and multipart for large files.
//
//   const uploads = useUploads()
//   uploads.enqueue({ client: requireS3(), connectionId, bucket, files: [{ file, key }] })
//   const off = uploads.onSettled(item => { if (item.status === 'done') refresh() })
//
// The queue itself (UploadQueue) is framework-light (Vue reactivity only) and
// takes any client with putObject/uploadLarge, so it is unit-testable.
// useUploads() binds the shared queue to the connections store
// (bindUploadsToConnections): a retry signs with the connection's current
// credentials, and signing out of / removing a server stops its uploads and
// drops the clients holding its secret key.
import { computed, markRaw, reactive, shallowReactive } from 'vue'
import type { CompleteMultipartUploadResult, PutObjectOptions, PutObjectResult, UploadLargeOptions } from '~/types/s3'
import { useConnectionsStore } from '~/stores/connections'
import { getS3Client } from '~/utils/clients'
import { hasCredentials } from '~/utils/connections'
import { isAbortedError, errorMessage } from '~/utils/errors'
import { guessContentType } from './useBrowserPaths'

/** Files above this size use multipart upload (S3Client.uploadLarge). */
export const UPLOAD_MULTIPART_THRESHOLD = 16 * 1024 * 1024
/** Files uploaded in parallel. */
export const UPLOAD_CONCURRENCY = 3

/** The part of S3Client the queue needs. */
export interface Uploader {
  putObject(bucket: string, key: string, body: Blob, opts?: PutObjectOptions): Promise<PutObjectResult>
  uploadLarge(bucket: string, key: string, file: Blob, opts?: UploadLargeOptions): Promise<CompleteMultipartUploadResult>
}

export type UploadStatus = 'queued' | 'uploading' | 'done' | 'error' | 'canceled'

export interface UploadItem {
  id: number
  connectionId: string
  bucket: string
  key: string
  file: File | Blob
  /** File name for display. */
  name: string
  size: number
  contentType?: string
  multipart: boolean
  status: UploadStatus
  /** Bytes sent. */
  loaded: number
  /** Smoothed bytes/second while uploading; average speed once done. */
  speed: number
  error?: string
  /** Raw error (S3Error …) for detailed display. */
  cause?: unknown
  startedAt?: number
  finishedAt?: number
  versionId?: string
  /** Number of attempts started (retries increment it). */
  attempts: number
}

export interface EnqueueFile {
  file: File | Blob
  key: string
  contentType?: string
}

export interface EnqueueRequest {
  client: Uploader
  connectionId: string
  bucket: string
  files: EnqueueFile[]
}

/**
 * Returns the client for a connection's current credentials, or undefined
 * when the connection is gone or signed out.
 */
export type ClientResolver = (connectionId: string) => Uploader | undefined

export interface UploadQueueOptions {
  concurrency?: number
  multipartThreshold?: number
  /** Clock (tests). */
  now?: () => number
  /** See UploadQueue.setClientResolver. */
  resolveClient?: ClientResolver
}

/** Error of a retry whose server has no credentials (any more). */
export const UPLOAD_NOT_CONNECTED = 'This server is not connected: sign in again to retry the upload.'
/** Error of uploads stopped because their server was signed out or removed. */
export const UPLOAD_CREDENTIALS_CLEARED = 'Upload stopped: the credentials for this server were cleared.'

type Listener = (item: UploadItem) => void

interface Runtime {
  /**
   * Client of the current/last attempt; dropped by forgetConnection so the
   * queue does not keep a forgotten secret key alive.
   */
  client?: Uploader
  /** Controller of the current attempt (identifies it: see start()). */
  controller?: AbortController
  lastSampleAt?: number
  lastSampleLoaded?: number
}

/** Upload queue with a concurrency limit; items are reactive. */
export class UploadQueue {
  readonly items: UploadItem[] = shallowReactive([])
  readonly concurrency: number
  readonly multipartThreshold: number
  private readonly now: () => number
  private readonly runtime = new Map<number, Runtime>()
  private readonly listeners = new Set<Listener>()
  private resolveClient?: ClientResolver
  private nextId = 1

  /** Aggregate numbers for the panel header. */
  readonly stats = computed(() => {
    let total = 0
    let loaded = 0
    let active = 0
    let queued = 0
    let done = 0
    let failed = 0
    let canceled = 0
    let speed = 0
    for (const it of this.items) {
      if (it.status === 'canceled') {
        canceled++
        continue
      }
      total += it.size
      loaded += it.status === 'done' ? it.size : it.loaded
      if (it.status === 'uploading') {
        active++
        speed += it.speed
      } else if (it.status === 'queued') queued++
      else if (it.status === 'done') done++
      else if (it.status === 'error') failed++
    }
    return { total, loaded, active, queued, done, failed, canceled, speed, pending: active + queued, count: this.items.length }
  })

  constructor(opts: UploadQueueOptions = {}) {
    this.concurrency = Math.max(1, opts.concurrency ?? UPLOAD_CONCURRENCY)
    this.multipartThreshold = opts.multipartThreshold ?? UPLOAD_MULTIPART_THRESHOLD
    this.now = opts.now ?? (() => Date.now())
    this.resolveClient = opts.resolveClient
  }

  /**
   * Sets how retry() finds a client. With a resolver a retry uses the
   * connection's current client (its credentials may have changed since the
   * file was queued) and fails when there is none; without one it reuses the
   * client the file was queued with.
   */
  setClientResolver(fn: ClientResolver | undefined): void {
    this.resolveClient = fn
  }

  /** Adds files to the queue and starts uploading (up to `concurrency` at once). */
  enqueue(req: EnqueueRequest): UploadItem[] {
    const added: UploadItem[] = []
    for (const f of req.files) {
      const name = (f.file as File).name || f.key.split('/').pop() || f.key
      const item = reactive<UploadItem>({
        id: this.nextId++,
        connectionId: req.connectionId,
        bucket: req.bucket,
        key: f.key,
        file: markRaw(f.file),
        name,
        size: f.file.size,
        contentType: f.contentType ?? guessContentType(name, f.file.type),
        multipart: f.file.size > this.multipartThreshold,
        status: 'queued',
        loaded: 0,
        speed: 0,
        attempts: 0
      }) as UploadItem
      this.runtime.set(item.id, { client: markRaw(req.client) })
      this.items.push(item)
      added.push(item)
    }
    this.pump()
    return added
  }

  get(id: number): UploadItem | undefined {
    return this.items.find(i => i.id === id)
  }

  /** Cancels a queued or running upload (multipart uploads are aborted server-side). */
  cancel(id: number): void {
    const item = this.get(id)
    if (!item || (item.status !== 'queued' && item.status !== 'uploading')) return
    const rt = this.runtime.get(id)
    const wasRunning = item.status === 'uploading'
    item.status = 'canceled'
    item.speed = 0
    item.finishedAt = this.now()
    rt?.controller?.abort()
    if (!wasRunning) this.emit(item)
    this.pump()
  }

  /** Cancels everything that has not finished. */
  cancelAll(): void {
    for (const it of this.items.slice()) this.cancel(it.id)
  }

  /** Re-queues a failed or canceled upload. */
  retry(id: number): void {
    const item = this.get(id)
    if (!item || (item.status !== 'error' && item.status !== 'canceled')) return
    const rt = this.runtime.get(id)
    if (!rt) return
    const client = this.resolveClient ? this.resolveClient(item.connectionId) : rt.client
    if (!client) {
      rt.client = undefined
      item.status = 'error'
      item.error = UPLOAD_NOT_CONNECTED
      item.cause = undefined
      item.speed = 0
      item.finishedAt = this.now()
      this.emit(item)
      return
    }
    rt.client = markRaw(client)
    item.status = 'queued'
    item.loaded = 0
    item.speed = 0
    item.error = undefined
    item.cause = undefined
    item.startedAt = undefined
    item.finishedAt = undefined
    this.pump()
  }

  /** Retries all failed uploads. */
  retryFailed(): void {
    for (const it of this.items) if (it.status === 'error') this.retry(it.id)
  }

  /**
   * Stops a connection's uploads after its credentials were cleared or it was
   * removed: running ones are aborted (an in-progress multipart upload is
   * aborted server-side, best-effort) and queued ones will not start; both
   * are marked failed, so they can be retried after signing in again. The
   * clients (which hold the secret key) of all its items are dropped.
   */
  forgetConnection(connectionId: string): void {
    for (const item of this.items) {
      if (item.connectionId !== connectionId) continue
      const rt = this.runtime.get(item.id)
      if (rt) rt.client = undefined
      if (item.status !== 'queued' && item.status !== 'uploading') continue
      // Detach the running attempt: its late settlement is ignored (see
      // start()); the item is settled here.
      const controller = rt?.controller
      if (rt) rt.controller = undefined
      controller?.abort()
      item.status = 'error'
      item.error = UPLOAD_CREDENTIALS_CLEARED
      item.cause = undefined
      item.speed = 0
      item.finishedAt = this.now()
      this.emit(item)
    }
    this.pump()
  }

  /** IDs of the connections that have items in the queue. */
  connectionIds(): Set<string> {
    return new Set(this.items.map(i => i.connectionId))
  }

  /** Removes one finished/failed/canceled item from the list. */
  remove(id: number): void {
    const i = this.items.findIndex(x => x.id === id)
    if (i < 0) return
    const it = this.items[i]!
    if (it.status === 'queued' || it.status === 'uploading') this.cancel(id)
    this.items.splice(i, 1)
    this.runtime.delete(id)
  }

  /** Removes all items that are done or canceled (failed ones stay for retry). */
  clearFinished(): void {
    for (const it of this.items.slice()) {
      if (it.status === 'done' || it.status === 'canceled') this.remove(it.id)
    }
  }

  /** Removes every item that is not queued or uploading (failed ones too). */
  clearSettled(): void {
    for (const it of this.items.slice()) {
      if (it.status !== 'queued' && it.status !== 'uploading') this.remove(it.id)
    }
  }

  /** Subscribes to items reaching a final state (done / error / canceled). */
  onSettled(fn: Listener): () => void {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  private emit(item: UploadItem): void {
    for (const fn of this.listeners) {
      try {
        fn(item)
      } catch {
        // Listener errors must not break the queue.
      }
    }
  }

  private pump(): void {
    let running = this.items.filter(i => i.status === 'uploading').length
    for (const item of this.items) {
      if (running >= this.concurrency) return
      if (item.status !== 'queued') continue
      running++
      void this.start(item)
    }
  }

  private async start(item: UploadItem): Promise<void> {
    const rt = this.runtime.get(item.id)
    if (!rt) return
    const client = rt.client
    if (!client) {
      // Not reachable through the public API (queued items have a client).
      item.status = 'error'
      item.error = UPLOAD_NOT_CONNECTED
      item.finishedAt = this.now()
      this.emit(item)
      return
    }
    const controller = new AbortController()
    rt.controller = controller
    rt.lastSampleAt = this.now()
    rt.lastSampleLoaded = 0
    item.status = 'uploading'
    item.attempts++
    item.loaded = 0
    item.speed = 0
    item.startedAt = rt.lastSampleAt

    const onProgress = ({ loaded }: { loaded: number }) => {
      if (controller.signal.aborted) return
      item.loaded = Math.min(loaded, item.size)
      const t = this.now()
      const dt = t - (rt.lastSampleAt ?? t)
      if (dt >= 250) {
        const instant = ((item.loaded - (rt.lastSampleLoaded ?? 0)) / dt) * 1000
        item.speed = item.speed ? item.speed * 0.7 + instant * 0.3 : instant
        rt.lastSampleAt = t
        rt.lastSampleLoaded = item.loaded
      }
    }
    const opts = { contentType: item.contentType, onProgress, signal: controller.signal }
    // A canceled attempt can settle after a retry started the next one (a
    // multipart upload is aborted server-side first): it must then leave the
    // item and the runtime to the new attempt.
    const current = () => rt.controller === controller

    try {
      const res = item.multipart
        ? await client.uploadLarge(item.bucket, item.key, item.file, opts)
        : await client.putObject(item.bucket, item.key, item.file, opts)
      if (controller.signal.aborted || !current()) return
      const t = this.now()
      item.status = 'done'
      item.loaded = item.size
      item.finishedAt = t
      item.versionId = res.versionId
      const secs = (t - (item.startedAt ?? t)) / 1000
      item.speed = secs > 0 ? item.size / secs : 0
      this.emit(item)
    } catch (e) {
      if (!current()) return
      if (controller.signal.aborted || isAbortedError(e)) {
        if (item.status === 'uploading') {
          item.status = 'canceled'
          item.finishedAt = this.now()
        }
        item.speed = 0
        this.emit(item)
        return
      }
      item.status = 'error'
      item.error = errorMessage(e)
      item.cause = markRaw(typeof e === 'object' && e !== null ? e : { message: String(e) })
      item.speed = 0
      item.finishedAt = this.now()
      this.emit(item)
    } finally {
      if (current()) rt.controller = undefined
      this.pump()
    }
  }
}

type ConnectionsStore = ReturnType<typeof useConnectionsStore>

/** Store actions after which a connection may have lost its credentials. */
const CREDENTIAL_ACTIONS = new Set(['clearCredentials', 'remove', 'update', 'setCredentials', 'syncFromStorage', 'importJson', 'resetState'])

/**
 * Binds a queue to the connections store: retries resolve the connection's
 * current S3 client, and after a store action that can drop credentials
 * (sign-out, removal, a change merged from another tab …) the uploads of
 * every connection left without credentials are stopped (forgetConnection).
 * Returns a function that undoes the binding.
 */
export function bindUploadsToConnections(queue: UploadQueue, store: ConnectionsStore): () => void {
  queue.setClientResolver((id) => {
    const c = store.byId(id)
    return c && hasCredentials(c) ? getS3Client(c) : undefined
  })
  const off = store.$onAction(({ name, after }) => {
    if (!CREDENTIAL_ACTIONS.has(name)) return
    after(() => {
      for (const id of queue.connectionIds()) {
        if (!hasCredentials(store.byId(id))) queue.forgetConnection(id)
      }
    })
  }, true) // detached: the queue outlives the component that created it
  return () => {
    off()
    queue.setClientResolver(undefined)
  }
}

let shared: UploadQueue | undefined

/** The app-wide upload queue (bound to the connections store). */
export function useUploads(): UploadQueue {
  if (!shared) {
    shared = new UploadQueue()
    bindUploadsToConnections(shared, useConnectionsStore())
  }
  return shared
}

/** Formats an ETA in seconds for a remaining byte count at a given speed ('' when unknown). */
export function uploadEta(remainingBytes: number, bytesPerSecond: number): string {
  if (!(bytesPerSecond > 0) || remainingBytes <= 0) return ''
  const s = Math.ceil(remainingBytes / bytesPerSecond)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}
