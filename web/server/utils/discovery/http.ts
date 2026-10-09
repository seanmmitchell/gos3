// Shared HTTP helpers for the /api/discovery/* Nitro routes. Errors use the
// admin API envelope ({"error": {"code", "message"}}) so the UI parses them
// like admin errors.

/** The h3 event type (h3 is not a direct dependency; take it from an auto-import). */
type H3Event = Parameters<typeof getQuery>[0]

export interface ErrorEnvelope {
  error: { code: string, message: string }
}

/** Sets the status and returns the JSON error envelope. */
export function errorResponse(event: H3Event, status: number, code: string, message: string): ErrorEnvelope {
  setResponseStatus(event, status)
  setResponseHeader(event, 'cache-control', 'no-store')
  return { error: { code, message } }
}

/**
 * Requires `Content-Type: application/json` on POSTs. Cross-site pages
 * cannot send that without a CORS preflight (which this server never
 * approves), so it doubles as CSRF protection for the scan/probe routes.
 */
export function isJsonRequest(event: H3Event): boolean {
  const ct = getRequestHeader(event, 'content-type') ?? ''
  return /^application\/json\b/i.test(ct.trim())
}

/** AbortSignal that fires when the client goes away before the response ends. */
export function clientAbortSignal(event: H3Event): AbortSignal {
  const ctrl = new AbortController()
  const res = event.node.res
  res.once('close', () => {
    if (!res.writableEnded) ctrl.abort()
  })
  return ctrl.signal
}

/** Reads a JSON body without throwing (undefined on invalid JSON). */
export async function readJsonBody(event: H3Event): Promise<{ ok: true, body: unknown } | { ok: false }> {
  try {
    return { ok: true, body: await readBody<unknown>(event) }
  } catch {
    return { ok: false }
  }
}
