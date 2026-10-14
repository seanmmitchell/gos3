// Admin pages + bucket settings E2E against a gos3 server mocked with
// Playwright request interception (admin JSON API + the S3 calls they use).
import { createHash } from 'node:crypto'
import { expect, test } from '@playwright/test'
import type { Page, Route } from '@playwright/test'

const ENDPOINT = 'http://gos3.mock.test'
const S3_ENDPOINT = 'http://s3only.mock.test'
const CONN_ID = 'g'
const S3_CONN_ID = 's'
const ADMIN = '/_gos3/admin/v1'

const CORS = {
  'access-control-allow-origin': '*',
  'access-control-allow-methods': 'GET,PUT,POST,DELETE,HEAD,PATCH',
  'access-control-allow-headers': '*',
  'access-control-expose-headers': 'ETag, x-amz-version-id'
}

const now = Date.now()
const minute = (ago: number) => new Date(Math.floor(now / 60000) * 60000 - ago * 60000).toISOString()

const INFO = {
  service: 'gos3',
  id: '0123456789abcdef',
  name: 'mock-gos3',
  version: '1.2.3',
  apiVersion: 'v1',
  region: 'eu-test-1',
  endpoints: { s3: ENDPOINT, admin: `${ENDPOINT}${ADMIN}`, ui: `${ENDPOINT}/_gos3/ui/` },
  features: ['versioning', 'object-lock', 'admin'],
  startedAt: new Date(now - 3600_000).toISOString()
}

// Root: createOwnKeys is always false (root has no IAM keys).
const ALL_CAPS = { admin: true, viewInfo: true, viewStats: true, manageConfig: true, manageUsers: true, managePolicies: true, manageBuckets: true, viewStorage: true, manageStorage: true, discovery: true, scan: true, probe: true, createOwnKeys: false }

interface MockMember { backing: string, role: string, state: string, healthy: boolean, pending: number, progress: { keys: number, versions: number, bytes: number } | null, lastSyncAt?: string, lastError: string }
interface MockPlacement { bucket: string, mode: string, primary: string, members: MockMember[], move: { from: string, to: string, startedAt: string, phase: string } | null }

const BACKINGS = [
  { name: 'default', kind: 'fs', path: '/data', default: true, healthy: true, lastError: '', checkedAt: new Date(now - 5000).toISOString(), buckets: 1, replicas: 0, disk: { path: '/data', totalBytes: 100_000_000_000, freeBytes: 60_000_000_000, usedBytes: 40_000_000_000 } },
  { name: 'fast', kind: 'fs', path: '/mnt/nvme', default: false, healthy: true, lastError: '', checkedAt: new Date(now - 5000).toISOString(), buckets: 0, replicas: 0, disk: { path: '/mnt/nvme', totalBytes: 1_000_000_000_000, freeBytes: 900_000_000_000, usedBytes: 100_000_000_000 } },
  { name: 'archive', kind: 'fs', path: '/mnt/archive', default: false, healthy: true, lastError: '', checkedAt: new Date(now - 5000).toISOString(), buckets: 0, replicas: 0, disk: null },
  { name: 'usb', kind: 'fs', path: '/mnt/usb', default: false, healthy: false, lastError: 'stat /mnt/usb: no such file or directory', checkedAt: new Date(now - 5000).toISOString(), buckets: 0, replicas: 0, disk: null }
]

const member = (backing: string, role: string, over: Partial<MockMember> = {}): MockMember => ({ backing, role, state: 'in-sync', healthy: true, pending: 0, progress: null, lastError: '', ...over })

function placementSummary(p: MockPlacement) {
  return { mode: p.mode, primary: p.primary, replicas: p.members.filter(m => m.role === 'replica').map(m => m.backing), moving: !!p.move }
}

interface MockState {
  log: string[]
  bodies: Record<string, string>
  /** Admin requests: payload hash header and the headers covered by the signature. */
  signing: Record<string, { payloadHash: string, signedHeaders: string }>
  users: { name: string, description?: string, disabled: boolean, policies: string[], keys: { accessKey: string, status: string, createdAt: string, description?: string }[], createdAt: string, updatedAt: string }[]
  policies: { name: string, description?: string, builtin: boolean, document: unknown, createdAt: string, updatedAt: string }[]
  settings: Record<string, unknown>
  lifecycle: string | null
  cors: string | null
  placement: MockPlacement
}

