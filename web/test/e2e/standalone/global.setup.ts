// Setup project of "standalone": runs the UI the way a standalone deployment
// does — `pnpm build` + `node .output/server/index.mjs` — next to two real
// gos3 instances, so every browser → gos3 request is cross-origin:
//
//   alpha, beta  gos3 (memory backend, known root keys) on free loopback
//                ports; CORS allows both UI origins; discovery on with scans
//                allowed, but no mDNS/beacon multicast and no seeds, so the
//                UI host's scan is what finds them
//   ui           the standalone Node server on http://localhost:3000 (or a
//                free port when 3000 is taken), no presets
//   presetUi     a second Node server with presets from public/servers.json
//                and from NUXT_PUBLIC_SERVERS (set at runtime). Nitro serves
//                public/ from a manifest made at build time, so servers.json
//                must exist before `nuxt build`: the setup builds a temp copy
//                of the sources (outside web/) with that file added — presets
//                never touch web/public
//
// The build goes to web/.e2e-cache/standalone-{nuxt,output} (NUXT_BUILD_DIR /
// NUXT_OUTPUT_DIR) so it never overwrites web/.output, which the embedded
// projects use; it is redone only when UI sources are newer. The gos3 binary
// is `go build`-ed into web/.e2e-cache/gos3-standalone when Go sources are
// newer (the embedded UI is not needed; bin/gos3 is the fallback if the
// build fails). Overrides: GOS3_BIN=<path>, E2E_SKIP_UI_BUILD=1.
import { spawn, spawnSync } from 'node:child_process'
import { cpSync, existsSync, mkdirSync, mkdtempSync, readdirSync, renameSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { test as setup } from '@playwright/test'
import {
  BUILD_DIR, CACHE_DIR, OUTPUT_DIR, PRESET_ENV_ID, PRESET_FILE_ENDPOINT, PRESET_FILE_ID, PRESET_OUTPUT_DIR, REPO_DIR, ROOT_ACCESS_KEY, ROOT_SECRET_KEY, STATE_FILE, WATCHDOG, WEB_DIR,
  type Gos3Instance, type StandaloneState, type UiInstance
} from './env'
import { stopAll } from './stop'

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
        if (e === 'node_modules' || e.startsWith('.')) continue
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

function run(cmd: string, args: string[], opts: { cwd: string, env?: NodeJS.ProcessEnv }): { ok: boolean, output: string } {
  const r = spawnSync(cmd, args, { cwd: opts.cwd, env: opts.env ?? process.env, encoding: 'utf8', maxBuffer: 64 << 20, timeout: 600_000 })
  return { ok: r.status === 0, output: `${cmd} ${args.join(' ')} → ${r.status ?? r.signal}\n${(r.stdout ?? '').slice(-4000)}\n${(r.stderr ?? '').slice(-4000)}` }
}

/** Files and directories of web/ that a build needs. */
const UI_SOURCES = ['app', 'server', 'public', 'nuxt.config.ts', 'package.json', 'tsconfig.json', 'test/tsconfig.json', '.nuxtrc']

/** Environment of a standalone build: base '/', not embedded. */
function buildEnv(extra: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env, ...extra }
  delete env.NUXT_APP_BASE_URL
  delete env.NUXT_PUBLIC_EMBEDDED
  delete env.NITRO_PRESET
  return env
}

/** True when `outputDir` holds a build newer than the UI sources. */
function buildIsFresh(outputDir: string): boolean {
  const entry = join(outputDir, 'server', 'index.mjs')
  if (process.env.E2E_SKIP_UI_BUILD === '1' && existsSync(entry)) return true
  return mtime(entry) > newestMtime(UI_SOURCES.map(f => join(WEB_DIR, f)))
}

/** `pnpm build` of web/ into OUTPUT_DIR (web/.output is left alone). */
function ensureStandaloneBuild(): void {
  if (buildIsFresh(OUTPUT_DIR)) return
  const r = run('pnpm', ['build'], { cwd: WEB_DIR, env: buildEnv({ NUXT_BUILD_DIR: BUILD_DIR, NUXT_OUTPUT_DIR: OUTPUT_DIR }) })
  if (!r.ok) throw new Error(`standalone UI build failed:\n${r.output}`)
}

/**
 * Builds a copy of the UI sources with public/servers.json into
 * PRESET_OUTPUT_DIR. The copy lives outside web/ (so typecheck, Tailwind and
 * the icon scan never see it) and shares web/node_modules via a symlink.
 */
