<script setup lang="ts">
import type { Settings } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { useConnectionsStore } from '~/stores/connections'
import { identityPending, settingsPageMode } from '~/components/admin/-capabilities'
import {
  COMPRESSION_MODES,
  dirtySections,
  emptySettingsForm,
  formToSettings,
  isFormDirty,
  LOG_FORMATS,
  SETTINGS_LIMITS,
  LOG_LEVELS,
  resolveSettingsSection,
  sectionOf,
  settingsSections,
  settingsToForm,
  validateSettingsForm,
  type SettingsForm,
  type SettingsSection
} from '~/components/admin/-settings-form'
import type { SectionChip } from '~/components/common/SectionChips.vue'
import { COMPRESSION_MODE_TEXT } from '~/components/bucket-settings/-compression'

/**
 * Runtime server settings (GET/PUT /config): general, CORS, discovery,
 * logging, lifecycle worker and the compression policy (hidden for servers
 * that do not report one). One section is shown at a time, picked with the
 * chips under the header (synced with `?tab=`); a single form and save bar
 * cover all of them, and the chips mark sections with unsaved changes or
 * invalid fields. Durations are edited in seconds (the API uses
 * nanoseconds). Dirty tracking, client validation, server
 * InvalidSettings messages and a leave-page guard.
 */
useHead({ title: 'Server settings' })

const route = useRoute()
const router = useRouter()
const id = computed(() => routeParam(route.params.id))
const store = useConnectionsStore()
const { connection, requireAdmin } = useConnection()
const { capabilities, status } = useWhoAmI()
const confirm = useConfirm()
const toast = useToast()

const mode = computed(() => settingsPageMode(connection.value?.kind, capabilities.value))
const pending = computed(() => identityPending(connection.value?.kind, status.value))
const readonly = computed(() => mode.value !== 'manage')

const loaded = ref<Settings | null>(null)
const original = ref<SettingsForm | null>(null)
const form = ref<SettingsForm>(emptySettingsForm())
/** True once the settings were loaded (form holds real values). */
const ready = ref(false)
const loading = ref(false)
const loadError = ref<unknown>(null)

async function load() {
  loading.value = true
  loadError.value = null
  try {
    const s = await requireAdmin().getConfig()
    loaded.value = s
    original.value = settingsToForm(s)
    form.value = settingsToForm(s)
    ready.value = true
    submitted.value = false
    serverError.value = null
  } catch (e) {
    loadError.value = e
  } finally {
    loading.value = false
  }
}

watch([mode, id], ([m]) => {
  if (m !== 'unavailable') void load()
}, { immediate: true })

const submitted = ref(false)
const saving = ref(false)
const serverError = ref<unknown>(null)

const dirty = computed(() => ready.value && isFormDirty(form.value, original.value))
const errors = computed(() => (ready.value ? validateSettingsForm(form.value) : {}))
const errorCount = computed(() => Object.keys(errors.value).length)
/** List editors validate as items are added; other fields after the first save attempt. */
const LIVE_FIELDS = new Set(['domains', 'cors.allowedOrigins', 'cors.allowedHeaders', 'cors.exposeHeaders', 'discovery.seeds', 'discovery.dnsSrv', 'compression.skipContentTypes'])
function err(path: string): string | undefined {
  return submitted.value || LIVE_FIELDS.has(path) ? errors.value[path] : undefined
}
/** Field paths whose error is currently shown (see err). */
const visibleErrors = computed(() => Object.keys(errors.value).filter(k => submitted.value || LIVE_FIELDS.has(k)))
function sectionErrors(section: SettingsSection): number {
  return visibleErrors.value.filter(k => sectionOf(k) === section).length
}

// --- sections ----------------------------------------------------------------
const sections = computed(() => settingsSections(ready.value ? form.value : null))
const section = computed<SettingsSection>({
  get: () => resolveSettingsSection(route.query.tab, sections.value),
  set: (v) => {
    void router.replace({ query: { ...route.query, tab: v === 'general' ? undefined : v } })
  }
})
const changedSections = computed(() => (ready.value ? dirtySections(form.value, original.value) : new Set<SettingsSection>()))
const chipItems = computed<SectionChip<SettingsSection>[]>(() => sections.value.map(s => ({
  ...s,
  issues: sectionErrors(s.value),
  changed: changedSections.value.has(s.value)
})))
const sectionLabel = computed(() => sections.value.find(s => s.value === section.value)?.label)

