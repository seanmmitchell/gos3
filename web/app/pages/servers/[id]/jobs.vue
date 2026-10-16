<script setup lang="ts">
/**
 * Background jobs of one gos3 server (GET /jobs, admin:ListJobs): copy and
 * mirror jobs and restic snapshot, restore and prune jobs, newest first,
 * with their state, progress, ETA or mirror lag and last error. Filters
 * (kind, state, bucket — the bucket is kept in `?bucket=`) apply to the
 * loaded list. With updateJobs the rows offer pause, resume, cancel, run
 * now and delete. Polls every JOB_POLL_FAST_MS while a job is queued,
 * running or a mirror has pending changes, else every JOB_POLL_SLOW_MS.
 */
import type { Job } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { canUpdateJobs, canViewJobs, identityPending } from '~/components/admin/-capabilities'
import { JOB_KINDS, JOB_STATE_ORDER, filterJobs, jobPollInterval, jobStateMeta, sortJobs, upsertJob } from '~/components/jobs/-jobs'
import JobsTable from '~/components/jobs/JobsTable.vue'

useHead({ title: 'Jobs' })

const route = useRoute()
const router = useRouter()
const id = computed(() => routeParam(route.params.id))
const { connection, isGos3, requireAdmin } = useConnection()
const { capabilities, status } = useWhoAmI()

const pending = computed(() => identityPending(connection.value?.kind, status.value))
const allowed = computed(() => canViewJobs(connection.value?.kind, capabilities.value))
const canManage = computed(() => canUpdateJobs(connection.value?.kind, capabilities.value))

const loader = useLoader(async () => sortJobs(await requireAdmin().listJobs()), { immediate: false })
const jobs = computed<Job[]>(() => loader.data.value ?? [])
const accessDenied = computed(() => loader.error.value instanceof AdminError && loader.error.value.isAccessDenied)
const lastLoaded = ref<number | null>(null)

// --- loading and polling ----------------------------------------------------------------

/** Set when the page unmounts: a response arriving later must not arm a new poll. */
let disposed = false
let timer: ReturnType<typeof setTimeout> | undefined
function stopTimer() {
  if (timer) clearTimeout(timer)
  timer = undefined
}

async function load() {
  const r = await loader.refresh()
  if (r && !disposed) lastLoaded.value = Date.now()
}

/**
 * Arms the next poll: fast while a job is busy, slow otherwise. The only
 * place a timer is set; it replaces any armed one, so there is never more
 * than one chain.
 */
function schedule() {
  stopTimer()
  if (disposed || !allowed.value || !loader.data.value) return
  timer = setTimeout(() => void poll(), jobPollInterval(jobs.value))
}

async function poll() {
  timer = undefined
  // A hidden tab skips the request but keeps polling.
  if (document.visibilityState !== 'hidden') await load()
  schedule()
}

/** Reloads now (the Refresh button) and re-plans the polling. */
async function refresh() {
  stopTimer()
  await load()
  schedule()
}

onBeforeUnmount(() => {
  disposed = true
  stopTimer()
})

watch([id, allowed], ([, ok]) => {
  stopTimer()
  loader.data.value = undefined
  lastLoaded.value = null
  if (ok) void refresh()
}, { immediate: true })

/** A row action changed or deleted a job: apply it now, refresh soon. */
function onChanged(job: Job | null, jobId: string) {
  const list = loader.data.value ?? []
  loader.data.value = job ? upsertJob(list, job) : list.filter(j => j.id !== jobId)
  schedule()
}

// --- filters ---------------------------------------------------------------------------

const kind = ref('')
const state = ref('')
const bucket = computed<string>({
  get: () => (typeof route.query.bucket === 'string' ? route.query.bucket : ''),
  set: (v) => {
    void router.replace({ query: { ...route.query, bucket: v || undefined } })
  }
})

const ALL = '__all'
const kindItems = [
  { label: 'All kinds', value: ALL },
  ...Object.entries(JOB_KINDS).map(([value, m]) => ({ label: m.label, value, icon: m.icon }))
]
const stateItems = [
  { label: 'Any state', value: ALL },
  { label: 'Active (not finished)', value: 'active' },
  ...JOB_STATE_ORDER.map(s => ({ label: jobStateMeta(s).label, value: s }))
]
const kindModel = computed({ get: () => kind.value || ALL, set: (v: string) => (kind.value = v === ALL ? '' : v) })
const stateModel = computed({ get: () => state.value || ALL, set: (v: string) => (state.value = v === ALL ? '' : v) })
const filtered = computed(() => filterJobs(jobs.value, { kind: kind.value, state: state.value, bucket: bucket.value }))
const filtering = computed(() => !!(kind.value || state.value || bucket.value))

