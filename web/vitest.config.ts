import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitest/config'

// Unit tests for the framework-independent core (S3/admin clients, store,
// utils). They import modules explicitly (no Nuxt auto-imports) and run in
// Node (native fetch/WebCrypto) with DOMParser + Web Storage installed by
// test/setup.ts.
export default defineConfig({
  resolve: {
    alias: {
      '~~': fileURLToPath(new URL('./', import.meta.url)),
      '~': fileURLToPath(new URL('./app', import.meta.url)),
      '@': fileURLToPath(new URL('./app', import.meta.url))
    }
  },
  test: {
    environment: 'node',
    setupFiles: ['./test/setup.ts'],
    include: ['test/unit/**/*.test.ts'],
    restoreMocks: true,
    unstubGlobals: true
  }
})
