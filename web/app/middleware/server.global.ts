// Guards /servers/:id/** routes:
// - unknown connection id            → /servers
// - connection without credentials   → /connect?server=:id&redirect=<path>
// and marks the connection as active.
import { useConnectionsStore } from '~/stores/connections'

export default defineNuxtRouteMiddleware(async (to) => {
  if (!to.path.startsWith('/servers/')) return
  const id = routeParam(to.params.id as string | string[] | undefined)
  if (!id) return
  const store = useConnectionsStore()
  await store.ready()
  const conn = store.byId(id)
  if (!conn) return navigateTo(appRoutes.servers(), { replace: true })
  if (!hasCredentials(conn)) return navigateTo(appRoutes.connect(id, to.fullPath), { replace: true })
  if (store.activeId !== id) store.setActive(id)
})
