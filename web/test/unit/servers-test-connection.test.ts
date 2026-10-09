// Connection testing and error diagnosis (app/components/servers/-test-connection.ts).
import { describe, expect, it } from 'vitest'
import type { Info, WhoAmI } from '~/types/admin'
import { AdminError, type FetchLike } from '~/utils/admin/client'
import { S3Error } from '~/utils/s3/errors'
import { detectKind, diagnose, errorCode, testConnection, verifyAndDiagnose, verifyCredentials } from '~/components/servers/-test-connection'

const INFO: Info = {
  service: 'gos3',
  id: 'b3f1c2d4e5f60718',
  name: 'nas',
  version: '0.1.0',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: 'http://h:9000', admin: 'http://h:9000/_gos3/admin/v1' },
  features: [],
  startedAt: '2026-10-09T12:00:00Z'
}

const WHOAMI: WhoAmI = {
  user: 'root',
  accessKey: 'AK',
  isRoot: true,
  policies: [],
  capabilities: { admin: true, viewInfo: true, viewStats: true, manageConfig: true, manageUsers: true, managePolicies: true, manageBuckets: true, discovery: true, scan: true },
  server: INFO
}

const LIST_BUCKETS = `<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>o</ID></Owner><Buckets>
<Bucket><Name>a</Name><CreationDate>2026-01-01T00:00:00.000Z</CreationDate></Bucket>
<Bucket><Name>b</Name><CreationDate>2026-01-01T00:00:00.000Z</CreationDate></Bucket>
</Buckets></ListAllMyBucketsResult>`

const s3Error = (code: string, status: number) => new Response(`<?xml version="1.0"?><Error><Code>${code}</Code><Message>${code} message</Message></Error>`, { status, headers: { 'content-type': 'application/xml' } })
const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })

/** Fake server routing by path. */
function server(routes: { info?: () => Response, whoami?: () => Response, list?: () => Response }): FetchLike {
  return async (req) => {
    const p = new URL(req.url).pathname
    const r = p === '/_gos3/info' ? routes.info : p === '/_gos3/admin/v1/whoami' ? routes.whoami : p === '/' ? routes.list : undefined
    if (!r) return new Response('nope', { status: 404 })
    return r()
  }
}
const down: FetchLike = async () => {
  throw new TypeError('Failed to fetch')
}

const CREDS = { endpoint: 'http://h:9000', region: 'us-east-1', accessKeyId: 'AK', secretAccessKey: 'SK', forcePathStyle: true }
const deps = (fetch: FetchLike, reachable = true) => ({ fetch, reachable: async () => reachable, origin: 'http://ui:3000', protocol: 'http:' })

describe('diagnose', () => {
  const ctx = { endpoint: 'http://h:9000', origin: 'http://ui:3000', protocol: 'http:' }

  it('separates CORS from unreachable hosts', () => {
    const net = new S3Error({ code: 'NetworkError', message: 'x', status: 0 })
    expect(diagnose(net, { ...ctx, reachable: true }).kind).toBe('cors')
    expect(diagnose(net, { ...ctx, reachable: true, kind: 'gos3' }).hint).toMatch(/http:\/\/ui:3000/)
    expect(diagnose(net, { ...ctx, reachable: false }).kind).toBe('network')
    expect(diagnose(net, { ...ctx, protocol: 'https:' }).kind).toBe('mixed-content')
    expect(diagnose(new AdminError('Timeout', 't', 0), ctx).kind).toBe('timeout')
  })

  it('maps credential errors from both APIs', () => {
    expect(diagnose(new S3Error({ code: 'SignatureDoesNotMatch', message: 'm', status: 403 }), ctx).kind).toBe('signature')
    expect(diagnose(new S3Error({ code: 'InvalidAccessKeyId', message: 'm', status: 403 }), ctx).kind).toBe('access-key')
    expect(diagnose(new S3Error({ code: 'RequestTimeTooSkewed', message: 'm', status: 403 }), ctx).kind).toBe('clock-skew')
    expect(diagnose(new S3Error({ code: 'AccessDenied', message: 'm', status: 403 }), ctx).kind).toBe('access-denied')
    // Admin API: 401 Unauthenticated with the S3 code in the message.
    const adm = (m: string) => new AdminError('Unauthenticated', m, 401)
    expect(errorCode(adm('SignatureDoesNotMatch: The request signature we calculated does not match'))).toBe('SignatureDoesNotMatch')
    expect(diagnose(adm('SignatureDoesNotMatch: …'), ctx).kind).toBe('signature')
    expect(diagnose(adm('InvalidAccessKeyId'), ctx).kind).toBe('access-key')
    expect(diagnose(adm('RequestTimeTooSkewed: clock'), ctx).kind).toBe('clock-skew')
    expect(diagnose(adm('anonymous'), ctx).kind).toBe('auth')
    expect(diagnose(new Error('boom'), ctx)).toMatchObject({ kind: 'other', message: 'boom' })
  })
})

