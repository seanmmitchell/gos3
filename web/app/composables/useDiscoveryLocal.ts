import { LocalDiscoveryClient, type LocalCapabilities } from '~/components/discovery/-local-client'
import { useConnectionsStore } from '~/stores/connections'

let detecting: Promise<LocalCapabilities | null> | null = null

/**
 * Local network discovery from the UI host (standalone Node server only).
 *
 *   const local = useDiscoveryLocal()
 *   await local.detect()                 // GET /api/discovery/capabilities once
 *   if (local.available.value) await local.client.discover(3000)
 *
 * In the embedded/static build the Nitro routes do not exist: detection is
 * skipped (embedded) or fails gracefully (404 / SPA fallback) and
 * `available` stays false, so pages simply hide the feature.
 */
export function useDiscoveryLocal() {
  const config = useRuntimeConfig()
  const store = useConnectionsStore()
  const capabilities = useState<LocalCapabilities | null>('gos3-local-discovery-caps', () => null)
  const checked = useState<boolean>('gos3-local-discovery-checked', () => false)
  const checking = ref(false)
  const baseURL = config.app.baseURL || '/'
  const client = new LocalDiscoveryClient(baseURL)

  /** Served by a gos3 binary (static embedded build): no server routes. */
  const embedded = computed(() => {
    const flag = config.public.embedded as boolean | string | undefined
    return flag === true || flag === 'true' || baseURL.startsWith('/_gos3/ui') || !!store.embeddedInfo
  })
  const available = computed(() => !!capabilities.value?.localDiscovery)

  async function detect(force = false): Promise<LocalCapabilities | null> {
    if (checked.value && !force) return capabilities.value
    if (embedded.value) {
      capabilities.value = null
      checked.value = true
      return null
    }
    checking.value = true
    try {
      detecting ??= client.capabilities()
      capabilities.value = await detecting
      checked.value = true
      return capabilities.value
    } finally {
      detecting = null
      checking.value = false
    }
  }

  return { client, capabilities, available, checked, checking, embedded, detect }
}
