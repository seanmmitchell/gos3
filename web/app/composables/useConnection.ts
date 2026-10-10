import type { MaybeRefOrGetter } from 'vue'
import type { AdminClient } from '~/utils/admin/client'
import type { S3Client } from '~/utils/s3/client'
import { useConnectionsStore } from '~/stores/connections'

/**
 * Resolves a connection and its (memoised) S3 / admin clients.
 *
 * With no argument the connection is taken from the current route
 * (`/servers/:id/...`) and falls back to the store's active connection,
 * then to the default server.
 *
 *   const { connection, s3, admin, requireS3 } = useConnection()
 *   const page = await requireS3().listObjectsV2(bucket, { delimiter: '/' })
 *
 * `admin` is only defined for kind === 'gos3' connections with credentials.
 */
export function useConnection(id?: MaybeRefOrGetter<string | null | undefined>) {
  const store = useConnectionsStore()
  const route = useRoute()

  const connId = computed<string | undefined>(() => {
    const explicit = toValue(id)
    if (explicit) return explicit
    const p = route.params.id
    if (typeof p === 'string' && p) return p
    // No server in the route: the active one, else the default server.
    return store.activeId ?? store.defaultConnection?.id ?? undefined
  })
  const connection = computed(() => store.byId(connId.value))
  const connected = computed(() => hasCredentials(connection.value))
  const isGos3 = computed(() => connection.value?.kind === 'gos3')
  const status = computed(() => (connId.value ? store.statusOf(connId.value) : undefined))

  const s3 = computed<S3Client | undefined>(() =>
    connection.value && connected.value ? getS3Client(connection.value) : undefined
  )
  const admin = computed<AdminClient | undefined>(() =>
    connection.value && connected.value && isGos3.value ? getAdminClient(connection.value) : undefined
  )

  /** S3 client or throws (use inside actions after the page checked `connected`). */
  function requireS3(): S3Client {
    if (!s3.value) throw new Error('This server is not connected (missing credentials).')
    return s3.value
  }

  /** Admin client or throws (gos3 connections only). */
  function requireAdmin(): AdminClient {
    if (!admin.value) {
      throw new Error(isGos3.value ? 'This server is not connected (missing credentials).' : 'Admin features are only available on gos3 servers.')
    }
    return admin.value
  }

  return { id: connId, connection, connected, isGos3, status, s3, admin, requireS3, requireAdmin }
}
