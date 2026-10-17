// gos3 storage backings in components: the CreateBucketModal selector is
// shown only when the server offers a choice, preselects the server default
// and sends it as x-gos3-backing (via CreateBucketOptions.backing). On an s3
// backing it asks for the bucket kind (the backing's default preselected):
// wrapped buckets get a container (always sent: whoami may be stale), native
// buckets a remote bucket name (sent when it differs from the bucket name);
// identities that may adopt remote buckets get a link to the backing page.
// The versioning tab explains instead of offering actions on buckets whose
// storage cannot keep versions.
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
const radios = () => [...document.body.querySelectorAll<HTMLButtonElement>('[role="radio"]')]
const submitButton = () => [...document.body.querySelectorAll<HTMLButtonElement>('button[type="submit"]')].find(b => b.textContent?.includes('Create bucket'))!

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

async function typeInto(selector: string, value: string) {
  const input = document.body.querySelector<HTMLInputElement>(selector)!
  input.value = value
  input.dispatchEvent(new Event('input'))
  await nextTick()
}

async function createNamed(name: string) {
  await typeInto('input[placeholder="my-bucket"]', name)
  document.body.querySelector('#create-bucket-form')!.dispatchEvent(new Event('submit', { cancelable: true }))
  await flushPromises()
}

const CLOUD = {
  name: 'cloud',
  kind: 's3',
  kinds: ['native', 'wrapped'],
  defaultKind: 'wrapped',
  containers: [{ id: 'home', label: 'shared', default: true }, { id: 'c-7f3a', label: 'photos-archive' }]
}

const backings = (def: string): AvailableBackings => ({
  default: def,
  available: [
    { name: 'default', kind: 'fs' },
    CLOUD,
    { name: 'bare', kind: 's3', kinds: ['native', 'wrapped'], defaultKind: 'wrapped', containers: [] }
  ]
})

describe('CreateBucketModal storage backing', () => {
  it('has no selector and sends no backing without a choice', async () => {
    const { client, created } = fakeClient()
    const w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings: { default: 'default', available: [{ name: 'default', kind: 'fs' }] } } as never })
    await flushPromises()
    expect(body()).not.toContain('Storage backing')
    expect(body()).not.toContain('Bucket kind')
    await createNamed('plain-bucket')
    expect(created).toEqual([['plain-bucket', { region: undefined, objectLock: false, backing: undefined }]])
    w.unmount()
    await nextTick()
  })

  it('preselects the server default, its default kind and container, and sends them', async () => {
    const { client, created } = fakeClient()
    const w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings: backings('cloud') } as never })
    await flushPromises()
    expect(body()).toContain('Storage backing')
    expect(body()).toContain('cloud (default)')
    expect(body()).toContain('Bucket kind')
    expect(body()).toContain('Wrapped (default)')
    expect(body()).toContain('gos3 format inside a container; every gos3 feature.')
    expect(body()).toContain('Objects stored as-is in a remote bucket; versioning and object lock follow the remote bucket.')
    expect(body()).toContain('shared (default)')
    // No adopt link without the capability.
    expect(body()).not.toContain('Adopt an existing remote bucket')
    expect(switches().every(s => !s.disabled)).toBe(true)
    await createNamed('remote-bucket')
    expect(created[0]![1]).toMatchObject({ backing: 'cloud', kind: 'wrapped', container: 'home', remoteBucket: undefined })
    w.unmount()
    await nextTick()
  })

  it('creates a native bucket with a remote bucket name that defaults to the bucket name', async () => {
    const { client, created } = fakeClient()
    const w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings: backings('cloud') } as never })
    await flushPromises()
    expect(radios()).toHaveLength(2)
    radios()[1]!.click()
    await flushPromises()
    expect(body()).toContain('Remote bucket')
    expect(body()).toContain('Versioning and object lock are set on the remote bucket')
    // Versioning and object lock stay available (passed through to the remote).
    expect(switches().every(s => !s.disabled)).toBe(true)
    await typeInto('input[placeholder="my-bucket"]', 'photos')
    expect(document.body.querySelector<HTMLInputElement>('input[aria-label="Remote bucket"]')!.value).toBe('photos')
    await createNamed('photos')
    expect(created[0]![1]).toMatchObject({ backing: 'cloud', kind: 'native', remoteBucket: undefined, container: undefined })
    w.unmount()
    await nextTick()
  })

  it('sends an edited remote bucket name and refuses an invalid one', async () => {
    const { client, created } = fakeClient()
    const w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings: backings('cloud') } as never })
    await flushPromises()
    radios()[1]!.click()
    await flushPromises()
    await typeInto('input[placeholder="my-bucket"]', 'photos')
    await typeInto('input[aria-label="Remote bucket"]', 'Bad_Name')
    expect(body()).toContain('Use only lowercase letters')
    expect(submitButton().disabled).toBe(true)
    // The remote name no longer follows the bucket name once edited.
    await typeInto('input[aria-label="Remote bucket"]', 'acme-photos')
    await createNamed('photos2')
    expect(created[0]![1]).toMatchObject({ kind: 'native', remoteBucket: 'acme-photos' })
    w.unmount()
    await nextTick()
  })

  it('explains a wrapped bucket on a backing without a container and offers adopting', async () => {
    const { client, created } = fakeClient()
    const w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings: backings('bare'), serverId: 'nas', canAdopt: true } as never })
    await flushPromises()
    expect(body()).toContain('This backing has no container; an administrator can add one.')
    await createNamed('wrapped-bucket')
    expect(created).toEqual([])
    expect(submitButton().disabled).toBe(true)
    const link = document.body.querySelector<HTMLAnchorElement>('a[href^="/servers/nas/backings/bare"]')!
    expect(link.textContent).toContain('Adopt an existing remote bucket…')
    expect(link.getAttribute('href')).toBe('/servers/nas/backings/bare?tab=remote&adopt=&bucket=wrapped-bucket')
    w.unmount()
    await nextTick()
  })

  it('asks for the kind on a single s3 backing without sending the backing', async () => {
    const { client, created } = fakeClient()
    const only: AvailableBackings = { default: 'cloud', available: [{ ...CLOUD, defaultKind: 'native' }] }
    const w = await mountSuspended(CreateBucketModal, { props: { open: true, client, backings: only } as never })
    await flushPromises()
    expect(body()).not.toContain('Storage backing')
    expect(body()).toContain('Native (default)')
    await createNamed('solo')
    expect(created[0]![1]).toMatchObject({ backing: undefined, kind: 'native' })
    w.unmount()
    await nextTick()
  })
})

describe('BucketSettingsVersioningTab on a bucket that cannot be versioned', () => {
  it('explains instead of offering to enable versioning', async () => {
    const w = await mountSuspended(VersioningTab, { props: { bucket: 'b', status: '', objectLock: false, unsupported: true } })
    expect(w.text()).toContain('Not supported for this bucket')
    expect(w.text()).toContain('native bucket whose remote S3 service does not support versioning')
    expect(w.findAll('button').some(b => b.text().includes('Enable versioning'))).toBe(false)

    await w.setProps({ unsupported: false })
    expect(w.findAll('button').some(b => b.text().includes('Enable versioning'))).toBe(true)
  })
})
