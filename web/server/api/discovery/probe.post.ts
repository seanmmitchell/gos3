// POST /api/discovery/probe {url} — fetches <url>/_gos3/info from the UI host
// and returns the identity document (Info). 502 ProbeFailed on failure.
import { errorResponse, isJsonRequest, readJsonBody, rejectForeignRequest } from '../../utils/discovery/http'
import { localDiscoveryEnabled } from '../../utils/discovery/local'
import { fetchInfo } from '../../utils/discovery/probe'
import { normalizeBaseUrl, ScanRequestError } from '../../utils/discovery/targets'

const PROBE_TIMEOUT_MS = 4000

export default defineEventHandler(async (event) => {
  const foreign = rejectForeignRequest(event)
  if (foreign) return foreign
  if (!localDiscoveryEnabled()) return errorResponse(event, 403, 'AccessDenied', 'Local discovery is disabled on this UI server.')
  if (!isJsonRequest(event)) return errorResponse(event, 415, 'InvalidRequest', 'Content-Type must be application/json.')
  const body = await readJsonBody(event)
  if (!body.ok) return errorResponse(event, 400, 'InvalidRequest', 'The request body is not valid JSON.')
  const raw = typeof body.body === 'object' && body.body !== null ? (body.body as Record<string, unknown>).url : undefined
  if (typeof raw !== 'string' || !raw.trim()) return errorResponse(event, 400, 'InvalidRequest', 'Body must be {"url": "http://host:port"}.')
  let url: string
  try {
    url = normalizeBaseUrl(raw)
  } catch (e) {
    return errorResponse(event, 400, 'InvalidRequest', e instanceof ScanRequestError ? e.message : 'Invalid URL.')
  }
  setResponseHeader(event, 'cache-control', 'no-store')
  try {
    return await fetchInfo(url, { timeoutMs: PROBE_TIMEOUT_MS })
  } catch (e) {
    return errorResponse(event, 502, 'ProbeFailed', `${url}: ${e instanceof Error ? e.message : String(e)}`)
  }
})
