import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import SettingsPage from '~/pages/servers/[id]/settings.vue'
import type { Info, Settings } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { AdminClient } from '~/utils/admin/client'
import { clearClientCache } from '~/utils/clients'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(SettingsPage) }) })
const info: Info = {
  service: 'gos3', id: '0123456789abcdef', name: 'NAS', version: '0.3.0', apiVersion: 'v1',
  region: 'us-east-1', endpoints: { s3: 'http://nas.example:9000', admin: 'http://nas.example:9000/_gos3/admin/v1' },
  features: ['admin'], startedAt: '2026-01-01T00:00:00Z'
}
const settings: Settings = {
  name: 'nas', region: 'us-east-1', publicUrl: 'http://nas.example:9000', domains: [], publicInfo: true, uiRedirect: true,
  cors: { allowedOrigins: ['*'], allowedHeaders: ['*'], exposeHeaders: ['ETag'], maxAgeSeconds: 3600 },
  discovery: { enabled: false, advertise: false, mdns: false, beacon: false, beaconGroup: '239.255.73.83:7373', beaconInterval: 10_000_000_000,
    seeds: null, dnsSrv: null, peerTtl: 60_000_000_000, allowScan: false, maxScanHosts: 1024 },
  logging: { level: 'info', format: 'text', accessLog: true }, lifecycle: { enabled: false, intervalSeconds: 3600 }, metricsPublic: false
}

async function connect(manageConfig: boolean) {
  const store = useConnectionsStore()
  store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname
    const body = path === '/_gos3/info' ? info : path === '/_gos3/admin/v1/whoami'
      ? { user: 'root', accessKey: 'AK', isRoot: true, policies: [], server: info, capabilities: { ...NO_CAPABILITIES, admin: true, manageConfig } }
      : null
    return new Response(JSON.stringify(body), { status: body ? 200 : 404, headers: { 'content-type': 'application/json' } })
  }))
  await store.refreshStatus('nas')
}

describe('server settings page', () => {
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

  it('loads settings, tracks a changed name, and saves the edited document', async () => {
    await connect(true)
    const getConfig = vi.spyOn(AdminClient.prototype, 'getConfig').mockResolvedValue(settings)
    const putConfig = vi.spyOn(AdminClient.prototype, 'putConfig').mockImplementation(async updated => updated)
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/settings' })
    await vi.waitFor(() => expect(page.text()).toContain('All changes saved.'))
    expect(getConfig).toHaveBeenCalled()
    await page.find('input').setValue('nas-two')
    expect(page.text()).toContain('Unsaved changes')
    await page.find('form').trigger('submit')
    await vi.waitFor(() => expect(putConfig).toHaveBeenCalled())
    expect(putConfig.mock.calls.at(-1)![0].name).toBe('nas-two')
    await vi.waitFor(() => expect(page.text()).toContain('All changes saved.'))
    page.unmount()
  })

  it('shows a read-only form without update permission', async () => {
    await connect(false)
    vi.spyOn(AdminClient.prototype, 'getConfig').mockResolvedValue(settings)
    const putConfig = vi.spyOn(AdminClient.prototype, 'putConfig')
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/settings' })
    await vi.waitFor(() => expect(page.text()).toContain('Changing settings requires the admin:UpdateConfig permission'))
    expect(page.find('fieldset').attributes('disabled')).toBeDefined()
    expect(putConfig).not.toHaveBeenCalled()
    page.unmount()
  })
})
