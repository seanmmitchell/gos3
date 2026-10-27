// Jobs and restic components against a fake gos3 admin API: the jobs table
// (states, progress, errors, per-state actions), the Jobs page (filters,
// polling interval), the "Copy / Mirror" wizard (dry run, then create) and
// the bucket's "Backup & archive" tab (archive toggle, recovery
// repositories and snippets, revealing credentials, snapshot schedule,
// read-only and 403 states).
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, nextTick, type Component } from 'vue'
import { UApp } from '#components'
import type { BucketEncryptionProvider, BucketSnapshots, Capabilities, Info, Job, ResticInfo, WhoAmI } from '~/types/admin'
import { NO_CAPABILITIES, useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'
import { S3Client } from '~/utils/s3/client'
import JobsTable from '~/components/jobs/JobsTable.vue'
import { JOB_POLL_FAST_MS, JOB_POLL_SLOW_MS } from '~/components/jobs/-jobs'
import { SNAPSHOT_POLL_MS } from '~/components/bucket-settings/-restic'
import ResticCredentialsModal from '~/components/bucket-settings/ResticCredentialsModal.vue'
import CopyBucketModal from '~/components/jobs/CopyBucketModal.vue'
import ResticTab from '~/components/bucket-settings/ResticTab.vue'
import ResticSnapshotsCard from '~/components/bucket-settings/ResticSnapshotsCard.vue'
import JobsPage from '~/pages/servers/[id]/jobs.vue'
import OverviewTab from '~/components/bucket-settings/OverviewTab.vue'
import EncryptionTab from '~/components/bucket-settings/EncryptionTab.vue'

const INFO: Info = {
  service: 'gos3', id: 'b3f1c2d4e5f60718', name: 'nas', version: '0.1.0', apiVersion: 'v1', region: 'us-east-1',
  endpoints: { s3: 'http://10.9.9.4:9000', admin: 'http://10.9.9.4:9000/_gos3/admin/v1' }, features: [], startedAt: '2026-10-09T12:00:00Z'
}

const job = (over: Partial<Job> = {}): Job => ({
  id: 'j1',
  kind: 'copy',
  state: 'running',
  phase: 'copying',
  createdAt: '2026-10-15T10:00:00Z',
  spec: { kind: 'copy', source: { bucket: 'photos' }, target: { bucket: 'photos-bak' } },
  labels: { source: 'photos', target: 'photos-bak' },
  status: { totalsKnown: true, sourceKeysDone: 10, keysScanned: 10, keysCopied: 5, versionsCopied: 5, bytesCopied: 500, bytesDone: 500, keysDeleted: 0, conflicts: 0, totalKeys: 20, totalBytes: 1000, bytesPerSecond: 100, etaSeconds: 5 },
  ...over
})

const JOBS: Job[] = [
  job(),
  job({ id: 'j2', kind: 'mirror', state: 'failed', lastError: 'copy a.jpg: AccessDenied', failures: 2, spec: { kind: 'mirror', source: { bucket: 'docs' }, target: { bucket: 'docs-mirror' } }, labels: {} }),
  job({ id: 'j3', kind: 'restic-snapshot', state: 'succeeded', createdAt: '2026-10-14T10:00:00Z', spec: {}, labels: { bucket: 'photos' }, status: { keys: 12, bytes: 2048, snapshot: 'abcdef0123' } })
]

const RESTIC_INFO: ResticInfo = {
  settings: { provider: 'restic', scope: 'all' },
  archive: [
    { backing: 'default', role: 'primary', info: { bucketId: 'b1', repository: '/data/.gos3/restic/b1', recordLocation: '/data/.gos3/keys/b1.json', passwordSource: 'field password of the record', initialized: true } },
    { backing: 'mem', role: 'replica', error: 'no repository' }
  ],
  snapshots: null,
  snapshotRepository: null,
  tags: { object: 'gos3-object', bucket: 'gos3-bucket:cGhvdG9z', bucketId: 'gos3-bucket-id:b1', snapshot: 'gos3-snapshot' }
}

const SNAPSHOTS: BucketSnapshots = {
  config: {
    enabled: true, interval: '24h0m0s', at: '03:00', consistent: true, prefix: '',
    retention: { keepLast: 7, keepHourly: 0, keepDaily: 7, keepWeekly: 4, keepMonthly: 12, keepYearly: 0, keepWithin: '0s' },
    target: { kind: 'bucket' }, prune: { enabled: false, interval: '168h0m0s' }
  },
  status: { lastSuccessAt: '2026-10-15T03:00:00Z', lastAttemptAt: '2026-10-15T03:00:00Z', nextRunAt: '2026-10-16T03:00:00Z', lastSnapshot: 'f'.repeat(64) },
  repository: { repository: '/data/.gos3/restic/b1', passwordSource: 'record', initialized: true },
  snapshots: [
    { id: 'a'.repeat(64), shortId: 'aaaaaaaa', time: '2026-10-14T03:00:00Z', summary: { filesNew: 3, filesChanged: 1, filesUnmodified: 10, dataAdded: 4096, totalFilesProcessed: 14, totalBytesProcessed: 9000 } },
    { id: 'b'.repeat(64), shortId: 'bbbbbbbb', time: '2026-10-15T03:00:00Z', partial: true }
  ]
}

interface Seen { method: string, path: string, search: string, body: string }

interface FakeOptions {
  provider?: BucketEncryptionProvider
  /** GET /buckets/photos/restic answers 403. */
  resticDenied?: boolean
  snapshots?: BucketSnapshots
  jobs?: Job[]
  /** GET /buckets/photos/jobs answers 403. */
  bucketJobsDenied?: boolean
  /** whoami capabilities exactly as given (an older server lacks fields). */
  exactCaps?: Capabilities
  /** ResticInfo.snapshotsError. */
  snapshotsError?: string
}

/** Requests that wait until the test opens them (set a gate after the first load). */
interface Gates {
  jobs?: Promise<void>
  snapshots?: Promise<void>
}

function gate() {
  let open!: () => void
  const promise = new Promise<void>(r => (open = r))
  return { promise, open }
}

function fakeServer(caps: Partial<Capabilities>, opts: FakeOptions = {}) {
  const seen: Seen[] = []
  const gates: Gates = {}
  let provider: BucketEncryptionProvider = opts.provider ?? { provider: 'restic', scope: 'all', available: ['AES256', 'restic'], backings: ['default'] }
  let snapshots: BucketSnapshots = opts.snapshots ?? SNAPSHOTS
  let jobs = [...(opts.jobs ?? JOBS)]
  const whoami = (): WhoAmI => ({
    user: 'alice', accessKey: 'AKALICE', isRoot: false, policies: [], capabilities: opts.exactCaps ?? { ...NO_CAPABILITIES, ...caps }, server: INFO,
    backings: { default: 'default', available: [{ name: 'default', kind: 'fs' }, { name: 'cloud', kind: 's3', kinds: ['native', 'wrapped'], defaultKind: 'wrapped', containers: [{ id: 'home', default: true }] }] }
  })
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input))
    const url = new URL(req.url)
    const body = req.method === 'GET' || req.method === 'DELETE' ? '' : await req.text()
    seen.push({ method: req.method, path: url.pathname, search: url.search, body })
    const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })
    if (url.host !== '10.9.9.4:9000') throw new TypeError('Failed to fetch')
    const p = url.pathname.replace('/_gos3/admin/v1', '')
    if (url.pathname === '/_gos3/info') return json(INFO)
    if (p === '/whoami') return json(whoami())
    if (p === '/jobs' && req.method === 'GET') {
      if (gates.jobs) await gates.jobs
      return json({ jobs })
    }
    if (p === '/jobs' && req.method === 'POST') {
      const spec = JSON.parse(body) as { dryRun?: boolean }
      if (spec.dryRun) {
        const { dryRun: _d, ...norm } = spec
        return json({ spec: { ...norm, options: { ...(norm as { options?: object }).options, versions: 'all' } }, warnings: ['photos-bak is on another backing'] })
      }
      return json(job({ id: 'new1', state: 'queued', spec }), 201)
    }
    const action = /^\/jobs\/([^/]+)\/(pause|resume|cancel|run)$/.exec(p)
    if (action && req.method === 'POST') {
      const state = { pause: 'paused', resume: 'queued', cancel: 'canceled', run: 'queued' }[action[2] as 'pause']
      const j = { ...jobs.find(x => x.id === action[1])!, state }
      jobs = jobs.map(x => (x.id === j.id ? j : x))
      return json(j)
    }
    if (p.startsWith('/jobs/') && req.method === 'DELETE') {
      jobs = jobs.filter(x => `/jobs/${x.id}` !== p)
      return new Response(null, { status: 204 })
    }
    if (p === '/buckets/photos/jobs' && opts.bucketJobsDenied) return json({ error: { code: 'AccessDenied', message: 'admin:GetJob denied' } }, 403)
    if (p === '/buckets/photos/jobs') return json({ jobs: jobs.filter(j => j.labels?.source === 'photos' || j.labels?.bucket === 'photos') })
    if (p === '/buckets/photos/encryption-provider') {
      if (req.method === 'PUT') provider = { ...provider, ...JSON.parse(body) }
      return json(provider)
    }
    if (p === '/buckets/photos/restic') {
      if (opts.resticDenied) return json({ error: { code: 'AccessDenied', message: 'admin:GetBucketEncryption denied' } }, 403)
      return json(opts.snapshotsError ? { ...RESTIC_INFO, snapshotsError: opts.snapshotsError } : RESTIC_INFO)
    }
    if (p === '/buckets/photos/restic/credentials') {
      return json({ backing: 'default', purpose: 'archive', repository: '/data/.gos3/restic/b1', password: "s3cr3t'pw", env: {}, options: [] })
    }
    if (p === '/buckets/photos/restic/snapshots' && req.method === 'GET') {
      if (gates.snapshots) await gates.snapshots
      return json(snapshots)
    }
    if (p === '/buckets/photos/restic/snapshots/config' && req.method === 'PUT') {
      const cfg = JSON.parse(body)
      // The server chooses the repository of a schedule that names none.
      if (!cfg.target) cfg.target = { kind: 'backing', backing: 'default', repo: 'snap-9f' }
      snapshots = { ...snapshots, config: cfg }
      return json(cfg)
    }
    if (p === '/buckets/photos/restic/snapshots' && req.method === 'POST') return json(job({ id: 'snap1', kind: 'restic-snapshot', state: 'queued' }), 202)
    return new Response('', { status: 404 })
  }))
  return { seen, gates }
}

