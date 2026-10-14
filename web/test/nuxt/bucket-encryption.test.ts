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

const AES_RULE = '<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>'
const KMS_RULE = '<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>arn:aws:kms:k</KMSMasterKeyID></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>'

interface FakeOptions {
  /** Status of every admin encryption-provider request (403 = S3-only user, 404 = older server). */
  providerStatus?: number
  /** Providers the server reports available. */
  available?: string[]
  /** The stored default encryption document (undefined = none). */
  rule?: string
  /** Answer the provider PUT with this error status. */
  putProviderStatus?: number
}

function fakeServer(opts: FakeOptions = {}) {
  const writes: { path: string, method: string, body: string }[] = []
  let rule = opts.rule
  let provider = 'AES256'
  const whoami: WhoAmI = { user: 'root', accessKey: 'AK', isRoot: true, policies: [], capabilities: { admin: true } as WhoAmI['capabilities'], server: INFO }
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input))
    const url = new URL(req.url)
    const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })
    if (url.pathname === '/_gos3/info') return json(INFO)
    if (url.pathname === '/_gos3/admin/v1/whoami') return json(whoami)
    if (req.method !== 'GET') writes.push({ path: url.pathname, method: req.method, body: await req.clone().text() })
    if (url.pathname === '/_gos3/admin/v1/buckets/photos/encryption-provider') {
      if (opts.providerStatus) return json({ error: { code: opts.providerStatus === 403 ? 'AccessDenied' : 'NotFound', message: 'no' } }, opts.providerStatus)
      if (req.method === 'PUT') {
        if (opts.putProviderStatus) return json({ error: { code: 'InvalidRequest', message: 'Cannot prepare the restic repository of this bucket; the provider was not changed.' } }, opts.putProviderStatus)
        provider = (await req.json()).provider
      }
      return json({ provider, available: opts.available ?? ['AES256', 'restic'], backings: ['default'] })
    }
    if (url.pathname === '/photos/' && url.searchParams.has('encryption')) {
      if (req.method === 'PUT') rule = await req.clone().text()
      if (req.method === 'DELETE') rule = undefined
      if (req.method !== 'GET') return new Response(null, { status: req.method === 'DELETE' ? 204 : 200 })
      return new Response(rule ?? '<Error><Code>ServerSideEncryptionConfigurationNotFoundError</Code></Error>',
        { status: rule ? 200 : 404, headers: { 'content-type': 'application/xml' } })
    }
    return new Response('', { status: 404 })
  }))
  return writes
}

async function mountTab(kind: 'gos3' | 's3' = 'gos3') {
  const store = useConnectionsStore()
  const c = store.add({ name: 'nas', endpoint: 'http://10.9.9.2:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind })
  store.setActive(c.id)
  await store.refreshStatus(c.id)
  const w = await mountSuspended(EncryptionTab, { props: { bucket: 'photos' } })
  await vi.waitFor(() => expect(w.find('[data-testid="default-encryption"]').exists(), w.text()).toBe(true))
  return w
}

type TabVm = {
  mode: string
  provider: string
  dirty: boolean
  providerDirty: boolean
  kmsKeyId: string
  bucketKey: boolean
  save: { run: () => Promise<unknown> }
  saveProvider: { run: () => Promise<unknown> }
}

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  useConnectionsStore().resetState()
})
afterEach(() => vi.unstubAllGlobals())

