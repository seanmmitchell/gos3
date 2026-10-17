<script setup lang="ts">
import type { AvailableBacking, BucketSummary, Job } from '~/types/admin'
import type { VersioningStatus } from '~/types/s3'
import { AdminError } from '~/utils/admin/client'
import { canUpdateJobs, serverHasJobs, type BucketSettingsTab } from '~/components/admin/-capabilities'
import { jobPollInterval, sortJobs, upsertJob } from '~/components/jobs/-jobs'
import JobsTable from '~/components/jobs/JobsTable.vue'

/**
 * Bucket overview: usage, owner, creation date, region and configured
 * sub-resources (gos3 admin API when available), plus what plain S3 can
 * tell (location, creation date, policy status) and, on gos3, the storage
 * backing and, on s3 backings, the bucket's kind and location. On gos3
 * servers with jobs, a Jobs card lists the copy, mirror and restic jobs
 * whose source or target is the bucket (GET /buckets/{name}/jobs, which
 * needs admin:GetJob on the bucket: the card stays hidden on a 403).
 */
const props = defineProps<{
  bucket: string
  /** Admin BucketSummary (gos3 with admin rights) or null. */
  summary: BucketSummary | null
  summaryLoading?: boolean
  versioning: VersioningStatus | null
  objectLock: boolean
  /** gos3 primary storage backing name, when known. */
  backing?: string
  /** Its kind (WhoAmI.backings), when listed. */
  backingDetails?: AvailableBacking
  /** s3 backings: the bucket kind (native or wrapped), when known. */
  kind?: string
  /** s3 backings: the remote bucket (native) or container (wrapped). */
  location?: string
  /** Changes when a job for this bucket was created elsewhere on the page (reloads the Jobs card). */
  jobsKey?: number
}>()

const emit = defineEmits<{ navigate: [tab: BucketSettingsTab] }>()

const { id: serverId, requireS3, requireAdmin, connection } = useConnection()
const { capabilities } = useWhoAmI()

const s3Info = useLoader(async () => {
  const s3 = requireS3()
  const [location, list, policyStatus] = await Promise.all([
    s3.getBucketLocation(props.bucket).catch(() => undefined),
    s3.listBuckets().catch(() => undefined),
    s3.getBucketPolicyStatus(props.bucket).catch(() => null)
  ])
  const entry = list?.buckets.find(b => b.name === props.bucket)
  return { location: location || entry?.region || undefined, created: entry?.creationDate, isPublic: policyStatus?.isPublic ?? null }
}, { watch: [() => props.bucket] })

const region = computed(() => props.summary?.region || s3Info.data.value?.location || connection.value?.region || '—')
const created = computed(() => props.summary?.createdAt || s3Info.data.value?.created)
const isPublic = computed<boolean | null>(() => props.summary?.public ?? s3Info.data.value?.isPublic ?? null)

const usage = computed(() => props.summary?.usage ?? null)
/** "… incl. versions", after the compression saving of the current versions when there is one. */
const sizeHint = computed(() => {
  const u = usage.value
  if (!u) return undefined
  const note = formatStoredNote(u.bytes, u.storedBytes)
  return `${note ? `${note} · ` : ''}${formatBytes(u.versionBytes)} incl. versions`
})

const configs = computed(() => {
  const c = props.summary?.configs
  if (!c) return []
  const items: { label: string, on: boolean, tab: BucketSettingsTab, detail?: string }[] = [
    { label: 'Bucket policy', on: c.policy, tab: 'policy' },
    { label: 'CORS', on: c.cors, tab: 'cors' },
    { label: 'Lifecycle', on: c.lifecycle, tab: 'lifecycle' },
    { label: 'Tags', on: c.tagging, tab: 'tags' },
    { label: 'Default encryption', on: c.encryption, tab: 'encryption' },
    { label: 'Object lock', on: c.objectLock, tab: 'object-lock' },
    { label: 'ACL', on: !!c.acl && c.acl !== 'private', tab: 'access', detail: c.acl || 'private' }
  ]
  const comp = props.summary?.compression
  if (comp) {
    // The bucket's own setting may differ from what happens (server mode).
    const detail = comp.effective ? (comp.enabled ? 'Enabled' : 'Enforced') : (comp.enabled ? 'Off (server disabled)' : 'Not set')
    items.splice(5, 0, { label: 'Compression', on: comp.effective, tab: 'compression', detail })
  }
  return items
})

