// Versioning end to end: enable it in the bucket settings, overwrite an
// object, restore an old version, delete (delete marker) and undelete.
import type { Page } from '@playwright/test'
import { confirmDialog, expect, test, THIS_SERVER, uniqueName } from './fixtures'

test.describe.configure({ mode: 'serial' })

function row(page: Page, name: string) {
  return page.getByRole('main').getByRole('row').filter({ has: page.getByRole('link', { name, exact: true }) })
}

async function upload(page: Page, name: string, content: string) {
  const chooser = page.waitForEvent('filechooser')
  await page.getByRole('main').getByRole('button', { name: 'Upload', exact: true }).click()
  await (await chooser).setFiles([{ name, mimeType: 'text/plain', buffer: Buffer.from(content) }])
}

interface Version { id: string, latest: boolean, marker: boolean }

/** Versions of key, newest first, as the server reports them. */
async function versionsOf(api: { getText: (b: string, k: string, q: string) => Promise<{ text: string }> }, bucket: string, key: string): Promise<Version[]> {
  const { text } = await api.getText(bucket, '', `?versions&prefix=${encodeURIComponent(key)}`)
  const out: Version[] = []
  for (const m of text.matchAll(/<(Version|DeleteMarker)(?:\s[^>]*)?>([\s\S]*?)<\/\1>/g)) {
    const body = m[2]!
    out.push({ id: /<VersionId>([^<]*)<\/VersionId>/.exec(body)![1]!, latest: /<IsLatest>true<\/IsLatest>/.test(body), marker: m[1] === 'DeleteMarker' })
  }
  return out
}

test.describe('object versions', () => {
  const bucket = uniqueName('versions')
  let page: Page

  test.beforeAll(async ({ browser, signIn, api }) => {
    await api.createBucket(bucket)
    page = await browser.newPage()
    await signIn(page)
  })
  test.afterAll(async ({ api }) => {
    await page.close()
    await api.forceDeleteBucket(bucket)
  })

  test('enables versioning in the bucket settings', async ({ api, ui }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/buckets/${bucket}/settings?tab=versioning`))
    await page.getByRole('button', { name: 'Enable versioning' }).click()
    await confirmDialog(page, 'Enable versioning')
    await expect(page.getByRole('button', { name: 'Suspend versioning' })).toBeVisible()
    expect((await api.getText(bucket, '', '?versioning')).text).toContain('<Status>Enabled</Status>')
  })

  test('overwrites an object and restores the old version', async ({ api, ui }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/buckets/${bucket}`))
    await expect(page.getByText('Versioned', { exact: true })).toBeVisible()
    await upload(page, 'doc.txt', 'version one')
    await expect(row(page, 'doc.txt')).toBeVisible()
    await expect.poll(async () => (await api.getText(bucket, 'doc.txt')).text).toBe('version one')
    await upload(page, 'doc.txt', 'version two!')
    // The UI asks before overwriting and explains that the old version is kept.
    await expect(page.getByRole('dialog', { name: 'Overwrite 1 existing object?' })).toContainText('kept as previous versions')
    await confirmDialog(page, 'Upload and overwrite')
    await expect.poll(async () => (await api.getText(bucket, 'doc.txt')).text).toBe('version two!')
    const before = await versionsOf(api, bucket, 'doc.txt')
    expect(before).toHaveLength(2)
    const oldId = before.find(v => !v.latest)!.id

    await row(page, 'doc.txt').getByRole('link', { name: 'doc.txt', exact: true }).click()
    const details = page.getByRole('dialog', { name: 'doc.txt' })
    await details.getByRole('tab', { name: 'Versions' }).click()
    const list = details.getByRole('list', { name: 'Object versions, newest first' })
    await expect(list.getByRole('listitem')).toHaveCount(2)

    // View the old version, then restore it (copies it on top as a new version).
    await list.getByRole('button', { name: `View version ${oldId}` }).click()
    await expect(details.getByText(/Viewing version/)).toBeVisible()
    await details.getByRole('tab', { name: 'Versions' }).click()
    await list.getByRole('button', { name: `Restore version ${oldId}` }).click()
    await expect.poll(async () => (await api.getText(bucket, 'doc.txt')).text).toBe('version one')
    await expect(list.getByRole('listitem')).toHaveCount(3)
    expect((await versionsOf(api, bucket, 'doc.txt')).filter(v => !v.marker)).toHaveLength(3)
    await details.getByRole('button', { name: 'Close' }).first().click()
  })

  test('deleting adds a delete marker that can be removed again', async ({ api }) => {
    await row(page, 'doc.txt').getByRole('button', { name: 'Actions for doc.txt' }).click()
    await page.getByRole('menuitem', { name: /Delete/ }).click()
    await confirmDialog(page)
    await expect(row(page, 'doc.txt')).toHaveCount(0)
    expect((await api.getText(bucket, 'doc.txt')).status).toBe(404)
    const versions = await versionsOf(api, bucket, 'doc.txt')
    expect(versions.find(v => v.latest)?.marker).toBe(true)

    // The versions view lists the delete marker next to the old versions.
    await page.getByRole('switch', { name: 'Show versions' }).click()
    const markerRow = page.getByRole('main').getByRole('row').filter({ hasText: 'Delete marker' })
    await expect(markerRow).toHaveCount(1)
    await markerRow.getByRole('link').first().click()
    const details = page.getByRole('dialog', { name: 'doc.txt' })
    await details.getByRole('tab', { name: 'Overview' }).click()
    await expect(details.getByText('This version is a delete marker')).toBeVisible()
    await details.getByRole('tab', { name: 'Versions' }).click()
    await details.getByRole('button', { name: 'Remove delete marker' }).click()
    await confirmDialog(page, 'Remove marker')
    await expect.poll(async () => (await api.getText(bucket, 'doc.txt')).text).toBe('version one')
    expect((await versionsOf(api, bucket, 'doc.txt')).some(v => v.marker)).toBe(false)
  })
})