/** setTimeout calls (spied) with one of `delays`: a poll armed. */
function pollTimers(spy: { mock: { calls: unknown[][] } }, ...delays: number[]) {
  return spy.mock.calls.filter(c => delays.includes(c[1] as number))
}

async function connect() {
  const store = useConnectionsStore()
  const c = store.add({ id: 'nas', name: 'nas', endpoint: 'http://10.9.9.4:9000', accessKeyId: 'AKALICE', secretAccessKey: 'SK', kind: 'gos3' })
  store.setActive(c.id)
  await store.refreshStatus(c.id)
  return c
}

/** Mounts `comp` inside UApp (tooltips and overlays need its providers). */
async function mountInApp(comp: Component, props: Record<string, unknown>) {
  const Wrapper = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(comp, props) }) })
  const w = await mountSuspended(Wrapper)
  return { w, inner: w.findComponent(comp) }
}

const page = () => document.body.textContent ?? ''
const bodyButton = (label: string) => [...document.body.querySelectorAll('button')].find(b => b.textContent?.trim() === label)

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  clearClientCache()
  useConnectionsStore().resetState()
})
afterEach(() => {
  vi.unstubAllGlobals()
})

describe('JobsTable', () => {
  it('uses the same qualified pass and mirror assessment in full and compact views', async () => {
    fakeServer({ admin: true, viewJobs: true })
    await connect()
    const mirror = job({ kind: 'mirror', state: 'waiting', phase: 'watching', status: { totalsKnown: true, passComplete: true, sourceKeysDone: 14, keysScanned: 14, totalKeys: 14, bytesDone: 666, totalBytes: 666, keysCopied: 17, bytesCopied: 867, unresolved: 2, pending: 1 } })
    for (const compact of [false, true]) {
      const { w } = await mountInApp(JobsTable, { jobs: [mirror], serverId: 'nas', compact })
      expect(w.text()).toContain('Last full pass')
      expect(w.text()).toContain('14 of 14 source keys examined')
      expect(w.text()).toContain('Lifetime: 17 key writes')
      expect(w.text()).toContain('1 pending · 2 unresolved keys')
      expect(w.text()).not.toContain('17 of 14')
      expect(w.text()).not.toContain('In sync')
      w.unmount()
      const recovered = await mountInApp(JobsTable, { jobs: [{ ...mirror, status: { ...mirror.status, pending: 0, unresolved: 0, lastPassAt: '2026-10-24T12:00:00Z' } }], serverId: 'nas', compact })
      expect(recovered.w.text()).toContain('No known pending changes · last full comparison')
      recovered.w.unmount()
    }
  })
  it('shows each job with its state, progress, error and the actions its state allows', async () => {
    fakeServer({ admin: true, viewJobs: true, manageJobs: true })
    await connect()
    const { w, inner } = await mountInApp(JobsTable, { jobs: JOBS, serverId: 'nas', canManage: true })
    const row = (id: string) => w.find(`[data-testid="job-${id}"]`)
    expect(row('j1').attributes('data-state')).toBe('running')
    expect(row('j1').find('[data-testid="job-state"]').text()).toBe('Running')
    expect(row('j1').find('[data-testid="job-percent"]').text()).toBe('50%')
    expect(row('j1').find('[data-testid="job-timing"]').text()).toBe('about 5s left')
    expect(row('j1').find('a[href="/servers/nas/buckets/photos/settings"]').exists()).toBe(true)
    expect(row('j1').find('a[href="/servers/nas/buckets/photos-bak/settings"]').exists()).toBe(true)
    expect(row('j2').find('[data-testid="job-error"]').text()).toBe('copy a.jpg: AccessDenied')
    expect(row('j2').text()).toContain('2 failed runs')
    expect(row('j3').text()).toContain('Snapshot of photos')
    expect(row('j3').text()).toContain('snapshot abcdef01')

    const vm = inner.vm as unknown as { menu: (j: Job) => { label: string }[][] }
    const labels = (j: Job) => vm.menu(j).map(g => g.map(i => i.label))
    expect(labels(JOBS[0]!)).toEqual([['Pause'], ['Cancel']])
    expect(labels(JOBS[1]!)).toEqual([['Resume'], ['Delete']])
    expect(labels(JOBS[2]!)).toEqual([['Delete']])
    expect(labels(job({ state: 'waiting' }))).toEqual([['Run now', 'Pause'], ['Cancel']])
    w.unmount()
  })

  it('has no actions without canManage, and a compact form', async () => {
    fakeServer({ admin: true, viewJobs: true })
    await connect()
    const { w } = await mountInApp(JobsTable, { jobs: JOBS, serverId: 'nas', compact: true })
    expect(w.find('button[aria-label^="Actions for"]').exists()).toBe(false)
    expect(w.findAll('th[scope="col"]').map(t => t.text())).toEqual(['Job', 'State', 'Progress'])
    // The compact form keeps the last error, under the state.
    expect(w.find('[data-testid="job-j2"] [data-testid="job-error"]').exists()).toBe(true)
    w.unmount()
  })

  it('pauses a job at once and deletes a finished one after confirming', async () => {
    const { seen } = fakeServer({ admin: true, viewJobs: true, manageJobs: true })
    await connect()
    const { w, inner } = await mountInApp(JobsTable, { jobs: JOBS, serverId: 'nas', canManage: true })
    const vm = inner.vm as unknown as { onAction: (j: Job, a: string) => Promise<void> }
    await vm.onAction(JOBS[0]!, 'pause')
    expect(seen.filter(s => s.method === 'POST').map(s => s.path)).toEqual(['/_gos3/admin/v1/jobs/j1/pause'])
    expect(inner.emitted('changed')![0]).toEqual([expect.objectContaining({ id: 'j1', state: 'paused' }), 'j1'])

    const pending = vm.onAction(JOBS[1]!, 'delete')
    await vi.waitFor(() => expect(page()).toContain('Delete this job from the history?'))
    expect(seen.some(s => s.method === 'DELETE')).toBe(false)
    bodyButton('Delete job')!.click()
    await pending
    expect(seen.filter(s => s.method === 'DELETE').map(s => s.path)).toEqual(['/_gos3/admin/v1/jobs/j2'])
    expect(inner.emitted('changed')![1]).toEqual([null, 'j2'])
    w.unmount()
  })
})

