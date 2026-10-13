<script setup lang="ts">
import type { Stats, StorageStats } from '~/types/admin'
import { overviewMode } from '~/components/admin/-capabilities'
import { fillMinutes, type TrafficSeries } from '~/components/admin/-metrics'

/**
 * Server overview: identity, storage totals, request metrics with a
 * 60-minute traffic chart, per-operation table, runtime and disk stats
 * (gos3 with admin:GetStats; auto-refresh every 10 s). Generic S3 servers
 * get a bucket summary instead.
 */
const REFRESH_MS = 10_000

const route = useRoute()
const id = computed(() => routeParam(route.params.id))
const { connection, requireS3, requireAdmin, status } = useConnection()
const { capabilities, whoami, loading: whoamiLoading } = useWhoAmI()
const { info, serverInfo, error: infoError, refresh: refreshInfo } = useServerInfo()

useHead({ title: () => `${connection.value?.name ?? 'Server'} · Overview` })

const mode = computed(() => overviewMode(connection.value?.kind, capabilities.value))

// --- stats (gos3 + admin:GetStats) -----------------------------------------
const stats = ref<Stats | null>(null)
const statsError = ref<unknown>(null)
const statsLoading = ref(false)
const lastUpdated = ref<number | null>(null)
const paused = ref(false)
let controller: AbortController | undefined

async function loadStats() {
  if (mode.value !== 'stats') return
  controller?.abort()
  const ctrl = new AbortController()
  controller = ctrl
  statsLoading.value = true
  try {
    stats.value = await requireAdmin().stats(ctrl.signal)
    statsError.value = null
    lastUpdated.value = Date.now()
  } catch (e) {
    if (!isAbortedError(e)) statsError.value = e
  } finally {
    if (controller === ctrl) statsLoading.value = false
  }
}

let timer: ReturnType<typeof setInterval> | undefined
function tick() {
  if (paused.value || mode.value !== 'stats') return
  if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return
  void loadStats()
  if (!serverInfo.value && capabilities.value.viewInfo) void refreshInfo()
}
onMounted(() => {
  timer = setInterval(tick, REFRESH_MS)
})
onBeforeUnmount(() => {
  clearInterval(timer)
  controller?.abort()
})

// --- bucket summary (generic S3, or gos3 without stats) --------------------
const buckets = useLoader(() => requireS3().listBuckets(), { immediate: false })
const bucketList = computed(() => buckets.data.value?.buckets ?? [])
const recentBuckets = computed(() =>
  [...bucketList.value].sort((a, b) => (b.creationDate ?? '').localeCompare(a.creationDate ?? '')).slice(0, 8)
)

watch([mode, id], ([m]) => {
  stats.value = null
  statsError.value = null
  if (m === 'stats') void loadStats()
  else void buckets.refresh()
}, { immediate: true })

function refreshAll() {
  if (mode.value === 'stats') void loadStats()
  else void buckets.refresh()
  void refreshInfo()
}

// --- derived ----------------------------------------------------------------
const metrics = computed(() => stats.value?.metrics ?? null)
const minutes = computed(() => fillMinutes(metrics.value?.recent))
const totalErrors = computed(() => (metrics.value ? metrics.value.errors4xx + metrics.value.errors5xx : 0))
const requestSeries: TrafficSeries[] = [
  { key: 'requests', label: 'Requests', color: 'primary' },
  { key: 'errors', label: 'Errors', color: 'error' }
]
const bytesSeries: TrafficSeries[] = [
  { key: 'bytesIn', label: 'In', color: 'info' },
  { key: 'bytesOut', label: 'Out', color: 'warning' }
]
const lastHour = computed(() => minutes.value.reduce((a, m) => ({ requests: a.requests + m.requests, errors: a.errors + m.errors }), { requests: 0, errors: 0 }))

const uptimeSeconds = computed(() => {
  if (serverInfo.value) return serverInfo.value.uptimeSeconds
  if (metrics.value) return metrics.value.uptimeSeconds
  const started = info.value?.startedAt ? Date.parse(info.value.startedAt) : NaN
  return Number.isNaN(started) ? undefined : Math.max(0, (Date.now() - started) / 1000)
})

const identityRows = computed(() => {
  const i = info.value
  if (!i) return []
  const rows: { label: string, value: string, mono?: boolean, copy?: boolean }[] = [
    { label: 'Name', value: i.name },
    { label: 'Version', value: i.version },
    { label: 'Region', value: i.region },
    { label: 'Server ID', value: i.id, mono: true, copy: true },
    { label: 'Uptime', value: formatSeconds(uptimeSeconds.value) }
  ]
  const s = serverInfo.value
  if (s) {
    rows.push({ label: 'Backend', value: s.dataPath ? `${s.backend} (${s.dataPath})` : s.backend })
    rows.push({ label: 'Runtime', value: `${s.goVersion} · ${s.os}/${s.arch} · ${s.numCPU} CPU` })
  }
  return rows
})

