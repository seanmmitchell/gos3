<script setup lang="ts">
import type { BucketSummary } from '~/types/admin'
import type { ObjectLockConfiguration, VersioningStatus } from '~/types/s3'
import { S3Error } from '~/utils/s3/errors'
import { bucketSettingsTabs, canShowBucketUsage, resolveBucketTab, type BucketSettingsTab } from '~/components/admin/-capabilities'
import { modeOption } from '~/components/admin/-storage'

/**
 * Bucket settings with one section per S3 bucket sub-resource, plus gos3-only
 * ones such as compression, picked with the chips under the header (synced
 * with `?tab=`). Shared state — versioning, object lock, the admin
 * BucketSummary and (gos3) the primary storage backing with, on s3
 * backings, the bucket's kind (native or wrapped) and location — is loaded
 * here and refreshed when a tab changes it.
 */
const route = useRoute()
const router = useRouter()
const id = computed(() => routeParam(route.params.id))
const bucket = computed(() => routeParam(route.params.bucket))
const { connection, requireS3, requireAdmin } = useConnection()
const { capabilities, whoami } = useWhoAmI()

useHead({ title: () => `${bucket.value} · Settings` })

// Placement is only offered for gos3 identities with viewStorage.
const tabs = computed(() => bucketSettingsTabs(connection.value?.kind, capabilities.value))
const tab = computed<BucketSettingsTab>({
  get: () => resolveBucketTab(route.query.tab, tabs.value),
  set: (v) => {
    void router.replace({ query: { ...route.query, tab: v === 'overview' ? undefined : v } })
  }
})
const tabLabel = computed(() => tabs.value.find(t => t.value === tab.value)?.label)

// --- shared state ----------------------------------------------------------
const versioning = ref<VersioningStatus | null>(null)
/** The bucket's storage cannot keep versions (GetBucketVersioning answered NotImplemented). */
const versioningUnsupported = ref(false)
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
    let unsupported = false
    const [v, ol] = await Promise.all([
      s3.getBucketVersioning(bucket.value).catch((e: unknown) => {
        // Buckets that cannot be versioned (e.g. a native bucket whose
        // remote S3 service has no versioning) may answer NotImplemented.
        if (isNotImplemented(e)) {
          unsupported = true
          return { status: '' as const }
        }
        throw e
      }),
      s3.getObjectLockConfiguration(bucket.value).catch((e: unknown) => {
        // Servers without object lock support answer NotImplemented; treat as disabled.
        if (isNotImplemented(e)) return null
        throw e
      })
    ])
    versioning.value = v.status
    versioningUnsupported.value = unsupported
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

// Primary storage backing (gos3): from the admin summary's placement, else
// HeadBucket's x-gos3-backing header; its kind from WhoAmI.backings. On s3
// backings the bucket kind comes from the summary or x-gos3-bucket-kind.
const head = ref<{ backing?: string, kind?: string }>({})
async function loadBacking() {
  const name = bucket.value
  head.value = {}
  if (connection.value?.kind !== 'gos3') return
  try {
    const { backing, kind } = await requireS3().headBucket(name)
    if (name === bucket.value) head.value = { backing, kind }
  } catch {
    // Optional; the tabs then simply do not know the backing.
  }
}
const backing = computed(() => summary.value?.placement?.primary || head.value.backing)
const backingDetails = computed(() => findBacking(backing.value, whoami.value?.backings))
/** native | wrapped (s3 backings), '' when unknown or on other backings. */
const bucketKind = computed(() => summary.value?.kind || head.value.kind || '')
const bucketLocation = computed(() => summary.value?.location || '')
const kindInfo = computed(() => (bucketKind.value ? bucketKindInfo(bucketKind.value) : null))
const backingBadgeTitle = computed(() => {
  const parts = [backingDetails.value ? `Storage backing: ${backingKindInfo(backingDetails.value).label}` : `Storage backing ${backing.value}`]
  if (kindInfo.value) parts.push(`${kindInfo.value.label} bucket${bucketLocation.value ? ` in ${bucketLocation.value}` : ''}`)
  return parts.join(' · ')
})

function refreshShared() {
  void loadState()
  void loadSummary()
}

watch([id, bucket], () => {
  refreshShared()
  void loadBacking()
}, { immediate: true })
watch(showUsage, () => void loadSummary())

const replication = computed(() => {
  const p = summary.value?.placement
  if (!p || p.mode === 'single') return null
  return `${modeOption(p.mode)?.label ?? p.mode} ×${(p.replicas?.length ?? 0) + 1}`
})

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
        <UBadge v-if="replication" :label="replication" color="primary" variant="subtle" size="sm" icon="i-lucide-copy" />
        <UBadge v-if="summary?.placement?.moving" label="Moving" color="warning" variant="subtle" size="sm" icon="i-lucide-arrow-right-left" />
        <UBadge
          v-if="backing && (backingDetails?.kind === 's3' || kindInfo)"
          :label="kindInfo ? `${backing} · ${kindInfo.label.toLowerCase()}` : backing"
          :icon="kindInfo?.icon ?? backingKindInfo(backingDetails).icon"
          :title="backingBadgeTitle"
          color="neutral"
          variant="subtle"
          size="sm"
          data-testid="backing-badge"
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
      <CommonSectionChips v-model="tab" :items="tabs" label="Bucket settings sections" panel-id="bucket-settings-panel" />

      <div v-if="stateLoading && versioning === null" class="space-y-3">
        <USkeleton class="h-24 w-full" />
        <USkeleton class="h-40 w-full" />
      </div>
      <div v-else id="bucket-settings-panel" role="tabpanel" :aria-label="tabLabel">
        <BucketSettingsOverviewTab
          v-if="tab === 'overview'"
          :bucket="bucket"
          :summary="summary"
          :summary-loading="summaryLoading"
          :versioning="versioning"
          :object-lock="lockEnabled"
          :backing="backing"
          :backing-details="backingDetails"
          :kind="bucketKind"
          :location="bucketLocation"
          @navigate="tab = $event"
        />
        <BucketSettingsVersioningTab
          v-else-if="tab === 'versioning'"
          :bucket="bucket"
          :status="versioning"
          :object-lock="lockEnabled"
          :unsupported="versioningUnsupported"
          @changed="refreshShared"
        />
        <BucketSettingsPolicyTab v-else-if="tab === 'policy'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsCorsTab v-else-if="tab === 'cors'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsLifecycleTab v-else-if="tab === 'lifecycle'" :bucket="bucket" :versioning="versioning" @changed="loadSummary" />
        <BucketSettingsTagsTab v-else-if="tab === 'tags'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsEncryptionTab v-else-if="tab === 'encryption'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsCompressionTab v-else-if="tab === 'compression'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsObjectLockTab
          v-else-if="tab === 'object-lock'"
          :bucket="bucket"
          :versioning="versioning"
          :unsupported="versioningUnsupported"
          @changed="refreshShared"
        />
        <BucketSettingsAccessTab v-else-if="tab === 'access'" :bucket="bucket" @changed="loadSummary" />
        <BucketSettingsPlacementTab v-else-if="tab === 'placement'" :bucket="bucket" :kind="bucketKind" @changed="loadSummary" />
        <BucketSettingsDangerZoneTab v-else-if="tab === 'danger'" :bucket="bucket" :kind="bucketKind" :location="bucketLocation" />
      </div>
    </template>
  </div>
</template>
