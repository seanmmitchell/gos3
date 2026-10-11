// POST /api/discovery/probe {url} — fetches <url>/_gos3/info from the UI host
// and returns the identity document (Info). 502 ProbeFailed on failure, with
// a coarse reason only (unreachable, timed out, HTTP <status>, authentication
// required, not a gos3 server, address not allowed). At most
// MAX_CONCURRENT_PROBES run at once (429 TooManyRequests beyond); the body is
// capped at PROBE_BODY_LIMIT bytes (413 beyond).
import { clientAbortSignal, discoveryDisabled, errorResponse, isJsonRequest, MAX_CONCURRENT_PROBES, PROBE_BODY_LIMIT, probeSlots, readJsonBody, rejectForeignRequest, tooManyRequests } from '../../utils/discovery/http'
import { localDiscoveryEnabled } from '../../utils/discovery/local'
import { fetchInfo, ProbeError } from '../../utils/discovery/probe'
import { assertProbeableUrl, normalizeBaseUrl, ScanRequestError } from '../../utils/discovery/targets'

const PROBE_TIMEOUT_MS = 4000

export default defineEventHandler(async (event) => {
  const foreign = rejectForeignRequest(event)
  if (foreign) return foreign
  if (!localDiscoveryEnabled()) return discoveryDisabled(event)
  if (!isJsonRequest(event)) return errorResponse(event, 415, 'InvalidRequest', 'Content-Type must be application/json.')
  // Take the slot before reading the body: concurrent uploads are bounded too.
  const release = probeSlots.tryAcquire()
  if (!release) return tooManyRequests(event, 'probes', MAX_CONCURRENT_PROBES)
  try {
    const body = await readJsonBody(event, PROBE_BODY_LIMIT)
    if (!body.ok) return body.error
    const raw = typeof body.body === 'object' && body.body !== null ? (body.body as Record<string, unknown>).url : undefined
    if (typeof raw !== 'string' || !raw.trim()) return errorResponse(event, 400, 'InvalidRequest', 'Body must be {"url": "http://host:port"}.')
    let url: string
    try {
      url = normalizeBaseUrl(raw)
      assertProbeableUrl(url, raw)
    } catch (e) {
      return errorResponse(event, 400, 'InvalidRequest', e instanceof ScanRequestError ? e.message : 'Invalid URL.')
    }
    setResponseHeader(event, 'cache-control', 'no-store')
    try {
      return await fetchInfo(url, { timeoutMs: PROBE_TIMEOUT_MS, signal: clientAbortSignal(event) })
    } catch (e) {
      return errorResponse(event, 502, 'ProbeFailed', `${url}: ${e instanceof ProbeError ? e.message : 'unreachable'}`)
    }
  } finally {
    release()
  }
})
