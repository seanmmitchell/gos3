// Remote buckets and containers of s3 backings: the admin client calls, the
// capability gating and the pure helpers behind the backing page's remote
// bucket browser, its containers list and their modals.
import { describe, expect, it, vi } from 'vitest'
import { AdminClient, AdminError } from '~/utils/admin/client'
import type { Capabilities, ContainerInfo, RemoteBucket } from '~/types/admin'
import { NO_CAPS, remoteAccess, type RemoteAccess } from '~/components/admin/-capabilities'
import {
  addedContainerFromError,
  adoptRequest,
  createRemoteRequest,
  describeUse,
  incompleteListMessage,
  nativeBucketOf,
  noErrors,
  remoteBucketActions,
  remoteNameNote,
  remoteStatusMeta,
  requestReachedServer,
  suggestedBucketName,
  validateAdoptForm,
  validateCreateRemoteForm,
  validatePrefix,
  validateRemoteBucketName,
  validateWrappedForm,
  wrappedRequest,
  type WrappedForm
} from '~/components/admin/-remote-buckets'
import {
  canBeDefault,
  containerName,
  containerRemoveRefusal,
  containerRoleLabel,
  containerSelectItems,
  containerSpec,
  containerStateMeta,
  emptyContainerForm,
  preferredContainer,
  remoteLocationUri,
  validateContainerForm
} from '~/components/admin/-containers'

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

const container = (id: string, over: Partial<ContainerInfo> = {}): ContainerInfo => ({
  id, location: { bucket: `bucket-${id}`, prefix: 'gos3/' }, role: 'dedicated', buckets: 0, state: 'ready', ...over
})

const ALL: RemoteAccess = { browse: true, create: true, adopt: true, containers: true, release: true }
const NONE: RemoteAccess = { browse: false, create: false, adopt: false, containers: false, release: false }

describe('AdminClient remote buckets and containers', () => {
  it('lists remote buckets with the query, tolerating a null list', async () => {
    const { admin, calls } = makeAdmin(() => json({ buckets: null, complete: false, listError: 'AccessDenied: no ListAllMyBuckets' }))
    expect(await admin.listRemoteBuckets('cloud')).toEqual({ buckets: [], complete: false, listError: 'AccessDenied: no ListAllMyBuckets' })
    await admin.listRemoteBuckets('my cloud', { prefix: 'acme-', continuationToken: 't/1', maxBuckets: 100, probe: true })
    await admin.listRemoteBuckets('cloud', { prefix: '', probe: false })
    expect(calls.map(c => `${c.method} ${c.url}`)).toEqual([
      'GET /backings/cloud/remote-buckets',
      'GET /backings/my%20cloud/remote-buckets?prefix=acme-&continuationToken=t%2F1&maxBuckets=100&probe=true',
      'GET /backings/cloud/remote-buckets'
    ])
  })

  it('creates remote buckets, adds, defaults and removes containers', async () => {
    const { admin, calls } = makeAdmin((c) => {
      if (c.method === 'DELETE') return new Response(null, { status: 204 })
      if (c.url.endsWith('/containers/default')) return json({ containers: [container('home'), container('c-1', { default: true })] })
      if (c.url.endsWith('/containers') && c.method === 'GET') return json({ containers: null })
      return json(c.url.includes('remote-buckets') ? { name: 'acme-new', status: 'unmanaged' } : container('c-1'), 201)
    })
    expect(await admin.createRemoteBucket('cloud', { name: 'acme-new', objectLock: true })).toMatchObject({ name: 'acme-new' })
    expect(await admin.listContainers('cloud')).toEqual([])
    expect(await admin.addContainer('cloud', { bucket: 'acme-archive', prefix: 'gos3/', createBucket: true })).toMatchObject({ id: 'c-1' })
    expect((await admin.setDefaultContainer('cloud', 'c-1')).map(c => c.id)).toEqual(['home', 'c-1'])
    await admin.removeContainer('cloud', 'c-1')
    expect(calls.map(c => [c.method, c.url, c.body])).toEqual([
      ['POST', '/backings/cloud/remote-buckets', '{"name":"acme-new","objectLock":true}'],
      ['GET', '/backings/cloud/containers', ''],
      ['POST', '/backings/cloud/containers', '{"bucket":"acme-archive","prefix":"gos3/","createBucket":true}'],
      ['PUT', '/backings/cloud/containers/default', '{"id":"c-1"}'],
      ['DELETE', '/backings/cloud/containers/c-1', '']
    ])
    for (const c of calls) expect(c.payloadHash).toMatch(/^[0-9a-f]{64}$/)
  })

  it('creates buckets on a backing and releases native buckets', async () => {
    const { admin, calls } = makeAdmin(c => (c.url.endsWith('/release') ? new Response(null, { status: 204 }) : json({ name: 'photos', kind: 'native' }, 201)))
    expect(await admin.createBucketOnBacking('cloud', { bucket: 'photos', kind: 'native', remoteBucket: 'acme-photos', adopt: true })).toMatchObject({ kind: 'native' })
    await admin.releaseBucket('photos')
    expect(calls.map(c => [c.method, c.url, c.body])).toEqual([
      ['POST', '/backings/cloud/buckets', '{"bucket":"photos","kind":"native","remoteBucket":"acme-photos","adopt":true}'],
      ['POST', '/buckets/photos/release', '']
    ])
  })

  it('tells failures that reached the server and names a container the server kept', () => {
    expect(requestReachedServer(new AdminError('Conflict', 'exists', 409))).toBe(true)
    expect(requestReachedServer(new AdminError('Timeout', 'timed out', 0))).toBe(true)
    expect(requestReachedServer(new AdminError('NetworkError', 'unreachable', 0))).toBe(false)
    expect(requestReachedServer(new Error('not connected'))).toBe(false)
    const kept = new AdminError('Conflict', 'bucket exists (container "c-9a1b" was added and is kept; retry with "container": "c-9a1b")', 409)
    expect(addedContainerFromError(kept)).toBe('c-9a1b')
    expect(addedContainerFromError(new AdminError('Conflict', 'bucket exists', 409))).toBeUndefined()
    expect(addedContainerFromError('nope')).toBeUndefined()
  })

  it('surfaces conflicts as AdminError', async () => {
    const { admin } = makeAdmin(() => json({ error: { code: 'Conflict', message: 'the remote bucket exists: adopt it instead' } }, 409))
    await expect(admin.createBucketOnBacking('cloud', { bucket: 'photos', kind: 'native' })).rejects.toMatchObject({ code: 'Conflict', status: 409 })
  })
})

