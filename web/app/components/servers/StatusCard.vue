<script setup lang="ts">
/**
 * Dashboard card of one connection: live status, identity, version/region
 * and bucket / object statistics when the identity may read them.
 *
 *   <ServersStatusCard :connection="c" :is-default="..." @edit="..." @set-default="..." />
 */
import type { DropdownMenuItem } from '@nuxt/ui'
import type { Connection } from '~/types/connection'
import { useConnectionsStore } from '~/stores/connections'
import { connectionColor } from './-form'

const props = withDefaults(defineProps<{
  connection: Connection
  isDefault?: boolean
}>(), { isDefault: false })

const emit = defineEmits<{
  'edit': [connection: Connection]
  'set-default': [connection: Connection]
}>()

const store = useConnectionsStore()

const status = computed(() => store.statusOf(props.connection.id))
const connected = computed(() => hasCredentials(props.connection))
const info = computed(() => status.value.whoami?.server ?? status.value.info)
const whoami = computed(() => status.value.whoami)
const caps = computed(() => store.capabilitiesOf(props.connection.id))
const color = computed(() => connectionColor(props.connection.color))

interface CardStats {
  buckets: number
  objects?: number
  bytes?: number
  /** ListBuckets denied for this identity. */
  denied?: boolean
}
const stats = ref<CardStats | null>(null)
const statsLoading = ref(false)
let statsFor = ''

async function loadStats() {
  const c = props.connection
  if (!connected.value || status.value.state !== 'online') {
    stats.value = null
    return
  }
  statsLoading.value = true
  try {
    if (c.kind === 'gos3' && caps.value.viewStats) {
      const s = await getAdminClient(c).stats()
      stats.value = { buckets: s.storage.buckets, objects: s.storage.objects, bytes: s.storage.bytes }
    } else {
      const r = await getS3Client(c).listBuckets()
      stats.value = { buckets: r.buckets.length }
    }
  } catch (e) {
    stats.value = isAccessDenied(e) ? { buckets: 0, denied: true } : null
  } finally {
    statsLoading.value = false
  }
}

function isAccessDenied(e: unknown): boolean {
  const d = describeError(e)
  return d.status === 403 || d.code === 'AccessDenied'
}

// (Re)load statistics whenever the connection comes online or its identity changes.
watch(() => [status.value.state, status.value.checkedAt, props.connection.accessKeyId, props.connection.endpoint] as const, ([state, checkedAt]) => {
  if (state !== 'online') return
  const key = `${props.connection.id}:${checkedAt}`
  if (key === statsFor) return
  statsFor = key
  void loadStats()
}, { immediate: true })

onMounted(() => {
  void store.ensureStatus(props.connection.id)
})

const sourceBadge = computed(() => {
  switch (props.connection.source) {
    case 'embedded': return { label: 'This server', color: 'primary' as const }
    case 'preset': return { label: 'Preset', color: 'info' as const }
    case 'discovered': return { label: 'Discovered', color: 'neutral' as const }
    default: return null
  }
})

const identity = computed(() => {
  if (!connected.value) return 'Not signed in'
  if (whoami.value) return whoami.value.isRoot ? `${whoami.value.user || 'root'} (root)` : whoami.value.user
  return props.connection.accessKeyId
})

const menu = computed<DropdownMenuItem[][]>(() => [
  [
    { label: 'Edit', icon: 'i-lucide-pencil', onSelect: () => emit('edit', props.connection) },
    { label: 'Refresh status', icon: 'i-lucide-refresh-cw', onSelect: () => void store.refreshStatus(props.connection.id) },
    props.isDefault
      ? { label: 'Default server', icon: 'i-lucide-star', disabled: true }
      : { label: 'Set as default', icon: 'i-lucide-star', onSelect: () => emit('set-default', props.connection) }
  ],
  connected.value
    ? [{ label: 'Sign out', icon: 'i-lucide-log-out', onSelect: () => store.clearCredentials(props.connection.id) }]
    : [{ label: 'Sign in', icon: 'i-lucide-log-in', to: appRoutes.connect(props.connection.id) }]
])
</script>

