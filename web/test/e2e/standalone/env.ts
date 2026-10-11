// Shared state of the "standalone" Playwright project: the standalone UI
// (Nitro Node server) and the gos3 instances that global.setup.ts starts,
// written to a state file the specs read back.
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

/** web/ directory. */
export const WEB_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
/** Repository root (Go module). */
export const REPO_DIR = join(WEB_DIR, '..')
/** Cache directory (git-ignored) shared with the real-server project. */
export const CACHE_DIR = join(WEB_DIR, '.e2e-cache')
/**
 * Standalone build output. `pnpm build` is run with NUXT_BUILD_DIR /
 * NUXT_OUTPUT_DIR pointing here so it never overwrites web/.output, which
 * the embedded projects use.
 */
export const BUILD_DIR = join(CACHE_DIR, 'standalone-nuxt')
export const OUTPUT_DIR = join(CACHE_DIR, 'standalone-output')
/** Build of a temp copy of the sources with public/servers.json (presets). */
export const PRESET_OUTPUT_DIR = join(CACHE_DIR, 'standalone-preset-output')
/** State file written by global.setup.ts and removed by global.teardown.ts. */
export const STATE_FILE = join(CACHE_DIR, 'standalone.json')
/** Generic process supervisor shared with the real-server project. */
export const WATCHDOG = join(WEB_DIR, 'test', 'e2e', 'real', 'watchdog.mjs')

/** Root credentials of both gos3 instances. */
export const ROOT_ACCESS_KEY = 'saroot'
export const ROOT_SECRET_KEY = 'saroot-secret-key'

/** Preset connection IDs (see global.setup.ts). */
export const PRESET_FILE_ID = 'preset-file'
export const PRESET_ENV_ID = 'preset-env'
/**
 * Endpoint of the servers.json preset. It is baked into a build, so it is a
 * fixed address (nothing listens on the discard port) rather than one of the
 * per-run gos3 ports — the build is then cached across runs.
 */
export const PRESET_FILE_ENDPOINT = 'http://127.0.0.1:9'

export interface Proc {
  /** PID of the watchdog that owns the process (see real/watchdog.mjs). */
  watchdogPid: number
  logFile: string
}

export interface Gos3Instance extends Proc {
  role: 'alpha' | 'beta'
  name: string
  url: string
  port: number
  id: string
}

export interface UiInstance extends Proc {
  /** Origin the browser uses, e.g. http://localhost:3000 */
  url: string
  port: number
  /** Directory with the server/ and public/ the process serves. */
  dir: string
}

export interface StandaloneState {
  binary: string
  tmpDir: string
  alpha: Gos3Instance
  beta: Gos3Instance
  /** Plain standalone UI (no presets, GOS3_UI_LOCAL_DISCOVERY=true) — the one most specs use. */
  ui: UiInstance
  /** Second UI: a build with public/servers.json, started with NUXT_PUBLIC_SERVERS and without GOS3_UI_LOCAL_DISCOVERY (local discovery off). */
  presetUi: UiInstance
}

export function readState(): StandaloneState {
  try {
    return JSON.parse(readFileSync(STATE_FILE, 'utf8')) as StandaloneState
  } catch (e) {
    throw new Error(`standalone state ${STATE_FILE} missing: run the "standalone" project (its setup project starts the servers). ${String(e)}`)
  }
}
