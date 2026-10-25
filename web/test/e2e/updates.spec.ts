import { expect, test, type BrowserContext, type Page } from '@playwright/test'
import { mkdir } from 'node:fs/promises'
import { resolve } from 'node:path'

const endpoint = 'http://updates.mock.test'
const base = '/_gos3/ui/servers/updates/settings?tab=updates'
const info = { service: 'gos3', id: 'updates-server-104', name: 'Backup server', version: 'v0.2.2', apiVersion: 'v1', region: 'us-east-1', endpoints: { s3: endpoint, admin: `${endpoint}/_gos3/admin/v1`, ui: `${endpoint}/_gos3/ui/` }, features: ['admin', 'ui'], startedAt: '2026-10-25T08:00:00Z' }
const release = (v: string) => ({ version: v, type: 'patch', url: `https://github.com/seanmmitchell/gos3/releases/tag/${v}` })

async function setup(context: BrowserContext, options: { readonly?: boolean, generic?: boolean } = {}) {
  const updates = { supported: true, enabled: true, comparable: true, installedVersion: 'v0.2.2', notifyFor: ['patch', 'minor', 'major'], lastCheck: '2026-10-25T12:00:00Z', lastSuccess: '2026-10-25T12:00:00Z', error: '', available: release('v0.2.3') }
  let settings = {
    name: 'Backup server', region: 'us-east-1', publicUrl: '', domains: [], publicInfo: true, uiRedirect: true, metricsPublic: false,
    cors: { allowedOrigins: ['*'], allowedHeaders: ['*'], exposeHeaders: ['ETag'], maxAgeSeconds: 3600 },
    discovery: { enabled: false, advertise: false, mdns: false, beacon: false, beaconGroup: '239.255.73.83:7373', beaconInterval: 10000000000, seeds: [], dnsSrv: [], peerTtl: 60000000000, allowScan: false, maxScanHosts: 1024 },
    logging: { level: 'info', format: 'text', accessLog: true }, lifecycle: { enabled: true, intervalSeconds: 3600 }, updates: { enabled: true, notifyFor: ['patch', 'minor', 'major'] }
  }
  const calls: string[] = []
  await context.addInitScript(({ endpoint, generic }) => {
    if (!localStorage.getItem('gos3.connections.v1')) localStorage.setItem('gos3.connections.v1', JSON.stringify({ version: 1, activeId: 'updates', dismissedPresets: [], connections: [{ id: 'updates', name: 'Backup server', endpoint, region: 'us-east-1', accessKeyId: 'TESTKEY', secretAccessKey: 'TESTSECRET', kind: generic ? 's3' : 'gos3', forcePathStyle: true, remember: true, source: 'user', serverId: 'updates-server-104' }] }))
  }, { endpoint, generic: options.generic })
  await context.route(`${endpoint}/**`, async route => {
    const req = route.request()
    const path = new URL(req.url()).pathname
    const headers = { 'access-control-allow-origin': '*', 'access-control-allow-methods': '*', 'access-control-allow-headers': '*', 'content-type': 'application/json' }
    const json = (body: unknown, status = 200) => route.fulfill({ status, headers, body: JSON.stringify(body) })
    if (req.method() === 'OPTIONS') return route.fulfill({ status: 204, headers })
    calls.push(path)
    if (path === '/_gos3/info') return options.generic ? json({}, 404) : json(info)
    if (path.endsWith('/whoami')) return json({ isRoot: !options.readonly, user: options.readonly ? 'backup-reader' : '', policies: ['readonly'], capabilities: { admin: !options.readonly, viewInfo: true, manageConfig: !options.readonly }, server: info })
    if (path.endsWith('/updates')) return json(updates)
    if (path.endsWith('/config')) {
      if (req.method() === 'PUT') {
        settings = req.postDataJSON() as typeof settings
        updates.enabled = settings.updates.enabled
        updates.notifyFor = settings.updates.notifyFor
      }
      return json(settings)
    }
    if (path.endsWith('/info')) return json({ ...info, backend: 'fs', dataPath: '/data', goVersion: 'go1.26.10', os: 'linux', arch: 'amd64', numCPU: 4, uptimeSeconds: 3600 })
    if (path === '/') return route.fulfill({ status: 200, headers: { ...headers, 'content-type': 'application/xml' }, body: '<ListAllMyBucketsResult><Buckets/></ListAllMyBucketsResult>' })
    return json({ error: { code: 'NotFound', message: 'Not available' } }, 404)
  })
  return { updates, calls, settings: () => settings }
}

