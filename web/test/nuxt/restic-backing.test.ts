import { mountSuspended } from '@nuxt/test-utils/runtime'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import type { Info, WhoAmI } from '~/types/admin'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import ResticBacking from '~/components/admin/ResticBacking.vue'

const INFO: Info = { service: 'gos3', id: 'b3f1c2d4e5f60718', name: 'nas', version: '0.1.0', apiVersion: 'v1', region: 'us-east-1', endpoints: { s3: 'http://10.9.9.2:9000', admin: 'http://10.9.9.2:9000/_gos3/admin/v1' }, features: [], startedAt: '2026-10-09T12:00:00Z' }
function fakeServer(locked = false, allow = true) {
  const writes: Record<string, unknown>[] = []
  const whoami: WhoAmI = { user: 'root', accessKey: 'AK', isRoot: true, policies: [], capabilities: { admin: true, manageConfig: true } as WhoAmI['capabilities'], server: INFO }
  let doc = { mode: locked ? 's3' : 'local', endpoint: locked ? 'https://s3.example.test' : '', bucket: locked ? 'archives' : '', prefix: '', region: '', locked, credentialsConfigured: locked, sessionTokenConfigured: false }
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input))
    const url = new URL(req.url)
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } })
    if (url.pathname === '/_gos3/info') return json(INFO)
    if (url.pathname === '/_gos3/admin/v1/whoami') return json(whoami)
    if (url.pathname === '/_gos3/admin/v1/backings/default/restic') {
      if (req.method === 'GET') return json(doc)
      const body = await req.json()
      writes.push(body)
      if (!allow) return json({ error: { code: 'InvalidRequest', message: 'Cannot open repository' } }, 400)
      doc = { ...doc, mode: body.mode, endpoint: body.endpoint ?? '', bucket: body.bucket ?? '', prefix: body.prefix ?? '', region: body.region ?? '', credentialsConfigured: !!body.accessKey, sessionTokenConfigured: !!body.sessionToken }
      return json(doc)
    }
    return new Response('', { status: 404 })
  }))
  return writes
}
async function mountTab() {
  const store = useConnectionsStore()
  const c = store.add({ name: 'nas', endpoint: 'http://10.9.9.2:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
  store.setActive(c.id)
  await store.refreshStatus(c.id)
  const w = await mountSuspended(ResticBacking, { props: { backing: 'default' } })
  await vi.waitFor(() => expect(w.find('form').exists()).toBe(true))
  return w
}
type Vm = { form: { mode: string, endpoint: string, bucket: string, accessKey: string, secretKey: string }, save: { run: () => Promise<unknown> } }
beforeEach(() => { localStorage.clear(); sessionStorage.clear(); clearClientCache(); useConnectionsStore().resetState() })
afterEach(() => vi.unstubAllGlobals())
describe('Restic backing settings', () => {
  it('saves S3 location and credentials on the backing and clears secret inputs', async () => {
    const writes = fakeServer()
    const w = await mountTab()
    const vm = w.vm as unknown as Vm
    Object.assign(vm.form, { mode: 's3', endpoint: 'https://s3.example.test', bucket: 'archives', accessKey: 'BACKING_AK', secretKey: 'BACKING_SK' })
    await vm.save.run()
    await nextTick()
    expect(writes).toHaveLength(1)
    expect(writes[0]).toMatchObject({ mode: 's3', endpoint: 'https://s3.example.test', bucket: 'archives', secretKey: 'BACKING_SK' })
    expect(vm.form.accessKey).toBe('')
    expect(vm.form.secretKey).toBe('')
    expect(w.text()).toContain('Configured. Leave blank to retain.')
  })
  it('locks repository location after initialization while allowing credential updates', async () => {
    fakeServer(true)
    const w = await mountTab()
    expect(w.find('input[placeholder="https://s3.example.com"]').attributes('disabled')).toBeDefined()
    expect(w.find('input[type="password"]').attributes('disabled')).toBeUndefined()
    expect(w.text()).toContain('move the buckets using Placement')
  })
  it('shows a refused credential update without reporting success', async () => {
    fakeServer(false, false)
    const w = await mountTab()
    const vm = w.vm as unknown as Vm
    Object.assign(vm.form, { mode: 's3', endpoint: 'https://s3.example.test', bucket: 'archives', accessKey: 'AK', secretKey: 'incorrect' })
    await vm.save.run()
    await nextTick()
    expect(w.text()).toContain('Cannot open repository')
    expect(vm.form.secretKey).toBe('incorrect')
  })
})