describe('detectKind', () => {
  it('detects gos3, protected gos3, generic S3 and unreachable hosts', async () => {
    expect(await detectKind('http://h:9000', deps(server({ info: () => json(INFO) })))).toMatchObject({ kind: 'gos3', info: { id: INFO.id } })
    expect(await detectKind('http://h:9000', deps(server({ info: () => json({ error: { code: 'Unauthenticated', message: 'x' } }, 401) })))).toMatchObject({ kind: 'gos3', protectedInfo: true })
    expect(await detectKind('http://h:9000', deps(server({ info: () => s3Error('AccessDenied', 403) })))).toMatchObject({ kind: 's3', reachable: true })
    expect(await detectKind('http://h:9000', deps(server({})))).toMatchObject({ kind: 's3' }) // 404 → not gos3
    expect(await detectKind('http://h:9000', deps(down, true))).toMatchObject({ kind: 's3', reachable: true })
    expect(await detectKind('http://h:9000', deps(down, false))).toMatchObject({ kind: undefined, reachable: false })
  })
})

describe('verifyCredentials', () => {
  it('uses whoami for gos3 and ListBuckets for S3', async () => {
    const f = server({ whoami: () => json(WHOAMI), list: () => new Response(LIST_BUCKETS, { status: 200 }) })
    expect((await verifyCredentials(CREDS, 'gos3', { fetch: f })).whoami?.user).toBe('root')
    expect(await verifyCredentials(CREDS, 's3', { fetch: f })).toEqual({ bucketCount: 2 })
    expect(await verifyCredentials(CREDS, 's3', { fetch: server({ list: () => s3Error('AccessDenied', 403) }) })).toEqual({ listDenied: true })
  })

  it('diagnoses failures', async () => {
    const bad = await verifyAndDiagnose(CREDS, 's3', deps(server({ list: () => s3Error('SignatureDoesNotMatch', 403) })))
    expect(bad.ok).toBe(false)
    if (!bad.ok) expect(bad.diagnosis.kind).toBe('signature')
    const cors = await verifyAndDiagnose(CREDS, 'gos3', deps(down, true))
    if (!cors.ok) expect(cors.diagnosis.kind).toBe('cors')
    else expect.fail('expected failure')
  })
})

describe('testConnection', () => {
  it('passes against a healthy gos3 server', async () => {
    const f = server({ info: () => json(INFO), whoami: () => json(WHOAMI), list: () => new Response(LIST_BUCKETS) })
    const r = await testConnection({ ...CREDS, kind: 'auto' }, deps(f))
    expect(r.ok).toBe(true)
    expect(r.kind).toBe('gos3')
    expect(r.steps.map(s => [s.id, s.status])).toEqual([['reach', 'ok'], ['identity', 'ok'], ['buckets', 'ok']])
    expect(r.bucketCount).toBe(2)
  })

  it('stops when unreachable and skips sign-in without credentials', async () => {
    const r = await testConnection({ ...CREDS, kind: 'auto' }, deps(down, false))
    expect(r.ok).toBe(false)
    expect(r.steps[0]).toMatchObject({ id: 'reach', status: 'error', diagnosis: { kind: 'network' } })
    expect(r.steps[1]!.status).toBe('skipped')

    const r2 = await testConnection({ ...CREDS, accessKeyId: '', secretAccessKey: '', kind: 'auto' }, deps(server({ info: () => json(INFO) })))
    expect(r2.ok).toBe(true)
    expect(r2.steps.at(-1)!.status).toBe('skipped')
  })

  it('reports wrong secrets on generic S3', async () => {
    const f = server({ info: () => s3Error('NoSuchBucket', 404), list: () => s3Error('SignatureDoesNotMatch', 403) })
    const r = await testConnection({ ...CREDS, kind: 'auto' }, deps(f))
    expect(r.kind).toBe('s3')
    expect(r.ok).toBe(false)
    expect(r.steps.map(s => s.id)).toEqual(['reach', 'buckets'])
    expect(r.steps[1]!.diagnosis?.kind).toBe('signature')
  })

  it('warns when forced to gos3 on a non-gos3 server', async () => {
    const f = server({ info: () => s3Error('AccessDenied', 403), whoami: () => s3Error('AccessDenied', 403) })
    const r = await testConnection({ ...CREDS, kind: 'gos3' }, deps(f))
    expect(r.steps[0]!.status).toBe('warning')
    expect(r.steps[1]).toMatchObject({ id: 'identity', status: 'error' })
  })
})
