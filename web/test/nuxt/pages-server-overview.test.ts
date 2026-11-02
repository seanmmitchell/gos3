import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import ServerOverviewPage from '~/pages/servers/[id]/index.vue'
import type { Info, Stats } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { AdminClient } from '~/utils/admin/client'
import { S3Client } from '~/utils/s3/client'
import { clearClientCache } from '~/utils/clients'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(ServerOverviewPage) }) })
const info: Info = {
  service: 'gos3', id: '0123456789abcdef', name: 'NAS', version: '0.3.0', apiVersion: 'v1',
  region: 'us-east-1', endpoints: { s3: 'http://nas.example:9000', admin: 'http://nas.example:9000/_gos3/admin/v1' },
  features: ['admin'], startedAt: '2026-01-01T00:00:00Z'
}
const stats: Stats = {
  metrics: { startedAt: '2026-01-01T00:00:00Z', uptimeSeconds: 3600, requests: 12, errors4xx: 2, errors5xx: 1,
    bytesIn: 100, bytesOut: 200, inFlight: 1, ops: [], recent: [] },
  storage: { buckets: 2, objects: 3, bytes: 4096, versions: 5, versionBytes: 8192, deleteMarkers: 1,
    pendingUploads: 2, computedAt: '2026-01-01T01:00:00Z' },
  runtime: { goroutines: 10, heapAllocBytes: 1024, sysBytes: 2048, gcPauseTotalNs: 0 }, disk: null
}

async function connect(kind: 'gos3' | 's3', viewInfo = false) {
  const store = useConnectionsStore()
  store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind })
  if (kind === 's3') return
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname
    const body = path === '/_gos3/info' ? info : path === '/_gos3/admin/v1/whoami'
      ? { user: 'root', accessKey: 'AK', isRoot: true, policies: [], server: info, capabilities: { ...NO_CAPABILITIES, admin: true, viewStats: true, viewInfo } }
      : null
    return new Response(JSON.stringify(body), { status: body ? 200 : 404, headers: { 'content-type': 'application/json' } })
  }))
  await store.refreshStatus('nas')
}

describe('server overview page', () => {
  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    clearClientCache()
    useConnectionsStore().resetState()
  })
  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('renders gos3 storage and request stats', async () => {
    await connect('gos3')
    const read = vi.spyOn(AdminClient.prototype, 'stats').mockResolvedValue(stats)
    const page = await mountSuspended(WithProviders, { route: '/servers/nas' })
    await vi.waitFor(() => expect(page.text()).toContain('pending uploads'))
    expect(page.text()).toContain('NAS')
    expect(page.text()).toContain('Delete markers')
    expect(page.text()).toContain('Requests')
    expect(read).toHaveBeenCalled()
    page.unmount()
  })

  it('shows cluster quorum, placement, and acknowledgement policy', async () => {
    await connect('gos3', true)
    vi.spyOn(AdminClient.prototype, 'serverInfo').mockResolvedValue({
      ...info, backend: 'cluster', dataPath: '/data', goVersion: 'go1.25', os: 'linux', arch: 'amd64', numCPU: 2, uptimeSeconds: 60
    })
    vi.spyOn(AdminClient.prototype, 'stats').mockResolvedValue(stats)
    const read = vi.spyOn(AdminClient.prototype, 'clusterStatus').mockResolvedValue({
      clusterId: 'c1', nodeId: 'node-a', leaderId: 'node-b', leader: false, available: true,
      commitIndex: 14, appliedIndex: 14, lag: 0, replicaCount: 3, minMemoryAcks: 2, minDurableAcks: 2,
      lastWrite: { operationId: 'operation-1', memoryAcks: 2, durableAcks: 2, consensusCommitted: true },
      lastRepair: { repaired: 3, checkedAt: '2026-01-01T00:00:00Z' },
      peers: [{ id: 'node-a', local: true, leader: false, progressKnown: false }, { id: 'node-b', local: false, leader: true, progressKnown: false }]
    })
    const page = await mountSuspended(WithProviders, { route: '/servers/nas' })
    await vi.waitFor(() => expect(page.text()).toContain('Quorum available'))
    expect(page.text()).toContain('node-b')
    expect(page.text()).toContain('3 replicas')
    expect(page.text()).toContain('2 memory · 2 durable')
    expect(page.text()).toContain('2/2 memory · 2/2 durable · consensus committed')
    expect(page.text()).toContain('Local replica repair')
    expect(page.text()).toContain('3 copies restored')
    expect(read).toHaveBeenCalled()
    page.unmount()
  })

  it('shows a failed status request and retries to the cluster diagnostic view', async () => {
    await connect('gos3', true)
    vi.spyOn(AdminClient.prototype, 'serverInfo').mockResolvedValue({
      ...info, backend: 'cluster', dataPath: '/data', goVersion: 'go1.25', os: 'linux', arch: 'amd64', numCPU: 2, uptimeSeconds: 60
    })
    vi.spyOn(AdminClient.prototype, 'stats').mockResolvedValue(stats)
    const read = vi.spyOn(AdminClient.prototype, 'clusterStatus')
      .mockRejectedValueOnce(new Error('quorum read timed out'))
      .mockResolvedValue({
        clusterId: 'c1', nodeId: 'node-a', leaderId: '', leader: false, available: false,
        error: 'no quorum', commitIndex: 14, appliedIndex: 12, lag: 2,
        replicaCount: 2, minMemoryAcks: 2, minDurableAcks: 2,
        lastWrite: { operationId: 'operation-2', memoryAcks: 2, durableAcks: 1, consensusCommitted: false, error: 'durable acknowledgment missing' },
        lastRepair: { repaired: 0, error: 'replica offline', checkedAt: '2026-01-01T00:00:00Z' },
        peers: [{ id: 'node-a', local: true, leader: false, progressKnown: false }]
      })
    const page = await mountSuspended(WithProviders, { route: '/servers/nas' })
    await vi.waitFor(() => expect(page.text()).toContain('Could not load cluster status'))
    expect(page.text()).toContain('quorum read timed out')
    const retry = page.findAll('button').find(button => button.text().includes('Retry'))
    expect(retry).toBeDefined()
    await retry!.trigger('click')
    await vi.waitFor(() => expect(page.text()).toContain('Quorum unavailable'))
    expect(page.text()).toContain('Unknown')
    expect(page.text()).toContain('2/2 memory · 1/2 durable · pending or failed')
    expect(page.text()).toContain('no quorum')
    expect(page.text()).toContain('durable acknowledgment missing')
    expect(page.text()).toContain('replica offline')
    expect(read).toHaveBeenCalledTimes(2)
    page.unmount()
  })

  it('uses the bucket summary for a generic S3 connection', async () => {
    await connect('s3')
    const read = vi.spyOn(S3Client.prototype, 'listBuckets').mockResolvedValue({ buckets: [
      { name: 'photos', creationDate: '2026-01-01T00:00:00Z' }
    ] })
    const admin = vi.spyOn(AdminClient.prototype, 'stats')
    const page = await mountSuspended(WithProviders, { route: '/servers/nas' })
    await vi.waitFor(() => expect(page.text()).toContain('photos'))
    expect(read).toHaveBeenCalled()
    expect(admin).not.toHaveBeenCalled()
    page.unmount()
  })
})
