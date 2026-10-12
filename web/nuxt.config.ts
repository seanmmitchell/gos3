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

const baseURL = process.env.NUXT_APP_BASE_URL || '/'

// Optional alternative build/output directories (default .nuxt / .output) so
// that a second build — e.g. the standalone e2e build in .e2e-cache — can run
// next to the regular ones without overwriting them.
const buildDir = process.env.NUXT_BUILD_DIR || undefined
const outputDir = process.env.NUXT_OUTPUT_DIR || undefined

export default defineNuxtConfig({
  compatibilityDate: '2026-10-01',

  modules: ['@nuxt/ui', '@pinia/nuxt'],

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
    serverBundle: 'local',
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
