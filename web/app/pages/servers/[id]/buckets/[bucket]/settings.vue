<script setup lang="ts">
import type { BucketSummary } from '~/types/admin'
import type { ObjectLockConfiguration, VersioningStatus } from '~/types/s3'
import { S3Error } from '~/utils/s3/errors'
import { BUCKET_TABS, canShowBucketUsage, resolveBucketTab, type BucketSettingsTab } from '~/components/admin/-capabilities'

/**
 * Bucket settings with one tab per S3 bucket sub-resource (synced with
 * `?tab=`). Shared state — versioning, object lock, the admin
 * BucketSummary and (gos3) the storage backing — is loaded here and
 * refreshed when a tab changes it.
 */
const route = useRoute()
const router = useRouter()
const id = computed(() => routeParam(route.params.id))
const bucket = computed(() => routeParam(route.params.bucket))
const { connection, requireS3, requireAdmin } = useConnection()
const { capabilities, whoami } = useWhoAmI()

useHead({ title: () => `${bucket.value} · Settings` })

const tab = computed<BucketSettingsTab>({
  get: () => resolveBucketTab(route.query.tab),
  set: (v) => {
    void router.replace({ query: { ...route.query, tab: v === 'overview' ? undefined : v } })
  }
})
const tabItems = BUCKET_TABS.map(t => ({ label: t.label, value: t.value, icon: t.icon }))

// --- shared state ----------------------------------------------------------
const versioning = ref<VersioningStatus | null>(null)
const objectLock = ref<ObjectLockConfiguration | null>(null)
const stateError = ref<unknown>(null)
const stateLoading = ref(false)

function isNotImplemented(e: unknown): boolean {
  return e instanceof S3Error && (e.code === 'NotImplemented' || e.status === 501)
}

async function loadState() {
  stateLoading.value = true
  stateError.value = null
  try {
    const s3 = requireS3()
    const [v, ol] = await Promise.all([
      s3.getBucketVersioning(bucket.value).catch((e: unknown) => {
        // Buckets that cannot be versioned (gos3 gateway backings) may answer NotImplemented.
        if (isNotImplemented(e)) return { status: '' as const }
        throw e
      }),
      s3.getObjectLockConfiguration(bucket.value).catch((e: unknown) => {
        // Servers without object lock support answer NotImplemented; treat as disabled.
        if (isNotImplemented(e)) return null
        throw e
      })
    ])
    versioning.value = v.status
    objectLock.value = ol
  } catch (e) {
    stateError.value = e
  } finally {
    stateLoading.value = false
  }
}

const showUsage = computed(() => canShowBucketUsage(connection.value?.kind, capabilities.value))
const summary = ref<BucketSummary | null>(null)
const summaryLoading = ref(false)
async function loadSummary() {
  if (!showUsage.value) {
    summary.value = null
    return
  }
  summaryLoading.value = true
  try {
    summary.value = await requireAdmin().getBucket(bucket.value)
  } catch {
    // Optional (admin:GetBucket may be denied); the overview falls back to S3 data.
    summary.value = null
  } finally {
    summaryLoading.value = false
  }
}

// Storage backing (gos3): from the admin summary, else HeadBucket's x-gos3-backing header.
const headBacking = ref<string | undefined>()
async function loadBacking() {
  const name = bucket.value
  headBacking.value = undefined
  if (connection.value?.kind !== 'gos3') return
  try {
    const { backing } = await requireS3().headBucket(name)
    if (name === bucket.value) headBacking.value = backing
  } catch {
    // Optional; the tabs then simply do not know the backing.
  }
}
const backing = computed(() => summary.value?.backing || headBacking.value)
const backingMode = computed(() => summary.value?.backingMode ?? backingModeOf(backing.value, whoami.value?.backings))
/** Gateway-backed buckets support neither versioning nor object lock. */
const noVersioning = computed(() => !backingSupportsVersioning(backingMode.value))

function refreshShared() {
  void loadState()
  void loadSummary()
}

