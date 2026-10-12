// Object browser E2E: a generic S3 connection pointing at an in-memory S3
// mock implemented with Playwright request interception (no real server).
import { expect, test } from '@playwright/test'
import type { Page, Route } from '@playwright/test'

const ENDPOINT = 'http://s3.mock.test'
const CONN_ID = 'mock'

interface Obj {
  body: string
  contentType: string
  lastModified: string
}

const CORS = {
  'access-control-allow-origin': '*',
  'access-control-allow-methods': 'GET,PUT,POST,DELETE,HEAD',
  'access-control-allow-headers': '*',
  'access-control-expose-headers': 'ETag, x-amz-version-id, x-amz-delete-marker, Content-Length, Content-Type, Last-Modified'
}

const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')

/** Minimal path-style S3: ListBuckets, ListObjectsV2, Head/Get/Put/Delete object, DeleteObjects, versioning, tagging. */
async function mockS3(page: Page) {
  const store = new Map<string, Map<string, Obj>>([
    ['photos', new Map<string, Obj>([
      ['readme.md', { body: '# Hello\n\nworld', contentType: 'text/markdown', lastModified: '2026-01-02T00:00:00.000Z' }],
      ['docs/', { body: '', contentType: 'application/x-directory', lastModified: '2026-01-01T00:00:00.000Z' }],
      ['docs/a.txt', { body: 'aaa', contentType: 'text/plain', lastModified: '2026-01-03T00:00:00.000Z' }],
      ['docs/b.txt', { body: 'bbbb', contentType: 'text/plain', lastModified: '2026-01-04T00:00:00.000Z' }]
    ])],
    ['empty', new Map()]
  ])
  const log: string[] = []

  await page.route(`${ENDPOINT}/**`, async (route: Route) => {
    const req = route.request()
    const url = new URL(req.url())
    const method = req.method()
    const fulfill = (status: number, body = '', headers: Record<string, string> = {}) =>
      route.fulfill({ status, body, headers: { ...CORS, 'content-type': 'application/xml', ...headers } })
    if (method === 'OPTIONS') return fulfill(204)
    const parts = url.pathname.split('/').slice(1)
    const bucket = decodeURIComponent(parts[0] ?? '')
    const key = parts.slice(1).map(decodeURIComponent).join('/')
    log.push(`${method} ${bucket}/${key}${url.search}`)

    if (bucket === '_gos3') return fulfill(404, 'not found', { 'content-type': 'text/plain' })
    if (!bucket) {
      const xml = [...store.keys()].map(b => `<Bucket><Name>${b}</Name><CreationDate>2026-01-01T00:00:00.000Z</CreationDate></Bucket>`).join('')
      return fulfill(200, `<ListAllMyBucketsResult><Owner><ID>o</ID></Owner><Buckets>${xml}</Buckets></ListAllMyBucketsResult>`)
    }
    const objs = store.get(bucket)
    if (!objs) return fulfill(404, `<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message></Error>`)

    if (!key) {
      if (url.searchParams.has('versioning')) return fulfill(200, '<VersioningConfiguration/>')
      if (method === 'POST' && url.searchParams.has('delete')) {
        const body = req.postData() ?? ''
        const keys = [...body.matchAll(/<Key>([^<]*)<\/Key>/g)].map(m => m[1]!.replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>'))
        for (const k of keys) objs.delete(k)
        return fulfill(200, `<DeleteResult>${keys.map(k => `<Deleted><Key>${esc(k)}</Key></Deleted>`).join('')}</DeleteResult>`)
      }
      if (url.searchParams.has('versions')) {
        // One version per key, plus an older version of readme.md.
        const prefix = url.searchParams.get('prefix') ?? ''
        const rows: string[] = []
        const prefixes = new Set<string>()
        for (const [k, o] of [...objs.entries()].sort(([a], [b]) => (a < b ? -1 : 1))) {
          if (!k.startsWith(prefix)) continue
          const rest = k.slice(prefix.length)
          const i = rest.indexOf('/')
          if (i >= 0) {
            prefixes.add(prefix + rest.slice(0, i + 1))
            continue
          }
          rows.push(`<Version><Key>${esc(k)}</Key><VersionId>v2-${esc(k)}</VersionId><IsLatest>true</IsLatest><LastModified>${o.lastModified}</LastModified><ETag>"e"</ETag><Size>${o.body.length}</Size></Version>`)
          if (k === 'readme.md') rows.push(`<Version><Key>${k}</Key><VersionId>v1-old</VersionId><IsLatest>false</IsLatest><LastModified>2025-12-01T00:00:00.000Z</LastModified><ETag>"e0"</ETag><Size>3</Size></Version>`)
        }
        const cp = [...prefixes].map(p => `<CommonPrefixes><Prefix>${esc(p)}</Prefix></CommonPrefixes>`).join('')
        return fulfill(200, `<ListVersionsResult><Name>${bucket}</Name><Prefix>${esc(prefix)}</Prefix><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>${rows.join('')}${cp}</ListVersionsResult>`)
      }
      if (url.searchParams.get('list-type') === '2') {
        const prefix = url.searchParams.get('prefix') ?? ''
        const delim = url.searchParams.get('delimiter') ?? ''
        const contents: string[] = []
        const prefixes = new Set<string>()
        for (const [k, o] of [...objs.entries()].sort(([a], [b]) => (a < b ? -1 : 1))) {
          if (!k.startsWith(prefix)) continue
          const rest = k.slice(prefix.length)
          const i = delim ? rest.indexOf(delim) : -1
          if (i >= 0) prefixes.add(prefix + rest.slice(0, i + 1))
          else contents.push(`<Contents><Key>${esc(k)}</Key><LastModified>${o.lastModified}</LastModified><ETag>"etag-${esc(k)}"</ETag><Size>${o.body.length}</Size><StorageClass>STANDARD</StorageClass></Contents>`)
        }
        const cp = [...prefixes].map(p => `<CommonPrefixes><Prefix>${esc(p)}</Prefix></CommonPrefixes>`).join('')
        return fulfill(200, `<ListBucketResult><Name>${bucket}</Name><Prefix>${esc(prefix)}</Prefix><Delimiter>${delim}</Delimiter><MaxKeys>1000</MaxKeys><KeyCount>${contents.length + prefixes.size}</KeyCount><IsTruncated>false</IsTruncated>${contents.join('')}${cp}</ListBucketResult>`)
      }
      return fulfill(200, '')
    }

    if (url.searchParams.has('tagging')) return fulfill(200, '<Tagging><TagSet></TagSet></Tagging>')
    const o = objs.get(key)
    if (method === 'PUT') {
      const body = req.postData() ?? ''
      objs.set(key, { body, contentType: req.headers()['content-type'] ?? 'application/octet-stream', lastModified: new Date().toISOString() })
      return fulfill(200, '', { etag: `"etag-${key}"` })
    }
    if (method === 'DELETE') {
      objs.delete(key)
      return fulfill(204)
    }
    if (!o) return fulfill(404, method === 'HEAD' ? '' : '<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>')
    const headers = { 'content-type': o.contentType, 'etag': `"etag-${key}"`, 'last-modified': new Date(o.lastModified).toUTCString(), 'content-length': String(o.body.length) }
    if (method === 'HEAD') return fulfill(200, '', headers)
    return fulfill(200, o.body, headers)
  })
  return { store, log }
}

async function seedConnection(page: Page) {
  await page.addInitScript(([id, endpoint]) => {
    localStorage.setItem('gos3.connections.v1', JSON.stringify({
      version: 1,
      activeId: id,
      dismissedPresets: [],
      connections: [{ id, name: 'Mock S3', endpoint, region: 'us-east-1', accessKeyId: 'AKMOCK', secretAccessKey: 'SECRETMOCK', kind: 's3', forcePathStyle: true, remember: true, source: 'user' }]
    }))
  }, [CONN_ID, ENDPOINT] as const)
}

test.describe('object browser', () => {
  test.beforeEach(async ({ page }) => {
    await seedConnection(page)
  })

  test('lists buckets and browses folders and objects', async ({ page }) => {
    await mockS3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/buckets`)
    await expect(page.getByRole('heading', { name: 'Buckets' })).toBeVisible()
    await page.getByRole('link', { name: 'photos', exact: true }).click()
    await expect(page).toHaveURL(/\/servers\/mock\/buckets\/photos\/objects\/$/)
    await expect(page.getByRole('link', { name: 'docs/' })).toBeVisible()
    await expect(page.getByRole('link', { name: 'readme.md' })).toBeVisible()

    await page.getByRole('link', { name: 'docs/' }).click()
    await expect(page).toHaveURL(/\/objects\/docs\/$/)
    await expect(page.getByRole('link', { name: 'a.txt' })).toBeVisible()
    // The folder placeholder object itself is not listed.
    await expect(page.getByRole('link', { name: /^docs/ })).toHaveCount(0)

    // Object details open in a slideover driven by the URL.
    await page.getByRole('link', { name: 'b.txt' }).click()
    await expect(page).toHaveURL(/\/objects\/docs\/b\.txt$/)
    const dialog = page.getByRole('dialog')
    await expect(dialog.getByText('etag-docs/b.txt')).toBeVisible()
    await expect(dialog.getByText('text/plain')).toBeVisible()
    await dialog.getByRole('tab', { name: 'Preview' }).click()
    await expect(dialog.getByText('bbbb')).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(page).toHaveURL(/\/objects\/docs\/$/)
  })

  test('legacy bucket links redirect into the browser', async ({ page }) => {
    await mockS3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/buckets/photos?prefix=docs%2F`)
    await expect(page).toHaveURL(/\/objects\/docs\/$/)
    await expect(page.getByRole('link', { name: 'a.txt' })).toBeVisible()
  })

  test('creates folders, uploads files and deletes a selection', async ({ page }) => {
    const { store } = await mockS3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/buckets/photos/objects/docs/`)
    await expect(page.getByRole('link', { name: 'a.txt' })).toBeVisible()

    await page.getByRole('button', { name: 'New folder' }).click()
    await page.getByLabel('Folder name').fill('reports')
    await page.getByRole('button', { name: 'Create folder' }).click()
    await expect(page.getByRole('link', { name: 'reports/' })).toBeVisible()
    expect(store.get('photos')!.has('docs/reports/')).toBe(true)

    await page.locator('input[type=file]').first().setInputFiles({ name: 'hello.txt', mimeType: 'text/plain', buffer: Buffer.from('hi there') })
    await expect(page.getByRole('region', { name: 'Uploads' }).getByText('1 upload complete')).toBeVisible()
    await expect(page.getByRole('link', { name: 'hello.txt' })).toBeVisible()
    expect(store.get('photos')!.get('docs/hello.txt')?.body).toBe('hi there')

    await page.getByRole('checkbox', { name: 'Select a.txt' }).check()
    await page.getByRole('checkbox', { name: 'Select b.txt' }).check()
    await expect(page.getByText('2 selected')).toBeVisible()
    await page.getByRole('region', { name: 'Selection actions' }).getByRole('button', { name: 'Delete' }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click()
    await expect(page.getByRole('link', { name: 'a.txt' })).toHaveCount(0)
    await expect(page.getByRole('link', { name: 'b.txt' })).toHaveCount(0)
    expect(store.get('photos')!.has('docs/a.txt')).toBe(false)
    expect(store.get('photos')!.has('docs/hello.txt')).toBe(true)
  })

  test('selects ranges with shift-click and lists versions', async ({ page }) => {
    const { log } = await mockS3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/buckets/photos/objects/`)
    await page.getByRole('checkbox', { name: 'Select docs' }).click()
    await page.getByRole('checkbox', { name: 'Select readme.md' }).click({ modifiers: ['Shift'] })
    await expect(page.getByText('2 selected')).toBeVisible()
    await page.getByRole('checkbox', { name: 'Select all listed items' }).click()
    await expect(page.getByText('2 selected')).toHaveCount(0)

    await page.getByRole('switch', { name: 'Show versions' }).click()
    await expect(page.getByRole('checkbox', { name: 'Select readme.md version v1-old' })).toBeVisible()
    await expect(page.getByText('Latest')).toHaveCount(1)
    expect(log.some(l => l.startsWith('GET photos/?') && l.includes('versions'))).toBe(true)
  })

  test('shows an empty state and a missing-bucket error', async ({ page }) => {
    await mockS3(page)
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/buckets/empty/objects/`)
    await expect(page.getByText('This bucket is empty')).toBeVisible()
    await page.goto(`/_gos3/ui/servers/${CONN_ID}/buckets/nope/objects/`)
    await expect(page.getByText('Bucket nope does not exist')).toBeVisible()
  })
})
