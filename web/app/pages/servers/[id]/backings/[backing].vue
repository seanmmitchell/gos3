<script setup lang="ts">
/**
 * One storage backing (from GET /backings, admin:ListBackings). For an s3
 * backing — a connection to a remote S3 service's account — it has three
 * sections, picked with the chips under the header (kept in `?tab=`):
 * - Overview: the home (or records kept locally), the default bucket kind
 *   and container, health and counts;
 * - Remote buckets (admin:ListRemoteBuckets): the account's buckets, to
 *   adopt as native buckets, use as containers or create wrapped buckets in;
 * - Containers: where wrapped buckets are stored.
 *
 * `?adopt=<remote bucket>&bucket=<gos3 name>` (from the create-bucket
 * dialog) opens the adopt dialog prefilled, on any tab: adopting needs
 * neither ListBackings nor ListRemoteBuckets.
 *
 * The Remote buckets tab is shown to everyone who sees the page, since
 * admin:ListRemoteBuckets may be granted for this backing only (whoami
 * reports capabilities for all backings); the tab explains a 403. Changes
 * to buckets or containers reload the backing and whoami (whose backing
 * choices list the containers).
 */
import type { Backing } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { useConnectionsStore } from '~/stores/connections'
import { canViewStorage, identityPending, remoteAccess } from '~/components/admin/-capabilities'
import { containerName, findContainer, remoteLocationUri } from '~/components/admin/-containers'
import { PLACEMENT_POLL_MS } from '~/components/admin/-storage'
import AdoptRemoteBucketModal from '~/components/admin/AdoptRemoteBucketModal.vue'

type BackingTab = 'overview' | 'remote' | 'containers'

const route = useRoute()
const router = useRouter()
const id = computed(() => routeParam(route.params.id))
const name = computed(() => routeParam(route.params.backing))
const { connection, isGos3, requireAdmin } = useConnection()
const { capabilities, status } = useWhoAmI()
const store = useConnectionsStore()
const overlay = useOverlay()

useHead({ title: () => `${name.value} · Storage` })

const pending = computed(() => identityPending(connection.value?.kind, status.value))
const allowed = computed(() => canViewStorage(connection.value?.kind, capabilities.value))
const access = computed(() => remoteAccess(connection.value?.kind, capabilities.value))

// --- loading ------------------------------------------------------------------------

const loader = useLoader(() => requireAdmin().listBackings(), { immediate: false })
const backing = computed<Backing | undefined>(() => loader.data.value?.backings?.find(b => b.name === name.value))
const notFound = computed(() => !!loader.data.value && !backing.value)
const accessDenied = computed(() => loader.error.value instanceof AdminError && loader.error.value.isAccessDenied)
const isS3 = computed(() => backing.value?.kind === 's3')

watch([id, name, allowed], ([, , ok]) => {
  loader.data.value = undefined
  if (ok) void loader.refresh()
}, { immediate: true })

function refresh() {
  void loader.refresh()
}

/** Buckets or containers changed: reload the backing and whoami (backing choices and their containers). */
function changed() {
  refresh()
  if (id.value) void store.refreshStatus(id.value)
}

// Poll while a container is loading (it becomes usable on its own).
const containersLoading = computed(() => (backing.value?.account?.containers ?? []).some(c => c.state === 'loading'))
let timer: ReturnType<typeof setInterval> | undefined
function stopTimer() {
  if (timer) clearInterval(timer)
  timer = undefined
}
watch(containersLoading, (busy) => {
  stopTimer()
  if (!busy) return
  timer = setInterval(() => {
    if (document.visibilityState === 'visible' && !loader.loading.value) refresh()
  }, PLACEMENT_POLL_MS)
}, { immediate: true })
onBeforeUnmount(stopTimer)

// --- sections -------------------------------------------------------------------------

const tabs = computed(() => {
  return [
    { value: 'overview', label: 'Overview', icon: 'i-lucide-info' },
    { value: 'remote', label: 'Remote buckets', icon: 'i-lucide-cloud' },
    { value: 'containers', label: 'Containers', icon: 'i-lucide-package' }
  ] as { value: BackingTab, label: string, icon: string }[]
})
const tab = computed<BackingTab>({
  get: () => {
    const q = route.query.tab
    const v = Array.isArray(q) ? q[0] : q
    return tabs.value.find(t => t.value === v)?.value ?? 'overview'
  },
  set: (v) => {
    void router.replace({ query: { ...route.query, tab: v === 'overview' ? undefined : v } })
  }
})
const tabLabel = computed(() => tabs.value.find(t => t.value === tab.value)?.label)

