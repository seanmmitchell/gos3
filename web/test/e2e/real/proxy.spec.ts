import { test, expect, ServerApi, uniqueName, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, THIS_SERVER } from './fixtures'
import { seedProxyConnections, exerciseProxyObjects } from '../proxy-scenarios'

test('gos3 administration, multipart and downloads through a reachable relay', async ({ page, primary, peer, ui }) => {
  const api = new ServerApi(peer)
  const bucket = uniqueName('proxy')
  const user = uniqueName('proxy-user')
  await api.createBucket(bucket)
  await api.putObject(bucket, 'hello.txt', 'hello through relay', { 'Content-Type': 'text/plain' })
  await api.putObject(bucket, 'pixel.png', Uint8Array.from(Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=', 'base64')), { 'Content-Type': 'image/png' })
  try {
    await seedProxyConnections(page, ui(), primary.url, peer.url, ROOT_ACCESS_KEY, ROOT_SECRET_KEY)
    await exerciseProxyObjects(page, ui(), bucket, async key => (await api.getText(bucket, key)).text)
    await page.goto(ui('servers/private-target/users'))
    await page.getByRole('main').getByRole('button', { name: /Create user/ }).first().click()
    const dialog = page.getByRole('dialog', { name: 'Create user' })
    await dialog.getByRole('textbox', { name: /User name/ }).fill(user)
    await dialog.getByRole('checkbox', { name: 'Create an access key now' }).uncheck()
    await dialog.getByRole('button', { name: 'Create user', exact: true }).click()
    await expect(dialog).toBeHidden()
    expect(await api.admin('GET', `/users/${user}`)).toMatchObject({ name: user })
  } finally {
    await api.admin('DELETE', `/users/${user}`).catch(() => undefined)
    await api.forceDeleteBucket(bucket)
  }
})

test('discovery offers the connected server as fallback when adding an unreachable peer', async ({ page, peer, signIn, ui }) => {
  await signIn(page)
  await page.route(`${peer.url}/**`, route => route.abort('connectionrefused'))
  await page.goto(ui(`servers/${THIS_SERVER}/discovery`))
  await page.getByRole('button', { name: `Add ${peer.name} as a server` }).click()
  const dialog = page.getByRole('dialog', { name: 'Add server' })
  await expect(dialog.getByRole('combobox', { name: 'Fallback relay' })).toContainText('e2e-primary')
  await dialog.getByRole('textbox', { name: 'Access key ID' }).fill(ROOT_ACCESS_KEY)
  await dialog.getByLabel('Secret access key', { exact: true }).fill(ROOT_SECRET_KEY)
  await dialog.getByRole('checkbox', { name: /I trust/ }).check()
  await dialog.getByRole('button', { name: 'Test connection', exact: true }).click()
  await expect(dialog.getByText('Signed S3 request succeeded:', { exact: false })).toBeVisible()
  await dialog.getByRole('button', { name: 'Add server', exact: true }).click()
  await expect(dialog).toBeHidden()
  const stored = await page.evaluate(() => JSON.parse(localStorage.getItem('gos3.connections.v1')!).connections as { endpoint: string, proxyConnectionId?: string }[])
  expect(stored.find(c => c.endpoint === peer.url)?.proxyConnectionId).toBe(THIS_SERVER)
})