function initialState(): MockState {
  return {
    log: [],
    bodies: {},
    signing: {},
    users: [{
      name: 'alice',
      description: 'Backup job',
      disabled: false,
      policies: ['readwrite'],
      keys: [{ accessKey: 'AKIAALICE0000000001', status: 'Active', createdAt: '2026-01-01T00:00:00Z', description: 'laptop' }],
      createdAt: '2026-01-01T00:00:00Z',
      updatedAt: '2026-01-02T00:00:00Z'
    }],
    policies: [
      { name: 'readwrite', builtin: true, document: { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: ['s3:*'], Resource: ['*'] }] }, createdAt: '', updatedAt: '' },
      { name: 'photos-ro', description: 'Read photos', builtin: false, document: { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: ['s3:GetObject'], Resource: ['arn:aws:s3:::photos/*'] }] }, createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-01T00:00:00Z' }
    ],
    settings: {
      name: 'mock-gos3',
      region: 'eu-test-1',
      publicUrl: '',
      domains: null,
      publicInfo: true,
      uiRedirect: true,
      cors: { allowedOrigins: ['*'], allowedHeaders: ['*'], exposeHeaders: ['ETag'], maxAgeSeconds: 3600 },
      discovery: { enabled: true, advertise: true, mdns: true, beacon: true, beaconGroup: '239.255.73.83:7373', beaconInterval: 10_000_000_000, seeds: null, dnsSrv: null, peerTtl: 60_000_000_000, allowScan: true, maxScanHosts: 1024 },
      logging: { level: 'info', format: 'text', accessLog: true },
      lifecycle: { enabled: true, intervalSeconds: 3600 },
      metricsPublic: false,
      compression: { mode: 'optional', level: 1, minSize: 4096, skipContentTypes: ['image/jpeg', 'video/*'] }
    },
    lifecycle: null,
    cors: null,
    placement: { bucket: 'photos', mode: 'single', primary: 'default', members: [member('default', 'primary')], move: null }
  }
}

function stats() {
  return {
    metrics: {
      startedAt: INFO.startedAt,
      uptimeSeconds: 3600,
      requests: 1234,
      errors4xx: 7,
      errors5xx: 1,
      bytesIn: 5_000_000,
      bytesOut: 12_000_000,
      inFlight: 2,
      ops: [
        { op: 'GetObject', requests: 1000, errors4xx: 5, errors5xx: 0, bytesIn: 0, bytesOut: 12_000_000, avgLatencyMs: 3, p50LatencyMs: 2, p99LatencyMs: 40 },
        { op: 'PutObject', requests: 234, errors4xx: 2, errors5xx: 1, bytesIn: 5_000_000, bytesOut: 0, avgLatencyMs: 12, p50LatencyMs: 9, p99LatencyMs: 120 }
      ],
      recent: [5, 4, 3, 2, 1, 0].map((ago, i) => ({ minute: minute(ago), requests: 10 * (i + 1), errors: i % 2, bytesIn: 1000 * i, bytesOut: 2000 * i }))
    },
    storage: { buckets: 2, objects: 1200, bytes: 123_456_789, versions: 1300, versionBytes: 133_456_789, deleteMarkers: 4, pendingUploads: 1, computedAt: new Date(now).toISOString() },
    runtime: { goroutines: 42, heapAllocBytes: 10_000_000, sysBytes: 50_000_000, gcPauseTotalNs: 1_500_000 },
    disk: { path: '/data', totalBytes: 100_000_000_000, freeBytes: 60_000_000_000, usedBytes: 40_000_000_000 }
  }
}

