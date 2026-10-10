// GET /api/discovery/local?wait=<ms> — mDNS browse + beacon query from the UI
// host for `wait` ms (default 3000, 500–10000), then verification of every
// candidate via GET /_gos3/info. Returns {peers, errors, candidates, elapsedMs}.
import { clientAbortSignal, errorResponse, rejectForeignRequest } from '../../utils/discovery/http'
import { DEFAULT_WAIT_MS, discoverLocal, loadMdns, localDiscoveryEnabled, MAX_WAIT_MS, MIN_WAIT_MS } from '../../utils/discovery/local'

export default defineEventHandler(async (event) => {
  const foreign = rejectForeignRequest(event)
  if (foreign) return foreign
  if (!localDiscoveryEnabled()) return errorResponse(event, 403, 'AccessDenied', 'Local discovery is disabled on this UI server.')
  const q = getQuery(event)
  let wait = DEFAULT_WAIT_MS
  if (q.wait !== undefined) {
    const raw = Array.isArray(q.wait) ? q.wait[0] : q.wait
    const n = Number(raw)
    if (!Number.isFinite(n) || n < 0) return errorResponse(event, 400, 'InvalidRequest', '"wait" must be a number of milliseconds.')
    wait = Math.min(Math.max(Math.round(n), MIN_WAIT_MS), MAX_WAIT_MS)
  }
  setResponseHeader(event, 'cache-control', 'no-store')
  try {
    const mdns = (await loadMdns()) !== null
    return await discoverLocal(wait, { mdns, signal: clientAbortSignal(event) })
  } catch (e) {
    return errorResponse(event, 500, 'InternalError', e instanceof Error ? e.message : String(e))
  }
})
