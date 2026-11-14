import { mountSuspended } from '@nuxt/test-utils/runtime'
import { enableAutoUnmount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { UApp } from '#components'
import type { Info, Peer } from '~/types/admin'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import ServersPage from '~/pages/servers/index.vue'
import ConnectPage from '~/pages/connect.vue'
import ServersTable from '~/components/discovery/ServersTable.vue'
import PeersTable from '~/components/discovery/PeersTable.vue'
import type { DiscoveredServer } from '~/components/discovery/-aggregate'

const INFO: Info = {
  service: 'gos3',
  id: 'version-test-server',
  name: 'nas',
  version: 'v0.3.1',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.9.9.1:9000', admin: 'http://10.9.9.1:9000/_gos3/admin/v1' },
  features: [],
  startedAt: '2026-10-09T12:00:00Z'
}

enableAutoUnmount(afterEach)

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  useConnectionsStore().resetState()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe.each(['v0.3.1', 'dev', '0.3.1'])('reported version %s', (version) => {
  async function addServer() {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ ...INFO, version }), {
      headers: { 'content-type': 'application/json' }
    })))
    const store = useConnectionsStore()
    const connection = store.add({ id: 'nas', name: INFO.name, endpoint: INFO.endpoints.s3, kind: 'gos3' })
    await store.refreshStatus(connection.id)
    return connection
  }

  it('shows the version unchanged in the Servers list', async () => {
    await addServer()
    const Wrapper = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(ServersPage) }) })
    const w = await mountSuspended(Wrapper, { route: '/servers' })
    const row = w.find('tbody tr')
    expect(row.find('p.text-xs').text()).toBe(version)
  })

  it('shows the version unchanged on the sign-in page', async () => {
    const connection = await addServer()
    const w = await mountSuspended(ConnectPage, { route: `/connect?server=${connection.id}` })
    expect(w.find('h1').text()).toBe('Sign in to nas')
    expect(w.find('span.text-xs').text()).toBe(version)
  })

  it('shows the version unchanged in discovery results', async () => {
    const server: DiscoveredServer = {
      ...INFO,
      version,
      addresses: [INFO.endpoints.s3],
      healthy: true,
      verified: true,
      foundBy: []
    }
    const Wrapper = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(ServersTable, { servers: [server] }) }) })
    const w = await mountSuspended(Wrapper)
    expect(w.find('tbody tr p.font-mono').text().replace(/\s+/g, ' ')).toBe(`${INFO.id} · ${version} · ${INFO.region}`)
  })

  it('shows the version unchanged in the peer table', async () => {
    const peer: Peer = {
      id: INFO.id,
      name: INFO.name,
      version,
      region: INFO.region,
      endpoints: INFO.endpoints,
      sources: ['mdns'],
      addresses: [INFO.endpoints.s3],
      firstSeen: INFO.startedAt,
      lastSeen: INFO.startedAt,
      healthy: true,
      verified: true
    }
    const Wrapper = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(PeersTable, { peers: [peer] }) }) })
    const w = await mountSuspended(Wrapper)
    expect(w.find('tbody tr p.font-mono').text().replace(/\s+/g, ' ')).toBe(`${INFO.id} · ${version} · ${INFO.region}`)
  })
})
