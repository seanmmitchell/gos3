// Object browser: bulk operations on objects, versions and folders (delete,
// copy / move / rename, restore a version, sequential downloads). Written
// against a minimal client interface so they can be unit-tested with fakes.
import type { AccessControlPolicy, CannedACL, CopyObjectOptions, CopyObjectResult, DeleteObjectsResult, HeadResult, ListResult, ListObjectsV2Params, ObjectIdentifier, ObjectRef, PresignOptions } from '~/types/s3'
import { attachmentDisposition, rebaseKey } from './useBrowserPaths'

/** The S3Client methods used here. */
export interface BrowserOpsClient {
  listObjectsV2(bucket: string, params?: ListObjectsV2Params, signal?: AbortSignal): Promise<ListResult>
  headObject(bucket: string, key: string, opts?: { versionId?: string, checksumMode?: boolean, signal?: AbortSignal }): Promise<HeadResult>
  deleteObject(bucket: string, key: string, versionId?: string, opts?: { ifMatch?: string, signal?: AbortSignal }): Promise<unknown>
  deleteObjects(bucket: string, objects: ObjectIdentifier[], opts?: { quiet?: boolean, signal?: AbortSignal }): Promise<DeleteObjectsResult>
  deletePrefix(bucket: string, prefix: string, opts?: { allVersions?: boolean, signal?: AbortSignal, onProgress?: (deleted: number) => void }): Promise<DeleteObjectsResult>
  copyObject(src: ObjectRef, dst: { bucket: string, key: string }, opts?: CopyObjectOptions): Promise<CopyObjectResult>
  /** Used to keep the ACL of moved / renamed objects (optional for fakes). */
  getObjectAcl?(bucket: string, key: string, versionId?: string): Promise<AccessControlPolicy>
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
  /** Completed with caveats (e.g. an ACL that could not be carried over). */
  warnings?: OpFailure[]
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
 * First plan item that would overwrite the source of another item before
 * that item is copied (destination inside the source, e.g. folder 'x/'
 * renamed to 'x/y/': 'x/f' → 'x/y/f' replaces 'x/y/f' before it is read).
 */
export function findTransferOverlap(plan: TransferPlanItem[], dstBucket: string): TransferPlanItem | undefined {
  const sources = new Set(plan.filter(p => p.src.bucket === dstBucket).map(p => p.src.key))
  return plan.find(p => sources.has(p.dstKey) && !(p.src.bucket === dstBucket && p.src.key === p.dstKey))
}

/** The S3Client methods used to look for existing destination objects. */
export interface DestinationLookupClient {
  listObjectsV2(bucket: string, params?: ListObjectsV2Params, signal?: AbortSignal): Promise<ListResult>
  headObject(bucket: string, key: string, opts?: { versionId?: string, checksumMode?: boolean, signal?: AbortSignal }): Promise<HeadResult>
}

/** Up to this many keys are checked with one HeadObject each. */
const HEAD_CHECK_LIMIT = 20
/** After listing, at most this many remaining keys are checked with HeadObject. */
const HEAD_FALLBACK_LIMIT = 500
/** Parallel HeadObject requests. */
const HEAD_CONCURRENCY = 6

function commonPrefix(keys: string[]): string {
  if (!keys.length) return ''
  let p = keys[0]!
  for (const k of keys) {
    let i = 0
    while (i < p.length && i < k.length && p[i] === k[i]) i++
    p = p.slice(0, i)
    if (!p) break
  }
  return p
}

/** Compares keys in S3 listing order (UTF-8 bytes = code points, not UTF-16 units). */
export function compareKeys(a: string, b: string): number {
  const x = [...a]
  const y = [...b]
  for (let i = 0; i < x.length && i < y.length; i++) {
    const d = x[i]!.codePointAt(0)! - y[i]!.codePointAt(0)!
    if (d) return d
  }
  return x.length - y.length
}

/**
 * Checks keys with HeadObject (HEAD_CONCURRENCY at a time). 404 means
 * missing; any other error makes the result incomplete (e.g. 403, which S3
 * also returns for missing keys without s3:ListBucket).
 */
async function headKeys(client: DestinationLookupClient, bucket: string, keys: string[], signal?: AbortSignal): Promise<{ existing: Set<string>, complete: boolean }> {
  const existing = new Set<string>()
  let complete = true
  let next = 0
  const worker = async () => {
    while (next < keys.length) {
      const key = keys[next++]!
      try {
        await client.headObject(bucket, key, { checksumMode: false, signal })
        existing.add(key)
      } catch (e) {
        if (signal?.aborted) throw e
        if ((e as { status?: number })?.status !== 404) complete = false
      }
    }
  }
  await Promise.all(Array.from({ length: Math.min(HEAD_CONCURRENCY, keys.length) }, worker))
  return { existing, complete }
}

/**
 * Keys of `keys` that already exist in a bucket (best effort: objects that
 * appear meanwhile are not reported). Few keys are checked with HeadObject;
 * more are found by listing their common prefix (up to `maxListed` keys),
 * then the keys the listing did not reach are checked with HeadObject (up
 * to HEAD_FALLBACK_LIMIT). `complete` is false when some keys could not be
 * checked: callers must then assume they may exist.
 */
export async function findExistingKeys(
  client: DestinationLookupClient,
  bucket: string,
  keys: string[],
  opts: { signal?: AbortSignal, maxListed?: number } = {}
): Promise<{ existing: string[], complete: boolean }> {
  const wanted = [...new Set(keys)]
  const ordered = (found: Set<string>) => wanted.filter(k => found.has(k))
  if (wanted.length <= HEAD_CHECK_LIMIT) {
    const r = await headKeys(client, bucket, wanted, opts.signal)
    return { existing: ordered(r.existing), complete: r.complete }
  }
  const set = new Set(wanted)
  const found = new Set<string>()
  const maxListed = opts.maxListed ?? Math.max(10000, wanted.length * 10)
  let listed = 0
  let token: string | undefined
  let last: string | undefined
  for (;;) {
    const page = await client.listObjectsV2(bucket, { prefix: commonPrefix(wanted) || undefined, continuationToken: token }, opts.signal)
    for (const o of page.objects) {
      if (set.has(o.key)) found.add(o.key)
      last = o.key
    }
    listed += page.objects.length
    if (!page.isTruncated || !page.nextContinuationToken) return { existing: ordered(found), complete: true }
    if (listed >= maxListed) break
    token = page.nextContinuationToken
  }
  // The listing stopped early: keys after the last listed one are unknown.
  const rest = wanted.filter(k => last === undefined || compareKeys(k, last) > 0)
  if (rest.length > HEAD_FALLBACK_LIMIT) return { existing: ordered(found), complete: false }
  const r = await headKeys(client, bucket, rest, opts.signal)
  for (const k of r.existing) found.add(k)
  return { existing: ordered(found), complete: r.complete }
}

/** Destination keys of a plan that already exist (see findExistingKeys). */
export function findExistingDestinations(
  client: DestinationLookupClient,
  dstBucket: string,
  plan: TransferPlanItem[],
  opts: { signal?: AbortSignal, maxListed?: number } = {}
): Promise<{ existing: string[], complete: boolean }> {
  return findExistingKeys(client, dstBucket, plan.map(p => p.dstKey), opts)
}

/**
 * Canned ACL to pass on the copy so the object keeps its ACL (CopyObject
 * never copies it: the copy gets the default, private ACL). 'private' and
 * unreadable ACLs give undefined; grants no canned ACL expresses give
 * 'custom' (they cannot be carried over).
 */
export async function aclToKeep(client: Pick<BrowserOpsClient, 'getObjectAcl'>, src: ObjectRef): Promise<CannedACL | 'custom' | undefined> {
  if (!client.getObjectAcl) return undefined
  try {
    const acl = await client.getObjectAcl(src.bucket, src.key, src.versionId)
    return acl.canned === 'private' ? undefined : acl.canned
  } catch {
    return undefined // not supported / not allowed: keep the server default
  }
}

/**
 * Executes a transfer plan with CopyObject (sequentially, so progress is
 * exact and the server is not flooded). With `move`, each source is deleted
 * only if it still has the ETag observed before copying, and objects keep
 * their ACL (see aclToKeep). Copies onto themselves are skipped. Throws, before
 * copying anything, when an item would overwrite another item's source
 * (findTransferOverlap).
 */
export async function runTransfer(
  client: BrowserOpsClient,
  plan: TransferPlanItem[],
  dstBucket: string,
  opts: { move?: boolean, signal?: AbortSignal, onProgress?: (p: OpProgress) => void } = {}
): Promise<OpResult> {
  const overlap = findTransferOverlap(plan, dstBucket)
  if (overlap) {
    throw new Error(`“${overlap.dstKey}” is both copied and overwritten by this transfer (the destination is inside the source). Choose a destination outside the selected folders.`)
  }
  const result: OpResult = { done: 0, failures: [], warnings: [] }
  const total = plan.length
  for (const item of plan) {
    if (opts.signal?.aborted) break
    opts.onProgress?.({ done: result.done, total, current: item.src.key })
    if (item.src.bucket === dstBucket && item.src.key === item.dstKey && !item.src.versionId) {
      result.failures.push({ key: item.src.key, message: 'Source and destination are the same' })
      continue
    }
    let copied = false
    try {
      const etag = opts.move && !item.src.versionId
        ? (await client.headObject(item.src.bucket, item.src.key, { checksumMode: false, signal: opts.signal })).etag
        : undefined
      if (opts.move && !item.src.versionId && !etag) throw new Error('Cannot safely move an object without a readable source ETag')
      const acl = opts.move ? await aclToKeep(client, item.src) : undefined
      await client.copyObject(item.src, { bucket: dstBucket, key: item.dstKey }, { signal: opts.signal, acl: acl === 'custom' ? undefined : acl })
      copied = true
      if (opts.move) await client.deleteObject(item.src.bucket, item.src.key, item.src.versionId, { ifMatch: etag })
      if (acl === 'custom') result.warnings!.push({ key: item.src.key, versionId: item.src.versionId, message: 'moved, but its custom ACL grants were not carried over (the object is private now)' })
      result.done++
    } catch (e) {
      if (opts.signal?.aborted && !copied) break
      result.failures.push({ key: item.src.key, versionId: item.src.versionId, message: copied ? `copied, but the source could not be deleted: ${msg(e)}` : msg(e) })
    }
  }
  opts.onProgress?.({ done: result.done, total })
  return result
}

/** Emitted by BrowserTransferModal when a transfer finished (also partially). */
export interface TransferDone {
  mode: 'copy' | 'move' | 'rename'
  dstBucket: string
  plan: TransferPlanItem[]
  result: OpResult
}

/** Result of restoreVersion. */
export interface RestoreResult {
  copy: CopyObjectResult
  /** The version had custom ACL grants, which were not carried over (the new version is private). */
  aclReset: boolean
}

/**
 * Restores an old version by copying it over the current one (creates a
 * new latest version). The version's canned ACL is kept (see aclToKeep).
 */
export async function restoreVersion(client: Pick<BrowserOpsClient, 'copyObject' | 'getObjectAcl'>, bucket: string, key: string, versionId: string): Promise<RestoreResult> {
  const acl = await aclToKeep(client, { bucket, key, versionId })
  const copy = await client.copyObject({ bucket, key, versionId }, { bucket, key }, { acl: acl === 'custom' ? undefined : acl })
  return { copy, aclReset: acl === 'custom' }
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
