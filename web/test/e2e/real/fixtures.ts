// Fixtures and helpers for the real-server specs: the running gos3
// instances (see global.setup.ts), sign-in through the embedded UI, and a
// signed Node-side client to arrange/verify state behind the UI's back.
import { expect, test as base, type Page } from '@playwright/test'
import { AwsClient } from 'aws4fetch'
import { readState, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, type RealServer } from './env'

export { expect }
export { ROOT_ACCESS_KEY, ROOT_SECRET_KEY }

/** ID of the embedded UI's connection for the hosting server. */
export const THIS_SERVER = 'this-server'

/** Signed client for one server (S3 and admin API) used outside the browser. */
export class ServerApi {
  readonly aws: AwsClient
  constructor(readonly server: RealServer, accessKeyId = ROOT_ACCESS_KEY, secretAccessKey = ROOT_SECRET_KEY) {
    this.aws = new AwsClient({ accessKeyId, secretAccessKey, service: 's3', region: 'us-east-1' })
  }

  /** Signed request to `<server><path>` (path starts with /). */
  fetch(path: string, init: RequestInit = {}): Promise<Response> {
    return this.aws.fetch(`${this.server.url}${path}`, init)
  }

  async admin<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
    const r = await this.fetch(`/_gos3/admin/v1${path}`, {
      method,
      headers: body === undefined ? {} : { 'content-type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body)
    })
    const text = await r.text()
    if (!r.ok) throw new Error(`admin ${method} ${path}: ${r.status} ${text}`)
    return (text ? JSON.parse(text) : undefined) as T
  }

  async putObject(bucket: string, key: string, body: string | Uint8Array, headers: Record<string, string> = {}): Promise<Response> {
    const r = await this.fetch(`/${bucket}/${encodeKey(key)}`, { method: 'PUT', body, headers })
    if (!r.ok) throw new Error(`PUT ${bucket}/${key}: ${r.status} ${await r.text()}`)
    return r
  }

  async getText(bucket: string, key: string, query = ''): Promise<{ status: number, text: string, headers: Headers }> {
    const r = await this.fetch(`/${bucket}/${encodeKey(key)}${query}`)
    return { status: r.status, text: await r.text(), headers: r.headers }
  }

  async createBucket(bucket: string, headers: Record<string, string> = {}): Promise<void> {
    const r = await this.fetch(`/${bucket}`, { method: 'PUT', headers })
    if (!r.ok) throw new Error(`create bucket ${bucket}: ${r.status} ${await r.text()}`)
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
/** Unique, valid bucket name (3..63 lowercase chars) with a readable prefix. */
export function uniqueName(prefix: string): string {
  counter++
  const rnd = Math.random().toString(36).slice(2, 7)
  return `${prefix}-${Date.now().toString(36)}-${counter}${rnd}`.toLowerCase().slice(0, 63)
}

interface WorkerFixtures {
  primary: RealServer
  peer: RealServer
  /** Root-signed client for the primary. */
  api: ServerApi
  /** Absolute URL of an embedded-UI path on the primary, e.g. ui('servers'). */
  ui: (path?: string) => string
  /** Signs in to the hosting server (primary) through the connect page. */
  signIn: (page: Page, opts?: { accessKey?: string, secretKey?: string, redirect?: string }) => Promise<void>
}

// Worker-scoped so that serial specs can use them in beforeAll/afterAll.
export const test = base.extend<object, WorkerFixtures>({
  // eslint-disable-next-line no-empty-pattern
  primary: [async ({}, use) => use(readState().primary), { scope: 'worker' }],
  // eslint-disable-next-line no-empty-pattern
  peer: [async ({}, use) => use(readState().peer), { scope: 'worker' }],
  api: [async ({ primary }, use) => use(new ServerApi(primary)), { scope: 'worker' }],
  ui: [async ({ primary }, use) => use((path = '') => `${primary.url}/_gos3/ui/${path.replace(/^\//, '')}`), { scope: 'worker' }],
  signIn: [async ({ ui }, use) => {
    await use(async (page, opts = {}) => {
      const redirect = opts.redirect ?? `/servers/${THIS_SERVER}`
      await page.goto(ui(`connect?server=${THIS_SERVER}&redirect=${encodeURIComponent(redirect)}`))
      await fillSignIn(page, opts.accessKey ?? ROOT_ACCESS_KEY, opts.secretKey ?? ROOT_SECRET_KEY)
      await expect(page).not.toHaveURL(/\/connect/)
    })
  }, { scope: 'worker' }]
})

/** Fills and submits the sign-in form currently shown. */
export async function fillSignIn(page: Page, accessKey: string, secretKey: string): Promise<void> {
  await page.getByLabel('Access key ID').fill(accessKey)
  await page.getByLabel('Secret access key').fill(secretKey)
  await page.getByRole('button', { name: /^sign in$/i }).click()
}

/** Waits for a toast whose title matches and returns it. */
export function toast(page: Page, title: string | RegExp) {
  return page.locator('[data-slot="root"], li[role="status"], [role="status"], [role="alert"]').filter({ hasText: title }).first()
}

/** Deterministic pseudo-random bytes (fast) of the given size. */
export function bytes(size: number, seed = 1): Uint8Array {
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
