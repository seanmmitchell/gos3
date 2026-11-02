import { mountSuspended } from '@nuxt/test-utils/runtime'
import { flushPromises } from '@vue/test-utils'
import { expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import type { Router } from 'vue-router'
import ObjectBrowser from '~/pages/servers/[id]/buckets/[bucket]/objects/[...path].vue'
import type { BrowserEntry } from '~/composables/useBrowserPaths'
import type { useBrowserSelection } from '~/composables/useBrowserSelection'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import { S3Client } from '~/utils/s3/client'
import { S3Error } from '~/utils/s3/errors'

it('refreshes the folder when navigation leaves the effective search prefix unchanged', async () => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  const store = useConnectionsStore()
  store.resetState()
  store.add({ id: 'search-test', endpoint: 'http://localhost:9000', accessKeyId: 'AK', secretAccessKey: 'SK' })
  vi.spyOn(S3Client.prototype, 'getBucketVersioning').mockResolvedValue({ status: '' })
  const list = vi.spyOn(S3Client.prototype, 'listObjectsV2').mockImplementation(async (_bucket, opts) => ({
    name: 'bucket', prefix: opts?.prefix ?? '', isTruncated: false,
    maxKeys: 1000, keyCount: opts?.prefix === 'docs/' ? 2 : 0,
    objects: opts?.prefix === 'docs/' ? [{ key: 'docs/', size: 0 }, { key: 'docs/file.txt', size: 1 }] : [],
    commonPrefixes: []
  }))
  const w = await mountSuspended(ObjectBrowser, {
    route: '/servers/search-test/buckets/bucket/objects/',
    global: { stubs: {
      BrowserObjectDetails: true, BrowserUploadPanel: true,
      BrowserCreateFolderModal: { inheritAttrs: false, template: '<div />' },
      BrowserShareModal: true, BrowserTransferModal: true, BrowserObjectTable: true
    } }
  })
  try {
    const vm = w.vm as unknown as {
      search: string, searchMode: string, prefix: string, entries: BrowserEntry[],
      selection: ReturnType<typeof useBrowserSelection>, router: Router
    }
    vm.searchMode = 'prefix'
    vm.search = 'docs/'
    await vi.waitFor(() => expect(vm.entries.map(e => e.name)).toEqual(['docs', 'docs/file.txt']))
    vm.selection.toggle('docs/file.txt')
    expect(vm.selection.count.value).toBe(1)
    list.mockClear()

    await vm.router.push('/servers/search-test/buckets/bucket/objects/docs/file.txt')
    await nextTick()
    await flushPromises()
    expect(vm.prefix).toBe('docs/')
    expect(vm.search).toBe('')
    expect(list).toHaveBeenCalledTimes(1)
    expect(list.mock.calls[0]?.[1]?.prefix).toBe('docs/')
    expect(vm.entries.map(e => e.name)).toEqual(['file.txt'])
    expect(vm.selection.count.value).toBe(0)
  } finally {
    w.unmount()
  }
})

it('paginates object listings and switches to version entries', async () => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  const store = useConnectionsStore()
  store.resetState()
  store.add({ id: 'paging-test', endpoint: 'http://localhost:9000', accessKeyId: 'AK', secretAccessKey: 'SK' })
  vi.spyOn(S3Client.prototype, 'getBucketVersioning').mockResolvedValue({ status: 'Enabled' })
  const list = vi.spyOn(S3Client.prototype, 'listObjectsV2').mockImplementation(async (_bucket, opts) => ({
    name: 'bucket', prefix: '', maxKeys: 1000, keyCount: 1, isTruncated: !opts?.continuationToken,
    nextContinuationToken: opts?.continuationToken ? undefined : 'next', commonPrefixes: [],
    objects: [{ key: opts?.continuationToken ? 'second.txt' : 'first.txt', size: 10 }]
  }))
  const versions = vi.spyOn(S3Client.prototype, 'listObjectVersions').mockResolvedValue({
    name: 'bucket', prefix: '', maxKeys: 1000, isTruncated: false, commonPrefixes: [],
    versions: [{ key: 'first.txt', versionId: 'v2', isLatest: true, size: 10, isDeleteMarker: false },
      { key: 'first.txt', versionId: 'v1', isLatest: false, size: 0, isDeleteMarker: true }]
  })
  const page = await mountSuspended(ObjectBrowser, {
    route: '/servers/paging-test/buckets/bucket/objects/',
    global: { stubs: { BrowserObjectDetails: true, BrowserUploadPanel: true,
      BrowserCreateFolderModal: { inheritAttrs: false, template: '<div />' },
      BrowserShareModal: true, BrowserTransferModal: true, BrowserObjectTable: true } }
  })
  try {
    const vm = page.vm as unknown as { entries: BrowserEntry[], listing: { truncated: boolean }, loadMore: () => Promise<void>, showVersions: boolean }
    await vi.waitFor(() => expect(vm.entries.map(e => e.name)).toEqual(['first.txt']))
    expect(vm.listing.truncated).toBe(true)
    await vm.loadMore()
    expect(vm.entries.map(e => e.name)).toEqual(['first.txt', 'second.txt'])
    expect(list.mock.calls.at(-1)?.[1]?.continuationToken).toBe('next')
    vm.showVersions = true
    await nextTick()
    await vi.waitFor(() => expect(versions).toHaveBeenCalled())
    await vi.waitFor(() => expect(vm.entries).toHaveLength(2))
    expect(vm.entries.some(e => e.type === 'object' && e.isDeleteMarker)).toBe(true)
  } finally {
    page.unmount()
  }
})

it('distinguishes an unavailable bucket from a denied object listing', async () => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  const store = useConnectionsStore()
  store.resetState()
  store.add({ id: 'error-test', endpoint: 'http://localhost:9000', accessKeyId: 'AK', secretAccessKey: 'SK' })
  vi.spyOn(S3Client.prototype, 'getBucketVersioning').mockResolvedValue({ status: '' })
  vi.spyOn(S3Client.prototype, 'listObjectsV2').mockRejectedValue(new S3Error({ code: 'NoSuchBucket', status: 404, message: 'missing' }))
  const page = await mountSuspended(ObjectBrowser, {
    route: '/servers/error-test/buckets/missing/objects/',
    global: { stubs: { BrowserObjectDetails: true, BrowserUploadPanel: true,
      BrowserCreateFolderModal: { inheritAttrs: false, template: '<div />' },
      BrowserShareModal: true, BrowserTransferModal: true, BrowserObjectTable: true } }
  })
  try {
    await vi.waitFor(() => expect(page.text()).toContain('Bucket missing does not exist'))
    expect(page.text()).toContain('Back to buckets')
  } finally {
    page.unmount()
  }
})
