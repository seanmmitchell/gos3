// gos3 web UI — Nuxt 4 SPA.
//
// Two deployment modes from one codebase (see README.md):
//   * embedded:   `pnpm generate:embed` (NUXT_APP_BASE_URL=/_gos3/ui/) → static
//                 files served by the gos3 binary at /_gos3/ui/
//   * standalone: `pnpm build` → Node server (.output/server/index.mjs) with the
//                 extra Nitro discovery routes in server/
//
// The embedded build must work fully offline: no runtime CDN, no remote fonts
// and no remote icon API. Icons are bundled into the client at build time.

import { existsSync, readFileSync } from 'node:fs'
import { SECURITY_HEADERS } from './server/utils/csp'
import { ALLOW_PRESET_SECRETS_ENV, presetSecretPaths, presetSecretsAllowed } from './server/utils/preset-secrets'

const baseURL = process.env.NUXT_APP_BASE_URL || '/'

// Optional alternative build/output directories (default .nuxt / .output) so
// that a second build — e.g. the standalone e2e build in .e2e-cache — can run
// next to the regular ones without overwriting them.
const buildDir = process.env.NUXT_BUILD_DIR || undefined
const outputDir = process.env.NUXT_OUTPUT_DIR || undefined

/**
 * Fails the build when preset connections carry secret keys: public/
 * servers.json and the runtime config (NUXT_PUBLIC_SERVERS, inlined into
 * every page) are readable by anyone who can load the UI. Set
 * GOS3_UI_ALLOW_PRESET_SECRETS=true to build anyway (not recommended).
 */
function checkPresetSecrets(rootDir: string): void {
  if (presetSecretsAllowed()) return
  const found: string[] = []
  const file = `${rootDir}/public/servers.json`
  if (existsSync(file)) found.push(...presetSecretPaths(readFileSync(file, 'utf8')).map(p => `public/servers.json: ${p}`))
  found.push(...presetSecretPaths(process.env.NUXT_PUBLIC_SERVERS ?? '').map(p => `NUXT_PUBLIC_SERVERS: ${p}`))
  if (found.length) {
    throw new Error(`Preset connections must not contain secret keys (anyone who can load the UI can read them): ${found.join(', ')}. Remove them, or set ${ALLOW_PRESET_SECRETS_ENV}=true to build anyway.`)
  }
}

export default defineNuxtConfig({
  compatibilityDate: '2026-10-01',

  modules: [
    '@nuxt/ui',
    '@pinia/nuxt',
    // Refuse preset secret keys at build/generate time (see checkPresetSecrets).
    (_options, nuxt) => {
      nuxt.hook('build:before', () => checkPresetSecrets(nuxt.options.rootDir))
    },
    // Standalone server: public/servers.json is not a static asset (Nitro's
    // static handler runs before any middleware, so it would be served for
    // any Host); server/routes/servers.json.get.ts serves its build-time text
    // behind the Host check instead. `nuxt generate` (embedded) keeps the file.
    (_options, nuxt) => {
      if ((nuxt.options as { _generate?: boolean })._generate) return
      const file = `${nuxt.options.rootDir}/public/servers.json`
      nuxt.options.runtimeConfig.presetsFile = existsSync(file) ? readFileSync(file, 'utf8') : ''
      nuxt.hook('nitro:config', (config) => {
        config.ignore = [...(config.ignore ?? []), '**/servers.json', '**/servers.example.json']
      })
    }
  ],

  ssr: false,

  ...(buildDir ? { buildDir } : {}),

  devtools: { enabled: false },


  app: {
    baseURL,
    head: {
      title: 'gos3',
      htmlAttrs: { lang: 'en' },
      meta: [
        { name: 'description', content: 'gos3 — S3-compatible object storage console' },
        { name: 'color-scheme', content: 'light dark' }
      ],
      link: [{ rel: 'icon', type: 'image/svg+xml', href: `${baseURL.replace(/\/?$/, '/')}favicon.svg` }]
    }
  },

  css: ['~/assets/css/main.css'],

  // Nuxt UI: no @nuxt/fonts (it would download Google Fonts); we use a system
  // font stack defined in main.css.
  ui: {
    fonts: false,
    theme: {
      colors: ['primary', 'secondary', 'success', 'info', 'warning', 'error', 'neutral']
    }
  },

  colorMode: {
    preference: 'system',
    fallback: 'light',
    storageKey: 'gos3-color-mode'
  },

  icon: {
    // Only ever use locally bundled icons (offline-safe).
    provider: 'none',
    fallbackToApi: false,
    // No server icon bundle: the client bundles its icons, and the module's
    // cached /api/_nuxt_icon endpoint must not exist (every distinct query
    // would pin a cache entry in memory). server/middleware blocks the path
    // as well.
    serverBundle: false,
    clientBundle: {
      // Scan .vue AND .ts sources so icon names used in navigation items,
      // toasts etc. are bundled too. Icon names must appear literally in the
      // source (e.g. 'i-lucide-trash-2'); names built at runtime are not
      // bundled and will render empty.
      scan: {
        globInclude: ['app/**/*.{vue,ts}'],
        globExclude: ['node_modules', '.nuxt', '.output', 'dist', 'test', '.*']
      },
      icons: [
        'lucide:loader-circle',
        'lucide:circle-alert',
        'lucide:circle-check',
        'lucide:info',
        'lucide:triangle-alert'
      ],
      sizeLimitKb: 1024
    }
  },

  runtimeConfig: {
    // Standalone server only, private: the text of public/servers.json at
    // build time, served by server/routes/servers.json.get.ts.
    presetsFile: '',
    public: {
      // JSON array of preset connections (env NUXT_PUBLIC_SERVERS).
      servers: '',
      // Force embedded mode detection (env NUXT_PUBLIC_EMBEDDED).
      embedded: false,
      appName: 'gos3'
    }
  },

  typescript: {
    strict: true,
    typeCheck: false
  },

  // Standalone server (`nuxt build`): security headers on every response,
  // like the embedded UI's (internal/webui). Rendered pages get a policy
  // with their inline-script hashes from server/plugins/csp.ts. A static
  // build (`nuxt generate`) ignores route rule headers; gos3 sends its own.
  $production: {
    routeRules: {
      '/**': { headers: SECURITY_HEADERS }
    }
  },

  nitro: {
    // `nuxt build` → standalone Node server. `nuxt generate` overrides the
    // preset with its static output automatically.
    preset: process.env.NITRO_PRESET || 'node-server',
    ...(outputDir ? { output: { dir: outputDir } } : {})
  },

  vite: {
    build: {
      // Keep the embedded bundle self-contained.
      assetsInlineLimit: 4096
    }
  }
})
