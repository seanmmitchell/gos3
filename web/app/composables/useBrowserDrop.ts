// Object browser: turning drag & drop payloads and <input type="file"> picks
// (including whole folders) into a flat list of files with relative paths.
//
// Drag & drop of folders uses the (widely supported) non-standard
// DataTransferItem.webkitGetAsEntry() API and walks directories recursively.
// The structural interfaces below match the DOM FileSystem*Entry types and
// let tests pass plain fakes.
import { normalizeRelativePath } from './useBrowserPaths'

export interface DroppedFile {
  file: File
  /** Path relative to the drop target, '/'-separated (e.g. 'photos/2024/a.jpg'). */
  relativePath: string
}

export interface FsEntryLike {
  isFile: boolean
  isDirectory: boolean
  name: string
  /** Absolute path within the dropped set, e.g. '/photos/a.jpg'. */
  fullPath?: string
}

export interface FsFileEntryLike extends FsEntryLike {
  file(success: (f: File) => void, error?: (e: unknown) => void): void
}

export interface FsDirectoryReaderLike {
  readEntries(success: (entries: FsEntryLike[]) => void, error?: (e: unknown) => void): void
}

export interface FsDirectoryEntryLike extends FsEntryLike {
  createReader(): FsDirectoryReaderLike
}

export interface DataTransferItemLike {
  kind: string
  getAsFile(): File | null
  webkitGetAsEntry?: () => FsEntryLike | null
}

export interface DataTransferLike {
  items?: ArrayLike<DataTransferItemLike> | null
  files?: ArrayLike<File> | null
}

export interface CollectOptions {
  /** Stop after this many files (protects against dropping a whole disk). Default 100 000. */
  maxFiles?: number
}

/** Result of flattening: files plus non-fatal problems (unreadable entries, limit reached). */
export interface CollectResult {
  files: DroppedFile[]
  errors: string[]
  truncated: boolean
}

function fileOf(entry: FsFileEntryLike): Promise<File> {
  return new Promise((resolve, reject) => entry.file(resolve, reject))
}

/** Reads all children of a directory (readEntries returns batches until an empty one). */
export async function readAllEntries(dir: FsDirectoryEntryLike): Promise<FsEntryLike[]> {
  const reader = dir.createReader()
  const out: FsEntryLike[] = []
  for (;;) {
    const batch = await new Promise<FsEntryLike[]>((resolve, reject) => reader.readEntries(resolve, reject))
    if (!batch.length) return out
    out.push(...batch)
  }
}

/** Recursively flattens file-system entries into files with relative paths. */
export async function flattenEntries(entries: FsEntryLike[], opts: CollectOptions = {}): Promise<CollectResult> {
  const max = opts.maxFiles ?? 100_000
  const result: CollectResult = { files: [], errors: [], truncated: false }

  async function walk(entry: FsEntryLike, parent: string): Promise<void> {
    if (result.files.length >= max) {
      result.truncated = true
      return
    }
    const path = parent ? `${parent}/${entry.name}` : entry.name
    if (entry.isFile) {
      try {
        const file = await fileOf(entry as FsFileEntryLike)
        result.files.push({ file, relativePath: normalizeRelativePath(path) })
      } catch (e) {
        result.errors.push(`${path}: ${e instanceof Error ? e.message : 'could not be read'}`)
      }
    } else if (entry.isDirectory) {
      let children: FsEntryLike[]
      try {
        children = await readAllEntries(entry as FsDirectoryEntryLike)
      } catch (e) {
        result.errors.push(`${path}/: ${e instanceof Error ? e.message : 'could not be read'}`)
        return
      }
      for (const c of children) await walk(c, path)
    }
  }

  for (const e of entries) await walk(e, '')
  return result
}

/**
 * Collects files from a drop event's DataTransfer, descending into dropped
 * folders. Must be called synchronously inside the drop handler (the items
 * list is only valid during the event); the returned promise may be awaited.
 */
export function collectDroppedFiles(dt: DataTransferLike | null | undefined, opts: CollectOptions = {}): Promise<CollectResult> {
  if (!dt) return Promise.resolve({ files: [], errors: [], truncated: false })
  const items = dt.items ? Array.from(dt.items) : []
  const entries: FsEntryLike[] = []
  const plain: File[] = []
  let entryApi = items.length > 0
  // Grab every entry synchronously before the first await.
  for (const it of items) {
    if (it.kind !== 'file') continue
    const entry = typeof it.webkitGetAsEntry === 'function' ? it.webkitGetAsEntry() : null
    if (entry) {
      entries.push(entry)
    } else {
      const f = it.getAsFile()
      if (f) plain.push(f)
      else entryApi = false
    }
  }
  if (!entryApi || (!entries.length && !plain.length)) {
    const files = dt.files ? Array.from(dt.files) : []
    return Promise.resolve({ files: files.map(f => ({ file: f, relativePath: normalizeRelativePath(f.name) })), errors: [], truncated: false })
  }
  return flattenEntries(entries, opts).then((r) => {
    for (const f of plain) r.files.push({ file: f, relativePath: normalizeRelativePath(f.name) })
    return r
  })
}

/**
 * Files picked with <input type="file"> (multiple, or webkitdirectory for a
 * folder: webkitRelativePath keeps the folder structure).
 */
export function filesFromInput(list: ArrayLike<File> | null | undefined): DroppedFile[] {
  if (!list) return []
  return Array.from(list).map((f) => {
    const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath
    return { file: f, relativePath: normalizeRelativePath(rel || f.name) }
  })
}

/** True when a drag event carries files (not text/links dragged from the page). */
export function dragHasFiles(dt: { types?: ArrayLike<string> | null } | null | undefined): boolean {
  if (!dt?.types) return false
  return Array.from(dt.types).includes('Files')
}
