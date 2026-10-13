// Servers & discovery components and composables in the Nuxt runtime.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { UApp } from '#components'
import type { Info, Peer, WhoAmI } from '~/types/admin'
import { useConnectionsStore } from '~/stores/connections'
import DiagnosisAlert from '~/components/servers/DiagnosisAlert.vue'
import StatusCard from '~/components/servers/StatusCard.vue'
import TestReport from '~/components/servers/TestReport.vue'
import PeersTable from '~/components/discovery/PeersTable.vue'
import ScanForm from '~/components/discovery/ScanForm.vue'
import SourceBadges from '~/components/discovery/SourceBadges.vue'
import ConfigSummary from '~/components/discovery/ConfigSummary.vue'

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

const PEER: Peer = {
  id: 'feedfacecafebeef',
  name: 'backup',
  version: '0.1.0',
  region: 'eu-west-1',
  endpoints: { s3: 'http://10.9.9.2:9000', admin: 'http://10.9.9.2:9000/_gos3/admin/v1' },
  sources: ['mdns', 'beacon'],
  addresses: ['http://10.9.9.2:9000'],
  firstSeen: '2026-10-09T12:00:00Z',
  lastSeen: '2026-10-09T12:00:00Z',
  healthy: true,
  verified: true
}

const WHOAMI: WhoAmI = {
  user: 'root',
  accessKey: 'AK',
  isRoot: true,
  policies: [],
  capabilities: { admin: true, viewInfo: true, viewStats: true, manageConfig: true, manageUsers: true, managePolicies: true, manageBuckets: true, discovery: true, scan: true },
  server: INFO
}

/** Fake gos3 at 10.9.9.1:9000 (info, whoami, discovery, stats); everything else is unreachable. */
function fakeFetch(input: RequestInfo | URL): Promise<Response> {
  const url = new URL(input instanceof Request ? input.url : String(input))
  const json = (v: unknown) => Promise.resolve(new Response(JSON.stringify(v), { status: 200, headers: { 'content-type': 'application/json' } }))
  if (url.host === '10.9.9.1:9000') {
    switch (url.pathname) {
      case '/_gos3/info': return json(INFO)
      case '/_gos3/admin/v1/whoami': return json(WHOAMI)
      case '/_gos3/admin/v1/discovery': return json({ enabled: true, config: {}, self: INFO, peers: [PEER] })
      case '/_gos3/admin/v1/stats': return json({ storage: { buckets: 3, objects: 1200, bytes: 2048 } })
    }
  }
  return Promise.reject(new TypeError('Failed to fetch'))
}

describe('servers components', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn(fakeFetch))
    useConnectionsStore().resetState()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('DiagnosisAlert shows title, message and hint', async () => {
    const w = await mountSuspended(DiagnosisAlert, {
      props: { diagnosis: { kind: 'cors', title: 'Blocked by CORS', message: 'reachable but blocked', hint: 'Add the origin', code: 'NetworkError' } }
    })
    expect(w.text()).toContain('Blocked by CORS')
    expect(w.text()).toContain('reachable but blocked')
    expect(w.text()).toContain('Add the origin')
    expect(w.find('[role="alert"]').exists()).toBe(true)
  })

  it('TestReport lists steps with their outcome', async () => {
    const w = await mountSuspended(TestReport, {
      props: {
        report: {
          ok: false,
          detectedKind: 'gos3',
          steps: [
            { id: 'reach', label: 'Reach server', status: 'ok', detail: 'gos3 answered' },
            { id: 'identity', label: 'Sign in (admin API)', status: 'error', detail: 'x', diagnosis: { kind: 'signature', title: 'Wrong secret key', message: 'mismatch' } }
          ]
        }
      }
    })
    expect(w.text()).toContain('Connection test failed')
    expect(w.text()).toContain('gos3 detected')
    expect(w.text()).toContain('gos3 answered')
    expect(w.text()).toContain('Wrong secret key')
  })

  it('StatusCard shows live status, identity and statistics', async () => {
    const store = useConnectionsStore()
    const c = store.add({ name: 'nas', endpoint: 'http://10.9.9.1:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
    await store.refreshStatus(c.id)
    const w = await mountSuspended(StatusCard, { props: { connection: c, isDefault: true } })
    await vi.waitFor(() => expect(w.text()).toContain('1.2K'))
    expect(w.text()).toContain('Online')
    expect(w.text()).toContain('root (root)')
    expect(w.text()).toContain('0.1.0')
    expect(w.find('[aria-label="Default server"]').exists()).toBe(true)
    expect(w.find(`a[href="/servers/${c.id}/buckets"]`).exists()).toBe(true)
    const stats = w.find('[role="group"][aria-label="Statistics"]')
    expect(stats.text()).toContain('3buckets')
    expect(stats.text()).toContain('1.2Kobjects')
  })

  it('StatusCard offers sign-in without credentials', async () => {
    const store = useConnectionsStore()
    const c = store.add({ name: 'nokeys', endpoint: 'http://10.9.9.1:9000', kind: 'gos3' })
    const w = await mountSuspended(StatusCard, { props: { connection: c } })
    expect(w.text()).toContain('Not signed in')
    expect(w.find(`a[href="/connect?server=${c.id}"]`).exists()).toBe(true)
  })
})

