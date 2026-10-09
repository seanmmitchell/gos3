// Minimal static server for E2E tests: serves .output/public under /_gos3/ui/
// with SPA fallback, and mocks the gos3 public identity endpoint so the UI's
// embedded-mode detection can be exercised without the Go server.
import { createReadStream, existsSync, statSync } from 'node:fs'
import { createServer } from 'node:http'
import { extname, join, normalize } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('../../.output/public/', import.meta.url))
const base = '/_gos3/ui/'
const port = Number(process.env.E2E_PORT || 4173)

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2'
}

export const MOCK_INFO = {
  service: 'gos3',
  id: 'e2e0000000000001',
  name: 'e2e-gos3',
  version: '0.0.0-e2e',
  apiVersion: 'v1',
  region: 'us-east-1',
  endpoints: { s3: `http://127.0.0.1:${port}`, admin: `http://127.0.0.1:${port}/_gos3/admin/v1`, ui: `http://127.0.0.1:${port}/_gos3/ui/` },
  features: ['admin', 'ui'],
  startedAt: new Date().toISOString()
}

function send(res, status, body, type) {
  res.writeHead(status, { 'content-type': type, 'cache-control': 'no-store' })
  res.end(body)
}

createServer((req, res) => {
  const url = new URL(req.url ?? '/', `http://${req.headers.host}`)
  if (url.pathname === '/_gos3/info') return send(res, 200, JSON.stringify(MOCK_INFO), 'application/json')
  if (url.pathname === '/') {
    res.writeHead(302, { location: base })
    return res.end()
  }
  if (!url.pathname.startsWith(base)) return send(res, 404, '{"error":{"code":"NotFound","message":"not found"}}', 'application/json')
  const rel = normalize(decodeURIComponent(url.pathname.slice(base.length))).replace(/^(\.\.[/\\])+/, '')
  let file = join(root, rel)
  if (!existsSync(file) || statSync(file).isDirectory()) {
    // servers.json is optional: answer 404 like a real deployment without it.
    if (rel === 'servers.json') return send(res, 404, 'not found', 'text/plain')
    file = join(root, existsSync(join(root, '200.html')) ? '200.html' : 'index.html')
  }
  res.writeHead(200, { 'content-type': TYPES[extname(file)] ?? 'application/octet-stream' })
  createReadStream(file).pipe(res)
}).listen(port, '127.0.0.1', () => {
  console.log(`e2e static server on http://127.0.0.1:${port}${base}`)
})