function ensurePresetBuild(): void {
  if (buildIsFresh(PRESET_OUTPUT_DIR)) return
  const src = join(tmpdir(), 'gos3-standalone-preset-src')
  rmSync(src, { recursive: true, force: true })
  mkdirSync(src, { recursive: true })
  for (const f of UI_SOURCES) {
    if (!existsSync(join(WEB_DIR, f))) continue
    mkdirSync(dirname(join(src, f)), { recursive: true })
    cpSync(join(WEB_DIR, f), join(src, f), { recursive: true })
  }
  symlinkSync(join(WEB_DIR, 'node_modules'), join(src, 'node_modules'), 'dir')
  writeFileSync(join(src, 'public', 'servers.json'), JSON.stringify({
    servers: [{ id: PRESET_FILE_ID, name: 'Preset from file', endpoint: PRESET_FILE_ENDPOINT, kind: 'gos3', color: 'violet' }]
  }, null, 2))
  const r = run('pnpm', ['exec', 'nuxt', 'build'], { cwd: src, env: buildEnv({ NUXT_OUTPUT_DIR: PRESET_OUTPUT_DIR }) })
  if (!r.ok) throw new Error(`preset UI build failed:\n${r.output}`)
  rmSync(src, { recursive: true, force: true })
}

/** gos3 binary: GOS3_BIN, else a fresh `go build`, else bin/gos3. */
function ensureBinary(): string {
  if (process.env.GOS3_BIN) return process.env.GOS3_BIN
  mkdirSync(CACHE_DIR, { recursive: true })
  const bin = join(CACHE_DIR, 'gos3-standalone')
  const goSrc = newestMtime([join(REPO_DIR, 'cmd'), join(REPO_DIR, 'internal'), join(REPO_DIR, 'go.mod'), join(REPO_DIR, 'go.sum')],
    p => p.endsWith('.go') && !p.endsWith('_test.go'))
  if (mtime(bin) > goSrc) return bin
  const tmp = `${bin}.tmp`
  const r = run('go', ['build', '-o', tmp, './cmd/gos3'], { cwd: REPO_DIR, env: { ...process.env, CGO_ENABLED: '0' } })
  if (r.ok) {
    renameSync(tmp, bin)
    return bin
  }
  const fallback = join(REPO_DIR, 'bin', 'gos3')
  if (existsSync(fallback)) {
    console.warn(`[standalone] go build failed, using ${fallback}:\n${r.output}`)
    return fallback
  }
  throw new Error(`cannot build gos3 (and no bin/gos3 to fall back to; run make build):\n${r.output}`)
}

function listen(port: number): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer()
    srv.once('error', reject)
    srv.listen(port, '127.0.0.1', () => {
      const addr = srv.address()
      const p = typeof addr === 'object' && addr ? addr.port : 0
      srv.close(() => resolve(p))
    })
  })
}

/** A free loopback port; `preferred` when it is free. */
async function freePort(preferred = 0): Promise<number> {
  if (preferred) {
    try {
      return await listen(preferred)
    } catch {
      // taken: fall through to a random port
    }
  }
  return listen(0)
}

/** Starts `binary args…` under the watchdog (stops it when the runner dies). */
function supervise(binary: string, args: string[], logFile: string, env: NodeJS.ProcessEnv): number {
  const watchdog = spawn(process.execPath, [WATCHDOG, String(process.ppid), logFile, binary, ...args], { env, detached: true, stdio: 'ignore' })
  watchdog.unref()
  return watchdog.pid ?? 0
}

function startGos3(binary: string, role: Gos3Instance['role'], port: number, origins: string[], tmp: string): Gos3Instance {
  const logFile = join(tmp, `${role}.log`)
  const args = [
    'serve',
    '--address', `127.0.0.1:${port}`,
    '--backend', 'memory',
    '--name', `sa-${role}`,
    '--log-level', 'debug',
    ...origins.flatMap(o => ['--cors-origin', o])
  ]
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    GOS3_ROOT_ACCESS_KEY: ROOT_ACCESS_KEY,
    GOS3_ROOT_SECRET_KEY: ROOT_SECRET_KEY,
    GOS3_DISCOVERY_ENABLED: 'true',
    GOS3_DISCOVERY_ALLOW_SCAN: 'true',
    // Multicast is not deterministic in CI containers.
    GOS3_DISCOVERY_MDNS: 'false',
    GOS3_DISCOVERY_BEACON: 'false',
    GOS3_PUBLIC_INFO: 'true'
  }
  delete env.GOS3_CONFIG
  delete env.GOS3_DATA
  const watchdogPid = supervise(binary, args, logFile, env)
  return { role, name: `sa-${role}`, url: `http://127.0.0.1:${port}`, port, id: '', logFile, watchdogPid }
}