describe('Jobs page', () => {
  it('lists the jobs, filters by bucket from the query and polls fast while one runs', async () => {
    const { seen } = fakeServer({ admin: true, viewJobs: true, manageJobs: true })
    await connect()
    const w = await mountSuspended(JobsPage, { route: '/servers/nas/jobs?bucket=docs' })
    await vi.waitFor(() => expect(w.find('[data-testid="jobs-table"]').exists(), w.text()).toBe(true))
    expect(seen.some(s => s.method === 'GET' && s.path === '/_gos3/admin/v1/jobs')).toBe(true)
    expect(w.findAll('tbody tr').map(r => r.attributes('data-testid'))).toEqual(['job-j2'])
    expect(w.text()).toContain('1 of 3 jobs shown.')
    expect(w.text()).toContain('refreshing every 3 s')
    w.unmount()
  })

  it('arms no poll when unmounted while a request is in flight', async () => {
    const { seen, gates } = fakeServer({ admin: true, viewJobs: true })
    const g = gate()
    gates.jobs = g.promise
    await connect()
    const w = await mountSuspended(JobsPage, { route: '/servers/nas/jobs' })
    await vi.waitFor(() => expect(seen.some(s => s.method === 'GET' && s.path === '/_gos3/admin/v1/jobs')).toBe(true))
    w.unmount()
    const timers = vi.spyOn(globalThis, 'setTimeout')
    g.open()
    await new Promise(r => setTimeout(r, 300))
    expect(pollTimers(timers, JOB_POLL_FAST_MS, JOB_POLL_SLOW_MS)).toEqual([])
    expect(seen.filter(s => s.path === '/_gos3/admin/v1/jobs')).toHaveLength(1)
  })

  it('explains when the identity may not list jobs', async () => {
    fakeServer({ admin: true })
    await connect()
    const w = await mountSuspended(JobsPage, { route: '/servers/nas/jobs' })
    await vi.waitFor(() => expect(w.text()).toContain('No access to jobs'))
    w.unmount()
  })
})

