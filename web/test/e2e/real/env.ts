// Shared state of the "real-server" Playwright project: where the setup
// project writes the description of the gos3 instances it started, and how
// the specs read it back.
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

/** web/ directory. */
export const WEB_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
/** Repository root (Go module). */
export const REPO_DIR = join(WEB_DIR, '..')
/** Cache directory for the e2e binary and run state (git-ignored). */
export const CACHE_DIR = join(WEB_DIR, '.e2e-cache')
/** State file written by global.setup.ts and removed by global.teardown.ts. */
export const STATE_FILE = join(CACHE_DIR, 'servers.json')

/** Root credentials of both instances. */
export const ROOT_ACCESS_KEY = 'e2eroot'
export const ROOT_SECRET_KEY = 'e2eroot-secret-key'

/** Origin of the standalone UI that the servers allow via CORS. */
export const STANDALONE_ORIGINS = ['http://localhost:3000', 'http://127.0.0.1:3000']

export interface RealServer {
  /** Short role name: "primary" or "peer". */
  role: string
  /** Base URL, e.g. http://127.0.0.1:41234 */
  url: string
  port: number
  /** Server name passed with --name. */
  name: string
  /** Server ID from /_gos3/info. */
  id: string
  dataDir: string
  logFile: string
  /** PID of the watchdog that owns the gos3 process (see watchdog.mjs). */
  watchdogPid: number
}

export interface RealState {
  binary: string
  tmpDir: string
  primary: RealServer
  peer: RealServer
}

export function readState(): RealState {
  try {
    return JSON.parse(readFileSync(STATE_FILE, 'utf8')) as RealState
  } catch (e) {
    throw new Error(`real-server state ${STATE_FILE} missing: run the "real-server" project (its setup project starts gos3). ${String(e)}`)
  }
}
