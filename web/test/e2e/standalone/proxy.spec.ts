import { test, Gos3Api, uniqueName, ROOT_ACCESS_KEY, ROOT_SECRET_KEY } from './fixtures'
import { seedProxyConnections, exerciseProxyObjects } from '../proxy-scenarios'

test('standalone generic S3 connection relays multipart, previews and downloads', async ({ page, alpha, beta, ui }) => {
  const api = new Gos3Api(beta)
  const bucket = uniqueName('proxy-s3')
  await api.createBucket(bucket)
  await api.putObject(bucket, 'hello.txt', 'hello through relay', { 'Content-Type': 'text/plain' })
  await api.fetch(`/${bucket}/pixel.png`, { method: 'PUT', body: Uint8Array.from(Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=', 'base64')), headers: { 'Content-Type': 'image/png' } })
  try {
    await seedProxyConnections(page, ui(), alpha.url, beta.url, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, 's3')
    await exerciseProxyObjects(page, ui(), bucket, key => api.getText(bucket, key))
  } finally { await api.forceDeleteBucket(bucket) }
})
