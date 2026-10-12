// POST /api/discovery/scan — network scan from the UI host. Body and result
// are the admin API's discovery.ScanRequest / discovery.ScanResult (timeout
// in nanoseconds). Hosts are capped at 1024, concurrency at 64, the body at
// SCAN_BODY_LIMIT bytes (413 beyond), and at most MAX_CONCURRENT_SCANS scans
// run at once (429 TooManyRequests beyond; the slot is taken before the
// body is read).
import { clientAbortSignal, discoveryDisabled, errorResponse, isJsonRequest, MAX_CONCURRENT_SCANS, readJsonBody, rejectForeignRequest, SCAN_BODY_LIMIT, scanSlots, tooManyRequests } from '../../utils/discovery/http'
import { localDiscoveryEnabled } from '../../utils/discovery/local'
import { runScan } from '../../utils/discovery/scan'
import { planScan, ScanRequestError, type ScanPlan } from '../../utils/discovery/targets'

export default defineEventHandler(async (event) => {
  const foreign = rejectForeignRequest(event)
  if (foreign) return foreign
  if (!localDiscoveryEnabled()) return discoveryDisabled(event)
  if (!isJsonRequest(event)) return errorResponse(event, 415, 'InvalidRequest', 'Content-Type must be application/json.')
  const release = scanSlots.tryAcquire()
  if (!release) return tooManyRequests(event, 'network scans', MAX_CONCURRENT_SCANS)
  try {
    const body = await readJsonBody(event, SCAN_BODY_LIMIT)
    if (!body.ok) return body.error
    let plan: ScanPlan
    try {
      plan = planScan(body.body)
    } catch (e) {
      if (e instanceof ScanRequestError) return errorResponse(event, 400, 'InvalidRequest', e.message)
      return errorResponse(event, 500, 'InternalError', 'The scan request could not be planned.')
    }
    setResponseHeader(event, 'cache-control', 'no-store')
    try {
      return await runScan(plan, { signal: clientAbortSignal(event) })
    } catch {
      return errorResponse(event, 500, 'InternalError', 'The scan failed.')
    }
  } finally {
    release()
  }
})
