import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import PolicyDetailPage from '~/pages/servers/[id]/policies/[name].vue'
import type { Info, NamedPolicy, User } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { AdminClient, AdminError } from '~/utils/admin/client'
import { clearClientCache } from '~/utils/clients'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(PolicyDetailPage) }) })
const info: Info = {
  service: 'gos3', id: '0123456789abcdef', name: 'NAS', version: '0.3.0', apiVersion: 'v1',
  region: 'us-east-1', endpoints: { s3: 'http://nas.example:9000', admin: 'http://nas.example:9000/_gos3/admin/v1' },
  features: ['admin'], startedAt: '2026-01-01T00:00:00Z'
}
const policy: NamedPolicy = {
  name: 'photos-write', description: 'Uploads', builtin: false,
  document: { Statement: [{ Effect: 'Allow', Action: ['s3:PutObject'], Resource: ['*'] }] },
  createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-02T00:00:00Z'
}
const alice: User = { name: 'alice', disabled: false, policies: ['photos-write'], keys: [], createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-02T00:00:00Z' }

async function connect(kind: 'gos3' | 's3') {
  const store = useConnectionsStore()
  store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind })
  if (kind === 's3') return
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname
    const body = path === '/_gos3/info' ? info : path === '/_gos3/admin/v1/whoami'
      ? { user: 'root', accessKey: 'AK', isRoot: true, policies: [], server: info, capabilities: { ...NO_CAPABILITIES, admin: true, managePolicies: true, manageUsers: true, listUsers: true } }
      : null
    return new Response(JSON.stringify(body), { status: body ? 200 : 404, headers: { 'content-type': 'application/json' } })
  }))
  await store.refreshStatus('nas')
}

describe('IAM policy detail page', () => {
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

  it('shows a custom policy and its attached users', async () => {
    await connect('gos3')
    vi.spyOn(AdminClient.prototype, 'getPolicy').mockResolvedValue(policy)
    vi.spyOn(AdminClient.prototype, 'listPolicies').mockResolvedValue([policy])
    vi.spyOn(AdminClient.prototype, 'listUsers').mockResolvedValue([alice])
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/policies/photos-write' })
    await vi.waitFor(() => expect(page.text()).toContain('Uploads'))
    expect(page.text()).toContain('Custom')
    expect(page.text()).toContain('alice')
    expect(page.text()).toContain('Delete')
    page.unmount()
  })

  it('treats built-in policies as read only', async () => {
    await connect('gos3')
    vi.spyOn(AdminClient.prototype, 'getPolicy').mockResolvedValue({ ...policy, name: 'readonly', builtin: true })
    vi.spyOn(AdminClient.prototype, 'listPolicies').mockResolvedValue([{ ...policy, name: 'readonly', builtin: true }])
    vi.spyOn(AdminClient.prototype, 'listUsers').mockResolvedValue([])
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/policies/readonly' })
    await vi.waitFor(() => expect(page.text()).toContain('Built-in'))
    expect(page.findAll('button').some(button => button.text() === 'Delete')).toBe(false)
    page.unmount()
  })

  it('reports a missing policy', async () => {
    await connect('gos3')
    vi.spyOn(AdminClient.prototype, 'getPolicy').mockRejectedValue(new AdminError('NotFound', 'missing', 404))
    vi.spyOn(AdminClient.prototype, 'listPolicies').mockResolvedValue([])
    vi.spyOn(AdminClient.prototype, 'listUsers').mockResolvedValue([])
    const missing = await mountSuspended(WithProviders, { route: '/servers/nas/policies/missing' })
    await vi.waitFor(() => expect(missing.text()).toContain('Policy not found'))
    missing.unmount()
  })

  it('avoids admin reads on generic S3', async () => {
    await connect('s3')
    const get = vi.spyOn(AdminClient.prototype, 'getPolicy')
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/policies/photos-write' })
    expect(page.text()).toContain('IAM policies are only available on gos3 servers')
    expect(get).not.toHaveBeenCalled()
    page.unmount()
  })
})
