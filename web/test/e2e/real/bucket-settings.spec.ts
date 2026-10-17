// Bucket settings tabs against a real gos3; every change is verified with
// the S3 API (and anonymous access with a plain fetch).
import type { Page } from '@playwright/test'
import { confirmDialog, expect, test, THIS_SERVER, uniqueName } from './fixtures'

test.describe.configure({ mode: 'serial' })

test.describe('bucket settings', () => {
  const bucket = uniqueName('settings')
  const locked = uniqueName('lockcfg')
  let page: Page

  test.beforeAll(async ({ browser, signIn, api }) => {
    await api.createBucket(bucket)
    await api.createBucket(locked, { 'x-amz-bucket-object-lock-enabled': 'true' })
    await api.putObject(bucket, 'public/readme.txt', 'anyone may read me')
    page = await browser.newPage()
    await signIn(page)
  })
  test.afterAll(async ({ api }) => {
    await page.close()
    await api.forceDeleteBucket(bucket)
    await api.forceDeleteBucket(locked)
  })

  const open = (ui: (p: string) => string, b: string, tab: string) => page.goto(ui(`servers/${THIS_SERVER}/buckets/${b}/settings?tab=${tab}`))

  test('tabs are navigable', async ({ ui }) => {
    await open(ui, bucket, 'overview')
    const tabs = page.getByRole('main').getByRole('tablist').first()
    for (const name of ['Versioning', 'Policy', 'CORS', 'Lifecycle', 'Tags', 'Encryption', 'Object lock', 'Access', 'Danger zone', 'Overview']) {
      await tabs.getByRole('tab', { name }).click()
      await expect(page.getByRole('tabpanel', { name })).toBeVisible()
    }
  })

  test('versioning', async ({ ui, api }) => {
    await open(ui, bucket, 'versioning')
    await page.getByRole('button', { name: 'Enable versioning' }).click()
    await confirmDialog(page, 'Enable versioning')
    await expect(page.getByRole('button', { name: 'Suspend versioning' })).toBeVisible()
    expect((await api.getText(bucket, '', '?versioning')).text).toContain('<Status>Enabled</Status>')
    await page.getByRole('button', { name: 'Suspend versioning' }).click()
    await confirmDialog(page, 'Suspend')
    await expect(page.getByRole('button', { name: 'Enable versioning' })).toBeVisible()
    expect((await api.getText(bucket, '', '?versioning')).text).toContain('<Status>Suspended</Status>')
  })

  test('policy template "Public read" allows anonymous GET', async ({ ui, primary }) => {
    const objectUrl = `${primary.url}/${bucket}/public/readme.txt`
    expect((await fetch(objectUrl)).status).toBe(403)

    await open(ui, bucket, 'policy')
    await expect(page.getByText('No bucket policy')).toBeVisible()
    await page.getByRole('combobox', { name: 'Template' }).click()
    await page.getByRole('option', { name: 'Public read' }).click()
    await page.getByRole('button', { name: 'Use template' }).click()
    await expect(page.getByRole('textbox', { name: 'Policy document' })).toHaveValue(/"Principal"/)
    await page.getByRole('button', { name: 'Save policy' }).click()
    await expect(page.getByRole('dialog', { name: 'Grant public access?' })).not.toContainText('cannot be undone')
    await confirmDialog(page, 'Save public policy')
    await expect(page.getByRole('main').getByText('Public', { exact: true }).first()).toBeVisible()

    const anon = await fetch(objectUrl)
    expect(anon.status).toBe(200)
    expect(await anon.text()).toBe('anyone may read me')

    await page.getByRole('button', { name: 'Delete policy' }).click()
    await confirmDialog(page)
    await expect(page.getByText('No bucket policy')).toBeVisible()
    expect((await fetch(objectUrl)).status).toBe(403)
  })

  test('CORS rules', async ({ ui, api, primary }) => {
    await open(ui, bucket, 'cors')
    await expect(page.getByText('No CORS rules')).toBeVisible()
    await page.getByRole('button', { name: 'Add rule' }).first().click()
    const rule = page.getByRole('group', { name: /Rule 1/ })
    const origins = rule.getByRole('textbox').first()
    // Replace the default "*" origin (Backspace selects, then deletes the last tag).
    await origins.click()
    await origins.press('Backspace')
    await origins.press('Backspace')
    await origins.fill('https://app.example.com')
    await origins.press('Enter')
    await rule.getByRole('checkbox', { name: 'PUT' }).check()
    await page.getByRole('button', { name: 'Save CORS' }).click()
    await expect.poll(async () => (await api.getText(bucket, '', '?cors')).text).toContain('<AllowedOrigin>https://app.example.com</AllowedOrigin>')
    const cors = (await api.getText(bucket, '', '?cors')).text
    expect(cors).toContain('<AllowedMethod>PUT</AllowedMethod>')
    expect(cors).not.toContain('<AllowedOrigin>*</AllowedOrigin>')

    // The bucket rule answers a browser preflight from that origin.
    const pre = await fetch(`${primary.url}/${bucket}/x`, { method: 'OPTIONS', headers: { 'Origin': 'https://app.example.com', 'Access-Control-Request-Method': 'PUT' } })
    expect(pre.headers.get('access-control-allow-origin')).toBe('https://app.example.com')

    await page.getByRole('button', { name: 'Delete configuration' }).click()
    await confirmDialog(page)
    await expect(page.getByText('No CORS rules')).toBeVisible()
    expect((await api.getText(bucket, '', '?cors')).status).toBe(404)
  })

  test('lifecycle rules', async ({ ui, api }) => {
    await open(ui, bucket, 'lifecycle')
    await expect(page.getByText('No lifecycle rules')).toBeVisible()
    await page.getByRole('button', { name: 'Add rule' }).first().click()
    const modal = page.getByRole('dialog', { name: 'Add lifecycle rule' })
    await modal.getByRole('textbox', { name: 'Rule ID' }).fill('expire-logs')
    await modal.getByRole('textbox', { name: 'Key prefix' }).fill('logs/')
    await modal.getByRole('combobox', { name: 'Expiration' }).click()
    await page.getByRole('option', { name: /days/i }).first().click()
    await modal.getByRole('spinbutton', { name: 'Days after creation' }).fill('30')
    await modal.getByRole('button', { name: 'Add rule' }).click()
    await expect(modal).toBeHidden()
    await expect(page.getByRole('list', { name: 'Lifecycle rules' }).or(page.getByRole('table', { name: 'Lifecycle rules' }))).toContainText('expire-logs')
    await expect.poll(async () => (await api.getText(bucket, '', '?lifecycle')).text).toContain('<ID>expire-logs</ID>')
    const xml = (await api.getText(bucket, '', '?lifecycle')).text
    expect(xml).toContain('<Prefix>logs/</Prefix>')
    expect(xml).toContain('<Days>30</Days>')

    await page.getByRole('button', { name: 'Delete rule expire-logs' }).click()
    await confirmDialog(page)
    await expect.poll(async () => (await api.getText(bucket, '', '?lifecycle')).status).toBe(404)
  })

  test('bucket tags', async ({ ui, api }) => {
    await open(ui, bucket, 'tags')
    await page.getByRole('button', { name: 'Add tag' }).click()
    await page.getByRole('textbox', { name: 'Key 1' }).fill('team')
    await page.getByRole('textbox', { name: 'Value 1' }).fill('storage')
    await page.getByRole('button', { name: 'Save tags' }).click()
    await expect.poll(async () => (await api.getText(bucket, '', '?tagging')).text).toMatch(/<Key>team<\/Key>\s*<Value>storage<\/Value>/)
  })

  test('default encryption', async ({ ui, api }) => {
    await open(ui, bucket, 'encryption')
    await page.getByRole('radio', { name: 'SSE-S3 (AES256)' }).check()
    await page.getByRole('button', { name: 'Save default encryption' }).click()
    await expect.poll(async () => (await api.getText(bucket, '', '?encryption')).text).toContain('<SSEAlgorithm>AES256</SSEAlgorithm>')
    // New objects report the bucket default.
    const put = await api.putObject(bucket, 'enc.txt', 'x')
    expect(put.headers.get('x-amz-server-side-encryption')).toBe('AES256')
  })

  test('object lock default retention', async ({ ui, api }) => {
    await open(ui, locked, 'object-lock')
    await page.getByRole('radio', { name: 'Governance' }).check()
    await page.getByRole('spinbutton').fill('2')
    await page.getByRole('button', { name: 'Save default retention' }).click()
    await expect.poll(async () => (await api.getText(locked, '', '?object-lock')).text).toContain('<Mode>GOVERNANCE</Mode>')
    expect((await api.getText(locked, '', '?object-lock')).text).toContain('<Days>2</Days>')
    // A new object version gets the default retention.
    const put = await api.putObject(locked, 'held.txt', 'locked')
    const head = await api.fetch(`/${locked}/held.txt?versionId=${put.headers.get('x-amz-version-id')}`, { method: 'HEAD' })
    expect(head.headers.get('x-amz-object-lock-mode')).toBe('GOVERNANCE')

    // A bucket without object lock explains that versioning comes first.
    await open(ui, bucket, 'object-lock')
    await expect(page.getByRole('button', { name: 'Enable object lock' })).toBeVisible()
  })

  test('ACL and public access block', async ({ ui, api, primary }) => {
    await open(ui, bucket, 'access')
    await page.getByRole('radio', { name: 'public-read', exact: true }).check()
    await expect(page.getByText('Public bucket', { exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Save ACL' }).click()
    await confirmDialog(page, /^Make|Save/)
    await expect.poll(async () => (await api.getText(bucket, '', '?acl')).text).toContain('http://acs.amazonaws.com/groups/global/AllUsers')
    // A public-read bucket ACL lets anyone list the bucket; reading objects
    // still needs an object ACL or a policy (S3 semantics).
    const list = await fetch(`${primary.url}/${bucket}?list-type=2`)
    expect(list.status).toBe(200)
    expect(await list.text()).toContain('<Key>public/readme.txt</Key>')
    expect((await fetch(`${primary.url}/${bucket}/public/readme.txt`)).status).toBe(403)

    await page.getByRole('button', { name: 'Block all' }).click()
    await page.getByRole('button', { name: 'Save', exact: true }).click()
    await expect(page.getByText('All public access blocked')).toBeVisible()
    await expect.poll(async () => (await api.getText(bucket, '', '?publicAccessBlock')).text).toContain('<IgnorePublicAcls>true</IgnorePublicAcls>')
    // IgnorePublicAcls / RestrictPublicBuckets turn anonymous access off again.
    expect((await fetch(`${primary.url}/${bucket}?list-type=2`)).status).toBe(403)
  })

  test('danger zone: delete refuses a non-empty bucket, force delete removes everything', async ({ ui, api }) => {
    await open(ui, bucket, 'danger')
    await page.getByRole('button', { name: 'Delete bucket' }).click()
    await confirmDialog(page, 'Delete bucket')
    await expect(page.getByText(/not empty/i).first()).toBeVisible()
    expect((await api.fetch(`/${bucket}`, { method: 'HEAD' })).status).toBe(200)

    await page.getByRole('button', { name: 'Force delete' }).click()
    await confirmDialog(page, 'Delete everything')
    await expect(page).toHaveURL(/\/buckets$/)
    expect((await api.fetch(`/${bucket}`, { method: 'HEAD' })).status).toBe(404)
  })
})
