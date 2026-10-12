// Regression tests from the third web UI review against a real gos3:
// - uploads ask before overwriting objects the listing does not show
//   (folder uploads, files beyond the loaded rows)
// - file names with a backslash keep it (no subfolder)
// - restoring an old version keeps its public-read ACL
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { Page } from '@playwright/test'
import { confirmDialog, expect, test, THIS_SERVER, uniqueName } from './fixtures'

test.describe.configure({ mode: 'serial' })

function row(page: Page, name: string) {
  return page.getByRole('main').getByRole('row').filter({ has: page.getByRole('link', { name, exact: true }) })
}

/** The useConfirm dialog asking before uploads overwrite objects. */
function overwriteDialog(page: Page) {
  return page.getByRole('dialog').filter({ has: page.getByRole('button', { name: 'Upload and overwrite' }) })
}

test.describe('upload overwrite checks and restores', () => {
  const bucket = uniqueName('review3')
  let page: Page

  test.beforeAll(async ({ browser, signIn, api }) => {
    await api.createBucket(bucket)
    await api.putObject(bucket, 'many/obj-00001.txt', 'ORIGINAL')
    page = await browser.newPage()
    await signIn(page, { redirect: `/servers/${THIS_SERVER}/buckets/${bucket}/objects/` })
  })
  test.afterAll(async ({ api }) => {
    await page.close()
    await api.forceDeleteBucket(bucket)
  })

  test('a folder upload asks before overwriting objects in the folder', async ({ api }) => {
    const dir = mkdtempSync(join(tmpdir(), 'gos3-e2e-'))
    mkdirSync(join(dir, 'many'))
    writeFileSync(join(dir, 'many', 'obj-00001.txt'), 'NEW')
    await expect(row(page, 'many/')).toBeVisible()
    await page.locator('input[type=file][webkitdirectory]').setInputFiles(join(dir, 'many'))
    const dialog = overwriteDialog(page)
    await expect(dialog).toBeVisible()
    await expect(dialog).toContainText('Overwrite 1 existing object?')
    await dialog.getByRole('button', { name: 'Cancel' }).click()
    await expect(dialog).toBeHidden()
    expect((await api.getText(bucket, 'many/obj-00001.txt')).text).toBe('ORIGINAL')
  })

  test('keeps backslashes in uploaded file names', async ({ api }) => {
    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('main').getByRole('button', { name: 'Upload', exact: true }).click()
    await (await chooser).setFiles([{ name: 'report\\2026.txt', mimeType: 'text/plain', buffer: Buffer.from('backslash') }])
    await expect.poll(async () => (await api.getText(bucket, 'report\\2026.txt')).text).toBe('backslash')
    expect((await api.getText(bucket, 'report/2026.txt')).status).toBe(404)
  })

  test('restoring a version keeps its public-read ACL', async ({ api, primary, ui }) => {
    const r = await api.fetch(`/${bucket}?versioning`, { method: 'PUT', body: '<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>' })
    expect(r.status, await r.text()).toBe(200)
    const v1 = (await api.putObject(bucket, 'pub.txt', 'v1', { 'x-amz-acl': 'public-read', 'content-type': 'text/plain' })).headers.get('x-amz-version-id')!
    await api.putObject(bucket, 'pub.txt', 'v2', { 'x-amz-acl': 'public-read', 'content-type': 'text/plain' })
    await page.goto(ui(`servers/${THIS_SERVER}/buckets/${bucket}/objects/?key=pub.txt&versionId=${encodeURIComponent(v1)}`))
    const details = page.getByRole('dialog', { name: 'pub.txt' })
    await details.getByRole('button', { name: 'More' }).click()
    await page.getByRole('menuitem', { name: 'Restore this version' }).click()
    await expect(page.getByText('Version restored', { exact: true })).toBeVisible()
    const anon = await fetch(`${primary.url}/${bucket}/pub.txt`)
    expect(anon.status).toBe(200)
    expect(await anon.text()).toBe('v1')
  })
})

test.describe('uploads beyond the loaded rows', () => {
  const bucket = uniqueName('review3-big')
  let page: Page

  test.beforeAll(async ({ browser, signIn, api }) => {
    await api.createBucket(bucket)
    // More than one page of rows (1,000): the last key is not loaded.
    const keys = Array.from({ length: 1005 }, (_, i) => `obj-${String(i).padStart(5, '0')}.txt`)
    for (let i = 0; i < keys.length; i += 50) {
      await Promise.all(keys.slice(i, i + 50).map(k => api.putObject(bucket, k, 'ORIGINAL')))
    }
    page = await browser.newPage()
    await signIn(page, { redirect: `/servers/${THIS_SERVER}/buckets/${bucket}/objects/` })
  })
  test.afterAll(async ({ api }) => {
    await page.close()
    await api.forceDeleteBucket(bucket)
  })

  test('asks before overwriting an object that is not in the loaded rows', async ({ api }) => {
    await expect(row(page, 'obj-00000.txt')).toBeVisible()
    await expect(row(page, 'obj-01004.txt')).toHaveCount(0)
    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('main').getByRole('button', { name: 'Upload', exact: true }).click()
    await (await chooser).setFiles([{ name: 'obj-01004.txt', mimeType: 'text/plain', buffer: Buffer.from('NEW CONTENT') }])
    await confirmDialog(page, 'Cancel')
    expect((await api.getText(bucket, 'obj-01004.txt')).text).toBe('ORIGINAL')
  })
})
