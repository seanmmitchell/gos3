// servers.json of the standalone server (see ../routes/servers.json.get.ts).

/** The h3 event type (h3 is not a direct dependency; take it from an auto-import). */
type H3Event = Parameters<typeof getQuery>[0]

/**
 * Answers a request for servers.json with the preset file's text (bundled
 * at build time), or 404 when the build has none.
 */
export function presetsFileResponse(event: H3Event, content: string | null): string {
  if (content === null) {
    setResponseStatus(event, 404)
    setResponseHeader(event, 'cache-control', 'no-store')
    return ''
  }
  setResponseHeader(event, 'content-type', 'application/json; charset=utf-8')
  setResponseHeader(event, 'cache-control', 'no-cache')
  return content
}
