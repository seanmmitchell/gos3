import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, type Component } from 'vue'
import CreateUserModal from '~/components/admin/CreateUserModal.vue'
import CreateFolderModal from '~/components/browser/CreateFolderModal.vue'
import RestoreSnapshotModal from '~/components/bucket-settings/RestoreSnapshotModal.vue'
import type { Info, Job, Snapshot } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { AdminClient, AdminError } from '~/utils/admin/client'
import { S3Client } from '~/utils/s3/client'
import { clearClientCache } from '~/utils/clients'

const info: Info = {
  service: 'gos3', id: '0123456789abcdef', name: 'NAS', version: '0.3.0', apiVersion: 'v1',
  region: 'us-east-1', endpoints: { s3: 'http://nas.example:9000', admin: 'http://nas.example:9000/_gos3/admin/v1' },
  features: ['admin'], startedAt: '2026-01-01T00:00:00Z'
}
const snapshot: Snapshot = { id: 'a'.repeat(64), shortId: 'aaaaaaaa', time: '2026-01-01T00:00:00Z' }
const job: Job = { id: 'job-1', kind: 'restic-restore', state: 'queued', createdAt: '2026-01-01T00:00:00Z' }
const route = '/servers/nas/buckets/photos/settings'

function withProviders(component: Component, props: Record<string, unknown>) {
  return defineComponent({ setup: () => () => h(UApp, null, { default: () => h(component, props) }) })
}

async function connect() {
  const store = useConnectionsStore()
  store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname
    const body = path === '/_gos3/info' ? info : path === '/_gos3/admin/v1/whoami'
      ? { user: 'root', accessKey: 'AK', isRoot: true, policies: [], server: info, capabilities: { ...NO_CAPABILITIES, admin: true, manageUsers: true } }
      : null
    return new Response(JSON.stringify(body), { status: body ? 200 : 404, headers: { 'content-type': 'application/json' } })
  }))
  await store.refreshStatus('nas')
}

