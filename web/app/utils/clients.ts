// Memoised S3/admin clients per connection (auto-imported by Nuxt).
// Clients are cached by everything that affects signing, so editing a
// connection transparently yields a fresh client.
import type { Connection } from '~/types/connection'
import { AdminClient } from './admin/client'
import { S3Client } from './s3/client'

type ClientConn = Pick<Connection, 'id' | 'endpoint' | 'region' | 'accessKeyId' | 'secretAccessKey' | 'forcePathStyle'>

const s3Cache = new Map<string, { sig: string, client: S3Client }>()
const adminCache = new Map<string, { sig: string, client: AdminClient }>()

function signature(c: ClientConn): string {
  return JSON.stringify([c.endpoint, c.region, c.accessKeyId, c.secretAccessKey, c.forcePathStyle])
}

/** S3 client for a connection (cached). Requires credentials. */
export function getS3Client(conn: ClientConn): S3Client {
  const sig = signature(conn)
  const hit = s3Cache.get(conn.id)
  if (hit && hit.sig === sig) return hit.client
  const client = new S3Client({
    endpoint: conn.endpoint,
    region: conn.region,
    accessKeyId: conn.accessKeyId,
    secretAccessKey: conn.secretAccessKey,
    forcePathStyle: conn.forcePathStyle
  })
  s3Cache.set(conn.id, { sig, client })
  return client
}

/** Admin client for a connection (cached). Requires credentials. */
export function getAdminClient(conn: ClientConn): AdminClient {
  const sig = signature(conn)
  const hit = adminCache.get(conn.id)
  if (hit && hit.sig === sig) return hit.client
  const client = new AdminClient({
    endpoint: conn.endpoint,
    region: conn.region,
    accessKeyId: conn.accessKeyId,
    secretAccessKey: conn.secretAccessKey
  })
  adminCache.set(conn.id, { sig, client })
  return client
}

/** Drops cached clients (all, or for one connection). */
export function clearClientCache(id?: string): void {
  if (id === undefined) {
    s3Cache.clear()
    adminCache.clear()
  } else {
    s3Cache.delete(id)
    adminCache.delete(id)
  }
}
