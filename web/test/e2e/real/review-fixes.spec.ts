// Regression tests from the web UI review against a real gos3:
// - keys with '.' / '..' segments never address another object
// - folders cannot be renamed into themselves (data loss)
// - transfers ask before overwriting existing objects
// - editing metadata keeps a public-read ACL
import { request as httpRequest } from 'node:http'
import type { Page } from '@playwright/test'
import { confirmDialog, expect, test, THIS_SERVER, uniqueName, type ServerApi } from './fixtures'

test.describe.configure({ mode: 'serial' })

function row(page: Page, name: string) {
  return page.getByRole('main').getByRole('row').filter({ has: page.getByRole('link', { name, exact: true }) })
}

/** Anonymous PUT with the raw (unnormalised) path, as other S3 clients send it. */
function rawPut(base: string, path: string, body: string): Promise<number> {
  const u = new URL(base)
  return new Promise((resolve, reject) => {
    const req = httpRequest({ host: u.hostname, port: u.port, method: 'PUT', path, headers: { 'content-length': Buffer.byteLength(body) } }, (res) => {
      res.resume()
      res.on('end', () => resolve(res.statusCode ?? 0))
    })
    req.on('error', reject)
    req.end(body)
  })
}

async function listKeys(api: ServerApi, bucket: string): Promise<string[]> {
  const r = await api.getText(bucket, '', '?list-type=2&encoding-type=url')
  return [...r.text.matchAll(/<Key>([^<]*)<\/Key>/g)].map(m => decodeURIComponent(m[1]!.replace(/\+/g, ' ')))
}

test.describe('object browser review fixes', () => {
  const bucket = uniqueName('review')
  let page: Page

  test.beforeAll(async ({ browser, signIn, api, primary }) => {
    await api.createBucket(bucket)
    await api.putObject(bucket, 'y', 'precious Y')
    await api.putObject(bucket, 'x/f', 'AAA')
    await api.putObject(bucket, 'x/y/f', 'BBB')
    await api.putObject(bucket, 'a.txt', 'A')
    await api.putObject(bucket, 'b.txt', 'B')
    await api.putObject(bucket, 'pub.txt', 'public', { 'x-amz-acl': 'public-read', 'content-type': 'text/plain' })
    // Create 'x/../y' like boto3 / the AWS CLI would (raw path), through a
    // temporary public-write policy (Node's signed fetch would normalise it).
    const policy = JSON.stringify({ Version: '2012-10-17', Statement: [{ Effect: 'Allow', Principal: '*', Action: 's3:PutObject', Resource: `arn:aws:s3:::${bucket}/*` }] })
    const pr = await api.fetch(`/${bucket}?policy`, { method: 'PUT', body: policy, headers: { 'content-type': 'application/json' } })
    expect(pr.status, await pr.text()).toBeLessThan(300)
    expect(await rawPut(primary.url, `/${bucket}/x/../y`, 'dotdot object')).toBe(200)
    await api.fetch(`/${bucket}?policy`, { method: 'DELETE' })
    expect(await listKeys(api, bucket)).toContain('x/../y')

    page = await browser.newPage()
    await signIn(page, { redirect: `/servers/${THIS_SERVER}/buckets/${bucket}/objects/` })
  })
  test.afterAll(async ({ api }) => {
    await page.close()
    await api.forceDeleteBucket(bucket)
  })

  test('a key with ".." segments is never confused with the key it normalises to', async ({ api, ui }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/buckets/${bucket}/objects/?key=${encodeURIComponent('x/../y')}`))
    const details = page.getByRole('dialog', { name: 'y' })
    await expect(details.getByText(/contains a “\.” or “\.\.” path segment/)).toBeVisible()
    // Nothing of 'y' is shown as if it were this object.
    await expect(details.getByText('precious Y')).toHaveCount(0)
    await details.getByRole('button', { name: 'More' }).click()
    await page.getByRole('menuitem', { name: /^Delete/ }).click()
    await confirmDialog(page)
    await expect.poll(() => listKeys(api, bucket)).not.toContain('x/../y')
    expect((await api.getText(bucket, 'y')).text).toBe('precious Y')
  })

  test('refuses to rename a folder into its own subfolder', async ({ api, ui }) => {
    await page.goto(ui(`servers/${THIS_SERVER}/buckets/${bucket}/objects/`))
    await row(page, 'x/').getByRole('button', { name: 'Actions for x' }).click()
    await page.getByRole('menuitem', { name: /Rename/ }).click()
    const dialog = page.getByRole('dialog').filter({ has: page.getByRole('textbox', { name: /New name/ }) })
    await dialog.getByRole('textbox', { name: /New name/ }).fill('x/y')
    await expect(dialog.getByText(/into itself/)).toBeVisible()
    await expect(dialog.getByRole('button', { name: 'Rename' })).toBeDisabled()
    await dialog.getByRole('button', { name: 'Cancel' }).click()
    expect((await api.getText(bucket, 'x/y/f')).text).toBe('BBB')
    expect((await api.getText(bucket, 'x/f')).text).toBe('AAA')
  })

  test('asks before a rename overwrites an existing object', async ({ api }) => {
    await row(page, 'a.txt').getByRole('button', { name: 'Actions for a.txt' }).click()
    await page.getByRole('menuitem', { name: /Rename/ }).click()
    const dialog = page.getByRole('dialog').filter({ hasText: 'Rename “a.txt”' })
    await dialog.getByRole('textbox', { name: /New name/ }).fill('b.txt')
    await dialog.getByRole('button', { name: 'Rename' }).click()
    await expect(dialog.getByText('1 object already exists at the destination')).toBeVisible()
    await dialog.getByRole('button', { name: 'Back' }).click()
    expect((await api.getText(bucket, 'b.txt')).text).toBe('B')
    await dialog.getByRole('button', { name: 'Rename' }).click()
    await dialog.getByRole('button', { name: 'Rename and overwrite' }).click()
    await expect(dialog).toBeHidden()
    expect((await api.getText(bucket, 'b.txt')).text).toBe('A')
    expect((await api.getText(bucket, 'a.txt')).status).toBe(404)
  })

  test('keeps a public-read ACL when metadata is edited and when renamed', async ({ primary }) => {
    const anon = (key: string) => fetch(`${primary.url}/${bucket}/${key}`).then(r => r.status)
    expect(await anon('pub.txt')).toBe(200)
    await row(page, 'pub.txt').getByRole('link', { name: 'pub.txt', exact: true }).click()
    const details = page.getByRole('dialog', { name: 'pub.txt' })
    await details.getByRole('tab', { name: 'Metadata' }).click()
    await details.getByRole('textbox', { name: 'Cache-Control' }).fill('max-age=5')
    await details.getByRole('button', { name: 'Save metadata' }).click()
    await expect(page.getByText('Metadata saved', { exact: true })).toBeVisible()
    expect(await anon('pub.txt')).toBe(200)
    await details.getByRole('button', { name: 'More' }).click()
    await page.getByRole('menuitem', { name: /Rename/ }).click()
    const dialog = page.getByRole('dialog').filter({ hasText: 'Rename “pub.txt”' })
    await dialog.getByRole('textbox', { name: /New name/ }).fill('pub2.txt')
    await dialog.getByRole('button', { name: 'Rename' }).click()
    await expect(dialog).toBeHidden()
    await expect.poll(() => anon('pub2.txt')).toBe(200)
  })
})
