<script setup lang="ts">
/**
 * Server connections: list with live status, add / edit (with connection
 * test), duplicate, set default, sign in/out, delete, import / export JSON.
 * Presets (NUXT_PUBLIC_SERVERS / servers.json) and the embedded server are
 * defined by configuration: only their credentials can be edited.
 */
import type { DropdownMenuItem, TableColumn } from '@nuxt/ui'
import type { Connection, ConnectionInput, ConnectionSource } from '~/types/connection'
import { useConnectionsStore } from '~/stores/connections'
import { connectionColor, duplicateInput, isDefinitionLocked } from '~/components/servers/-form'
import { defaultFirst, useDefaultServer } from '~/components/servers/-default'

useHead({ title: 'Servers' })

const store = useConnectionsStore()
const toast = useToast()
const confirm = useConfirm()
const { defaultId, setDefault } = useDefaultServer()

const search = ref('')
const rows = computed(() => {
  const q = search.value.trim().toLowerCase()
  const list = defaultFirst(store.sorted, defaultId.value)
  if (!q) return list
  return list.filter(c => c.name.toLowerCase().includes(q) || c.endpoint.toLowerCase().includes(q) || c.region.toLowerCase().includes(q) || (c.serverId ?? '').includes(q))
})

const refreshing = ref(false)
async function refreshAll() {
  refreshing.value = true
  try {
    await store.refreshAll()
  } finally {
    refreshing.value = false
  }
}

onMounted(() => {
  for (const c of store.connections) void store.ensureStatus(c.id)
})

// -- modals ----------------------------------------------------------------------

const formOpen = ref(false)
const editing = ref<Connection | undefined>()
const prefill = ref<ConnectionInput | undefined>()
const importOpen = ref(false)
const exportOpen = ref(false)

function openAdd(input?: ConnectionInput) {
  editing.value = undefined
  prefill.value = input
  formOpen.value = true
}

function openEdit(c: Connection) {
  prefill.value = undefined
  editing.value = c
  formOpen.value = true
}

function duplicate(c: Connection) {
  const copy = store.add(duplicateInput(c, store.connections.map(x => x.name)))
  toast.add({ title: `Duplicated as ${copy.name}`, color: 'success', icon: 'i-lucide-copy' })
  void store.refreshStatus(copy.id)
  openEdit(copy)
}

function makeDefault(c: Connection) {
  setDefault(c.id)
  store.setActive(c.id)
  toast.add({ title: `${c.name} is now the default server`, icon: 'i-lucide-star', color: 'success' })
}

function signOut(c: Connection) {
  store.clearCredentials(c.id)
  toast.add({ title: `Signed out of ${c.name}`, description: 'The secret key was forgotten on this device.', icon: 'i-lucide-log-out' })
}

async function remove(c: Connection) {
  const extra = c.source === 'preset'
    ? ' This preset stays hidden in this browser until you add it again.'
    : c.source === 'embedded'
      ? ' The hosting server is added again the next time this UI is opened.'
      : ''
  const ok = await confirm({
    title: `Remove ${c.name}?`,
    description: `The connection and any credentials stored for it are removed from this browser. Nothing on the server is changed.${extra}`,
    confirmLabel: 'Remove',
    icon: 'i-lucide-trash-2'
  })
  if (!ok) return
  store.remove(c.id)
  if (defaultId.value === c.id) setDefault(null)
  toast.add({ title: `${c.name} removed`, icon: 'i-lucide-trash-2' })
}

function rowMenu(c: Connection): DropdownMenuItem[][] {
  const connected = hasCredentials(c)
  return [
    [
      connected
        ? { label: 'Open', icon: 'i-lucide-arrow-right', to: appRoutes.server(c.id) }
        : { label: 'Sign in', icon: 'i-lucide-log-in', to: appRoutes.connect(c.id) },
      { label: 'Edit / test', icon: 'i-lucide-pencil', onSelect: () => openEdit(c) },
      { label: 'Check status', icon: 'i-lucide-refresh-cw', onSelect: () => void store.refreshStatus(c.id) },
      { label: 'Duplicate', icon: 'i-lucide-copy', onSelect: () => duplicate(c) },
      defaultId.value === c.id
        ? { label: 'Default server', icon: 'i-lucide-star', disabled: true }
        : { label: 'Set as default', icon: 'i-lucide-star', onSelect: () => makeDefault(c) }
    ],
    [
      ...(connected ? [{ label: 'Sign out', icon: 'i-lucide-log-out', onSelect: () => signOut(c) }] : []),
      { label: 'Remove', icon: 'i-lucide-trash-2', color: 'error' as const, onSelect: () => void remove(c) }
    ]
  ]
}