async function mockGos3(page: Page): Promise<MockState> {
  const st = initialState()
  await page.route(`${ENDPOINT}/**`, async (route: Route) => {
    const req = route.request()
    const url = new URL(req.url())
    const method = req.method()
    const path = url.pathname
    const json = (status: number, body?: unknown) =>
      route.fulfill({ status, body: body === undefined ? '' : JSON.stringify(body), headers: { ...CORS, 'content-type': 'application/json' } })
    const xml = (status: number, body = '') => route.fulfill({ status, body, headers: { ...CORS, 'content-type': 'application/xml' } })
    if (method === 'OPTIONS') return route.fulfill({ status: 204, headers: CORS })
    st.log.push(`${method} ${path}${url.search}`)
    const body = req.postData() ?? ''
    if (body) st.bodies[`${method} ${path}${url.search}`] = body

    if (path === '/_gos3/info') return json(200, INFO)
    if (path.startsWith(ADMIN)) {
      const h = req.headers()
      st.signing[`${method} ${path}${url.search}`] = {
        payloadHash: h['x-amz-content-sha256'] ?? '',
        signedHeaders: /SignedHeaders=([^,]+)/.exec(h.authorization ?? '')?.[1] ?? ''
      }
      const p = path.slice(ADMIN.length)
      if (p === '/whoami') return json(200, { user: '', accessKey: 'ROOTKEY', isRoot: true, policies: [], capabilities: ALL_CAPS, server: INFO })
      if (p === '/info') return json(200, { ...INFO, backend: 'fs', dataPath: '/data', goVersion: 'go1.25.0', os: 'linux', arch: 'amd64', numCPU: 8, uptimeSeconds: 3600 })
      if (p === '/stats') return json(200, stats())
      if (p === '/config' && method === 'GET') return json(200, st.settings)
      if (p === '/config' && method === 'PUT') {
        st.settings = JSON.parse(body) as Record<string, unknown>
        return json(200, st.settings)
      }
      if (p === '/users' && method === 'GET') return json(200, { users: st.users })
      if (p === '/users' && method === 'POST') {
        const spec = JSON.parse(body) as { name: string, policies: string[], disabled: boolean, description?: string, createKey?: boolean }
        const key = { accessKey: 'AKIANEWUSER00000001', secretKey: 'SECRET/new+user/key0000000000000000000', status: 'Active', createdAt: new Date().toISOString() }
        const user = { name: spec.name, description: spec.description, disabled: spec.disabled, policies: spec.policies, keys: spec.createKey ? [{ ...key, secretKey: undefined }] : [], createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() }
        st.users.push(user)
        return json(201, { user, key: spec.createKey ? key : undefined })
      }
      const um = /^\/users\/([^/]+)(\/keys)?$/.exec(p)
      if (um) {
        const u = st.users.find(x => x.name === decodeURIComponent(um[1]!))
        if (!u) return json(404, { error: { code: 'NotFound', message: 'no such user' } })
        if (um[2] && method === 'POST') {
          const key = { accessKey: 'AKIAALICE0000000002', secretKey: 'alice-second-secret-000000000000000000', status: 'Active', createdAt: new Date().toISOString() }
          u.keys.push({ ...key })
          return json(201, key)
        }
        if (method === 'GET') return json(200, u)
        if (method === 'PATCH') {
          Object.assign(u, JSON.parse(body))
          return json(200, u)
        }
        if (method === 'DELETE') {
          st.users = st.users.filter(x => x !== u)
          return json(204)
        }
      }
      if (p.startsWith('/keys/') && method === 'PATCH') {
        const ak = decodeURIComponent(p.slice('/keys/'.length))
        for (const u of st.users) for (const k of u.keys) if (k.accessKey === ak) k.status = (JSON.parse(body) as { status: string }).status
        return json(204)
      }
      if (p === '/policies') return json(200, { policies: st.policies })
      const pm = /^\/policies\/([^/]+)$/.exec(p)
      if (pm) {
        const name = decodeURIComponent(pm[1]!)
        const pol = st.policies.find(x => x.name === name)
        if (method === 'GET') return pol ? json(200, pol) : json(404, { error: { code: 'NotFound', message: 'no such policy' } })
        if (method === 'PUT') {
          const r = JSON.parse(body) as { description?: string, document: { Statement?: unknown[] } }
          if (JSON.stringify(r.document).includes('s3:Bogus')) return json(400, { error: { code: 'InvalidPolicy', message: 'Policy has invalid action: s3:Bogus' } })
          const next = { name, description: r.description, builtin: false, document: r.document, createdAt: pol?.createdAt ?? new Date().toISOString(), updatedAt: new Date().toISOString() }
          st.policies = [...st.policies.filter(x => x.name !== name), next]
          return json(200, next)
        }
        if (method === 'DELETE') {
          if (st.users.some(u => u.policies.includes(name))) return json(409, { error: { code: 'Conflict', message: 'policy is attached to users' } })
          st.policies = st.policies.filter(x => x.name !== name)
          return json(204)
        }
      }
      if (p === '/authz/simulate') {
        const r = JSON.parse(body) as { action: string }
        return json(200, { Allowed: r.action === 's3:GetObject', ExplicitDeny: false, Reason: r.action === 's3:GetObject' ? 'allowed by policy readwrite' : 'no statement allows the action' })
      }
      if (p === '/backings') return json(200, { defaultBacking: 'default', autoFailover: false, backings: BACKINGS })
      if (p === '/buckets' && method === 'GET') {
        return json(200, { buckets: [{
          name: 'photos', createdAt: '2026-01-01T00:00:00Z', region: 'eu-test-1', versioning: 'Enabled', objectLock: false, owner: 'root',
          configs: { policy: false, cors: false, lifecycle: false, tagging: false, encryption: false, website: false, objectLock: false, acl: 'private' },
          public: false,
          usage: { objects: 42, bytes: 4096, versions: 50, versionBytes: 5000, deleteMarkers: 1, pendingUploads: 0 },
          placement: placementSummary(st.placement)
        }] })
      }
      if (p === '/buckets/photos/placement' && method === 'GET') {
        const res = JSON.parse(JSON.stringify(st.placement)) as MockPlacement
        // A running replica copy finishes once it has been seen (the UI polls meanwhile).
        for (const m of st.placement.members) {
          if (m.state === 'syncing' && m.role === 'replica') Object.assign(m, { state: 'in-sync', progress: null, lastSyncAt: new Date().toISOString() })
        }
        return json(200, res)
      }
      if (p === '/buckets/photos/placement' && method === 'PUT') {
        const r = JSON.parse(body) as { mode: string, replicas: string[] }
        const keep = st.placement.members.filter(m => m.role === 'primary' || r.replicas.includes(m.backing))
        const added = r.replicas.filter(b => !keep.some(m => m.backing === b)).map(b => member(b, 'replica', { state: 'syncing', progress: { keys: 10, versions: 12, bytes: 2048 } }))
        st.placement = { ...st.placement, mode: r.mode, members: [...keep, ...added] }
        return json(200, st.placement)
      }
      if (p === '/buckets/photos/placement/move' && method === 'POST') {
        const r = JSON.parse(body) as { to: string }
        st.placement = {
          ...st.placement,
          members: [...st.placement.members, member(r.to, 'move-target', { state: 'syncing', progress: { keys: 1, versions: 1, bytes: 1 } })],
          move: { from: st.placement.primary, to: r.to, startedAt: new Date().toISOString(), phase: 'copying' }
        }
        return json(202, st.placement)
      }
      if (p === '/buckets/photos/placement/move' && method === 'DELETE') {
        st.placement = { ...st.placement, members: st.placement.members.filter(m => m.role !== 'move-target'), move: null }
        return json(200, st.placement)
      }
      if (p === '/buckets/photos') {
        return json(200, {
          name: 'photos', createdAt: '2026-01-01T00:00:00Z', region: 'eu-test-1', versioning: 'Enabled', objectLock: false, owner: 'root',
          configs: { policy: false, cors: !!st.cors, lifecycle: !!st.lifecycle, tagging: false, encryption: false, website: false, objectLock: false, acl: 'private' },
          public: false,
          usage: { objects: 42, bytes: 4096, versions: 50, versionBytes: 5000, deleteMarkers: 1, pendingUploads: 0 },
          placement: placementSummary(st.placement)
        })
      }
      return json(404, { error: { code: 'NotFound', message: `unmocked ${method} ${p}` } })
    }

    // S3 (path-style)
    const bucket = decodeURIComponent(path.split('/')[1] ?? '')
    const q = url.searchParams
    if (!bucket) {
      return xml(200, '<ListAllMyBucketsResult><Owner><ID>root</ID></Owner><Buckets><Bucket><Name>photos</Name><CreationDate>2026-01-01T00:00:00.000Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>')
    }
    if (bucket !== 'photos') return xml(404, '<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message></Error>')
    if (q.has('versioning')) return xml(200, '<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>')
    if (q.has('object-lock')) return xml(404, '<Error><Code>ObjectLockConfigurationNotFoundError</Code><Message>none</Message></Error>')
    if (q.has('location')) return xml(200, '<LocationConstraint>eu-test-1</LocationConstraint>')
    if (q.has('policyStatus')) return xml(404, '<Error><Code>NoSuchBucketPolicy</Code><Message>none</Message></Error>')
    if (q.has('policy')) return xml(404, '<Error><Code>NoSuchBucketPolicy</Code><Message>none</Message></Error>')
    if (q.has('lifecycle')) {
      if (method === 'PUT') {
        st.lifecycle = body
        return xml(200)
      }
      if (method === 'DELETE') {
        st.lifecycle = null
        return xml(204)
      }
      return st.lifecycle ? xml(200, st.lifecycle) : xml(404, '<Error><Code>NoSuchLifecycleConfiguration</Code><Message>none</Message></Error>')
    }
    if (q.has('cors')) {
      if (method === 'PUT') {
        st.cors = body
        return xml(200)
      }
      return st.cors ? xml(200, st.cors) : xml(404, '<Error><Code>NoSuchCORSConfiguration</Code><Message>none</Message></Error>')
    }
    if (q.has('tagging')) return method === 'GET' ? xml(404, '<Error><Code>NoSuchTagSet</Code><Message>none</Message></Error>') : xml(204)
    if (q.has('acl')) return xml(200, '<AccessControlPolicy><Owner><ID>root</ID></Owner><AccessControlList><Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>root</ID></Grantee><Permission>FULL_CONTROL</Permission></Grant></AccessControlList></AccessControlPolicy>')
    if (q.has('publicAccessBlock')) return xml(404, '<Error><Code>NoSuchPublicAccessBlockConfiguration</Code><Message>none</Message></Error>')
    if (q.has('encryption')) return xml(404, '<Error><Code>ServerSideEncryptionConfigurationNotFoundError</Code><Message>none</Message></Error>')
    return xml(501, '<Error><Code>NotImplemented</Code><Message>mock</Message></Error>')
  })
  return st
}