<template>
  <UCard
    as="article"
    :aria-label="`Server ${connection.name}`"
    class="relative flex h-full flex-col overflow-hidden"
    :ui="{ body: 'flex-1 space-y-3', footer: 'flex flex-wrap items-center gap-2' }"
  >
    <span v-if="color" class="absolute inset-y-0 left-0 w-1" :style="{ backgroundColor: color }" aria-hidden="true" />
    <template #header>
      <div class="flex items-start gap-3">
        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-1.5">
            <UIcon v-if="isDefault" name="i-lucide-star" class="size-4 text-warning" aria-label="Default server" />
            <h3 class="truncate text-base font-semibold text-highlighted">
              <NuxtLink :to="connected ? appRoutes.server(connection.id) : appRoutes.connect(connection.id)" class="hover:underline focus-visible:underline">
                {{ connection.name }}
              </NuxtLink>
            </h3>
            <UBadge :label="connection.kind === 'gos3' ? 'gos3' : 'S3'" :color="connection.kind === 'gos3' ? 'primary' : 'neutral'" variant="outline" size="sm" />
            <UBadge v-if="sourceBadge" :label="sourceBadge.label" :color="sourceBadge.color" variant="subtle" size="sm" />
          </div>
          <p class="mt-0.5 truncate font-mono text-xs text-muted" :title="connection.endpoint">
            {{ connection.endpoint }}
          </p>
        </div>
        <UDropdownMenu :items="menu" :content="{ align: 'end' }">
          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-ellipsis-vertical" :aria-label="`More actions for ${connection.name}`" />
        </UDropdownMenu>
      </div>
    </template>

    <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm">
      <CommonStatusDot :state="status.state" show-label />
      <span v-if="status.latencyMs !== undefined && status.state === 'online'" class="text-xs text-muted">{{ status.latencyMs }} ms</span>
      <span v-if="status.checkedAt" class="text-xs text-dimmed">checked <CommonRelativeTime :value="status.checkedAt" /></span>
    </div>
    <p v-if="status.error && status.state !== 'online'" class="line-clamp-2 text-xs text-error" :title="status.error">
      {{ status.error }}
    </p>

    <dl class="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
      <div>
        <dt class="text-xs text-muted">
          Version
        </dt>
        <dd class="truncate">
          {{ info?.version ?? (connection.kind === 's3' ? '—' : 'unknown') }}
        </dd>
      </div>
      <div>
        <dt class="text-xs text-muted">
          Region
        </dt>
        <dd class="truncate font-mono text-xs leading-5">
          {{ info?.region || connection.region }}
        </dd>
      </div>
      <div class="col-span-2">
        <dt class="text-xs text-muted">
          Identity
        </dt>
        <dd class="flex flex-wrap items-center gap-1">
          <span class="truncate" :class="connected ? '' : 'text-dimmed'">{{ identity }}</span>
          <UBadge v-for="p in whoami?.policies ?? []" :key="p" :label="p" size="sm" color="neutral" variant="soft" />
        </dd>
      </div>
    </dl>

    <div v-if="connected && status.state === 'online'" class="grid grid-cols-3 gap-2 rounded-md bg-elevated/50 p-2 text-center" aria-label="Statistics">
      <template v-if="statsLoading && !stats">
        <USkeleton v-for="i in 3" :key="i" class="mx-auto h-9 w-14" />
      </template>
      <template v-else-if="stats && !stats.denied">
        <div>
          <p class="text-lg font-semibold text-highlighted">
            {{ formatNumber(stats.buckets) }}
          </p>
          <p class="text-xs text-muted">
            buckets
          </p>
        </div>
        <div>
          <p class="text-lg font-semibold text-highlighted">
            {{ stats.objects !== undefined ? formatCompact(stats.objects) : '—' }}
          </p>
          <p class="text-xs text-muted">
            objects
          </p>
        </div>
        <div>
          <p class="text-lg font-semibold text-highlighted">
            <CommonByteSize v-if="stats.bytes !== undefined" :bytes="stats.bytes" />
            <template v-else>
              —
            </template>
          </p>
          <p class="text-xs text-muted">
            stored
          </p>
        </div>
      </template>
      <p v-else class="col-span-3 py-2 text-xs text-muted">
        {{ stats?.denied ? 'This identity may not list buckets.' : 'Statistics unavailable.' }}
      </p>
    </div>

    <template #footer>
      <template v-if="connected">
        <UButton :to="appRoutes.buckets(connection.id)" icon="i-lucide-folder-open" label="Browse" size="sm" />
        <UButton
          v-if="connection.kind === 'gos3' && caps.admin"
          :to="appRoutes.server(connection.id)"
          icon="i-lucide-layout-dashboard"
          label="Admin"
          size="sm"
          color="neutral"
          variant="outline"
        />
        <UButton
          v-if="connection.kind === 'gos3' && caps.discovery"
          :to="appRoutes.discovery(connection.id)"
          icon="i-lucide-radar"
          :aria-label="`Discovery on ${connection.name}`"
          size="sm"
          color="neutral"
          variant="ghost"
        />
      </template>
      <UButton v-else :to="appRoutes.connect(connection.id)" icon="i-lucide-log-in" label="Sign in" size="sm" />
      <UButton
        v-if="status.state === 'unauthorized'"
        color="warning"
        variant="soft"
        size="sm"
        icon="i-lucide-key-round"
        label="Update credentials"
        @click="emit('edit', connection)"
      />
    </template>
  </UCard>
</template>
