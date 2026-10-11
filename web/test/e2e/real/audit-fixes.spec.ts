// Security audit fixes seen end to end against a real gos3:
// - C1: admin requests with a body must sign the payload (the UI does);
// - C2/C3: self-service key creation needs admin:CreateOwnAccessKey, probing
//   needs admin:ProbeNetwork, and the UI explains both;
// - web-3: an import never retargets the embedded connection.
import { createHash } from 'node:crypto'
import type { Page } from '@playwright/test'
import { expect, fillSignIn, test, THIS_SERVER, uniqueName, watchPage } from './fixtures'

interface CreatedUser { key: { accessKey: string, secretKey: string } }

async function signInAs(page: Page, ui: (p?: string) => string, accessKey: string, secretKey: string, path: string): Promise<void> {
  watchPage(page)
  await page.goto(ui(`connect?server=${THIS_SERVER}&redirect=${encodeURIComponent(path)}`))
  await fillSignIn(page, accessKey, secretKey)
  await expect(page).not.toHaveURL(/\/connect/)
}

test.describe('admin payload signing (C1)', () => {
  const user = uniqueName('signed').replace(/-/g, '_').slice(0, 40)

  test.afterAll(async ({ api }) => {
    await api.admin('DELETE', `/users/${user}`).catch(() => undefined)
  })

  test('rejects an unsigned admin body and accepts a signed one', async ({ api }) => {
    await api.admin('POST', '/users', { name: user, policies: ['readonly'] })
    const body = JSON.stringify({ description: 'unsigned' })
    const unsigned = await api.fetch(`/_gos3/admin/v1/users/${user}`, {
      method: 'PATCH',
      headers: { 'content-type': 'application/json', 'x-amz-content-sha256': 'UNSIGNED-PAYLOAD' },
      body
    })
    expect(unsigned.status).toBe(400)
    expect(await unsigned.text()).toContain('InvalidRequest')

    const signed = await api.fetch(`/_gos3/admin/v1/users/${user}`, {
      method: 'PATCH',
      headers: { 'content-type': 'application/json', 'x-amz-content-sha256': createHash('sha256').update(body).digest('hex') },
      body
    })
    expect(signed.status).toBe(200)
    expect((await api.admin<{ description?: string }>('GET', `/users/${user}`)).description).toBe('unsigned')
  })
})

test.describe('self-service keys need admin:CreateOwnAccessKey (C3)', () => {
  test.describe.configure({ mode: 'serial' })
  const user = uniqueName('selfkeys').replace(/-/g, '_').slice(0, 40)
  const policy = uniqueName('e2e-own-keys')
  let creds: CreatedUser['key']

  test.beforeAll(async ({ api }) => {
    creds = (await api.admin<CreatedUser>('POST', '/users', { name: user, policies: ['readonly'], createKey: true })).key
  })
  test.afterAll(async ({ api }) => {
    await api.admin('DELETE', `/users/${user}`).catch(() => undefined)
    await api.admin('DELETE', `/policies/${policy}`).catch(() => undefined)
  })

  test('without the grant the keys are listed but cannot be created', async ({ page, ui }) => {
    await signInAs(page, ui, creds.accessKey, creds.secretKey, `/servers/${THIS_SERVER}/users`)
    await expect(page.getByRole('heading', { name: 'My access keys' }).first()).toBeVisible()
    await expect(page.getByRole('main').getByText(creds.accessKey)).toBeVisible()
    await expect(page.getByTestId('create-own-key-denied')).toContainText('admin:CreateOwnAccessKey')
    await expect(page.getByRole('main').getByRole('button', { name: 'Create key' })).toHaveCount(0)
  })

  test('with the grant the user creates a key for itself', async ({ page, ui, api }) => {
    await api.admin('PUT', `/policies/${policy}`, {
      document: { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: ['admin:CreateOwnAccessKey'], Resource: ['arn:gos3:admin:::*'] }] }
    })
    await api.admin('PATCH', `/users/${user}`, { policies: ['readonly', policy] })

    await signInAs(page, ui, creds.accessKey, creds.secretKey, `/servers/${THIS_SERVER}/users`)
    await expect(page.getByRole('main').getByText(creds.accessKey)).toBeVisible()
    await expect(page.getByTestId('create-own-key-denied')).toHaveCount(0)
    await page.getByRole('main').getByRole('button', { name: 'Create key' }).click()
    const modal = page.getByRole('dialog', { name: 'Create my access key' })
    await modal.getByRole('textbox', { name: 'Description' }).fill('e2e self-service')
    await modal.getByRole('button', { name: 'Create key' }).click()
    const created = page.getByRole('dialog', { name: 'Access key created' })
    await expect(created).toBeVisible()
    await created.getByRole('checkbox', { name: 'I have stored the secret key' }).check()
    await created.getByRole('button', { name: 'Done' }).click()
    await expect(page.getByRole('main').getByText('e2e self-service')).toBeVisible()
    const u = await api.admin<{ keys: { description?: string }[] }>('GET', `/users/${user}`)
    expect(u.keys.some(k => k.description === 'e2e self-service')).toBe(true)
  })
})

