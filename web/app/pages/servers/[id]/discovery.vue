<script setup lang="ts">
/**
 * Discovery on one gos3 server: configuration + identity, the peer table
 * (GET /discovery, auto-refreshed), probing a URL (POST /discovery/probe) and
 * network scans (POST /discovery/scan). Every peer can be added as a server,
 * optionally reusing this connection's credentials.
 */
import type { ConnectionInput } from '~/types/connection'
import type { Peer, ScanRequest, ScanResult } from '~/types/admin'
import { prefillFromPeer } from '~/components/servers/-form'
import { suggestCidr } from '~/components/discovery/-scan-form'
import { canProbe } from '~/components/admin/-capabilities'
import { AdminError } from '~/utils/admin/client'

useHead({ title: 'Discovery' })

const AUTO_REFRESH_MS = 10000

const route = useRoute()
const toast = useToast()
const id = computed(() => routeParam(route.params.id))
const { connection, isGos3, requireAdmin } = useConnection()
const { can, whoami, capabilities } = useWhoAmI()

const discovery = useLoader(() => requireAdmin().discovery(), { immediate: false })
const state = computed(() => discovery.data.value)
const peers = computed(() => [...(state.value?.peers ?? [])].sort((a, b) => Number(b.healthy) - Number(a.healthy) || a.name.localeCompare(b.name)))
const accessDenied = computed(() => discovery.error.value instanceof AdminError && discovery.error.value.isAccessDenied)
const lastLoaded = ref<number | null>(null)

async function refresh() {
  const r = await discovery.refresh()
  if (r) lastLoaded.value = Date.now()
}

// -- auto refresh ------------------------------------------------------------------

const autoRefresh = ref(true)
let timer: ReturnType<typeof setInterval> | undefined
function stopTimer() {
  if (timer) clearInterval(timer)
  timer = undefined
}
watch([autoRefresh, () => !!state.value], ([on, loaded]) => {
  stopTimer()
  if (!on || !loaded) return
  timer = setInterval(() => {
    if (document.visibilityState === 'visible' && !discovery.loading.value) void refresh()
  }, AUTO_REFRESH_MS)
}, { immediate: true })
onBeforeUnmount(stopTimer)

// -- probe & scan ------------------------------------------------------------------

/** POST /discovery/probe needs admin:ProbeNetwork (not admin:GetDiscovery). */
const probeCapable = computed(() => canProbe(connection.value?.kind, capabilities.value))

async function probe(url: string): Promise<Peer> {
  let p: Peer
  try {
    p = await requireAdmin().probe(url)
  } catch (e) {
    if (e instanceof AdminError && e.isAccessDenied) {
      throw new AdminError(e.code, `Not allowed: probing a URL needs admin:ProbeNetwork. ${e.message}`.trim(), e.status, e)
    }
    throw e
  }
  void refresh() // the probed peer joins the table
  return p
}

const scanAllowed = computed(() => state.value?.config.allowScan ?? false)
const scanCapable = computed(() => can('scan') || !!whoami.value?.isRoot)
const scanning = ref(false)
const scanResult = ref<ScanResult | null>(null)
const scanError = ref<unknown>(null)
let scanCtrl: AbortController | null = null

async function runScan(req: ScanRequest) {
  scanCtrl?.abort()
  scanCtrl = new AbortController()
  scanning.value = true
  scanError.value = null
  scanResult.value = null
  try {
    scanResult.value = await requireAdmin().scan(req, scanCtrl.signal)
    const n = scanResult.value.found?.length ?? 0
    toast.add({ title: 'Scan finished', description: `${n} server${n === 1 ? '' : 's'} found, ${scanResult.value.probed} URLs probed in ${scanResult.value.elapsed}.`, color: 'success', icon: 'i-lucide-scan-search' })
    void refresh()
  } catch (e) {
    if (!isAbortedError(e)) scanError.value = e
  } finally {
    scanning.value = false
    scanCtrl = null
  }
}

function cancelScan() {
  scanCtrl?.abort()
}
onBeforeUnmount(cancelScan)

const scanPlaceholder = computed(() => {
  const self = state.value?.self
  const cidr = suggestCidr([self?.endpoints?.s3, ...(self?.endpoints?.all ?? []), connection.value?.endpoint])
  return cidr ? `${cidr}\nnas.local\nhttp://10.0.0.5:9000` : undefined
})

// Reload when the route switches to another server.
watch([id, isGos3], ([, gos3]) => {
  discovery.data.value = undefined
  scanResult.value = null
  if (gos3) void refresh()
}, { immediate: true })

// -- add as server -------------------------------------------------------------------

const addOpen = ref(false)
const addPrefill = ref<ConnectionInput | undefined>()

function addPeer(p: Peer) {
  const prefill = prefillFromPeer(p)
  if (!prefill) {
    toast.add({ title: 'No usable address', description: 'This peer did not advertise a URL.', color: 'warning', icon: 'i-lucide-triangle-alert' })
    return
  }
  addPrefill.value = prefill
  addOpen.value = true
}
</script>

