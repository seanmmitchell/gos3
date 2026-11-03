import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, nextTick } from 'vue'
import PoliciesPage from '~/pages/servers/[id]/policies/index.vue'
import type { Info, NamedPolicy } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { AdminClient } from '~/utils/admin/client'
import { clearClientCache } from '~/utils/clients'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(PoliciesPage) }) })
const info: Info = {
  service: 'gos3', id: '0123456789abcdef', name: 'NAS', version: '0.3.0', apiVersion: 'v1', region: 'us-east-1',
  endpoints: { s3: 'http://nas.example:9000', admin: 'http://nas.example:9000/_gos3/admin/v1' }, features: ['admin'], startedAt: '2026-01-01T00:00:00Z'
}
const policies: NamedPolicy[] = [
  { name: 'readonly', builtin: true, document: { Statement: [{ Effect: 'Allow', Action: ['s3:GetObject'], Resource: ['*'] }] }, createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-01T00:00:00Z' },
  { name: 'photos-write', description: 'Upload photos', builtin: false, document: { Statement: [{ Effect: 'Allow', Action: ['s3:PutObject'], Resource: ['*'] }] }, createdAt: '2026-02-01T00:00:00Z', updatedAt: '2026-02-01T00:00:00Z' }
]

async function connect(managePolicies: boolean) {
  const store = useConnectionsStore()
  store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname
    const body = path === '/_gos3/info' ? info : path === '/_gos3/admin/v1/whoami'
      ? { user: 'root', accessKey: 'AK', isRoot: true, policies: [], server: info, capabilities: { ...NO_CAPABILITIES, admin: true, managePolicies } }
      : null
    return new Response(JSON.stringify(body), { status: body ? 200 : 404, headers: { 'content-type': 'application/json' } })
  }))
  await store.refreshStatus('nas')
}

describe('IAM policies page', () => {
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

  it('separates built-in and custom policies in the list', async () => {
    await connect(true)
    vi.spyOn(AdminClient.prototype, 'listPolicies').mockResolvedValue(policies)
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/policies' })
    await vi.waitFor(() => expect(page.text()).toContain('photos-write'))
    expect(page.text()).toContain('readonly')
    expect(page.text()).toContain('Create policy')
    const view = page.findComponent(PoliciesPage)
    const vm = view.vm as unknown as { filter: string }
    vm.filter = 'custom'
    await nextTick()
    expect(page.find('tbody').text()).toContain('photos-write')
    expect(page.find('tbody').text()).not.toContain('readonly')
    page.unmount()
  })

  it('lists policies without edit actions for a read-only administrator', async () => {
    await connect(false)
    vi.spyOn(AdminClient.prototype, 'listPolicies').mockResolvedValue(policies)
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/policies' })
    await vi.waitFor(() => expect(page.text()).toContain('photos-write'))
    expect(page.text()).toContain('Creating or changing policies requires the admin:PutPolicy permission')
    expect(page.text()).not.toContain('Create policy')
    page.unmount()
  })
})
