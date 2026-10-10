// Standalone UI: managing connections to real gos3 servers cross-origin —
// add through the servers page (type detection, test connection,
// credentials), export/import, and presets (servers.json + NUXT_PUBLIC_SERVERS).
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { PRESET_ENV_ID, PRESET_FILE_ENDPOINT, PRESET_FILE_ID, WEB_DIR } from './env'
import { addServerViaUi, expect, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, serverDialog, test, watchPage } from './fixtures'

test('adds a gos3 server: detects the type, tests the connection and signs in', async ({ page, ui, alpha }) => {
  await page.goto(ui('servers'))
  await expect(page.getByText('No servers yet')).toBeVisible()
  await page.getByRole('button', { name: 'Add server' }).first().click()
  const dialog = serverDialog(page)
  await expect(dialog).toBeVisible()

  // Type detection: unsigned GET /_gos3/info (CORS "*").
  await dialog.getByLabel('Endpoint URL').fill(alpha.url)
  await expect(dialog.getByText(/gos3 \S+ · “sa-alpha” · region us-east-1/)).toBeVisible()

  // A wrong secret is explained, not just "failed".
  await dialog.getByLabel('Access key ID').fill(ROOT_ACCESS_KEY)
  await dialog.getByLabel('Secret access key').fill('not-the-secret')
  await dialog.getByRole('button', { name: 'Test connection' }).click()
  const results = dialog.getByRole('region', { name: 'Connection test results' })
  await expect(results.getByText('Connection test failed')).toBeVisible()
  await expect(results.getByText('Wrong secret key')).toBeVisible()

  // Correct credentials: reach → admin sign-in (whoami) → signed ListBuckets,
  // all cross-origin through the server's CORS.
  await dialog.getByLabel('Secret access key').fill(ROOT_SECRET_KEY)
  await dialog.getByRole('button', { name: 'Test connection' }).click()
  await expect(results.getByText('Connection works')).toBeVisible()
  await expect(results.getByText('gos3 detected')).toBeVisible()
  await expect(results.getByText(/Signed in as .*root/)).toBeVisible()
  await expect(results.getByText(/Signed S3 request succeeded/)).toBeVisible()

  await dialog.getByLabel('Display name').fill('Alpha')
  await dialog.getByRole('button', { name: 'Add server' }).click()
  await expect(dialog).toBeHidden()

  const row = page.getByRole('row').filter({ hasText: 'Alpha' })
  await expect(row.getByText('gos3', { exact: true })).toBeVisible()
  await expect(row.getByText('Online')).toBeVisible()
  await expect(row.getByText('This session')).toBeVisible()

  // Secrets are kept in sessionStorage only (remember is off).
  const stored = await page.evaluate(() => localStorage.getItem('gos3.connections.v1') ?? '')
  expect(stored).toContain(alpha.url)
  expect(stored).not.toContain(ROOT_SECRET_KEY)

  // The server's overview works with the admin API (whoami / stats) cross-origin.
  await row.getByRole('link', { name: 'Alpha' }).click()
  await expect(page).toHaveURL(/\/servers\/[^/]+$/)
  await expect(page.getByRole('button', { name: /^Account: root/ })).toBeVisible()
  await expect(page.getByRole('heading', { name: /sa-alpha|Alpha/ }).first()).toBeVisible()
})

test('"Add server" in the sidebar opens the new-server form', async ({ page, ui, alpha }) => {
  await addServerViaUi(page, ui, alpha, 'Alpha')
  await page.getByRole('link', { name: 'Alpha' }).click()
  await page.getByRole('button', { name: /^Active server: Alpha/ }).click()
  await page.getByRole('menuitem', { name: 'Add server' }).click()
  await expect(page).toHaveURL(/\/connect\?new=1$/)
  await expect(page.getByLabel('Endpoint URL')).toBeVisible()
})

