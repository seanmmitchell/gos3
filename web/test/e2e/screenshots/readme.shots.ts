// README screenshot generator (Playwright project "screenshots", run with
// `pnpm screenshots`; not part of any test run).
//
// Starts two gos3 instances with the real embedded UI — "nas-primary" (fs
// backend) and "nas-backup" (its discovery seed) — fills the primary with
// realistic demo data (buckets, folders, varied file types, a versioned
// bucket with lifecycle rules, users), keeps some S3 traffic flowing so the
// metrics chart has data, and captures 1440×900 PNGs into docs/screenshots/.
//
// Env: SHOTS_TRAFFIC_MINUTES (default 5) — how long traffic runs before the
// overview (metrics) screenshot; GOS3_BIN — prebuilt binary with the UI
// embedded; SHOTS_ONLY=dashboard,browser — capture only these.
import { createHash } from 'node:crypto'
import { mkdirSync, mkdtempSync, rmSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { expect, test, type Browser, type BrowserContext, type Page } from '@playwright/test'
import { AwsClient } from 'aws4fetch'
import { stopProcs } from '../standalone/stop'
import { ensureEmbeddedBinary, freePort, SHOTS_DIR, supervise } from './env'

const ACCESS_KEY = 'nasadmin'
const SECRET_KEY = 'nasadmin-demo-secret'
const VIEWPORT = { width: 1440, height: 900 }
const MAX_PNG_BYTES = 400 * 1024
const TRAFFIC_MINUTES = Number(process.env.SHOTS_TRAFFIC_MINUTES ?? 5)
const ONLY = (process.env.SHOTS_ONLY ?? '').split(',').map(s => s.trim()).filter(Boolean)

interface Instance { name: string, url: string, port: number, watchdogPid: number, logFile: string }

class Api {
  readonly aws = new AwsClient({ accessKeyId: ACCESS_KEY, secretAccessKey: SECRET_KEY, service: 's3', region: 'us-east-1' })
  constructor(readonly base: string) {}

  async req(method: string, path: string, body?: string | Uint8Array<ArrayBuffer>, headers: Record<string, string> = {}): Promise<Response> {
    const r = await this.aws.fetch(`${this.base}${path}`, { method, body, headers })
    if (!r.ok) throw new Error(`${method} ${path}: ${r.status} ${await r.text()}`)
    return r
  }

  /** Bucket subresource PUT with the Content-MD5 S3 requires for config documents. */
  config(bucket: string, sub: string, xml: string): Promise<Response> {
    return this.req('PUT', `/${bucket}?${sub}`, xml, { 'content-type': 'application/xml', 'content-md5': createHash('md5').update(xml).digest('base64') })
  }

  put(bucket: string, key: string, body: string | Uint8Array<ArrayBuffer>, contentType: string, extra: Record<string, string> = {}): Promise<Response> {
    return this.req('PUT', `/${bucket}/${key.split('/').map(encodeURIComponent).join('/')}`, body, { 'content-type': contentType, ...extra })
  }

  admin(method: string, path: string, body?: unknown): Promise<Response> {
    return this.req(method, `/_gos3/admin/v1${path}`, body === undefined ? undefined : JSON.stringify(body), body === undefined ? {} : { 'content-type': 'application/json' })
  }
}

/** Deterministic pseudo-random bytes (incompressible "media"). */
function noise(size: number, seed: number): Uint8Array<ArrayBuffer> {
  const b = new Uint8Array(size)
  let x = seed >>> 0 || 1
  for (let i = 0; i < size; i++) {
    x ^= x << 13
    x ^= x >>> 17
    x ^= x << 5
    b[i] = x & 0xff
  }
  return b
}

function pdf(title: string): string {
  // Minimal valid single-page PDF.
  const text = `BT /F1 24 Tf 72 720 Td (${title.replace(/[()\\]/g, '')}) Tj ET`
  return `%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj\n4 0 obj<</Length ${text.length}>>stream\n${text}\nendstream endobj\n5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n`
}

/** Renders "photos" (JPEG landscapes) with a canvas in the browser. */
async function renderPhotos(browser: Browser, n: number): Promise<Uint8Array<ArrayBuffer>[]> {
  const page = await browser.newPage()
  try {
    const out = await page.evaluate(async (count) => {
      const palettes = [
        ['#0f2027', '#2c5364', '#f4d03f'], ['#ff7e5f', '#feb47b', '#2d3436'], ['#1e3c72', '#2a5298', '#f1c40f'],
        ['#134e5e', '#71b280', '#fefefe'], ['#42275a', '#734b6d', '#ffcf71'], ['#2193b0', '#6dd5ed', '#ffffff'],
        ['#cc2b5e', '#753a88', '#fbd786'], ['#56ab2f', '#a8e063', '#fffbd5']
      ]
      const res: string[] = []
      for (let i = 0; i < count; i++) {
        const [a, b, sun] = palettes[i % palettes.length]!
        const c = document.createElement('canvas')
        c.width = 1600
        c.height = 1067
        const g = c.getContext('2d')!
        const sky = g.createLinearGradient(0, 0, 0, c.height)
        sky.addColorStop(0, a!)
        sky.addColorStop(1, b!)
        g.fillStyle = sky
        g.fillRect(0, 0, c.width, c.height)
        g.fillStyle = sun!
        g.globalAlpha = 0.9
        g.beginPath()
        g.arc(300 + i * 140, 280 + (i % 3) * 40, 90, 0, Math.PI * 2)
        g.fill()
        for (let layer = 0; layer < 3; layer++) {
          g.globalAlpha = 0.35 + layer * 0.25
          g.fillStyle = layer === 2 ? '#1b1b2f' : a!
          g.beginPath()
          g.moveTo(0, c.height)
          for (let x = 0; x <= c.width; x += 40) {
            const y = 600 + layer * 120 - Math.abs(Math.sin((x + i * 97 + layer * 211) / (180 - layer * 30))) * (220 - layer * 50)
            g.lineTo(x, y)
          }
          g.lineTo(c.width, c.height)
          g.fill()
        }
        res.push(c.toDataURL('image/jpeg', 0.86).split(',')[1]!)
      }
      return res
    }, n)
    return out.map(b64 => new Uint8Array(Buffer.from(b64, 'base64')))
  } finally {
    await page.close()
  }
}

async function seed(api: Api, browser: Browser): Promise<void> {
  const photos = await renderPhotos(browser, 8)
  for (const b of ['photos', 'documents', 'website', 'backups', 'media']) await api.req('PUT', `/${b}`)

  // photos: folders by year/trip, JPEGs, a README
  const trips = ['2026/lisbon', '2026/alps-hiking', '2025/iceland']
  let i = 0
  for (const trip of trips) {
    for (let k = 0; k < 3; k++) {
      await api.put('photos', `${trip}/IMG_${4810 + i * 7}.jpg`, photos[i % photos.length]!, 'image/jpeg', { 'x-amz-meta-camera': 'Fujifilm X-T5', 'x-amz-meta-photographer': 'alice' })
      i++
    }
  }
  await api.put('photos', 'cover.jpg', photos[1]!, 'image/jpeg', {
    'x-amz-meta-camera': 'Fujifilm X-T5',
    'x-amz-meta-lens': 'XF 16-55mm F2.8',
    'x-amz-meta-location': 'Lisbon, Portugal',
    'x-amz-meta-photographer': 'alice',
    'cache-control': 'max-age=86400'
  })
  await api.put('photos', 'sunset-panorama.jpg', photos[6]!, 'image/jpeg')
  await api.put('photos', 'IMG_5012.jpg', photos[2]!, 'image/jpeg')
  await api.put('photos', 'IMG_5013.jpg', photos[5]!, 'image/jpeg')
  await api.put('photos', 'wallpapers/mountains-4k.jpg', photos[7]!, 'image/jpeg')
  await api.put('photos', 'slideshow-2026.mp4', noise(6_300_000, 77), 'video/mp4')
  await api.put('photos', 'edits.zip', noise(2_100_000, 78), 'application/zip')
  await api.put('photos', 'print-order.pdf', pdf('Print order'), 'application/pdf')
  await api.put('photos', 'album.json', JSON.stringify({ title: 'Family album', albums: trips }, null, 2), 'application/json')
  await api.put('photos', 'README.md', '# Photos\n\nOriginals live under `raw/`; edited exports per trip.\n', 'text/markdown')
  await api.put('photos', 'raw/DSCF0042.raf', noise(3_400_000, 42), 'application/octet-stream')
  await api.put('photos', 'raw/DSCF0043.raf', noise(3_100_000, 43), 'application/octet-stream')
  await api.config('photos', 'tagging', '<Tagging><TagSet><Tag><Key>team</Key><Value>family</Value></Tag></TagSet></Tagging>')

  // documents: PDFs, spreadsheets, notes
  for (const m of ['01', '02', '03', '04', '05', '06', '07', '08', '09']) await api.put('documents', `invoices/2026/INV-2026-${m}.pdf`, pdf(`Invoice 2026-${m}`), 'application/pdf')
  await api.put('documents', 'contracts/office-lease.pdf', pdf('Office lease'), 'application/pdf')
  await api.put('documents', 'contracts/nda-acme.pdf', pdf('NDA ACME'), 'application/pdf')
  await api.put('documents', 'reports/q3-summary.csv', 'month,revenue,costs\nJuly,48200,31100\nAugust,51800,32900\nSeptember,55100,33400\n', 'text/csv')
  await api.put('documents', 'handbook.pdf', pdf('Employee handbook'), 'application/pdf')
  await api.put('documents', 'meeting-notes.md', '# Weekly sync\n\n- storage migration done\n- enable versioning on backups\n', 'text/markdown')

  // website: static site, public-read bucket policy
  await api.put('website', 'index.html', '<!doctype html><title>Acme</title><h1>Hello</h1>', 'text/html', { 'cache-control': 'max-age=300' })
  await api.put('website', 'css/site.css', 'body{font-family:system-ui;margin:2rem}', 'text/css')
  await api.put('website', 'js/app.js', 'console.log("hello")', 'text/javascript')
  await api.put('website', 'assets/logo.svg', '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><circle cx="5" cy="5" r="4" fill="teal"/></svg>', 'image/svg+xml')
  await api.put('website', 'assets/hero.jpg', photos[3]!, 'image/jpeg')
  const policy = JSON.stringify({ Version: '2012-10-17', Statement: [{ Sid: 'PublicRead', Effect: 'Allow', Principal: '*', Action: ['s3:GetObject'], Resource: ['arn:aws:s3:::website/*'] }] })
  await api.req('PUT', '/website?policy', policy, { 'content-type': 'application/json' })

  // backups: versioned, several versions per dump, lifecycle rules
  await api.config('backups', 'versioning', '<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>')
  for (let v = 0; v < 4; v++) await api.put('backups', 'db/postgres-main.sql.gz', noise(1_800_000 + v * 90_000, 100 + v), 'application/gzip')
  for (let v = 0; v < 3; v++) await api.put('backups', 'db/redis.rdb', noise(420_000 + v * 30_000, 200 + v), 'application/octet-stream')
  await api.put('backups', 'etc/nginx.tar.gz', noise(86_000, 7), 'application/gzip')
  await api.put('backups', 'etc/gos3-config.yaml', 'server:\n  address: ":9000"\nstorage:\n  backend: fs\n', 'application/yaml')
  await api.config('backups', 'lifecycle', '<LifecycleConfiguration>'
    + '<Rule><ID>expire-old-versions</ID><Filter><Prefix>db/</Prefix></Filter><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>30</NoncurrentDays></NoncurrentVersionExpiration></Rule>'
    + '<Rule><ID>abort-stale-uploads</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>'
    + '</LifecycleConfiguration>')
  await api.config('backups', 'tagging', '<Tagging><TagSet><Tag><Key>retention</Key><Value>30d</Value></Tag><Tag><Key>owner</Key><Value>ops</Value></Tag></TagSet></Tagging>')

  // media: large files
  await api.put('media', 'videos/product-demo.mp4', noise(14_000_000, 9), 'video/mp4')
  await api.put('media', 'videos/onboarding.mp4', noise(9_500_000, 10), 'video/mp4')
  await api.put('media', 'podcasts/episode-12.mp3', noise(5_200_000, 11), 'audio/mpeg')

  // users
  const users = [
    { name: 'alice', description: 'Photographer — manages the photo library', policies: ['readwrite'], createKey: true },
    { name: 'bob', description: 'Finance, read-only access to documents', policies: ['readonly'], createKey: true },
    { name: 'ci-deploy', description: 'CI pipeline publishing the website', policies: ['writeonly'], createKey: true },
    { name: 'grafana', description: 'Monitoring (metrics and stats)', policies: ['diagnostics'], createKey: true },
    { name: 'intern', description: 'Summer intern (access paused)', policies: ['readonly'], disabled: true }
  ]
  for (const u of users) await api.admin('POST', '/users', { disabled: false, ...u })
}

/** Background S3 traffic with a varying rate (so the chart is not flat). */
function startTraffic(api: Api): () => Promise<void> {
  let stop = false
  const loop = (async () => {
    let n = 0
    while (!stop) {
      const minute = Math.floor(Date.now() / 60_000)
      const burst = 4 + ((minute * 7) % 9)
      const ops: Promise<unknown>[] = []
      for (let k = 0; k < burst; k++) {
        n++
        const r = n % 10
        if (r < 5) ops.push(api.aws.fetch(`${api.base}/photos/cover.jpg`).then(x => x.arrayBuffer()))
        else if (r < 7) ops.push(api.aws.fetch(`${api.base}/documents?list-type=2&prefix=invoices/`).then(x => x.text()))
        else if (r < 9 || n % 4 !== 0) ops.push(api.put('website', `logs/access-${n % 50}.log`, `GET / 200 ${n}\n`.repeat(200 + (n % 7) * 300), 'text/plain'))
        else ops.push(api.aws.fetch(`${api.base}/photos/missing-${n}.jpg`, { method: 'HEAD' }))
      }
      // Some heavier transfers so "Traffic per minute" has volume too.
      if (n % 9 === 0) ops.push(api.aws.fetch(`${api.base}/media/podcasts/episode-12.mp3`).then(x => x.arrayBuffer()))
      if (n % 13 === 0) ops.push(api.put('media', `uploads/clip-${n % 5}.mov`, noise(900_000 + (minute % 4) * 400_000, n), 'video/quicktime'))
      await Promise.allSettled(ops)
      await new Promise(r => setTimeout(r, 1000))
    }
  })()
  return async () => {
    stop = true
    await loop
  }
}

async function startInstances(binary: string, tmp: string): Promise<[Instance, Instance]> {
  const [pPort, bPort] = [await freePort(), await freePort()]
  const pUrl = `http://127.0.0.1:${pPort}`
  const bUrl = `http://127.0.0.1:${bPort}`
  const start = (name: string, port: number, seedUrl: string): Instance => {
    const dataDir = join(tmp, name)
    mkdirSync(dataDir)
    const logFile = join(tmp, `${name}.log`)
    const env: NodeJS.ProcessEnv = {
      ...process.env,
      GOS3_ROOT_ACCESS_KEY: ACCESS_KEY,
      GOS3_ROOT_SECRET_KEY: SECRET_KEY,
      GOS3_DISCOVERY_ENABLED: 'true',
      GOS3_DISCOVERY_ALLOW_SCAN: 'true',
      GOS3_DISCOVERY_MDNS: 'false',
      GOS3_DISCOVERY_BEACON: 'false'
    }
    delete env.GOS3_CONFIG
    const args = ['serve', '--address', `127.0.0.1:${port}`, '--backend', 'fs', '--data', dataDir, '--fsync=false', '--name', name, '--seed', seedUrl, '--cors-origin', seedUrl, '--log-level', 'warn']
    return { name, url: `http://127.0.0.1:${port}`, port, logFile, watchdogPid: supervise(binary, args, logFile, env) }
  }
  const backup = start('nas-backup', bPort, pUrl)
  const primary = start('nas-primary', pPort, bUrl)
  for (const s of [backup, primary]) {
    await expect.poll(async () => (await fetch(`${s.url}/_gos3/health`).catch(() => null))?.status ?? 0, { timeout: 30_000, message: `${s.name} (log ${s.logFile})` }).toBe(200)
  }
  return [primary, backup]
}

/** localStorage state: the hosting server and the backup node, signed in (remembered). */
function connectionsState(primary: Instance, backup: Instance) {
  return {
    version: 1,
    activeId: 'this-server',
    defaultId: 'this-server',
    dismissedPresets: [],
    connections: [
      { id: 'this-server', name: 'nas-primary (this server)', endpoint: primary.url, region: 'us-east-1', kind: 'gos3', forcePathStyle: true, source: 'embedded', accessKeyId: ACCESS_KEY, secretAccessKey: SECRET_KEY, remember: true },
      { id: 'nas-backup', name: 'nas-backup', endpoint: backup.url, region: 'us-east-1', kind: 'gos3', forcePathStyle: true, source: 'user', color: '#8b5cf6', accessKeyId: ACCESS_KEY, secretAccessKey: SECRET_KEY, remember: true }
    ]
  }
}

async function newContext(browser: Browser, state: unknown, colorScheme: 'light' | 'dark'): Promise<BrowserContext> {
  const ctx = await browser.newContext({ viewport: VIEWPORT, deviceScaleFactor: 1, colorScheme, reducedMotion: 'reduce' })
  await ctx.addInitScript((s) => {
    if (!localStorage.getItem('gos3.connections.v1')) localStorage.setItem('gos3.connections.v1', s)
  }, JSON.stringify(state))
  return ctx
}

/** Settles the page (no spinners, no toasts, no focus rings) and writes the PNG. */
async function shoot(page: Page, name: string): Promise<void> {
  if (ONLY.length && !ONLY.includes(name)) return
  await page.waitForLoadState('networkidle').catch(() => {})
  await expect(page.locator('[aria-busy="true"], .animate-spin')).toHaveCount(0, { timeout: 20_000 }).catch(() => {})
  await page.addStyleTag({ content: '[aria-label^="Notifications"], [data-slot="viewport"] { display: none !important } *, *::before, *::after { caret-color: transparent !important; transition: none !important; animation: none !important }' })
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur())
  await page.mouse.move(VIEWPORT.width - 2, VIEWPORT.height - 2)
  await page.waitForTimeout(300)
  const file = join(SHOTS_DIR, `${name}.png`)
  await page.screenshot({ path: file, animations: 'disabled', caret: 'hide', scale: 'css' })
  const size = statSync(file).size
  expect(size, `${name}.png is ${Math.round(size / 1024)} KiB`).toBeLessThan(MAX_PNG_BYTES)
}