const backingInfo = computed(() => (props.backingDetails ? backingKindInfo(props.backingDetails) : null))
const kindInfo = computed(() => (props.kind ? bucketKindInfo(props.kind) : null))

// --- jobs of this bucket ---------------------------------------------------------------

const jobsEnabled = computed(() => serverHasJobs(connection.value?.kind, capabilities.value))
const canManage = computed(() => canUpdateJobs(connection.value?.kind, capabilities.value))
const jobs = useLoader(async () => (jobsEnabled.value ? sortJobs(await requireAdmin().listBucketJobs(props.bucket)) : null), {
  watch: [() => props.bucket, () => props.jobsKey, jobsEnabled]
})
/** Hidden while unknown, on servers without jobs and when not allowed (403) or unsupported (404). */
const jobsHidden = computed(() => {
  const e = jobs.error.value
  return !jobsEnabled.value || (e instanceof AdminError && (e.isAccessDenied || e.isNotFound)) || (!jobs.data.value && !e)
})
const jobList = computed<Job[]>(() => jobs.data.value ?? [])

/** Set on unmount: a response arriving later must not arm a new poll. */
let disposed = false
let jobsTimer: ReturnType<typeof setTimeout> | undefined
function stopJobsTimer() {
  if (jobsTimer) clearTimeout(jobsTimer)
  jobsTimer = undefined
}
/** Arms the next poll of the bucket's jobs (the only place a timer is set). */
function scheduleJobs() {
  stopJobsTimer()
  const list = jobs.data.value
  if (disposed || !list) return
  jobsTimer = setTimeout(() => void pollJobs(), jobPollInterval(list))
}
async function pollJobs() {
  jobsTimer = undefined
  if (document.visibilityState !== 'hidden') await jobs.refresh()
  scheduleJobs()
}
// Every load (also after the bucket changes) re-plans the polling.
watch(() => jobs.data.value, () => scheduleJobs())
onBeforeUnmount(() => {
  disposed = true
  stopJobsTimer()
})

function onJobChanged(job: Job | null, jobId: string) {
  const list = jobs.data.value ?? []
  jobs.data.value = job ? upsertJob(list, job) : list.filter(j => j.id !== jobId)
}

const versioningLabel = computed(() => (props.versioning === 'Enabled' ? 'Enabled' : props.versioning === 'Suspended' ? 'Suspended' : 'Never enabled'))
</script>

