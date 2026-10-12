// Build + process helpers for the README screenshot generator
// (readme.shots.ts): the embedded UI is generated into its own directories
// and overlaid into a dedicated gos3 binary, so nothing in web/.output or
// internal/webui/dist is touched.
import { spawn, spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, readdirSync, renameSync, statSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { join, relative } from 'node:path'
import { CACHE_DIR, REPO_DIR, WATCHDOG, WEB_DIR } from '../standalone/env'

export { CACHE_DIR, REPO_DIR, WEB_DIR }

/** Output directory of the screenshots (repository docs). */
export const SHOTS_DIR = join(REPO_DIR, 'docs', 'screenshots')
const EMBED_BUILD_DIR = join(CACHE_DIR, 'shots-nuxt')
const EMBED_OUTPUT_DIR = join(CACHE_DIR, 'shots-output')
const EMBED_PUBLIC = join(EMBED_OUTPUT_DIR, 'public')

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

function run(cmd: string, args: string[], cwd: string, env: NodeJS.ProcessEnv): void {
  const r = spawnSync(cmd, args, { cwd, env, encoding: 'utf8', maxBuffer: 64 << 20, timeout: 600_000 })
  if (r.status !== 0) throw new Error(`${cmd} ${args.join(' ')} failed (${r.status ?? r.signal}):\n${(r.stdout ?? '').slice(-4000)}\n${(r.stderr ?? '').slice(-4000)}`)
}

/** `pnpm generate:embed` into EMBED_OUTPUT_DIR when the UI sources changed. */
function ensureEmbedBuild(): void {
  const src = newestMtime(['app', 'public', 'nuxt.config.ts', 'package.json'].map(f => join(WEB_DIR, f)))
  if (mtime(join(EMBED_PUBLIC, 'index.html')) > src) return
  run('pnpm', ['generate:embed'], WEB_DIR, { ...process.env, NUXT_BUILD_DIR: EMBED_BUILD_DIR, NUXT_OUTPUT_DIR: EMBED_OUTPUT_DIR })
}

/** gos3 binary embedding EMBED_PUBLIC (go build -overlay), rebuilt when stale. */
export function ensureEmbeddedBinary(): string {
  if (process.env.GOS3_BIN) return process.env.GOS3_BIN
  ensureEmbedBuild()
  mkdirSync(CACHE_DIR, { recursive: true })
  const bin = join(CACHE_DIR, 'gos3-shots')
  const goSrc = newestMtime([join(REPO_DIR, 'cmd'), join(REPO_DIR, 'internal'), join(REPO_DIR, 'go.mod')], p => p.endsWith('.go') && !p.endsWith('_test.go'))
  if (mtime(bin) > Math.max(goSrc, newestMtime([EMBED_PUBLIC]))) return bin
  const distDir = join(REPO_DIR, 'internal', 'webui', 'dist')
  const replace: Record<string, string> = {}
  const walk = (dir: string) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, e.name)
      if (e.isDirectory()) walk(p)
      else replace[join(distDir, relative(EMBED_PUBLIC, p))] = p
    }
  }
  walk(EMBED_PUBLIC)
  if (!existsSync(join(EMBED_PUBLIC, 'index.html'))) throw new Error(`${EMBED_PUBLIC} has no index.html`)
  const overlay = join(CACHE_DIR, 'shots-overlay.json')
  writeFileSync(overlay, JSON.stringify({ Replace: replace }))
  run('go', ['build', '-overlay', overlay, '-o', `${bin}.tmp`, './cmd/gos3'], REPO_DIR, { ...process.env, CGO_ENABLED: '0' })
  renameSync(`${bin}.tmp`, bin)
  return bin
}

export function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer()
    srv.once('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address()
      const p = typeof addr === 'object' && addr ? addr.port : 0
      srv.close(() => resolve(p))
    })
  })
}

/** Starts a process under the shared watchdog; returns the watchdog PID. */
export function supervise(binary: string, args: string[], logFile: string, env: NodeJS.ProcessEnv): number {
  const w = spawn(process.execPath, [WATCHDOG, String(process.ppid), logFile, binary, ...args], { env, detached: true, stdio: 'ignore' })
  w.unref()
  return w.pid ?? 0
}