describe('EncryptionTab', () => {
  it('saves the default rule and the SSE-S3 implementation separately', async () => {
    const writes = fakeServer()
    const w = await mountTab()
    await vi.waitFor(() => expect(w.find('[data-testid="encryption-provider"] form').exists(), w.text()).toBe(true))
    expect(w.text()).not.toContain('SSE-KMS (aws:kms)')
    const vm = w.vm as unknown as TabVm

    vm.provider = 'restic'
    await nextTick()
    expect(vm.providerDirty).toBe(true)
    expect(vm.dirty).toBe(false)
    await vm.saveProvider.run()
    expect(writes).toEqual([{ method: 'PUT', path: '/_gos3/admin/v1/buckets/photos/encryption-provider', body: '{"provider":"restic"}' }])
    await nextTick()
    expect(w.text()).toContain('inherited from this bucket’s placement')
    expect(w.find('input[type="password"]').exists()).toBe(false)

    writes.length = 0
    vm.mode = 'AES256'
    await vm.save.run()
    expect(writes.map(r => `${r.method} ${r.path}`)).toEqual(['PUT /photos/'])
    expect(writes[0]!.body).toContain('<SSEAlgorithm>AES256</SSEAlgorithm>')

    // "None" removes only the default rule; the implementation stays visible.
    writes.length = 0
    vm.mode = 'none'
    await vm.save.run()
    expect(writes).toEqual([{ method: 'DELETE', path: '/photos/', body: '' }])
    await nextTick()
    expect(vm.provider).toBe('restic')
    expect(w.find('[data-testid="encryption-provider"]').text()).toContain('Restic')
    expect(w.emitted('changed')).toHaveLength(3)
  })

  it('keeps the saved implementation when preparation fails', async () => {
    const writes = fakeServer({ putProviderStatus: 400 })
    const w = await mountTab()
    await vi.waitFor(() => expect(w.find('[data-testid="encryption-provider"] form').exists()).toBe(true))
    const vm = w.vm as unknown as TabVm
    vm.provider = 'restic'
    await vm.saveProvider.run()
    await nextTick()
    expect(writes).toHaveLength(1)
    expect(vm.providerDirty).toBe(true)
    expect(w.emitted('changed')).toBeUndefined()
    // The error is reported once (toast), not repeated inline.
    expect(w.text()).not.toContain('provider was not changed')
  })

  it('disables restic when the server reports it unavailable', async () => {
    fakeServer({ available: ['AES256'] })
    const w = await mountTab()
    await vi.waitFor(() => expect(w.find('[data-testid="encryption-provider"] form').exists()).toBe(true))
    expect(w.text()).toContain('restic must be installed on the server')
    const restic = w.findAll('[data-testid="encryption-provider"] input[type="radio"], [data-testid="encryption-provider"] button[role="radio"]')
      .find(el => el.attributes('value') === 'restic')
    expect(restic?.attributes('disabled')).toBeDefined()
  })

  for (const [status, testid] of [[403, 'provider-denied'], [404, 'provider-unsupported']] as const) {
    it(`falls back to S3-only mode on admin ${status}`, async () => {
      const writes = fakeServer({ providerStatus: status })
      const w = await mountTab()
      await vi.waitFor(() => expect(w.find(`[data-testid="${testid}"]`).exists(), w.text()).toBe(true))
      const vm = w.vm as unknown as TabVm
      vm.mode = 'AES256'
      await vm.save.run()
      expect(writes.map(r => `${r.method} ${r.path}`)).toEqual(['PUT /photos/'])
      expect(w.emitted('changed')).toHaveLength(1)
    })
  }

  it('offers to replace a legacy KMS default on gos3 with SSE-S3', async () => {
    const writes = fakeServer({ rule: KMS_RULE })
    const w = await mountTab()
    const vm = w.vm as unknown as TabVm
    expect(w.find('[data-testid="legacy-kms"]').exists()).toBe(true)
    expect(vm.mode).toBe('AES256')
    expect(vm.dirty).toBe(true)
    await vm.save.run()
    expect(writes).toHaveLength(1)
    expect(writes[0]!.body).toContain('<SSEAlgorithm>AES256</SSEAlgorithm>')
    expect(writes[0]!.body).not.toContain('KMSMasterKeyID')
    await nextTick()
    expect(w.find('[data-testid="legacy-kms"]').exists()).toBe(false)
  })

  it('keeps SSE-KMS for generic S3 and ignores fields of other modes', async () => {
    const writes = fakeServer({ rule: AES_RULE })
    const w = await mountTab('s3')
    expect(w.text()).toContain('SSE-KMS (aws:kms)')
    expect(w.find('[data-testid="encryption-provider"]').exists()).toBe(false)
    expect(writes).toHaveLength(0)
    const vm = w.vm as unknown as TabVm
    vm.mode = 'aws:kms'
    await nextTick()
    expect(w.text()).toContain('Key ID or ARN')
    expect(w.text()).toContain('Reduce KMS requests')
    vm.bucketKey = true
    vm.kmsKeyId = '  '
    vm.mode = 'AES256'
    await nextTick()
    expect(vm.dirty).toBe(false)
    vm.mode = 'aws:kms'
    vm.kmsKeyId = ' arn:aws:kms:x '
    await vm.save.run()
    expect(writes[0]!.body).toContain('<KMSMasterKeyID>arn:aws:kms:x</KMSMasterKeyID>')
  })
})