const notice = (page: Page) => page.getByRole('status', { name: 'gos3 update available' })

test('release notice, settings save, and nonintrusive error status', async ({ page, context }) => {
  const state = await setup(context)
  await page.setViewportSize({ width: 1440, height: 1080 })
  await page.goto(base)
  await expect(notice(page)).toBeVisible()
  await expect(notice(page)).toContainText('v0.2.2')
  await expect(notice(page).getByRole('link', { name: 'Release notes' })).toHaveAttribute('href', release('v0.2.3').url)
  await expect(page.getByRole('heading', { name: 'Release check status' })).toBeVisible()
  if (process.env.UPDATE_SCREENSHOTS === '1') {
    const dir = resolve('../docs/screenshots/updates')
    await mkdir(dir, { recursive: true })
    await page.screenshot({ path: resolve(dir, 'release-notice.png'), fullPage: true })
  }
  await notice(page).getByRole('button', { name: 'Dismiss', exact: true }).click()
  if (process.env.UPDATE_SCREENSHOTS === '1') await page.screenshot({ path: resolve('../docs/screenshots/updates/settings.png'), fullPage: true })
  await page.getByRole('checkbox', { name: 'Major releases' }).click()
  await page.getByRole('button', { name: 'Save settings', exact: true }).click()
  await expect.poll(() => state.settings().updates.notifyFor).toEqual(['patch', 'minor'])
  state.updates.error = 'GitHub release check returned HTTP 429 (next attempt in 24 hours)'
  await page.reload()
  await expect(page.getByText('GitHub release check returned HTTP 429', { exact: false })).toBeVisible()
  await expect(notice(page)).toHaveCount(0)
  await page.getByRole('switch', { name: 'Check for updates' }).click()
  await page.getByRole('button', { name: 'Save settings', exact: true }).click()
  await expect.poll(() => state.updates.enabled).toBe(false)
})

test('dismiss lasts until next check; skip persists across later checks and visits', async ({ page, context }) => {
  const state = await setup(context)
  await page.goto(base)
  await notice(page).getByRole('button', { name: 'Dismiss', exact: true }).click()
  await page.reload()
  await expect(notice(page)).toHaveCount(0)
  state.updates.lastCheck = '2026-10-26T12:00:00Z'
  await page.reload()
  await expect(notice(page)).toBeVisible()
  await notice(page).getByRole('button', { name: 'Skip this version' }).click()
  state.updates.lastCheck = '2026-10-27T12:00:00Z'
  await page.reload()
  await expect(notice(page)).toHaveCount(0)
  state.updates.available = release('v0.2.4')
  await page.reload()
  await expect(notice(page)).toContainText('v0.2.4')
})

test('multiple tabs display one notice and share dismissal', async ({ page, context }) => {
  await setup(context)
  await page.goto(base)
  await expect(notice(page)).toBeVisible()
  const second = await context.newPage()
  await second.goto(base)
  await expect(second.getByRole('heading', { name: 'Release check status' })).toBeVisible()
  await expect(notice(second)).toHaveCount(0)
  await notice(page).getByRole('button', { name: 'Dismiss', exact: true }).click()
  await second.reload()
  await expect(notice(second)).toHaveCount(0)
})

test('users without config-edit permission can see release notices', async ({ page, context }) => {
  const state = await setup(context, { readonly: true })
  await page.goto('/_gos3/ui/servers/updates/buckets')
  await expect(notice(page)).toBeVisible()
  expect(state.calls).not.toContain('/_gos3/admin/v1/config')
})

test('tabs without Web Locks coordinate one notice for each check', async ({ page, context }) => {
  await setup(context)
  await context.addInitScript(() => { Object.defineProperty(navigator, 'locks', { value: undefined }) })
  const second = await context.newPage()
  await Promise.all([page.goto(base), second.goto(base)])
  await expect(page.getByRole('heading', { name: 'Release check status' })).toBeVisible()
  await expect(second.getByRole('heading', { name: 'Release check status' })).toBeVisible()
  await expect.poll(async () => await notice(page).count() + await notice(second).count()).toBe(1)
})

test('generic S3 connections do not request release status', async ({ page, context }) => {
  const state = await setup(context, { generic: true })
  await page.goto('/_gos3/ui/servers/updates/buckets')
  await expect(page.getByRole('heading', { name: 'Buckets', exact: true })).toBeVisible()
  await expect(notice(page)).toHaveCount(0)
  expect(state.calls).not.toContain('/_gos3/admin/v1/updates')
})