const serverErrorIsValidation = computed(() => serverError.value instanceof AdminError && serverError.value.code === 'InvalidSettings')

async function save() {
  submitted.value = true
  serverError.value = null
  if (!ready.value || errorCount.value) {
    // Show the first section with a problem (the current one if it has any).
    const bad = sections.value.map(s => s.value).filter(s => sectionErrors(s) > 0)
    if (bad.length && !bad.includes(section.value)) section.value = bad[0]!
    toast.add({ title: 'Please fix the highlighted fields', color: 'warning', icon: 'i-lucide-triangle-alert' })
    return
  }
  saving.value = true
  try {
    const updated = await requireAdmin().putConfig(formToSettings(form.value, loaded.value ?? undefined))
    loaded.value = updated
    original.value = settingsToForm(updated)
    form.value = settingsToForm(updated)
    submitted.value = false
    toast.add({ title: 'Settings saved', description: 'Changes take effect immediately.', color: 'success', icon: 'i-lucide-circle-check' })
    // Name / region / endpoints may have changed.
    void store.refreshStatus(id.value)
  } catch (e) {
    serverError.value = e
    toast.add({ title: 'Could not save settings', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
  } finally {
    saving.value = false
  }
}

async function reset() {
  if (!loaded.value) return
  if (dirty.value && !(await confirm({ title: 'Discard changes?', description: 'All unsaved changes will be lost.', confirmLabel: 'Discard', color: 'warning' }))) return
  form.value = settingsToForm(loaded.value)
  submitted.value = false
  serverError.value = null
}

onBeforeRouteLeave(async () => {
  if (!dirty.value) return true
  return confirm({ title: 'Leave without saving?', description: 'You have unsaved changes to the server settings.', confirmLabel: 'Leave', color: 'warning' })
})

function onBeforeUnload(e: BeforeUnloadEvent) {
  if (dirty.value) e.preventDefault()
}
onMounted(() => window.addEventListener('beforeunload', onBeforeUnload))
onBeforeUnmount(() => window.removeEventListener('beforeunload', onBeforeUnload))

const levelItems: { label: string, value: string }[] = LOG_LEVELS.map(l => ({ label: l, value: l }))
const formatItems: { label: string, value: string }[] = LOG_FORMATS.map(f => ({ label: f, value: f }))
const compressionModeItems: { label: string, description: string, value: string }[] = COMPRESSION_MODES.map(m => ({ value: m, ...COMPRESSION_MODE_TEXT[m] }))
</script>

<template>
  <div class="space-y-6 pb-20">
    <CommonPageHeader title="Server settings" icon="i-lucide-settings" description="Runtime settings stored by the server (settings.json). Changes apply without a restart.">
      <template v-if="ready" #badge>
        <UBadge v-if="dirty" label="Unsaved changes" color="warning" variant="subtle" size="sm" />
        <UBadge v-if="readonly" label="Read-only" color="neutral" variant="subtle" size="sm" icon="i-lucide-lock" />
      </template>
      <template v-if="mode !== 'unavailable'" #actions>
        <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" label="Reload" :loading="loading" :disabled="saving" @click="dirty ? reset() : load()" />
      </template>
    </CommonPageHeader>

    <div v-if="pending" class="space-y-3" aria-busy="true">
      <USkeleton class="h-10 w-full" />
      <USkeleton class="h-40 w-full" />
    </div>
    <CommonEmptyState
      v-else-if="mode === 'unavailable'"
      icon="i-lucide-lock"
      title="Not available"
      :description="connection?.kind === 'gos3' ? 'Your identity has no admin permissions on this server.' : 'Server settings are only available on gos3 servers.'"
    />
    <CommonErrorAlert v-else-if="loadError" :error="loadError" title="Could not load settings" retryable @retry="load" />
    <div v-else-if="!ready" class="space-y-4">
      <USkeleton class="h-9 w-full max-w-xl" />
      <USkeleton v-for="n in 2" :key="n" class="h-40 w-full" />
    </div>

    <template v-else>
      <CommonSectionChips v-model="section" :items="chipItems" label="Settings sections" panel-id="settings-panel" />

      <form class="space-y-6" novalidate @submit.prevent="save">
        <UAlert
          v-if="readonly"
          color="neutral"
          variant="subtle"
          icon="i-lucide-eye"
          title="Read-only"
          description="Changing settings requires the admin:UpdateConfig permission."
        />
        <UAlert
          v-if="serverError"
          color="error"
          variant="subtle"
          icon="i-lucide-circle-alert"
          :title="serverErrorIsValidation ? 'The server rejected these settings' : 'Could not save settings'"
          :description="errorMessage(serverError)"
          role="alert"
        />

        <!-- Every section stays mounted (v-show) so list input not yet added survives a switch. -->
        <div id="settings-panel" role="tabpanel" :aria-label="sectionLabel">
          <fieldset :disabled="readonly" class="min-w-0">
            <!-- General -->
            <UCard v-show="section === 'general'">
              <template #header>
                <h2 class="font-semibold text-highlighted">
                  General
                </h2>
                <p class="text-sm text-muted">
                  How this server presents itself and what it serves without authentication.
                </p>
              </template>
              <div class="grid gap-4 md:grid-cols-2">
                <UFormField label="Server name" name="name" required help="Shown in the UI and in discovery." :error="err('name')">
                  <UInput v-model="form.name" class="w-full" :disabled="readonly" />
                </UFormField>
                <UFormField label="Region" name="region" required help="Reported region (GetBucketLocation, discovery)." :error="err('region')">
                  <UInput v-model="form.region" class="w-full font-mono" :disabled="readonly" />
                </UFormField>
                <UFormField label="Public URL" name="publicUrl" help="Base URL clients use, if different from the listen address (e.g. behind a proxy)." :error="err('publicUrl')" class="md:col-span-2">
                  <UInput v-model="form.publicUrl" class="w-full font-mono" placeholder="https://s3.example.com" :disabled="readonly" />
                </UFormField>
                <UFormField label="Domains" name="domains" help="Base domains for virtual-hosted-style requests (bucket.domain)." :error="err('domains')" class="md:col-span-2">
                  <AdminListInput v-model="form.domains" placeholder="s3.example.com" :disabled="readonly" />
                </UFormField>
              </div>
              <div class="mt-4 grid gap-4 md:grid-cols-3">
                <USwitch v-model="form.publicInfo" label="Public info" description="Serve /_gos3/info without authentication (needed for discovery)." :disabled="readonly" />
                <USwitch v-model="form.uiRedirect" label="UI redirect" description="Redirect browsers from / to the web UI." :disabled="readonly" />
                <USwitch v-model="form.metricsPublic" label="Public metrics" description="Serve /_gos3/metrics without authentication." :disabled="readonly" />
              </div>
            </UCard>

            <!-- CORS -->
            <UCard v-show="section === 'cors'">
              <template #header>
                <h2 class="font-semibold text-highlighted">
                  CORS
                </h2>
                <p class="text-sm text-muted">
                  Global defaults for browsers (buckets may override with their own CORS rules). The web UI's origin must be allowed when it runs elsewhere.
                </p>
              </template>
              <div class="grid gap-4 md:grid-cols-2">
                <UFormField label="Allowed origins" name="cors.allowedOrigins" help="“*” or origins like https://app.example.com" :error="err('cors.allowedOrigins')">
                  <AdminListInput v-model="form.cors.allowedOrigins" placeholder="https://app.example.com" :disabled="readonly" />
                </UFormField>
                <UFormField label="Allowed headers" name="cors.allowedHeaders" help="Request headers browsers may send (“*” = any)." :error="err('cors.allowedHeaders')">
                  <AdminListInput v-model="form.cors.allowedHeaders" placeholder="*" :disabled="readonly" />
                </UFormField>
                <UFormField label="Exposed headers" name="cors.exposeHeaders" help="Response headers scripts may read (“*” = all). Uploads need ETag." :error="err('cors.exposeHeaders')">
                  <AdminListInput v-model="form.cors.exposeHeaders" placeholder="ETag" :disabled="readonly" />
                </UFormField>
                <UFormField label="Max age (seconds)" name="cors.maxAgeSeconds" help="How long browsers cache preflight responses." :error="err('cors.maxAgeSeconds')">
                  <UInputNumber v-model="form.cors.maxAgeSeconds" :min="0" :step="60" class="w-full" :disabled="readonly" />
                </UFormField>
              </div>
              <UAlert
                v-if="form.cors.allowedOrigins.length && !form.cors.exposeHeaders.some(h => h === '*' || h.toLowerCase() === 'etag')"
                class="mt-4"
                color="warning"
                variant="subtle"
                icon="i-lucide-triangle-alert"
                title="ETag is not exposed"
                description="Browser multipart uploads (including this UI) need the ETag response header."
              />
            </UCard>

            <!-- Discovery -->
            <UCard v-show="section === 'discovery'">
              <template #header>
                <h2 class="font-semibold text-highlighted">
                  Discovery
                </h2>
                <p class="text-sm text-muted">
                  How this server finds and announces other gos3 servers.
                </p>
              </template>
              <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
                <USwitch v-model="form.discovery.enabled" label="Enabled" description="Run discovery at all." :disabled="readonly" />
                <USwitch v-model="form.discovery.advertise" label="Advertise" description="Announce this server." :disabled="readonly || !form.discovery.enabled" />
                <USwitch v-model="form.discovery.mdns" label="mDNS" description="_gos3._tcp on the local network." :disabled="readonly || !form.discovery.enabled" />
                <USwitch v-model="form.discovery.beacon" label="UDP beacon" description="Multicast announcements." :disabled="readonly || !form.discovery.enabled" />
              </div>
              <div class="mt-4 grid gap-4 md:grid-cols-3">
                <UFormField label="Beacon group" name="discovery.beaconGroup" help="Multicast address:port" :error="err('discovery.beaconGroup')">
                  <UInput v-model="form.discovery.beaconGroup" class="w-full font-mono" placeholder="239.255.73.83:7373" :disabled="readonly || !form.discovery.beacon" />
                </UFormField>
                <UFormField label="Beacon interval (seconds)" name="discovery.beaconIntervalSeconds" :error="err('discovery.beaconIntervalSeconds')" :help="`= ${formatDuration(secondsToNs(form.discovery.beaconIntervalSeconds || 0))}`">
                  <UInputNumber v-model="form.discovery.beaconIntervalSeconds" :min="SETTINGS_LIMITS.minBeaconIntervalSeconds" :max="SETTINGS_LIMITS.maxBeaconIntervalSeconds" :step="1" :format-options="{ maximumFractionDigits: 3 }" class="w-full" :disabled="readonly || !form.discovery.beacon" />
                </UFormField>
                <UFormField label="Peer TTL (seconds)" name="discovery.peerTtlSeconds" help="Peers not seen for this long are dropped." :error="err('discovery.peerTtlSeconds')">
                  <UInputNumber v-model="form.discovery.peerTtlSeconds" :min="1" :step="10" :format-options="{ maximumFractionDigits: 3 }" class="w-full" :disabled="readonly" />
                </UFormField>
                <UFormField label="Static seeds" name="discovery.seeds" help="Servers to probe directly: http://host:9000 or host:9000" :error="err('discovery.seeds')" class="md:col-span-3">
                  <AdminListInput v-model="form.discovery.seeds" placeholder="http://10.0.0.5:9000" :disabled="readonly" />
                </UFormField>
                <UFormField label="DNS SRV domains" name="discovery.dnsSrv" help="Domains whose _gos3._tcp.<domain> SRV records list servers, e.g. example.com" :error="err('discovery.dnsSrv')" class="md:col-span-3">
                  <AdminListInput v-model="form.discovery.dnsSrv" placeholder="example.com" :disabled="readonly" />
                </UFormField>
                <USwitch v-model="form.discovery.allowScan" label="Allow network scans" description="Let admins scan address ranges from this server." :disabled="readonly" />
                <UFormField label="Max hosts per scan" name="discovery.maxScanHosts" :error="err('discovery.maxScanHosts')">
                  <UInputNumber v-model="form.discovery.maxScanHosts" :min="1" :max="SETTINGS_LIMITS.maxScanHosts" :step="256" class="w-full" :disabled="readonly || !form.discovery.allowScan" />
                </UFormField>
              </div>
            </UCard>

            <!-- Logging -->
            <UCard v-show="section === 'logging'">
              <template #header>
                <h2 class="font-semibold text-highlighted">
                  Logging
                </h2>
              </template>
              <div class="grid gap-4 sm:grid-cols-2">
                <UFormField label="Level" name="logging.level" :error="err('logging.level')">
                  <USelect v-model="form.logging.level" :items="levelItems" class="w-full" :disabled="readonly" />
                </UFormField>
                <UFormField label="Format" name="logging.format" :error="err('logging.format')">
                  <USelect v-model="form.logging.format" :items="formatItems" class="w-full" :disabled="readonly" />
                </UFormField>
                <USwitch v-model="form.logging.accessLog" label="Access log" description="Log every request." class="sm:col-span-2" :disabled="readonly" />
              </div>
            </UCard>

            <!-- Lifecycle -->
            <UCard v-show="section === 'lifecycle'">
              <template #header>
                <h2 class="font-semibold text-highlighted">
                  Lifecycle worker
                </h2>
              </template>
              <div class="grid gap-4 sm:grid-cols-2">
                <USwitch v-model="form.lifecycle.enabled" label="Enabled" description="Apply bucket lifecycle rules (expiration, clean-up)." class="sm:col-span-2" :disabled="readonly" />
                <UFormField label="Interval (seconds)" name="lifecycle.intervalSeconds" :help="`Runs every ${formatSeconds(form.lifecycle.intervalSeconds || 0)}`" :error="err('lifecycle.intervalSeconds')">
                  <UInputNumber v-model="form.lifecycle.intervalSeconds" :min="SETTINGS_LIMITS.minLifecycleSeconds" :max="SETTINGS_LIMITS.maxLifecycleSeconds" :step="60" class="w-full" :disabled="readonly || !form.lifecycle.enabled" />
                </UFormField>
              </div>
            </UCard>

            <!-- Compression (servers that report a compression policy) -->
            <UCard v-if="form.compression" v-show="section === 'compression'">
              <template #header>
                <h2 class="font-semibold text-highlighted">
                  Compression
                </h2>
                <p class="text-sm text-muted">
                  Server-wide policy for storing new objects compressed (DEFLATE). Clients read objects back unchanged, with the same ETags, checksums and sizes. Existing objects are not rewritten.
                </p>
              </template>
              <div class="space-y-4">
                <UFormField name="compression.mode" :error="err('compression.mode')">
                  <URadioGroup
                    v-model="form.compression.mode"
                    :items="compressionModeItems"
                    legend="Mode (administrator policy for all buckets)"
                    orientation="horizontal"
                    variant="card"
                    :ui="{ fieldset: 'grid gap-2 md:grid-cols-3', item: 'w-full' }"
                    :disabled="readonly"
                  />
                </UFormField>
                <div class="grid gap-4 md:grid-cols-2">
                  <UFormField label="Level" name="compression.level" help="1 = fastest (default), 9 = smallest." :error="err('compression.level')">
                    <UInputNumber v-model="form.compression.level" :min="SETTINGS_LIMITS.minCompressionLevel" :max="SETTINGS_LIMITS.maxCompressionLevel" :step="1" class="w-full" :disabled="readonly || form.compression.mode === 'disabled'" />
                  </UFormField>
                  <UFormField label="Minimum size (bytes)" name="compression.minSize" :help="`Smaller objects are stored as is (= ${formatBytes(form.compression.minSize || 0)}).`" :error="err('compression.minSize')">
                    <UInputNumber v-model="form.compression.minSize" :min="0" :max="SETTINGS_LIMITS.maxCompressionMinSize" :step="1024" class="w-full" :disabled="readonly || form.compression.mode === 'disabled'" />
                  </UFormField>
                  <UFormField
                    label="Skipped content types"
                    name="compression.skipContentTypes"
                    help="Already compressed media stored as is: type/subtype, type/* or a prefix ending in *. Objects with a Content-Encoding are never compressed."
                    :error="err('compression.skipContentTypes')"
                    class="md:col-span-2"
                  >
                    <AdminListInput v-model="form.compression.skipContentTypes" placeholder="image/jpeg" :disabled="readonly || form.compression.mode === 'disabled'" />
                  </UFormField>
                </div>
              </div>
            </UCard>
          </fieldset>
        </div>

        <!-- Sticky save bar (saves every section at once) -->
        <div
          v-if="!readonly"
          class="sticky -bottom-4 z-10 -mx-4 flex flex-wrap items-center justify-between gap-3 border-t border-default bg-default/95 px-4 py-3 backdrop-blur sm:-bottom-6 sm:-mx-6 sm:px-6"
        >
          <p class="text-sm" :class="dirty ? 'text-warning' : 'text-muted'" aria-live="polite">
            <template v-if="submitted && errorCount">
              {{ errorCount }} field{{ errorCount === 1 ? '' : 's' }} need{{ errorCount === 1 ? 's' : '' }} attention.
            </template>
            <template v-else-if="dirty">
              You have unsaved changes.
            </template>
            <template v-else>
              All changes saved.
            </template>
          </p>
          <div class="flex gap-2">
            <UButton color="neutral" variant="ghost" icon="i-lucide-undo-2" label="Reset" :disabled="!dirty || saving" @click="reset" />
            <UButton type="submit" icon="i-lucide-save" label="Save settings" :loading="saving" :disabled="!dirty" />
          </div>
        </div>
      </form>
    </template>
  </div>
</template>
