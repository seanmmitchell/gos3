// Teardown project of "real-server": stops both gos3 instances and removes
// their data (set E2E_KEEP_DATA=1 to keep data dirs and logs for debugging).
import { test as teardown } from '@playwright/test'
import { stopServers } from './stop'

teardown('stop gos3 instances', async () => {
  teardown.setTimeout(60_000)
  await stopServers()
})