describe('CopyBucketModal', () => {
  it('checks the job with a dry run, shows the warnings, then creates it', async () => {
    const { seen } = fakeServer({ admin: true, viewJobs: true, manageJobs: true })
    await connect()
    vi.spyOn(S3Client.prototype, 'listBuckets').mockResolvedValue({ buckets: [{ name: 'photos' }, { name: 'docs' }] })
    const w = await mountSuspended(CopyBucketModal, { props: { bucket: 'photos', versioning: 'Enabled', objectLock: false, serverId: 'nas' }, attrs: { open: true } })
    await flushPromises()
    const vm = w.vm as unknown as { step: string, form: { targetBucket: string, backing: string }, next: () => void, submit: () => Promise<void> }
    expect(vm.form.targetBucket).toBe('photos-copy')
    expect(document.body.querySelector('[data-testid="copy-wizard"]')!.getAttribute('data-step')).toBe('target')

    // An invalid target keeps the wizard on its first step.
    vm.form.targetBucket = 'docs'
    vm.next()
    await nextTick()
    expect(vm.step).toBe('target')
    expect(page()).toContain('A bucket with this name exists')

    vm.form.targetBucket = 'photos-bak'
    vm.form.backing = 'cloud'
    vm.next()
    await nextTick()
    expect(vm.step).toBe('options')
    vm.next()
    await vi.waitFor(() => expect(document.body.querySelector('[data-testid="dry-run-warning"]')).not.toBeNull())
    expect(page()).toContain('photos-bak is on another backing')
    const summary = document.body.querySelector('[data-testid="dry-run-summary"]')!.textContent!
    expect(summary).toContain('photos-bak (new bucket)')
    expect(summary).toContain('backing cloud, wrapped, container home')
    // The summary shows the server's normalized spec.
    expect(summary).toContain('All versions')
    const dry = seen.find(s => s.method === 'POST' && s.path.endsWith('/jobs'))!
    expect(JSON.parse(dry.body)).toMatchObject({ kind: 'copy', source: { bucket: 'photos' }, target: { bucket: 'photos-bak', create: { backing: 'cloud', kind: 'wrapped', container: 'home' } }, dryRun: true })
    // Object lock is always explicit, so the server never follows the source on its own.
    expect(JSON.parse(dry.body).target.create.objectLock).toBe(false)
    expect(summary).toContain('no object lock')

    await vm.submit()
    expect(w.emitted('close')).toEqual([[expect.objectContaining({ id: 'new1' })]])
    const posts = seen.filter(s => s.method === 'POST' && s.path.endsWith('/jobs'))
    expect(posts).toHaveLength(2)
    expect(JSON.parse(posts[1]!.body)).not.toHaveProperty('dryRun')
    w.unmount()
  })
})

