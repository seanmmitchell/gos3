// Setup project of "real-server": builds the gos3 binary with the current
// embedded UI and starts two instances on loopback ports:
//
//   primary  fs backend (temp dir), known root credentials, discovery on
//            (allowScan, no mDNS/beacon multicast), CORS for the standalone
//            UI origin, seeded with the peer
//   peer     same, seeded with the primary
//
// The binary is built into web/.e2e-cache/gos3 with `go build -overlay`
// replacing internal/webui/dist by web/.output/public, so the shared
// internal/webui/dist placeholder in the source tree is never touched (unlike
// `make build`). It is rebuilt only when Go sources or the UI build are newer.
// Overrides: GOS3_BIN=<path> uses a prebuilt binary (e.g. bin/gos3 from
// `make build`); E2E_SKIP_UI_BUILD=1 skips the `pnpm generate:embed` freshness
// check.
import { spawn, spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readdirSync, renameSync, statSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join, relative } from 'node:path'
import { test as setup } from '@playwright/test'
import { AwsClient } from 'aws4fetch'
import { CACHE_DIR, REPO_DIR, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, STANDALONE_ORIGINS, STATE_FILE, WEB_DIR, type RealServer, type RealState } from './env'
import { stopServers } from './stop'

const UI_BUILD = join(WEB_DIR, '.output', 'public')

/** Newest mtime (ms) of the files under paths accepted by filter. */
function newestMtime(paths: string[], filter: (p: string) => boolean = () => true): number {
  let newest = 0
  const visit = (p: string) => {
    let st
    try {
      st = statSync(p)
    } catch {
      return
    }
    if (st.isDirectory()) {
      for (const e of readdirSync(p)) {
        if (e === 'node_modules' || e.startsWith('.nuxt') || e === '.output') continue
        visit(join(p, e))
      }
    } else if (filter(p)) {
      newest = Math.max(newest, st.mtimeMs)
    }
  }
  paths.forEach(visit)
  return newest
}

function mtime(p: string): number {
  try {
    return statSync(p).mtimeMs
  } catch {
    return 0
  }
}

function run(cmd: string, args: string[], opts: { cwd: string, env?: NodeJS.ProcessEnv }) {
  const r = spawnSync(cmd, args, { cwd: opts.cwd, env: { ...process.env, ...opts.env }, encoding: 'utf8', maxBuffer: 64 << 20, timeout: 600_000 })
  if (r.status !== 0) {
    throw new Error(`${cmd} ${args.join(' ')} failed (${r.status ?? r.signal}):\n${(r.stdout ?? '').slice(-4000)}\n${(r.stderr ?? '').slice(-4000)}`)
  }
}

/** Rebuilds the embedded UI (base /_gos3/ui/) when app sources are newer than the build. */
function ensureUiBuild() {
  if (process.env.E2E_SKIP_UI_BUILD === '1' && existsSync(join(UI_BUILD, 'index.html'))) return
  const src = newestMtime([join(WEB_DIR, 'app'), join(WEB_DIR, 'public'), join(WEB_DIR, 'nuxt.config.ts'), join(WEB_DIR, 'package.json')])
  const built = Math.min(mtime(join(UI_BUILD, 'index.html')), mtime(join(UI_BUILD, '200.html')) || Infinity)
  if (built > src) return
  run('pnpm', ['generate:embed'], { cwd: WEB_DIR })
}

/** Builds (or reuses) the gos3 binary embedding UI_BUILD; returns its path. */
function ensureBinary(): string {
  if (process.env.GOS3_BIN) return process.env.GOS3_BIN
  mkdirSync(CACHE_DIR, { recursive: true })
  const bin = join(CACHE_DIR, 'gos3')
  const goSrc = newestMtime([join(REPO_DIR, 'cmd'), join(REPO_DIR, 'internal'), join(REPO_DIR, 'go.mod'), join(REPO_DIR, 'go.sum')],
    p => p.endsWith('.go') && !p.endsWith('_test.go') && !p.includes(`${join('internal', 'webui', 'dist')}`))
  const uiSrc = newestMtime([UI_BUILD])
  if (mtime(bin) > Math.max(goSrc, uiSrc)) return bin

  // Overlay: internal/webui/dist/<rel> → web/.output/public/<rel>. The
  // placeholder index.html is replaced too (the build always has one).
  const replace: Record<string, string> = {}
  const distDir = join(REPO_DIR, 'internal', 'webui', 'dist')
  const walk = (dir: string) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, e.name)
      if (e.isDirectory()) walk(p)
      else replace[join(distDir, relative(UI_BUILD, p))] = p
    }
  }
  walk(UI_BUILD)
  if (!replace[join(distDir, 'index.html')]) throw new Error(`${UI_BUILD} has no index.html: run pnpm generate:embed`)
  const overlay = join(CACHE_DIR, 'overlay.json')
  writeFileSync(overlay, JSON.stringify({ Replace: replace }))
  const tmp = `${bin}.tmp`
  run('go', ['build', '-overlay', overlay, '-o', tmp, './cmd/gos3'], { cwd: REPO_DIR, env: { CGO_ENABLED: '0' } })
  renameSync(tmp, bin)
  return bin
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer()
    srv.once('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address()
      const port = typeof addr === 'object' && addr ? addr.port : 0
      srv.close(() => resolve(port))
    })
  })
}

