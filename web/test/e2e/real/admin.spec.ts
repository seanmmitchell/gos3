// Server-level pages against a real gos3: overview metrics, persisted server
// settings and discovery with a second gos3 instance as the peer.
import { confirmDialog, expect, test, THIS_SERVER, uniqueName } from './fixtures'

test.describe('server overview', () => {
  test('renders identity, storage and request metrics', async ({ page, signIn, api, primary, ui }) => {
    const bucket = uniqueName('metrics')
    await api.createBucket(bucket)
    await api.putObject(bucket, 'm.txt', 'metrics')
    try {
      await signIn(page)
      await expect(page).toHaveURL(/\/servers\/this-server$/)
      const main = page.getByRole('main')
      await expect(main.getByText('Server ID')).toBeVisible()
      await expect(main.getByText(primary.id)).toBeVisible()
      await expect(main.getByRole('heading', { name: /Storage computed/ })).toBeVisible()
      const requests = main.getByRole('region', { name: /^Requests since/ })
      await expect(requests).toBeVisible()
      // Our own requests show up in the counters and in the per-operation table.
      await expect(requests.getByRole('figure', { name: /Requests per minute/ })).toBeVisible()
      const ops = main.getByRole('region', { name: 'Operations' }).getByRole('table')
      await expect(ops.getByRole('row', { name: /PutObject/ })).toBeVisible()
      await expect(ops.getByRole('row', { name: /^Admin/ })).toBeVisible()

      // The multi-server dashboard shows the hosting server online with its storage totals.
      await page.goto(ui(''))
      await expect(page.getByRole('heading', { name: 'Dashboard', level: 1 })).toBeVisible()
      const card = page.getByRole('article', { name: /\(this server\)$/ })
      await expect(card).toContainText('Online')
      await expect(card.getByRole('group', { name: 'Statistics' })).toContainText(/bucket/)
    } finally {
      await api.forceDeleteBucket(bucket)
    }
  })
})

test.describe('server settings', () => {
  test('changes the name and beacon interval; both persist', async ({ page, signIn, ui, api, primary }) => {
    const original = await api.admin<{ name: string, discovery: { beacon: boolean, beaconInterval: number } }>('GET', '/config')
    const name = `e2e-renamed-${Date.now().toString(36)}`
    try {
      await signIn(page, { redirect: `/servers/${THIS_SERVER}/settings` })
      await expect(page.getByRole('heading', { name: 'Server settings' })).toBeVisible()
      const nameInput = page.getByRole('textbox', { name: /Server name/ })
      await expect(nameInput).toHaveValue(original.name)
      await nameInput.fill(name)
      await page.getByRole('switch', { name: 'UDP beacon' }).click()
      const interval = page.getByRole('spinbutton', { name: /Beacon interval/ })
      await interval.fill('25')
      await interval.blur()
      await expect(page.getByText('Unsaved changes', { exact: true })).toBeVisible()
      await page.getByRole('button', { name: /^Save/ }).click()
      await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0)

      // Stored on the server (settings.json) and applied live.
      const saved = await api.admin<{ name: string, discovery: { beacon: boolean, beaconInterval: number } }>('GET', '/config')
      expect(saved.name).toBe(name)
      expect(saved.discovery.beaconInterval).toBe(25_000_000_000)
      expect(saved.discovery.beacon).toBe(true)
      expect((await (await fetch(`${primary.url}/_gos3/info`)).json()).name).toBe(name)

      await page.reload()
      await expect(page.getByRole('textbox', { name: /Server name/ })).toHaveValue(name)
      await expect(page.getByRole('spinbutton', { name: /Beacon interval/ })).toHaveValue('25')
    } finally {
      // Restore for the other specs (the server name shows up in several places).
      const cur = await api.admin<Record<string, unknown> & { discovery: Record<string, unknown> }>('GET', '/config')
      await api.admin('PUT', '/config', { ...cur, name: original.name, discovery: { ...cur.discovery, beacon: original.discovery.beacon, beaconInterval: original.discovery.beaconInterval } })
    }
  })

  test('rejects invalid input before saving', async ({ page, signIn, api }) => {
    const before = JSON.stringify(await api.admin('GET', '/config'))
    await signIn(page, { redirect: `/servers/${THIS_SERVER}/settings` })
    // The interval input is bounded like the server (1 s – 1 h).
    await page.getByRole('switch', { name: 'UDP beacon' }).click()
    const interval = page.getByRole('spinbutton', { name: /Beacon interval/ })
    await interval.fill('7200')
    await interval.blur()
    await expect(interval).toHaveValue('3,600')
    const group = page.getByRole('textbox', { name: /Beacon group/ })
    await group.fill('10.0.0.1:7373')
    await page.getByRole('button', { name: /^Save/ }).click()
    await expect(page.getByText(/must be a multicast address/)).toBeVisible()
    expect(JSON.stringify(await api.admin('GET', '/config'))).toBe(before)
  })
})

