// Object browser: routing, listing-model and naming helpers (auto-imported by
// Nuxt; also imported explicitly by unit tests, so everything here imports
// its own dependencies and stays framework-free).
//
// Browser URLs:
//   folder  /servers/:id/buckets/:bucket/objects/<prefix segments>/   (trailing '/')
//   object  /servers/:id/buckets/:bucket/objects/<key segments>       (no trailing '/')
// Each key segment is URL-encoded on its own, so keys may contain any
// character; '' (bucket root) is `/objects/`.
//
// Keys and prefixes with '.' / '..' segments ('x/../y') cannot live in the
// path: browsers (and history.pushState) resolve dot segments, even
// percent-encoded ones. They are carried in the query of the root route:
//   folder  …/objects/?prefix=<prefix>
//   object  …/objects/?key=<key>[&versionId=…]
import type { CommonPrefix, ObjectVersion, S3Object } from '~/types/s3'
import { baseName, parentPrefix, prefixBreadcrumbs } from '~/utils/paths'
import { appRoutes } from '~/utils/routes'
import { hasDotSegment } from '~/utils/s3/url'

const seg = (s: string) => encodeURIComponent(s)

/** Encodes a key or prefix segment by segment (keeps '/' separators and a trailing '/'). */
export function encodeKeyPath(key: string): string {
  return key.split('/').map(seg).join('/')
}

/** Folder route of the object browser ('' = bucket root). */
export function browserRoute(id: string, bucket: string, prefix = ''): string {
  const p = prefix && !prefix.endsWith('/') ? `${prefix}/` : prefix
  const base = `/servers/${seg(id)}/buckets/${seg(bucket)}/objects/`
  if (hasDotSegment(p)) return `${base}?prefix=${encodeURIComponent(p)}`
  return `${base}${encodeKeyPath(p)}`
}

/** Object route (opens the details panel); see appRoutes.object. */
export function browserObjectRoute(id: string, bucket: string, key: string, versionId?: string): string {
  return appRoutes.object(id, bucket, key, versionId)
}

export type BrowserTarget =
  | { kind: 'folder', prefix: string }
  | { kind: 'object', key: string, prefix: string }

