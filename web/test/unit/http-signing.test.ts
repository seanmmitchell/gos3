import { createHash } from 'node:crypto'
import { AwsClient as NativeAwsClient } from 'aws4fetch'
import { describe, expect, it, vi } from 'vitest'
import { AdminClient } from '~/utils/admin/client'
import { sha256Hex } from '~/utils/s3/hash'
import { AwsClient } from '~/utils/s3/signing'

const options = { accessKeyId: 'AK', secretAccessKey: 'SK', sessionToken: 'session-token', service: 's3', region: 'eu-west-1' }
const datetime = '20261018T120000Z'

describe('signing without WebCrypto (HTTP LAN origins)', () => {
  it('hashes exact UTF-8 and binary views without crypto.subtle', async () => {
    vi.stubGlobal('crypto', undefined)
    const backing = Uint8Array.from([255, 0, 1, 128, 254, 255])
    for (const input of ['', 'abc', 'ünï 🚀', 'a'.repeat(1000), backing.subarray(1, 5), new DataView(backing.buffer, 1, 4)]) {
      const bytes = typeof input === 'string' ? input : new Uint8Array(input.buffer, input.byteOffset, input.byteLength)
      expect(await sha256Hex(input)).toBe(createHash('sha256').update(bytes).digest('hex'))
    }
  })

  it.each([false, true])('matches native SigV4 signing, signQuery=%s, including cached keys and date rollover', async (signQuery) => {
    const native = new NativeAwsClient(options)
    const client = new AwsClient(options)
    const url = 'http://10.0.0.5:9000/bucket/folder/%C3%BC%20name.txt?versionId=a%2Bb&response-content-type=text%2Fplain'
    for (const date of [datetime, datetime, '20261019T120000Z']) {
      const init = { method: 'GET', headers: { Range: 'bytes=0-9', 'x-amz-meta-label': ' a  b ' }, aws: { datetime: date, signQuery } }
      const expected = await native.sign(url, init)
      vi.stubGlobal('crypto', { getRandomValues: globalThis.crypto.getRandomValues })
      try {
        const actual = await client.sign(url, init)
        expect(actual.url).toBe(expected.url)
        expect([...actual.headers]).toEqual([...expected.headers])
      } finally {
        vi.unstubAllGlobals()
      }
    }
  })

  it('signs management reads and JSON writes with the exact payload hash', async () => {
    const captured: Request[] = []
    const admin = new AdminClient({ endpoint: 'http://10.0.0.5:9000', ...options }, {
      datetime,
      fetch: async (req) => {
        captured.push(req.clone())
        return new Response('{}', { headers: { 'content-type': 'application/json' } })
      }
    })
    vi.stubGlobal('crypto', undefined)
    await admin.whoami()
    await admin.request('PUT', '/config', { body: { name: 'HTTP LAN ü 🚀' } })
    vi.unstubAllGlobals()
    const native = new NativeAwsClient(options)
    for (const req of captured) {
      const body = ['GET', 'HEAD'].includes(req.method) ? undefined : await req.text()
      expect(req.headers.get('x-amz-content-sha256')).toBe(createHash('sha256').update(body ?? '').digest('hex'))
      const headers = new Headers(req.headers)
      headers.delete('authorization')
      const expected = await native.sign(req.url, { method: req.method, headers, body, aws: { datetime } })
      expect(req.headers.get('authorization')).toBe(expected.headers.get('authorization'))
    }
  })
})
