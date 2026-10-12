// URL construction for S3 requests (path-style and virtual-hosted-style).

/** RFC 3986 encoding of one path segment / query component. */
export function encodeRfc3986(value: string): string {
  return encodeURIComponent(value).replace(/[!'()*]/g, c => '%' + c.charCodeAt(0).toString(16).toUpperCase())
}

/** Encodes an object key for use in a URL path: each '/'-separated segment is encoded. */
export function encodeKey(key: string): string {
  return key.split('/').map(encodeRfc3986).join('/')
}

/**
 * True when a key (or prefix) has a '.' or '..' path segment ('x/../y',
 * 'a/./b', '..', 'dir/.'). Such keys are legal in S3, but the URL parser of
 * browsers (WHATWG URL, also used by fetch and XMLHttpRequest) resolves dot
 * segments — percent-encoded ones ('%2e%2e') included — so a request for
 * 'x/../y' would silently address 'y'. They can only be named where the key
 * travels outside the URL path: request bodies (DeleteObjects), the
 * x-amz-copy-source header and query parameters.
 */
export function hasDotSegment(key: string): boolean {
  return key.split('/').some(s => s === '.' || s === '..')
}

const IPV4 = /^\d{1,3}(?:\.\d{1,3}){3}$/

/** True when bucket can be used as a DNS label in virtual-hosted-style URLs over TLS. */
export function isDnsCompatibleBucket(bucket: string): boolean {
  return /^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/.test(bucket) && !bucket.includes('..')
}

/** True when hostname is an IP literal or localhost (virtual hosting impossible). */
export function isIpOrLocalhost(hostname: string): boolean {
  const h = hostname.replace(/^\[|\]$/g, '')
  return IPV4.test(h) || h.includes(':') || h === 'localhost' || h.endsWith('.localhost')
}

export type QueryValue = string | number | boolean | null | undefined
export type Query = Record<string, QueryValue>

export interface UrlBuilderOptions {
  endpoint: string
  forcePathStyle: boolean
}

/**
 * Builds the request URL for bucket/key with query parameters. Query keys
 * with value '' are emitted as `name=` (S3 sub-resources like `?versioning`);
 * null/undefined values are skipped.
 */
export function buildS3Url(opts: UrlBuilderOptions, bucket?: string, key?: string, query?: Query): URL {
  const base = new URL(opts.endpoint)
  const basePath = base.pathname.replace(/\/+$/, '')
  let host = base.host
  let p = basePath
  if (bucket) {
    const virtual = !opts.forcePathStyle && isDnsCompatibleBucket(bucket) && !isIpOrLocalhost(base.hostname)
    if (virtual) {
      host = `${bucket}.${base.host}`
      p += '/'
    } else {
      p += `/${encodeRfc3986(bucket)}/`
    }
    if (key) p += encodeKey(key)
  } else {
    p += '/'
  }
  const url = new URL(`${base.protocol}//${host}${p}`)
  if (query) {
    const parts: string[] = []
    for (const [k, v] of Object.entries(query)) {
      if (v === undefined || v === null) continue
      parts.push(`${encodeRfc3986(k)}=${encodeRfc3986(String(v))}`)
    }
    if (parts.length) url.search = '?' + parts.join('&')
  }
  return url
}

/** Normalises a user-entered endpoint: adds http:// if missing, drops trailing slashes. */
export function normalizeEndpoint(input: string): string {
  let s = input.trim()
  if (!s) throw new TypeError('Endpoint is required')
  if (!/^[a-z][a-z0-9+.-]*:\/\//i.test(s)) s = `http://${s}`
  const u = new URL(s)
  if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new TypeError('Endpoint must use http or https')
  const p = u.pathname.replace(/\/+$/, '')
  return `${u.protocol}//${u.host}${p}`
}

/** Returns normalizeEndpoint(input) or undefined when invalid. */
export function tryNormalizeEndpoint(input: string): string | undefined {
  try {
    return normalizeEndpoint(input)
  } catch {
    return undefined
  }
}
