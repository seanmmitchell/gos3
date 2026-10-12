// Components changed by the security audit fixes: the import preview (web-3),
// self-service key creation and probing gated on their capabilities (C2/C3)
// and admin resources in the simulator (C4).
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import type { Capabilities, Info, WhoAmI } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import ImportModal from '~/components/servers/ImportModal.vue'
import MyAccessKeys from '~/components/admin/MyAccessKeys.vue'
import SimulatorPanel from '~/components/admin/SimulatorPanel.vue'
import PolicyEditor from '~/components/admin/PolicyEditor.vue'

const INFO: Info = {
  service: 'gos3',
  id: 'b3f1c2d4e5f60718',
  name: 'nas',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.9.9.1:9000', admin: 'http://10.9.9.1:9000/_gos3/admin/v1' },
  features: [],
  startedAt: '2026-10-09T12:00:00Z'
}

interface Seen { method: string, path: string, body: string }

/** Fake gos3 at 10.9.9.1:9000 answering whoami with `caps` (non-root user alice). */
function fakeServer(caps: Partial<Capabilities> & Record<string, unknown>) {
  const seen: Seen[] = []
  const whoami = (): WhoAmI => ({ user: 'alice', accessKey: 'AKALICE', isRoot: false, policies: [], capabilities: caps as Capabilities, server: INFO })
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input))
    const url = new URL(req.url)
    seen.push({ method: req.method, path: url.pathname, body: req.method === 'GET' ? '' : await req.text() })
    const json = (v: unknown) => new Response(JSON.stringify(v), { status: 200, headers: { 'content-type': 'application/json' } })
    if (url.host !== '10.9.9.1:9000') throw new TypeError('Failed to fetch')
    switch (url.pathname) {
      case '/_gos3/info': return json(INFO)
      case '/_gos3/admin/v1/whoami': return json(whoami())
      case '/_gos3/admin/v1/whoami/keys': return json({ keys: [{ accessKey: 'AKALICE', status: 'Active', createdAt: '2026-01-01T00:00:00Z' }, { accessKey: 'AKOLD', status: 'Active', createdAt: '2026-01-02T00:00:00Z' }] })
      case '/_gos3/admin/v1/authz/simulate': return json({ allowed: true, explicitDeny: false, reason: 'allowed by policy team' })
    }
    return new Response('', { status: 404 })
  })
  vi.stubGlobal('fetch', fetch)
  return { seen }
}

async function connect() {
  const store = useConnectionsStore()
  const c = store.add({ name: 'nas', endpoint: 'http://10.9.9.1:9000', accessKeyId: 'AKALICE', secretAccessKey: 'SK', kind: 'gos3' })
  store.setActive(c.id)
  await store.refreshStatus(c.id)
  return c
}

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  useConnectionsStore().resetState()
})
afterEach(() => {
  vi.unstubAllGlobals()
})

describe('MyAccessKeys (admin:CreateOwnAccessKey)', () => {
  it('offers "Create key" when createOwnKeys is allowed', async () => {
    fakeServer({ ...NO_CAPABILITIES, createOwnKeys: true })
    await connect()
    const w = await mountSuspended(MyAccessKeys)
    await vi.waitFor(() => expect(w.text()).toContain('AKOLD'))
    expect(w.findAll('button').some(b => b.text() === 'Create key')).toBe(true)
    expect(w.find('[data-testid="create-own-key-denied"]').exists()).toBe(false)
  })

  it('explains admin:CreateOwnAccessKey and keeps listing/deleting when not allowed', async () => {
    fakeServer({ ...NO_CAPABILITIES, createOwnKeys: false })
    await connect()
    const w = await mountSuspended(MyAccessKeys)
    await vi.waitFor(() => expect(w.text()).toContain('AKOLD'))
    expect(w.findAll('button').some(b => b.text() === 'Create key')).toBe(false)
    expect(w.find('[data-testid="create-own-key-denied"]').text()).toContain('admin:CreateOwnAccessKey')
    expect(w.find('button[aria-label="Delete access key AKOLD"]').attributes('disabled')).toBeUndefined()
  })

  it('treats a whoami without createOwnKeys (older server) as allowed', async () => {
    const { createOwnKeys: _c, probe: _p, ...old } = { ...NO_CAPABILITIES }
    fakeServer(old)
    await connect()
    const w = await mountSuspended(MyAccessKeys)
    await vi.waitFor(() => expect(w.text()).toContain('AKOLD'))
    expect(w.findAll('button').some(b => b.text() === 'Create key')).toBe(true)
  })
})