describe('discovery components', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn(fakeFetch))
    useConnectionsStore().resetState()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('SourceBadges labels mechanisms', async () => {
    const w = await mountSuspended(SourceBadges, { props: { sources: ['mdns', 'dns', 'scan'] } })
    expect(w.text()).toContain('mDNS')
    expect(w.text()).toContain('DNS SRV')
    expect(w.text()).toContain('Scan')
  })

  it('PeersTable offers "Add as server" and links configured peers', async () => {
    // Tooltips need the provider from UApp.
    const onAdd = vi.fn()
    const Wrapper = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(PeersTable, { peers: [PEER], onAdd }) }) })
    const w = await mountSuspended(Wrapper)
    expect(w.text()).toContain('backup')
    expect(w.text()).toContain('http://10.9.9.2:9000')
    await w.find('button[aria-label="Add backup as a server"]').trigger('click')
    expect(onAdd).toHaveBeenCalledWith(PEER)

    // Same server ID at another address: flagged, not linked as that server.
    const store = useConnectionsStore()
    store.add({ id: 'bk', name: 'Backup box', endpoint: 'http://other:9000', serverId: PEER.id })
    await nextTick()
    expect(w.text()).not.toContain('Open Backup box')
    expect(w.find('[data-testid="id-claim-badge"]').text()).toContain('ID of Backup box')
    expect(w.find('button[aria-label="Add backup as a server"]').exists()).toBe(true)

    // A connection at the address it was observed at: configured.
    store.add({ id: 'bk2', name: 'Backup NAS', endpoint: 'http://10.9.9.2:9000' })
    await nextTick()
    expect(w.text()).toContain('Open Backup NAS')
    expect(w.find('[data-testid="id-claim-badge"]').exists()).toBe(false)
  })

  it('ScanForm validates and emits a ScanRequest', async () => {
    const w = await mountSuspended(ScanForm, { props: { maxHosts: 1024 } })
    await w.find('form').trigger('submit')
    expect(w.text()).toContain('Enter at least one target')
    expect(w.emitted('submit')).toBeUndefined()
    await w.find('textarea').setValue('10.0.0.0/30\nnas.local')
    await w.find('form').trigger('submit')
    expect(w.emitted('submit')?.[0]).toEqual([{ targets: ['10.0.0.0/30', 'nas.local'], ports: [9000], schemes: ['http'], timeout: 1_500_000_000, concurrency: 64 }])
    expect(w.text()).toContain('3 hosts')
  })

  it('useDiscovery aggregates peer tables and browser probes', async () => {
    const store = useConnectionsStore()
    store.add({ id: 'g', name: 'nas', endpoint: 'http://10.9.9.1:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
    store.add({ id: 'n', name: 'nokeys', endpoint: 'http://10.9.9.3:9000', kind: 'gos3' })
    const d = useDiscovery()
    await d.refreshServers()
    const states = Object.fromEntries(d.sources.value.map(s => [s.key, s.state]))
    expect(states).toEqual({ 'server:g': 'ok', 'server:n': 'skipped' })
    expect(d.servers.value.map(s => s.id)).toEqual([PEER.id])
    expect(d.servers.value[0]!.foundBy[0]).toMatchObject({ kind: 'server', label: 'nas', connectionId: 'g', sources: ['mdns', 'beacon'] })

    const r = await d.probeUrls(['http://10.9.9.1:9000', 'http://10.9.9.4:9000'])
    expect(r.found.map(p => p.id)).toEqual([INFO.id])
    expect(r.failed.map(f => f.url)).toEqual(['http://10.9.9.4:9000'])
    expect(d.servers.value.map(s => s.id).sort()).toEqual([INFO.id, PEER.id].sort())
    expect(d.probeFailures.value).toHaveLength(1)
  })

  it('useDiscoveryLocal hides local discovery when the routes are missing', async () => {
    const local = useDiscoveryLocal()
    expect(await local.detect(true)).toBeNull()
    expect(local.available.value).toBe(false)
    expect(local.checked.value).toBe(true)
  })
})

describe('DiscoveryConfigSummary', () => {
  it('tells screen readers which mechanisms are off', async () => {
    const w = await mountSuspended(ConfigSummary, {
      props: {
        state: {
          enabled: true,
          config: { enabled: true, advertise: true, mdns: false, beacon: false, beaconGroup: '239.255.73.83:7373', beaconInterval: 10e9, seeds: [], dnsSrv: [], peerTtl: 60e9, allowScan: true, maxScanHosts: 1024 },
          self: INFO,
          peers: []
        }
      }
    })
    const items = w.findAll('ul[aria-label="Discovery mechanisms"] li').map(li => li.text().replace(/\s+/g, ' ').trim())
    expect(items).toEqual(['Enabled: on', 'Advertise: on', 'mDNS: off', 'Beacon: off', 'Scans allowed: on'])
  })
})
