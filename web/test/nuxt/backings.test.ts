// gos3 storage backings in components: the CreateBucketModal selector is
// shown only when the server offers a choice, preselects the server default,
// sends it as x-gos3-backing (via CreateBucketOptions.backing) and turns
// versioning and object lock off for gateway backings; the versioning tab
// explains instead of offering actions on gateway buckets.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { flushPromises } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { nextTick } from 'vue'
import CreateBucketModal from '~/components/browser/CreateBucketModal.vue'
import VersioningTab from '~/components/bucket-settings/VersioningTab.vue'
import type { AvailableBackings } from '~/types/admin'
import type { CreateBucketOptions } from '~/types/s3'
import type { S3Client } from '~/utils/s3/client'

const body = () => document.body.textContent ?? ''
const switches = () => [...document.body.querySelectorAll<HTMLButtonElement>('[role="switch"]')]

function fakeClient() {
  const created: [string, CreateBucketOptions][] = []
  const client = {
    createBucket: async (name: string, opts: CreateBucketOptions) => {
      created.push([name, opts])
    },
    putBucketVersioning: async () => {}
  } as unknown as S3Client
  return { client, created }
}

async function createNamed(name: string) {
  const input = document.body.querySelector<HTMLInputElement>('input[placeholder="my-bucket"]')!
  input.value = name
  input.dispatchEvent(new Event('input'))
  await nextTick()
  document.body.querySelector('#create-bucket-form')!.dispatchEvent(new Event('submit', { cancelable: true }))
  await flushPromises()
}

const backings = (def: string): AvailableBackings => ({
  default: def,
  available: [{ name: 'default', kind: 'fs' }, { name: 'aws', kind: 's3', mode: 'gateway' }, { name: 'archive', kind: 's3', mode: 'blob' }]
})

describe('CreateBucketModal storage backing', () => {
  it('has no selector and sends no backing without a choice', async () => {
    const { client, created } = fakeClient()
    const w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings: { default: 'default', available: [{ name: 'default', kind: 'fs' }] } } as never })
    await flushPromises()
    expect(body()).not.toContain('Storage backing')
    await createNamed('plain-bucket')
    expect(created).toEqual([['plain-bucket', { region: undefined, objectLock: false, backing: undefined }]])
    w.unmount()
    await nextTick()
  })

  it('preselects the server default and sends it', async () => {
    const { client, created } = fakeClient()
    const w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings: backings('archive') } as never })
    await flushPromises()
    expect(body()).toContain('Storage backing')
    expect(body()).toContain('archive (default)')
    expect(switches().every(s => !s.disabled)).toBe(true)
    await createNamed('blob-bucket')
    expect(created[0]![1].backing).toBe('archive')
    w.unmount()
    await nextTick()
  })

  it('disables versioning and object lock for a gateway backing', async () => {
    const { client, created } = fakeClient()
    const w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings: backings('aws') } as never })
    await flushPromises()
    expect(body()).toContain('versioning and object lock are not available')
    expect(switches()).toHaveLength(2)
    expect(switches().every(s => s.disabled)).toBe(true)
    await createNamed('gateway-bucket')
    expect(created[0]![1]).toMatchObject({ backing: 'aws', objectLock: false })
    w.unmount()
    await nextTick()
  })
})

describe('BucketSettingsVersioningTab on a gateway bucket', () => {
  it('explains instead of offering to enable versioning', async () => {
    const w = await mountSuspended(VersioningTab, { props: { bucket: 'b', status: '', objectLock: false, unsupported: true } })
    expect(w.text()).toContain('Not available on a gateway backing')
    expect(w.text()).toContain('Not supported')
    expect(w.findAll('button').some(b => b.text().includes('Enable versioning'))).toBe(false)

    await w.setProps({ unsupported: false })
    expect(w.findAll('button').some(b => b.text().includes('Enable versioning'))).toBe(true)
  })
})
