// Bucket compression tab against a fake gos3: the administrator's mode
// disables the switch direction the server would refuse (409), a refused
// change (403 / 409) is shown in the card, and "Reset to server default"
// sends DELETE and reloads the setting.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import type { Info, WhoAmI } from '~/types/admin'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import CompressionTab from '~/components/bucket-settings/CompressionTab.vue'

const INFO: Info = {
  service: 'gos3',
  id: 'b3f1c2d4e5f60718',
  name: 'nas',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.9.9.2:9000', admin: 'http://10.9.9.2:9000/_gos3/admin/v1' },
  features: [],
  startedAt: '2026-10-09T12:00:00Z'
}

interface FakeOptions {
  mode: 'optional' | 'enforced' | 'disabled'
  enabled: boolean
  /** Answer PUT/DELETE with 403. */
  denyWrites?: boolean
}

function fakeServer(opts: FakeOptions) {
  const state = { ...opts }
  const requests: string[] = []
  const whoami: WhoAmI = { user: 'ops', accessKey: 'AK', isRoot: false, policies: [], capabilities: { admin: true, manageCompression: !opts.denyWrites } as WhoAmI['capabilities'], server: INFO }
  const doc = () => ({
    enabled: state.enabled,
    effective: state.mode === 'enforced' || (state.mode === 'optional' && state.enabled),
    serverMode: state.mode,
    algorithm: 'deflate',
    level: 1,
    minSize: 4096
  })
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input))
    const url = new URL(req.url)
    const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })
    if (url.pathname === '/_gos3/info') return json(INFO)
    if (url.pathname === '/_gos3/admin/v1/whoami') return json(whoami)
    if (url.pathname === '/_gos3/admin/v1/buckets/photos/compression') {
      const body = req.method === 'PUT' ? await req.text() : ''
      requests.push(`${req.method}${body ? ` ${body}` : ''}`)
      if (req.method === 'GET') return json(doc())
      if (state.denyWrites) return json({ error: { code: 'AccessDenied', message: 'Access Denied.' } }, 403)
      if (req.method === 'DELETE') {
        state.enabled = false
        return new Response(null, { status: 204 })
      }
      const { enabled } = JSON.parse(body) as { enabled: boolean }
      if (state.mode === 'enforced' && !enabled) return json({ error: { code: 'Conflict', message: 'compression is enforced on this server by its administrator (compression.mode is "enforced")' } }, 409)
      if (state.mode === 'disabled' && enabled) return json({ error: { code: 'Conflict', message: 'compression is disabled on this server by its administrator (compression.mode is "disabled")' } }, 409)
      state.enabled = enabled
      return json(doc())
    }
    return new Response('', { status: 404 })
  }))
  return { requests, state }
}

async function mountTab() {
  const store = useConnectionsStore()
  const c = store.add({ name: 'nas', endpoint: 'http://10.9.9.2:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
  store.setActive(c.id)
  await store.refreshStatus(c.id)
  const w = await mountSuspended(CompressionTab, { props: { bucket: 'photos' } })
  await vi.waitFor(() => expect(w.find('[data-testid="compression-effective"]').exists()).toBe(true))
  return w
}

type TabVm = { onToggle: (v: boolean) => void, put: { run: (v: boolean) => Promise<unknown> }, reset: { run: () => Promise<unknown> } }

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  useConnectionsStore().resetState()
})
afterEach(() => {
  vi.unstubAllGlobals()
})

describe('CompressionTab', () => {
  it('enables compression in optional mode', async () => {
    const { requests } = fakeServer({ mode: 'optional', enabled: false })
    const w = await mountTab()
    expect(w.find('[data-testid="compression-effective"]').text()).toBe('Not compressing')
    expect(w.find('button[role="switch"]').attributes('disabled')).toBeUndefined()
    await (w.vm as unknown as TabVm).put.run(true)
    await nextTick()
    expect(requests).toEqual(['GET', 'PUT {"enabled":true}'])
    expect(w.find('[data-testid="compression-effective"]').text()).toBe('Compressing new objects')
  })

  it('explains an enforced mode and cannot turn it off', async () => {
    fakeServer({ mode: 'enforced', enabled: true })
    const w = await mountTab()
    expect(w.text()).toContain('Compression is enforced by the server administrator')
    expect(w.find('button[role="switch"]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('cannot be turned off for this bucket')
  })

  it('cannot enable compression the administrator disabled, and shows a 409 refusal', async () => {
    const { requests } = fakeServer({ mode: 'disabled', enabled: false })
    const w = await mountTab()
    expect(w.text()).toContain('Compression is disabled by the server administrator')
    expect(w.find('button[role="switch"]').attributes('disabled')).toBeDefined()
    // The server refuses it anyway (e.g. the mode changed after loading).
    await (w.vm as unknown as TabVm).put.run(true)
    await nextTick()
    expect(requests).toContain('PUT {"enabled":true}')
    const alert = w.find('[data-testid="compression-refused"]')
    expect(alert.text()).toContain('The server refused this setting')
    expect(alert.text()).toContain('compression.mode is "disabled"')
  })

  it('shows a 403 for an identity without admin:PutBucketCompression', async () => {
    fakeServer({ mode: 'optional', enabled: false, denyWrites: true })
    const w = await mountTab()
    expect(w.text()).toContain('changes work only if it is granted for this bucket')
    await (w.vm as unknown as TabVm).put.run(true)
    await nextTick()
    const alert = w.find('[data-testid="compression-refused"]')
    expect(alert.text()).toContain('Not allowed')
    expect(alert.text()).toContain('admin:PutBucketCompression')
  })

  it('resets the bucket to the server default', async () => {
    const { requests, state } = fakeServer({ mode: 'optional', enabled: true })
    const w = await mountTab()
    expect(w.find('[data-testid="compression-effective"]').text()).toBe('Compressing new objects')
    await (w.vm as unknown as TabVm).reset.run()
    await nextTick()
    expect(requests).toEqual(['GET', 'DELETE', 'GET'])
    expect(state.enabled).toBe(false)
    expect(w.find('[data-testid="compression-effective"]').text()).toBe('Not compressing')
  })
})
