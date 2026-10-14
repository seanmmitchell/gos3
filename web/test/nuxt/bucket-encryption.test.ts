import { mountSuspended } from '@nuxt/test-utils/runtime'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import type { Info, WhoAmI } from '~/types/admin'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import EncryptionTab from '~/components/bucket-settings/EncryptionTab.vue'

const INFO: Info = {
  service: 'gos3', id: 'b3f1c2d4e5f60718', name: 'nas', version: '0.1.0', apiVersion: 'v1',
  region: 'us-east-1', endpoints: { s3: 'http://10.9.9.2:9000', admin: 'http://10.9.9.2:9000/_gos3/admin/v1' },
  features: [], startedAt: '2026-10-09T12:00:00Z'
}

function fakeServer(ready = true) {
  const writes: { path: string, method: string, body: string }[] = []
  let encrypted = false
  let provider = { provider: 'AES256' }
  const whoami: WhoAmI = { user: 'root', accessKey: 'AK', isRoot: true, policies: [], capabilities: { admin: true } as WhoAmI['capabilities'], server: INFO }
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input))
    const url = new URL(req.url)
    const json = (v: unknown) => new Response(JSON.stringify(v), { headers: { 'content-type': 'application/json' } })
    if (url.pathname === '/_gos3/info') return json(INFO)
    if (url.pathname === '/_gos3/admin/v1/whoami') return json(whoami)
    if (req.method !== 'GET') writes.push({ path: url.pathname, method: req.method, body: await req.clone().text() })
    if (url.pathname === '/_gos3/admin/v1/buckets/photos/encryption-provider') {
      if (req.method === 'GET') return json(provider)
      const body = await req.json()
      provider = { provider: body.provider }
      return json({ ...provider, ready, error: ready ? undefined : 'Provider settings saved, but repository preparation failed.' })
    }
    if (url.pathname === '/photos/' && url.searchParams.has('encryption')) {
      if (req.method === 'PUT') encrypted = true
      if (req.method === 'DELETE') encrypted = false
      return new Response(encrypted
        ? '<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>'
        : '<Error><Code>ServerSideEncryptionConfigurationNotFoundError</Code></Error>',
      { status: encrypted || req.method === 'DELETE' ? 200 : 404, headers: { 'content-type': 'application/xml' } })
    }
    return new Response('', { status: 404 })
  }))
  return writes
}

async function mountTab() {
  const store = useConnectionsStore()
  const c = store.add({ name: 'nas', endpoint: 'http://10.9.9.2:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
  store.setActive(c.id)
  await store.refreshStatus(c.id)
  const w = await mountSuspended(EncryptionTab, { props: { bucket: 'photos' } })
  await vi.waitFor(() => expect(w.find('form').exists(), w.text()).toBe(true))
  return w
}

type TabVm = {
  mode: string
  save: { run: () => Promise<unknown> }
}

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  useConnectionsStore().resetState()
})
afterEach(() => vi.unstubAllGlobals())

describe('EncryptionTab', () => {
  it('saves only the restic provider and enables standard S3 AES256', async () => {
    const writes = fakeServer()
    const w = await mountTab()
    expect(w.text()).not.toContain('SSE-KMS (aws:kms)')
    const vm = w.vm as unknown as TabVm
    vm.mode = 'restic'
    await vm.save.run()
    await nextTick()
    expect(writes.map(r => r.path)).toEqual(['/_gos3/admin/v1/buckets/photos/encryption-provider', '/photos/'])
    expect(JSON.parse(writes[0]!.body)).toEqual({ provider: 'restic' })
    expect(writes[1]!.body).toContain('<SSEAlgorithm>AES256</SSEAlgorithm>')
    expect(vm.mode).toBe('restic')
    expect(w.text()).toContain('inherited from this bucket’s placement')
    expect(w.find('input[type="password"]').exists()).toBe(false)
    expect(w.emitted('changed')).toHaveLength(1)
  })

  it('shows preparation failure and does not enable default SSE', async () => {
    const writes = fakeServer(false)
    const w = await mountTab()
    const vm = w.vm as unknown as TabVm
    vm.mode = 'restic'
    await vm.save.run()
    await nextTick()
    expect(writes).toHaveLength(1)
    expect(w.text()).toContain('repository preparation failed')
    expect(w.emitted('changed')).toBeUndefined()
  })

  it('disables only the S3 default, preserving provider keys and archive settings', async () => {
    const writes = fakeServer()
    const w = await mountTab()
    const vm = w.vm as unknown as TabVm
    vm.mode = 'AES256'
    await vm.save.run()
    writes.length = 0
    vm.mode = 'none'
    await vm.save.run()
    expect(writes).toEqual([{ method: 'DELETE', path: '/photos/', body: '' }])
  })
})
