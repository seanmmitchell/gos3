import { createHash, createHmac } from 'node:crypto'
import { describe, expect, it, vi } from 'vitest'
import { AdminClient, AdminError, fetchInfo, normalizeDecision, parseAdminError, probeServer } from '~/utils/admin/client'
import type { Info } from '~/types/admin'

const INFO: Info = {
  service: 'gos3',
  id: 'b3f1c2d4e5f60718',
  name: 'nas-gos3',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://10.0.0.5:9000', admin: 'http://10.0.0.5:9000/_gos3/admin/v1' },
  features: ['admin'],
  startedAt: '2026-10-09T12:00:00Z'
}

interface Captured { method: string, url: string, headers: Record<string, string>, body: string, bytes: Uint8Array }

const EMPTY_SHA256 = 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'
const sha256 = (data: string | Uint8Array) => createHash('sha256').update(data).digest('hex')

/**
 * Independent SigV4 check (secret 'SK'): recomputes the signature of a
 * captured request, using its X-Amz-Content-Sha256 as the payload hash.
 * Only the simple paths/queries these tests use are canonicalised.
 */
function verifySigV4(c: Captured): boolean {
  const m = /^AWS4-HMAC-SHA256 Credential=([^/]+)\/(\d{8})\/([^/]+)\/s3\/aws4_request, SignedHeaders=([^,]+), Signature=([0-9a-f]{64})$/.exec(c.headers.authorization ?? '')
  if (!m) return false
  const [, , date, region, signed, signature] = m as unknown as [string, string, string, string, string, string]
  const url = new URL(c.url)
  const names = signed.split(';')
  const query = [...url.searchParams].map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`).sort().join('&')
  const canonical = [
    c.method,
    url.pathname,
    query,
    names.map(n => `${n}:${n === 'host' ? url.host : c.headers[n]}`).join('\n') + '\n',
    signed,
    c.headers['x-amz-content-sha256']
  ].join('\n')
  const datetime = c.headers['x-amz-date']!
  const scope = `${date}/${region}/s3/aws4_request`
  const toSign = ['AWS4-HMAC-SHA256', datetime, scope, sha256(canonical)].join('\n')
  const hmac = (key: string | Buffer, v: string) => createHmac('sha256', key).update(v).digest()
  const key = hmac(hmac(hmac(hmac('AWS4SK', date), region), 's3'), 'aws4_request')
  return createHmac('sha256', key).update(toSign).digest('hex') === signature
}

function makeAdmin(responder: (c: Captured) => Response | Promise<Response>) {
  const calls: Captured[] = []
  const fetch = vi.fn(async (req: Request) => {
    const headers: Record<string, string> = {}
    req.headers.forEach((v, k) => (headers[k] = v))
    const bytes = ['GET', 'HEAD'].includes(req.method) ? new Uint8Array() : new Uint8Array(await req.arrayBuffer())
    const c = { method: req.method, url: req.url, headers, body: new TextDecoder().decode(bytes), bytes }
    calls.push(c)
    return responder(c)
  })
  const admin = new AdminClient({ endpoint: 'http://localhost:9000/', region: 'eu-west-1', accessKeyId: 'AK', secretAccessKey: 'SK' }, { fetch, datetime: '20261009T120000Z' })
  return { admin, calls }
}

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })

describe('AdminClient', () => {
  it('signs requests to /_gos3/admin/v1 with SigV4 (service s3, connection region)', async () => {
    const whoami = { user: 'alice', accessKey: 'AK', isRoot: false, policies: ['readwrite'], capabilities: { admin: false }, server: INFO }
    const { admin, calls } = makeAdmin(() => json(whoami))
    const r = await admin.whoami()
    expect(r.user).toBe('alice')
    const c = calls[0]!
    expect(c.method).toBe('GET')
    expect(c.url).toBe('http://localhost:9000/_gos3/admin/v1/whoami')
    expect(c.headers.authorization).toMatch(/^AWS4-HMAC-SHA256 Credential=AK\/20261009\/eu-west-1\/s3\/aws4_request, SignedHeaders=accept;host;x-amz-content-sha256;x-amz-date, Signature=[0-9a-f]{64}$/)
    // The (empty) payload is signed too, never UNSIGNED-PAYLOAD.
    expect(c.headers['x-amz-content-sha256']).toBe(EMPTY_SHA256)
    expect(verifySigV4(c)).toBe(true)
  })

  it('signs the SHA-256 of the exact body bytes sent', async () => {
    const { admin, calls } = makeAdmin(c => (c.method === 'DELETE' || c.method === 'PATCH' ? new Response(null, { status: 204 }) : json({ name: 'p', builtin: false, document: {} })))
    await admin.putPolicy('p', { document: { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: ['s3:GetObject'], Resource: ['arn:aws:s3:::b/ünï ✓/*'] }] } })
    await admin.updateUser('bob', { description: 'naïve – “quoted” 🙂' })
    await admin.deleteUser('bob')
    await admin.probe('http://10.0.0.7:9000')
    for (const c of calls) {
      const hash = c.headers['x-amz-content-sha256']!
      expect(hash).toMatch(/^[0-9a-f]{64}$/)
      // the header is the hash of the bytes that went over the wire …
      expect(hash).toBe(sha256(c.bytes))
      // … it is covered by the signature …
      expect(c.headers.authorization).toMatch(/SignedHeaders=[^,]*\bx-amz-content-sha256\b/)
      expect(verifySigV4(c)).toBe(true)
      // … and another body would not match it.
      expect(verifySigV4({ ...c, headers: { ...c.headers, 'x-amz-content-sha256': sha256(c.body + ' ') } })).toBe(false)
    }
    // the bytes sent are exactly the UTF-8 JSON that was hashed
    expect(calls[0]!.bytes).toEqual(new TextEncoder().encode(JSON.stringify({ document: { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: ['s3:GetObject'], Resource: ['arn:aws:s3:::b/ünï ✓/*'] }] } })))
    expect(JSON.parse(calls[1]!.body)).toEqual({ description: 'naïve – “quoted” 🙂' })
    expect(calls[2]!.headers['x-amz-content-sha256']).toBe(EMPTY_SHA256)
    expect(calls[3]!).toMatchObject({ method: 'POST', body: '{"url":"http://10.0.0.7:9000"}' })
  })

  it('sends JSON bodies and encodes path params', async () => {
    const { admin, calls } = makeAdmin(c => (c.method === 'DELETE' || c.method === 'PATCH' ? new Response(null, { status: 204 }) : json({ user: { name: 'bob' }, key: { accessKey: 'K', secretKey: 'S' } }, 201)))
    const r = await admin.createUser({ name: 'bob', policies: ['readonly'], disabled: false, createKey: true })
    expect(r.key?.secretKey).toBe('S')
    expect(JSON.parse(calls[0]!.body)).toEqual({ name: 'bob', policies: ['readonly'], disabled: false, createKey: true })
    expect(calls[0]!.headers['content-type']).toBe('application/json')
    await admin.deleteUser('a b/c')
    expect(calls[1]!.url).toBe('http://localhost:9000/_gos3/admin/v1/users/a%20b%2Fc')
    await expect(admin.updateAccessKey('KEY1', 'Inactive')).resolves.toBeUndefined()
    expect(calls[2]!).toMatchObject({ method: 'PATCH', url: 'http://localhost:9000/_gos3/admin/v1/keys/KEY1', body: '{"status":"Inactive"}' })
  })

  it('unwraps list envelopes and tolerates null lists', async () => {
    const { admin, calls } = makeAdmin(c => {
      if (c.url.endsWith('/users')) return json({ users: null })
      if (c.url.includes('/buckets')) return json({ buckets: [{ name: 'b' }] })
      return json({ policies: [{ name: 'admin', builtin: true }] })
    })
    expect(await admin.listUsers()).toEqual([])
    expect((await admin.listBuckets({ usage: true }))[0]!.name).toBe('b')
    expect(calls[1]!.url).toBe('http://localhost:9000/_gos3/admin/v1/buckets?usage=true')
    expect((await admin.listPolicies())[0]!.builtin).toBe(true)
  })

  it('deletes buckets with force', async () => {
    const { admin, calls } = makeAdmin(() => new Response(null, { status: 204 }))
    await admin.deleteBucket('photos', { force: true })
    expect(calls[0]!).toMatchObject({ method: 'DELETE', url: 'http://localhost:9000/_gos3/admin/v1/buckets/photos?force=true' })
  })

  it('throws AdminError from the error envelope', async () => {
    const { admin } = makeAdmin(() => json({ error: { code: 'Conflict', message: 'bucket not empty' } }, 409))
    const err = await admin.deleteBucket('b').catch(e => e)
    expect(err).toBeInstanceOf(AdminError)
    expect(err).toMatchObject({ code: 'Conflict', message: 'bucket not empty', status: 409 })
  })

  it('maps 401 to Unauthenticated', async () => {
    const { admin } = makeAdmin(() => json({ error: { code: 'Unauthenticated', message: 'SignatureDoesNotMatch' } }, 401))
    const err = (await admin.whoami().catch((e: unknown) => e)) as AdminError
    expect(err.isUnauthenticated).toBe(true)
    expect(err.message).toBe('SignatureDoesNotMatch')
  })

  it('normalises iam.Decision casing from simulate', async () => {
    const { admin } = makeAdmin(() => json({ Allowed: true, ExplicitDeny: false, Reason: 'allowed by policy readwrite' }))
    expect(await admin.simulate({ user: 'alice', action: 's3:GetObject', bucket: 'b', key: 'k' })).toEqual({ allowed: true, explicitDeny: false, reason: 'allowed by policy readwrite' })
    expect(normalizeDecision({ allowed: false, explicitDeny: true, reason: 'deny' })).toEqual({ allowed: false, explicitDeny: true, reason: 'deny' })
    expect(normalizeDecision(null)).toEqual({ allowed: false, explicitDeny: false, reason: '' })
  })

  it('reports network failures and timeouts', async () => {
    const failing = new AdminClient({ endpoint: 'http://x:1', accessKeyId: 'a', secretAccessKey: 'b' }, { fetch: () => Promise.reject(new TypeError('Failed to fetch')) })
    await expect(failing.whoami()).rejects.toMatchObject({ code: 'NetworkError', status: 0 })
    const hanging = new AdminClient({ endpoint: 'http://x:1', accessKeyId: 'a', secretAccessKey: 'b' }, {
      timeoutMs: 20,
      fetch: req => new Promise((_, reject) => req.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError'))))
    })
    await expect(hanging.whoami()).rejects.toMatchObject({ code: 'Timeout' })
  })

  it('parses non-JSON error bodies', () => {
    expect(parseAdminError(404, '<html>nope</html>')).toMatchObject({ code: 'NotFound', status: 404 })
    expect(parseAdminError(403, '<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>')).toMatchObject({ code: 'AccessDenied', message: 'Access Denied' })
    expect(parseAdminError(500, 'plain failure')).toMatchObject({ code: 'InternalError', message: 'plain failure' })
    expect(parseAdminError(502, '{"error":{"code":"ProbeFailed","message":"dial tcp: refused"}}')).toMatchObject({ code: 'ProbeFailed' })
  })
})

describe('fetchInfo / probeServer', () => {
  it('fetches and validates /_gos3/info without signing', async () => {
    const fetch = vi.fn(async (req: Request) => {
      expect(req.url).toBe('http://10.0.0.5:9000/_gos3/info')
      expect(req.headers.get('authorization')).toBeNull()
      return json(INFO)
    })
    expect(await fetchInfo('10.0.0.5:9000/', { fetch })).toEqual(INFO)
  })

  it('rejects non-gos3 servers', async () => {
    await expect(fetchInfo('http://s3.example', { fetch: async () => json({ hello: 'world' }) })).rejects.toMatchObject({ code: 'NotGos3' })
    await expect(fetchInfo('http://s3.example', { fetch: async () => new Response('<Error/>', { status: 404 }) })).rejects.toMatchObject({ code: 'NotGos3' })
    await expect(fetchInfo('http://s3.example', { fetch: async () => new Response('<html>', { status: 200 }) })).rejects.toMatchObject({ code: 'NotGos3' })
  })

  it('probeServer never throws', async () => {
    const ok = await probeServer('http://a', { fetch: async () => json(INFO) })
    expect(ok.ok).toBe(true)
    expect(ok.info?.id).toBe(INFO.id)
    expect(ok.latencyMs).toBeGreaterThanOrEqual(0)
    const bad = await probeServer('http://a', { fetch: () => Promise.reject(new TypeError('x')) })
    expect(bad.ok).toBe(false)
    expect(bad.error?.code).toBe('NetworkError')
  })
})
