// Standalone UI: object browsing and uploads against a gos3 on another
// origin. Everything here depends on the server's CORS: signed requests,
// the multipart upload reading ETag, and the metadata editor reading
// x-amz-meta-* response headers (if those were not exposed, the editor would
// show no metadata and saving would wipe it).
import type { Page } from '@playwright/test'
import { addServerViaUi, expect, test, uniqueName } from './fixtures'

test.describe.configure({ mode: 'serial' })

const MiB = 1024 * 1024

function row(page: Page, name: string) {
  return page.getByRole('main').getByRole('row').filter({ has: page.getByRole('link', { name, exact: true }) })
}

/** Deterministic pseudo-random bytes. */
function bytes(size: number, seed: number): Buffer {
  const b = Buffer.alloc(size)
  let x = seed
  for (let i = 0; i < size; i++) {
    x ^= x << 13
    x ^= x >>> 17
    x ^= x << 5
    b[i] = x & 0xff
  }
  return b
}

test.describe('object browser (cross-origin)', () => {
  const bucket = uniqueName('sa-browse')
  let page: Page
  let connId = ''

  test.beforeAll(async ({ browser, ui, alpha, alphaApi }) => {
    await alphaApi.createBucket(bucket)
    await alphaApi.putObject(bucket, 'reports/q3.txt', 'Quarterly numbers\n', {
      'content-type': 'text/plain',
      'cache-control': 'max-age=300',
      'x-amz-meta-project': 'apollo',
      'x-amz-meta-owner': 'ops-team'
    })
    page = await browser.newPage()
    connId = await addServerViaUi(page, ui, alpha, 'Alpha')
  })
  test.afterAll(async ({ alphaApi }) => {
    await page.close()
    await alphaApi.forceDeleteBucket(bucket)
  })

  test('lists buckets and browses folders', async ({ ui }) => {
    await page.goto(ui(`servers/${connId}/buckets`))
    await page.getByRole('main').getByRole('link', { name: bucket, exact: true }).click()
    await expect(page.getByRole('heading', { name: bucket, level: 1 })).toBeVisible()
    await row(page, 'reports/').getByRole('link', { name: 'reports/', exact: true }).click()
    await expect(page.getByRole('navigation', { name: 'Folder path' })).toContainText('reports')
    await expect(row(page, 'q3.txt')).toBeVisible()
  })

  test('uploads with PutObject and multipart (reads ETag cross-origin)', async ({ alphaApi }) => {
    const big = bytes(18 * MiB, 11)
    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('main').getByRole('button', { name: 'Upload', exact: true }).click()
    await (await chooser).setFiles([
      { name: 'notes.md', mimeType: 'text/markdown', buffer: Buffer.from('# Notes\n') },
      { name: 'big.bin', mimeType: 'application/octet-stream', buffer: big }
    ])
    await expect(row(page, 'notes.md')).toBeVisible()
    await expect(row(page, 'big.bin')).toBeVisible({ timeout: 60_000 })
    expect(await alphaApi.getText(bucket, 'reports/notes.md')).toBe('# Notes\n')
    const head = await alphaApi.head(bucket, 'reports/big.bin')
    expect(head.headers.get('content-length')).toBe(String(big.length))
    // Multipart ETag: the browser had to read each part's ETag header.
    expect(head.headers.get('etag')).toMatch(/^"[0-9a-f]{32}-\d+"$/)
  })

  test('shows existing metadata and ETag, and keeps metadata on edit', async ({ alphaApi }) => {
    const etag = (await alphaApi.head(bucket, 'reports/q3.txt')).headers.get('etag')!.replace(/"/g, '')
    await row(page, 'q3.txt').getByRole('link', { name: 'q3.txt', exact: true }).click()
    const details = page.getByRole('dialog', { name: 'q3.txt' })
    await expect(details).toBeVisible()
    await expect(details.getByText(etag)).toBeVisible()
    await expect(details.getByText('apollo')).toBeVisible()

    await details.getByRole('tab', { name: 'Metadata' }).click()
    await expect(details.getByRole('textbox', { name: 'Cache-Control' })).toHaveValue('max-age=300')
    const names = details.getByRole('textbox', { name: /^Name/ })
    const values = details.getByRole('textbox', { name: /^Value/ })
    await expect(names).toHaveCount(2)
    const pairs = new Map<string, string>()
    for (let i = 0; i < 2; i++) pairs.set(await names.nth(i).inputValue(), await values.nth(i).inputValue())
    expect(Object.fromEntries(pairs)).toEqual({ project: 'apollo', owner: 'ops-team' })

    await details.getByRole('button', { name: 'Add metadata' }).click()
    await names.last().fill('reviewed')
    await values.last().fill('yes')
    await details.getByRole('button', { name: 'Save metadata' }).click()
    await expect.poll(async () => (await alphaApi.head(bucket, 'reports/q3.txt')).headers.get('x-amz-meta-reviewed')).toBe('yes')
    const head = await alphaApi.head(bucket, 'reports/q3.txt')
    expect(head.headers.get('x-amz-meta-project')).toBe('apollo')
    expect(head.headers.get('x-amz-meta-owner')).toBe('ops-team')
    expect(head.headers.get('cache-control')).toBe('max-age=300')
    expect(await alphaApi.getText(bucket, 'reports/q3.txt')).toBe('Quarterly numbers\n')
  })
})