<template>
  <div>
    <CommonPageHeader
      title="Discovery"
      icon="i-lucide-radar"
      :description="connection ? `gos3 servers that ${connection.name} knows about (mDNS, beacon, seeds, DNS SRV and scans).` : undefined"
    >
      <template #actions>
        <USwitch v-if="isGos3 && state" v-model="autoRefresh" label="Auto-refresh" size="sm" />
        <UButton
          v-if="isGos3"
          color="neutral"
          variant="outline"
          icon="i-lucide-refresh-cw"
          label="Refresh"
          :loading="discovery.loading.value"
          @click="refresh"
        />
        <UButton color="neutral" variant="ghost" icon="i-lucide-scan-search" label="Discover everywhere" :to="appRoutes.discover()" />
      </template>
    </CommonPageHeader>

    <div class="space-y-6">
      <CommonEmptyState
        v-if="!isGos3"
        icon="i-lucide-server-off"
        title="Discovery needs a gos3 server"
        description="This connection is a generic S3 server. Use the Discover page to find gos3 servers from this browser."
      >
        <UButton :to="appRoutes.discover()" icon="i-lucide-scan-search" label="Open Discover" />
      </CommonEmptyState>

      <template v-else>
        <CommonEmptyState
          v-if="accessDenied"
          icon="i-lucide-lock"
          title="No access to discovery"
          description="Your identity is not allowed to read this server's discovery state (admin:GetDiscovery). Ask an administrator for the diagnostics or admin policy."
        />
        <CommonErrorAlert v-else-if="discovery.error.value && !state" :error="discovery.error.value" title="Could not load discovery" retryable @retry="refresh" />

        <template v-if="!state && discovery.loading.value">
          <div class="grid gap-4 lg:grid-cols-2">
            <USkeleton class="h-48" />
            <USkeleton class="h-48" />
          </div>
          <USkeleton class="h-40" />
        </template>

        <template v-if="state">
          <UAlert
            v-if="!state.enabled"
            color="warning"
            variant="subtle"
            icon="i-lucide-radar"
            title="Discovery is disabled on this server"
            description="Enable it in the server settings (discovery.enabled) to advertise this server and find peers automatically. Probes and scans still work."
          />
          <CommonErrorAlert v-if="discovery.error.value" :error="discovery.error.value" title="Refresh failed; showing the last known state" retryable @retry="refresh" />

          <DiscoveryConfigSummary :state="state" />

          <section aria-labelledby="peers-heading" class="space-y-3">
            <div class="flex flex-wrap items-baseline justify-between gap-2">
              <h2 id="peers-heading" class="text-base font-semibold text-highlighted">
                Peers <span class="text-sm font-normal text-muted">({{ peers.length }})</span>
              </h2>
              <p v-if="lastLoaded" class="text-xs text-muted">
                Updated <CommonRelativeTime :value="lastLoaded" /><template v-if="autoRefresh">
                  · refreshing every {{ AUTO_REFRESH_MS / 1000 }} s
                </template>
              </p>
            </div>
            <DiscoveryPeersTable
              :peers="peers"
              :loading="discovery.loading.value"
              :empty-text="state.enabled ? 'No peers yet. Other gos3 servers appear here when they are found via mDNS, beacon, seeds, DNS SRV or scans.' : 'No peers: discovery is disabled.'"
              @add="addPeer"
            />
          </section>

          <div class="grid gap-6 xl:grid-cols-5">
            <UCard class="xl:col-span-2" :ui="{ body: 'space-y-3' }">
              <template #header>
                <h2 class="text-sm font-semibold text-highlighted">
                  Probe a URL
                </h2>
                <p class="text-xs text-muted">
                  {{ connection?.name }} fetches /_gos3/info from the URL and adds the server to its peer table.
                </p>
              </template>
              <UAlert
                v-if="!probeCapable"
                color="neutral"
                variant="subtle"
                icon="i-lucide-lock"
                title="Not allowed"
                description="Your identity lacks admin:ProbeNetwork. Reading the peer table (admin:GetDiscovery) does not include probing URLs; an administrator can grant it with a custom policy or the admin policy."
              />
              <DiscoveryProbeForm v-else :probe="probe" id-prefix="server-probe" @add="addPeer" />
            </UCard>

            <UCard class="xl:col-span-3" :ui="{ body: 'space-y-4' }">
              <template #header>
                <h2 class="text-sm font-semibold text-highlighted">
                  Scan the network
                </h2>
                <p class="text-xs text-muted">
                  Runs on {{ connection?.name }}: probes every target × port × scheme for /_gos3/info.
                </p>
              </template>
              <UAlert
                v-if="!scanAllowed"
                color="neutral"
                variant="subtle"
                icon="i-lucide-ban"
                title="Scans are disabled on this server"
                description="An administrator can allow them with discovery.allowScan."
              />
              <UAlert
                v-else-if="!scanCapable"
                color="neutral"
                variant="subtle"
                icon="i-lucide-lock"
                title="Not allowed"
                description="Your identity lacks admin:ScanNetwork."
              />
              <template v-else>
                <DiscoveryScanForm
                  id-prefix="server-scan"
                  :running="scanning"
                  :max-hosts="state.config.maxScanHosts || 1024"
                  :placeholder="scanPlaceholder"
                  @submit="runScan"
                  @cancel="cancelScan"
                />
                <CommonErrorAlert v-if="scanError" :error="scanError" title="Scan failed" />
                <div v-if="scanResult" class="space-y-2" aria-live="polite">
                  <p class="text-sm text-muted">
                    {{ scanResult.found?.length ?? 0 }} found · {{ formatNumber(scanResult.probed) }} URLs probed · {{ scanResult.elapsed }}
                  </p>
                  <DiscoveryPeersTable v-if="scanResult.found?.length" :peers="scanResult.found" @add="addPeer" />
                </div>
              </template>
            </UCard>
          </div>
        </template>
      </template>
    </div>

    <ServersFormModal v-model:open="addOpen" :prefill="addPrefill" :credentials-from="connection" />
  </div>
</template>
