<script setup lang="ts">
import type { DropdownMenuItem, TableColumn } from '@nuxt/ui'
import type { BucketUsage, PlacementSummary } from '~/types/admin'
import { S3Error } from '~/utils/s3/errors'
import { AdminError } from '~/utils/admin/client'
import { usageAgeNote } from '~/components/browser/-bucket-usage'
import { placementChips, placementWorthShowing } from '~/components/admin/-storage'
import { BYPASS_GOVERNANCE_HELP, FORCE_DELETE_LOCKED_TITLE, forceDeleteBlock } from '~/components/bucket-settings/-force-delete'

/**
 * Bucket list. gos3 identities with admin rights get the admin listing
 * (usage, versioning, public / object-lock flags); everybody else gets plain
 * S3 ListBuckets.
 */
useHead({ title: 'Buckets' })

const route = useRoute()
const router = useRouter()
const confirm = useConfirm()
const toast = useToast()
const { connection, isGos3, admin, s3, requireS3, requireAdmin } = useConnection()
const { can } = useWhoAmI()

const serverId = computed(() => routeParam(route.params.id))

interface BucketRow {
  name: string
  createdAt?: string
  region?: string
  versioning?: 'Enabled' | 'Suspended' | ''
  objectLock?: boolean
  public?: boolean
  usage?: BucketUsage | null
  /** Storage backing / replication (gos3 with storage backings). */
  placement?: PlacementSummary
}

interface Listing {
  rows: BucketRow[]
  /** 'admin' = gos3 admin API (with usage), 's3' = ListBuckets. */
  source: 'admin' | 's3'
  /** Why the admin listing was not used (shown as a hint). */
  adminError?: unknown
}

const useAdminListing = computed(() => isGos3.value && !!admin.value && can('admin'))

async function load(): Promise<Listing> {
  let adminError: unknown
  if (useAdminListing.value) {
    try {
      const list = await requireAdmin().listBuckets({ usage: true })
      return {
        source: 'admin',
        rows: list.map(b => ({
          name: b.name,
          createdAt: b.createdAt,
          region: b.region,
          versioning: b.versioning,
          objectLock: b.objectLock,
          public: b.public,
          usage: b.usage,
          placement: b.placement
        }))
      }
    } catch (e) {
      // Not allowed to use admin:ListBuckets → fall back to S3.
      adminError = e
    }
  }
  const r = await requireS3().listBuckets()
  return { source: 's3', adminError, rows: r.buckets.map(b => ({ name: b.name, createdAt: b.creationDate, region: b.region })) }
}

const { data, loading, error, refresh } = useLoader(load, { watch: [serverId, useAdminListing] })

// ---------------------------------------------------------------------------
// Filter + sort
// ---------------------------------------------------------------------------

const query = ref('')
type SortKey = 'name' | 'created' | 'objects' | 'bytes'
const sort = ref<{ by: SortKey, desc: boolean }>({ by: 'name', desc: false })

const rows = computed<BucketRow[]>(() => {
  const q = query.value.trim().toLowerCase()
  const list = (data.value?.rows ?? []).filter(r => !q || r.name.includes(q))
  const dir = sort.value.desc ? -1 : 1
  const val = (r: BucketRow): number | string => {
    switch (sort.value.by) {
      case 'created': return r.createdAt ? Date.parse(r.createdAt) || 0 : 0
      case 'objects': return r.usage?.objects ?? -1
      case 'bytes': return r.usage?.bytes ?? -1
      default: return r.name
    }
  }
  return list.slice().sort((a, b) => {
    const va = val(a)
    const vb = val(b)
    const c = typeof va === 'number' && typeof vb === 'number' ? va - vb : String(va).localeCompare(String(vb))
    return (c || a.name.localeCompare(b.name)) * dir
  })
})

function setSort(by: SortKey) {
  sort.value = sort.value.by === by ? { by, desc: !sort.value.desc } : { by, desc: by !== 'name' }
}

/** Accessible name of a sortable column header button. */
function sortLabel(by: SortKey, label: string): string {
  if (sort.value.by !== by) return `${label}: not sorted, activate to sort`
  return `${label}: sorted ${sort.value.desc ? 'descending' : 'ascending'}, activate to reverse`
}

const showUsage = computed(() => data.value?.source === 'admin')
// Placement chips only when they tell something (not "default" on every row).
const showPlacement = computed(() => showUsage.value && placementWorthShowing((data.value?.rows ?? []).map(r => r.placement)))
const showRegion = computed(() => (data.value?.rows ?? []).some(r => !!r.region))

