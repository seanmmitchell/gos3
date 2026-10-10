// Object browser components in the Nuxt runtime.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { describe, expect, it } from 'vitest'
import { ref } from 'vue'
import ObjectTable from '~/components/browser/ObjectTable.vue'
import UploadPanel from '~/components/browser/UploadPanel.vue'
import type { BrowserEntry, BrowserSort } from '~/composables/useBrowserPaths'
import { useBrowserSelection } from '~/composables/useBrowserSelection'
import { useUploads } from '~/composables/useUploads'
import type { Uploader } from '~/composables/useUploads'

const entries: BrowserEntry[] = [
  { type: 'folder', id: 'docs/', key: 'docs/', name: 'docs' },
  { type: 'object', id: 'a.txt', key: 'a.txt', name: 'a.txt', size: 2048, lastModified: '2026-01-01T00:00:00Z', storageClass: 'STANDARD' },
  { type: 'object', id: 'b.png', key: 'b.png', name: 'b.png', size: 10, lastModified: '2026-01-02T00:00:00Z' }
]

describe('BrowserObjectTable', () => {
  it('renders folders and objects with links and sizes', async () => {
    const selection = useBrowserSelection(() => entries.map(e => e.id))
    const w = await mountSuspended(ObjectTable, {
      props: {
        entries,
        selection,
        sort: { by: 'name', desc: false } satisfies BrowserSort,
        linkFor: (e: BrowserEntry) => `/x/${e.key}`
      }
    })
    const links = w.findAll('a').map(a => [a.text(), a.attributes('href')])
    expect(links).toEqual([['docs/', '/x/docs/'], ['a.txt', '/x/a.txt'], ['b.png', '/x/b.png']])
    expect(w.text()).toContain('2 KiB')
    expect(w.text()).toContain('STANDARD')
  })

  it('toggles selection through the checkboxes (with shift ranges and select all)', async () => {
    const selection = useBrowserSelection(() => entries.map(e => e.id))
    const w = await mountSuspended(ObjectTable, {
      props: { entries, selection, sort: { by: 'name', desc: false }, linkFor: () => '/' }
    })
    const box = (label: string) => w.find(`input[aria-label="${label}"]`)
    await box('Select docs').trigger('click')
    await box('Select b.png').trigger('click', { shiftKey: true })
    expect(selection.ids.value.sort()).toEqual(['a.txt', 'b.png', 'docs/'])
    expect(selection.coverage.value).toBe('all')

    await box('Select all listed items').trigger('change')
    expect(selection.count.value).toBe(0)
    await box('Select all listed items').trigger('change')
    expect(selection.count.value).toBe(3)
  })

  it('emits sort changes from the column headers', async () => {
    const sort = ref<BrowserSort>({ by: 'name', desc: false })
    const selection = useBrowserSelection(() => entries.map(e => e.id))
    const w = await mountSuspended(ObjectTable, {
      props: {
        'entries': entries,
        selection,
        'sort': sort.value,
        'linkFor': () => '/',
        'onUpdate:sort': (v: BrowserSort) => (sort.value = v)
      }
    })
    await w.find('button[aria-label^="Size"]').trigger('click')
    expect(sort.value).toEqual({ by: 'size', desc: true })
  })
})

describe('BrowserUploadPanel', () => {
  it('shows queue progress and per-file status', async () => {
    const pending: (() => void)[] = []
    const client: Uploader = {
      putObject: () => new Promise((resolve) => {
        pending.push(() => resolve({ etag: 'e' }))
      }),
      uploadLarge: () => Promise.reject(new Error('unused'))
    }
    const uploads = useUploads()
    uploads.enqueue({ client, connectionId: 'c', bucket: 'bkt', files: [{ file: new File(['abc'], 'one.txt'), key: 'dir/one.txt' }] })
    const w = await mountSuspended(UploadPanel)
    expect(w.text()).toContain('Uploading 1 file')
    expect(w.text()).toContain('bkt/dir/one.txt')
    pending.shift()!()
    await new Promise(r => setTimeout(r, 0))
    await w.vm.$nextTick()
    expect(w.text()).toContain('1 upload complete')
    // A clean batch collapses the file list so the panel covers less of the page.
    expect(w.find('button[aria-label="Expand upload list"]').exists()).toBe(true)
    // The spacer lets the page scroll its last rows above the floating panel.
    expect(w.find('[data-testid="upload-panel-spacer"]').exists()).toBe(true)
    uploads.clearFinished()
    await w.vm.$nextTick()
    expect(w.find('section[aria-label="Uploads"]').exists()).toBe(false)
    expect(w.find('[data-testid="upload-panel-spacer"]').exists()).toBe(false)
  })

  it('keeps the list expanded when an upload failed', async () => {
    const client: Uploader = {
      putObject: () => Promise.reject(new Error('boom')),
      uploadLarge: () => Promise.reject(new Error('unused'))
    }
    const uploads = useUploads()
    uploads.enqueue({ client, connectionId: 'c', bucket: 'bkt', files: [{ file: new File(['x'], 'bad.txt'), key: 'bad.txt' }] })
    const w = await mountSuspended(UploadPanel)
    await new Promise(r => setTimeout(r, 0))
    await w.vm.$nextTick()
    expect(w.text()).toContain('1 upload failed')
    expect(w.find('button[aria-label="Collapse upload list"]').exists()).toBe(true)
    // Only failed uploads left: the clear button dismisses them, so the panel can be closed.
    await w.find('button[aria-label="Dismiss failed uploads"]').trigger('click')
    expect(uploads.items).toHaveLength(0)
    expect(w.find('section[aria-label="Uploads"]').exists()).toBe(false)
  })
})
