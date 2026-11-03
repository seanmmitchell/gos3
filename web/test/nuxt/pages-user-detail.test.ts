import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import UserDetailPage from '~/pages/servers/[id]/users/[name].vue'
import type { Info, User } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { AdminClient } from '~/utils/admin/client'
import { clearClientCache } from '~/utils/clients'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(UserDetailPage) }) })
const info: Info = {
  service: 'gos3', id: '0123456789abcdef', name: 'NAS', version: '0.3.0', apiVersion: 'v1',
  region: 'us-east-1', endpoints: { s3: 'http://nas.example:9000', admin: 'http://nas.example:9000/_gos3/admin/v1' },
  features: ['admin'], startedAt: '2026-01-01T00:00:00Z'
}
const alice: User = {
  name: 'alice', description: 'Original', disabled: false, policies: ['readonly'],
  keys: [{ accessKey: 'ALICEKEY', status: 'Active', createdAt: '2026-01-02T00:00:00Z' }],
  createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-02T00:00:00Z'
}

async function connect(kind: 'gos3' | 's3') {
  const store = useConnectionsStore()
  store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind })
  if (kind === 's3') return
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname
    const body = path === '/_gos3/info' ? info : path === '/_gos3/admin/v1/whoami'
      ? { user: 'root', accessKey: 'AK', isRoot: true, policies: [], server: info, capabilities: { ...NO_CAPABILITIES, admin: true, manageUsers: true } }
      : null
    return new Response(JSON.stringify(body), { status: body ? 200 : 404, headers: { 'content-type': 'application/json' } })
  }))
  await store.refreshStatus('nas')
}

describe('IAM user detail page', () => {
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

  it('loads a user, saves an edited description, and resets unsaved edits', async () => {
    await connect('gos3')
    vi.spyOn(AdminClient.prototype, 'getUser').mockResolvedValue(alice)
    vi.spyOn(AdminClient.prototype, 'listPolicies').mockResolvedValue([])
    const update = vi.spyOn(AdminClient.prototype, 'updateUser').mockImplementation(async (_name, change) => ({ ...alice, description: change.description ?? alice.description }))
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/users/alice' })
    await vi.waitFor(() => expect(page.text()).toContain('Original'))
    expect(page.text()).toContain('ALICEKEY')
    const description = page.find('input[placeholder="Optional"]')
    await description.setValue('Changed')
    await page.find('form').trigger('submit')
    await vi.waitFor(() => expect(update).toHaveBeenCalledWith('alice', expect.objectContaining({ description: 'Changed' })))
    await vi.waitFor(() => expect(page.text()).toContain('Changed'))
    await description.setValue('Unsaved')
    await page.findAll('button').find(button => button.text() === 'Reset')!.trigger('click')
    expect((description.element as HTMLInputElement).value).toBe('Changed')
    page.unmount()
  })

  it('does not load IAM users from a generic S3 service', async () => {
    await connect('s3')
    const get = vi.spyOn(AdminClient.prototype, 'getUser')
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/users/alice' })
    expect(page.text()).toContain('IAM users are only available on gos3 servers')
    expect(get).not.toHaveBeenCalled()
    page.unmount()
  })
})
