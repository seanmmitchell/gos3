// Object key / prefix helpers (auto-imported by Nuxt from app/utils/).
// Keys are '/'-delimited; a "folder" prefix always ends with '/'.

export interface PrefixCrumb {
  /** Display label (segment name). */
  label: string
  /** Full prefix up to and including this segment (ends with '/'). */
  prefix: string
}

/** Ensures a non-empty prefix ends with '/'. '' stays ''. */
export function asFolderPrefix(prefix: string): string {
  if (!prefix) return ''
  return prefix.endsWith('/') ? prefix : `${prefix}/`
}

/** 'a/b/c/' → [{label:'a',prefix:'a/'},{label:'b',prefix:'a/b/'},{label:'c',prefix:'a/b/c/'}] */
export function prefixBreadcrumbs(prefix: string): PrefixCrumb[] {
  const out: PrefixCrumb[] = []
  if (!prefix) return out
  const parts = prefix.split('/')
  // A trailing '' comes from the final '/'.
  if (parts[parts.length - 1] === '') parts.pop()
  let acc = ''
  for (const p of parts) {
    acc += `${p}/`
    out.push({ label: p, prefix: acc })
  }
  return out
}

/** Joins key segments with single '/' (keeps a trailing '/' of the last segment). */
export function joinKey(...parts: (string | undefined | null)[]): string {
  const segs = parts.filter((p): p is string => !!p)
  if (!segs.length) return ''
  const trailing = segs[segs.length - 1]!.endsWith('/')
  const joined = segs
    .map((s, i) => {
      let v = s
      if (i > 0) v = v.replace(/^\/+/, '')
      if (i < segs.length - 1) v = v.replace(/\/+$/, '')
      return v
    })
    .filter(s => s !== '')
    .join('/')
  return trailing && !joined.endsWith('/') ? `${joined}/` : joined
}

/** Parent folder prefix of a key or prefix: 'a/b/c.txt' → 'a/b/', 'a/b/' → 'a/', 'a' → ''. */
export function parentPrefix(keyOrPrefix: string): string {
  const s = keyOrPrefix.endsWith('/') ? keyOrPrefix.slice(0, -1) : keyOrPrefix
  const i = s.lastIndexOf('/')
  return i < 0 ? '' : s.slice(0, i + 1)
}

/** Last segment: 'a/b/c.txt' → 'c.txt', 'a/b/' → 'b'. */
export function baseName(keyOrPrefix: string): string {
  const s = keyOrPrefix.endsWith('/') ? keyOrPrefix.slice(0, -1) : keyOrPrefix
  const i = s.lastIndexOf('/')
  return i < 0 ? s : s.slice(i + 1)
}

/** File extension in lower case without dot ('' if none): 'a/b.TAR.GZ' → 'gz'. */
export function extName(key: string): string {
  const b = baseName(key)
  const i = b.lastIndexOf('.')
  return i <= 0 ? '' : b.slice(i + 1).toLowerCase()
}

/** True when key denotes a folder placeholder ('a/b/'). */
export function isFolderKey(key: string): boolean {
  return key.endsWith('/')
}

/**
 * Encodes a key/prefix for use in a catch-all route param
 * (`/servers/:id/buckets/:bucket/objects/:path(.*)*`): returns path segments.
 */
export function keyToRouteSegments(key: string): string[] {
  return key.split('/')
}

/** Inverse of keyToRouteSegments for `route.params.path` (string | string[]). */
export function routeParamToKey(param: string | string[] | undefined): string {
  if (param === undefined) return ''
  return Array.isArray(param) ? param.join('/') : param
}

/** Validates an object key (≤ 1024 UTF-8 bytes, non-empty). Returns an error message or undefined. */
export function validateObjectKey(key: string): string | undefined {
  if (!key) return 'Key must not be empty'
  if (new TextEncoder().encode(key).length > 1024) return 'Key must be at most 1024 bytes'
  // eslint-disable-next-line no-control-regex
  if (/[\x00-\x08\x0b\x0c\x0e-\x1f]/.test(key)) return 'Key must not contain control characters'
  return undefined
}

/** Validates a bucket name per S3 rules. Returns an error message or undefined. */
export function validateBucketName(name: string): string | undefined {
  if (name.length < 3 || name.length > 63) return 'Bucket names must be 3–63 characters long'
  if (!/^[a-z0-9.-]+$/.test(name)) return 'Use only lowercase letters, digits, dots and hyphens'
  if (!/^[a-z0-9]/.test(name) || !/[a-z0-9]$/.test(name)) return 'Bucket names must start and end with a letter or digit'
  if (name.includes('..')) return 'Bucket names must not contain two adjacent dots'
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(name)) return 'Bucket names must not be formatted as an IP address'
  if (name.startsWith('xn--')) return 'Bucket names must not start with "xn--"'
  if (name.endsWith('-s3alias') || name.endsWith('--ol-s3')) return 'Bucket names must not end with "-s3alias" or "--ol-s3"'
  return undefined
}
