import { expect, test } from '@playwright/test'

test.describe('app shell (embedded build)', () => {
  test('loads offline-safe and detects the hosting gos3 server', async ({ page }) => {
    const external: string[] = []
    page.on('request', (req) => {
      const u = new URL(req.url())
      if (u.hostname !== '127.0.0.1' && u.protocol.startsWith('http')) external.push(req.url())
    })
    await page.goto('/_gos3/ui/servers')
    await expect(page).toHaveTitle(/gos3/)
    await expect(page.getByRole('navigation', { name: 'Global navigation' })).toBeVisible()
    // Embedded detection adds "This server" from the mocked /_gos3/info.
    await expect(page.getByRole('button', { name: /Active server: e2e-gos3 \(this server\)/ })).toBeVisible()
    expect(external).toEqual([])
  })

  test('redirects servers without credentials to the connect page', async ({ page }) => {
    await page.goto('/_gos3/ui/servers/this-server/buckets')
    await expect(page).toHaveURL(/\/_gos3\/ui\/connect\?server=this-server&redirect=/)
  })

  test('redirects unknown servers to the server list', async ({ page }) => {
    await page.goto('/_gos3/ui/servers/does-not-exist')
    await expect(page).toHaveURL(/\/_gos3\/ui\/servers$/)
  })

  test('toggles the color mode', async ({ page }) => {
    await page.goto('/_gos3/ui/')
    const html = page.locator('html')
    const before = await html.getAttribute('class')
    await page.getByRole('button', { name: /switch to (dark|light) mode/i }).first().click()
    await expect.poll(async () => html.getAttribute('class')).not.toBe(before)
  })
})