test('exports connections and imports them into another browser', async ({ page, ui, alpha, beta, browser }) => {
  await addServerViaUi(page, ui, alpha, 'Alpha')
  await addServerViaUi(page, ui, beta, 'Beta')

  await page.getByRole('button', { name: 'Export' }).click()
  const exportDialog = page.getByRole('dialog', { name: 'Export servers' })
  const json = exportDialog.getByLabel('Exported JSON')
  await expect(json).toHaveValue(/"type": "gos3-connections"/)
  expect(await json.inputValue()).not.toContain(ROOT_SECRET_KEY)
  await exportDialog.getByRole('switch', { name: 'Include access keys and secret keys' }).click()
  await expect(json).toHaveValue(new RegExp(ROOT_SECRET_KEY))
  const [download] = await Promise.all([page.waitForEvent('download'), exportDialog.getByRole('button', { name: 'Download' }).click()])
  expect(download.suggestedFilename()).toMatch(/^gos3-connections-\d{4}-\d{2}-\d{2}-with-secrets\.json$/)
  const file = await download.path()
  const exported = JSON.parse(readFileSync(file, 'utf8')) as { connections: { name: string, endpoint: string, secretAccessKey?: string }[] }
  expect(exported.connections.map(c => c.name).sort()).toEqual(['Alpha', 'Beta'])
  expect(exported.connections.find(c => c.name === 'Beta')).toMatchObject({ endpoint: beta.url, secretAccessKey: ROOT_SECRET_KEY })

  // A fresh browser profile (empty storage) imports the file.
  const other = await browser.newContext()
  try {
    const p2 = await other.newPage()
    watchPage(p2)
    await p2.goto(ui('servers'))
    await expect(p2.getByText('No servers yet')).toBeVisible()
    await p2.getByRole('button', { name: 'Import' }).first().click()
    const importDialog = p2.getByRole('dialog', { name: 'Import servers' })
    await importDialog.getByLabel('Choose a JSON file').setInputFiles(file)
    await expect(importDialog.getByText('2 servers found.')).toBeVisible()
    await importDialog.getByRole('button', { name: 'Import' }).click()
    await expect(importDialog).toBeHidden()
    for (const name of ['Alpha', 'Beta']) {
      const row = p2.getByRole('row').filter({ hasText: name })
      await expect(row.getByText('Online')).toBeVisible()
      await expect(row.getByText(ROOT_ACCESS_KEY)).toBeVisible()
    }
  } finally {
    await other.close()
  }
})

test('loads presets from servers.json and NUXT_PUBLIC_SERVERS', async ({ page, env, beta }) => {
  // The servers.json preset lives only in the setup's temp copy of the sources.
  expect(existsSync(join(WEB_DIR, 'public', 'servers.json'))).toBe(false)

  const presetUi = (path: string) => `${env.presetUi.url}/${path}`
  await page.goto(presetUi('servers'))
  const fromFile = page.getByRole('row').filter({ hasText: 'Preset from file' })
  const fromEnv = page.getByRole('row').filter({ hasText: 'Preset from env' })
  await expect(fromFile.getByText('Preset', { exact: true })).toBeVisible()
  await expect(fromFile.getByText(PRESET_FILE_ENDPOINT)).toBeVisible()
  await expect(fromEnv.getByText('Preset', { exact: true })).toBeVisible()
  await expect(fromEnv.getByText(beta.url)).toBeVisible()
  // The env preset ships the access key; the secret is entered by the user.
  await expect(fromEnv.getByText('Not signed in')).toBeVisible()
  await expect(fromEnv.getByText(ROOT_ACCESS_KEY)).toBeVisible()
  // Without credentials the status comes from the public /_gos3/info.
  await expect(fromEnv.getByText('Online')).toBeVisible()
  await expect(fromFile.getByText('Offline')).toBeVisible()
  await expect(fromFile.getByRole('link', { name: 'Preset from file' })).toHaveAttribute('href', new RegExp(`/connect\\?server=${PRESET_FILE_ID}`))

  // Preset definitions are read-only: only credentials can be changed.
  await page.getByRole('button', { name: 'Edit Preset from env' }).click()
  const dialog = serverDialog(page)
  await expect(dialog.getByLabel('Endpoint URL')).toBeDisabled()
  await dialog.getByRole('button', { name: 'Cancel' }).click()

  // Sign in to the env preset through the connect page.
  await fromEnv.getByRole('link', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(new RegExp(`/connect\\?server=${PRESET_ENV_ID}`))
  await expect(page.getByLabel('Access key ID')).toHaveValue(ROOT_ACCESS_KEY)
  await page.getByLabel('Secret access key').fill(ROOT_SECRET_KEY)
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page).toHaveURL(new RegExp(`/servers/${PRESET_ENV_ID}$`))
  await expect(page.getByRole('button', { name: /^Account: root/ })).toBeVisible()

  // Both presets also appear on the dashboard.
  await page.goto(presetUi(''))
  await expect(page.getByText('Preset from file').first()).toBeVisible()
  await expect(page.getByText('Preset from env').first()).toBeVisible()
})