const endpoints = computed(() => {
  const e = info.value?.endpoints
  if (!e) return []
  const out = [{ label: 'S3', url: e.s3 }, { label: 'Admin API', url: e.admin }]
  if (e.ui) out.push({ label: 'Web UI', url: e.ui })
  for (const extra of e.all ?? []) if (extra !== e.s3) out.push({ label: 'Also at', url: extra })
  return out
})

/** "… incl. versions", after the compression saving of the current versions when there is one. */
function storedHint(s: StorageStats): string {
  const note = formatStoredNote(s.bytes, s.storedBytes)
  return `${note ? `${note} · ` : ''}${formatBytes(s.versionBytes)} incl. versions`
}

const storageCards = computed(() => {
  const s = stats.value?.storage
  if (!s) return []
  return [
    { label: 'Buckets', value: formatNumber(s.buckets), icon: 'i-lucide-database' },
    { label: 'Objects', value: formatNumber(s.objects), icon: 'i-lucide-files', hint: `${formatNumber(s.versions)} versions` },
    { label: 'Stored', value: formatBytes(s.bytes), icon: 'i-lucide-hard-drive', hint: storedHint(s) },
    { label: 'Delete markers', value: formatNumber(s.deleteMarkers), icon: 'i-lucide-eraser', hint: `${formatNumber(s.pendingUploads)} pending uploads` }
  ]
})
</script>

