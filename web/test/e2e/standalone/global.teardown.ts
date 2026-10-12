// Teardown project of "standalone": stops the UI servers and gos3 instances
// (set E2E_KEEP_DATA=1 to keep the temp dir with logs for debugging).
import { test as teardown } from '@playwright/test'
import { stopAll } from './stop'

teardown('stop standalone UI and gos3 instances', async () => {
  teardown.setTimeout(60_000)
  await stopAll()
})