// "Adopt an existing remote bucket…" from the create-bucket dialog: read
// once, opened when the backing is known to be an s3 backing.
const initialAdopt = ref<{ remoteBucket: string, bucket?: string } | null>(null)
{
  const { adopt, bucket } = route.query
  if (typeof adopt === 'string') {
    initialAdopt.value = { remoteBucket: adopt, bucket: typeof bucket === 'string' && bucket ? bucket : undefined }
    void router.replace({ query: { ...route.query, adopt: undefined, bucket: undefined } })
  }
}
/** Remounts the remote bucket list (after an adoption made here). */
const remoteKey = ref(0)
watch(isS3, async (s3) => {
  const init = initialAdopt.value
  if (!s3 || !init) return
  initialAdopt.value = null
  const adopted = await overlay.create(AdoptRemoteBucketModal, { destroyOnClose: true }).open({
    backing: name.value,
    remoteBucket: init.remoteBucket,
    bucketName: init.bucket
  })
  if (adopted) {
    changed()
    remoteKey.value++
  }
}, { immediate: true })

// --- overview -------------------------------------------------------------------------

const account = computed(() => backing.value?.account)
const home = computed(() => (account.value?.home ? remoteLocationUri(account.value.home) : ''))
const localRecords = computed(() => account.value ? account.value.records === 'local' || !account.value.home : backing.value?.location === 'local:')
const defaultKind = computed(() => backing.value?.defaultKind || account.value?.defaultKind || '')
const kinds = computed(() => (backing.value?.kinds ?? account.value?.kinds ?? []).map(k => bucketKindInfo(k).label).join(', '))
const containers = computed(() => account.value?.containers ?? [])
const defaultContainerInfo = computed(() => findContainer(containers.value, account.value?.defaultContainer) ?? containers.value.find(c => c.default))
const noContainer = computed(() => !!account.value && !containers.value.length)

function showContainers() {
  tab.value = 'containers'
}
</script>

