// Fixtures for the standalone specs: the running UI servers and gos3
// instances (see global.setup.ts), a root-signed Node-side client per gos3 to
// arrange and verify state, and helpers to add a server through the UI.
import { expect, test as base, type Page } from '@playwright/test'
import { createHash } from 'node:crypto'
import { AwsClient } from 'aws4fetch'
import { readState, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, type Gos3Instance, type StandaloneState, type UiInstance } from './env'

export { expect }
export { ROOT_ACCESS_KEY, ROOT_SECRET_KEY }

/** Signed client (S3 + admin API) for one gos3 instance, used outside the browser. */
export class Gos3Api {
  readonly aws: AwsClient
  constructor(readonly server: Gos3Instance) {
    this.aws = new AwsClient({ accessKeyId: ROOT_ACCESS_KEY, secretAccessKey: ROOT_SECRET_KEY, service: 's3', region: 'us-east-1' })
  }

  /**
   * Signed request. String and byte bodies are signed by their SHA-256 (the
   * admin API refuses unsigned bodies); other requests use UNSIGNED-PAYLOAD.
   */
  fetch(path: string, init: RequestInit = {}): Promise<Response> {
    const headers = new Headers(init.headers)
    const body = init.body
    if ((typeof body === 'string' || body instanceof Uint8Array) && !headers.has('x-amz-content-sha256')) {
      headers.set('x-amz-content-sha256', createHash('sha256').update(body).digest('hex'))
    }
    return this.aws.fetch(`${this.server.url}${path}`, { ...init, headers })
  }

  async createBucket(bucket: string): Promise<void> {
    const r = await this.fetch(`/${bucket}`, { method: 'PUT' })
    if (!r.ok) throw new Error(`create bucket ${bucket}: ${r.status} ${await r.text()}`)
  }

  async putObject(bucket: string, key: string, body: string, headers: Record<string, string> = {}): Promise<Response> {
    const r = await this.fetch(`/${bucket}/${encodeKey(key)}`, { method: 'PUT', body, headers })
    if (!r.ok) throw new Error(`PUT ${bucket}/${key}: ${r.status} ${await r.text()}`)
    return r
  }

  async head(bucket: string, key: string): Promise<Response> {
    return this.fetch(`/${bucket}/${encodeKey(key)}`, { method: 'HEAD' })
  }

  async getText(bucket: string, key: string): Promise<string> {
    const r = await this.fetch(`/${bucket}/${encodeKey(key)}`)
    if (!r.ok) throw new Error(`GET ${bucket}/${key}: ${r.status}`)
    return r.text()
  }

  /** Force-deletes a bucket (all versions); ignores a missing bucket. */
  async forceDeleteBucket(bucket: string): Promise<void> {
    const r = await this.fetch(`/_gos3/admin/v1/buckets/${bucket}?force=true`, { method: 'DELETE' })
    if (!r.ok && r.status !== 404) throw new Error(`force delete ${bucket}: ${r.status} ${await r.text()}`)
  }
}

export function encodeKey(key: string): string {
  return key.split('/').map(encodeURIComponent).join('/')
}

let counter = 0
/** Unique, valid bucket name with a readable prefix. */
export function uniqueName(prefix: string): string {
  counter++
  return `${prefix}-${Date.now().toString(36)}-${counter}${Math.random().toString(36).slice(2, 6)}`.toLowerCase().slice(0, 63)
}

interface WorkerFixtures {
  env: StandaloneState
  alpha: Gos3Instance
  beta: Gos3Instance
  alphaApi: Gos3Api
  /** The plain standalone UI (no presets). */
  standalone: UiInstance
  /** Absolute URL of a path on the plain standalone UI, e.g. ui('servers'). */
  ui: (path?: string) => string
}

export const test = base.extend<{ page: Page }, WorkerFixtures>({
  // eslint-disable-next-line no-empty-pattern
  env: [async ({}, use) => use(readState()), { scope: 'worker' }],
  alpha: [async ({ env }, use) => use(env.alpha), { scope: 'worker' }],
  beta: [async ({ env }, use) => use(env.beta), { scope: 'worker' }],
  alphaApi: [async ({ alpha }, use) => use(new Gos3Api(alpha)), { scope: 'worker' }],
  standalone: [async ({ env }, use) => use(env.ui), { scope: 'worker' }],
  ui: [async ({ standalone }, use) => use((path = '') => `${standalone.url}/${path.replace(/^\//, '')}`), { scope: 'worker' }],
  page: async ({ page }, use) => {
    watchPage(page)
    await use(page)
  }
})

/**
 * Reports uncaught exceptions and console errors on stderr (they do not fail
 * the test; failed requests a test provokes are logged by Chromium too).
 */
export function watchPage(page: Page): void {
  page.on('pageerror', e => console.error(`[pageerror] ${page.url()}: ${e.message}`))
  page.on('console', (m) => {
    if (m.type() === 'error' && !m.text().startsWith('Failed to load resource')) console.error(`[console.error] ${page.url()}: ${m.text()}`)
  })
}

/** The open "Add server" / "Edit …" modal. */
export function serverDialog(page: Page) {
  return page.getByRole('dialog').filter({ has: page.getByRole('button', { name: 'Test connection' }) })
}

/**
 * Adds a gos3 server through the servers page's "Add server" modal with root
 * credentials and waits until the modal closed. Returns the connection id.
 */
export async function addServerViaUi(page: Page, ui: (p?: string) => string, server: Gos3Instance, name: string): Promise<string> {
  await page.goto(ui('servers'))
  await page.getByRole('button', { name: 'Add server' }).first().click()
  const dialog = serverDialog(page)
  await expect(dialog).toBeVisible()
  await dialog.getByLabel('Endpoint URL').fill(server.url)
  await expect(dialog.getByText(`“${server.name}”`)).toBeVisible()
  await dialog.getByLabel('Display name').fill(name)
  await dialog.getByLabel('Access key ID').fill(ROOT_ACCESS_KEY)
  await dialog.getByLabel('Secret access key').fill(ROOT_SECRET_KEY)
  await dialog.getByRole('button', { name: 'Add server' }).click()
  await expect(dialog).toBeHidden()
  return connectionIdByName(page, name)
}

/** ID of a connection in the UI's persisted store (localStorage). */
export async function connectionIdByName(page: Page, name: string): Promise<string> {
  const id = await page.evaluate((n) => {
    const raw = localStorage.getItem('gos3.connections.v1')
    const state = raw ? JSON.parse(raw) as { connections: { id: string, name: string }[] } : { connections: [] }
    return state.connections.find(c => c.name === n)?.id ?? ''
  }, name)
  expect(id, `connection "${name}" in localStorage`).not.toBe('')
  return id
}