const columns = computed<TableColumn<BucketRow>[]>(() => {
  const cols: TableColumn<BucketRow>[] = [{ id: 'name', header: 'Name' }]
  if (showUsage.value) {
    cols.push({ id: 'versioning', header: 'Versioning' })
    if (showPlacement.value) cols.push({ id: 'placement', header: 'Storage' })
    cols.push({ id: 'objects', header: 'Objects', meta: { class: { th: 'text-right', td: 'text-right' } } })
    cols.push({ id: 'bytes', header: 'Size', meta: { class: { th: 'text-right', td: 'text-right' } } })
  }
  if (showRegion.value) cols.push({ id: 'region', header: 'Region' })
  cols.push({ id: 'created', header: 'Created' })
  cols.push({ id: 'actions', header: '', meta: { class: { td: 'w-0 text-right', th: 'w-0' } } })
  return cols
})

// Usage is cached by the server for up to 30 s: say how old it is.
const usageNote = computed(() => (data.value && showUsage.value ? usageAgeNote(data.value.rows.map(r => r.usage)) : ''))

const totals = computed(() => {
  const list = data.value?.rows ?? []
  if (!showUsage.value) return null
  return list.reduce((acc, r) => ({ objects: acc.objects + (r.usage?.objects ?? 0), bytes: acc.bytes + (r.usage?.bytes ?? 0) }), { objects: 0, bytes: 0 })
})

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

const createOpen = ref(false)
const canForceDelete = computed(() => isGos3.value && can('manageBuckets'))

function objectsLink(name: string) {
  return browserRoute(serverId.value, name, '')
}

function isNotEmpty(e: unknown): boolean {
  return (e instanceof S3Error && e.code === 'BucketNotEmpty') || (e instanceof AdminError && e.status === 409)
}

const deleting = ref<string | null>(null)

async function forceDelete(name: string, bypassGovernanceRetention = false) {
  const ok = bypassGovernanceRetention || await confirm({
    title: `Force delete bucket ${name}?`,
    description: 'Every object, version, delete marker, pending upload and configuration in this bucket is deleted permanently, then the bucket itself. This cannot be undone.',
    confirmLabel: 'Delete everything',
    requireText: name,
    icon: 'i-lucide-triangle-alert'
  })
  if (!ok) return
  deleting.value = name
  try {
    await requireAdmin().deleteBucket(name, { force: true, bypassGovernanceRetention })
    toast.add({ title: `Bucket ${name} and all its contents deleted`, color: 'success', icon: 'i-lucide-circle-check' })
    await refresh()
  } catch (e) {
    const block = forceDeleteBlock(e)
    if (block?.governance && !bypassGovernanceRetention) {
      // Only GOVERNANCE retention is in the way: offer to bypass it.
      deleting.value = null
      const again = await confirm({
        title: 'Bypass governance retention?',
        description: `${block.message} ${BYPASS_GOVERNANCE_HELP}`,
        confirmLabel: 'Delete with bypass',
        requireText: name,
        icon: 'i-lucide-lock-open'
      })
      if (again) await forceDelete(name, true)
      return
    }
    toast.add({ title: block ? FORCE_DELETE_LOCKED_TITLE : 'Could not delete bucket', description: errorMessage(e), color: 'error', icon: block ? 'i-lucide-lock' : 'i-lucide-circle-alert' })
  } finally {
    deleting.value = null
  }
}

