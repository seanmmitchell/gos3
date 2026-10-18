import { expect, type Page } from '@playwright/test'

export async function seedProxyConnections(page: Page, ui: string, relay: string, destination: string, accessKeyId: string, secretAccessKey: string, kind: 'gos3' | 's3' = 'gos3'): Promise<void> {
  await page.addInitScript(({ relay, destination, accessKeyId, secretAccessKey, kind }) => {
    const common = { region: 'us-east-1', accessKeyId, secretAccessKey, forcePathStyle: true, remember: true, source: 'user' }
    localStorage.setItem('gos3.connections.v1', JSON.stringify({ version: 1, activeId: 'private-target', dismissedPresets: [], connections: [
      { ...common, id: 'proxy-relay', name: 'Relay server', endpoint: relay, kind: 'gos3' },
      { ...common, id: 'private-target', name: 'Private destination', endpoint: destination, kind, proxyConnectionId: 'proxy-relay' }
    ] }))
  }, { relay, destination, accessKeyId, secretAccessKey, kind })
  await page.route(`${destination}/**`, route => route.abort('connectionrefused'))
}

export async function exerciseProxyObjects(page: Page, ui: string, bucket: string, verify: (key: string) => Promise<string>): Promise<void> {
  await page.goto(ui + `servers/private-target/buckets/${bucket}`)
  await expect(page.getByText('Connected through Relay server', { exact: true })).toBeVisible()
  await expect(page.getByRole('main').getByRole('link', { name: 'hello.txt', exact: true })).toBeVisible()
  const chooser = page.waitForEvent('filechooser')
  await page.getByRole('main').getByRole('button', { name: 'Upload', exact: true }).click()
  await (await chooser).setFiles({ name: 'multipart.txt', mimeType: 'text/plain', buffer: Buffer.alloc(17 * 1024 * 1024, 97) })
  await expect(page.getByRole('main').getByRole('link', { name: 'multipart.txt', exact: true })).toBeVisible({ timeout: 60000 })
  expect((await verify('multipart.txt')).length).toBe(17 * 1024 * 1024)
  await page.getByRole('main').getByRole('link', { name: 'hello.txt', exact: true }).click()
  const details = page.getByRole('dialog', { name: 'hello.txt' })
  await expect(details.getByText('19 B (19 bytes)', { exact: true })).toBeVisible()
  await details.getByRole('tab', { name: 'Preview', exact: true }).click()
  await expect(details.getByText('hello through relay', { exact: true })).toBeVisible()
  await details.getByRole('button', { name: 'Share', exact: true }).click()
  const share = page.getByRole('dialog', { name: 'Share link' })
  const field = share.getByRole('textbox').last()
  await expect(field).toHaveValue(/\/_gos3\/admin\/v1\/proxy\?/)
  const url = await field.inputValue()
  const read = await fetch(url)
  expect(read.status).toBe(200)
  expect(await read.text()).toBe('hello through relay')
  const range = await fetch(url, { headers: { Range: 'bytes=0-4' } })
  expect(range.status).toBe(206)
  expect(await range.text()).toBe('hello')
  expect(range.headers.get('Content-Range')).toBe('bytes 0-4/19')
  await share.getByRole('button', { name: 'Close', exact: true }).last().click()
  const download = page.waitForEvent('download')
  await details.getByRole('button', { name: 'Download', exact: true }).click()
  expect((await download).suggestedFilename()).toBe('hello.txt')
  await details.getByRole('button', { name: 'Close', exact: true }).click()
  await page.getByRole('main').getByRole('link', { name: 'pixel.png', exact: true }).click()
  const imageDetails = page.getByRole('dialog', { name: 'pixel.png' })
  await imageDetails.getByRole('tab', { name: 'Preview', exact: true }).click()
  const image = imageDetails.getByRole('img', { name: 'Preview of pixel.png' })
  await expect(image).toHaveAttribute('src', /\/_gos3\/admin\/v1\/proxy\?/)
  await expect(image).toHaveJSProperty('naturalWidth', 1)
}
