// Runs one gos3 process for the "real-server" Playwright project and makes
// sure it never outlives the test run:
//
//   node watchdog.mjs <ownerPid> <logFile> <binary> [args...]
//
// - gos3's stdout/stderr are appended to <logFile>;
// - SIGTERM/SIGINT to the watchdog stop gos3 gracefully (SIGKILL after 10 s);
// - when <ownerPid> (the Playwright runner) disappears — e.g. the run was
//   aborted before the teardown project ran — gos3 is stopped as well;
// - when gos3 exits on its own the watchdog exits with its status.
import { spawn } from 'node:child_process'
import { openSync } from 'node:fs'

const [ownerArg, logFile, binary, ...args] = process.argv.slice(2)
const ownerPid = Number(ownerArg)
if (!ownerPid || !logFile || !binary) {
  console.error('usage: watchdog.mjs <ownerPid> <logFile> <binary> [args...]')
  process.exit(2)
}

const out = openSync(logFile, 'a')
const child = spawn(binary, args, { stdio: ['ignore', out, out], env: process.env })

let stopping = false
function stop() {
  if (stopping) return
  stopping = true
  child.kill('SIGTERM')
  setTimeout(() => child.kill('SIGKILL'), 10_000).unref()
}

function alive(pid) {
  try {
    process.kill(pid, 0)
    return true
  } catch (e) {
    return e.code === 'EPERM'
  }
}

process.on('SIGTERM', stop)
process.on('SIGINT', stop)
const timer = setInterval(() => {
  if (!alive(ownerPid)) stop()
}, 500)

child.on('exit', (code, signal) => {
  clearInterval(timer)
  process.exit(code ?? (signal ? 1 : 0))
})
child.on('error', (err) => {
  console.error(`watchdog: cannot start ${binary}: ${err.message}`)
  process.exit(1)
})
