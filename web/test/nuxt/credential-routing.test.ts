// Credential routing in the browser (security audit: web-auth-1, web-auth-4 /
// web-content-4, web-auth-2 / discovery-4): verified keys stay with the
// endpoint they were verified against, link prefills are pointed out and
// confirmed, and reusing a connection's keys for a discovered host needs an
// explicit confirmation naming that host.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import type { Info, WhoAmI } from '~/types/admin'
import type { Connection } from '~/types/connection'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import ConnectPage from '~/pages/connect.vue'
import FormModal from '~/components/servers/FormModal.vue'

const INFO: Info = {
  service: 'gos3',
  id: 'prod-server-id',
  name: 'prod',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.9.9.1:9000', admin: 'http://10.9.9.1:9000/_gos3/admin/v1' },
  features: [],
  startedAt: '2026-10-09T12:00:00Z'
}

/** Fake gos3 "prod" at 10.9.9.1:9000; every other host is unreachable. Records the hosts contacted. */
function fakeProd() {
  const hosts: string[] = []
  const whoami: WhoAmI = { user: 'root', accessKey: 'ROOTKEY', isRoot: true, policies: [], capabilities: { admin: true } as WhoAmI['capabilities'], server: INFO }
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(input instanceof Request ? input.url : String(input))
    hosts.push(url.host)
    const json = (v: unknown) => new Response(JSON.stringify(v), { status: 200, headers: { 'content-type': 'application/json' } })
    if (url.host !== '10.9.9.1:9000') throw new TypeError('Failed to fetch')
    if (url.pathname === '/_gos3/info') return json(INFO)
    if (url.pathname === '/_gos3/admin/v1/whoami') return json(whoami)
    return new Response('', { status: 404 })
  })
  vi.stubGlobal('fetch', fetch)
  return { hosts }
}

