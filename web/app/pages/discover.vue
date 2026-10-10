<script setup lang="ts">
/**
 * Discover gos3 servers from every vantage point and add them in one click:
 *   (a) the peer tables of all connected gos3 servers (GET /discovery),
 *   (b) local discovery from the UI host (standalone Node server only:
 *       mDNS + beacon + scans via /api/discovery/*; hidden in the embedded build),
 *   (c) direct probes from this browser (GET <url>/_gos3/info, CORS "*").
 * Results are deduplicated by server ID and show who found each server.
 */
import type { ConnectionInput, Connection } from '~/types/connection'
import type { ScanRequest } from '~/types/admin'
import { findConfigured, parseUrlList, probeUrlFor, type DiscoveredServer } from '~/components/discovery/-aggregate'
import { prefillFromPeer } from '~/components/servers/-form'
import { useConnectionsStore } from '~/stores/connections'

useHead({ title: 'Discover servers' })

const store = useConnectionsStore()
const toast = useToast()
const d = useDiscovery()
const local = d.local

const WAIT_OPTIONS = [
  { label: '1 second', value: 1000 },
  { label: '3 seconds', value: 3000 },
  { label: '5 seconds', value: 5000 },
  { label: '10 seconds', value: 10000 }
]
const waitMs = ref(3000)

const hideConfigured = ref(false)
const visible = computed(() => (hideConfigured.value ? d.servers.value.filter(s => !findConfigured(s, store.connections)) : d.servers.value))
const configuredCount = computed(() => d.servers.value.filter(s => findConfigured(s, store.connections)).length)
const serverSources = computed(() => d.sources.value.filter(s => s.kind === 'server'))
const gos3Count = computed(() => store.connections.filter(c => c.kind === 'gos3').length)

onMounted(() => {
  void d.refreshAll(waitMs.value)
})

// -- probes ------------------------------------------------------------------------

const probeText = ref('')
const probeVia = ref<'browser' | 'host'>('browser')
const probeError = ref<string | undefined>()

async function probe() {
  const entries = parseUrlList(probeText.value)
  if (!entries.length) {
    probeError.value = 'Enter at least one URL or host'
    return
  }
  if (entries.length > 64) {
    probeError.value = 'At most 64 entries at once; use a scan for larger ranges'
    return
  }
  probeError.value = undefined
  const r = await d.probeUrls(entries.map(e => probeUrlFor(e)), probeVia.value)
  toast.add({
    title: r.found.length ? `${r.found.length} gos3 server${r.found.length === 1 ? '' : 's'} found` : 'No gos3 server found',
    description: r.failed.length ? `${r.failed.length} URL${r.failed.length === 1 ? '' : 's'} did not answer as gos3.` : undefined,
    color: r.found.length ? 'success' : 'warning',
    icon: r.found.length ? 'i-lucide-badge-check' : 'i-lucide-triangle-alert'
  })
}

// -- local scan ----------------------------------------------------------------------

const scanning = ref(false)
const scanError = ref<unknown>(null)
let scanCtrl: AbortController | null = null

