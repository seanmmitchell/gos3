import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, type Component } from 'vue'
import ObjectDetails from '~/components/browser/ObjectDetails.vue'
import type { HeadResult } from '~/types/s3'
import { S3Client } from '~/utils/s3/client'
import { S3Error } from '~/utils/s3/errors'

const client = new S3Client({ endpoint: 'http://s3.example:9000', region: 'us-east-1', accessKeyId: 'AK', secretAccessKey: 'SK' })
const head: HeadResult = {
  contentLength: 2048, contentType: 'text/plain', etag: 'abcdef', versionId: 'v1',
  lastModified: '2026-01-01T00:00:00Z', checksums: { CRC32: 'AAAAAA==' },
  metadata: { owner: 'alice' }, objectLockMode: 'GOVERNANCE', objectLockRetainUntilDate: '2027-01-01T00:00:00Z',
  tagCount: 1, headers: {}
}

function wrapper(props: Record<string, unknown>) {
  return defineComponent({ setup: () => () => h(UApp, null, { default: () => h(ObjectDetails as Component, props) }) })
}

describe('object details slideover', () => {
  afterEach(() => vi.restoreAllMocks())

  it('shows head metadata, retention, checksums, and version identity', async () => {
    const read = vi.spyOn(S3Client.prototype, 'headObject').mockResolvedValue(head)
    const page = await mountSuspended(wrapper({ open: true, client, bucket: 'photos', objectKey: 'docs/readme.txt', versionId: 'v1', versioning: 'Enabled' }), { attachTo: document.body })
    await vi.waitFor(() => expect(document.body.textContent).toContain('2 KiB'))
    expect(document.body.textContent).toContain('readme.txt')
    expect(document.body.textContent).toContain('GOVERNANCE')
    expect(document.body.textContent).toContain('CRC32')
    expect(read).toHaveBeenCalledWith('photos', 'docs/readme.txt', { versionId: 'v1' })
    page.unmount()
  })

  it('recognizes an exact delete marker version', async () => {
    vi.spyOn(S3Client.prototype, 'headObject').mockRejectedValue(new S3Error({ code: 'MethodNotAllowed', status: 405, message: 'delete marker' }))
    const page = await mountSuspended(wrapper({ open: true, client, bucket: 'photos', objectKey: 'removed.txt', versionId: 'marker', versioning: 'Enabled' }), { attachTo: document.body })
    await vi.waitFor(() => expect(document.body.textContent).toContain('delete marker'))
    page.unmount()
  })

  it('redirects a folder key that was opened without its trailing slash', async () => {
    vi.spyOn(S3Client.prototype, 'headObject').mockRejectedValue(new S3Error({ code: 'NoSuchKey', status: 404, message: 'missing' }))
    vi.spyOn(S3Client.prototype, 'listObjectsV2').mockResolvedValue({ name: 'photos', prefix: 'docs/', maxKeys: 1, keyCount: 1, isTruncated: false, commonPrefixes: [], objects: [{ key: 'docs/file.txt', size: 5 }] })
    const folder = vi.fn()
    const page = await mountSuspended(wrapper({ open: true, client, bucket: 'photos', objectKey: 'docs', onFolder: folder }), { attachTo: document.body })
    await vi.waitFor(() => expect(folder).toHaveBeenCalledWith('docs/'))
    page.unmount()
  })
})
