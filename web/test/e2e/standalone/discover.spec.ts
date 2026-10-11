// Standalone UI: the global Discover page. Local discovery runs on the UI's
// Node server (/api/discovery/*): a scan of 127.0.0.1 with both gos3 ports
// finds both instances; a URL can be probed from the browser (CORS "*" on
// /_gos3/info) or from the UI host; a found server is added in one click.
import { request as httpRequest } from 'node:http'
import { addServerViaUi, expect, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, serverDialog, test } from './fixtures'

/** POST /api/discovery/probe with explicit Host / Origin headers (DNS rebinding). */
function probeWithHost(uiUrl: string, host: string, origin: string | undefined, target: string): Promise<{ status: number, body: string }> {
  const u = new URL(uiUrl)
  const body = JSON.stringify({ url: target })
  return new Promise((resolve, reject) => {
    const headers: Record<string, string> = { 'host': host, 'content-type': 'application/json', 'content-length': String(Buffer.byteLength(body)) }
    if (origin) headers.origin = origin
    const req = httpRequest({ host: '127.0.0.1', port: u.port, method: 'POST', path: '/api/discovery/probe', headers }, (res) => {
      let text = ''
      res.setEncoding('utf8')
      res.on('data', (c: string) => (text += c))
      res.on('end', () => resolve({ status: res.statusCode ?? 0, body: text }))
    })
    req.on('error', reject)
    req.end(body)
  })
}

test('the UI host offers local discovery', async ({ page, ui }) => {
  const caps = await page.request.get(ui('api/discovery/capabilities'))
  expect(caps.ok()).toBe(true)
  expect(await caps.json()).toMatchObject({ localDiscovery: true, scan: true })
  // POSTs require JSON (CSRF protection for the scan/probe routes).
  const form = await page.request.post(ui('api/discovery/probe'), { form: { url: 'http://127.0.0.1:9' } })
  expect(form.status()).toBe(415)
  // Failures carry a coarse reason only, never socket error codes.
  const down = await page.request.post(ui('api/discovery/probe'), { data: { url: 'http://127.0.0.1:9' } })
  expect(down.status()).toBe(502)
  expect((await down.json()).error.message).toBe('http://127.0.0.1:9: unreachable')
  // Cloud metadata addresses are never probed (also in IPv4-mapped form).
  for (const url of ['http://169.254.169.254/latest/meta-data', 'http://[::ffff:a9fe:a9fe]']) {
    const meta = await page.request.post(ui('api/discovery/probe'), { data: { url } })
    expect(meta.status(), url).toBe(400)
  }
  // Request bodies are capped (8 KiB for probes).
  const big = await page.request.post(ui('api/discovery/probe'), { data: { url: `http://127.0.0.1:9/${'a'.repeat(16 * 1024)}` } })
  expect(big.status()).toBe(413)

  await page.goto(ui('discover'))
  await expect(page.getByRole('heading', { name: 'Discover servers', level: 1 })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Search local network' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Scan from the UI host' })).toBeVisible()
})

test('rejects requests for a foreign Host (DNS rebinding) or Origin', async ({ ui, alpha }) => {
  const port = new URL(ui()).port
  const foreign = await probeWithHost(ui(), `attacker.example:${port}`, `http://attacker.example:${port}`, alpha.url)
  expect(foreign.status).toBe(403)
  expect(foreign.body).not.toContain(alpha.id)
  const crossOrigin = await probeWithHost(ui(), `localhost:${port}`, 'http://evil.example', alpha.url)
  expect(crossOrigin.status).toBe(403)
  const own = await probeWithHost(ui(), `localhost:${port}`, `http://localhost:${port}`, alpha.url)
  expect(own.status).toBe(200)
  expect(own.body).toContain(alpha.id)
})