function clearFilters() {
  kind.value = ''
  state.value = ''
  bucket.value = ''
}
</script>

<template>
  <div>
    <CommonPageHeader
      title="Jobs"
      icon="i-lucide-list-checks"
      :description="connection ? `Copy, mirror and restic jobs of ${connection.name}. Start a copy or mirror from a bucket's settings.` : undefined"
    >
      <template #actions>
        <UButton
          v-if="allowed"
          color="neutral"
          variant="outline"
          icon="i-lucide-refresh-cw"
          label="Refresh"
          :loading="loader.loading.value"
          @click="refresh"
        />
        <UButton color="neutral" variant="ghost" icon="i-lucide-database" label="Buckets" :to="appRoutes.buckets(id)" />
      </template>
    </CommonPageHeader>

    <div class="space-y-4">
      <CommonEmptyState
        v-if="!isGos3"
        icon="i-lucide-server-off"
        title="Jobs need a gos3 server"
        description="This connection is a generic S3 server."
      />
      <div v-else-if="pending" class="space-y-3" aria-busy="true">
        <USkeleton class="h-10 w-full" />
        <USkeleton class="h-40 w-full" />
      </div>
      <CommonEmptyState
        v-else-if="!allowed || accessDenied"
        icon="i-lucide-lock"
        title="No access to jobs"
        description="Your identity is not allowed to list this server's jobs (admin:ListJobs). Ask an administrator for the diagnostics or admin policy."
      />
      <CommonErrorAlert v-else-if="loader.error.value && !loader.data.value" :error="loader.error.value" title="Could not load the jobs" retryable @retry="refresh" />
      <div v-else-if="!loader.data.value" class="space-y-3" aria-busy="true" aria-label="Loading jobs">
        <USkeleton class="h-10 w-full" />
        <USkeleton class="h-40 w-full" />
      </div>

      <template v-else>
        <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Refresh failed; showing the last known state" retryable @retry="refresh" />

        <div class="flex flex-col gap-2 sm:flex-row sm:items-center" role="search" aria-label="Filter jobs">
          <USelect v-model="kindModel" :items="kindItems" value-key="value" class="sm:w-44" aria-label="Kind" />
          <USelect v-model="stateModel" :items="stateItems" value-key="value" class="sm:w-52" aria-label="State" />
          <UInput
            v-model.trim="bucket"
            icon="i-lucide-database"
            placeholder="Bucket"
            class="sm:w-56"
            aria-label="Bucket"
            autocomplete="off"
            spellcheck="false"
          >
            <template v-if="bucket" #trailing>
              <UButton color="neutral" variant="link" size="sm" icon="i-lucide-x" aria-label="Clear bucket filter" @click="bucket = ''" />
            </template>
          </UInput>
          <span v-if="lastLoaded" class="text-xs text-muted sm:ms-auto">
            Updated <CommonRelativeTime :value="lastLoaded" /> · refreshing every {{ jobPollInterval(jobs) / 1000 }} s
          </span>
        </div>

        <CommonEmptyState
          v-if="!jobs.length"
          icon="i-lucide-list-checks"
          title="No jobs yet"
          description="Copy and mirror jobs are started from a bucket (Settings → Copy / Mirror…); restic snapshots from its Backup & archive tab."
        />
        <CommonEmptyState
          v-else-if="!filtered.length"
          icon="i-lucide-filter-x"
          title="No jobs match"
          :description="`None of the ${jobs.length} jobs match the filters.`"
        >
          <UButton size="sm" color="neutral" variant="outline" label="Clear filters" @click="clearFilters" />
        </CommonEmptyState>
        <UCard v-else :ui="{ body: 'p-0 sm:p-0' }">
          <JobsTable :jobs="filtered" :server-id="id" :can-manage="canManage" @changed="onChanged" />
        </UCard>
        <p v-if="filtering && filtered.length" class="text-xs text-muted">
          {{ filtered.length }} of {{ jobs.length }} jobs shown.
        </p>
      </template>
    </div>
  </div>
</template>
