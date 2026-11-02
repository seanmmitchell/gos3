import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import BucketsPage from '~/pages/servers/[id]/buckets/index.vue'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import { S3Client } from '~/utils/s3/client'
import { S3Error } from '~/utils/s3/errors'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(BucketsPage) }) })

describe('bucket list page', () => {
  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    clearClientCache()
    const store = useConnectionsStore()
    store.resetState()
    store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 's3' })
  })

  afterEach(() => vi.restoreAllMocks())

  it('lists S3 buckets and filters the visible rows without another request', async () => {
    const list = vi.spyOn(S3Client.prototype, 'listBuckets').mockResolvedValue({ buckets: [
      { name: 'z-logs', creationDate: '2026-01-01T00:00:00Z' },
      { name: 'a-photos', creationDate: '2026-02-01T00:00:00Z' }
    ] })
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/buckets' })
    await vi.waitFor(() => expect(page.text()).toContain('a-photos'))
    expect(page.text()).toContain('z-logs')
    expect(page.findAll('tbody tr').map(row => row.text())).toEqual(expect.arrayContaining([expect.stringContaining('a-photos'), expect.stringContaining('z-logs')]))

    const beforeFilter = list.mock.calls.length
    await page.find('input[aria-label="Filter buckets by name"]').setValue('photos')
    expect(page.text()).toContain('a-photos')
    expect(page.text()).not.toContain('z-logs')
    expect(list).toHaveBeenCalledTimes(beforeFilter)
    page.unmount()
  })

  it('offers direct bucket access when ListBuckets is denied and validates its name', async () => {
    vi.spyOn(S3Client.prototype, 'listBuckets').mockRejectedValue(new S3Error({ code: 'AccessDenied', status: 403, message: 'denied' }))
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/buckets' })
    await vi.waitFor(() => expect(page.text()).toContain('Open a bucket by name'))
    const input = page.find('input[placeholder="bucket-name"]')
    await input.setValue('Bad_Name')
    expect(page.text()).toContain('bucket')
    expect(page.find('form button[type="submit"]').attributes('disabled')).toBeDefined()
    await input.setValue('private-photos')
    expect(page.find('form button[type="submit"]').attributes('disabled')).toBeUndefined()
    page.unmount()
  })
})