async function runScan(req: ScanRequest) {
  scanCtrl?.abort()
  scanCtrl = new AbortController()
  scanning.value = true
  scanError.value = null
  try {
    const r = await d.scanLocal(req, scanCtrl.signal)
    const n = r.found?.length ?? 0
    toast.add({ title: 'Scan finished', description: `${n} server${n === 1 ? '' : 's'} found, ${r.probed} URLs probed in ${r.elapsed}.`, color: 'success', icon: 'i-lucide-scan-search' })
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

// -- add -------------------------------------------------------------------------------

const addOpen = ref(false)
const addPrefill = ref<ConnectionInput | undefined>()
const addCredentialsFrom = ref<Connection | undefined>()

function addServer(s: DiscoveredServer) {
  const prefill = prefillFromPeer(s)
  if (!prefill) {
    toast.add({ title: 'No usable address', description: 'This server did not advertise a URL.', color: 'warning', icon: 'i-lucide-triangle-alert' })
    return
  }
  addPrefill.value = prefill
  // Offer the credentials of a connected server that reported this peer.
  addCredentialsFrom.value = s.foundBy
    .map(f => (f.connectionId ? store.byId(f.connectionId) : undefined))
    .find(c => !!c && hasCredentials(c))
  addOpen.value = true
}

const STATE_ICONS = {
  idle: 'i-lucide-circle-dashed',
  loading: 'i-lucide-loader-circle',
  ok: 'i-lucide-circle-check',
  error: 'i-lucide-circle-x',
  skipped: 'i-lucide-circle-minus'
} as const
const STATE_CLASSES = {
  idle: 'text-dimmed',
  loading: 'animate-spin text-info',
  ok: 'text-success',
  error: 'text-error',
  skipped: 'text-dimmed'
} as const
</script>

<template>
  <div>
    <CommonPageHeader
      title="Discover servers"
      icon="i-lucide-scan-search"
      description="Find gos3 servers through your connected servers, the UI host's local network and direct probes — then add them with one click."
    >
      <template #actions>
        <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" label="Refresh" :loading="d.loading.value" @click="d.refreshAll(waitMs)" />
      </template>
    </CommonPageHeader>

    <div class="space-y-6">
      <div class="grid gap-6 lg:grid-cols-2">
        <!-- Sources -->
        <UCard :ui="{ body: 'space-y-4' }">
          <template #header>
            <h2 class="text-sm font-semibold text-highlighted">
              Sources
            </h2>
          </template>

          <section aria-labelledby="src-servers" class="space-y-2">
            <h3 id="src-servers" class="flex items-center gap-2 text-sm font-medium">
              <UIcon name="i-lucide-server" class="size-4 text-muted" aria-hidden="true" /> Connected gos3 servers
            </h3>
            <p v-if="!gos3Count" class="text-sm text-muted">
              No gos3 servers configured. Their peer tables (mDNS, beacon, seeds, DNS SRV, scans) appear here once you
              <NuxtLink :to="appRoutes.servers()" class="text-primary hover:underline">add one</NuxtLink>.
            </p>
            <ul v-else class="space-y-1.5">
              <li v-for="s in serverSources" :key="s.key" class="flex items-start gap-2 text-sm">
                <UIcon :name="STATE_ICONS[s.state]" class="mt-0.5 size-4 shrink-0" :class="STATE_CLASSES[s.state]" aria-hidden="true" />
                <div class="min-w-0 flex-1">
                  <span class="font-medium">{{ s.label }}</span>
                  <span class="sr-only">: {{ s.state }}</span>
                  <span v-if="s.message" class="text-muted"> — {{ s.message }}</span>
                </div>
                <UButton
                  v-if="s.connectionId && s.state === 'skipped' && !hasCredentials(store.byId(s.connectionId))"
                  size="xs"
                  variant="link"
                  label="Sign in"
                  :to="appRoutes.connect(s.connectionId, appRoutes.discover())"
                />
                <UButton
                  v-else-if="s.connectionId && s.state === 'ok'"
                  size="xs"
                  variant="link"
                  color="neutral"
                  label="Details"
                  :to="appRoutes.discovery(s.connectionId)"
                />
              </li>
            </ul>
          </section>

          <USeparator />

          <section aria-labelledby="src-local" class="space-y-2">
            <h3 id="src-local" class="flex items-center gap-2 text-sm font-medium">
              <UIcon name="i-lucide-monitor" class="size-4 text-muted" aria-hidden="true" /> Local network of the UI host
            </h3>
            <p v-if="local.embedded.value" class="text-sm text-muted">
              Not available in the embedded console. The gos3 server hosting it discovers its neighbours itself — see its Discovery page.
            </p>
            <p v-else-if="local.checking.value || !local.checked.value" class="flex items-center gap-2 text-sm text-muted">
              <UIcon name="i-lucide-loader-circle" class="size-4 animate-spin" aria-hidden="true" /> Checking whether the UI server supports local discovery…
            </p>
            <p v-else-if="local.capabilities.value && !local.capabilities.value.localDiscovery" class="text-sm text-muted">
              Turned off on the UI server (<code class="font-mono text-xs">GOS3_UI_LOCAL_DISCOVERY</code>). Probes from this browser still work.
            </p>
            <p v-else-if="!local.available.value" class="text-sm text-muted">
              Not available: this console is served as static files. Run it as a Node server (<code class="font-mono text-xs">node .output/server/index.mjs</code>) for mDNS / beacon discovery and scans from the UI host.
            </p>
            <template v-else>
              <div class="flex flex-wrap items-end gap-2">
                <UFormField label="Listen for" class="w-36">
                  <USelect v-model="waitMs" :items="WAIT_OPTIONS" value-key="value" class="w-full" />
                </UFormField>
                <UButton
                  icon="i-lucide-radar"
                  label="Search local network"
                  :loading="d.sources.value.some(s => s.key === 'local' && s.state === 'loading')"
                  @click="d.refreshLocal(waitMs)"
                />
              </div>
              <p class="text-xs text-muted">
                mDNS <UBadge :label="local.capabilities.value?.mdns ? 'on' : 'unavailable'" :color="local.capabilities.value?.mdns ? 'success' : 'neutral'" variant="subtle" size="sm" />
                · beacon <UBadge :label="local.capabilities.value?.beacon ? 'on' : 'unavailable'" :color="local.capabilities.value?.beacon ? 'success' : 'neutral'" variant="subtle" size="sm" />
              </p>
              <ul class="space-y-1.5">
                <li v-for="s in d.sources.value.filter(x => x.kind === 'local')" :key="s.key" class="text-sm">
                  <div class="flex items-start gap-2">
                    <UIcon :name="STATE_ICONS[s.state]" class="mt-0.5 size-4 shrink-0" :class="STATE_CLASSES[s.state]" aria-hidden="true" />
                    <span><span class="font-medium">{{ s.label }}</span><span v-if="s.message" class="text-muted"> — {{ s.message }}</span></span>
                  </div>
                  <ul v-if="s.warnings?.length" class="ml-6 mt-1 list-disc space-y-0.5 pl-4 text-xs text-warning">
                    <li v-for="w in s.warnings" :key="w">
                      {{ w }}
                    </li>
                  </ul>
                </li>
              </ul>
            </template>
          </section>
        </UCard>

        <!-- Direct probes -->
        <UCard :ui="{ body: 'space-y-3' }">
          <template #header>
            <h2 class="text-sm font-semibold text-highlighted">
              Probe URLs
            </h2>
            <p class="text-xs text-muted">
              Checks <code class="font-mono">/_gos3/info</code> on each URL or host (port 9000 unless given).
            </p>
          </template>
          <form class="space-y-3" novalidate @submit.prevent="probe">
            <UFormField label="URLs or hosts" :error="probeError" help="One per line, e.g. http://10.0.0.5:9000, nas.local, 192.168.1.20:9001">
              <UTextarea v-model="probeText" :rows="4" autoresize :maxrows="10" class="w-full font-mono" placeholder="http://10.0.0.5:9000&#10;nas.local" spellcheck="false" />
            </UFormField>
            <URadioGroup
              v-if="local.available.value"
              v-model="probeVia"
              legend="Probe from"
              orientation="horizontal"
              :items="[{ label: 'This browser', value: 'browser' }, { label: 'UI host', value: 'host' }]"
              value-key="value"
            />
            <div class="flex flex-wrap items-center gap-2">
              <UButton type="submit" icon="i-lucide-radio-tower" label="Probe" :loading="d.probing.value" />
              <UButton v-if="d.probeFailures.value.length || d.sources.value.some(s => s.key === 'browser')" color="neutral" variant="ghost" label="Clear probe results" @click="d.clearProbes()" />
            </div>
          </form>
          <div v-if="d.probeFailures.value.length" class="space-y-1" aria-live="polite">
            <p class="text-xs font-medium text-muted">
              Not found
            </p>
            <ul class="space-y-1 text-xs">
              <li v-for="f in d.probeFailures.value" :key="f.url" class="flex flex-wrap gap-x-2">
                <code class="font-mono">{{ f.url }}</code>
                <span class="text-error">{{ errorMessage(f.error) }}</span>
              </li>
            </ul>
          </div>
        </UCard>
      </div>

      <!-- Local scan -->
      <UCard v-if="local.available.value && local.capabilities.value?.scan" :ui="{ body: 'space-y-4' }">
        <template #header>
          <h2 class="text-sm font-semibold text-highlighted">
            Scan from the UI host
          </h2>
          <p class="text-xs text-muted">
            The console's Node server probes every target × port × scheme for <code class="font-mono">/_gos3/info</code>.
          </p>
        </template>
        <DiscoveryScanForm id-prefix="local-scan" :running="scanning" :max-hosts="local.capabilities.value?.maxScanHosts ?? 1024" @submit="runScan" @cancel="cancelScan" />
        <CommonErrorAlert v-if="scanError" :error="scanError" title="Scan failed" />
      </UCard>

      <!-- Results -->
      <section aria-labelledby="results-heading" class="space-y-3">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h2 id="results-heading" class="text-base font-semibold text-highlighted">
            Servers found <span class="text-sm font-normal text-muted">({{ d.servers.value.length }})</span>
          </h2>
          <USwitch v-if="configuredCount" v-model="hideConfigured" :label="`Hide configured (${configuredCount})`" size="sm" />
        </div>
        <CommonEmptyState
          v-if="!d.servers.value.length && !d.loading.value"
          icon="i-lucide-radar"
          title="No gos3 servers found yet"
          :description="gos3Count ? 'Your connected servers have no peers. Probe a URL above, or enable discovery on your servers.' : 'Connect a gos3 server to use its peer table, or probe a URL above.'"
          compact
        />
        <DiscoveryServersTable v-else :servers="visible" :loading="d.loading.value" @add="addServer" />
      </section>
    </div>

    <ServersFormModal v-model:open="addOpen" :prefill="addPrefill" :credentials-from="addCredentialsFrom" />
  </div>
</template>
