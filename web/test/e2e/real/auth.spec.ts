// Sign-in to the hosting gos3 through the embedded UI.
import { expect, fillSignIn, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, test, THIS_SERVER } from './fixtures'

test.describe('sign in (embedded UI)', () => {
  test('a wrong secret shows a clear error and the right one signs in', async ({ page, ui, primary }) => {
    await page.goto(ui())
    // The embedded UI detects its host and asks for credentials.
    await page.goto(ui(`servers/${THIS_SERVER}/buckets`))
    await expect(page).toHaveURL(/\/_gos3\/ui\/connect\?server=this-server/)
    await expect(page.getByRole('heading', { name: `Sign in to ${primary.name}` })).toBeVisible()

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