function start(binary: string, role: string, name: string, port: number, seed: string, tmp: string): Omit<RealServer, 'id'> {
  const dataDir = join(tmp, `${role}-data`)
  mkdirSync(dataDir)
  const logFile = join(tmp, `${role}.log`)
  const url = `http://127.0.0.1:${port}`
  const args = [
    'serve',
    '--address', `127.0.0.1:${port}`,
    '--backend', 'fs',
    '--data', dataDir,
    '--fsync=false',
    '--name', name,
    '--seed', seed,
    '--log-level', 'debug',
    ...STANDALONE_ORIGINS.flatMap(o => ['--cors-origin', o])
  ]
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    GOS3_ROOT_ACCESS_KEY: ROOT_ACCESS_KEY,
    GOS3_ROOT_SECRET_KEY: ROOT_SECRET_KEY,
    GOS3_DISCOVERY_ENABLED: 'true',
    GOS3_DISCOVERY_ALLOW_SCAN: 'true',
    // Multicast is not deterministic in CI containers; peers come from seeds.
    GOS3_DISCOVERY_MDNS: 'false',
    GOS3_DISCOVERY_BEACON: 'false',
    GOS3_PUBLIC_INFO: 'true',
    GOS3_METRICS_ENABLED: 'true',
    GOS3_LIFECYCLE_INTERVAL: '1h'
  }
  delete env.GOS3_CONFIG
  const watchdog = spawn(process.execPath, [join(WEB_DIR, 'test', 'e2e', 'real', 'watchdog.mjs'), String(process.ppid), logFile, binary, ...args], {
    env,
    detached: true,
    stdio: 'ignore'
  })
  watchdog.unref()
  return { role, url, port, name, dataDir, logFile, watchdogPid: watchdog.pid ?? 0 }
}

async function waitHealthy(s: Omit<RealServer, 'id'>, timeoutMs = 30_000): Promise<string> {
  const deadline = Date.now() + timeoutMs
  let last = ''
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`${s.url}/_gos3/info`)
      if (r.ok) {
        const info = await r.json() as { id: string }
        return info.id
      }
      last = `HTTP ${r.status}`
    } catch (e) {
      last = String(e)
    }
    await new Promise(r => setTimeout(r, 200))
  }
  throw new Error(`gos3 ${s.role} at ${s.url} did not become healthy: ${last} (log: ${s.logFile})`)
}

/** Waits until server `a` lists `peerId` as a healthy, verified discovery peer. */
async function waitPeer(a: RealServer, peerId: string, timeoutMs = 60_000) {
  const aws = new AwsClient({ accessKeyId: ROOT_ACCESS_KEY, secretAccessKey: ROOT_SECRET_KEY, service: 's3', region: 'us-east-1' })
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const r = await aws.fetch(`${a.url}/_gos3/admin/v1/discovery`)
    if (r.ok) {
      const d = await r.json() as { peers: { id: string, healthy: boolean }[] }
      if (d.peers.some(p => p.id === peerId && p.healthy)) return
    }
    await new Promise(r => setTimeout(r, 500))
  }
  throw new Error(`${a.role} did not discover peer ${peerId} via its seed within ${timeoutMs} ms (log: ${a.logFile})`)
}

setup('build and start gos3 instances', async () => {
  setup.setTimeout(900_000)
  await stopServers()

  ensureUiBuild()
  const binary = ensureBinary()

  const tmp = mkdtempSync(join(tmpdir(), 'gos3-e2e-'))
  const [pPort, qPort] = [await freePort(), await freePort()]
  const pUrl = `http://127.0.0.1:${pPort}`
  const qUrl = `http://127.0.0.1:${qPort}`
  // The peer starts first so that the primary's start-up probe of its seed
  // succeeds right away (seeds are re-probed only every 30 s); the peer finds
  // the primary on its next round, which no spec depends on.
  const q = start(binary, 'peer', 'e2e-peer', qPort, pUrl, tmp)
  const state: RealState = { binary, tmpDir: tmp, primary: { ...q, role: 'primary', watchdogPid: 0, id: '' }, peer: { ...q, id: '' } }
  // Write early so the teardown can clean up even if a later step fails.
  mkdirSync(CACHE_DIR, { recursive: true })
  writeFileSync(STATE_FILE, JSON.stringify(state, null, 2))
  state.peer.id = await waitHealthy(q)
  const p = start(binary, 'primary', 'e2e-primary', pPort, qUrl, tmp)
  state.primary = { ...p, id: '' }
  writeFileSync(STATE_FILE, JSON.stringify(state, null, 2))
  state.primary.id = await waitHealthy(p)
  writeFileSync(STATE_FILE, JSON.stringify(state, null, 2))

  await waitPeer(state.primary, state.peer.id)
})