function startUi(dir: string, port: number, logFile: string, extraEnv: NodeJS.ProcessEnv = {}): UiInstance {
  const env: NodeJS.ProcessEnv = { ...process.env, PORT: String(port), HOST: '127.0.0.1', NODE_ENV: 'production', ...extraEnv }
  for (const k of ['NITRO_PORT', 'NITRO_HOST', 'GOS3_UI_LOCAL_DISCOVERY', 'NUXT_APP_BASE_URL', 'NUXT_PUBLIC_EMBEDDED']) delete env[k]
  if (!extraEnv.NUXT_PUBLIC_SERVERS) delete env.NUXT_PUBLIC_SERVERS
  const watchdogPid = supervise(process.execPath, [join(dir, 'server', 'index.mjs')], logFile, env)
  return { url: `http://localhost:${port}`, port, dir, logFile, watchdogPid }
}

async function waitFor(what: string, logFile: string, check: () => Promise<string | undefined>, timeoutMs = 30_000): Promise<string> {
  const deadline = Date.now() + timeoutMs
  let last = ''
  while (Date.now() < deadline) {
    try {
      const v = await check()
      if (v !== undefined) return v
    } catch (e) {
      last = String(e)
    }
    await new Promise(r => setTimeout(r, 200))
  }
  throw new Error(`${what} did not become ready: ${last} (log: ${logFile})`)
}

function gos3Ready(s: Gos3Instance): Promise<string> {
  return waitFor(`gos3 ${s.role} at ${s.url}`, s.logFile, async () => {
    const r = await fetch(`${s.url}/_gos3/info`)
    return r.ok ? ((await r.json()) as { id: string }).id : undefined
  })
}

function uiReady(u: UiInstance): Promise<string> {
  // 127.0.0.1: Node's fetch may resolve "localhost" to ::1 first.
  return waitFor(`standalone UI at ${u.url}`, u.logFile, async () => {
    const r = await fetch(`http://127.0.0.1:${u.port}/api/discovery/capabilities`)
    return r.ok ? 'ok' : undefined
  })
}

setup('build and start the standalone UI and two gos3 instances', async () => {
  setup.setTimeout(900_000)
  await stopAll()

  ensureStandaloneBuild()
  ensurePresetBuild()
  const binary = ensureBinary()

  const tmp = mkdtempSync(join(tmpdir(), 'gos3-standalone-'))
  const uiPort = await freePort(3000)
  const presetPort = await freePort()
  const [aPort, bPort] = [await freePort(), await freePort()]
  const origins = [uiPort, presetPort].flatMap(p => [`http://localhost:${p}`, `http://127.0.0.1:${p}`])

  const write = (s: Partial<StandaloneState>) => {
    mkdirSync(CACHE_DIR, { recursive: true })
    writeFileSync(STATE_FILE, JSON.stringify(s, null, 2))
  }
  const state: Partial<StandaloneState> = { binary, tmpDir: tmp }
  // Write after every start so that the teardown can clean up after a failure.
  state.alpha = startGos3(binary, 'alpha', aPort, origins, tmp)
  write(state)
  state.beta = startGos3(binary, 'beta', bPort, origins, tmp)
  write(state)
  state.ui = startUi(OUTPUT_DIR, uiPort, join(tmp, 'ui.log'))
  write(state)

  // Presets: servers.json baked into the preset build + NUXT_PUBLIC_SERVERS (beta).
  state.presetUi = startUi(PRESET_OUTPUT_DIR, presetPort, join(tmp, 'preset-ui.log'), {
    NUXT_PUBLIC_SERVERS: JSON.stringify([{ id: PRESET_ENV_ID, name: 'Preset from env', endpoint: state.beta.url, kind: 'gos3', accessKeyId: ROOT_ACCESS_KEY }])
  })
  write(state)

  state.alpha.id = await gos3Ready(state.alpha)
  state.beta.id = await gos3Ready(state.beta)
  await uiReady(state.ui)
  await uiReady(state.presetUi)
  write(state)
})
