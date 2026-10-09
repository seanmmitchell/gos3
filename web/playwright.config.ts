import { existsSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { defineConfig, devices } from '@playwright/test'

// E2E smoke tests against the embedded static build served by
// test/e2e/serve.mjs (which also mocks GET /_gos3/info).
// Browsers are never downloaded: we use a locally installed Chromium
// (PW_CHROMIUM_PATH, or the newest chromium-* under PLAYWRIGHT_BROWSERS_PATH
// / /opt/pw-browsers).
function localChromium(): string | undefined {
  if (process.env.PW_CHROMIUM_PATH) return process.env.PW_CHROMIUM_PATH
  const root = process.env.PLAYWRIGHT_BROWSERS_PATH || '/opt/pw-browsers'
  try {
    const dirs = readdirSync(root).filter(d => /^chromium-\d+$/.test(d)).sort((a, b) => Number(b.split('-')[1]) - Number(a.split('-')[1]))
    for (const d of dirs) {
      const p = join(root, d, 'chrome-linux', 'chrome')
      if (existsSync(p)) return p
    }
  } catch {
    // fall back to Playwright's own resolution
  }
  return undefined
}

const PORT = Number(process.env.E2E_PORT || 4173)

export default defineConfig({
  testDir: 'test/e2e',
  timeout: 30_000,
  fullyParallel: true,
  reporter: process.env.CI ? 'line' : 'list',
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: 'retain-on-failure'
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'], launchOptions: { executablePath: localChromium() } }
    }
  ],
  webServer: {
    command: `pnpm generate:embed && node test/e2e/serve.mjs`,
    url: `http://127.0.0.1:${PORT}/_gos3/ui/`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    env: { E2E_PORT: String(PORT) }
  }
})
