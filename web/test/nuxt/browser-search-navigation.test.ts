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
