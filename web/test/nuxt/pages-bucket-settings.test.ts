import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import BucketSettingsPage from '~/pages/servers/[id]/buckets/[bucket]/settings.vue'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import { S3Client } from '~/utils/s3/client'
import { S3Error } from '~/utils/s3/errors'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(BucketSettingsPage) }) })

describe('bucket settings page', () => {
  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    clearClientCache()
    const store = useConnectionsStore()
    store.resetState()
    store.add({ id: 'remote', name: 'Remote S3', endpoint: 'http://s3.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 's3' })
  })
  afterEach(() => vi.restoreAllMocks())

  it('loads versioning and object-lock state for a bucket', async () => {
    const versioning = vi.spyOn(S3Client.prototype, 'getBucketVersioning').mockResolvedValue({ status: 'Enabled' })
    const lock = vi.spyOn(S3Client.prototype, 'getObjectLockConfiguration').mockResolvedValue({ enabled: true })
    const page = await mountSuspended(WithProviders, { route: '/servers/remote/buckets/photos/settings?tab=versioning' })
    await vi.waitFor(() => expect(page.text()).toContain('Object lock is enabled'))
    expect(page.text()).toContain('Versioned')
    expect(versioning).toHaveBeenCalledWith('photos')
    expect(lock).toHaveBeenCalledWith('photos')
    expect(page.findAll('button').find(button => button.text() === 'Suspend versioning')?.attributes('disabled')).toBeDefined()
    page.unmount()
  })

  it('explains unsupported versioning without failing the rest of the page', async () => {
    vi.spyOn(S3Client.prototype, 'getBucketVersioning').mockRejectedValue(new S3Error({ code: 'NotImplemented', status: 501, message: 'unsupported' }))
    vi.spyOn(S3Client.prototype, 'getObjectLockConfiguration').mockRejectedValue(new S3Error({ code: 'NotImplemented', status: 501, message: 'unsupported' }))
    const page = await mountSuspended(WithProviders, { route: '/servers/remote/buckets/photos/settings?tab=versioning' })
    await vi.waitFor(() => expect(page.text()).toContain('Not supported for this bucket'))
    expect(page.text()).toContain('Versioning cannot be enabled')
    page.unmount()
  })

  it('shows a missing-bucket state when the S3 read returns NoSuchBucket', async () => {
    vi.spyOn(S3Client.prototype, 'getBucketVersioning').mockRejectedValue(new S3Error({ code: 'NoSuchBucket', status: 404, message: 'missing' }))
    vi.spyOn(S3Client.prototype, 'getObjectLockConfiguration').mockResolvedValue(null)
    const page = await mountSuspended(WithProviders, { route: '/servers/remote/buckets/missing/settings' })
    await vi.waitFor(() => expect(page.text()).toContain('Bucket not found'))
    expect(page.text()).toContain('There is no bucket named')
    page.unmount()
  })
})