test.describe.configure({ mode: 'serial' })

test.describe('README screenshots', () => {
  let tmp = ''
  let primary: Instance
  let backup: Instance
  let api: Api
  let stopTraffic: (() => Promise<void>) | undefined
  let trafficStarted = 0
  let light: BrowserContext
  let dark: BrowserContext
  const ui = (path = '') => `${primary.url}/_gos3/ui/${path}`

  test.beforeAll(async ({ browser }) => {
    test.setTimeout(900_000)
    mkdirSync(SHOTS_DIR, { recursive: true })
    const binary = ensureEmbeddedBinary()
    tmp = mkdtempSync(join(tmpdir(), 'gos3-shots-'))
    ;[primary, backup] = await startInstances(binary, tmp)
    api = new Api(primary.url)
    stopTraffic = startTraffic(api)
    trafficStarted = Date.now()
    await seed(api, browser)
    const backupApi = new Api(backup.url)
    for (const b of ['archive', 'replica-photos', 'offsite-db']) await backupApi.req('PUT', `/${b}`)
    for (let k = 0; k < 6; k++) await backupApi.put('archive', `2025/project-${k + 1}.tar.zst`, noise(1_200_000 + k * 250_000, 300 + k), 'application/zstd')
    await backupApi.put('offsite-db', 'postgres-main.sql.gz', noise(2_400_000, 400), 'application/gzip')
    // The backup node must be a verified peer of the primary (seed probe).
    await expect.poll(async () => {
      const d = await (await api.admin('GET', '/discovery')).json() as { peers: { name: string, healthy: boolean }[] }
      return d.peers.some(p => p.name === 'nas-backup' && p.healthy)
    }, { timeout: 60_000 }).toBe(true)
    const state = connectionsState(primary, backup)
    light = await newContext(browser, state, 'light')
    dark = await newContext(browser, state, 'dark')
  })

  test.afterAll(async () => {
    await stopTraffic?.()
    await light?.close()
    await dark?.close()
    await stopProcs([primary, backup])
    if (tmp && process.env.E2E_KEEP_DATA !== '1') rmSync(tmp, { recursive: true, force: true })
  })

  for (const [scheme, suffix] of [['light', ''], ['dark', '-dark']] as const) {
    test(`dashboard${suffix}`, async () => {
      const page = await (scheme === 'light' ? light : dark).newPage()
      await page.goto(ui(''))
      await expect(page.getByText('nas-backup').first()).toBeVisible()
      await expect(page.getByText(/buckets?/i).first()).toBeVisible()
      await page.waitForTimeout(1500)
      await shoot(page, `dashboard${suffix}`)
      await page.close()
    })

    test(`browser${suffix}`, async () => {
      const page = await (scheme === 'light' ? light : dark).newPage()
      await page.goto(ui('servers/this-server/buckets/photos'))
      await expect(page.getByRole('main').getByRole('link', { name: 'cover.jpg', exact: true })).toBeVisible()
      await shoot(page, `browser${suffix}`)
      await page.close()
    })
  }

  test('object-details', async () => {
    const page = await light.newPage()
    await page.goto(ui('servers/this-server/buckets/photos'))
    await page.getByRole('main').getByRole('link', { name: 'cover.jpg', exact: true }).click()
    const details = page.getByRole('dialog', { name: 'cover.jpg' })
    await expect(details.getByText('Lisbon, Portugal')).toBeVisible()
    await details.getByRole('tab', { name: 'Preview' }).click()
    await expect.poll(() => details.locator('img').first().evaluate(i => (i as HTMLImageElement).naturalWidth).catch(() => 0)).toBeGreaterThan(0)
    await details.getByRole('tab', { name: 'Overview' }).click()
    await shoot(page, 'object-details')
    await page.close()
  })

  test('bucket-settings', async () => {
    const page = await light.newPage()
    await page.goto(ui('servers/this-server/buckets/backups/settings'))
    await expect(page.getByText('expire-old-versions').first().or(page.getByText(/Enabled/).first())).toBeVisible()
    await page.waitForTimeout(1000)
    await shoot(page, 'bucket-settings')
    await page.close()
  })

  test('users', async () => {
    const page = await light.newPage()
    await page.goto(ui('servers/this-server/users'))
    await expect(page.getByText('ci-deploy')).toBeVisible()
    await shoot(page, 'users')
    await page.close()
  })

  test('discovery', async () => {
    const page = await light.newPage()
    await page.goto(ui('servers/this-server/discovery'))
    await expect(page.getByRole('main').getByText('nas-backup').first()).toBeVisible()
    await shoot(page, 'discovery')
    await page.close()
  })

  test('overview', async () => {
    test.setTimeout((TRAFFIC_MINUTES + 3) * 60_000)
    // Let traffic accumulate over several minutes for the per-minute chart.
    const wait = TRAFFIC_MINUTES * 60_000 - (Date.now() - trafficStarted)
    if (wait > 0 && (!ONLY.length || ONLY.includes('overview'))) await new Promise(r => setTimeout(r, wait))
    const page = await light.newPage()
    await page.goto(ui('servers/this-server'))
    await expect(page.getByText('Requests per minute')).toBeVisible()
    await page.waitForTimeout(2000)
    // Show storage, request counters and both charts (scrolls the panel body).
    await page.locator('#storage-heading').evaluate((el) => {
      // Scroll only the panel body (scrollIntoView would also shift the shell).
      let p = el.parentElement
      while (p && !(p.scrollHeight > p.clientHeight && /auto|scroll/.test(getComputedStyle(p).overflowY))) p = p.parentElement
      if (p) p.scrollTop += el.getBoundingClientRect().top - p.getBoundingClientRect().top - 24
    })
    await shoot(page, 'overview')
    await page.close()
  })
})
