// GET /api/discovery/capabilities — tells the UI that local discovery from
// the UI host is available (standalone Node server only; the static/embedded
// build has no server routes, so the UI hides the feature there).
import { loadMdns, localDiscoveryEnabled } from '../../utils/discovery/local'
import { MAX_SCAN_HOSTS } from '../../utils/discovery/targets'

export interface LocalDiscoveryCapabilities {
  localDiscovery: boolean
  mdns: boolean
  beacon: boolean
  scan: boolean
  maxScanHosts: number
}

export default defineEventHandler(async (event): Promise<LocalDiscoveryCapabilities> => {
  setResponseHeader(event, 'cache-control', 'no-store')
  const enabled = localDiscoveryEnabled()
  const mdns = enabled ? (await loadMdns()) !== null : false
  return { localDiscovery: enabled, mdns, beacon: enabled, scan: enabled, maxScanHosts: MAX_SCAN_HOSTS }
})