async function deleteBucket(name: string) {
  const ok = await confirm({
    title: `Delete bucket ${name}?`,
    description: 'Only empty buckets can be deleted. The name may become available to others.',
    confirmLabel: 'Delete bucket',
    requireText: name
  })
  if (!ok) return
  deleting.value = name
  try {
    await requireS3().deleteBucket(name)
    toast.add({ title: `Bucket ${name} deleted`, color: 'success', icon: 'i-lucide-circle-check' })
    await refresh()
  } catch (e) {
    if (isNotEmpty(e)) {
      if (canForceDelete.value) {
        deleting.value = null
        const again = await confirm({
          title: `Bucket ${name} is not empty`,
          description: 'You can force delete it: all objects, versions and uploads are removed first.',
          confirmLabel: 'Force delete…',
          color: 'warning'
        })
        if (again) await forceDelete(name)
        return
      }
      toast.add({ title: `Bucket ${name} is not empty`, description: 'Delete its objects (and versions) first.', color: 'warning', icon: 'i-lucide-triangle-alert' })
    } else {
      toast.add({ title: 'Could not delete bucket', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
    }
  } finally {
    deleting.value = null
  }
}

function rowMenu(row: BucketRow): DropdownMenuItem[][] {
  const groups: DropdownMenuItem[][] = [[
    { label: 'Browse objects', icon: 'i-lucide-folder-open', to: objectsLink(row.name) },
    { label: 'Settings', icon: 'i-lucide-settings-2', to: appRoutes.bucketSettings(serverId.value, row.name) }
  ]]
  const danger: DropdownMenuItem[] = [
    { label: 'Delete', icon: 'i-lucide-trash-2', color: 'error', onSelect: () => void deleteBucket(row.name) }
  ]
  if (canForceDelete.value) {
    danger.push({ label: 'Force delete…', icon: 'i-lucide-bomb', color: 'error', onSelect: () => void forceDelete(row.name) })
  }
  groups.push(danger)
  return groups
}

async function onCreated(name: string) {
  await refresh()
  await router.push(objectsLink(name))
}

// Open a bucket by name (for identities that may not list buckets).
const goName = ref('')
const goError = computed(() => (goName.value ? validateBucketName(goName.value) : undefined))
function goToBucket() {
  if (!goName.value || goError.value) return
  void router.push(objectsLink(goName.value))
}

const isAccessDenied = computed(() => {
  const e = error.value
  return (e instanceof S3Error && (e.code === 'AccessDenied' || e.status === 403)) || (e instanceof AdminError && e.status === 403)
})
</script>

<template>
  <div>
    <CommonPageHeader
      title="Buckets"
      icon="i-lucide-database"
      :description="data ? `${data.rows.length} bucket${data.rows.length === 1 ? '' : 's'} on ${connection?.name ?? 'this server'}${totals ? ` · ${formatNumber(totals.objects)} object${totals.objects === 1 ? '' : 's'} · ${formatBytes(totals.bytes)}${usageNote}` : ''}` : `Buckets on ${connection?.name ?? 'this server'}`"
    >
      <template #actions>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="outline"
          aria-label="Refresh bucket list"
          :loading="loading"
          @click="refresh()"
        />
        <UButton icon="i-lucide-plus" label="Create bucket" :disabled="!s3" @click="createOpen = true" />
      </template>
    </CommonPageHeader>

    <div class="mb-4 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
      <UInput
        v-model="query"
        icon="i-lucide-search"
        placeholder="Filter buckets…"
        aria-label="Filter buckets by name"
        class="w-full sm:max-w-xs"
        :ui="{ trailing: 'pe-1' }"
      >
        <template v-if="query" #trailing>
          <UButton color="neutral" variant="link" size="sm" icon="i-lucide-x" aria-label="Clear filter" @click="query = ''" />
        </template>
      </UInput>
      <p v-if="data?.source === 's3' && isGos3 && data.adminError" class="text-xs text-muted">
        Usage figures need the <code>admin:ListBuckets</code> permission.
      </p>
    </div>

    <div v-if="error" class="space-y-4">
      <CommonErrorAlert :error="error" title="Could not list buckets" retryable @retry="refresh()" />
      <UCard v-if="isAccessDenied">
        <form class="flex flex-col gap-3 sm:flex-row sm:items-end" @submit.prevent="goToBucket">
          <UFormField
            label="Open a bucket by name"
            description="Your identity may not list buckets but can still access buckets it has been granted."
            :error="goError"
            class="flex-1"
          >
            <UInput v-model.trim="goName" class="w-full font-mono" placeholder="bucket-name" autocomplete="off" />
          </UFormField>
          <UButton type="submit" icon="i-lucide-arrow-right" label="Open" :disabled="!goName || !!goError" />
        </form>
      </UCard>
    </div>

    <template v-else-if="!data && loading">
      <div class="space-y-2" aria-busy="true" aria-label="Loading buckets">
        <USkeleton v-for="i in 5" :key="i" class="h-11 w-full" />
      </div>
    </template>

    <CommonEmptyState
      v-else-if="data && !data.rows.length"
      icon="i-lucide-database"
      title="No buckets yet"
      description="Create a bucket to start storing objects."
    >
      <UButton icon="i-lucide-plus" label="Create bucket" @click="createOpen = true" />
    </CommonEmptyState>

    <CommonEmptyState
      v-else-if="data && !rows.length"
      icon="i-lucide-search-x"
      title="No matching buckets"
      :description="`No bucket name contains “${query}”.`"
    >
      <UButton color="neutral" variant="outline" label="Clear filter" @click="query = ''" />
    </CommonEmptyState>

    <div v-else-if="data" class="overflow-x-auto rounded-lg border border-default">
      <UTable
        :data="rows"
        :columns="columns"
        :loading="loading"
        caption="Buckets"
        :ui="{ caption: 'sr-only', td: 'py-2.5', th: 'py-2' }"
      >
        <template #name-header>
          <UButton
            color="neutral"
            variant="ghost"
            size="xs"
            class="-ms-2 text-sm font-semibold text-highlighted"
            label="Name"
            :aria-label="sortLabel('name', 'Name')"
            :trailing-icon="sort.by === 'name' ? (sort.desc ? 'i-lucide-arrow-down' : 'i-lucide-arrow-up') : 'i-lucide-arrow-up-down'"
            @click="setSort('name')"
          />
        </template>
        <template #objects-header>
          <UButton
            color="neutral"
            variant="ghost"
            size="xs"
            class="-me-2 text-sm font-semibold text-highlighted"
            label="Objects"
            :aria-label="sortLabel('objects', 'Objects')"
            :trailing-icon="sort.by === 'objects' ? (sort.desc ? 'i-lucide-arrow-down' : 'i-lucide-arrow-up') : 'i-lucide-arrow-up-down'"
            @click="setSort('objects')"
          />
        </template>
        <template #bytes-header>
          <UButton
            color="neutral"
            variant="ghost"
            size="xs"
            class="-me-2 text-sm font-semibold text-highlighted"
            label="Size"
            :aria-label="sortLabel('bytes', 'Size')"
            :trailing-icon="sort.by === 'bytes' ? (sort.desc ? 'i-lucide-arrow-down' : 'i-lucide-arrow-up') : 'i-lucide-arrow-up-down'"
            @click="setSort('bytes')"
          />
        </template>
        <template #created-header>
          <UButton
            color="neutral"
            variant="ghost"
            size="xs"
            class="-ms-2 text-sm font-semibold text-highlighted"
            label="Created"
            :aria-label="sortLabel('created', 'Created')"
            :trailing-icon="sort.by === 'created' ? (sort.desc ? 'i-lucide-arrow-down' : 'i-lucide-arrow-up') : 'i-lucide-arrow-up-down'"
            @click="setSort('created')"
          />
        </template>

        <template #name-cell="{ row }">
          <div class="flex min-w-0 items-center gap-2">
            <UIcon name="i-lucide-database" class="size-4 shrink-0 text-muted" aria-hidden="true" />
            <NuxtLink :to="objectsLink(row.original.name)" class="truncate font-medium text-highlighted hover:underline focus-visible:underline">
              {{ row.original.name }}
            </NuxtLink>
            <UBadge v-if="row.original.public" color="warning" variant="subtle" size="sm" label="Public" icon="i-lucide-globe" />
            <UBadge v-if="row.original.objectLock" color="info" variant="subtle" size="sm" label="Object lock" icon="i-lucide-lock" />
          </div>
        </template>
        <template #versioning-cell="{ row }">
          <UBadge v-if="row.original.versioning === 'Enabled'" color="success" variant="subtle" size="sm" label="Enabled" />
          <UBadge v-else-if="row.original.versioning === 'Suspended'" color="warning" variant="subtle" size="sm" label="Suspended" />
          <span v-else class="text-sm text-muted">Off</span>
        </template>
        <template #placement-cell="{ row }">
          <div class="flex flex-wrap items-center gap-1" data-testid="placement-chips">
            <UTooltip v-for="chip in placementChips(row.original.placement)" :key="chip.label" :text="chip.title">
              <UBadge :label="chip.label" :color="chip.color" :icon="chip.icon" variant="subtle" size="sm" />
            </UTooltip>
          </div>
        </template>
        <template #objects-cell="{ row }">
          <span class="tabular-nums">{{ row.original.usage ? formatNumber(row.original.usage.objects) : '—' }}</span>
        </template>
        <template #bytes-cell="{ row }">
          <CommonByteSize v-if="row.original.usage" :bytes="row.original.usage.bytes" class="tabular-nums" />
          <span v-else class="text-muted">—</span>
        </template>
        <template #region-cell="{ row }">
          <span class="font-mono text-xs">{{ row.original.region || '—' }}</span>
        </template>
        <template #created-cell="{ row }">
          <CommonRelativeTime :value="row.original.createdAt" class="text-sm text-muted" />
        </template>
        <template #actions-cell="{ row }">
          <div class="flex items-center justify-end gap-1">
            <UButton
              :to="appRoutes.bucketSettings(serverId, row.original.name)"
              icon="i-lucide-settings-2"
              color="neutral"
              variant="ghost"
              size="sm"
              :aria-label="`Settings of bucket ${row.original.name}`"
              class="hidden sm:inline-flex"
            />
            <UDropdownMenu :items="rowMenu(row.original)" :content="{ align: 'end' }">
              <UButton
                icon="i-lucide-ellipsis-vertical"
                color="neutral"
                variant="ghost"
                size="sm"
                :loading="deleting === row.original.name"
                :aria-label="`Actions for bucket ${row.original.name}`"
              />
            </UDropdownMenu>
          </div>
        </template>
      </UTable>
    </div>

    <BrowserCreateBucketModal
      v-model:open="createOpen"
      :client="s3"
      :default-region="connection?.region"
      :existing-names="data?.rows.map(r => r.name)"
      @created="onCreated"
    />
  </div>
</template>
