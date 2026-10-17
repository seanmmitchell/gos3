// Copy, mirror and restic jobs: the admin client calls, the capability
// gating, the pure helpers behind the Jobs page and its table (state
// badges, progress, ETA / lag, actions, polling, filters) and the "Copy /
// Mirror" wizard's rules (versions, validation, request body, summary).
import { describe, expect, it, vi } from 'vitest'
import { AdminClient } from '~/utils/admin/client'
import type { AvailableBackings, Capabilities, Job } from '~/types/admin'
import { NO_CAPS, canManageJobs, canUpdateJobs, canViewJobs, serverHasJobs } from '~/components/admin/-capabilities'
import { NO_CAPABILITIES } from '~/stores/connections'
import {
  JOB_POLL_FAST_MS,
  JOB_POLL_SLOW_MS,
  copyStatusOf,
  describeJob,
  filterJobs,
  formatEta,
  jobActions,
  jobBusy,
  jobKindMeta,
  jobPollInterval,
  jobProgress,
  jobStateMeta,
  jobTiming,
  snapshotStatusOf,
  sortJobs,
  upsertJob
} from '~/components/jobs/-jobs'
import {
  allVersionsRefusal,
  copyHints,
  copyJobSpec,
  describeSpec,
  emptyCopyForm,
  stepValid,
  suggestedTargetName,
  targetKind,
  TARGET_STEP_FIELDS,
  validateCopyForm,
  versionsItems,
  type CopyContext
} from '~/components/jobs/-copy-job'
import { durationText, formatGoDuration, parseGoDuration } from '~/utils/durations'

interface Captured { method: string, url: string, body: string, payloadHash: string }

function makeAdmin(responder: (c: Captured) => Response) {
  const calls: Captured[] = []
  const fetch = vi.fn(async (req: Request) => {
    const body = ['GET', 'HEAD'].includes(req.method) ? '' : await req.text()
    const c = { method: req.method, url: req.url.replace('http://localhost:9000/_gos3/admin/v1', ''), body, payloadHash: req.headers.get('x-amz-content-sha256') ?? '' }
    calls.push(c)
    return responder(c)
  })
  const admin = new AdminClient({ endpoint: 'http://localhost:9000', accessKeyId: 'AK', secretAccessKey: 'SK' }, { fetch, datetime: '20261009T120000Z' })
  return { admin, calls }
}

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })

const job = (over: Partial<Job> = {}): Job => ({
  id: 'j1',
  kind: 'copy',
  state: 'running',
  createdAt: '2026-10-15T10:00:00Z',
  spec: { kind: 'copy', source: { bucket: 'photos' }, target: { bucket: 'photos-bak' } },
  labels: { source: 'photos', target: 'photos-bak' },
  status: { keysScanned: 10, keysCopied: 5, versionsCopied: 5, bytesCopied: 400, bytesDone: 500, keysDeleted: 0, conflicts: 0, totalKeys: 20, totalBytes: 1000, bytesPerSecond: 100, etaSeconds: 5 },
  ...over
})