interface ConnectVm {
  model: { endpoint: string, accessKeyId: string, secretAccessKey: string }
  trustPrefill: boolean
  submit: () => Promise<void>
}

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  useConnectionsStore().resetState()
})
afterEach(() => {
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('/connect: new server', () => {
  it('stores verified keys on a new connection, never on an entry that only claims the server ID (web-auth-1)', async () => {
    const { hosts } = fakeProd()
    const store = useConnectionsStore()
    // Attacker entry (import file / spoofed peer) claiming prod's ID, no keys.
    store.add({ id: 'evil', name: 'prod', endpoint: 'http://evil.example:9000', serverId: INFO.id })
    const w = await mountSuspended(ConnectPage, { route: '/connect?new=1' })
    const vm = w.vm as unknown as ConnectVm
    vm.model.endpoint = 'http://10.9.9.1:9000'
    vm.model.accessKeyId = 'ROOTKEY'
    vm.model.secretAccessKey = 'ROOTSECRET'
    await nextTick()
    await vm.submit()

    const evil = store.byId('evil')!
    expect(evil.accessKeyId).toBe('')
    expect(evil.secretAccessKey).toBe('')
    const prod = store.connections.find(c => c.id !== 'evil')!
    expect(prod).toMatchObject({ endpoint: 'http://10.9.9.1:9000', accessKeyId: 'ROOTKEY', secretAccessKey: 'ROOTSECRET', serverId: INFO.id })
    expect(hosts).not.toContain('evil.example:9000')
  })

  it('points out a link prefill and requires confirming an unknown host (web-auth-4)', async () => {
    const { hosts } = fakeProd()
    const w = await mountSuspended(ConnectPage, { route: '/connect?new=1&endpoint=http://gos3-corp.attacker.example:9000&name=gos3.corp' })
    const vm = w.vm as unknown as ConnectVm
    const alert = w.find('[data-testid="link-prefill"]')
    expect(alert.exists()).toBe(true)
    expect(alert.text()).toContain('http://gos3-corp.attacker.example:9000')
    expect(alert.text()).toContain('gos3.corp')
    expect(alert.text()).toContain('only continue if you trust it')
    expect(w.find('[data-testid="trust-prefill"]').exists()).toBe(true)

    vm.model.accessKeyId = 'ROOTKEY'
    vm.model.secretAccessKey = 'ROOTSECRET'
    await nextTick()
    await vm.submit()
    // Nothing was signed for or sent to the prefilled host, nothing stored.
    expect(hosts).toHaveLength(0)
    expect(useConnectionsStore().connections).toHaveLength(0)
    await nextTick()
    expect(w.text()).toContain('Confirm that you trust http://gos3-corp.attacker.example:9000')

    // Typing another address replaces the prefill: no confirmation needed.
    vm.model.endpoint = 'http://10.9.9.1:9000'
    await nextTick()
    expect(w.find('[data-testid="link-prefill"]').exists()).toBe(false)
  })

  it('requires the tick and warns for an http link to a host configured over https', async () => {
    const { hosts } = fakeProd()
    useConnectionsStore().add({ name: 'prod', endpoint: 'https://10.9.9.1:9000' })
    const w = await mountSuspended(ConnectPage, { route: '/connect?new=1&endpoint=http://10.9.9.1:9000' })
    const alert = w.find('[data-testid="link-prefill"]')
    expect(alert.text()).toContain('plain http')
    expect(w.find('[data-testid="trust-prefill"]').exists()).toBe(true)
    const vm = w.vm as unknown as ConnectVm
    vm.model.accessKeyId = 'ROOTKEY'
    vm.model.secretAccessKey = 'ROOTSECRET'
    await nextTick()
    await vm.submit()
    expect(hosts).toHaveLength(0)
  })

  it('does not ask to confirm a prefilled host that is already configured', async () => {
    fakeProd()
    useConnectionsStore().add({ name: 'prod', endpoint: 'http://10.9.9.1:9000' })
    const w = await mountSuspended(ConnectPage, { route: '/connect?new=1&endpoint=http://10.9.9.1:9000' })
    expect(w.find('[data-testid="link-prefill"]').text()).toContain('Check that it is the server you expect')
    expect(w.find('[data-testid="trust-prefill"]').exists()).toBe(false)
  })
})

describe('Add as server: reusing credentials for a discovered host (web-auth-2, discovery-4)', () => {
  interface FormVm { reuseCredentials: boolean, reuseConfirmed: boolean, save: () => Promise<void> }

  async function open(prefillEndpoint: string, from: Connection) {
    const w = await mountSuspended(FormModal, {
      props: {
        open: true,
        prefill: { endpoint: prefillEndpoint, name: 'prod-nas', serverId: 'nas-id', kind: 'gos3', source: 'discovered' },
        credentialsFrom: from,
        advertisedEndpoint: 'https://nas.lookalike.example'
      },
      attachTo: document.body
    })
    return { w, vm: w.vm as unknown as FormVm }
  }

  it('names the host, warns, and saves only after an explicit confirmation', async () => {
    fakeProd()
    const store = useConnectionsStore()
    const from = store.add({ id: 'a', name: 'nas', endpoint: 'http://10.9.9.1:9000', accessKeyId: 'ROOTKEY', secretAccessKey: 'ROOTSECRET', kind: 'gos3' })
    const { vm } = await open('http://10.0.0.66:9000', from)
    const body = () => document.body.textContent ?? ''
    expect(body()).toContain('https://nas.lookalike.example') // advertised endpoint only as a hint
    expect(vm.reuseCredentials).toBe(false) // off by default

    vm.reuseCredentials = true
    await nextTick()
    await vi.waitFor(() => expect(document.body.querySelector('[data-testid="reuse-warning"]')).toBeTruthy())
    expect(body()).toContain('10.0.0.66:9000 will receive requests signed with nas\'s keys')
    expect(body()).toContain('Discovery is not authenticated')

    await vm.save()
    expect(store.connections).toHaveLength(1) // refused without confirmation

    vm.reuseConfirmed = true
    await nextTick()
    await vm.save()
    expect(store.connections).toHaveLength(2)
    expect(store.connections[1]).toMatchObject({ endpoint: 'http://10.0.0.66:9000', accessKeyId: 'ROOTKEY', source: 'discovered' })
  })

  it('needs no confirmation when the keys stay with their own host', async () => {
    fakeProd()
    const store = useConnectionsStore()
    const from = store.add({ id: 'a', name: 'nas', endpoint: 'http://10.9.9.1:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
    const { vm } = await open('http://10.9.9.1:9000', from)
    vm.reuseCredentials = true
    await nextTick()
    expect(document.body.querySelector('[data-testid="reuse-warning"]')).toBeNull()
  })
})

describe('NewAccessKeyModal with a forged key-creation response (gap-1)', () => {
  it('offers no snippets or downloads for values with line breaks', async () => {
    const { default: NewAccessKeyModal } = await import('~/components/admin/NewAccessKeyModal.vue')
    const forged = { accessKey: 'GOS3X\n[default]\ncredential_process = sh -c evil', secretKey: 'abcdefghijklmnopqrstuvwxyz', status: 'Active' as const, createdAt: '2026-01-01T00:00:00Z' }
    await mountSuspended(NewAccessKeyModal, { props: { open: true, accessKey: forged, user: 'alice', endpoint: 'http://10.9.9.1:9000', region: 'us-east-1' }, attachTo: document.body })
    await vi.waitFor(() => expect(document.body.querySelector('[data-testid="snippet-problem"]')).toBeTruthy())
    expect(document.body.textContent).toContain('unexpected format')
    expect(document.body.textContent).not.toContain('AWS_SECRET_ACCESS_KEY')
    expect([...document.body.querySelectorAll('button')].some(b => b.textContent?.includes('Download'))).toBe(false)
  })

  it('offers them for a normal key', async () => {
    const { default: NewAccessKeyModal } = await import('~/components/admin/NewAccessKeyModal.vue')
    const key = { accessKey: 'GOS3AKEXAMPLE', secretKey: 'abcdefghijklmnopqrstuvwxyz0123456789ABCD', status: 'Active' as const, createdAt: '2026-01-01T00:00:00Z' }
    await mountSuspended(NewAccessKeyModal, { props: { open: true, accessKey: key, user: 'alice', endpoint: 'http://10.9.9.1:9000', region: 'us-east-1' }, attachTo: document.body })
    await vi.waitFor(() => expect(document.body.textContent).toContain('AWS_SECRET_ACCESS_KEY=abcdefghijklmnopqrstuvwxyz0123456789ABCD'))
    expect(document.body.querySelector('[data-testid="snippet-problem"]')).toBeNull()
  })
})