async function mockPlainS3(page: Page) {
  await page.route(`${S3_ENDPOINT}/**`, async (route) => {
    const url = new URL(route.request().url())
    if (route.request().method() === 'OPTIONS') return route.fulfill({ status: 204, headers: CORS })
    if (url.pathname.startsWith('/_gos3/')) return route.fulfill({ status: 404, body: 'not found', headers: CORS })
    const buckets = ['alpha', 'beta', 'gamma'].map(b => `<Bucket><Name>${b}</Name><CreationDate>2026-01-0${b.length}T00:00:00.000Z</CreationDate></Bucket>`).join('')
    return route.fulfill({ status: 200, body: `<ListAllMyBucketsResult><Owner><ID>me</ID></Owner><Buckets>${buckets}</Buckets></ListAllMyBucketsResult>`, headers: { ...CORS, 'content-type': 'application/xml' } })
  })
}

async function seed(page: Page) {
  await page.addInitScript(([g, s, ge, se]) => {
    localStorage.setItem('gos3.connections.v1', JSON.stringify({
      version: 1,
      activeId: g,
      dismissedPresets: [],
      connections: [
        { id: g, name: 'Mock gos3', endpoint: ge, region: 'eu-test-1', accessKeyId: 'ROOTKEY', secretAccessKey: 'ROOTSECRET', kind: 'gos3', forcePathStyle: true, remember: true, source: 'user' },
        { id: s, name: 'Plain S3', endpoint: se, region: 'us-east-1', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 's3', forcePathStyle: true, remember: true, source: 'user' }
      ]
    }))
  }, [CONN_ID, S3_CONN_ID, ENDPOINT, S3_ENDPOINT] as const)
}