const columns: TableColumn<Connection>[] = [
  { accessorKey: 'name', header: 'Name' },
  { accessorKey: 'endpoint', header: 'Endpoint' },
  { id: 'status', header: 'Status' },
  { accessorKey: 'region', header: 'Region' },
  { id: 'credentials', header: 'Credentials' },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, 'Actions') }
]

function credentialBadge(c: Connection): { label: string, color: 'success' | 'info' | 'neutral', title: string } {
  if (!hasCredentials(c)) return { label: 'Not signed in', color: 'neutral', title: 'No secret key on this device' }
  if (c.remember) return { label: 'Remembered', color: 'success', title: 'Secret key stored in this browser (localStorage)' }
  return { label: 'This session', color: 'info', title: 'Secret key kept until the browser session ends (sessionStorage)' }
}

interface SourceBadge { label: string, color: 'info' | 'primary' | 'neutral', title: string }
const SOURCE_BADGES: Partial<Record<ConnectionSource, SourceBadge>> = {
  preset: { label: 'Preset', color: 'info', title: 'Defined by the UI configuration (read-only definition)' },
  embedded: { label: 'This server', color: 'primary', title: 'The gos3 server hosting this UI' },
  discovered: { label: 'Discovered', color: 'neutral', title: 'Added from discovery' }
}
</script>