describe('AdminClient jobs', () => {
  it('lists jobs with the filters as query, tolerating null lists', async () => {
    const { admin, calls } = makeAdmin(() => json({ jobs: null }))
    expect(await admin.listJobs()).toEqual([])
    await admin.listJobs({ kind: 'mirror', state: 'running', bucket: 'my photos' })
    expect(calls.map(c => c.url)).toEqual(['/jobs', '/jobs?kind=mirror&state=running&bucket=my+photos'])
    expect(await admin.listBucketJobs('a b')).toEqual([])
    expect(calls[2]).toMatchObject({ method: 'GET', url: '/buckets/a%20b/jobs' })
  })

  it('creates a job without dryRun and dry-runs with it', async () => {
    const spec = { kind: 'copy' as const, source: { bucket: 'photos' }, target: { bucket: 'bak', create: {} }, dryRun: true }
    const { admin, calls } = makeAdmin(c => (JSON.parse(c.body).dryRun ? json({ spec: { ...spec, dryRun: undefined, options: { versions: 'all' } }, warnings: null }) : json(job(), 201)))
    const created = await admin.createCopyJob(spec)
    expect(created.id).toBe('j1')
    expect(calls[0]).toMatchObject({ method: 'POST', url: '/jobs' })
    expect(JSON.parse(calls[0]!.body)).toEqual({ kind: 'copy', source: { bucket: 'photos' }, target: { bucket: 'bak', create: {} } })
    const dry = await admin.dryRunCopyJob({ ...spec, dryRun: undefined })
    expect(JSON.parse(calls[1]!.body).dryRun).toBe(true)
    expect(dry.warnings).toEqual([])
    expect(dry.spec.options?.versions).toBe('all')
  })

  it('pauses, resumes, cancels, runs, gets and deletes jobs', async () => {
    const { admin, calls } = makeAdmin(c => (c.method === 'DELETE' ? new Response(null, { status: 204 }) : json(job())))
    for (const a of ['pause', 'resume', 'cancel', 'run'] as const) await admin.jobAction('j/1', a)
    await admin.getJob('j1')
    await expect(admin.deleteJob('j1')).resolves.toBeUndefined()
    expect(calls.map(c => `${c.method} ${c.url}`)).toEqual([
      'POST /jobs/j%2F1/pause', 'POST /jobs/j%2F1/resume', 'POST /jobs/j%2F1/cancel', 'POST /jobs/j%2F1/run', 'GET /jobs/j1', 'DELETE /jobs/j1'
    ])
  })

  it('reports a state change the job does not allow as 409', async () => {
    const { admin } = makeAdmin(() => json({ error: { code: 'Conflict', message: 'job j1 is running' } }, 409))
    await expect(admin.deleteJob('j1')).rejects.toMatchObject({ code: 'Conflict', status: 409, message: 'job j1 is running' })
  })
})

describe('job capabilities', () => {
  const caps = (over: Partial<Capabilities> = {}): Capabilities => ({ ...NO_CAPS, ...over })

  it('defaults to false everywhere capabilities are enumerated', () => {
    for (const c of [NO_CAPS, NO_CAPABILITIES]) {
      expect(c).toMatchObject({ viewJobs: false, manageJobs: false, updateJobs: false, manageSnapshots: false, revealResticCredentials: false, runSnapshots: false, restoreSnapshots: false })
    }
  })

  it('needs gos3 and the capability; a missing field (older server) hides jobs', () => {
    expect(canViewJobs('gos3', caps({ viewJobs: true }))).toBe(true)
    expect(canViewJobs('s3', caps({ viewJobs: true }))).toBe(false)
    expect(canManageJobs('gos3', caps({ viewJobs: true }))).toBe(false)
    expect(canManageJobs('gos3', caps({ manageJobs: true }))).toBe(true)
    // Job actions follow updateJobs (admin:UpdateJob), not manageJobs; older servers lack it.
    expect(canUpdateJobs('gos3', caps({ manageJobs: true }))).toBe(false)
    expect(canUpdateJobs('gos3', caps({ updateJobs: true }))).toBe(true)
    expect(canUpdateJobs('s3', caps({ updateJobs: true }))).toBe(false)
    const { updateJobs: _u, ...beforeUpdateJobs } = caps({ manageJobs: true })
    expect(canUpdateJobs('gos3', beforeUpdateJobs)).toBe(false)
    expect(serverHasJobs('gos3', caps())).toBe(true)
    const { viewJobs: _v, ...old } = caps()
    expect(serverHasJobs('gos3', old)).toBe(false)
    expect(serverHasJobs('s3', caps({ viewJobs: true }))).toBe(false)
  })
})