<template>
  <div class="space-y-6">
    <div class="grid grid-cols-2 gap-3 xl:grid-cols-4">
      <template v-if="usage">
        <AdminStatCard label="Objects" :value="formatNumber(usage.objects)" icon="i-lucide-files" :hint="`${formatNumber(usage.versions)} versions`" />
        <AdminStatCard label="Size" :value="formatBytes(usage.bytes)" icon="i-lucide-hard-drive" :hint="sizeHint" />
        <AdminStatCard label="Delete markers" :value="formatNumber(usage.deleteMarkers)" icon="i-lucide-eraser" />
        <AdminStatCard label="Pending uploads" :value="formatNumber(usage.pendingUploads)" icon="i-lucide-upload" :color="usage.pendingUploads ? 'warning' : 'default'" />
      </template>
      <template v-else-if="summaryLoading">
        <AdminStatCard v-for="n in 4" :key="n" label="Loading…" value="" loading />
      </template>
    </div>
    <UAlert
      v-if="!usage && !summaryLoading"
      color="neutral"
      variant="subtle"
      icon="i-lucide-info"
      title="Usage not available"
      :description="connection?.kind === 'gos3' ? 'Bucket usage needs admin permissions (admin:GetBucket).' : 'Bucket usage is reported by gos3 servers only.'"
    />

    <div class="grid gap-6 lg:grid-cols-2">
      <UCard>
        <template #header>
          <h3 class="font-semibold text-highlighted">
            Details
          </h3>
        </template>
        <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
          <dt class="text-muted">
            Name
          </dt>
          <dd class="flex items-center gap-1 font-mono text-xs">
            {{ bucket }}<CommonCopyButton :value="bucket" aria-label="Copy bucket name" />
          </dd>
          <dt class="text-muted">
            Region
          </dt>
          <dd>{{ region }}</dd>
          <dt class="text-muted">
            Created
          </dt>
          <dd><CommonRelativeTime :value="created" /></dd>
          <template v-if="summary?.owner">
            <dt class="text-muted">
              Owner
            </dt>
            <dd>{{ summary.owner }}</dd>
          </template>
          <template v-if="backing">
            <dt class="text-muted">
              Storage backing
            </dt>
            <dd class="flex flex-wrap items-center gap-1.5">
              <span class="font-mono text-xs">{{ backing }}</span>
              <UBadge v-if="backingInfo" :label="backingInfo.label" :icon="backingInfo.icon" color="neutral" variant="subtle" size="sm" :title="backingInfo.description" />
            </dd>
          </template>
          <template v-if="kindInfo">
            <dt class="text-muted">
              Bucket kind
            </dt>
            <dd data-testid="bucket-kind">
              <UBadge :label="kindInfo.label" :icon="kindInfo.icon" color="neutral" variant="subtle" size="sm" />
              <p class="mt-1 text-xs text-muted">
                {{ kindInfo.description }}
              </p>
            </dd>
          </template>
          <template v-if="location">
            <dt class="text-muted">
              {{ kind === 'native' ? 'Remote bucket' : kind === 'wrapped' ? 'Container' : 'Location' }}
            </dt>
            <dd class="flex items-center gap-1 font-mono text-xs" data-testid="bucket-location">
              <span class="truncate" :title="location">{{ location }}</span><CommonCopyButton :value="location" aria-label="Copy location" />
            </dd>
          </template>
          <dt class="text-muted">
            Versioning
          </dt>
          <dd>
            <button type="button" class="rounded hover:text-primary focus-visible:outline-2 focus-visible:outline-primary" @click="emit('navigate', 'versioning')">
              {{ versioningLabel }}
            </button>
          </dd>
          <dt class="text-muted">
            Object lock
          </dt>
          <dd>{{ objectLock ? 'Enabled' : 'Disabled' }}</dd>
          <dt class="text-muted">
            Access
          </dt>
          <dd>
            <UBadge v-if="isPublic === true" label="Public" color="warning" variant="subtle" size="sm" icon="i-lucide-globe" />
            <UBadge v-else-if="isPublic === false" label="Not public" color="success" variant="subtle" size="sm" icon="i-lucide-lock" />
            <span v-else class="text-muted">Unknown</span>
          </dd>
        </dl>
      </UCard>

      <UCard v-if="configs.length">
        <template #header>
          <h3 class="font-semibold text-highlighted">
            Configuration
          </h3>
        </template>
        <ul class="divide-y divide-default">
          <li v-for="c in configs" :key="c.label" class="flex items-center justify-between gap-2 py-2 text-sm">
            <button type="button" class="rounded text-left hover:text-primary focus-visible:outline-2 focus-visible:outline-primary" @click="emit('navigate', c.tab)">
              {{ c.label }}
            </button>
            <UBadge
              :label="c.detail ?? (c.on ? 'Configured' : 'Not set')"
              :color="c.on ? 'primary' : 'neutral'"
              variant="subtle"
              size="sm"
            />
          </li>
        </ul>
      </UCard>
    </div>

    <UCard v-if="!jobsHidden" :ui="{ body: 'p-0 sm:p-0' }" data-testid="bucket-jobs">
      <template #header>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h3 class="font-semibold text-highlighted">
              Jobs
            </h3>
            <p class="text-sm text-muted">
              Copy, mirror and restic jobs reading or writing this bucket.
            </p>
          </div>
          <UButton
            v-if="serverId"
            color="neutral"
            variant="ghost"
            size="sm"
            trailing-icon="i-lucide-arrow-right"
            label="All jobs"
            :to="appRoutes.jobs(serverId, bucket)"
          />
        </div>
      </template>
      <CommonErrorAlert v-if="jobs.error.value" :error="jobs.error.value" title="Could not load the bucket's jobs" retryable class="m-4" @retry="jobs.refresh" />
      <p v-else-if="!jobList.length" class="p-4 text-sm text-muted" data-testid="bucket-jobs-empty">
        No jobs for this bucket.
      </p>
      <JobsTable v-else :jobs="jobList.slice(0, 10)" :server-id="serverId ?? ''" :can-manage="canManage" compact label="Jobs of this bucket" @changed="onJobChanged" />
    </UCard>
  </div>
</template>