test('scans 127.0.0.1 from the UI host and finds both servers', async ({ page, ui, alpha, beta }) => {
  await page.goto(ui('discover'))
  const scan = page.locator('form').filter({ has: page.getByRole('textbox', { name: 'Targets' }) })
  await scan.getByRole('textbox', { name: 'Targets' }).fill('127.0.0.1')
  await scan.getByRole('textbox', { name: 'Ports' }).fill(`${alpha.port}, ${beta.port}`)
  await expect(scan.getByRole('checkbox', { name: 'http', exact: true })).toBeChecked()
  await expect(scan.getByRole('checkbox', { name: 'https', exact: true })).not.toBeChecked()
  await scan.getByRole('button', { name: 'Start scan' }).click()
  await expect(page.getByText(/^2 servers found, 2 URLs probed/)).toBeVisible()

  const results = page.getByRole('region', { name: /Servers found/ })
  for (const s of [alpha, beta]) {
    const row = results.getByRole('row').filter({ hasText: s.id })
    await expect(row.getByText(s.name, { exact: true })).toBeVisible()
    await expect(row.getByText('UI host (scan)')).toBeVisible()
    await expect(row.getByText(s.url)).toBeVisible()
  }

  // One click: "Add" opens the server form prefilled from the scan result.
  await results.getByRole('button', { name: `Add ${beta.name} as a server` }).click()
  const dialog = serverDialog(page)
  await expect(dialog.getByLabel('Endpoint URL')).toHaveValue(beta.url)
  await dialog.getByLabel('Access key ID').fill(ROOT_ACCESS_KEY)
  await dialog.getByLabel('Secret access key').fill(ROOT_SECRET_KEY)
  await dialog.getByRole('button', { name: 'Add server' }).click()
  await expect(dialog).toBeHidden()
  const added = results.getByRole('row').filter({ hasText: beta.id })
  await expect(added.getByText('Configured')).toBeVisible()
  await expect(added.getByRole('link', { name: `Open ${beta.name}` })).toBeVisible()
})

test('probes a URL directly from the browser and from the UI host', async ({ page, ui, alpha, beta }) => {
  await page.goto(ui('discover'))
  const probe = page.getByRole('textbox', { name: 'URLs or hosts' })
  // A bare host:port gets http://; an unreachable entry is listed as not found.
  await probe.fill(`${alpha.url}\n127.0.0.1:${beta.port}\nhttp://127.0.0.1:9`)
  await page.getByRole('button', { name: 'Probe', exact: true }).click()
  await expect(page.getByText('2 gos3 servers found', { exact: true })).toBeVisible()
  const results = page.getByRole('region', { name: /Servers found/ })
  await expect(results.getByRole('row').filter({ hasText: alpha.id }).getByText('This browser (direct probe)')).toBeVisible()
  await expect(results.getByRole('row').filter({ hasText: beta.id }).getByText('This browser (direct probe)')).toBeVisible()
  await expect(page.getByText('http://127.0.0.1:9', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: 'Clear probe results' }).click()
  await page.getByRole('radio', { name: 'UI host' }).check()
  await probe.fill(alpha.url)
  await page.getByRole('button', { name: 'Probe', exact: true }).click()
  await expect(page.getByText('1 gos3 server found', { exact: true })).toBeVisible()
  await expect(results.getByRole('row').filter({ hasText: alpha.id }).getByText('UI host (probe)')).toBeVisible()
})

test('merges the peer tables of connected gos3 servers', async ({ page, ui, alpha, beta, alphaApi }) => {
  // alpha learns about beta through its own (admin API) probe.
  const r = await alphaApi.fetch('/_gos3/admin/v1/discovery/probe', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ url: beta.url })
  })
  expect(r.status, await r.clone().text()).toBe(200)

  await addServerViaUi(page, ui, alpha, 'Alpha')
  await page.goto(ui('discover'))
  const results = page.getByRole('region', { name: /Servers found/ })
  const row = results.getByRole('row').filter({ hasText: beta.id })
  // Found by the connected server "Alpha" (its peer table).
  await expect(row.getByText('Alpha', { exact: true })).toBeVisible()
})

test('the UI host routes are off unless GOS3_UI_LOCAL_DISCOVERY=true', async ({ page, env, alpha }) => {
  // presetUi runs without GOS3_UI_LOCAL_DISCOVERY.
  const presetUi = (path: string) => `${env.presetUi.url}/${path}`
  const caps = await page.request.get(presetUi('api/discovery/capabilities'))
  expect(caps.ok()).toBe(true)
  expect(await caps.json()).toMatchObject({ localDiscovery: false, scan: false })
  const scan = await page.request.post(presetUi('api/discovery/scan'), { data: { targets: ['127.0.0.1'], ports: [alpha.port] } })
  expect(scan.status()).toBe(403)
  expect((await scan.json()).error.message).toMatch(/disabled/)
  const probe = await page.request.post(presetUi('api/discovery/probe'), { data: { url: alpha.url } })
  expect(probe.status()).toBe(403)
  expect(await probe.text()).not.toContain(alpha.id)
  const local = await page.request.get(presetUi('api/discovery/local?wait=500'))
  expect(local.status()).toBe(403)

  await page.goto(presetUi('discover'))
  await expect(page.getByText(/Turned off on the UI server/)).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Scan from the UI host' })).toHaveCount(0)
  // Probing from the browser still works.
  await page.getByRole('textbox', { name: 'URLs or hosts' }).fill(alpha.url)
  await expect(page.getByRole('radio', { name: 'UI host' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Probe', exact: true }).click()
  await expect(page.getByText('1 gos3 server found', { exact: true })).toBeVisible()
})