describe('job helpers', () => {
  it('describes states and kinds, unknown ones as sent', () => {
    expect(jobStateMeta('running')).toMatchObject({ label: 'Running', color: 'info' })
    expect(jobStateMeta('failed').color).toBe('error')
    expect(jobStateMeta('exploded')).toMatchObject({ label: 'exploded', color: 'neutral' })
    expect(jobKindMeta('mirror').label).toBe('Mirror')
    expect(jobKindMeta('restic-snapshot').label).toBe('Snapshot')
    expect(jobKindMeta('defrag').label).toBe('defrag')
  })

  it('describes what a job copies from its spec, else its labels', () => {
    expect(describeJob(job()).title).toBe('photos → photos-bak')
    const prefixed = describeJob(job({ spec: { kind: 'mirror', source: { bucket: 'a', prefix: 'x/' }, target: { bucket: 'b', prefix: 'y/' } } }))
    expect(prefixed.title).toBe('a/x/ → b/y/')
    expect(prefixed.buckets).toEqual(['a', 'b', 'photos', 'photos-bak'])
    expect(describeJob(job({ spec: undefined, labels: { source: 's', target: 't' } })).title).toBe('s → t')
    expect(describeJob(job({ kind: 'restic-snapshot', spec: {}, labels: { bucket: 'photos' } })).title).toBe('Snapshot of photos')
    expect(describeJob(job({ kind: 'restic-restore', spec: { target: { bucket: 'r' } }, labels: { source: 'photos' } })).title).toBe('Restore photos snapshot → r')
    expect(describeJob(job({ kind: 'copy', spec: null, labels: null })).title).toBe('Copy')
  })

  it('computes byte progress from bytesDone / totalBytes, indeterminate without a total', () => {
    expect(jobProgress(job())).toEqual({ percent: 50, done: 500, total: 1000 })
    expect(jobProgress(job({ status: { bytesDone: 3, totalBytes: 0 } }))).toEqual({ percent: null, done: 3, total: 0 })
    expect(jobProgress(job({ status: { bytesDone: 5000, totalBytes: 1000 } }))!.percent).toBe(100)
    expect(jobProgress(job({ state: 'succeeded', status: { bytesDone: 10, totalBytes: 1000 } }))!.percent).toBe(100)
    expect(jobProgress(job({ status: null }))).toEqual({ percent: null, done: 0, total: 0 })
    expect(jobProgress(job({ kind: 'restic-snapshot' }))).toBeNull()
  })

  it('normalizes statuses with missing counters', () => {
    expect(copyStatusOf(job({ status: { pending: 3 } }))).toMatchObject({ pending: 3, bytesDone: 0, totalKeys: 0, etaSeconds: 0 })
    expect(snapshotStatusOf(job({ kind: 'restic-snapshot', status: { keys: 4, snapshot: 'abc' } }))).toEqual({ keys: 4, bytes: 0, materialized: 0, partials: 0, forgotten: 0, snapshot: 'abc', repository: undefined })
    expect(snapshotStatusOf(job())).toBeUndefined()
  })

  it('formats ETAs with two units at most', () => {
    expect(formatEta(3725)).toBe('1h 2m')
    expect(formatEta(42)).toBe('42s')
    expect(formatEta(90061)).toBe('1d 1h')
    expect(formatEta(0.4)).toBe('< 1s')
    expect(formatEta(0)).toBe('')
    expect(formatEta(-3)).toBe('')
    expect(formatEta(Number.NaN)).toBe('')
    expect(formatEta(undefined)).toBe('')
  })

  it('shows the ETA of a running copy and the lag of a mirror', () => {
    expect(jobTiming(job())).toBe('about 5s left')
    expect(jobTiming(job({ state: 'paused' }))).toBe('')
    const mirror = (status: Record<string, unknown>, over: Partial<Job> = {}) => job({ kind: 'mirror', state: 'waiting', status, ...over })
    expect(jobTiming(mirror({ pending: 1200, lagSeconds: 125 }))).toBe(`${(1200).toLocaleString()} pending · lag 2m 5s`)
    expect(jobTiming(mirror({ pending: 0 }))).toBe('In sync')
    expect(jobTiming(job({ kind: 'restic-prune' }))).toBe('')
  })

  it('offers the actions each state allows', () => {
    expect(jobActions({ state: 'queued' })).toEqual(['pause', 'cancel'])
    expect(jobActions({ state: 'running' })).toEqual(['pause', 'cancel'])
    expect(jobActions({ state: 'waiting' })).toEqual(['run', 'pause', 'cancel'])
    expect(jobActions({ state: 'paused' })).toEqual(['resume', 'cancel'])
    expect(jobActions({ state: 'failed' })).toEqual(['resume', 'delete'])
    expect(jobActions({ state: 'succeeded' })).toEqual(['delete'])
    expect(jobActions({ state: 'canceled' })).toEqual(['delete'])
    expect(jobActions({ state: 'weird' })).toEqual([])
  })

  it('polls fast while a job is queued, running or a mirror has pending changes', () => {
    expect(jobBusy(job({ state: 'queued' }))).toBe(true)
    expect(jobBusy(job({ kind: 'mirror', state: 'waiting', status: { pending: 2 } }))).toBe(true)
    expect(jobBusy(job({ kind: 'mirror', state: 'waiting', status: { pending: 0 } }))).toBe(false)
    expect(jobBusy(job({ state: 'paused' }))).toBe(false)
    expect(jobPollInterval([job({ state: 'succeeded' }), job({ state: 'running' })])).toBe(JOB_POLL_FAST_MS)
    expect(jobPollInterval([job({ state: 'succeeded' })])).toBe(JOB_POLL_SLOW_MS)
    expect(jobPollInterval(null)).toBe(JOB_POLL_SLOW_MS)
    expect(JOB_POLL_FAST_MS).toBe(3000)
    expect(JOB_POLL_SLOW_MS).toBe(15000)
  })

  it('filters by kind, state (or active) and bucket, sorts newest first and upserts', () => {
    const jobs = [
      job({ id: 'a', state: 'succeeded', createdAt: '2026-10-15T09:00:00Z' }),
      job({ id: 'b', kind: 'mirror', state: 'waiting', createdAt: '2026-10-15T11:00:00Z', spec: { kind: 'mirror', source: { bucket: 'docs' }, target: { bucket: 'docs-mirror' } }, labels: {} }),
      job({ id: 'c', kind: 'restic-snapshot', state: 'failed', createdAt: '2026-10-15T10:00:00Z', spec: {}, labels: { bucket: 'photos' } })
    ]
    expect(filterJobs(jobs, { kind: 'mirror' }).map(j => j.id)).toEqual(['b'])
    expect(filterJobs(jobs, { state: 'active' }).map(j => j.id)).toEqual(['b'])
    expect(filterJobs(jobs, { state: 'failed' }).map(j => j.id)).toEqual(['c'])
    expect(filterJobs(jobs, { bucket: ' PHOTOS ' }).map(j => j.id)).toEqual(['a', 'c'])
    expect(filterJobs(jobs, { bucket: 'mirror' }).map(j => j.id)).toEqual(['b'])
    expect(filterJobs(jobs, {}).length).toBe(3)
    expect(sortJobs(jobs).map(j => j.id)).toEqual(['b', 'c', 'a'])
    expect(upsertJob(jobs, job({ id: 'b', state: 'paused' })).find(j => j.id === 'b')!.state).toBe('paused')
    expect(upsertJob(jobs, job({ id: 'd' }))[0]!.id).toBe('d')
  })
})