test.describe('admin pages', () => {
  let pageErrors: string[] = []
  test.beforeEach(async ({ page }) => {
    pageErrors = []
    page.on('pageerror', e => pageErrors.push(e.message))
    await seed(page)
  })
  test.afterEach(() => {
    expect(pageErrors).toEqual([])
  })

  test('overview shows identity, metrics, chart and operations; auto-refresh can be paused', async ({ page }) => {
    const st = await mockGos3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}`)
    await expect(page.getByRole('heading', { name: 'mock-gos3', level: 1 })).toBeVisible()
    await expect(page.getByText('1.2.3')).toBeVisible()
    await expect(page.getByText('fs (/data)')).toBeVisible()
    await expect(page.getByText('1,234').first()).toBeVisible()
    await expect(page.getByRole('img', { name: /Requests per minute, last 60 minutes/ })).toBeVisible()
    await expect(page.getByRole('img', { name: /Traffic per minute/ })).toBeVisible()
    await expect(page.getByRole('meter', { name: 'Disk usage' })).toHaveAttribute('aria-valuenow', '40')
    const opsRows = page.locator('table tbody th')
    await expect(opsRows).toHaveText(['GetObject', 'PutObject'])
    await page.getByRole('button', { name: 'p99' }).click()
    await expect(opsRows).toHaveText(['PutObject', 'GetObject'])
    await page.getByRole('button', { name: /Pause auto-refresh/ }).click()
    await expect(page.getByRole('button', { name: 'Resume auto-refresh' })).toHaveAttribute('aria-pressed', 'true')
    expect(st.log.some(l => l.includes('/stats'))).toBe(true)
  })

  test('generic S3 overview shows a bucket summary', async ({ page }) => {
    await mockPlainS3(page)
    await page.goto(`/_gos3/ui/servers/${S3_CONN_ID}`)
    await expect(page.getByText('Generic S3 server')).toBeVisible()
    await expect(page.getByText('3', { exact: true })).toBeVisible()
    await expect(page.getByRole('link', { name: 'gamma' })).toBeVisible()
    for (const label of ['Users', 'Policies', 'Settings']) {
      await expect(page.getByRole('navigation', { name: 'Server navigation' }).getByRole('link', { name: label })).toHaveCount(0)
    }
  })

  test('creates a user with an access key and shows the secret once', async ({ page }) => {
    const st = await mockGos3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/users`)
    await expect(page.getByRole('link', { name: 'alice' })).toBeVisible()
    await page.getByRole('button', { name: 'Create user' }).first().click()
    const dialog = page.getByRole('dialog', { name: 'Create user' })
    await dialog.getByLabel('User name').fill('root')
    await dialog.getByRole('button', { name: 'Create user' }).click()
    await expect(dialog.getByText('"root" is reserved')).toBeVisible()
    await dialog.getByLabel('User name').fill('bob')
    await dialog.getByRole('button', { name: 'Create user' }).click()
    const secret = page.getByRole('dialog', { name: 'Access key created' })
    await expect(secret.getByTestId('secret-key')).toHaveText('SECRET/new+user/key0000000000000000000')
    await expect(secret.getByRole('button', { name: 'Done' })).toBeDisabled()
    await secret.getByText('I have stored the secret key').click()
    await secret.getByRole('button', { name: 'Done' }).click()
    await expect(secret).toBeHidden()
    await expect(page.getByRole('link', { name: 'bob' })).toBeVisible()
    expect(JSON.parse(st.bodies['POST /_gos3/admin/v1/users']!)).toMatchObject({ name: 'bob', createKey: true, disabled: false })
    // Admin payloads are signed (C1): the hash of the exact body, covered by the signature.
    const sha256 = (s: string) => createHash('sha256').update(s).digest('hex')
    expect(st.signing['POST /_gos3/admin/v1/users']).toEqual({ payloadHash: sha256(st.bodies['POST /_gos3/admin/v1/users']!), signedHeaders: expect.stringContaining('x-amz-content-sha256') })
    expect(st.signing['GET /_gos3/admin/v1/whoami']?.payloadHash).toBe(sha256(''))
    for (const s of Object.values(st.signing)) expect(s.payloadHash).not.toBe('UNSIGNED-PAYLOAD')
  })

  test('user detail: keys, deactivate with confirmation and permission check', async ({ page }) => {
    const st = await mockGos3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/users/alice`)
    await expect(page.getByRole('heading', { name: 'alice', level: 1 })).toBeVisible()
    await expect(page.getByText('AKIAALICE0000000001')).toBeVisible()
    await page.getByRole('button', { name: 'Deactivate' }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Deactivate' }).click()
    await expect(page.getByRole('button', { name: 'Activate' })).toBeVisible()
    expect(st.bodies['PATCH /_gos3/admin/v1/keys/AKIAALICE0000000001']).toBe('{"status":"Inactive"}')

    await page.getByLabel('Bucket').fill('photos')
    await page.getByRole('button', { name: 'Simulate' }).click()
    await expect(page.getByText('Allowed', { exact: true })).toBeVisible()
    await expect(page.getByText('allowed by policy readwrite')).toBeVisible()
  })

  test('policies: built-in badge, edit with inline server validation, create from a template', async ({ page }) => {
    await mockGos3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/policies`)
    await expect(page.getByRole('link', { name: 'readwrite' })).toBeVisible()
    await expect(page.getByText('Built-in').first()).toBeVisible()

    await page.getByRole('link', { name: 'photos-ro' }).click()
    const editor = page.getByLabel('Policy document')
    await expect(editor).toHaveValue(/s3:GetObject/)
    await editor.fill(JSON.stringify({ Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: 's3:Bogus', Resource: '*' }] }))
    await page.getByRole('button', { name: 'Save policy' }).click()
    await expect(page.getByText('Policy has invalid action: s3:Bogus')).toBeVisible()

    // Built-in policies are read-only.
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/policies/readwrite`)
    await expect(page.getByText('Built-in policies cannot be changed.')).toBeVisible()
    await expect(page.getByRole('button', { name: 'Save policy' })).toHaveCount(0)

    await page.goto(`/_gos3/ui/servers/${CONN_ID}/policies`)
    await page.getByRole('button', { name: 'Create policy' }).first().click()
    const dialog = page.getByRole('dialog', { name: 'Create policy' })
    await dialog.getByLabel('Policy name').fill('logs-rw')
    await dialog.getByLabel('Bucket').fill('logs')
    await dialog.getByRole('button', { name: 'Apply template' }).click()
    await expect(dialog.getByLabel('Policy document')).toHaveValue(/arn:aws:s3:::logs\/\*/)
    await dialog.getByRole('button', { name: 'Create policy' }).click()
    await expect(page).toHaveURL(/\/policies\/logs-rw$/)
  })

  test('settings: edits durations in seconds and saves nanoseconds', async ({ page }) => {
    const st = await mockGos3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/settings`)
    // One section at a time: the chips under the header switch (?tab=).
    const chips = page.getByRole('tablist', { name: 'Settings sections' })
    await expect(chips.getByRole('tab', { name: 'General' })).toHaveAttribute('aria-selected', 'true')
    await expect(page.getByLabel('Beacon interval (seconds)')).toBeHidden()
    const discovery = chips.getByRole('tab', { name: /^Discovery/ })
    await discovery.click()
    await expect(page).toHaveURL(/[?&]tab=discovery/)
    await expect(page.getByRole('tabpanel', { name: 'Discovery' })).toBeVisible()
    await expect(page.getByLabel('Server name')).toBeHidden()
    const interval = page.getByLabel('Beacon interval (seconds)')
    await expect(interval).toHaveValue('10')
    await interval.fill('5')
    await interval.blur()
    await expect(page.getByText('You have unsaved changes.')).toBeVisible()
    await expect(discovery).toContainText('unsaved changes')
    await page.getByRole('button', { name: 'Save settings' }).click()
    await expect(page.getByText('All changes saved.')).toBeVisible()
    const put = JSON.parse(st.bodies['PUT /_gos3/admin/v1/config']!) as { discovery: { beaconInterval: number, peerTtl: number } }
    expect(put.discovery.beaconInterval).toBe(5_000_000_000)
    expect(put.discovery.peerTtl).toBe(60_000_000_000)
  })

  test('bucket settings: overview usage, lifecycle rule and CORS rule', async ({ page }) => {
    const st = await mockGos3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/buckets/photos/settings`)
    await expect(page.getByRole('heading', { name: 'photos', level: 1 })).toBeVisible()
    await expect(page.getByText('Versioned')).toBeVisible()
    await expect(page.getByText('42', { exact: true })).toBeVisible()

    await page.getByRole('tab', { name: 'Lifecycle' }).click()
    await expect(page).toHaveURL(/tab=lifecycle/)
    await page.getByRole('button', { name: 'Add rule' }).first().click()
    const dialog = page.getByRole('dialog', { name: 'Add lifecycle rule' })
    await dialog.getByLabel('Key prefix').fill('tmp/')
    await dialog.getByRole('button', { name: 'Add rule' }).click()
    await expect(page.getByText('prefix "tmp/"')).toBeVisible()
    expect(st.lifecycle).toContain('<Prefix>tmp/</Prefix>')
    expect(st.lifecycle).toContain('<Days>30</Days>')

    await page.getByRole('tab', { name: 'CORS' }).click()
    await page.getByRole('button', { name: 'Add rule' }).click()
    await page.getByRole('button', { name: 'Save CORS' }).click()
    await expect.poll(() => st.cors).toContain('<AllowedMethod>GET</AllowedMethod>')
    await page.getByRole('tab', { name: 'XML' }).click()
    await expect(page.getByText(/<CORSConfiguration/)).toBeVisible()
  })
  test('storage: backings page, replication and a canceled move in bucket settings', async ({ page }) => {
    const st = await mockGos3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/storage`)
    await expect(page.getByRole('heading', { name: 'Storage', level: 1 })).toBeVisible()
    for (const name of ['default', 'fast', 'archive', 'usb']) await expect(page.getByRole('heading', { name, level: 2, exact: true })).toBeVisible()
    await expect(page.getByRole('meter', { name: 'Disk usage of default' })).toHaveAttribute('aria-valuenow', '40')
    await expect(page.getByTestId('unhealthy-backings')).toContainText('usb')
    await expect(page.getByText('stat /mnt/usb: no such file or directory')).toBeVisible()
    await expect(page.getByRole('navigation', { name: 'Server navigation' }).getByRole('link', { name: 'Storage' })).toBeVisible()

    await page.goto(`/_gos3/ui/servers/${CONN_ID}/buckets/photos/settings?tab=placement`)
    await expect(page.getByRole('tab', { name: 'Placement' })).toHaveAttribute('aria-selected', 'true')
    await expect(page.getByTestId('placement-mode')).toHaveText('Single copy')

    // Replicate to "fast": the new replica syncs, then (polled) is in sync.
    await page.getByRole('radio', { name: /Active-passive/ }).click()
    await page.getByRole('checkbox', { name: 'fast' }).click()
    await page.getByRole('button', { name: 'Save replication' }).click()
    await expect(page.getByTestId('placement-mode')).toHaveText('Active-passive')
    expect(JSON.parse(st.bodies['PUT /_gos3/admin/v1/buckets/photos/placement']!)).toEqual({ mode: 'active-passive', replicas: ['fast'] })
    await expect(page.getByTestId('member-fast')).toContainText('Replica')
    await expect(page.getByTestId('member-fast')).toContainText('In sync', { timeout: 10_000 })

    // Move the primary to "archive", then cancel while copying.
    await page.getByRole('combobox', { name: 'Target backing' }).click()
    await page.getByRole('option', { name: 'archive' }).click()
    await page.getByRole('button', { name: 'Move', exact: true }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Start move' }).click()
    await expect(page.getByTestId('move-banner')).toContainText('Moving from default to archive')
    expect(JSON.parse(st.bodies['POST /_gos3/admin/v1/buckets/photos/placement/move']!)).toEqual({ to: 'archive' })
    await page.getByTestId('move-banner').getByRole('button', { name: 'Cancel move' }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Cancel move' }).click()
    await expect(page.getByTestId('move-banner')).toBeHidden()
    expect(st.log).toContain('DELETE /_gos3/admin/v1/buckets/photos/placement/move')

    // The bucket list shows the replication chip.
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/buckets`)
    await expect(page.getByTestId('placement-chips')).toContainText('AP ×2')
  })
})
