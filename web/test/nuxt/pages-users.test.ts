import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import UsersPage from '~/pages/servers/[id]/users/index.vue'
import type { Info, User } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { AdminClient } from '~/utils/admin/client'
import { clearClientCache } from '~/utils/clients'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(UsersPage) }) })
const info: Info = {
  service: 'gos3', id: '0123456789abcdef', name: 'NAS', version: '0.3.0', apiVersion: 'v1',
  region: 'us-east-1', endpoints: { s3: 'http://nas.example:9000', admin: 'http://nas.example:9000/_gos3/admin/v1' },
  features: ['admin'], startedAt: '2026-01-01T00:00:00Z'
}
const users: User[] = [
  { name: 'alice', description: 'Photo uploads', disabled: false, policies: ['photos-write'], keys: [], createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-01T00:00:00Z' },
  { name: 'bob', disabled: true, policies: ['reports-read'], keys: [], createdAt: '2026-01-02T00:00:00Z', updatedAt: '2026-01-02T00:00:00Z' }
]

describe('IAM users page', () => {
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

  it('lists managed users and filters by policy without another API request', async () => {
    const store = useConnectionsStore()
    store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname
      const body = path === '/_gos3/info' ? info : path === '/_gos3/admin/v1/whoami'
        ? { user: 'root', accessKey: 'AK', isRoot: true, policies: [], server: info, capabilities: { ...NO_CAPABILITIES, admin: true, manageUsers: true } }
        : null
      return new Response(JSON.stringify(body), { status: body ? 200 : 404, headers: { 'content-type': 'application/json' } })
    }))
    await store.refreshStatus('nas')
    const listUsers = vi.spyOn(AdminClient.prototype, 'listUsers').mockResolvedValue(users)
    vi.spyOn(AdminClient.prototype, 'listPolicies').mockResolvedValue([])
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/users' })
    await vi.waitFor(() => expect(page.text()).toContain('alice'))
    expect(page.text()).toContain('bob')
    expect(page.text()).toContain('Disabled')
    const beforeFilter = listUsers.mock.calls.length
    await page.find('input[aria-label="Filter users"]').setValue('photos-write')
    expect(page.text()).toContain('alice')
    expect(page.text()).not.toContain('bob')
    expect(listUsers).toHaveBeenCalledTimes(beforeFilter)
    page.unmount()
  })

  it('explains that generic S3 identities are managed by their own service', async () => {
    const store = useConnectionsStore()
    store.add({ id: 'remote', name: 'Remote', endpoint: 'http://remote.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 's3' })
    const listUsers = vi.spyOn(AdminClient.prototype, 'listUsers')
    const page = await mountSuspended(WithProviders, { route: '/servers/remote/users' })
    expect(page.text()).toContain('Not available for this server')
    expect(page.text()).toContain('Generic S3 services manage identities')
    expect(listUsers).not.toHaveBeenCalled()
    page.unmount()
  })
})
