import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, type Component } from 'vue'
import ObjectPreview from '~/components/browser/ObjectPreview.vue'
import { S3Client } from '~/utils/s3/client'

const client = new S3Client({ endpoint: 'http://s3.example:9000', region: 'us-east-1', accessKeyId: 'AK', secretAccessKey: 'SK' })
function wrapper(props: Record<string, unknown>) {
  return defineComponent({ setup: () => () => h(UApp, null, { default: () => h(ObjectPreview as Component, props) }) })
}

describe('object preview', () => {
  afterEach(() => vi.restoreAllMocks())

  it('loads a ranged text preview and shows truncation', async () => {
    const read = vi.spyOn(S3Client.prototype, 'getObjectText').mockResolvedValue({ text: 'hello from the object', truncated: true, size: 500000 })
    const page = await mountSuspended(wrapper({ client, bucket: 'photos', objectKey: 'note.txt', size: 500000, contentType: 'text/plain' }))
    await vi.waitFor(() => expect(page.text()).toContain('hello from the object'))
    expect(page.text()).toContain('Showing the first')
    expect(read).toHaveBeenCalledWith('photos', 'note.txt', expect.objectContaining({ maxBytes: expect.any(Number) }))
    page.unmount()
  })

  it('requires an explicit click before loading a very large image', async () => {
    const presign = vi.spyOn(S3Client.prototype, 'presignGetUrl').mockResolvedValue('https://example.test/image.png')
    const page = await mountSuspended(wrapper({ client, bucket: 'photos', objectKey: 'image.png', size: 10_000_000_000, contentType: 'image/png' }))
    expect(page.text()).toContain('Large file')
    expect(presign).not.toHaveBeenCalled()
    await page.findAll('button').find(button => button.text() === 'Load preview anyway')!.trigger('click')
    await vi.waitFor(() => expect(presign).toHaveBeenCalled())
    expect(page.find('img[alt="Preview of image.png"]').attributes('src')).toBe('https://example.test/image.png')
    page.unmount()
  })

  it('explains when an object type has no preview', async () => {
    const read = vi.spyOn(S3Client.prototype, 'getObjectText')
    const page = await mountSuspended(wrapper({ client, bucket: 'photos', objectKey: 'archive.bin', size: 50, contentType: 'application/octet-stream' }))
    expect(page.text()).toContain('No preview available')
    expect(read).not.toHaveBeenCalled()
    page.unmount()
  })
})
