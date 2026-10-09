// Active scan from the UI host (POST /api/discovery/scan): probes every URL
// of a validated plan (./targets.ts) and returns a discovery.ScanResult.
import type { Peer, ScanResult } from '~/types/admin'
import { mergePeers, peerFromInfo } from './peers'
import { fetchInfo, runPool, type FetchFn } from './probe'
import { formatGoDuration, type ScanPlan } from './targets'

export async function runScan(plan: ScanPlan, opts: { signal?: AbortSignal, fetch?: FetchFn } = {}): Promise<ScanResult> {
  const t0 = Date.now()
  let probed = 0
  const now = new Date()
  const results = await runPool(plan.urls, plan.concurrency, async (url) => {
    try {
      const info = await fetchInfo(url, { timeoutMs: plan.timeoutMs, signal: opts.signal, fetch: opts.fetch })
      return peerFromInfo(info, url, 'scan', now)
    } catch {
      return undefined
    } finally {
      probed++
    }
  }, opts.signal)
  const found = mergePeers(results.filter((p): p is Peer => !!p))
  return { found, probed, elapsed: formatGoDuration(Date.now() - t0) }
}