describe('ResticTab', () => {
  const ALL: Partial<Capabilities> = { admin: true, manageSnapshots: true, runSnapshots: true, restoreSnapshots: true, revealResticCredentials: true, manageJobs: true, viewJobs: true }

  it('shows the archive, the recovery repositories and the snapshots', async () => {
    fakeServer(ALL)
    await connect()
    const { w } = await mountInApp(ResticTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="snapshot-list"]').exists(), w.text()).toBe(true))
    expect(w.find('[data-testid="archive-scope"]').text()).toBe('Archiving every write')
    expect(w.find('[data-testid="archive-cost"]').exists()).toBe(true)
    const repo = w.find('[data-testid="restic-repo-archive-default"]')
    expect(repo.text()).toContain('/data/.gos3/restic/b1')
    expect(repo.text()).toContain('field password of the record')
    const snippet = repo.find('[data-testid="restic-snippet"]').text()
    expect(snippet).toContain('export RESTIC_REPOSITORY=/data/.gos3/restic/b1')
    expect(snippet).toContain('restic snapshots --tag gos3-object,gos3-bucket:cGhvdG9z')
    expect(snippet).not.toContain('RESTIC_PASSWORD=')
    expect(w.find('[data-testid="restic-repo-none-mem"]').text()).toContain('no repository')
    expect(repo.text()).toContain('Reveal credentials…')
    // PowerShell
    await w.findAll('button').find(b => b.text() === 'PowerShell')!.trigger('click')
    expect(repo.find('[data-testid="restic-snippet"]').text()).toContain("$env:RESTIC_REPOSITORY = '/data/.gos3/restic/b1'")

    expect(w.find('[data-testid="snapshot-schedule"]').text()).toBe('Daily at 03:00 UTC')
    expect(w.find('[data-testid="snapshot-form"]').exists()).toBe(true)
    const rows = w.findAll('[data-testid="snapshot-list"] tbody tr')
    expect(rows.map(r => r.attributes('data-testid'))).toEqual(['snapshot-bbbbbbbb', 'snapshot-aaaaaaaa'])
    expect(rows[0]!.text()).toContain('partial')
    expect(rows[1]!.text()).toContain('3 new · 1 changed')
    // Partial snapshots cannot be restored or forgotten on their own.
    expect(rows[0]!.find('button[aria-label^="Actions for"]').exists()).toBe(false)
    expect(rows[1]!.find('button[aria-label^="Actions for"]').exists()).toBe(true)
    expect(w.findAll('button').some(x => x.text() === 'Snapshot now')).toBe(true)
    // The gos3 CLI commands name this server, and the offline config.
    const cli = w.find('[data-testid="restic-cli"]').text()
    expect(cli).toContain("gos3 restic env --bucket 'photos' --shell 'powershell' --server 'http://10.9.9.4:9000'")
    expect(cli).toContain("--config '/etc/gos3/gos3.yaml'")
    w.unmount()
  })

  it('reveals the credentials once, after confirming', async () => {
    const { seen } = fakeServer(ALL)
    await connect()
    const { w } = await mountInApp(ResticTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="restic-repo-archive-default"]').exists()).toBe(true))
    await w.find('[data-testid="restic-repo-archive-default"]').findAll('button').find(b => b.text() === 'Reveal credentials…')!.trigger('click')
    await vi.waitFor(() => expect(page()).toContain('Reveal the repository password?'))
    expect(seen.some(s => s.path.endsWith('/restic/credentials'))).toBe(false)
    bodyButton('Reveal credentials')!.click()
    await vi.waitFor(() => expect(document.body.querySelector('[data-testid="restic-credentials"]')).not.toBeNull())
    expect(JSON.parse(seen.find(s => s.path.endsWith('/restic/credentials'))!.body)).toEqual({ backing: 'default', purpose: 'archive' })
    // The password is masked until shown, also in the commands; shown, it is quoted for the shell.
    const snippet = () => document.body.querySelector('[data-testid="restic-credentials-snippet"]')!.textContent!
    expect(document.body.querySelector('[data-testid="restic-password"]')!.textContent).not.toContain('s3cr3t')
    expect(snippet()).not.toContain('s3cr3t')
    expect(snippet()).toContain('RESTIC_PASSWORD=')
    ;(document.body.querySelector('[data-testid="show-secrets"]') as HTMLButtonElement).click()
    await vi.waitFor(() => expect(snippet()).toContain("export RESTIC_PASSWORD='s3cr3t'\\''pw'"))
    w.unmount()
  })

  it('turns the archive on after confirming its cost', async () => {
    const { seen } = fakeServer(ALL, { provider: { provider: 'AES256', available: ['AES256', 'restic'] } })
    await connect()
    const { w, inner } = await mountInApp(ResticTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="archive-scope"]').exists()).toBe(true))
    expect(w.find('[data-testid="archive-scope"]').text()).toBe('Off')
    const vm = inner.vm as unknown as { onToggle: (on: boolean) => Promise<void> }
    const pending = vm.onToggle(true)
    await vi.waitFor(() => expect(page()).toContain('Archive every object version in restic?'))
    expect(page()).toContain('instead of native AES-256-GCM')
    bodyButton('Turn on archive')!.click()
    await pending
    expect(seen.filter(s => s.method === 'PUT').map(s => s.body)).toEqual(['{"provider":"restic","scope":"all"}'])
    await vi.waitFor(() => expect(w.find('[data-testid="archive-scope"]').text()).toBe('Archiving every write'))
    expect(inner.emitted('changed')).toHaveLength(1)
    w.unmount()
  })

  it('offers no toggle when restic is unavailable for the bucket', async () => {
    fakeServer(ALL, { provider: { provider: 'AES256', available: [] } })
    await connect()
    const { w } = await mountInApp(ResticTab, { bucket: 'photos', kind: 'native' })
    await vi.waitFor(() => expect(w.find('[data-testid="archive-toggle"]').exists()).toBe(true))
    const toggle = w.find('[data-testid="archive-toggle"]')
    const button = toggle.element.tagName === 'BUTTON' ? toggle : toggle.find('button')
    expect(button.attributes('disabled')).toBeDefined()
    expect(w.find('[data-testid="restic-archive"]').text()).toContain('native buckets of S3 backings store objects as they are')
    expect(w.find('[data-testid="archive-cost"]').exists()).toBe(false)
    w.unmount()
  })

  it('disables the toggle, also while on, when restic is not available', async () => {
    fakeServer(ALL, { provider: { provider: 'restic', scope: 'all', available: ['AES256'] } })
    await connect()
    const { w } = await mountInApp(ResticTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="archive-toggle"]').exists()).toBe(true))
    expect(w.find('[data-testid="archive-scope"]').text()).toBe('Archiving every write')
    const toggle = w.find('[data-testid="archive-toggle"]')
    const button = toggle.element.tagName === 'BUTTON' ? toggle : toggle.find('button')
    expect(button.attributes('disabled')).toBeDefined()
    expect(w.find('[data-testid="restic-archive"]').text()).toContain('Restic is not available for this bucket')
    w.unmount()
  })

  it('hides Snapshot now and Restore without runSnapshots and restoreSnapshots', async () => {
    fakeServer({ admin: true, manageSnapshots: true, manageJobs: true })
    await connect()
    const { w, inner } = await mountInApp(ResticTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="snapshot-list"]').exists()).toBe(true))
    expect(w.findAll('button').some(x => x.text() === 'Snapshot now')).toBe(false)
    const card = inner.findComponent(ResticSnapshotsCard).vm as unknown as { snapshotMenu: (s: object) => { label: string }[][] }
    // Forget stays (manageSnapshots); Restore needs restoreSnapshots.
    expect(card.snapshotMenu(SNAPSHOTS.snapshots![0]!).map(g => g.map(i => i.label))).toEqual([['Forget…']])
    w.unmount()
  })

  it('leaves a new schedule\u2019s repository to the server and shows its choice', async () => {
    const { seen } = fakeServer(ALL, { snapshots: { config: null, status: null, repository: null, snapshots: null } })
    await connect()
    const { w, inner } = await mountInApp(ResticTab, { bucket: 'photos', kind: 'native' })
    await vi.waitFor(() => expect(w.find('[data-testid="snapshot-form"]').exists()).toBe(true))
    expect(w.find('[data-testid="no-snapshots"]').exists()).toBe(true)
    const card = inner.findComponent(ResticSnapshotsCard)
    const vm = card.vm as unknown as { form: { interval: string, at: string, targetKind: string, backing: string }, onSave: () => Promise<void> }
    expect(vm.form.targetKind).toBe('auto')
    vm.form.interval = '12h'
    vm.form.at = ''
    await vm.onSave()
    const put = seen.find(s => s.method === 'PUT' && s.path.endsWith('/restic/snapshots/config'))!
    expect(JSON.parse(put.body)).toEqual({
      enabled: true, interval: '12h', consistent: true,
      retention: { keepLast: 7, keepHourly: 0, keepDaily: 7, keepWeekly: 4, keepMonthly: 12, keepYearly: 0, keepWithin: '0s' },
      // Prune is off by default while the archive is on (its lock stalls archive writes).
      prune: { enabled: false, interval: '168h', default: true }
    })
    await vi.waitFor(() => expect(w.find('[data-testid="snapshot-schedule"]').text()).toBe('Every 12h'))
    // The form now shows the repository the server chose.
    await vi.waitFor(() => expect(w.find('[data-testid="snapshot-target"]').text()).toBe('Saved: Repository snap-9f on default'))
    expect(vm.form.targetKind).toBe('backing')
    w.unmount()
  })

  it('shows repository errors instead of an empty snapshot list', async () => {
    fakeServer(ALL, {
      snapshots: { ...SNAPSHOTS, snapshots: null, error: 'restic: repository is locked by another process' },
      snapshotsError: 'cannot open the snapshot repository on cold: unreachable'
    })
    await connect()
    const { w } = await mountInApp(ResticTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="snapshots-error"]').exists(), w.text()).toBe(true))
    expect(w.find('[data-testid="snapshots-error"]').text()).toContain('repository is locked')
    expect(w.find('[data-testid="no-snapshots"]').exists()).toBe(false)
    expect(w.find('[data-testid="snapshots-repo-error"]').text()).toContain('unreachable')
    w.unmount()
  })

  it('arms no poll when unmounted while a snapshot poll is in flight', async () => {
    const { seen, gates } = fakeServer(ALL, { snapshots: { ...SNAPSHOTS, status: { ...SNAPSHOTS.status, running: 'snap1' } } })
    await connect()
    const { w, inner } = await mountInApp(ResticTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="snapshot-running"]').exists()).toBe(true))
    const g = gate()
    gates.snapshots = g.promise
    const card = inner.findComponent(ResticSnapshotsCard).vm as unknown as { poll: () => Promise<void> }
    const polled = card.poll()
    const gets = () => seen.filter(s => s.method === 'GET' && s.path.endsWith('/restic/snapshots')).length
    const before = gets()
    await vi.waitFor(() => expect(gets()).toBe(before + 1))
    w.unmount()
    const timers = vi.spyOn(globalThis, 'setTimeout')
    g.open()
    await polled
    await new Promise(r => setTimeout(r, 100))
    expect(pollTimers(timers, SNAPSHOT_POLL_MS)).toEqual([])
  })

  it('is read-only without the capabilities and explains a 403', async () => {
    fakeServer({ admin: true }, { resticDenied: true })
    await connect()
    const { w } = await mountInApp(ResticTab, { bucket: 'photos' })
    await vi.waitFor(() => expect(w.find('[data-testid="snapshot-readonly"]').exists(), w.text()).toBe(true))
    expect(w.find('[data-testid="recovery-denied"]').exists()).toBe(true)
    expect(w.find('[data-testid="snapshot-form"]').exists()).toBe(false)
    expect(w.text()).not.toContain('Snapshot now')
    expect(w.text()).not.toContain('Reveal credentials')
    expect(w.find('[data-testid="snapshot-list"] button[aria-label^="Actions for"]').exists()).toBe(false)
    w.unmount()
  })
})

