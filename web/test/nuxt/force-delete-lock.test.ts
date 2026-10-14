// Force delete of a bucket under object lock: the Danger zone shows the
// server's 409 explanation and sends bypassGovernanceRetention only when
// "Bypass governance retention" is ticked.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import type { Info, WhoAmI } from '~/types/admin'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import DangerZoneTab from '~/components/bucket-settings/DangerZoneTab.vue'

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
const GOVERNANCE_MSG = 'bucket "locked" has 2 object(s) under GOVERNANCE retention (e.g. "a"); nothing was deleted (add bypassGovernanceRetention=true, which requires s3:BypassGovernanceRetention, to delete them)'

function fakeServer() {
  const deletes: string[] = []
  const whoami: WhoAmI = { user: 'root', accessKey: 'AK', isRoot: true, policies: [], capabilities: { admin: true, manageBuckets: true } as WhoAmI['capabilities'], server: INFO }
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input))
    const url = new URL(req.url)
    const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })
    if (url.pathname === '/_gos3/info') return json(INFO)
    if (url.pathname === '/_gos3/admin/v1/whoami') return json(whoami)
    if (req.method === 'DELETE' && url.pathname === '/_gos3/admin/v1/buckets/locked') {
      deletes.push(url.search)
      if (url.searchParams.get('bypassGovernanceRetention') !== 'true') return json({ error: { code: 'Conflict', message: GOVERNANCE_MSG } }, 409)
      return new Response(null, { status: 204 })
    }
    return new Response('', { status: 404 })
  }))
  return { deletes }
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

describe('DangerZoneTab force delete and object lock', () => {
  it('shows the 409 explanation and retries with bypassGovernanceRetention when ticked', async () => {
    const { deletes } = fakeServer()
    const store = useConnectionsStore()
    const c = store.add({ name: 'nas', endpoint: 'http://10.9.9.1:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
    store.setActive(c.id)
    await store.refreshStatus(c.id)
    const w = await mountSuspended(DangerZoneTab, { props: { bucket: 'locked' } })
    expect(w.find('[data-testid="bypass-governance"]').exists()).toBe(true)
    const vm = w.vm as unknown as { bypassGovernance: boolean, force: { run: () => Promise<unknown> } }

    await expect(vm.force.run()).rejects.toBeTruthy()
    await nextTick()
    const alert = w.find('[data-testid="force-delete-locked"]')
    expect(alert.text()).toContain('Object lock prevents deleting this bucket')
    expect(alert.text()).toContain('GOVERNANCE retention')
    expect(alert.text()).toContain('Tick “Bypass governance retention”')
    expect(deletes).toEqual(['?force=true'])

    vm.bypassGovernance = true
    await nextTick()
    await vm.force.run()
    expect(deletes[1]).toBe('?force=true&bypassGovernanceRetention=true')
    await nextTick()
    expect(w.find('[data-testid="force-delete-locked"]').exists()).toBe(false)
  })
})
