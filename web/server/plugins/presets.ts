// Warns at startup when the preset connections of the runtime config
// (NUXT_PUBLIC_SERVERS) carry secret keys: the config is inlined into every
// page, so anyone who can load the console can read them. The build refuses
// them already (nuxt.config.ts); this covers values set at runtime.
import { presetSecretPaths } from '../utils/preset-secrets'

export default defineNitroPlugin(() => {
  const paths = presetSecretPaths(useRuntimeConfig().public?.servers)
  if (paths.length) {
    console.warn(`[gos3-ui] NUXT_PUBLIC_SERVERS contains secret keys (${paths.join(', ')}): every visitor of the console can read them. Remove them; users enter their own keys.`)
  }
})
