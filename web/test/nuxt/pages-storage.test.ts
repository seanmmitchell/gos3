import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import StoragePage from '~/pages/servers/[id]/storage.vue'
import type { BackingList, Info } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { AdminClient, AdminError } from '~/utils/admin/client'
import { clearClientCache } from '~/utils/clients'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(StoragePage) }) })
const info: Info = {
  service: 'gos3', id: '0123456789abcdef', name: 'NAS', version: '0.3.0', apiVersion: 'v1',
  region: 'us-east-1', endpoints: { s3: 'http://nas.example:9000', admin: 'http://nas.example:9000/_gos3/admin/v1' },
  features: ['admin'], startedAt: '2026-01-01T00:00:00Z'
}
const backings: BackingList = { defaultBacking: 'fast', autoFailover: false, backings: [
  { name: 'fast', kind: 'fs', path: '/data/fast', default: true, healthy: true, lastError: '', buckets: 2, replicas: 0, disk: null },
  { name: 'slow', kind: 'fs', path: '/data/slow', default: false, healthy: false, lastError: 'offline', buckets: 0, replicas: 1, disk: null }
] }

async function connect(kind: 'gos3' | 's3') {
  const store = useConnectionsStore()
  store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind })
  if (kind === 's3') return
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname
    const body = path === '/_gos3/info' ? info : path === '/_gos3/admin/v1/whoami'
      ? { user: 'root', accessKey: 'AK', isRoot: true, policies: [], server: info, capabilities: { ...NO_CAPABILITIES, admin: true, viewStorage: true } }
      : null
    return new Response(JSON.stringify(body), { status: body ? 200 : 404, headers: { 'content-type': 'application/json' } })
  }))
  await store.refreshStatus('nas')
}

describe('server storage page', () => {
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

  it('shows healthy and unhealthy backings from the admin API', async () => {
    await connect('gos3')
    const list = vi.spyOn(AdminClient.prototype, 'listBackings').mockResolvedValue(backings)
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/storage' })
    await vi.waitFor(() => expect(page.text()).toContain('1 backing is unhealthy'))
    expect(page.text()).toContain('fast')
    expect(page.text()).toContain('slow')
    expect(page.text()).toContain('Automatic failover off')
    expect(list).toHaveBeenCalled()
    page.unmount()
  })

  it('reports denied storage access', async () => {
    await connect('gos3')
    vi.spyOn(AdminClient.prototype, 'listBackings').mockRejectedValue(new AdminError('AccessDenied', 'denied', 403))
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/storage' })
    await vi.waitFor(() => expect(page.text()).toContain('No access to storage'))
    page.unmount()
  })

  it('skips admin backing reads for a generic S3 service', async () => {
    await connect('s3')
    const list = vi.spyOn(AdminClient.prototype, 'listBackings')
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/storage' })
    expect(page.text()).toContain('Storage backings need a gos3 server')
    expect(list).not.toHaveBeenCalled()
    page.unmount()
  })
})
