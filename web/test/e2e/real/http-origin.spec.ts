// Exercise the browser's actual insecure-context policy on a non-loopback
// IPv4 origin. No crypto stubs, secure-origin flags, or mocked S3/admin APIs.
import { createServer, request } from 'node:http'
import { networkInterfaces } from 'node:os'
import { once } from 'node:events'
import { expect, fillSignIn, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, test, THIS_SERVER, uniqueName } from './fixtures'

test('signs in and uploads over HTTP on a LAN IP without WebCrypto', async ({ page, primary, api }) => {
  const address = Object.values(networkInterfaces()).flat().find(a => a?.family === 'IPv4' && !a.internal)?.address
  if (!address) throw new Error('HTTP LAN regression test requires a non-loopback IPv4 interface')
  // The isolated gos3 fixture listens on loopback. Forward real HTTP traffic
  // through a LAN listener, retaining Host so the server verifies exactly
  // what the browser signed. Neither endpoint nor authentication is mocked.
  const relay = createServer((incoming, outgoing) => {
    const upstream = request(new URL(incoming.url ?? '/', primary.url), {
      method: incoming.method, headers: incoming.headers
    }, response => {
      outgoing.writeHead(response.statusCode ?? 502, response.headers)
      response.pipe(outgoing)
    })
    upstream.on('error', () => { outgoing.writeHead(502); outgoing.end() })
    incoming.pipe(upstream)
  })
  relay.listen(0, address)
  await once(relay, 'listening')
  const bound = relay.address()
  if (!bound || typeof bound === 'string') throw new Error('Missing LAN relay address')
  const origin = `http://${address}:${bound.port}`
  const bucket = uniqueName('http-lan')
  const user = uniqueName('http-user')
  const body = 'browser upload from an insecure HTTP LAN origin\n'
  try {
    await page.goto(`${origin}/_gos3/ui/connect?server=${THIS_SERVER}`)
    expect(await page.evaluate(() => ({ secure: isSecureContext, subtle: typeof crypto.subtle }))).toEqual({ secure: false, subtle: 'undefined' })

    await fillSignIn(page, ROOT_ACCESS_KEY, 'wrong-secret')
    await expect(page.getByRole('alert')).toContainText(/secret key/i)
    await page.getByLabel('Secret access key').fill(ROOT_SECRET_KEY)
    await page.getByRole('button', { name: /^sign in$/i }).click()
    await expect(page.getByRole('button', { name: /^Account: root/ })).toBeVisible()

    await page.goto(`${origin}/_gos3/ui/servers/${THIS_SERVER}/buckets`)
    await page.getByRole('main').getByRole('button', { name: 'Create bucket' }).first().click()
    const dialog = page.getByRole('dialog', { name: 'Create bucket' })
    await dialog.getByRole('textbox', { name: /Bucket name/ }).fill(bucket)
    await dialog.getByRole('button', { name: 'Create bucket' }).click()
    await expect(dialog).toBeHidden()
    await page.goto(`${origin}/_gos3/ui/servers/${THIS_SERVER}/buckets/${bucket}`)
    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('main').getByRole('button', { name: 'Upload', exact: true }).click()
    await (await chooser).setFiles({ name: 'lan.txt', mimeType: 'text/plain', buffer: Buffer.from(body) })
    await expect(page.getByRole('main').getByRole('link', { name: 'lan.txt', exact: true })).toBeVisible()
    expect((await api.getText(bucket, 'lan.txt')).text).toBe(body)

    // Presigning also needs HMAC; verify the browser-generated URL against
    // the real server without adding any credentials on the Node side.
    await page.getByRole('main').getByRole('link', { name: 'lan.txt', exact: true }).click()
    await page.getByRole('dialog', { name: 'lan.txt' }).getByRole('button', { name: 'Share' }).click()
    const share = page.getByRole('dialog', { name: 'Share link' })
    await expect(share.getByRole('textbox').last()).toHaveValue(/X-Amz-Signature=/)
    const shared = await fetch(await share.getByRole('textbox').last().inputValue())
    expect(shared.status).toBe(200)
    expect(await shared.text()).toBe(body)
    await share.getByRole('button', { name: 'Close' }).last().click()

    // A signed management write exercises body hashing as well as HMAC.
    await page.goto(`${origin}/_gos3/ui/servers/${THIS_SERVER}/users`)
    await expect(page.getByRole('heading', { name: 'Users', exact: true })).toBeVisible()
    await page.getByRole('main').getByRole('button', { name: /Create user/ }).first().click()
    const userDialog = page.getByRole('dialog', { name: 'Create user' })
    await userDialog.getByRole('textbox', { name: /User name/ }).fill(user)
    await userDialog.getByRole('textbox', { name: 'Description' }).fill('Created over HTTP LAN')
    await userDialog.getByRole('checkbox', { name: 'Create an access key now' }).uncheck()
    await userDialog.getByRole('button', { name: 'Create user' }).click()
    await expect(userDialog).toBeHidden()
    expect(await api.admin('GET', `/users/${user}`)).toMatchObject({ description: 'Created over HTTP LAN' })
  } finally {
    try {
      await api.forceDeleteBucket(bucket)
      await api.admin('DELETE', `/users/${user}`).catch(() => undefined)
    } finally {
      await new Promise<void>((resolve, reject) => {
        relay.close(error => error ? reject(error) : resolve())
        relay.closeAllConnections()
      })
    }
  }
})