describe('remote access capabilities', () => {
  const caps = (over: Partial<Capabilities>): Capabilities => ({ ...NO_CAPS, ...over })

  it('maps whoami capabilities, for gos3 only', () => {
    expect(remoteAccess('gos3', caps({ admin: true, browseRemote: true, adoptRemote: true }))).toEqual({ browse: true, create: false, adopt: true, containers: false, release: true })
    expect(remoteAccess('gos3', caps({ createRemote: true, manageContainers: true }))).toEqual({ browse: false, create: true, adopt: false, containers: true, release: false })
    expect(remoteAccess('s3', caps({ admin: true, browseRemote: true }))).toEqual(NONE)
    // Older servers do not report them.
    const { browseRemote: _b, createRemote: _c, adoptRemote: _a, manageContainers: _m, ...old } = caps({ admin: true })
    expect(remoteAccess('gos3', old)).toEqual({ ...NONE, release: true })
    expect(NO_CAPS).toMatchObject({ browseRemote: false, createRemote: false, adoptRemote: false, manageContainers: false })
  })
})

describe('remote bucket status and actions', () => {
  const rb = (status: string, over: Partial<RemoteBucket> = {}): RemoteBucket => ({ name: 'acme', status, ...over })
  const actions = (b: RemoteBucket, a: RemoteAccess) => remoteBucketActions(b, a).map(x => x.action)

  it('maps statuses to badges, unknown ones neutral', () => {
    expect(remoteStatusMeta('native')).toMatchObject({ label: 'Native bucket', color: 'primary' })
    expect(remoteStatusMeta('unmanaged')).toMatchObject({ label: 'Unmanaged', color: 'neutral' })
    expect(remoteStatusMeta('inaccessible').color).toBe('error')
    expect(remoteStatusMeta('claimed').color).toBe('warning')
    expect(remoteStatusMeta('weird')).toMatchObject({ label: 'weird', color: 'neutral', icon: 'i-lucide-circle-help' })
  })

  it('offers actions by status and permission', () => {
    const native = rb('native', { uses: [{ role: 'native', bucket: 'photos', created: false }] })
    expect(actions(native, ALL)).toEqual(['open', 'release'])
    expect(actions(native, NONE)).toEqual(['open'])
    expect(actions(rb('native'), ALL)).toEqual([])
    expect(actions(rb('unmanaged'), ALL)).toEqual(['adopt', 'use-as-container', 'create-wrapped'])
    expect(actions(rb('unmanaged'), { ...NONE, adopt: true })).toEqual(['adopt'])
    expect(actions(rb('unmanaged'), { ...ALL, adopt: false })).toEqual([])
    expect(actions(rb('gos3'), ALL)).toEqual(['attach-container'])
    expect(actions(rb('gos3'), { ...ALL, containers: false })).toEqual([])
    expect(actions(rb('home'), NONE)).toEqual(['containers'])
    expect(actions(rb('container'), NONE)).toEqual(['containers'])
    expect(actions(rb('claimed'), ALL)).toEqual([])
    expect(actions(rb('inaccessible'), ALL)).toEqual([])
  })

  it('offers nothing to adopt or wrap for names the server does not accept', () => {
    expect(actions(rb('unmanaged', { name: 'Legacy_Bucket' }), ALL)).toEqual([])
    expect(actions(rb('gos3', { name: 'Old.Store' }), ALL)).toEqual([])
    expect(actions(rb('unmanaged', { name: 'legacy_bucket' }), ALL)).toEqual(['adopt', 'use-as-container', 'create-wrapped'])
    expect(remoteNameNote('Legacy_Bucket')).toMatch(/gos3 cannot use this bucket/)
    expect(remoteNameNote('legacy_bucket')).toBe('')
  })

  it('describes uses and finds the native gos3 bucket', () => {
    expect(describeUse({ role: 'native', bucket: 'photos', created: false })).toBe('gos3 bucket photos (adopted)')
    expect(describeUse({ role: 'native', bucket: 'docs', created: true })).toBe('gos3 bucket docs (created by gos3)')
    expect(describeUse({ role: 'container', containerId: 'c-7f3a', prefix: 'gos3/' })).toBe('Container c-7f3a at gos3/')
    expect(describeUse({ role: 'home', containerId: 'home' })).toBe('Home of this backing')
    expect(describeUse({ role: 'claimed', backing: 'other' })).toBe('Used by backing other')
    expect(describeUse({ role: 'mirror' })).toBe('mirror')
    expect(nativeBucketOf(rb('native', { uses: [{ role: 'container', containerId: 'c' }, { role: 'native', bucket: 'photos' }] }))).toBe('photos')
    expect(nativeBucketOf(rb('unmanaged'))).toBeUndefined()
  })

  it('explains incomplete lists', () => {
    expect(incompleteListMessage({ complete: true })).toBe('')
    expect(incompleteListMessage(null)).toBe('')
    expect(incompleteListMessage({ complete: false, listError: 'AccessDenied' })).toMatch(/^These credentials cannot list the account's buckets \(AccessDenied\)\. .*adopt other buckets by name\.$/)
    expect(incompleteListMessage({ complete: false })).not.toContain('(')
  })
})

describe('remote bucket forms', () => {
  it('validates remote bucket names strictly for new buckets and like the server for existing ones', () => {
    expect(validateRemoteBucketName('', false)).toBeTruthy()
    // Existing buckets: ^[a-z0-9][a-z0-9._-]{1,254}$ (internal/admin/remotebuckets.go).
    for (const ok of ['legacy_bucket', 'ab', 'a.b-c_d', `a${'b'.repeat(254)}`]) expect(validateRemoteBucketName(ok, false)).toBeUndefined()
    for (const bad of ['Legacy_Bucket', 'a', '_ab', '-ab', 'a b', 'a/b', `a${'b'.repeat(255)}`]) expect(validateRemoteBucketName(bad, false)).toBeTruthy()
    // New buckets: the S3 naming rules.
    expect(validateRemoteBucketName('legacy_bucket', true)).toBeTruthy()
    expect(validateRemoteBucketName('acme-new', true)).toBeUndefined()
  })

  it.each([['', true], ['gos3/', true], ['a/b/', true], ['a/b', true], ['/gos3/', false], ['a//b/', false], ['a/../b/', false], ['./', false], ['x'.repeat(513), false]])('validates prefix %j', (prefix, ok) => {
    expect(!validatePrefix(prefix)).toBe(ok)
  })

  it('suggests a gos3 name for a remote bucket', () => {
    expect(suggestedBucketName('acme-photos')).toBe('acme-photos')
    expect(suggestedBucketName('legacy_bucket')).toBe('legacy-bucket')
    expect(suggestedBucketName('old..name_')).toBe('old.name')
    expect(suggestedBucketName('')).toBe('')
  })

  it('validates the adopt form and builds its request', () => {
    const form = { remoteBucket: 'legacy_bucket', bucket: 'legacy', region: '' }
    expect(noErrors(validateAdoptForm(form))).toBe(true)
    expect(adoptRequest(form)).toEqual({ bucket: 'legacy', kind: 'native', remoteBucket: 'legacy_bucket', adopt: true })
    expect(validateAdoptForm({ ...form, remoteBucket: 'Legacy_Bucket' }).remoteBucket).toBeTruthy()
    expect(adoptRequest({ ...form, region: ' eu-central-1 ' })).toMatchObject({ region: 'eu-central-1' })
    expect(validateAdoptForm({ remoteBucket: '', bucket: 'Bad_Name', region: 'EU' })).toEqual({
      remoteBucket: expect.any(String), bucket: expect.any(String), region: expect.any(String)
    })
    expect(validateAdoptForm({ remoteBucket: 'acme', bucket: 'taken', region: '' }, ['taken']).bucket).toMatch(/already/)
  })

  it('validates the create-remote form and builds its request', () => {
    expect(validateCreateRemoteForm({ name: 'Bad_Name', region: '', objectLock: false }).name).toBeTruthy()
    expect(noErrors(validateCreateRemoteForm({ name: 'acme-new', region: 'eu-central-1', objectLock: true }))).toBe(true)
    expect(createRemoteRequest({ name: ' acme-new ', region: '', objectLock: false })).toEqual({ name: 'acme-new' })
    expect(createRemoteRequest({ name: 'acme-new', region: 'eu-central-1', objectLock: true })).toEqual({ name: 'acme-new', region: 'eu-central-1', objectLock: true })
  })

  it('validates the wrapped-bucket form and builds its request', () => {
    const base: WrappedForm = { bucket: 'docs', target: 'existing', container: 'c-7f3a', remoteBucket: '', prefix: '', label: '', region: '' }
    expect(noErrors(validateWrappedForm(base))).toBe(true)
    expect(wrappedRequest(base)).toEqual({ bucket: 'docs', kind: 'wrapped', container: 'c-7f3a' })
    expect(validateWrappedForm({ ...base, container: '' }).container).toBeTruthy()
    const fresh: WrappedForm = { ...base, target: 'new', container: '', remoteBucket: 'acme-media', prefix: 'gos3/', label: ' media ', region: 'eu-1' }
    expect(noErrors(validateWrappedForm(fresh))).toBe(true)
    expect(wrappedRequest(fresh)).toEqual({ bucket: 'docs', kind: 'wrapped', newContainer: { bucket: 'acme-media', prefix: 'gos3/', label: 'media', region: 'eu-1' } })
    expect(wrappedRequest({ ...fresh, prefix: '', label: '', region: '' })).toEqual({ bucket: 'docs', kind: 'wrapped', newContainer: { bucket: 'acme-media' } })
    expect(validateWrappedForm({ ...fresh, prefix: '/x' }).prefix).toBeTruthy()
    expect(validateWrappedForm({ ...fresh, label: 'x'.repeat(129) }).label).toBeTruthy()
    // 128 bytes of UTF-8, as the server counts.
    expect(validateWrappedForm({ ...fresh, label: 'x'.repeat(128) }).label).toBeUndefined()
    expect(validateWrappedForm({ ...fresh, label: 'é'.repeat(65) }).label).toBe('At most 128 bytes')
    expect(validateWrappedForm({ ...fresh, label: 'é'.repeat(64) }).label).toBeUndefined()
    expect(validateWrappedForm({ ...fresh, bucket: '' }).bucket).toBeTruthy()
  })
})

describe('containers', () => {
  it('describes states, roles, locations and names', () => {
    expect(containerStateMeta('ready')).toMatchObject({ label: 'Ready', color: 'success' })
    expect(containerStateMeta('failed').color).toBe('error')
    expect(containerStateMeta('odd')).toMatchObject({ label: 'odd', color: 'neutral' })
    expect(containerRoleLabel('existing')).toBe('Existing bucket')
    expect(containerRoleLabel('other')).toBe('other')
    expect(remoteLocationUri({ bucket: 'acme', prefix: 'gos3/' })).toBe('s3://acme/gos3/')
    expect(remoteLocationUri({ bucket: 'acme' })).toBe('s3://acme/')
    expect(remoteLocationUri(null)).toBe('')
    expect(containerName({ id: 'home' })).toBe('shared (home)')
    expect(containerName({ id: 'c-1', label: 'archive' })).toBe('archive')
  })

  it('removes only empty containers that are neither the home nor the default', () => {
    expect(containerRemoveRefusal(container('home', { role: 'home' }))).toMatch(/home/)
    expect(containerRemoveRefusal(container('c-1', { buckets: 2 }))).toMatch(/2 wrapped buckets/)
    expect(containerRemoveRefusal(container('c-1', { buckets: 1 }))).toMatch(/1 wrapped bucket;/)
    expect(containerRemoveRefusal(container('c-1', { default: true }))).toBe('It is the default container; make another one the default first.')
    expect(containerRemoveRefusal(container('c-1'))).toBe('')
    expect(canBeDefault(container('c-1'))).toBe(true)
    expect(canBeDefault(container('c-1', { default: true }))).toBe(false)
    expect(canBeDefault(container('c-1', { state: 'loading' }))).toBe(false)
  })

  it('offers ready containers in pickers, preferring the default', () => {
    const list = [container('home', { role: 'home', label: 'shared', default: true, state: 'failed' }), container('c-1', { label: 'archive' }), container('c-2', { state: 'loading' })]
    expect(containerSelectItems(list)).toEqual([
      { label: 'shared (default) (failed)', value: 'home', description: 's3://bucket-home/gos3/', disabled: true },
      { label: 'archive', value: 'c-1', description: 's3://bucket-c-1/gos3/' },
      { label: 'c-2 (loading)', value: 'c-2', description: 's3://bucket-c-2/gos3/', disabled: true }
    ])
    expect(preferredContainer(list, 'home')).toBe('c-1')
    expect(preferredContainer([container('a'), container('b', { default: true })])).toBe('b')
    expect(preferredContainer([container('a'), container('b')], 'b')).toBe('b')
    expect(preferredContainer([])).toBe('')
  })

  it('validates the add-container form and builds its spec', () => {
    expect(containerSpec(emptyContainerForm({ bucket: ' acme-archive ', source: 'create' }))).toEqual({ bucket: 'acme-archive', createBucket: true })
    expect(containerSpec(emptyContainerForm({ bucket: 'legacy_bucket', prefix: 'gos3/', region: 'eu-1', attach: true, label: 'old' }))).toEqual({
      bucket: 'legacy_bucket', prefix: 'gos3/', region: 'eu-1', attach: true, label: 'old'
    })
    // attach only applies to existing buckets.
    expect(containerSpec(emptyContainerForm({ bucket: 'acme', source: 'create', attach: true }))).toEqual({ bucket: 'acme', createBucket: true })
    expect(noErrors(validateContainerForm(emptyContainerForm({ bucket: 'legacy_bucket' })))).toBe(true)
    expect(validateContainerForm(emptyContainerForm({ bucket: 'Legacy_Bucket' })).bucket).toBeTruthy()
    expect(validateContainerForm(emptyContainerForm({ bucket: 'legacy_bucket', source: 'create' })).bucket).toBeTruthy()
    expect(validateContainerForm(emptyContainerForm({ bucket: 'acme', prefix: '../x/', region: 'EU', label: 'a\nb' }))).toEqual({
      prefix: expect.any(String), region: expect.any(String), label: expect.any(String)
    })
  })
})