describe('bucket overview Jobs card', () => {
  const props = { bucket: 'photos', summary: null, versioning: 'Enabled', objectLock: false }

  it('lists the jobs of the bucket with a link to the Jobs page', async () => {
    const { seen } = fakeServer({ admin: true, viewJobs: true, updateJobs: true })
    await connect()
    vi.spyOn(S3Client.prototype, 'getBucketLocation').mockResolvedValue('us-east-1')
    vi.spyOn(S3Client.prototype, 'listBuckets').mockResolvedValue({ buckets: [{ name: 'photos' }] })
    vi.spyOn(S3Client.prototype, 'getBucketPolicyStatus').mockRejectedValue(new Error('not allowed'))
    const { w } = await mountInApp(OverviewTab, props)
    await vi.waitFor(() => expect(w.find('[data-testid="bucket-jobs"]').exists(), w.text()).toBe(true))
    expect(seen.some(s => s.path === '/_gos3/admin/v1/buckets/photos/jobs')).toBe(true)
    const card = w.find('[data-testid="bucket-jobs"]')
    expect(card.findAll('tbody tr').map(r => r.attributes('data-testid'))).toEqual(['job-j1', 'job-j3'])
    expect(card.find('a[href="/servers/nas/jobs?bucket=photos"]').exists()).toBe(true)
    expect(card.find('button[aria-label^="Actions for"]').exists()).toBe(true)
    w.unmount()
  })

  it('stays hidden when the identity may not read the jobs of the bucket', async () => {
    const { seen } = fakeServer({ admin: true }, { bucketJobsDenied: true })
    await connect()
    vi.spyOn(S3Client.prototype, 'getBucketLocation').mockResolvedValue('us-east-1')
    vi.spyOn(S3Client.prototype, 'listBuckets').mockResolvedValue({ buckets: [{ name: 'photos' }] })
    vi.spyOn(S3Client.prototype, 'getBucketPolicyStatus').mockRejectedValue(new Error('not allowed'))
    const { w } = await mountInApp(OverviewTab, props)
    await vi.waitFor(() => expect(seen.some(s => s.path === '/_gos3/admin/v1/buckets/photos/jobs')).toBe(true))
    await flushPromises()
    expect(w.find('[data-testid="bucket-jobs"]').exists()).toBe(false)
    w.unmount()
  })
})

