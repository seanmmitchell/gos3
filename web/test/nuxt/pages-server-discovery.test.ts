import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import DiscoveryPage from '~/pages/servers/[id]/discovery.vue'
import type { DiscoveryState, Info } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { AdminClient, AdminError } from '~/utils/admin/client'
import { clearClientCache } from '~/utils/clients'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(DiscoveryPage) }) })
const info: Info = {
  service: 'gos3', id: '0123456789abcdef', name: 'NAS', version: '0.3.0', apiVersion: 'v1',
  region: 'us-east-1', endpoints: { s3: 'http://nas.example:9000', admin: 'http://nas.example:9000/_gos3/admin/v1' },
  features: ['admin'], startedAt: '2026-01-01T00:00:00Z'
}
const state: DiscoveryState = {
  enabled: false, self: info, peers: [],
  config: { enabled: false, advertise: false, mdns: false, beacon: false, beaconGroup: '239.255.73.83:7373',
    beaconInterval: 10_000_000_000, seeds: null, dnsSrv: null, peerTtl: 60_000_000_000, allowScan: false, maxScanHosts: 1024 }
}

async function connect(kind: 'gos3' | 's3') {
  const store = useConnectionsStore()
  store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind })
  if (kind === 's3') return
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname
    const body = path === '/_gos3/info' ? info : path === '/_gos3/admin/v1/whoami'
      ? { user: 'root', accessKey: 'AK', isRoot: true, policies: [], server: info, capabilities: { ...NO_CAPABILITIES, admin: true, diagnostics: true } }
      : null
    return new Response(JSON.stringify(body), { status: body ? 200 : 404, headers: { 'content-type': 'application/json' } })
  }))
  await store.refreshStatus('nas')
}

describe('server discovery page', () => {
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

  it('shows discovery state and the scan-disabled reason', async () => {
    await connect('gos3')
    const load = vi.spyOn(AdminClient.prototype, 'discovery').mockResolvedValue(state)
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/discovery' })
    await vi.waitFor(() => expect(page.text()).toContain('Discovery is disabled on this server'))
    expect(page.text()).toContain('Scans are disabled on this server')
    expect(page.text()).toContain('No peers: discovery is disabled')
    expect(load).toHaveBeenCalled()
    page.unmount()
  })

  it('reports denied discovery access', async () => {
    await connect('gos3')
    vi.spyOn(AdminClient.prototype, 'discovery').mockRejectedValue(new AdminError('AccessDenied', 'denied', 403))
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/discovery' })
    await vi.waitFor(() => expect(page.text()).toContain('No access to discovery'))
    page.unmount()
  })

  it('keeps generic S3 connections out of the admin discovery API', async () => {
    await connect('s3')
    const load = vi.spyOn(AdminClient.prototype, 'discovery')
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/discovery' })
    expect(page.text()).toContain('Discovery needs a gos3 server')
    expect(load).not.toHaveBeenCalled()
    page.unmount()
  })
})
