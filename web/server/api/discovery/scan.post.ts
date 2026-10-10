// POST /api/discovery/scan — network scan from the UI host. Body and result
// are the admin API's discovery.ScanRequest / discovery.ScanResult (timeout
// in nanoseconds). Hosts are capped at 1024, concurrency at 64, and at most
// MAX_CONCURRENT_SCANS scans run at once (429 TooManyRequests beyond).
import { clientAbortSignal, errorResponse, isJsonRequest, MAX_CONCURRENT_SCANS, readJsonBody, rejectForeignRequest, scanSlots, tooManyRequests } from '../../utils/discovery/http'
import { localDiscoveryEnabled } from '../../utils/discovery/local'
import { runScan } from '../../utils/discovery/scan'
import { planScan, ScanRequestError, type ScanPlan } from '../../utils/discovery/targets'

export default defineEventHandler(async (event) => {
  const foreign = rejectForeignRequest(event)
  if (foreign) return foreign
  if (!localDiscoveryEnabled()) return errorResponse(event, 403, 'AccessDenied', 'Local discovery is disabled on this UI server.')
  if (!isJsonRequest(event)) return errorResponse(event, 415, 'InvalidRequest', 'Content-Type must be application/json.')
  const body = await readJsonBody(event)
  if (!body.ok) return errorResponse(event, 400, 'InvalidRequest', 'The request body is not valid JSON.')
  let plan: ScanPlan
  try {
    plan = planScan(body.body)
  } catch (e) {
    if (e instanceof ScanRequestError) return errorResponse(event, 400, 'InvalidRequest', e.message)
    return errorResponse(event, 500, 'InternalError', e instanceof Error ? e.message : String(e))
  }
  const release = scanSlots.tryAcquire()
  if (!release) return tooManyRequests(event, 'network scans', MAX_CONCURRENT_SCANS)
  setResponseHeader(event, 'cache-control', 'no-store')
  try {
    return await runScan(plan, { signal: clientAbortSignal(event) })
  } catch (e) {
    return errorResponse(event, 500, 'InternalError', e instanceof Error ? e.message : String(e))
  } finally {
    release()
  }
})
