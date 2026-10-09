// Object browser: bulk operations on objects, versions and folders (delete,
// copy / move / rename, restore a version, sequential downloads). Written
// against a minimal client interface so they can be unit-tested with fakes.
import type { CopyObjectOptions, CopyObjectResult, DeleteObjectsResult, ListResult, ListObjectsV2Params, ObjectIdentifier, ObjectRef, PresignOptions } from '~/types/s3'
import { attachmentDisposition, rebaseKey } from './useBrowserPaths'

/** The S3Client methods used here. */
export interface BrowserOpsClient {
  listObjectsV2(bucket: string, params?: ListObjectsV2Params, signal?: AbortSignal): Promise<ListResult>
  deleteObjects(bucket: string, objects: ObjectIdentifier[], opts?: { quiet?: boolean, signal?: AbortSignal }): Promise<DeleteObjectsResult>
  deletePrefix(bucket: string, prefix: string, opts?: { allVersions?: boolean, signal?: AbortSignal, onProgress?: (deleted: number) => void }): Promise<DeleteObjectsResult>
  copyObject(src: ObjectRef, dst: { bucket: string, key: string }, opts?: CopyObjectOptions): Promise<CopyObjectResult>
}

/** A selected row: an object (optionally one version) or a folder prefix. */
export interface OpTarget {
  key: string
  isFolder: boolean
  versionId?: string
}

export interface OpProgress {
  done: number
  /** Total when known (after enumerating folders), else undefined. */
  total?: number
  /** Key currently being processed. */
  current?: string
}

export interface OpFailure {
  key: string
  versionId?: string
  message: string
}

export interface OpResult {
  done: number
  failures: OpFailure[]
}

/** Lists every object key under a prefix (all pages). */
export async function listKeysUnder(client: Pick<BrowserOpsClient, 'listObjectsV2'>, bucket: string, prefix: string, signal?: AbortSignal): Promise<string[]> {
  const keys: string[] = []
  let token: string | undefined
  for (;;) {
    const page = await client.listObjectsV2(bucket, { prefix, continuationToken: token }, signal)
    for (const o of page.objects) keys.push(o.key)
    if (!page.isTruncated || !page.nextContinuationToken) return keys
    token = page.nextContinuationToken
  }
}

function msg(e: unknown): string {
  if (e instanceof Error) return e.message
  return typeof e === 'string' ? e : 'Unknown error'
}

/**
 * Deletes targets. Objects (and specific versions) go through DeleteObjects
 * in batches; folders through deletePrefix. With `allVersions`, folders are
 * purged including every version and delete marker.
 */
export async function deleteTargets(
  client: BrowserOpsClient,
  bucket: string,
  targets: OpTarget[],
  opts: { allVersions?: boolean, signal?: AbortSignal, onProgress?: (p: OpProgress) => void } = {}
): Promise<OpResult> {
  const result: OpResult = { done: 0, failures: [] }
  const objects: ObjectIdentifier[] = targets.filter(t => !t.isFolder).map(t => ({ key: t.key, versionId: t.versionId }))
  const folders = targets.filter(t => t.isFolder)
  if (objects.length) {
    const r = await client.deleteObjects(bucket, objects, { quiet: false, signal: opts.signal })
    result.done += r.deleted.length
    for (const e of r.errors) result.failures.push({ key: e.key, versionId: e.versionId, message: `${e.code}: ${e.message}` })
    opts.onProgress?.({ done: result.done })
  }
  for (const f of folders) {
    const before = result.done
    const r = await client.deletePrefix(bucket, f.key, {
      allVersions: opts.allVersions,
      signal: opts.signal,
      onProgress: n => opts.onProgress?.({ done: before + n, current: f.key })
    })
    result.done += r.deleted.length
    for (const e of r.errors) result.failures.push({ key: e.key, versionId: e.versionId, message: `${e.code}: ${e.message}` })
  }
  return result
}

export interface TransferPlanItem {
  src: ObjectRef
  dstKey: string
}

/**
 * Expands targets into individual copy operations. Folders are enumerated
 * recursively; keys keep their path relative to `fromPrefix`. A single
 * object can be given an explicit `renameTo` destination key.
 */
export async function planTransfer(
  client: Pick<BrowserOpsClient, 'listObjectsV2'>,
  srcBucket: string,
  targets: OpTarget[],
  dst: { fromPrefix: string, toPrefix: string, renameTo?: string },
  signal?: AbortSignal
): Promise<TransferPlanItem[]> {
  const plan: TransferPlanItem[] = []
  if (dst.renameTo !== undefined && targets.length === 1) {
    const t = targets[0]!
    if (!t.isFolder) return [{ src: { bucket: srcBucket, key: t.key, versionId: t.versionId }, dstKey: dst.renameTo }]
    // Renaming a folder: everything under it moves to the new folder prefix.
    const newPrefix = dst.renameTo.endsWith('/') ? dst.renameTo : `${dst.renameTo}/`
    for (const k of await listKeysUnder(client, srcBucket, t.key, signal)) {
      plan.push({ src: { bucket: srcBucket, key: k }, dstKey: rebaseKey(k, t.key, newPrefix) })
    }
    return plan
  }
  for (const t of targets) {
    if (t.isFolder) {
      for (const k of await listKeysUnder(client, srcBucket, t.key, signal)) {
        plan.push({ src: { bucket: srcBucket, key: k }, dstKey: rebaseKey(k, dst.fromPrefix, dst.toPrefix) })
      }
    } else {
      plan.push({ src: { bucket: srcBucket, key: t.key, versionId: t.versionId }, dstKey: rebaseKey(t.key, dst.fromPrefix, dst.toPrefix) })
    }
  }
  return plan
}