<template>
  <div class="space-y-6">
    <CommonPageHeader
      :title="info?.name ?? connection?.name ?? 'Overview'"
      icon="i-lucide-layout-dashboard"
      :description="mode === 's3' ? `S3-compatible service at ${connection?.endpoint}` : connection?.endpoint"
    >
      <template #badge>
        <UBadge v-if="mode !== 's3'" label="gos3" color="primary" variant="subtle" size="sm" />
        <UBadge v-else label="S3" color="neutral" variant="subtle" size="sm" />
        <CommonStatusDot v-if="status" :state="status.state" show-label />
      </template>
      <template #actions>
        <span v-if="mode === 'stats' && lastUpdated" class="text-xs text-muted" aria-live="off">
          Updated <CommonRelativeTime :value="lastUpdated" />
        </span>
        <UButton
          v-if="mode === 'stats'"
          color="neutral"
          variant="outline"
          :icon="paused ? 'i-lucide-play' : 'i-lucide-pause'"
          :label="paused ? 'Resume' : 'Pause'"
          :aria-pressed="paused"
          :aria-label="paused ? 'Resume auto-refresh' : 'Pause auto-refresh (every 10 seconds)'"
          @click="paused = !paused"
        />
        <UButton
          color="neutral"
          variant="outline"
          icon="i-lucide-refresh-cw"
          label="Refresh"
          :loading="statsLoading || buckets.loading.value"
          @click="refreshAll"
        />
        <UButton icon="i-lucide-database" label="Buckets" :to="appRoutes.buckets(id)" />
      </template>
    </CommonPageHeader>

    <!-- Identity (gos3) -->
    <UCard v-if="mode !== 's3'">
      <template #header>
        <div class="flex items-center justify-between gap-2">
          <h2 class="font-semibold text-highlighted">
            Server
          </h2>
          <span v-if="whoami" class="text-xs text-muted">
            Signed in as <span class="font-medium text-default">{{ whoami.isRoot ? 'root' : whoami.user }}</span>
          </span>
        </div>
      </template>
      <CommonErrorAlert v-if="infoError" :error="infoError" title="Could not load server information" retryable class="mb-4" @retry="refreshInfo" />
      <div v-if="!info && (whoamiLoading || status?.state === 'checking')" class="grid gap-3 sm:grid-cols-2">
        <USkeleton v-for="n in 6" :key="n" class="h-5 w-full" />
      </div>
      <CommonEmptyState
        v-else-if="!info"
        compact
        icon="i-lucide-server-off"
        title="Server identity unavailable"
        description="The public /_gos3/info document could not be read (the server may require authentication for it or be offline)."
      />
      <div v-else class="grid gap-6 lg:grid-cols-2">
        <dl class="grid min-w-0 grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-2 text-sm">
          <template v-for="r in identityRows" :key="r.label">
            <dt class="text-muted">
              {{ r.label }}
            </dt>
            <dd class="flex min-w-0 items-center gap-1" :class="r.mono ? 'font-mono text-xs' : ''">
              <span class="truncate">{{ r.value }}</span>
              <CommonCopyButton v-if="r.copy" :value="r.value" :aria-label="`Copy ${r.label}`" />
            </dd>
          </template>
        </dl>
        <div class="min-w-0 space-y-4">
          <div>
            <h3 class="mb-2 text-xs font-medium uppercase tracking-wide text-muted">
              Endpoints
            </h3>
            <ul class="space-y-1 text-sm">
              <li v-for="e in endpoints" :key="`${e.label}-${e.url}`" class="flex min-w-0 items-center gap-2">
                <span class="w-20 shrink-0 text-muted">{{ e.label }}</span>
                <code class="min-w-0 truncate font-mono text-xs" :title="e.url">{{ e.url }}</code>
                <CommonCopyButton :value="e.url" :aria-label="`Copy ${e.label} endpoint`" />
              </li>
            </ul>
          </div>
          <div v-if="info.features?.length">
            <h3 class="mb-2 text-xs font-medium uppercase tracking-wide text-muted">
              Features
            </h3>
            <div class="flex flex-wrap gap-1.5">
              <UBadge v-for="f in info.features" :key="f" :label="f" color="neutral" variant="subtle" size="sm" />
            </div>
          </div>
        </div>
      </div>
    </UCard>

    <!-- Metrics dashboard -->
    <template v-if="mode === 'stats'">
      <CommonErrorAlert v-if="statsError" :error="statsError" title="Could not load statistics" retryable @retry="loadStats" />

      <section aria-labelledby="storage-heading" class="space-y-3">
        <h2 id="storage-heading" class="text-sm font-semibold text-highlighted">
          Storage
          <span v-if="stats?.storage.computedAt" class="ml-2 text-xs font-normal text-muted">
            computed <CommonRelativeTime :value="stats.storage.computedAt" />
          </span>
        </h2>
        <div class="grid grid-cols-2 gap-3 xl:grid-cols-4">
          <template v-if="stats">
            <AdminStatCard v-for="c in storageCards" :key="c.label" v-bind="c" />
          </template>
          <template v-else>
            <AdminStatCard v-for="n in 4" :key="n" label="Loading…" value="" loading />
          </template>
        </div>
      </section>

      <section aria-labelledby="requests-heading" class="space-y-3">
        <h2 id="requests-heading" class="text-sm font-semibold text-highlighted">
          Requests
          <span v-if="metrics" class="ml-2 text-xs font-normal text-muted">since <CommonRelativeTime :value="metrics.startedAt" /></span>
        </h2>
        <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-5">
          <AdminStatCard label="Requests" :value="formatNumber(metrics?.requests)" icon="i-lucide-activity" :hint="`${formatNumber(lastHour.requests)} in the last hour`" :loading="!metrics" />
          <AdminStatCard
            label="Errors"
            :value="formatNumber(totalErrors)"
            icon="i-lucide-circle-alert"
            :color="metrics?.errors5xx ? 'error' : totalErrors ? 'warning' : 'default'"
            :hint="metrics ? `4xx ${formatNumber(metrics.errors4xx)} · 5xx ${formatNumber(metrics.errors5xx)}` : undefined"
            :loading="!metrics"
          />
          <AdminStatCard label="Bytes in" :value="formatBytes(metrics?.bytesIn)" icon="i-lucide-arrow-down-to-line" :loading="!metrics" />
          <AdminStatCard label="Bytes out" :value="formatBytes(metrics?.bytesOut)" icon="i-lucide-arrow-up-from-line" :loading="!metrics" />
          <AdminStatCard label="In flight" :value="formatNumber(metrics?.inFlight)" icon="i-lucide-loader" :loading="!metrics" />
        </div>
        <div v-if="metrics" class="grid gap-3 xl:grid-cols-2">
          <UCard>
            <AdminTrafficChart title="Requests per minute" :points="minutes" :series="requestSeries" />
          </UCard>
          <UCard>
            <AdminTrafficChart title="Traffic per minute" :points="minutes" :series="bytesSeries" unit="bytes" />
          </UCard>
        </div>
      </section>

      <section v-if="metrics" aria-labelledby="ops-heading" class="space-y-3">
        <h2 id="ops-heading" class="text-sm font-semibold text-highlighted">
          Operations
        </h2>
        <AdminOpsTable :ops="metrics.ops" />
      </section>

      <div v-if="stats" class="grid gap-3 lg:grid-cols-2">
        <UCard>
          <template #header>
            <h2 class="font-semibold text-highlighted">
              Runtime
            </h2>
          </template>
          <dl class="grid grid-cols-2 gap-4 text-sm">
            <div>
              <dt class="text-muted">
                Goroutines
              </dt>
              <dd class="text-lg font-semibold tabular-nums">
                {{ formatNumber(stats.runtime.goroutines) }}
              </dd>
            </div>
            <div>
              <dt class="text-muted">
                Heap in use
              </dt>
              <dd class="text-lg font-semibold tabular-nums">
                {{ formatBytes(stats.runtime.heapAllocBytes) }}
              </dd>
            </div>
            <div>
              <dt class="text-muted">
                Memory from OS
              </dt>
              <dd class="text-lg font-semibold tabular-nums">
                {{ formatBytes(stats.runtime.sysBytes) }}
              </dd>
            </div>
            <div>
              <dt class="text-muted">
                Total GC pause
              </dt>
              <dd class="text-lg font-semibold tabular-nums">
                {{ formatDuration(stats.runtime.gcPauseTotalNs) }}
              </dd>
            </div>
          </dl>
        </UCard>
        <UCard>
          <template #header>
            <h2 class="font-semibold text-highlighted">
              Disk
            </h2>
          </template>
          <AdminDiskUsage v-if="stats.disk" :disk="stats.disk" />
          <CommonEmptyState
            v-else
            compact
            icon="i-lucide-memory-stick"
            title="No disk statistics"
            :description="serverInfo?.backend === 'memory' ? 'The in-memory backend keeps everything in RAM.' : 'Disk usage is reported for the filesystem backend only.'"
          />
        </UCard>
      </div>
    </template>

    <!-- Bucket summary: generic S3 and gos3 without admin:GetStats -->
    <template v-else>
      <UAlert
        v-if="mode === 's3'"
        color="info"
        variant="subtle"
        icon="i-lucide-info"
        title="Generic S3 server"
        description="Metrics, users, policies and server settings are available on gos3 servers only. Bucket and object management works with any S3-compatible service."
      />
      <UAlert
        v-else-if="whoami"
        color="neutral"
        variant="subtle"
        icon="i-lucide-lock"
        title="Limited view"
        description="Your identity is not allowed to read server statistics (admin:GetStats)."
      />
      <UCard>
        <template #header>
          <div class="flex items-center justify-between gap-2">
            <h2 class="font-semibold text-highlighted">
              Buckets
            </h2>
            <UButton size="xs" color="neutral" variant="ghost" trailing-icon="i-lucide-arrow-right" label="All buckets" :to="appRoutes.buckets(id)" />
          </div>
        </template>
        <CommonErrorAlert v-if="buckets.error.value" :error="buckets.error.value" title="Could not list buckets" retryable @retry="buckets.refresh" />
        <div v-else-if="buckets.loading.value && !buckets.data.value" class="space-y-2">
          <USkeleton class="h-8 w-32" />
          <USkeleton v-for="n in 3" :key="n" class="h-5 w-full" />
        </div>
        <template v-else>
          <div class="mb-4 flex flex-wrap gap-6">
            <div>
              <div class="text-3xl font-semibold tabular-nums text-highlighted">
                {{ formatNumber(bucketList.length) }}
              </div>
              <div class="text-sm text-muted">
                bucket{{ bucketList.length === 1 ? '' : 's' }}
              </div>
            </div>
            <div v-if="buckets.data.value?.owner?.displayName || buckets.data.value?.owner?.id">
              <div class="text-sm font-medium text-highlighted">
                {{ buckets.data.value?.owner?.displayName || buckets.data.value?.owner?.id }}
              </div>
              <div class="text-sm text-muted">
                owner
              </div>
            </div>
            <div>
              <div class="text-sm font-medium text-highlighted">
                {{ connection?.region }}
              </div>
              <div class="text-sm text-muted">
                signing region
              </div>
            </div>
          </div>
          <CommonEmptyState v-if="!bucketList.length" compact icon="i-lucide-database" title="No buckets yet" description="Create a bucket to start storing objects.">
            <UButton size="sm" icon="i-lucide-plus" label="Go to buckets" :to="appRoutes.buckets(id)" />
          </CommonEmptyState>
          <ul v-else class="divide-y divide-default">
            <li v-for="b in recentBuckets" :key="b.name" class="flex items-center justify-between gap-2 py-2">
              <NuxtLink :to="appRoutes.bucket(id, b.name)" class="flex min-w-0 items-center gap-2 rounded text-sm font-medium text-highlighted hover:text-primary focus-visible:outline-2 focus-visible:outline-primary">
                <UIcon name="i-lucide-database" class="size-4 shrink-0 text-muted" aria-hidden="true" />
                <span class="truncate">{{ b.name }}</span>
              </NuxtLink>
              <span class="shrink-0 text-xs text-muted">
                <CommonRelativeTime v-if="b.creationDate" :value="b.creationDate" />
              </span>
            </li>
          </ul>
          <p v-if="bucketList.length > recentBuckets.length" class="mt-2 text-xs text-muted">
            Showing the {{ recentBuckets.length }} newest of {{ formatNumber(bucketList.length) }} buckets.
          </p>
        </template>
      </UCard>
    </template>
  </div>
</template>
