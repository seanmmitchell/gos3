// Sign-in to the hosting gos3 through the embedded UI.
import { expect, fillSignIn, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, test, THIS_SERVER } from './fixtures'
import { STANDALONE_ORIGINS } from './env'

test.describe('sign in (embedded UI)', () => {
  test('a wrong secret shows a clear error and the right one signs in', async ({ page, ui, primary }) => {
    await page.goto(ui())
    // The embedded UI detects its host and asks for credentials.
    await page.goto(ui(`servers/${THIS_SERVER}/buckets`))
    await expect(page).toHaveURL(/\/_gos3\/ui\/connect\?server=this-server/)
    // (The name is not asserted: the settings spec renames the server meanwhile.)
    await expect(page.getByRole('heading', { name: /^Sign in to / })).toBeVisible()

    await fillSignIn(page, ROOT_ACCESS_KEY, 'definitely-not-the-secret')
    const alert = page.getByRole('alert').filter({ hasText: /secret/i })
    await expect(alert).toBeVisible()
    await expect(alert).toContainText(/secret key/i)
    await expect(page).toHaveURL(/\/connect/)

    await page.getByLabel('Secret access key').fill(ROOT_SECRET_KEY)
    await page.getByRole('button', { name: /^sign in$/i }).click()
    await expect(page).toHaveURL(/\/servers\/this-server\/buckets$/)
    await expect(page.getByRole('heading', { name: 'Buckets', exact: true })).toBeVisible()
  })

  test('an unknown access key is reported as such', async ({ page, ui }) => {
    await page.goto(ui(`connect?server=${THIS_SERVER}`))
    await fillSignIn(page, 'nosuchkey', 'whatever-secret')
    await expect(page.getByRole('alert').filter({ hasText: /access key/i })).toBeVisible()
  })
})

test.describe('CORS for the standalone UI', () => {
  test('the configured standalone origin may call the S3 and admin APIs', async ({ primary, api }) => {
    for (const path of ['/', '/_gos3/admin/v1/whoami']) {
      const pre = await fetch(`${primary.url}${path}`, {
        method: 'OPTIONS',
        headers: { 'Origin': STANDALONE_ORIGINS[0]!, 'Access-Control-Request-Method': 'GET', 'Access-Control-Request-Headers': 'authorization,x-amz-date,x-amz-content-sha256' }
      })
      expect(pre.status, path).toBeLessThan(300)
      expect(pre.headers.get('access-control-allow-origin'), path).toBe(STANDALONE_ORIGINS[0])
    }
    // Actual responses expose ETag & co. so the browser can read them.
    const r = await api.fetch('/', { headers: { Origin: STANDALONE_ORIGINS[0]! } })
    expect(r.headers.get('access-control-allow-origin')).toBe(STANDALONE_ORIGINS[0])
    expect((r.headers.get('access-control-expose-headers') ?? '').toLowerCase()).toContain('etag')
    // Other origins are not allowed.
    const other = await fetch(`${primary.url}/`, { method: 'OPTIONS', headers: { 'Origin': 'http://evil.example', 'Access-Control-Request-Method': 'GET' } })
    expect(other.headers.get('access-control-allow-origin')).toBeNull()
  })
})
