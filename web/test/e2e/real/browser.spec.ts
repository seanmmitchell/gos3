// Object browser against a real gos3: buckets, uploads (single PUT and
// multipart), folders, previews, share links, downloads, tags, metadata,
// rename/move and delete. Each step is verified behind the UI's back with a
// signed Node client.
import { readFile } from 'node:fs/promises'
import type { Page } from '@playwright/test'
import { bytes, expect, test, THIS_SERVER, uniqueName } from './fixtures'

test.describe.configure({ mode: 'serial' })

const MiB = 1024 * 1024
// 1×1 red PNG.
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg==', 'base64')

async function uploadFiles(page: Page, files: { name: string, mimeType: string, buffer: Buffer }[]) {
  const chooser = page.waitForEvent('filechooser')
  await page.getByRole('main').getByRole('button', { name: 'Upload', exact: true }).click()
  await (await chooser).setFiles(files)
}

function row(page: Page, name: string) {
  return page.getByRole('main').getByRole('row').filter({ has: page.getByRole('link', { name, exact: true }) })
}

test.describe('object browser', () => {
  const bucket = uniqueName('browse')
  const lockBucket = uniqueName('locked')
  let page: Page

  test.beforeAll(async ({ browser, signIn }) => {
    page = await browser.newPage()
    await signIn(page, { redirect: `/servers/${THIS_SERVER}/buckets` })
  })
  test.afterAll(async ({ api }) => {
    await page.close()
    await api.forceDeleteBucket(bucket)
    await api.forceDeleteBucket(lockBucket)
  })

  test('creates buckets (plain and with object lock)', async ({ api, ui }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/buckets`))
    await page.getByRole('main').getByRole('button', { name: 'Create bucket' }).first().click()
    const dialog = page.getByRole('dialog', { name: 'Create bucket' })
    await dialog.getByRole('textbox', { name: /Bucket name/ }).fill(bucket)
    await dialog.getByRole('button', { name: 'Create bucket' }).click()
    await expect(dialog).toBeHidden()
    // The new bucket opens (or is listed); either way it exists on the server.
    await expect.poll(async () => (await api.fetch(`/${bucket}`, { method: 'HEAD' })).status).toBe(200)

    await page.goto(ui(`servers/${THIS_SERVER}/buckets`))
    await expect(page.getByRole('main').getByRole('link', { name: bucket, exact: true })).toBeVisible()

    await page.getByRole('main').getByRole('button', { name: 'Create bucket' }).first().click()
    await dialog.getByRole('textbox', { name: /Bucket name/ }).fill(lockBucket)
    await dialog.getByRole('switch', { name: 'Object lock' }).click()
    await expect(dialog.getByRole('switch', { name: 'Versioning' })).toBeChecked()
    await dialog.getByRole('button', { name: 'Create bucket' }).click()
    await expect(dialog).toBeHidden()
    await expect.poll(async () => (await api.getText(lockBucket, '', '?object-lock')).text).toContain('<ObjectLockEnabled>Enabled</ObjectLockEnabled>')
    const versioning = await api.getText(lockBucket, '', '?versioning')
    expect(versioning.text).toContain('<Status>Enabled</Status>')
  })

  test('uploads small files and a 20 MiB file (multipart)', async ({ api, ui }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/buckets/${bucket}`))
    await expect(page.getByRole('heading', { name: bucket, level: 1 })).toBeVisible()
    await uploadFiles(page, [
      { name: 'hello.txt', mimeType: 'text/plain', buffer: Buffer.from('Hello from the gos3 e2e test!\n') },
      { name: 'pixel.png', mimeType: 'image/png', buffer: PNG }
    ])
    await expect(row(page, 'hello.txt')).toBeVisible()
    await expect(row(page, 'pixel.png')).toBeVisible()
    expect((await api.getText(bucket, 'hello.txt')).text).toBe('Hello from the gos3 e2e test!\n')

    const big = Buffer.from(bytes(20 * MiB, 7))
    await uploadFiles(page, [{ name: 'big.bin', mimeType: 'application/octet-stream', buffer: big }])
    await expect(row(page, 'big.bin')).toBeVisible({ timeout: 60_000 })
    await expect(row(page, 'big.bin')).toContainText('20 MiB')
    const head = await api.fetch(`/${bucket}/big.bin`, { method: 'HEAD' })
    expect(head.status).toBe(200)
    expect(head.headers.get('content-length')).toBe(String(20 * MiB))
    // A multipart ETag is md5-of-part-md5s with a "-<parts>" suffix.
    expect(head.headers.get('etag')).toMatch(/^"[0-9a-f]{32}-\d+"$/)
    const got = new Uint8Array(await (await api.fetch(`/${bucket}/big.bin`)).arrayBuffer())
    expect(Buffer.compare(Buffer.from(got), big)).toBe(0)
  })

  test('creates a folder and browses it with breadcrumbs', async ({ api }) => {
    await page.getByRole('main').getByRole('button', { name: 'New folder' }).first().click()
    const dialog = page.getByRole('dialog', { name: 'New folder' })
    await dialog.getByRole('textbox', { name: /Folder name/ }).fill('docs')
    await dialog.getByRole('button', { name: 'Create folder' }).click()
    await expect(dialog).toBeHidden()
    expect((await api.fetch(`/${bucket}/docs/`, { method: 'HEAD' })).status).toBe(200)

    // A new folder is opened right away or listed; open it.
    if (!/\/objects\/docs\/$/.test(page.url())) await row(page, 'docs/').getByRole('link', { name: 'docs/', exact: true }).click()
    await expect(page).toHaveURL(/\/objects\/docs\/$/)
    const crumbs = page.getByRole('navigation', { name: 'Folder path' })
    await expect(crumbs).toContainText('docs')
    await uploadFiles(page, [{ name: 'notes.md', mimeType: 'text/markdown', buffer: Buffer.from('# Notes\n\nsome *markdown*\n') }])
    await expect(row(page, 'notes.md')).toBeVisible()
    expect((await api.getText(bucket, 'docs/notes.md')).status).toBe(200)

    await crumbs.getByRole('link', { name: bucket }).click()
    await expect(row(page, 'docs/')).toBeVisible()
    await expect(row(page, 'hello.txt')).toBeVisible()
  })

  test('previews text and images', async () => {
    await row(page, 'hello.txt').getByRole('link', { name: 'hello.txt', exact: true }).click()
    const details = page.getByRole('dialog', { name: 'hello.txt' })
    await expect(details).toBeVisible()
    await details.getByRole('tab', { name: 'Preview' }).click()
    await expect(details.getByText('Hello from the gos3 e2e test!')).toBeVisible()
    await details.getByRole('button', { name: 'Close' }).first().click()
    await expect(details).toBeHidden()

    await row(page, 'pixel.png').getByRole('link', { name: 'pixel.png', exact: true }).click()
    const img = page.getByRole('dialog', { name: 'pixel.png' })
    await img.getByRole('tab', { name: 'Preview' }).click()
    const el = img.locator('img')
    await expect(el).toBeVisible()
    await expect.poll(() => el.evaluate(i => (i as HTMLImageElement).naturalWidth)).toBe(1)
    await img.getByRole('button', { name: 'Close' }).first().click()
  })

  test('creates a share link that serves the content', async () => {
    await row(page, 'hello.txt').getByRole('link', { name: 'hello.txt', exact: true }).click()
    const details = page.getByRole('dialog', { name: 'hello.txt' })
    await details.getByRole('button', { name: 'Share' }).click()
    const share = page.getByRole('dialog', { name: 'Share link' })
    await expect(share).toBeVisible()
    const link = share.getByRole('textbox').last()
    await expect(link).toHaveValue(/X-Amz-Signature=/)
    const url = await link.inputValue()
    // Fetch outside the browser: a presigned URL needs no credentials.
    const r = await fetch(url)
    expect(r.status).toBe(200)
    expect(await r.text()).toBe('Hello from the gos3 e2e test!\n')
    await share.getByRole('button', { name: 'Close' }).last().click()
    await expect(share).toBeHidden()
  })

  test('downloads an object', async () => {
    const details = page.getByRole('dialog', { name: 'hello.txt' })
    const dl = page.waitForEvent('download')
    await details.getByRole('button', { name: 'Download' }).click()
    const download = await dl
    expect(download.suggestedFilename()).toBe('hello.txt')
    expect(await readFile(await download.path(), 'utf8')).toBe('Hello from the gos3 e2e test!\n')
  })

  test('edits tags and metadata', async ({ api }) => {
    const details = page.getByRole('dialog', { name: 'hello.txt' })
    await details.getByRole('tab', { name: 'Tags' }).click()
    await details.getByRole('button', { name: 'Add tag' }).click()
    await details.getByRole('textbox', { name: /^Tag/ }).last().fill('project')
    await details.getByRole('textbox', { name: /^Value/ }).last().fill('e2e')
    await details.getByRole('button', { name: 'Save tags' }).click()
    await expect.poll(async () => (await api.getText(bucket, 'hello.txt', '?tagging')).text).toMatch(/<Key>project<\/Key>\s*<Value>e2e<\/Value>/)

    await details.getByRole('tab', { name: 'Metadata' }).click()
    await details.getByRole('textbox', { name: 'Cache-Control' }).fill('max-age=60')
    await details.getByRole('button', { name: 'Add metadata' }).click()
    await details.getByRole('textbox', { name: /^Name/ }).last().fill('owner')
    await details.getByRole('textbox', { name: /^Value/ }).last().fill('playwright')
    await details.getByRole('button', { name: 'Save metadata' }).click()
    await expect.poll(async () => (await api.fetch(`/${bucket}/hello.txt`, { method: 'HEAD' })).headers.get('x-amz-meta-owner')).toBe('playwright')
    const head = await api.fetch(`/${bucket}/hello.txt`, { method: 'HEAD' })
    expect(head.headers.get('cache-control')).toBe('max-age=60')
    expect(head.headers.get('content-type')).toMatch(/^text\/plain/)
    // Content and tags survive the metadata copy.
    expect((await api.getText(bucket, 'hello.txt')).text).toBe('Hello from the gos3 e2e test!\n')
    expect((await api.getText(bucket, 'hello.txt', '?tagging')).text).toContain('<Key>project</Key>')

    await details.getByRole('tab', { name: 'Overview' }).click()
    await expect(details.getByText('playwright')).toBeVisible()
    await details.getByRole('button', { name: 'Close' }).first().click()
  })

  test('renames and moves objects', async ({ api }) => {
    await row(page, 'pixel.png').getByRole('button', { name: 'Actions for pixel.png' }).click()
    await page.getByRole('menuitem', { name: /Rename/ }).click()
    const dialog = page.getByRole('dialog').filter({ has: page.getByRole('textbox', { name: /New name/ }) })
    await dialog.getByRole('textbox', { name: /New name/ }).fill('red.png')
    await dialog.getByRole('button', { name: 'Rename' }).click()
    await expect(row(page, 'red.png')).toBeVisible()
    await expect(row(page, 'pixel.png')).toHaveCount(0)
    expect((await api.fetch(`/${bucket}/red.png`, { method: 'HEAD' })).status).toBe(200)
    expect((await api.fetch(`/${bucket}/pixel.png`, { method: 'HEAD' })).status).toBe(404)

    await row(page, 'red.png').getByRole('button', { name: 'Actions for red.png' }).click()
    await page.getByRole('menuitem', { name: /Move/ }).click()
    const move = page.getByRole('dialog').filter({ has: page.getByRole('textbox', { name: /Destination folder/ }) })
    await move.getByRole('textbox', { name: /Destination folder/ }).fill('docs/')
    await move.getByRole('button', { name: 'Move' }).click()
    await expect(row(page, 'red.png')).toHaveCount(0)
    await expect.poll(async () => (await api.fetch(`/${bucket}/docs/red.png`, { method: 'HEAD' })).status).toBe(200)
  })

  test('deletes objects and folders', async ({ api }) => {
    await row(page, 'big.bin').getByRole('button', { name: 'Actions for big.bin' }).click()
    await page.getByRole('menuitem', { name: /Delete/ }).click()
    const confirm = page.getByRole('alertdialog').or(page.getByRole('dialog')).filter({ hasText: /Delete/ }).last()
    await confirm.getByRole('button', { name: /^Delete/ }).click()
    await expect(row(page, 'big.bin')).toHaveCount(0)
    expect((await api.fetch(`/${bucket}/big.bin`, { method: 'HEAD' })).status).toBe(404)

    await page.getByRole('checkbox', { name: 'Select docs' }).click()
    await page.getByRole('region', { name: 'Selection actions' }).getByRole('button', { name: 'Delete' }).click()
    const confirm2 = page.getByRole('alertdialog').or(page.getByRole('dialog')).filter({ hasText: /Delete/ }).last()
    await confirm2.getByRole('button', { name: /^Delete/ }).click()
    await expect(row(page, 'docs/')).toHaveCount(0)
    const list = await api.getText(bucket, '', '?list-type=2&prefix=docs/')
    expect(list.text).not.toContain('<Key>')
  })
})
