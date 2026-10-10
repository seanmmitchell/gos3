import { existsSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { defineConfig, devices } from '@playwright/test'

// Two kinds of E2E tests:
//
// - project "chromium": smoke tests against the embedded static build served
//   by test/e2e/serve.mjs (which mocks GET /_gos3/info); feature specs mock
//   the APIs with page.route().
// - project "real-server": test/e2e/real/*.spec.ts drive the UI embedded in a
//   real gos3 binary. Its setup project (real/global.setup.ts) builds the
//   binary and starts two gos3 instances; the teardown project stops them.
//   Run with `pnpm test:e2e:real` (= playwright test --project real-server).
// - project "standalone": test/e2e/standalone/*.spec.ts drive the standalone
//   UI (`pnpm build` → node .output/server/index.mjs, built into
//   .e2e-cache/) against two real gos3 binaries, cross-origin (CORS). Its
//   setup/teardown projects start and stop everything. Run with
//   `pnpm test:e2e:standalone`.
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

// The static mock server is only needed by the "chromium" project. Skip it
// when only other projects run (E2E_REAL_ONLY=1 or --project without
// chromium), so its `pnpm generate:embed` does not race with the real-server
// setup's own UI build.
const argv = process.argv.join(' ')
const projectArgs = [...argv.matchAll(/--project[= ](\S+)/g)].map(m => m[1]!)
const realOnly = process.env.E2E_REAL_ONLY === '1' || (projectArgs.length > 0 && !projectArgs.includes('chromium'))

const chrome = { ...devices['Desktop Chrome'], launchOptions: { executablePath: localChromium() } }

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
      testIgnore: ['real/**', 'standalone/**'],
      use: chrome
    },
    {
      name: 'real-server-setup',
      testDir: 'test/e2e/real',
      testMatch: /global\.setup\.ts$/,
      teardown: 'real-server-teardown'
    },
    {
      name: 'real-server-teardown',
      testDir: 'test/e2e/real',
      testMatch: /global\.teardown\.ts$/
    },
    {
      name: 'real-server',
      testDir: 'test/e2e/real',
      testMatch: /\.spec\.ts$/,
      dependencies: ['real-server-setup'],
      timeout: 120_000,
      expect: { timeout: 15_000 },
      // Specs share two servers; files run in parallel but use unique names.
      fullyParallel: false,
      use: { ...chrome, baseURL: undefined, acceptDownloads: true, actionTimeout: 20_000, navigationTimeout: 30_000 }
    },
    {
      name: 'standalone-setup',
      testDir: 'test/e2e/standalone',
      testMatch: /global\.setup\.ts$/,
      teardown: 'standalone-teardown'
    },
    {
      name: 'standalone-teardown',
      testDir: 'test/e2e/standalone',
      testMatch: /global\.teardown\.ts$/
    },
    {
      name: 'standalone',
      testDir: 'test/e2e/standalone',
      testMatch: /\.spec\.ts$/,
      dependencies: ['standalone-setup'],
      timeout: 120_000,
      expect: { timeout: 15_000 },
      // Specs share the servers (and the UI's localStorage is per test), so
      // files may run in parallel; they use unique bucket names.
      fullyParallel: false,
      use: { ...chrome, baseURL: undefined, acceptDownloads: true, actionTimeout: 20_000, navigationTimeout: 30_000 }
    }
  ],
  webServer: realOnly ? undefined : {
    command: `pnpm generate:embed && node test/e2e/serve.mjs`,
    url: `http://127.0.0.1:${PORT}/_gos3/ui/`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    env: { E2E_PORT: String(PORT) }
  }
})
