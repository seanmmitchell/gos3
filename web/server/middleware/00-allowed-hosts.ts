// Host check for the console's own pages (standalone Node server only).
//
// The SPA shell inlines the public runtime config, including the preset
// connections from NUXT_PUBLIC_SERVERS, and `servers.json` holds presets
// too. A website that DNS-rebinds its own name to the console's address
// becomes same-origin and could read them. Requests that can return presets
// therefore need a Host (and, behind a proxy, X-Forwarded-Host / Forwarded
// host) the attacker cannot control: an IP address, localhost, a name that
// cannot exist in public DNS (single label, .local, .internal, .home.arpa,
// .lan, …) or a name listed in GOS3_UI_ALLOWED_HOSTS ("*" accepts any).
// Other hosts get 421 Misdirected Request. Without presets the shell holds
// nothing server-side worth reading, so any Host is served.
//
// The /api/discovery/* routes apply their own, stricter check
// (../utils/discovery/http.ts rejectForeignRequest).
import { allowedHostsFromEnv, hasConfiguredPresets, hostnameOf, isConsoleHost, requestHosts } from '../utils/discovery/http'

/** Host names already logged as refused (bounded). */
const warned = new Set<string>()

export default defineEventHandler((event) => {
  const path = event.path ?? ''
  const presetFile = /\/servers\.json(?:[?#]|$)/.test(path)
  if (!presetFile && !hasConfiguredPresets(useRuntimeConfig(event).public?.servers)) return
  const allowed = allowedHostsFromEnv()
  // A missing Host (HTTP/1.0, not a browser) counts as allowed, so `bad` is
  // undefined exactly when every value passes.
  const bad = requestHosts(event).find(h => !isConsoleHost(h, allowed))
  if (bad === undefined) return
  const name = hostnameOf(bad)
  if (!warned.has(name) && warned.size < 64) {
    warned.add(name)
    console.warn(`[gos3-ui] refused a request for Host "${name.slice(0, 64)}": add it to GOS3_UI_ALLOWED_HOSTS to serve the console (with its presets) under that name`)
  }
  setResponseStatus(event, 421)
  setResponseHeader(event, 'cache-control', 'no-store')
  setResponseHeader(event, 'content-type', 'text/plain; charset=utf-8')
  return 'This console is not served under this host name. The operator can allow it with GOS3_UI_ALLOWED_HOSTS.\n'
})