test.describe('probing needs admin:ProbeNetwork (C3)', () => {
  const user = uniqueName('diag').replace(/-/g, '_').slice(0, 40)
  let creds: CreatedUser['key']

  test.beforeAll(async ({ api }) => {
    creds = (await api.admin<CreatedUser>('POST', '/users', { name: user, policies: ['diagnostics'], createKey: true })).key
  })
  test.afterAll(async ({ api }) => {
    await api.admin('DELETE', `/users/${user}`).catch(() => undefined)
  })

  test('a diagnostics user reads the peer table but is told it may not probe', async ({ page, ui, peer }) => {
    await signInAs(page, ui, creds.accessKey, creds.secretKey, `/servers/${THIS_SERVER}/discovery`)
    await expect(page.getByRole('region', { name: /^Peers/ }).getByRole('row', { name: new RegExp(peer.name) })).toBeVisible()
    const probeCard = page.getByRole('main').locator('div')
      .filter({ has: page.getByRole('heading', { name: 'Probe a URL' }) })
      .filter({ hasText: 'admin:ProbeNetwork' })
      .last()
    await expect(probeCard).toContainText('admin:ProbeNetwork')
    await expect(probeCard.getByRole('button', { name: 'Probe' })).toHaveCount(0)
    await expect(probeCard.getByRole('textbox', { name: 'URL' })).toHaveCount(0)
  })
})

test.describe('import never retargets the embedded connection (web-3)', () => {
  test('an entry for this-server pointing elsewhere is added as a separate server', async ({ page, signIn, primary }) => {
    await signIn(page, { redirect: '/servers' })
    await page.getByRole('button', { name: 'Import' }).first().click()
    const dialog = page.getByRole('dialog', { name: 'Import servers' })
    await dialog.getByRole('textbox').fill(JSON.stringify({ connections: [{ id: THIS_SERVER, name: 'Hijack', endpoint: 'http://127.0.0.1:9' }] }))
    const preview = dialog.getByRole('list', { name: 'Import preview' })
    await expect(preview).toContainText('Add Hijack (http://127.0.0.1:9) without credentials as a separate server')
    await expect(preview).toContainText('is a built-in embedded server and is never changed by an import')
    await dialog.getByRole('button', { name: 'Import' }).click()
    await expect(dialog).toBeHidden()

    const stored = await page.evaluate(() => JSON.parse(localStorage.getItem('gos3.connections.v1') ?? '{}') as { connections: { id: string, name: string, endpoint: string, accessKeyId: string }[] })
    const self = stored.connections.find(c => c.id === THIS_SERVER)!
    expect(self.endpoint).toBe(primary.url)
    expect(self.accessKeyId).not.toBe('')
    const added = stored.connections.find(c => c.name === 'Hijack')!
    expect(added.id).not.toBe(THIS_SERVER)
    expect(added).toMatchObject({ endpoint: 'http://127.0.0.1:9', accessKeyId: '' })
  })
})
