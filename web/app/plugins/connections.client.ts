// Initialises the connections store before the first navigation: loads
// persisted connections, merges presets (NUXT_PUBLIC_SERVERS and
// `${baseURL}servers.json`) and detects the embedding gos3 server.
import { useConnectionsStore } from '~/stores/connections'

export default defineNuxtPlugin({
  name: 'gos3-connections',
  dependsOn: ['pinia'],
  async setup() {
    const store = useConnectionsStore()
    const config = useRuntimeConfig()
    await store.init({
      presets: config.public.servers,
      baseURL: config.app.baseURL,
      embedded: config.public.embedded as boolean | string
    })
  }
})