describe('EncryptionTab and the archive', () => {
  it('shows the scope, links to Backup & archive and warns that native encryption ends the archive', async () => {
    fakeServer({ admin: true, manageSnapshots: false })
    await connect()
    vi.spyOn(S3Client.prototype, 'getBucketEncryption').mockResolvedValue(null)
    const w = await mountSuspended(EncryptionTab, { props: { bucket: 'photos' } })
    await vi.waitFor(() => expect(w.find('[data-testid="provider-scope"]').exists(), w.text()).toBe(true))
    expect(w.find('[data-testid="provider-scope"]').text()).toContain('Every write (restic archive)')
    expect(w.find('[data-testid="restic-tab-link"]').attributes('href')).toBe('/servers/nas/buckets/photos/settings?tab=restic')
    expect(w.find('[data-testid="archive-off-warning"]').exists()).toBe(false)
    const vm = w.vm as unknown as { provider: string }
    vm.provider = 'AES256'
    await nextTick()
    expect(w.find('[data-testid="archive-off-warning"]').exists()).toBe(true)
    w.unmount()
  })

  it('has no link on servers without the tab', async () => {
    const { manageSnapshots: _m, ...older } = { ...NO_CAPABILITIES, admin: true }
    fakeServer({}, { provider: { provider: 'restic', available: ['restic'] }, exactCaps: older })
    await connect()
    vi.spyOn(S3Client.prototype, 'getBucketEncryption').mockResolvedValue(null)
    const w = await mountSuspended(EncryptionTab, { props: { bucket: 'photos' } })
    await vi.waitFor(() => expect(w.find('[data-testid="provider-scope"]').exists()).toBe(true))
    expect(w.find('[data-testid="provider-scope"]').text()).toContain('Writes that request SSE-S3')
    expect(w.find('[data-testid="restic-tab-link"]').exists()).toBe(false)
    w.unmount()
  })
})

