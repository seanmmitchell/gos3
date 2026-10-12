// Review fixes (round 3) in object-browser components: the metadata editor
// refuses to save when CORS hides headers (REPLACE would delete them) and
// non-ASCII Content-Disposition types; the transfer modal keeps spaces in
// names and asks before copying when existing objects could not be checked.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { flushPromises, type DOMWrapper } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { nextTick } from 'vue'
import ObjectMetadataEditor from '~/components/browser/ObjectMetadataEditor.vue'
import TransferModal from '~/components/browser/TransferModal.vue'
import type { HeadResult } from '~/types/s3'
import type { S3Client } from '~/utils/s3/client'

const head = (extra: Partial<HeadResult> = {}): HeadResult => ({ contentLength: 3, contentType: 'text/plain', metadata: {}, checksums: {}, headers: {}, ...extra })

function saveButton(w: { findAll: (s: 'button') => DOMWrapper<HTMLButtonElement>[] }) {
  return w.findAll('button').find(b => b.text().includes('Save metadata'))!
}

describe('ObjectMetadataEditor', () => {
  it('cannot save when CORS hid response headers', async () => {
    const copies: unknown[] = []
    const client = { copyObject: async (...a: unknown[]) => copies.push(a), getObjectAcl: async () => ({ grants: [], canned: 'private' }) } as unknown as S3Client
    const w = await mountSuspended(ObjectMetadataEditor, { props: { client, bucket: 'b', objectKey: 'k', head: head({ headersHidden: true }) } })
    expect(w.text()).toContain('Metadata cannot be edited here')
    const cache = w.findAll('input')[1]!
    await cache.setValue('max-age=120')
    expect(saveButton(w).attributes('disabled')).toBeDefined()
    await w.find('form').trigger('submit')
    await flushPromises()
    expect(copies).toHaveLength(0)

    await w.setProps({ head: head() })
    await w.findAll('input')[1]!.setValue('max-age=120')
    expect(w.text()).not.toContain('Metadata cannot be edited here')
    expect(saveButton(w).attributes('disabled')).toBeUndefined()
  })

  it('rejects a Content-Disposition it cannot send', async () => {
    const w = await mountSuspended(ObjectMetadataEditor, { props: { client: {} as S3Client, bucket: 'b', objectKey: 'k', head: head() } })
    await w.findAll('input')[2]!.setValue('pièce-jointe')
    expect(w.text()).toContain('only the filename may contain non-ASCII')
    expect(saveButton(w).attributes('disabled')).toBeDefined()
    await w.findAll('input')[2]!.setValue('attachment; filename="résumé.pdf"')
    expect(w.text()).not.toContain('only the filename may contain non-ASCII')
    expect(saveButton(w).attributes('disabled')).toBeUndefined()
  })
})

const body = () => document.body.textContent ?? ''
const bodyButton = (label: string) => [...document.body.querySelectorAll('button')].find(b => b.textContent?.trim() === label) as HTMLButtonElement | undefined

describe('TransferModal', () => {
  it('keeps spaces in names: renaming "notes.txt " unchanged is refused', async () => {
    const client = { listBuckets: async () => ({ buckets: [] }) } as unknown as S3Client
    const w = await mountSuspended(TransferModal, { props: { open: true, client, bucket: 'b', targets: [{ key: 'notes.txt ', isFolder: false }], mode: 'rename', fromPrefix: '' } as never })
    await flushPromises()
    await nextTick()
    const input = document.body.querySelector('input') as HTMLInputElement
    expect(input.value).toBe('notes.txt ')
    expect(body()).toContain('The new name is the same as the current one')
    expect(bodyButton('Rename')?.disabled).toBe(true)
    w.unmount()
    await nextTick()
  })

  it('asks before copying when existing destinations could not be checked', async () => {
    const copied: string[] = []
    const client = {
      listBuckets: async () => ({ buckets: [{ name: 'b' }] }),
      headObject: async () => {
        throw Object.assign(new Error('Forbidden'), { status: 403 })
      },
      copyObject: async (_s: unknown, dst: { key: string }) => {
        copied.push(dst.key)
        return { etag: 'e' }
      }
    } as unknown as S3Client
    const w = await mountSuspended(TransferModal, { props: { open: true, client, bucket: 'b', targets: [{ key: 'src/a.txt', isFolder: false }], mode: 'copy', fromPrefix: 'src/' } as never })
    await flushPromises()
    const input = [...document.body.querySelectorAll('input')].find(i => i.placeholder === '(bucket root)') as HTMLInputElement
    input.value = 'dst/'
    input.dispatchEvent(new Event('input'))
    await nextTick()
    bodyButton('Copy')!.click()
    await flushPromises()
    expect(body()).toContain('Existing destination objects could not all be checked')
    expect(copied).toEqual([])
    bodyButton('Copy and overwrite')!.click()
    await flushPromises()
    expect(copied).toEqual(['dst/a.txt'])
    w.unmount()
    await nextTick()
  })
})
