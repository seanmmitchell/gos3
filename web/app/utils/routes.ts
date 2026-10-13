// Canonical in-app route builders (auto-imported by Nuxt). Use these instead
// of hand-written strings so every page links consistently.
//
// Conventions:
// - Folder browsing:   /servers/:id/buckets/:bucket?prefix=a/b/
// - Object details:    /servers/:id/buckets/:bucket/objects/<key segments>
//   (keys with '.' / '..' segments: /servers/:id/buckets/:bucket/objects/?key=<key>)
//   (catch-all param `path`; read the key back with routeParamToKey()).

import { hasDotSegment } from './s3/url'

const seg = (s: string) => encodeURIComponent(s)

export const appRoutes = {
  home: () => '/',
  servers: () => '/servers',
  discover: () => '/discover',
  /** Connect page; optionally for an existing connection and with a post-login redirect. */
  connect: (serverId?: string, redirect?: string) => {
    const q = new URLSearchParams()
    if (serverId) q.set('server', serverId)
    if (redirect) q.set('redirect', redirect)
    const s = q.toString()
    return s ? `/connect?${s}` : '/connect'
  },
  /**
   * "Connect to a new server" form (never the sign-in of an existing
   * connection, not even the hosting server's), optionally prefilled.
   */
  newServer: (prefill: { endpoint?: string, name?: string } = {}) => {
    const q = new URLSearchParams({ new: '1' })
    if (prefill.endpoint) q.set('endpoint', prefill.endpoint)
    if (prefill.name) q.set('name', prefill.name)
    return `/connect?${q.toString()}`
  },
  server: (id: string) => `/servers/${seg(id)}`,
  buckets: (id: string) => `/servers/${seg(id)}/buckets`,
  bucket: (id: string, bucket: string, prefix?: string) =>
    `/servers/${seg(id)}/buckets/${seg(bucket)}${prefix ? `?prefix=${encodeURIComponent(prefix)}` : ''}`,
  bucketSettings: (id: string, bucket: string, tab?: string) =>
    `/servers/${seg(id)}/buckets/${seg(bucket)}/settings${tab ? `?tab=${encodeURIComponent(tab)}` : ''}`,
  /**
   * Object details. Keys with '.' / '..' segments go into `?key=` instead of
   * the path, which browsers would normalise to a different key.
   */
  object: (id: string, bucket: string, key: string, versionId?: string) => {
    const base = `/servers/${seg(id)}/buckets/${seg(bucket)}/objects/`
    const v = versionId ? `versionId=${encodeURIComponent(versionId)}` : ''
    if (hasDotSegment(key)) return `${base}?key=${encodeURIComponent(key)}${v ? `&${v}` : ''}`
    return `${base}${key.split('/').map(seg).join('/')}${v ? `?${v}` : ''}`
  },
  users: (id: string) => `/servers/${seg(id)}/users`,
  user: (id: string, name: string) => `/servers/${seg(id)}/users/${seg(name)}`,
  policies: (id: string) => `/servers/${seg(id)}/policies`,
  policy: (id: string, name: string) => `/servers/${seg(id)}/policies/${seg(name)}`,
  settings: (id: string) => `/servers/${seg(id)}/settings`,
  storage: (id: string) => `/servers/${seg(id)}/storage`,
  discovery: (id: string) => `/servers/${seg(id)}/discovery`
}

/** Single string route param (first element for arrays). */
export function routeParam(v: string | string[] | undefined): string {
  if (Array.isArray(v)) return v[0] ?? ''
  return v ?? ''
}