test.describe('discovery', () => {
  test('shows the seeded peer, probes, scans and adds it as a server', async ({ page, signIn, ui, primary, peer }) => {
    await signIn(page, { redirect: `/servers/${THIS_SERVER}/discovery` })
    await expect(page.getByRole('heading', { name: 'Discovery', level: 1 })).toBeVisible()
    await expect(page.getByRole('list', { name: 'Discovery mechanisms' })).toContainText('mDNS: off')

    const peers = page.getByRole('region', { name: /^Peers/ })
    const peerRow = peers.getByRole('row', { name: new RegExp(peer.name) })
    await expect(peerRow).toBeVisible()
    await expect(peerRow).toContainText('Seed')
    await expect(peerRow).toContainText('Healthy')
    await expect(peerRow).toContainText(peer.url)

    // Probe: the primary fetches the peer's /_gos3/info.
    await page.getByRole('textbox', { name: 'URL' }).fill(peer.url)
    await page.getByRole('button', { name: 'Probe' }).click()
    const probeCard = page.getByRole('main').locator('div')
      .filter({ has: page.getByRole('heading', { name: 'Probe a URL' }) })
      .filter({ has: page.getByRole('textbox', { name: 'URL' }) })
      .last()
    await expect(probeCard.getByRole('button', { name: 'Add as server' })).toBeVisible()
    await expect(probeCard).toContainText(peer.id)

    // Scan 127.0.0.1 on both ports.
    await page.getByRole('textbox', { name: /Targets/ }).fill('127.0.0.1')
    await page.getByRole('textbox', { name: 'Ports' }).fill(`${primary.port},${peer.port}`)
    await page.getByRole('button', { name: 'Start scan' }).click()
    const scan = page.getByRole('main').locator('div')
      .filter({ has: page.getByRole('heading', { name: 'Scan the network' }) })
      .filter({ has: page.getByRole('button', { name: /Start scan|Scanning/ }) })
      .last()
    // The scanning server skips itself: only the peer is reported.
    await expect(scan).toContainText(/1 found · 2 URLs probed/, { timeout: 30_000 })
    await expect(scan.getByRole('row', { name: new RegExp(peer.name) })).toContainText('Scan')

    // Add the peer as a server (credentials are shared between both instances).
    await peerRow.getByRole('button', { name: `Add ${peer.name} as a server` }).click()
    const modal = page.getByRole('dialog').filter({ has: page.getByRole('button', { name: 'Add server' }) })
    await expect(modal.getByRole('textbox', { name: /Endpoint URL/ })).toHaveValue(peer.url)
    const same = modal.getByRole('checkbox', { name: /Use the same credentials as/ })
    if (await same.count()) await same.check()
    else {
      await modal.getByRole('textbox', { name: 'Access key ID' }).fill('e2eroot')
      await modal.getByRole('textbox', { name: 'Secret access key' }).fill('e2eroot-secret-key')
    }
    await modal.getByRole('button', { name: 'Add server' }).click()
    await expect(modal).toBeHidden()
    await expect(peerRow.getByRole('link', { name: new RegExp(`Open ${peer.name}`) }).or(peerRow.getByRole('button', { name: new RegExp(`Open ${peer.name}`) }))).toBeVisible()

    // The new connection works from the primary's embedded UI (cross-origin:
    // the peer allows the primary's origin): signed admin and S3 requests.
    await page.goto(ui('servers'))
    const peerLink = page.getByRole('main').getByRole('link', { name: peer.name, exact: true })
    const peerRowInList = page.getByRole('main').getByRole('row').filter({ has: page.getByRole('link', { name: peer.name, exact: true }) })
    await expect(peerRowInList).toContainText('Online')
    await peerLink.click()
    await expect(page.getByRole('main').getByText(peer.id)).toBeVisible()
    await expect(page.getByRole('main').getByText('Signed in as')).toBeVisible()
    await page.getByRole('navigation', { name: 'Server navigation' }).getByRole('link', { name: 'Buckets' }).click()
    await expect(page.getByRole('heading', { name: 'Buckets', level: 1 })).toBeVisible()
    await expect(page.getByRole('main').getByText(/buckets? on/)).toBeVisible()
    await expect(page.getByRole('alert')).toHaveCount(0)
  })
})
