import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, type Component } from 'vue'
import PolicyTab from '~/components/bucket-settings/PolicyTab.vue'
import CorsTab from '~/components/bucket-settings/CorsTab.vue'
import AccessTab from '~/components/bucket-settings/AccessTab.vue'
import ObjectLockTab from '~/components/bucket-settings/ObjectLockTab.vue'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import { S3Client } from '~/utils/s3/client'
import { S3Error } from '~/utils/s3/errors'

function withProviders(component: Component, props: Record<string, unknown>) {
  return defineComponent({ setup: () => () => h(UApp, null, { default: () => h(component, props) }) })
}
const route = '/servers/remote/buckets/photos/settings'

describe('bucket settings tabs', () => {
  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    clearClientCache()
    const store = useConnectionsStore()
    store.resetState()
    store.add({ id: 'remote', name: 'Remote S3', endpoint: 'http://s3.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 's3' })
  })
  afterEach(() => vi.restoreAllMocks())

  it('loads an absent bucket policy and applies a template for review', async () => {
    vi.spyOn(S3Client.prototype, 'getBucketPolicy').mockResolvedValue(null)
    vi.spyOn(S3Client.prototype, 'getBucketPolicyStatus').mockResolvedValue({ isPublic: false })
    const page = await mountSuspended(withProviders(PolicyTab, { bucket: 'photos' }), { route })
    await vi.waitFor(() => expect(page.text()).toContain('No bucket policy'))
    await page.findAll('button').find(button => button.text() === 'Use template')!.trigger('click')
    expect(page.text()).toContain('Unsaved')
    expect(page.text()).toContain('Save policy')
    page.unmount()
  })

  it('adds a CORS rule after loading an empty configuration', async () => {
    vi.spyOn(S3Client.prototype, 'getBucketCors').mockResolvedValue([])
    const page = await mountSuspended(withProviders(CorsTab, { bucket: 'photos' }), { route })
    await vi.waitFor(() => expect(page.text()).toContain('No CORS rules'))
    await page.findAll('button').find(button => button.text() === 'Add rule')!.trigger('click')
    expect(page.text()).toContain('Unsaved')
    expect(page.text()).toContain('Save CORS')
    page.unmount()
  })

  it('shows a canned ACL and an unsupported public access block', async () => {
    vi.spyOn(S3Client.prototype, 'getBucketAcl').mockResolvedValue({ canned: 'private', grants: [] })
    vi.spyOn(S3Client.prototype, 'send').mockRejectedValue(new S3Error({ code: 'NotImplemented', status: 501, message: 'unsupported' }))
    const page = await mountSuspended(withProviders(AccessTab, { bucket: 'photos' }), { route })
    await vi.waitFor(() => expect(page.text()).toContain('This server does not implement the public access block API'))
    expect(page.text()).toContain('Access control list')
    page.unmount()
  })

  it('requires versioning before enabling object lock', async () => {
    vi.spyOn(S3Client.prototype, 'getObjectLockConfiguration').mockResolvedValue(null)
    const page = await mountSuspended(withProviders(ObjectLockTab, { bucket: 'photos', versioning: '' }), { route })
    await vi.waitFor(() => expect(page.text()).toContain('Versioning required'))
    expect(page.findAll('button').find(button => button.text() === 'Enable object lock')?.attributes('disabled')).toBeDefined()
    page.unmount()
  })

  it('loads a configured governance retention period', async () => {
    vi.spyOn(S3Client.prototype, 'getObjectLockConfiguration').mockResolvedValue({ enabled: true, rule: { defaultRetention: { mode: 'GOVERNANCE', days: 30 } } })
    const page = await mountSuspended(withProviders(ObjectLockTab, { bucket: 'photos', versioning: 'Enabled' }), { route })
    await vi.waitFor(() => expect(page.text()).toContain('Save default retention'))
    expect(page.text()).toContain('Governance')
    page.unmount()
  })
})
