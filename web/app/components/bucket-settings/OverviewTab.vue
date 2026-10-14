<script setup lang="ts">
import type { BucketSummary } from '~/types/admin'
import type { VersioningStatus } from '~/types/s3'
import type { BucketSettingsTab } from '~/components/admin/-capabilities'

/**
 * Bucket overview: usage, owner, creation date, region and configured
 * sub-resources (gos3 admin API when available), plus what plain S3 can
 * tell (location, creation date, policy status).
 */
const props = defineProps<{
  bucket: string
  /** Admin BucketSummary (gos3 with admin rights) or null. */
  summary: BucketSummary | null
  summaryLoading?: boolean
  versioning: VersioningStatus | null
  objectLock: boolean
}>()

const emit = defineEmits<{ navigate: [tab: BucketSettingsTab] }>()

const { requireS3, connection } = useConnection()

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
  </div>
</template>
