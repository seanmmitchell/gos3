// IAM policies: built-ins are read-only, custom policies can be created,
// edited and deleted, and the simulator evaluates them on the server.
import type { Page } from '@playwright/test'
import { confirmDialog, expect, test, THIS_SERVER, uniqueName } from './fixtures'

test.describe.configure({ mode: 'serial' })

test.describe('policies', () => {
  const policy = uniqueName('e2e-photos-ro')
  const user = uniqueName('simuser').replace(/-/g, '_').slice(0, 40)
  const bucket = uniqueName('photos')
  let page: Page

  const doc = {
    Version: '2012-10-17',
    Statement: [{ Sid: 'ReadPhotos', Effect: 'Allow', Action: ['s3:GetObject'], Resource: [`arn:aws:s3:::${bucket}/*`] }]
  }

  test.beforeAll(async ({ browser, signIn }) => {
    page = await browser.newPage()
    await signIn(page)
  })
  test.afterAll(async ({ api }) => {
    await page.close()
    await api.forceDeleteBucket(bucket)
    await api.admin('DELETE', `/users/${user}`).catch(() => undefined)
    await api.admin('DELETE', `/policies/${policy}`).catch(() => undefined)
  })

  test('built-in policies are read-only', async ({ ui }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/policies`))
    const row = page.getByRole('row').filter({ has: page.getByRole('link', { name: 'readonly', exact: true }) })
    await expect(row).toContainText('Built-in')
    await row.getByRole('link', { name: 'readonly', exact: true }).click()
    await expect(page).toHaveURL(/\/policies\/readonly$/)
    await expect(page.getByText('Built-in policy', { exact: true })).toBeVisible()
    await expect(page.getByRole('textbox', { name: 'Policy document' })).toHaveAttribute('readonly', /.*/)
    await expect(page.getByRole('button', { name: 'Save policy' })).toHaveCount(0)
    await expect(page.getByRole('button', { name: 'Delete' })).toHaveCount(0)
  })

  test('creates a custom policy', async ({ ui, api }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/policies`))
    await page.getByRole('main').getByRole('button', { name: 'Create policy' }).first().click()
    const modal = page.getByRole('dialog', { name: 'Create policy' })
    await modal.getByRole('textbox', { name: /Policy name/ }).fill(policy)
    await modal.getByRole('textbox', { name: 'Description' }).fill('read photos')
    await modal.getByRole('textbox', { name: 'Policy document' }).fill(JSON.stringify(doc, null, 2))
    await modal.getByRole('button', { name: 'Create policy' }).click()
    await expect(modal).toBeHidden()
    const stored = await api.admin<{ description: string, document: typeof doc }>('GET', `/policies/${policy}`)
    expect(stored.description).toBe('read photos')
    // IAM JSON allows a string or a list; the server may normalise either way.
    expect([stored.document.Statement[0]!.Resource].flat()).toEqual([`arn:aws:s3:::${bucket}/*`])
  })

  test('edits the custom policy', async ({ ui, api }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/policies/${policy}`))
    await expect(page.getByRole('heading', { name: policy })).toBeVisible()
    await page.getByRole('textbox', { name: 'Description' }).fill('read photos and list')
    const edited = { ...doc, Statement: [...doc.Statement, { Sid: 'List', Effect: 'Allow', Action: ['s3:ListBucket'], Resource: [`arn:aws:s3:::${bucket}`] }] }
    await page.getByRole('textbox', { name: 'Policy document' }).fill(JSON.stringify(edited, null, 2))
    await expect(page.getByText('Unsaved changes')).toBeVisible()
    await page.getByRole('button', { name: 'Save policy' }).click()
    await expect.poll(async () => (await api.admin<{ description: string }>('GET', `/policies/${policy}`)).description).toBe('read photos and list')
    const stored = await api.admin<{ document: typeof doc }>('GET', `/policies/${policy}`)
    expect(stored.document.Statement.map(s => s.Sid)).toEqual(['ReadPhotos', 'List'])

    // An invalid document is rejected with an explanation.
    await page.getByRole('textbox', { name: 'Policy document' }).fill('{"Version":"2012-10-17","Statement":[{"Effect":"Maybe","Action":"s3:*","Resource":"*"}]}')
    await page.getByRole('button', { name: 'Save policy' }).click()
    await expect(page.getByText(/Effect must be "Allow" or "Deny"/).first()).toBeVisible()
    await page.getByRole('button', { name: 'Reset' }).click()
    await confirmDialog(page, 'Discard')
    await expect(page.getByText('Unsaved changes')).toHaveCount(0)
  })

  test('the simulator evaluates the policy for a user', async ({ ui, api }) => {
    await api.admin('POST', '/users', { name: user, policies: [policy] })
    await page.goto(ui(`servers/${THIS_SERVER}/policies/${policy}`))
    const sim = page.getByRole('main').locator('form').filter({ has: page.getByRole('button', { name: 'Simulate' }) })
    await sim.getByRole('button', { name: 'User' }).click()
    await page.getByRole('option', { name: user, exact: true }).click()
    await sim.getByRole('button', { name: 'Action' }).click()
    await page.getByPlaceholder('Search or type an action…').fill('s3:GetObject')
    await page.getByRole('option', { name: 's3:GetObject', exact: true }).first().click()
    await sim.getByRole('textbox', { name: 'Bucket' }).fill(bucket)
    await sim.getByRole('textbox', { name: 'Object key' }).fill('cat.jpg')
    await sim.getByRole('button', { name: 'Simulate' }).click()
    // The bucket does not exist yet: the error stays visible next to the form.
    const failure = page.getByRole('alert').filter({ hasText: 'Simulation failed' })
    await expect(failure).toBeVisible()
    await expect(failure).toContainText(/bucket/i)

    await api.createBucket(bucket)
    await sim.getByRole('button', { name: 'Simulate' }).click()
    await expect(failure).toHaveCount(0)
    await expect(page.getByText('Allowed', { exact: true })).toBeVisible()

    await sim.getByRole('button', { name: 'Action' }).click()
    await page.getByPlaceholder('Search or type an action…').fill('s3:PutObject')
    await page.getByRole('option', { name: 's3:PutObject', exact: true }).first().click()
    await sim.getByRole('button', { name: 'Simulate' }).click()
    await expect(page.getByText('Denied (no matching allow)')).toBeVisible()
  })

  test('deletes the custom policy', async ({ ui, api }) => {
    await api.admin('DELETE', `/users/${user}`)
    await page.goto(ui(`servers/${THIS_SERVER}/policies/${policy}`))
    await page.getByRole('button', { name: 'Delete' }).click()
    await confirmDialog(page)
    await expect(page).toHaveURL(/\/policies$/)
    const r = await api.fetch(`/_gos3/admin/v1/policies/${policy}`)
    expect(r.status).toBe(404)
  })
})
