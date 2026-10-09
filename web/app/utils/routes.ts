// Canonical in-app route builders (auto-imported by Nuxt). Use these instead
// of hand-written strings so every page links consistently.
//
// Conventions:
// - Folder browsing:   /servers/:id/buckets/:bucket?prefix=a/b/
// - Object details:    /servers/:id/buckets/:bucket/objects/<key segments>
//   (catch-all param `path`; read the key back with routeParamToKey()).

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
  server: (id: string) => `/servers/${seg(id)}`,
  buckets: (id: string) => `/servers/${seg(id)}/buckets`,
  bucket: (id: string, bucket: string, prefix?: string) =>
    `/servers/${seg(id)}/buckets/${seg(bucket)}${prefix ? `?prefix=${encodeURIComponent(prefix)}` : ''}`,
  bucketSettings: (id: string, bucket: string, tab?: string) =>
    `/servers/${seg(id)}/buckets/${seg(bucket)}/settings${tab ? `?tab=${encodeURIComponent(tab)}` : ''}`,
  object: (id: string, bucket: string, key: string, versionId?: string) =>
    `/servers/${seg(id)}/buckets/${seg(bucket)}/objects/${key.split('/').map(seg).join('/')}${versionId ? `?versionId=${encodeURIComponent(versionId)}` : ''}`,
  users: (id: string) => `/servers/${seg(id)}/users`,
  user: (id: string, name: string) => `/servers/${seg(id)}/users/${seg(name)}`,
  policies: (id: string) => `/servers/${seg(id)}/policies`,
  policy: (id: string, name: string) => `/servers/${seg(id)}/policies/${seg(name)}`,
  settings: (id: string) => `/servers/${seg(id)}/settings`,
  discovery: (id: string) => `/servers/${seg(id)}/discovery`
}

/** Single string route param (first element for arrays). */
export function routeParam(v: string | string[] | undefined): string {
  if (Array.isArray(v)) return v[0] ?? ''
  return v ?? ''
}