describe('ResticCredentialsModal', () => {
  it('masks secrets in the fields and the commands until shown; copying uses the real values', async () => {
    fakeServer({ admin: true })
    await connect()
    const credentials = {
      backing: 'cloud', purpose: 'snapshots', repository: 's3:https://s3.example.com/acme/restic-aux/snap-1', password: 'pw-1',
      env: { AWS_ACCESS_KEY_ID: 'AKIAX', AWS_SECRET_ACCESS_KEY: 'secret-2' }, options: ['s3.region=eu-central-1']
    }
    const w = await mountSuspended(ResticCredentialsModal, { props: { bucket: 'photos', credentials, tags: RESTIC_INFO.tags, server: 'http://10.9.9.4:9000' }, attrs: { open: true } })
    await flushPromises()
    const shown = () => document.body.querySelector('[data-testid="restic-credentials"]')!.textContent!
    expect(shown()).not.toContain('pw-1')
    expect(shown()).not.toContain('secret-2')
    expect(shown()).toContain('AKIAX')
    const vm = w.vm as unknown as { snippet: string, shownSnippet: string }
    expect(vm.snippet).toContain('export RESTIC_PASSWORD=pw-1')
    expect(vm.snippet).toContain('export AWS_SECRET_ACCESS_KEY=secret-2')
    expect(vm.shownSnippet).not.toContain('pw-1')
    ;(document.body.querySelector('[data-testid="show-secrets"]') as HTMLButtonElement).click()
    await vi.waitFor(() => expect(shown()).toContain('secret-2'))
    expect(document.body.querySelector('[data-testid="restic-credentials-snippet"]')!.textContent).toContain('export RESTIC_PASSWORD=pw-1')
    w.unmount()
  })
})

describe('EncryptionTab on a native bucket', () => {
  it('offers no provider', async () => {
    fakeServer({ admin: true }, { provider: { provider: 'AES256', available: ['AES256', 'restic'] } })
    await connect()
    vi.spyOn(S3Client.prototype, 'getBucketEncryption').mockResolvedValue(null)
    const w = await mountSuspended(EncryptionTab, { props: { bucket: 'photos', kind: 'native' } })
    await vi.waitFor(() => expect(w.find('[data-testid="provider-scope"]').exists()).toBe(true))
    const radios = w.findAll('[data-testid="encryption-provider"] button[role="radio"]')
    expect(radios.length).toBe(2)
    for (const r of radios) expect(r.attributes('disabled')).toBeDefined()
    expect(w.find('[data-testid="encryption-provider"]').text()).toContain('native buckets of S3 backings store objects as they are')
    w.unmount()
  })
})