function safeDecode(s: string): string {
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

/**
 * Parses the (encoded) route path of the objects page into the folder prefix
 * or object key it addresses. Trailing slash ⇒ folder; otherwise object.
 *
 *   '/servers/x/buckets/b/objects'            → folder ''
 *   '/servers/x/buckets/b/objects/a/b%20c/'   → folder 'a/b c/'
 *   '/servers/x/buckets/b/objects/a/f.txt'    → object 'a/f.txt' in 'a/'
 *   '/servers/x/buckets/b/objects/' + {key: 'x/../y'} → object 'x/../y' in 'x/../'
 */
export function parseBrowserPath(routePath: string, query: Record<string, unknown> = {}): BrowserTarget {
  const m = /^\/servers\/[^/]+\/buckets\/[^/]+\/objects(?:\/(.*))?$/.exec(routePath)
  const rest = m?.[1] ?? ''
  if (rest === '') {
    // Root route: keys / prefixes that cannot be put into the path.
    const q = (name: string) => {
      const v = Array.isArray(query[name]) ? (query[name] as unknown[])[0] : query[name]
      return typeof v === 'string' ? v : ''
    }
    const key = q('key')
    if (key) return { kind: 'object', key, prefix: parentPrefix(key) }
    const prefix = q('prefix')
    if (prefix) return { kind: 'folder', prefix: prefix.endsWith('/') ? prefix : `${prefix}/` }
    return { kind: 'folder', prefix: '' }
  }
  const decoded = rest.split('/').map(safeDecode).join('/')
  if (rest.endsWith('/')) return { kind: 'folder', prefix: decoded }
  return { kind: 'object', key: decoded, prefix: parentPrefix(decoded) }
}

/** Breadcrumb trail (bucket root + folders) for a prefix; `to` links are folder routes. */
export function browserCrumbs(id: string, bucket: string, prefix: string): { label: string, prefix: string, to: string }[] {
  return [
    { label: bucket, prefix: '', to: browserRoute(id, bucket, '') },
    ...prefixBreadcrumbs(prefix).map(c => ({ label: c.label, prefix: c.prefix, to: browserRoute(id, bucket, c.prefix) }))
  ]
}

// ---------------------------------------------------------------------------
// Listing model
// ---------------------------------------------------------------------------

export interface BrowserFolderEntry {
  type: 'folder'
  /** Stable id (the prefix). */
  id: string
  key: string
  name: string
}

export interface BrowserObjectEntry {
  type: 'object'
  id: string
  key: string
  name: string
  size: number
  lastModified?: string
  storageClass?: string
  etag?: string
  /** Set in versions mode. */
  versionId?: string
  isLatest?: boolean
  isDeleteMarker?: boolean
}

export type BrowserEntry = BrowserFolderEntry | BrowserObjectEntry

/**
 * Version to address when opening a listed object: none for the latest
 * version (the plain object URL), but always the delete marker's own version
 * ID — the plain key of a deleted object answers 404, which would show
 * "Object not found" instead of the delete marker.
 */
export function entryVersionParam(e: Pick<BrowserObjectEntry, 'versionId' | 'isLatest' | 'isDeleteMarker'>): string | undefined {
  if (!e.versionId) return undefined
  return !e.isLatest || e.isDeleteMarker ? e.versionId : undefined
}

/** Separator between key and version id in selection ids (cannot occur in keys we create). */
export const VERSION_ID_SEP = '\u0000'

/** Selection / row id of an entry: key, or key + NUL + versionId in versions mode. */
export function entryId(key: string, versionId?: string): string {
  return versionId ? `${key}${VERSION_ID_SEP}${versionId}` : key
}

/** Display name of a key/prefix relative to the listed prefix ('a/b/c.txt' in 'a/' → 'b/c.txt'). */
export function relativeName(keyOrPrefix: string, prefix: string): string {
  const rel = prefix && keyOrPrefix.startsWith(prefix) ? keyOrPrefix.slice(prefix.length) : keyOrPrefix
  const trimmed = rel.endsWith('/') ? rel.slice(0, -1) : rel
  // Folders like 'a//' would be empty: show the raw remainder instead.
  return trimmed || rel || baseName(keyOrPrefix)
}

/**
 * Builds folder + object entries from a ListObjectsV2 page. The folder
 * placeholder object of the listed prefix itself ('a/' when listing 'a/') is
 * hidden.
 */
export function toBrowserEntries(prefixes: CommonPrefix[], objects: S3Object[], prefix: string): BrowserEntry[] {
  const out: BrowserEntry[] = []
  for (const p of prefixes) out.push({ type: 'folder', id: p.prefix, key: p.prefix, name: relativeName(p.prefix, prefix) })
  for (const o of objects) {
    if (o.key === prefix) continue
    out.push({
      type: 'object',
      id: o.key,
      key: o.key,
      name: relativeName(o.key, prefix),
      size: o.size,
      lastModified: o.lastModified,
      storageClass: o.storageClass,
      etag: o.etag
    })
  }
  return out
}

/** Builds entries from a ListObjectVersions page (one row per version / delete marker). */
export function toBrowserVersionEntries(prefixes: CommonPrefix[], versions: ObjectVersion[], prefix: string): BrowserEntry[] {
  const out: BrowserEntry[] = []
  for (const p of prefixes) out.push({ type: 'folder', id: p.prefix, key: p.prefix, name: relativeName(p.prefix, prefix) })
  for (const v of versions) {
    if (v.key === prefix) continue
    out.push({
      type: 'object',
      id: entryId(v.key, v.versionId),
      key: v.key,
      name: relativeName(v.key, prefix),
      size: v.size,
      lastModified: v.lastModified,
      storageClass: v.storageClass,
      etag: v.etag,
      versionId: v.versionId,
      isLatest: v.isLatest,
      isDeleteMarker: v.isDeleteMarker
    })
  }
  return out
}

/** Appends a page to an existing list, skipping ids already present (folders can repeat across pages). */
export function mergeBrowserEntries(existing: BrowserEntry[], page: BrowserEntry[]): BrowserEntry[] {
  const seen = new Set(existing.map(e => e.id))
  const out = existing.slice()
  for (const e of page) {
    if (seen.has(e.id)) continue
    seen.add(e.id)
    out.push(e)
  }
  return out
}

/** Row actions emitted by the object table. */
export type BrowserRowAction = 'open' | 'details' | 'download' | 'share' | 'copy' | 'move' | 'rename' | 'delete' | 'restore'

export type BrowserSortKey = 'name' | 'size' | 'modified'
export interface BrowserSort {
  by: BrowserSortKey
  desc: boolean
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

/**
 * Sorts entries: folders always first (by name), then objects by the chosen
 * column with name (then version recency) as tie-breaker. Returns a new array.
 */
export function sortBrowserEntries(entries: BrowserEntry[], sort: BrowserSort): BrowserEntry[] {
  const dir = sort.desc ? -1 : 1
  const time = (e: BrowserObjectEntry) => (e.lastModified ? Date.parse(e.lastModified) || 0 : 0)
  return entries.slice().sort((a, b) => {
    if (a.type !== b.type) return a.type === 'folder' ? -1 : 1
    if (a.type === 'folder' || b.type === 'folder') {
      return collator.compare(a.name, b.name) * (sort.by === 'name' ? dir : 1)
    }
    let c = 0
    if (sort.by === 'size') c = a.size - b.size
    else if (sort.by === 'modified') c = time(a) - time(b)
    if (c === 0) c = collator.compare(a.name, b.name)
    if (c === 0) {
      // Versions of the same key: newest first regardless of direction.
      return time(b) - time(a)
    }
    return c * dir
  })
}

/** Case-insensitive substring filter on the display name. */
export function filterBrowserEntries(entries: BrowserEntry[], query: string): BrowserEntry[] {
  const q = query.trim().toLowerCase()
  if (!q) return entries
  return entries.filter(e => e.name.toLowerCase().includes(q))
}

// ---------------------------------------------------------------------------
// Keys for new objects
// ---------------------------------------------------------------------------

/**
 * Normalises a relative upload path ('./a//b.txt' → 'a/b.txt'). Backslashes
 * are kept: they are legal in file names on Linux and macOS, and browsers
 * already give relative paths (webkitRelativePath, dropped folders) with '/'
 * separators, also on Windows.
 */
export function normalizeRelativePath(p: string): string {
  return p
    .split('/')
    .filter(s => s !== '' && s !== '.')
    .join('/')
}

/** Destination key of an uploaded file: listed prefix + relative path. */
export function uploadKey(prefix: string, relativePath: string): string {
  return `${prefix}${normalizeRelativePath(relativePath)}`
}

/**
 * Validates a new folder name (may contain '/' for nested folders). Names
 * are not trimmed: spaces are part of S3 keys.
 */
export function validateFolderName(name: string): string | undefined {
  const n = name
  if (!n.trim()) return 'Folder name must not be empty'
  const parts = n.replace(/\/+$/, '').split('/')
  if (parts.some(p => p === '')) return 'Folder names must not contain empty segments ("//") or start with "/"'
  if (parts.some(p => p === '.' || p === '..')) return 'Folder names must not contain "." or ".." segments'
  return undefined
}

/** Folder key for a new folder under prefix ('a/' + 'b' → 'a/b/'). */
export function folderKey(prefix: string, name: string): string {
  return `${prefix}${name.replace(/\/+$/, '')}/`
}

/**
 * Destination key when copying/moving `key` (located under `fromPrefix`) to
 * `toPrefix`: the part relative to fromPrefix is kept.
 *   ('a/b/c.txt', 'a/', 'x/') → 'x/b/c.txt'
 */
export function rebaseKey(key: string, fromPrefix: string, toPrefix: string): string {
  const rel = fromPrefix && key.startsWith(fromPrefix) ? key.slice(fromPrefix.length) : key
  return `${toPrefix}${rel}`
}

/** Content-Disposition for downloads ("attachment" with an RFC 5987 UTF-8 filename). */
export function attachmentDisposition(filename: string): string {
  const ascii = filename.replace(/[^\x20-\x7e]/g, '_').replace(/["\\]/g, '_')
  const encoded = encodeURIComponent(filename).replace(/['()*]/g, c => `%${c.charCodeAt(0).toString(16).toUpperCase()}`)
  return `attachment; filename="${ascii}"; filename*=UTF-8''${encoded}`
}

const MIME: Record<string, string> = {
  txt: 'text/plain',
  log: 'text/plain',
  md: 'text/markdown',
  markdown: 'text/markdown',
  csv: 'text/csv',
  html: 'text/html',
  htm: 'text/html',
  css: 'text/css',
  js: 'text/javascript',
  mjs: 'text/javascript',
  json: 'application/json',
  xml: 'application/xml',
  yaml: 'application/yaml',
  yml: 'application/yaml',
  pdf: 'application/pdf',
  zip: 'application/zip',
  gz: 'application/gzip',
  tar: 'application/x-tar',
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  gif: 'image/gif',
  webp: 'image/webp',
  avif: 'image/avif',
  svg: 'image/svg+xml',
  ico: 'image/x-icon',
  mp4: 'video/mp4',
  webm: 'video/webm',
  mov: 'video/quicktime',
  mp3: 'audio/mpeg',
  wav: 'audio/wav',
  ogg: 'audio/ogg',
  wasm: 'application/wasm'
}

/** Content type for an upload: the browser's type, else a guess from the extension. */
export function guessContentType(name: string, browserType?: string): string | undefined {
  if (browserType) return browserType
  const b = baseName(name)
  const i = b.lastIndexOf('.')
  if (i <= 0) return undefined
  return MIME[b.slice(i + 1).toLowerCase()]
}
