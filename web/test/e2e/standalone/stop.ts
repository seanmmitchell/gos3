// Stops the processes recorded in the standalone state file and removes
// their temporary files (shared by the setup and teardown projects).
import { existsSync, readFileSync, rmSync } from 'node:fs'
import { STATE_FILE, type Proc, type StandaloneState } from './env'

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0)
    return true
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === 'EPERM'
  }
}

/** Sends SIGTERM to the watchdogs of the given processes and waits for them to exit (SIGKILL after 15 s). */
export async function stopProcs(procs: (Proc | undefined)[]): Promise<void> {
  const pids = procs.map(p => p?.watchdogPid ?? 0).filter(pid => pid > 0 && alive(pid))
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
}

/**
 * Stops both UIs and both gos3 instances of the last run and removes the
 * temp dir (kept when E2E_KEEP_DATA=1) and the state file.
 */
export async function stopAll(): Promise<void> {
  if (!existsSync(STATE_FILE)) return
  let state: Partial<StandaloneState>
  try {
    state = JSON.parse(readFileSync(STATE_FILE, 'utf8')) as Partial<StandaloneState>
  } catch {
    rmSync(STATE_FILE, { force: true })
    return
  }
  await stopProcs([state.ui, state.presetUi, state.alpha, state.beta])
  if (process.env.E2E_KEEP_DATA !== '1' && state.tmpDir) rmSync(state.tmpDir, { recursive: true, force: true })
  rmSync(STATE_FILE, { force: true })
}
