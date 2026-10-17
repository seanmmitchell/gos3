// Sends each rendered HTML page of the standalone server with a
// Content-Security-Policy that allows exactly its inline scripts by hash
// (see ../utils/csp.ts). The runtime config Nuxt inlines comes from the
// environment at request time, so the hashes cannot be computed at build
// time; they are cached per distinct page.
import { contentSecurityPolicy, inlineScriptHashes } from '../utils/csp'

const MAX_CACHED = 32

export default defineNitroPlugin((nitroApp) => {
  // `nuxt dev` (Vite client, HMR) and prerendering (the embedded build is
  // served by gos3 with its own policy) are left alone.
  if (import.meta.dev || import.meta.prerender) return
  const cache = new Map<string, string>()
  nitroApp.hooks.hook('render:html', (html, { event }) => {
    const page = [...html.head, ...html.bodyPrepend, ...html.body, ...html.bodyAppend].join('\n')
    let policy = cache.get(page)
    if (policy === undefined) {
      policy = contentSecurityPolicy(inlineScriptHashes(page))
      if (cache.size >= MAX_CACHED) cache.clear()
      cache.set(page, policy)
    }
    setResponseHeader(event, 'content-security-policy', policy)
  })
})
