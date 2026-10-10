// IAM users through the UI, then the console as seen by a read-only user.
import type { Page } from '@playwright/test'
import { expect, fillSignIn, test, THIS_SERVER, uniqueName } from './fixtures'

test.describe.configure({ mode: 'serial' })

test.describe('users and a read-only identity', () => {
  const user = uniqueName('viewer').replace(/-/g, '_').slice(0, 40)
  const bucket = uniqueName('rodata')
  let page: Page
  let accessKey = ''
  let secretKey = ''

  test.beforeAll(async ({ browser, signIn, api }) => {
    await api.createBucket(bucket)
    await api.putObject(bucket, 'shared/report.txt', 'quarterly numbers')
    page = await browser.newPage()
    await signIn(page)
  })
  test.afterAll(async ({ api }) => {
    await page.close()
    await api.forceDeleteBucket(bucket)
    await api.admin('DELETE', `/users/${user}`).catch(() => undefined)
  })

  test('creates a read-only user with an access key', async ({ ui, api }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/users`))
    await page.getByRole('main').getByRole('button', { name: /Create user/ }).first().click()
    const modal = page.getByRole('dialog', { name: 'Create user' })
    await modal.getByRole('textbox', { name: /User name/ }).fill(user)
    await modal.getByRole('textbox', { name: 'Description' }).fill('e2e read-only viewer')
    await modal.getByRole('button', { name: 'Policies' }).click()
    await page.getByRole('option', { name: 'readonly', exact: true }).click()
    await page.keyboard.press('Escape')
    await modal.getByRole('checkbox', { name: 'Create an access key now' }).check()
    await modal.getByRole('button', { name: 'Create user' }).click()

    const keyModal = page.getByRole('dialog', { name: 'Access key created' })
    await expect(keyModal).toBeVisible()
    secretKey = (await keyModal.getByTestId('secret-key').textContent())?.trim() ?? ''
    accessKey = (await keyModal.locator('dd code').first().textContent())?.trim() ?? ''
    expect(accessKey).not.toBe('')
    expect(secretKey.length).toBeGreaterThanOrEqual(8)
    await keyModal.getByRole('checkbox', { name: 'I have stored the secret key' }).check()
    await keyModal.getByRole('button', { name: 'Done' }).click()
    await expect(keyModal).toBeHidden()

    await expect(page.getByRole('main').getByRole('link', { name: user, exact: true })).toBeVisible()
    const u = await api.admin<{ policies: string[], keys: { accessKey: string }[] }>('GET', `/users/${user}`)
    expect(u.policies).toEqual(['readonly'])
    expect(u.keys.map(k => k.accessKey)).toContain(accessKey)
  })

  test('the read-only user can browse but not upload, and sees no admin pages', async ({ browser, ui }) => {
    const ctx = await browser.newContext()
    const p = await ctx.newPage()
    try {
      await p.goto(ui(`connect?server=${THIS_SERVER}`))
      await fillSignIn(p, accessKey, secretKey)
      await expect(p).not.toHaveURL(/\/connect/)
      await expect(p.getByRole('button', { name: new RegExp(`Account: ${user}`) })).toBeVisible()

      // Admin navigation is hidden for an identity without admin rights.
      const nav = p.getByRole('navigation', { name: 'Server navigation' })
      await expect(nav.getByRole('link', { name: 'Buckets' })).toBeVisible()
      for (const name of ['Users', 'Policies', 'Settings', 'Discovery']) await expect(nav.getByRole('link', { name })).toHaveCount(0)

      // Browsing works.
      await p.goto(ui(`servers/${THIS_SERVER}/buckets`))
      await p.getByRole('main').getByRole('link', { name: bucket, exact: true }).click()
      await p.getByRole('link', { name: 'shared/', exact: true }).click()
      await p.getByRole('link', { name: 'report.txt', exact: true }).click()
      const details = p.getByRole('dialog', { name: 'report.txt' })
      await details.getByRole('tab', { name: 'Preview' }).click()
      await expect(details.getByText('quarterly numbers')).toBeVisible()
      await details.getByRole('button', { name: 'Close' }).first().click()

      // Uploading is denied with a clear message.
      const chooser = p.waitForEvent('filechooser')
      await p.getByRole('main').getByRole('button', { name: 'Upload', exact: true }).click()
      await (await chooser).setFiles([{ name: 'nope.txt', mimeType: 'text/plain', buffer: Buffer.from('x') }])
      const panel = p.getByRole('region', { name: 'Uploads' })
      await expect(panel).toContainText('1 upload failed')
      await expect(panel).toContainText(/access denied|not allowed/i)
    } finally {
      await ctx.close()
    }
  })
})
