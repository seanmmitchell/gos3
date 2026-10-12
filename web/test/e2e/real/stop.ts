// Stops the gos3 instances recorded in the real-server state file and
// removes their temporary data (shared by the setup and teardown projects).
import { existsSync, readFileSync, rmSync } from 'node:fs'
import { STATE_FILE, type RealState } from './env'

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0)
    return true
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === 'EPERM'
  }
}

/**
 * Sends SIGTERM to each instance's watchdog (which stops gos3 gracefully) and
 * waits for them to exit. Keeps the temp dir (data + logs) when
 * E2E_KEEP_DATA=1.
 */
export async function stopServers(): Promise<void> {
  if (!existsSync(STATE_FILE)) return
  let state: RealState
  try {
    state = JSON.parse(readFileSync(STATE_FILE, 'utf8')) as RealState
  } catch {
    rmSync(STATE_FILE, { force: true })
    return
  }
  const pids = [state.primary, state.peer].map(s => s.watchdogPid).filter(pid => pid > 0 && alive(pid))
  for (const pid of pids) {
    try {
      process.kill(pid, 'SIGTERM')
    } catch {
      // already gone
    }
  }
  const deadline = Date.now() + 15_000
  while (pids.some(alive) && Date.now() < deadline) await new Promise(r => setTimeout(r, 100))
  for (const pid of pids.filter(alive)) {
    try {
      process.kill(pid, 'SIGKILL')
    } catch {
      // already gone
    }
  }
  if (process.env.E2E_KEEP_DATA !== '1' && state.tmpDir) rmSync(state.tmpDir, { recursive: true, force: true })
  rmSync(STATE_FILE, { force: true })
}