describe('SimulatorPanel (admin resources)', () => {
  it('sends an admin resource instead of bucket/key for admin actions', async () => {
    const { seen } = fakeServer({ ...NO_CAPABILITIES, admin: true, manageUsers: true })
    await connect()
    const w = await mountSuspended(SimulatorPanel, { props: { fixedUser: 'helper' } })
    expect(w.find('input[placeholder="photos"]').exists()).toBe(true)
    const vm = w.vm as unknown as { action: string, showConditions: boolean }
    vm.action = 'admin:UpdateUser'
    vm.showConditions = true
    await nextTick()
    expect(w.find('input[placeholder="photos"]').exists()).toBe(false)
    const input = w.find('input[placeholder="arn:gos3:admin:::user/alice"]')
    expect(input.exists()).toBe(true)
    expect(w.text()).toContain('arn:gos3:admin:::user/<name>')
    await vi.waitFor(() => expect(w.text()).toContain('admin:UpdateUser also sets admin:PolicyName'))

    await input.setValue('arn:gos3:admin:::thing/x')
    await w.find('form').trigger('submit')
    await nextTick()
    expect(w.text()).toContain('Use arn:gos3:admin:::user/<name>')
    expect(seen.some(s => s.path.endsWith('/authz/simulate'))).toBe(false)

    await input.setValue('arn:gos3:admin:::user/team-a')
    await w.find('form').trigger('submit')
    await vi.waitFor(() => expect(w.text()).toContain('Allowed'))
    const sent = JSON.parse(seen.find(s => s.path.endsWith('/authz/simulate'))!.body) as Record<string, unknown>
    expect(sent).toEqual({ user: 'helper', action: 'admin:UpdateUser', resource: 'arn:gos3:admin:::user/team-a' })
    expect(w.text()).toContain('helper → admin:UpdateUser on arn:gos3:admin:::user/team-a')
  })

  it('sends bucket/key and no resource for s3 actions', async () => {
    const { seen } = fakeServer({ ...NO_CAPABILITIES, admin: true, manageUsers: true })
    await connect()
    const w = await mountSuspended(SimulatorPanel, { props: { fixedUser: 'helper' } })
    const vm = w.vm as unknown as { action: string, resource: string }
    vm.resource = 'arn:gos3:admin:::user/stale' // left over from an admin action
    await w.find('input[placeholder="photos"]').setValue('photos')
    await w.find('input[placeholder="2024/cat.jpg"]').setValue('cat.jpg')
    await w.find('form').trigger('submit')
    await vi.waitFor(() => expect(w.text()).toContain('Allowed'))
    const sent = JSON.parse(seen.find(s => s.path.endsWith('/authz/simulate'))!.body) as Record<string, unknown>
    expect(sent).toEqual({ user: 'helper', action: 's3:GetObject', bucket: 'photos', key: 'cat.jpg' })
  })

  it('offers the new admin actions', async () => {
    fakeServer({ ...NO_CAPABILITIES, admin: true })
    await connect()
    const w = await mountSuspended(SimulatorPanel)
    const items = (w.vm as unknown as { actionItems: string[] }).actionItems
    expect(items).toEqual(expect.arrayContaining(['admin:ProbeNetwork', 'admin:CreateOwnAccessKey', 'admin:DeleteOwnAccessKey']))
  })
})

describe('PolicyEditor', () => {
  it('lists the admin resource forms and admin:PolicyName', async () => {
    fakeServer({ ...NO_CAPABILITIES, admin: true, managePolicies: true })
    await connect()
    const w = await mountSuspended(PolicyEditor, { props: { mode: 'create' } })
    const toggle = w.findAll('button').find(b => b.text() === 'Admin resources and condition keys')!
    await toggle.trigger('click')
    await vi.waitFor(() => expect(w.find('[data-testid="admin-resource-help"]').exists()).toBe(true))
    const help = w.find('[data-testid="admin-resource-help"]').text()
    for (const arn of ['arn:gos3:admin:::user/<name>', 'arn:gos3:admin:::policy/<name>', 'arn:gos3:admin:::bucket/<name>', 'admin:PolicyName']) expect(help).toContain(arn)
  })
})

describe('ImportModal preview', () => {
  it('lists what each entry does, including connections kept instead of retargeted', async () => {
    const store = useConnectionsStore()
    store.add({ id: 'nas', name: 'NAS', endpoint: 'http://10.0.0.5:9000', accessKeyId: 'AK', secretAccessKey: 'SECRET' })
    store.add({ id: 'lab', name: 'Lab', endpoint: 'http://lab:9000' })
    const w = await mountSuspended(ImportModal, { props: { open: true }, attachTo: document.body })
    const textarea = () => document.body.querySelector('textarea') as HTMLTextAreaElement
    await vi.waitFor(() => expect(textarea()).toBeTruthy())
    textarea().value = JSON.stringify({ connections: [
      { id: 'nas', endpoint: 'http://attacker:9000' },
      { id: 'lab', endpoint: 'http://lab:9000', name: 'Lab 2' },
      { endpoint: 'http://new:9000', name: 'New' }
    ] })
    textarea().dispatchEvent(new Event('input'))
    await vi.waitFor(() => expect(document.body.querySelector('[aria-label="Import preview"]')).toBeTruthy())
    const preview = document.body.querySelector('[aria-label="Import preview"]')!.textContent ?? ''
    expect(document.body.textContent).toContain('3 servers found.')
    expect(preview).toContain('Add http://attacker:9000 without credentials as a separate server')
    expect(preview).toContain('"NAS" points to http://10.0.0.5:9000; it keeps its endpoint and stored credentials')
    expect(preview).toContain('Update Lab (http://lab:9000)')
    expect(preview).toContain('name: Lab → Lab 2')
    expect(preview).toContain('Add New (http://new:9000) without credentials')
    expect(preview).not.toContain('SECRET')
    // nothing changed before confirming
    expect(store.byId('lab')?.name).toBe('Lab')
    w.unmount()
  })
})