/**
 * Executes a transfer plan with CopyObject (sequentially, so progress is
 * exact and the server is not flooded). With `move`, successfully copied
 * sources are deleted afterwards in batches. Copies onto themselves are
 * skipped.
 */
export async function runTransfer(
  client: BrowserOpsClient,
  plan: TransferPlanItem[],
  dstBucket: string,
  opts: { move?: boolean, signal?: AbortSignal, onProgress?: (p: OpProgress) => void } = {}
): Promise<OpResult> {
  const result: OpResult = { done: 0, failures: [] }
  const copied: ObjectIdentifier[] = []
  const total = plan.length
  for (const item of plan) {
    if (opts.signal?.aborted) break
    opts.onProgress?.({ done: result.done, total, current: item.src.key })
    if (item.src.bucket === dstBucket && item.src.key === item.dstKey && !item.src.versionId) {
      result.failures.push({ key: item.src.key, message: 'Source and destination are the same' })
      continue
    }
    try {
      await client.copyObject(item.src, { bucket: dstBucket, key: item.dstKey }, { signal: opts.signal })
      result.done++
      copied.push({ key: item.src.key, versionId: item.src.versionId })
    } catch (e) {
      if (opts.signal?.aborted) break
      result.failures.push({ key: item.src.key, versionId: item.src.versionId, message: msg(e) })
    }
  }
  opts.onProgress?.({ done: result.done, total })
  if (opts.move && copied.length) {
    const srcBucket = plan[0]!.src.bucket
    const r = await client.deleteObjects(srcBucket, copied, { quiet: false })
    for (const e of r.errors) result.failures.push({ key: e.key, versionId: e.versionId, message: `copied, but the source could not be deleted: ${e.code}: ${e.message}` })
  }
  return result
}

/** Emitted by BrowserTransferModal when a transfer finished (also partially). */
export interface TransferDone {
  mode: 'copy' | 'move' | 'rename'
  dstBucket: string
  plan: TransferPlanItem[]
  result: OpResult
}

/** Restores an old version by copying it over the current one (creates a new latest version). */
export function restoreVersion(client: Pick<BrowserOpsClient, 'copyObject'>, bucket: string, key: string, versionId: string): Promise<CopyObjectResult> {
  return client.copyObject({ bucket, key, versionId }, { bucket, key })
}

export interface Presigner {
  presignGetUrl(bucket: string, key: string, opts?: PresignOptions): Promise<string>
}

/** Presigned download URL that makes browsers save the file under its base name. */
export function browserDownloadUrl(client: Presigner, bucket: string, key: string, versionId?: string, expiresSeconds = 900): Promise<string> {
  const name = key.endsWith('/') ? key.slice(0, -1) : key
  const filename = name.slice(name.lastIndexOf('/') + 1) || 'download'
  return client.presignGetUrl(bucket, key, { versionId, expiresSeconds, responseContentDisposition: attachmentDisposition(filename) })
}

/** Starts a browser download for a URL via a temporary <a download>. */
export function browserTriggerDownload(url: string): void {
  const a = document.createElement('a')
  a.href = url
  a.rel = 'noopener'
  a.download = ''
  a.style.display = 'none'
  document.body.appendChild(a)
  a.click()
  a.remove()
}

/**
 * Downloads several objects one after another (presigned GETs). A short pause
 * between files keeps browsers from collapsing the downloads.
 */
export async function downloadSequentially(
  client: Presigner,
  bucket: string,
  items: { key: string, versionId?: string }[],
  opts: { delayMs?: number, signal?: AbortSignal, onProgress?: (p: OpProgress) => void, trigger?: (url: string) => void } = {}
): Promise<OpResult> {
  const result: OpResult = { done: 0, failures: [] }
  const trigger = opts.trigger ?? browserTriggerDownload
  const delay = opts.delayMs ?? 600
  for (const [i, it] of items.entries()) {
    if (opts.signal?.aborted) break
    opts.onProgress?.({ done: i, total: items.length, current: it.key })
    try {
      trigger(await browserDownloadUrl(client, bucket, it.key, it.versionId))
      result.done++
    } catch (e) {
      result.failures.push({ key: it.key, versionId: it.versionId, message: msg(e) })
    }
    if (i < items.length - 1 && delay > 0) await new Promise(r => setTimeout(r, delay))
  }
  opts.onProgress?.({ done: result.done, total: items.length })
  return result
}
