// The @nuxt/icon server endpoint is disabled (icon.serverBundle: false in
// nuxt.config.ts; icons are bundled into the client). Should a module
// update register it again, its handler caches every distinct `icons`
// query in memory without bound, so the path is refused here regardless.
export default defineEventHandler((event) => {
  if (!/^\/(?:.*\/)?api\/_nuxt_icon(?:\/|\?|$)/.test(event.path ?? '')) return
  setResponseStatus(event, 404)
  setResponseHeader(event, 'cache-control', 'no-store')
  return ''
})
