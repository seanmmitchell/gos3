// Servers & discovery E2E against the embedded build: sign-in to the hosting
// server, adding a server with gos3 auto-detection, and the Discover page.
// gos3 APIs are mocked with Playwright request interception.
import { expect, test } from '@playwright/test'
import type { Page, Route } from '@playwright/test'

const CORS = {
  'access-control-allow-origin': '*',
  'access-control-allow-methods': 'GET,PUT,POST,DELETE,HEAD',
  'access-control-allow-headers': '*'
}

function info(id: string, name: string, base: string) {
  return {
    service: 'gos3',
    id,
    name,
    version: '1.2.3',
    apiVersion: 'v1',
    region: 'eu-test-1',
    endpoints: { s3: base, admin: `${base}/_gos3/admin/v1` },
    features: ['admin', 'discovery'],
    startedAt: '2026-10-09T12:00:00Z'
  }
}

/** A remote gos3 that only answers GET /_gos3/info (CORS "*"). */
async function mockRemote(page: Page, base: string, id: string, name: string) {
  await page.route(`${base}/**`, async (route: Route) => {
    const url = new URL(route.request().url())
    if (route.request().method() === 'OPTIONS') return route.fulfill({ status: 204, headers: CORS })
    if (url.pathname === '/_gos3/info') return route.fulfill({ status: 200, headers: CORS, json: info(id, name, base) })
    return route.fulfill({ status: 404, headers: CORS, body: '' })
  })
}

test.describe('servers & discovery (embedded build)', () => {
  test('signs in to the hosting server and explains a wrong secret', async ({ page }) => {
    let accept = false
    await page.route('**/_gos3/admin/v1/whoami', async (route) => {
      if (!accept) {
        return route.fulfill({ status: 401, json: { error: { code: 'Unauthenticated', message: 'SignatureDoesNotMatch: The request signature we calculated does not match the signature you provided.' } } })
      }
      const origin = new URL(route.request().url()).origin
      return route.fulfill({
        status: 200,
        json: {
          user: 'root',
          accessKey: 'AK',
          isRoot: true,
          policies: [],
          capabilities: { admin: true, viewInfo: true, viewStats: true, manageConfig: true, manageUsers: true, managePolicies: true, manageBuckets: true, discovery: true, scan: true, probe: true, createOwnKeys: false },
          server: info('e2e0000000000001', 'e2e-gos3', origin)
        }
      })
    })

    await page.goto('/_gos3/ui/')
    await expect(page.getByRole('heading', { name: /Sign in to e2e-gos3/ })).toBeVisible()
    await page.getByRole('main').getByRole('link', { name: 'Sign in' }).first().click()
    await expect(page).toHaveURL(/\/_gos3\/ui\/connect\?server=this-server/)

    await page.getByLabel('Access key ID').fill('AK')
    await page.getByLabel('Secret access key', { exact: true }).fill('wrong')
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(page.getByRole('alert').filter({ hasText: 'Wrong secret key' })).toBeVisible()

    accept = true
    await page.getByLabel('Secret access key', { exact: true }).fill('right')
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(page).toHaveURL(/\/_gos3\/ui\/servers\/this-server$/)
  })

  test('adds a server with gos3 auto-detection', async ({ page }) => {
    const base = 'http://10.77.0.1:9000'
    await mockRemote(page, base, 'aaaaaaaaaaaaaaaa', 'lab')
    await page.goto('/_gos3/ui/servers')
    await page.getByRole('button', { name: 'Add server' }).first().click()
    const dialog = page.getByRole('dialog')
    await dialog.getByLabel('Endpoint URL').fill(base)
    await expect(dialog.getByText(/gos3 1\.2\.3 · “lab”/)).toBeVisible()
    await dialog.getByRole('button', { name: 'Add server' }).click()
    await expect(dialog).toBeHidden()
    const row = page.getByRole('row').filter({ hasText: base })
    await expect(row.getByRole('link', { name: 'lab' })).toBeVisible()
    await expect(row.getByText('gos3', { exact: true })).toBeVisible()
  })

  test('discovers servers by direct probe and adds one', async ({ page }) => {
    const base = 'http://10.77.0.2:9000'
    await mockRemote(page, base, 'bbbbbbbbbbbbbbbb', 'probed')
    await page.goto('/_gos3/ui/discover')
    await expect(page.getByText('Not available in the embedded console')).toBeVisible()
    await page.getByLabel('URLs or hosts').fill('10.77.0.2\nhttp://10.77.0.3:9000')
    await page.route('http://10.77.0.3:9000/**', route => route.abort('connectionrefused'))
    await page.getByRole('button', { name: 'Probe', exact: true }).click()
    const row = page.getByRole('row').filter({ hasText: 'bbbbbbbbbbbbbbbb' })
    await expect(row).toContainText('This browser')
    await expect(page.getByText('http://10.77.0.3:9000', { exact: true })).toBeVisible()
    await row.getByRole('button', { name: 'Add probed as a server' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog.getByLabel('Endpoint URL')).toHaveValue(base)
    await dialog.getByRole('button', { name: 'Add server' }).click()
    await expect(row.getByText('Configured')).toBeVisible()
  })
})