describe('Go durations', () => {
  it('parses Go durations (and days) to seconds', () => {
    expect(parseGoDuration('24h')).toBe(86400)
    expect(parseGoDuration('1h30m')).toBe(5400)
    expect(parseGoDuration('24h0m0s')).toBe(86400)
    expect(parseGoDuration('1.5h')).toBe(5400)
    expect(parseGoDuration('300ms')).toBe(0.3)
    expect(parseGoDuration('7d')).toBe(604800)
    expect(parseGoDuration('0')).toBe(0)
    expect(parseGoDuration('0s')).toBe(0)
    for (const bad of ['', '  ', 'h', '5', '-5m', '5 m', '5y', 'abc']) expect(parseGoDuration(bad), bad).toBeUndefined()
  })

  it('formats seconds with Go units and normalizes server values', () => {
    expect(formatGoDuration(86400)).toBe('24h')
    expect(formatGoDuration(5400)).toBe('1h30m')
    expect(formatGoDuration(30)).toBe('30s')
    expect(formatGoDuration(0.25)).toBe('250ms')
    expect(formatGoDuration(0)).toBe('0s')
    expect(durationText('24h0m0s')).toBe('24h')
    expect(durationText(300e9)).toBe('5m')
    expect(durationText('weird')).toBe('weird')
    expect(durationText(undefined, '1h')).toBe('1h')
  })
})