describe('administrative and browser dialogs', () => {
  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    clearClientCache()
    useConnectionsStore().resetState()
  })
  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('validates new IAM users and returns the created user', async () => {
    await connect()
    const created = { user: { name: 'carol', disabled: false, policies: [], keys: [], createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-01T00:00:00Z' } }
    const create = vi.spyOn(AdminClient.prototype, 'createUser').mockResolvedValue(created)
    const close = vi.fn()
    const page = await mountSuspended(withProviders(CreateUserModal, { existing: ['alice'], onClose: close }), { route })
    const modal = page.findComponent(CreateUserModal)
    const vm = modal.vm as unknown as { name: string, submit: () => Promise<void> }
    vm.name = 'alice'
    await vm.submit()
    expect(create).not.toHaveBeenCalled()
    vm.name = 'carol'
    await vm.submit()
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ name: 'carol', createKey: true }))
    expect(close).toHaveBeenCalledWith(created)
    page.unmount()
  })

  it('keeps a server-side name conflict visible for correction', async () => {
    await connect()
    vi.spyOn(AdminClient.prototype, 'createUser').mockRejectedValue(new AdminError('Conflict', 'exists', 409))
    const page = await mountSuspended(withProviders(CreateUserModal, {}), { route })
    const vm = page.findComponent(CreateUserModal).vm as unknown as { name: string, nameError: string | undefined, submit: () => Promise<void> }
    vm.name = 'alice'
    await vm.submit()
    expect(vm.nameError).toContain('already exists')
    page.unmount()
  })

  it('creates a nested folder placeholder under the current prefix', async () => {
    await connect()
    const client = new S3Client({ endpoint: 'http://nas.example:9000', region: 'us-east-1', accessKeyId: 'AK', secretAccessKey: 'SK' })
    const put = vi.spyOn(S3Client.prototype, 'putObject').mockResolvedValue({ etag: 'empty' })
    const created = vi.fn()
    const page = await mountSuspended(withProviders(CreateFolderModal, { open: true, client, bucket: 'photos', prefix: 'docs/', onCreated: created }), { route, attachTo: document.body })
    const vm = page.findComponent(CreateFolderModal).vm as unknown as { name: string, submit: () => Promise<void> }
    vm.name = 'reports/2026'
    await vm.submit()
    expect(put).toHaveBeenCalledWith('photos', 'docs/reports/2026/', expect.any(Uint8Array), expect.objectContaining({ contentType: 'application/x-directory' }))
    expect(created).toHaveBeenCalledWith('docs/reports/2026/')
    page.unmount()
  })

  it('rejects an empty folder name and keeps upload failures visible', async () => {
    await connect()
    const client = new S3Client({ endpoint: 'http://nas.example:9000', region: 'us-east-1', accessKeyId: 'AK', secretAccessKey: 'SK' })
    const put = vi.spyOn(S3Client.prototype, 'putObject').mockRejectedValue(new Error('backing unavailable'))
    const page = await mountSuspended(withProviders(CreateFolderModal, { open: true, client, bucket: 'photos', prefix: '' }), { route, attachTo: document.body })
    const vm = page.findComponent(CreateFolderModal).vm as unknown as { name: string, nameError: string | undefined, error: unknown, submit: () => Promise<void> }
    await vm.submit()
    expect(vm.nameError).toContain('Enter a folder name')
    expect(put).not.toHaveBeenCalled()
    vm.name = 'archive'
    await vm.submit()
    expect(put).toHaveBeenCalled()
    expect(vm.error).toBeInstanceOf(Error)
    page.unmount()
  })

  it('starts a snapshot restore into a new bucket', async () => {
    await connect()
    vi.spyOn(S3Client.prototype, 'listBuckets').mockResolvedValue({ buckets: [{ name: 'photos', creationDate: '2026-01-01T00:00:00Z' }] })
    const restore = vi.spyOn(AdminClient.prototype, 'restoreSnapshot').mockResolvedValue(job)
    const close = vi.fn()
    const page = await mountSuspended(withProviders(RestoreSnapshotModal, { bucket: 'photos', snapshot, serverId: 'nas', onClose: close }), { route })
    const vm = page.findComponent(RestoreSnapshotModal).vm as unknown as { form: { bucket: string }, submit: () => Promise<void> }
    await vi.waitFor(() => expect(vm.form.bucket).toBeTruthy())
    await vm.submit()
    expect(restore).toHaveBeenCalledWith('photos', snapshot.id, expect.objectContaining({ target: expect.objectContaining({ bucket: vm.form.bucket }) }))
    expect(close).toHaveBeenCalledWith(job)
    page.unmount()
  })

  it('restores a selected prefix into an existing bucket with merge', async () => {
    await connect()
    vi.spyOn(S3Client.prototype, 'listBuckets').mockResolvedValue({ buckets: [{ name: 'photos', creationDate: '2026-01-01T00:00:00Z' }, { name: 'archive', creationDate: '2026-01-02T00:00:00Z' }] })
    const restore = vi.spyOn(AdminClient.prototype, 'restoreSnapshot').mockResolvedValue(job)
    const page = await mountSuspended(withProviders(RestoreSnapshotModal, { bucket: 'photos', snapshot }), { route })
    const vm = page.findComponent(RestoreSnapshotModal).vm as unknown as {
      form: { target: string, bucket: string, prefix: string, onExisting: string, configs: string },
      submit: () => Promise<void>
    }
    await vi.waitFor(() => expect(vm.form.bucket).toBeTruthy())
    vm.form.target = 'existing'
    vm.form.bucket = 'archive'
    vm.form.prefix = 'docs/'
    vm.form.onExisting = 'merge'
    vm.form.configs = 'none'
    await vm.submit()
    expect(restore).toHaveBeenCalledWith('photos', snapshot.id, expect.objectContaining({ target: expect.objectContaining({ bucket: 'archive' }), prefix: 'docs/' }))
    page.unmount()
  })
})