<template>
  <div class="space-y-6">
    <CommonPageHeader
      :title="name"
      icon="i-lucide-cloud"
      :description="backing ? `${backingKindInfo(backing).label} backing of ${connection?.name ?? 'this server'}` : 'Storage backing'"
    >
      <template v-if="backing" #badge>
        <UBadge v-if="backing.default" label="Default" color="primary" variant="subtle" size="sm" icon="i-lucide-star" />
        <UBadge
          :label="backing.healthy ? 'Healthy' : 'Unhealthy'"
          :color="backing.healthy ? 'success' : 'error'"
          variant="subtle"
          size="sm"
          :icon="backing.healthy ? 'i-lucide-circle-check' : 'i-lucide-unplug'"
        />
      </template>
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
        <UButton color="neutral" variant="ghost" icon="i-lucide-hard-drive" label="Storage" :to="appRoutes.storage(id)" />
      </template>
    </CommonPageHeader>

    <CommonEmptyState
      v-if="!isGos3"
      icon="i-lucide-server-off"
      title="Storage backings need a gos3 server"
      description="This connection is a generic S3 server."
    />
    <div v-else-if="pending" class="space-y-3" aria-busy="true">
      <USkeleton class="h-10 w-full" />
      <USkeleton class="h-40 w-full" />
    </div>
    <CommonEmptyState
      v-else-if="!allowed || accessDenied"
      icon="i-lucide-lock"
      title="No access to storage"
      description="Your identity is not allowed to list this server's storage backings (admin:ListBackings). Ask an administrator for the diagnostics or admin policy."
    />
    <CommonErrorAlert v-else-if="loader.error.value && !loader.data.value" :error="loader.error.value" title="Could not load the backing" retryable @retry="refresh" />
    <div v-else-if="!loader.data.value" class="space-y-3" aria-busy="true" aria-label="Loading the backing">
      <USkeleton class="h-10 w-full" />
      <USkeleton class="h-40 w-full" />
    </div>
    <CommonEmptyState
      v-else-if="notFound || !backing"
      icon="i-lucide-hard-drive"
      title="Backing not found"
      :description="`This server has no storage backing named “${name}”.`"
    >
      <UButton size="sm" color="neutral" variant="outline" icon="i-lucide-arrow-left" label="Storage" :to="appRoutes.storage(id)" />
    </CommonEmptyState>
    <CommonEmptyState
      v-else-if="!isS3"
      :icon="backingKindInfo(backing).icon"
      title="Nothing to manage here"
      :description="`${backing.name} is a ${backingKindInfo(backing).label.toLowerCase()} backing. Only S3 backings have remote buckets and containers; bucket placement is set in each bucket's settings.`"
    >
      <UButton size="sm" color="neutral" variant="outline" icon="i-lucide-arrow-left" label="Storage" :to="appRoutes.storage(id)" />
    </CommonEmptyState>

    <template v-else>
      <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Refresh failed; showing the last known state" retryable @retry="refresh" />
      <CommonSectionChips v-model="tab" :items="tabs" label="Backing sections" panel-id="backing-panel" />

      <div id="backing-panel" role="tabpanel" :aria-label="tabLabel" class="space-y-6">
        <template v-if="tab === 'overview'">
          <UAlert
            v-if="!backing.healthy"
            color="error"
            variant="subtle"
            icon="i-lucide-unplug"
            title="Unhealthy"
            :description="backing.lastError || 'The last health check or operation failed.'"
          />
          <UAlert
            v-if="noContainer"
            color="info"
            variant="subtle"
            icon="i-lucide-package-x"
            title="No container for wrapped buckets"
            :description="`${backing.name} keeps its records on this server and has no container, so new buckets on it are native. ${access.containers ? 'Add a container to create wrapped buckets.' : 'An administrator can add a container for wrapped buckets.'}`"
            :actions="[{ label: 'Containers', color: 'neutral', variant: 'outline', onClick: showContainers }]"
            data-testid="no-container"
          />
          <div class="grid gap-6 lg:grid-cols-2">
            <UCard>
              <template #header>
                <h2 class="font-semibold text-highlighted">
                  Account
                </h2>
              </template>
              <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-3 text-sm" data-testid="backing-account">
                <dt class="text-muted">
                  Home
                </dt>
                <dd>
                  <span v-if="home" class="font-mono text-xs">{{ home }}</span>
                  <span v-else-if="localRecords">None: records kept locally (the server's system directory)</span>
                  <span v-else class="font-mono text-xs">{{ backing.location || '—' }}</span>
                </dd>
                <template v-if="defaultKind">
                  <dt class="text-muted">
                    Default bucket kind
                  </dt>
                  <dd>
                    <UBadge :label="bucketKindInfo(defaultKind).label" :icon="bucketKindInfo(defaultKind).icon" color="neutral" variant="subtle" size="sm" />
                    <p class="mt-1 text-xs text-muted">
                      {{ bucketKindInfo(defaultKind).description }}
                    </p>
                  </dd>
                </template>
                <template v-if="kinds">
                  <dt class="text-muted">
                    Bucket kinds offered
                  </dt>
                  <dd>{{ kinds }}</dd>
                </template>
                <template v-if="account">
                  <dt class="text-muted">
                    Default container
                  </dt>
                  <dd>
                    <template v-if="defaultContainerInfo">
                      {{ containerName(defaultContainerInfo) }}
                      <span class="block font-mono text-xs text-muted">{{ remoteLocationUri(defaultContainerInfo.location) }}</span>
                    </template>
                    <span v-else class="text-muted">None</span>
                  </dd>
                </template>
              </dl>
            </UCard>
            <UCard>
              <template #header>
                <h2 class="font-semibold text-highlighted">
                  Status
                </h2>
              </template>
              <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-3 text-sm">
                <dt class="text-muted">
                  Health
                </dt>
                <dd class="flex items-center gap-2">
                  <CommonStatusDot :state="backing.healthy ? 'healthy' : 'unhealthy'" show-label />
                </dd>
                <dt class="text-muted">
                  Checked
                </dt>
                <dd><CommonRelativeTime :value="backing.checkedAt" /></dd>
                <dt class="text-muted">
                  Buckets
                </dt>
                <dd class="tabular-nums">
                  {{ formatNumber(backing.buckets) }} primary · {{ formatNumber(backing.replicas) }} replica{{ backing.replicas === 1 ? '' : 's' }}
                  <span v-if="account" class="block text-xs text-muted">{{ formatNumber(account.nativeBuckets) }} native · {{ formatNumber(account.wrappedBuckets) }} wrapped</span>
                </dd>
                <template v-if="account">
                  <dt class="text-muted">
                    Containers
                  </dt>
                  <dd class="tabular-nums">
                    {{ formatNumber(containers.length) }}
                  </dd>
                </template>
              </dl>
            </UCard>
          </div>
        </template>

        <AdminRemoteBucketsTable
          v-else-if="tab === 'remote'"
          :key="remoteKey"
          :backing="backing"
          :server-id="id"
          @changed="changed"
          @navigate="tab = $event"
        />

        <AdminBackingContainers v-else-if="tab === 'containers'" :backing="backing" @changed="changed" />
      </div>
    </template>
  </div>
</template>