watch([id, bucket], () => {
  refreshShared()
  void loadBacking()
}, { immediate: true })
watch(showUsage, () => void loadSummary())

const notFound = computed(() => stateError.value instanceof S3Error && stateError.value.code === 'NoSuchBucket')
const lockEnabled = computed(() => !!objectLock.value?.enabled)
</script>

<template>
  <div class="space-y-6">
    <CommonPageHeader :title="bucket" icon="i-lucide-sliders-horizontal" description="Bucket settings">
      <template #badge>
        <UBadge v-if="versioning === 'Enabled'" label="Versioned" color="primary" variant="subtle" size="sm" icon="i-lucide-history" />
        <UBadge v-else-if="versioning === 'Suspended'" label="Versioning suspended" color="warning" variant="subtle" size="sm" />
        <UBadge v-if="lockEnabled" label="Object lock" color="primary" variant="subtle" size="sm" icon="i-lucide-shield-check" />
        <UBadge v-if="summary?.public" label="Public" color="warning" variant="subtle" size="sm" icon="i-lucide-globe" />
        <UBadge
          v-if="backing && backing !== LOCAL_BACKING"
          :label="backing"
          :icon="backingModeInfo(backingMode).icon"
          :title="`Storage backing (${backingModeInfo(backingMode).label})`"
          color="neutral"
          variant="subtle"
          size="sm"
        />
      </template>
      <template #actions>
        <UButton color="neutral" variant="outline" icon="i-lucide-folder-open" label="Browse" :to="appRoutes.bucket(id, bucket)" />
      </template>
    </CommonPageHeader>

    <CommonEmptyState v-if="notFound" icon="i-lucide-database-zap" title="Bucket not found" :description="`There is no bucket named “${bucket}” on this server.`">
      <UButton size="sm" color="neutral" variant="outline" icon="i-lucide-arrow-left" label="All buckets" :to="appRoutes.buckets(id)" />
    </CommonEmptyState>
    <CommonErrorAlert v-else-if="stateError" :error="stateError" title="Could not load the bucket" retryable @retry="refreshShared" />

    <template v-else>
      <div class="-mx-1 overflow-x-auto px-1 pb-1">
        <UTabs
          v-model="tab"
          :items="tabItems"
          :content="false"
          variant="link"
          class="min-w-max"
          aria-label="Bucket settings sections"
        />
      </div>

      <div v-if="stateLoading && versioning === null" class="space-y-3">
        <USkeleton class="h-24 w-full" />
        <USkeleton class="h-40 w-full" />
      </div>
      <div v-else role="tabpanel" :aria-label="tabItems.find(t => t.value === tab)?.label">
        <BucketSettingsOverviewTab
          v-if="tab === 'overview'"
          :bucket="bucket"
          :summary="summary"
          :summary-loading="summaryLoading"
          :versioning="versioning"
          :object-lock="lockEnabled"
          :backing="backing"
          :backing-mode="backingMode"
          @navigate="tab = $event"
        />
        <BucketSettingsVersioningTab
          v-else-if="tab === 'versioning'"
          :bucket="bucket"
          :status="versioning"
          :object-lock="lockEnabled"
          :unsupported="noVersioning"
          @changed="refreshShared"
        />
        <BucketSettingsPolicyTab v-else-if="tab === 'policy'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsCorsTab v-else-if="tab === 'cors'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsLifecycleTab v-else-if="tab === 'lifecycle'" :bucket="bucket" :versioning="versioning" @changed="loadSummary" />
        <BucketSettingsTagsTab v-else-if="tab === 'tags'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsEncryptionTab v-else-if="tab === 'encryption'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsObjectLockTab
          v-else-if="tab === 'object-lock'"
          :bucket="bucket"
          :versioning="versioning"
          :unsupported="noVersioning"
          @changed="refreshShared"
        />
        <BucketSettingsAccessTab v-else-if="tab === 'access'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsDangerZoneTab v-else-if="tab === 'danger'" :bucket="bucket" />
      </div>
    </template>
  </div>
</template>
