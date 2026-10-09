import { fileURLToPath } from 'node:url'
import { defineVitestProject } from '@nuxt/test-utils/config'
import { defineConfig } from 'vitest/config'

const alias = {
  '~~': fileURLToPath(new URL('./', import.meta.url)),
  '~': fileURLToPath(new URL('./app', import.meta.url)),
  '@': fileURLToPath(new URL('./app', import.meta.url))
}

// Two Vitest projects:
// - unit:  the framework-independent core (S3/admin clients, store, utils).
//          Modules are imported explicitly (no Nuxt auto-imports) and run in
//          Node (native fetch/WebCrypto) with DOMParser + Web Storage
//          installed by test/setup.ts. Fast; put most tests here.
// - nuxt:  components/composables that need the Nuxt runtime (auto-imports,
//          Nuxt UI, Pinia) via @nuxt/test-utils (`mountSuspended`), in a
//          happy-dom environment. Files: test/nuxt/**/*.test.ts.
export default defineConfig({
  test: {
    projects: [
      {
        resolve: { alias },
        test: {
          name: 'unit',
          environment: 'node',
          setupFiles: ['./test/setup.ts'],
          include: ['test/unit/**/*.test.ts'],
          restoreMocks: true,
          unstubGlobals: true
        }
      },
      await defineVitestProject({
        test: {
          name: 'nuxt',
          environment: 'nuxt',
          include: ['test/nuxt/**/*.test.ts'],
          restoreMocks: true,
          environmentOptions: {
            nuxt: { domEnvironment: 'happy-dom' }
          }
        }
      })
    ]
  }
})