<template>
  <div>
    <CommonPageHeader title="Servers" icon="i-lucide-server" description="S3 servers this console manages. Connections and credentials are stored in this browser only.">
      <template #actions>
        <UButton icon="i-lucide-plus" label="Add server" @click="openAdd()" />
        <UButton color="neutral" variant="outline" icon="i-lucide-upload" label="Import" @click="importOpen = true" />
        <UButton color="neutral" variant="outline" icon="i-lucide-download" label="Export" :disabled="!store.connections.length" @click="exportOpen = true" />
        <UButton color="neutral" variant="ghost" icon="i-lucide-scan-search" label="Discover" :to="appRoutes.discover()" />
      </template>
    </CommonPageHeader>

    <CommonEmptyState
      v-if="!store.connections.length"
      icon="i-lucide-server"
      title="No servers yet"
      description="Add a gos3 server or any S3-compatible service (AWS S3, MinIO, Ceph …), import a JSON export, or discover gos3 servers on your network."
    >
      <UButton icon="i-lucide-plus" label="Add server" @click="openAdd()" />
      <UButton color="neutral" variant="outline" icon="i-lucide-upload" label="Import" @click="importOpen = true" />
      <UButton color="neutral" variant="outline" icon="i-lucide-scan-search" label="Discover" :to="appRoutes.discover()" />
    </CommonEmptyState>

    <div v-else class="space-y-3">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <UInput
          v-model="search"
          icon="i-lucide-search"
          placeholder="Filter servers…"
          aria-label="Filter servers"
          class="w-full sm:w-72"
        />
        <UButton color="neutral" variant="ghost" icon="i-lucide-refresh-cw" label="Check all" :loading="refreshing" @click="refreshAll" />
      </div>

      <UTable :data="rows" :columns="columns" class="rounded-md border border-default" :ui="{ td: 'py-2.5' }">
        <template #empty>
          <p class="py-6 text-center text-sm text-muted">
            No server matches “{{ search }}”.
          </p>
        </template>
        <template #name-cell="{ row }">
          <div class="flex min-w-44 items-center gap-2">
            <span
              class="size-2.5 shrink-0 rounded-full"
              :class="connectionColor(row.original.color) ? '' : 'bg-neutral-300 dark:bg-neutral-600'"
              :style="connectionColor(row.original.color) ? { backgroundColor: connectionColor(row.original.color) } : undefined"
              aria-hidden="true"
            />
            <div class="min-w-0">
              <p class="flex flex-wrap items-center gap-1.5">
                <NuxtLink
                  :to="hasCredentials(row.original) ? appRoutes.server(row.original.id) : appRoutes.connect(row.original.id)"
                  class="truncate font-medium text-highlighted hover:underline focus-visible:underline"
                >
                  {{ row.original.name }}
                </NuxtLink>
                <UIcon v-if="defaultId === row.original.id" name="i-lucide-star" class="size-3.5 text-warning" aria-label="Default server" />
                <UBadge :label="row.original.kind === 'gos3' ? 'gos3' : 'S3'" :color="row.original.kind === 'gos3' ? 'primary' : 'neutral'" variant="outline" size="sm" />
                <UTooltip v-if="SOURCE_BADGES[row.original.source]" :text="SOURCE_BADGES[row.original.source]!.title">
                  <UBadge
                    :label="SOURCE_BADGES[row.original.source]!.label"
                    :color="SOURCE_BADGES[row.original.source]!.color"
                    :icon="isDefinitionLocked(row.original) ? 'i-lucide-lock' : undefined"
                    variant="subtle"
                    size="sm"
                    tabindex="0"
                  />
                </UTooltip>
              </p>
              <p v-if="store.statusOf(row.original.id).info?.version" class="text-xs text-muted">
                v{{ store.statusOf(row.original.id).info?.version }}
              </p>
            </div>
          </div>
        </template>
        <template #endpoint-cell="{ row }">
          <div class="flex items-center gap-1">
            <code class="truncate font-mono text-xs">{{ row.original.endpoint }}</code>
            <CommonCopyButton :value="row.original.endpoint" :aria-label="`Copy endpoint of ${row.original.name}`" />
          </div>
        </template>
        <template #status-cell="{ row }">
          <UTooltip :text="store.statusOf(row.original.id).error" :disabled="!store.statusOf(row.original.id).error">
            <span class="inline-flex items-center gap-2 whitespace-nowrap text-sm">
              <CommonStatusDot :state="store.statusOf(row.original.id).state" show-label />
              <span v-if="store.statusOf(row.original.id).state === 'online' && store.statusOf(row.original.id).latencyMs !== undefined" class="text-xs text-muted">
                {{ store.statusOf(row.original.id).latencyMs }} ms
              </span>
            </span>
          </UTooltip>
        </template>
        <template #region-cell="{ row }">
          <span class="font-mono text-xs">{{ row.original.region }}</span>
        </template>
        <template #credentials-cell="{ row }">
          <div class="flex flex-col gap-0.5">
            <UBadge :label="credentialBadge(row.original).label" :color="credentialBadge(row.original).color" variant="subtle" size="sm" :title="credentialBadge(row.original).title" class="w-fit" />
            <span v-if="row.original.accessKeyId" class="truncate font-mono text-xs text-muted">{{ row.original.accessKeyId }}</span>
          </div>
        </template>
        <template #actions-cell="{ row }">
          <div class="flex justify-end gap-1">
            <UButton
              v-if="!hasCredentials(row.original)"
              size="xs"
              icon="i-lucide-log-in"
              label="Sign in"
              :to="appRoutes.connect(row.original.id)"
            />
            <UButton
              size="xs"
              color="neutral"
              variant="ghost"
              icon="i-lucide-pencil"
              :aria-label="`Edit ${row.original.name}`"
              @click="openEdit(row.original)"
            />
            <UDropdownMenu :items="rowMenu(row.original)" :content="{ align: 'end' }">
              <UButton size="xs" color="neutral" variant="ghost" icon="i-lucide-ellipsis-vertical" :aria-label="`More actions for ${row.original.name}`" />
            </UDropdownMenu>
          </div>
        </template>
      </UTable>
      <p class="text-xs text-muted">
        <UIcon name="i-lucide-lock" class="size-3 align-[-1px]" aria-hidden="true" /> Presets and the hosting server are defined by configuration; only their credentials can be changed here.
      </p>
    </div>

    <ServersFormModal v-model:open="formOpen" :connection="editing" :prefill="prefill" />
    <ServersImportModal v-model:open="importOpen" />
    <ServersExportModal v-model:open="exportOpen" />
  </div>
</template>