describe('copy wizard', () => {
  const backings: AvailableBackings = {
    default: 'default',
    available: [
      { name: 'default', kind: 'fs' },
      { name: 'cloud', kind: 's3', kinds: ['native', 'wrapped'], defaultKind: 'wrapped', containers: [{ id: 'home', default: true }, { id: 'c-1', label: 'archive' }] },
      { name: 'bare', kind: 's3', kinds: ['native', 'wrapped'], defaultKind: 'native', containers: [] }
    ]
  }
  const ctx = (over: Partial<CopyContext> = {}): CopyContext => ({
    source: { bucket: 'photos', versioning: 'Enabled', objectLock: false },
    buckets: ['photos', 'docs', 'native-bak'],
    backings,
    ...over
  })

  it('suggests a free target name', () => {
    expect(suggestedTargetName('photos')).toBe('photos-copy')
    expect(suggestedTargetName('photos', ['photos-copy'])).toBe('photos-copy-2')
    expect(suggestedTargetName('a'.repeat(60))).toBe('')
    expect(emptyCopyForm(ctx()).targetBucket).toBe('photos-copy')
    expect(emptyCopyForm(ctx({ source: { bucket: 'locked', versioning: 'Enabled', objectLock: true } })).objectLock).toBe(true)
  })

  it('builds a copy into a new wrapped bucket with the default container', () => {
    const c = ctx()
    const form = emptyCopyForm(c, { backing: 'cloud' })
    expect(targetKind(form, c)).toBe('wrapped')
    expect(validateCopyForm(form, c)).toEqual({})
    expect(copyJobSpec(form, c)).toEqual({
      kind: 'copy',
      source: { bucket: 'photos' },
      target: { bucket: 'photos-copy', create: { backing: 'cloud', kind: 'wrapped', container: 'home', objectLock: false } },
      options: { configs: 'safe', encryption: 'preserve', compression: 'preserve', concurrency: 4, finalPass: true }
    })
  })

  it('always sends objectLock for a new target, so the server never follows a locked source on its own', () => {
    const locked = ctx({ source: { bucket: 'photos', versioning: 'Enabled', objectLock: true } })
    // The switch defaults to the source's lock; turned off it is sent as false.
    expect(copyJobSpec(emptyCopyForm(locked), locked).target.create).toEqual({ objectLock: true })
    const off = emptyCopyForm(locked, { objectLock: false, dropRetention: true })
    expect(copyJobSpec(off, locked).target.create).toEqual({ objectLock: false })
    expect(copyJobSpec(emptyCopyForm(ctx()), ctx()).target.create).toHaveProperty('objectLock', false)
    // Existing targets have no create part at all.
    expect(copyJobSpec(emptyCopyForm(ctx(), { target: 'existing', targetBucket: 'docs' }), ctx()).target).not.toHaveProperty('create')
    const summary = (spec: ReturnType<typeof copyJobSpec>) => describeSpec(spec).find(l => l.label === 'New bucket')!.value
    expect(summary(copyJobSpec(off, locked))).toBe('default backing, no object lock')
    expect(summary(copyJobSpec(emptyCopyForm(locked), locked))).toBe('default backing, object lock')
  })

  it('builds a native target with its remote bucket, and a mirror into an existing bucket', () => {
    const c = ctx()
    const native = emptyCopyForm(c, { backing: 'cloud', kind: 'native', remoteBucket: 'acme-photos', objectLock: true, versions: 'all', sourcePrefix: '2024/', targetPrefix: 'in/' })
    expect(copyJobSpec(native, c)).toMatchObject({
      source: { bucket: 'photos', prefix: '2024/' },
      target: { bucket: 'photos-copy', prefix: 'in/', create: { backing: 'cloud', kind: 'native', remoteBucket: 'acme-photos', objectLock: true } },
      options: { versions: 'all' }
    })
    // The remote name is left out when it equals the bucket name (the server's default).
    expect(copyJobSpec({ ...native, remoteBucket: 'photos-copy' }, c).target.create).not.toHaveProperty('remoteBucket')

    const mirror = emptyCopyForm(c, { mode: 'mirror', target: 'existing', targetBucket: 'docs', onExisting: 'adopt', deleteExtraneous: true, pollInterval: '1d', verifyInterval: '12h0m0s', allowPlaintext: true })
    expect(validateCopyForm(mirror, c)).toEqual({})
    expect(copyJobSpec(mirror, c)).toEqual({
      kind: 'mirror',
      source: { bucket: 'photos' },
      target: { bucket: 'docs' },
      options: {
        configs: 'safe', encryption: 'preserve', compression: 'preserve', concurrency: 4, onExisting: 'adopt', allowPlaintext: true,
        propagateDeletes: true, deleteExtraneous: true, pollInterval: '24h', verifyInterval: '12h'
      }
    })
  })

  it('validates names, the source itself, prefixes, intervals and concurrency', () => {
    const c = ctx()
    const errs = (over: Parameters<typeof emptyCopyForm>[1]) => validateCopyForm(emptyCopyForm(c, over), c)
    expect(errs({ targetBucket: '' }).targetBucket).toBe('Enter a name for the new bucket')
    expect(errs({ targetBucket: 'photos' }).targetBucket).toBe('Choose another bucket than the source')
    expect(errs({ targetBucket: 'Bad_Name' }).targetBucket).toBeTruthy()
    expect(errs({ targetBucket: 'docs' }).targetBucket).toMatch(/exists/)
    expect(errs({ target: 'existing', targetBucket: 'nope' }).targetBucket).toBe('There is no bucket with this name')
    expect(errs({ target: 'existing', targetBucket: '' }).targetBucket).toBe('Choose a bucket')
    expect(validateCopyForm(emptyCopyForm(c, { target: 'existing', targetBucket: 'typed' }), { ...c, buckets: [] }).targetBucket).toBeUndefined()
    expect(errs({ sourcePrefix: 'a\u0001' }).sourcePrefix).toBeTruthy()
    expect(errs({ backing: 'cloud', kind: 'native', remoteBucket: 'UPPER' }).remoteBucket).toBeTruthy()
    expect(errs({ backing: 'bare', kind: 'wrapped' }).container).toMatch(/no container/)
    expect(errs({ mode: 'mirror', pollInterval: '10s' }).pollInterval).toBe('At least 30s')
    expect(errs({ mode: 'mirror', pollInterval: 'often' }).pollInterval).toBeTruthy()
    expect(errs({ mode: 'mirror', verifyInterval: '' }).verifyInterval).toBeTruthy()
    expect(errs({ mode: 'copy', pollInterval: 'often' }).pollInterval).toBeUndefined()
    expect(errs({ concurrency: 0 }).concurrency).toBeTruthy()
    expect(errs({ concurrency: 33 }).concurrency).toBeTruthy()
    expect(errs({ concurrency: 2.5 }).concurrency).toBeTruthy()
    expect(errs({ target: 'existing', targetBucket: 'docs', onExisting: 'adopt' }).onExisting).toMatch(/mirror/)
    const target = errs({ targetBucket: '' })
    expect(stepValid(target, TARGET_STEP_FIELDS)).toBe(false)
    expect(stepValid(errs({ concurrency: 0 }), TARGET_STEP_FIELDS)).toBe(true)
  })

  it('needs object lock on the target or dropRetention for a locked source', () => {
    const c = ctx({ source: { bucket: 'photos', versioning: 'Enabled', objectLock: true } })
    expect(validateCopyForm(emptyCopyForm(c), c).dropRetention).toBeUndefined()
    expect(validateCopyForm(emptyCopyForm(c, { objectLock: false }), c).dropRetention).toMatch(/object lock/)
    expect(validateCopyForm(emptyCopyForm(c, { objectLock: false, dropRetention: true }), c).dropRetention).toBeUndefined()
    const existing = { ...c, target: { objectLock: false } }
    expect(validateCopyForm(emptyCopyForm(existing, { target: 'existing', targetBucket: 'docs' }), existing).dropRetention).toBeTruthy()
    const unknown = { ...c, target: { objectLock: null } }
    expect(validateCopyForm(emptyCopyForm(unknown, { target: 'existing', targetBucket: 'docs' }), unknown).dropRetention).toBeUndefined()
  })

  it('disables "all versions" with the reason when the target cannot keep them', () => {
    const native = ctx({ target: { kind: 'native', versioning: '' } })
    const form = emptyCopyForm(native, { target: 'existing', targetBucket: 'native-bak' })
    expect(allVersionsRefusal(form, native)).toMatch(/native bucket without versioning/)
    const all = versionsItems(form, native).find(i => i.value === 'all')!
    expect(all.disabled).toBe(true)
    expect(all.description).toMatch(/enable versioning/)
    expect(validateCopyForm({ ...form, versions: 'all' }, native).versions).toBeTruthy()

    const unsupported = ctx({ target: { kind: 'wrapped', versioningUnsupported: true } })
    expect(allVersionsRefusal(form, unsupported)).toMatch(/cannot keep versions/)
    const versioned = ctx({ target: { kind: 'native', versioning: 'Enabled' } })
    expect(allVersionsRefusal(form, versioned)).toBe('')
    // Not yet known: allowed (the dry run tells).
    expect(allVersionsRefusal(form, ctx({ target: { kind: 'native', versioning: null } }))).toBe('')
    // New buckets in gos3's format keep every version; a new native one gets a hint.
    expect(versionsItems(emptyCopyForm(ctx()), ctx()).find(i => i.value === 'all')!.disabled).toBeFalsy()
    const nativeNew = emptyCopyForm(ctx(), { backing: 'cloud', kind: 'native' })
    expect(versionsItems(nativeNew, ctx()).find(i => i.value === 'all')!.description).toMatch(/remote bucket/)
    const unversioned = ctx({ source: { bucket: 'photos', versioning: '', objectLock: false } })
    expect(versionsItems(emptyCopyForm(unversioned), unversioned).find(i => i.value === 'all')!.description).toMatch(/not versioned/)
  })

  it('hints at plaintext copies to native buckets and destructive mirror options', () => {
    const c = ctx()
    expect(copyHints(emptyCopyForm(c, { backing: 'cloud', kind: 'native' }), c).join(' ')).toMatch(/plaintext/)
    expect(copyHints(emptyCopyForm(c, { backing: 'cloud', kind: 'native', allowPlaintext: true }), c)).toEqual([])
    expect(copyHints(emptyCopyForm(c, { mode: 'mirror', deleteExtraneous: true }), c).join(' ')).toMatch(/deletes keys/)
    expect(copyHints(emptyCopyForm(c, { versions: 'current' }), c).join(' ')).toMatch(/Only current versions/)
  })

  it('summarizes a normalized spec for the review step', () => {
    const lines = describeSpec({
      kind: 'mirror',
      source: { bucket: 'photos', prefix: 'a/' },
      target: { bucket: 'bak', create: { backing: 'cloud', kind: 'native', remoteBucket: 'acme', objectLock: true } },
      options: { versions: 'all', configs: 'none', encryption: 'target', allowPlaintext: true, propagateDeletes: false, pollInterval: '5m', verifyInterval: '24h', concurrency: 8 }
    })
    const byLabel = Object.fromEntries(lines.map(l => [l.label, l.value]))
    expect(byLabel).toMatchObject({
      'Job': 'Mirror (continuous)',
      'Source': 'photos/a/',
      'Target': 'bak (new bucket)',
      'New bucket': 'backing cloud, native, remote bucket acme, object lock',
      'Versions': 'All versions',
      'Settings': 'None',
      'Encryption': 'target settings, plaintext allowed',
      'Mirror': 'deletes not propagated, poll 5m, verify 24h',
      'Parallel keys': '8'
    })
    expect(byLabel['Existing target']).toBeUndefined()
    const copy = Object.fromEntries(describeSpec({ kind: 'copy', source: { bucket: 'a' }, target: { bucket: 'b' }, options: { onExisting: 'merge', finalPass: false } }).map(l => [l.label, l.value]))
    expect(copy).toMatchObject({ 'Existing target': 'Copy without deleting', 'Final pass': 'No', 'Versions': 'Automatic' })
  })
})
