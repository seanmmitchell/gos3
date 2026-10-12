<script setup lang="ts">
/** Aggregated discovery results (Discover page): one row per server ID. */
import type { TableColumn } from '@nuxt/ui'
import { useConnectionsStore } from '~/stores/connections'
import { findConfigured, type DiscoveredServer } from './-aggregate'

withDefaults(defineProps<{
  servers: readonly DiscoveredServer[]
  loading?: boolean
}>(), { loading: false })

const emit = defineEmits<{ add: [server: DiscoveredServer] }>()

const store = useConnectionsStore()
const configured = (s: DiscoveredServer) => findConfigured(s, store.connections)

const columns: TableColumn<DiscoveredServer>[] = [
  { accessorKey: 'name', header: 'Server' },
  { id: 'foundBy', header: 'Found by' },
  { accessorKey: 'healthy', header: 'Status' },
  { accessorKey: 'addresses', header: 'Addresses' },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, 'Actions') }
]

const FINDER_ICONS = {
  server: 'i-lucide-server',
  local: 'i-lucide-monitor',
  browser: 'i-lucide-globe'
} as const
</script>

<template>
  <UTable :data="[...servers]" :columns="columns" :loading="loading" class="rounded-md border border-default" :ui="{ td: 'py-2 align-top' }">
    <template #empty>
      <p class="py-6 text-center text-sm text-muted">
        No servers found yet.
      </p>
    </template>
    <template #name-cell="{ row }">
      <div class="min-w-40">
        <p class="flex flex-wrap items-center gap-1.5 font-medium text-highlighted">
          {{ row.original.name }}
          <UBadge v-if="configured(row.original)" label="Configured" color="success" variant="subtle" size="sm" />
        </p>
        <p class="font-mono text-xs text-muted">
          {{ row.original.id }}
          <template v-if="row.original.version">
            · v{{ row.original.version }}
          </template>
          <template v-if="row.original.region">
            · {{ row.original.region }}
          </template>
        </p>
      </div>
    </template>
    <template #foundBy-cell="{ row }">
      <ul class="space-y-1">
        <li v-for="f in row.original.foundBy" :key="`${f.kind}:${f.connectionId ?? ''}`" class="flex flex-wrap items-center gap-1.5 text-sm">
          <UIcon :name="FINDER_ICONS[f.kind]" class="size-3.5 shrink-0 text-muted" aria-hidden="true" />
          <span>{{ f.label }}</span>
          <DiscoverySourceBadges :sources="f.sources" />
        </li>
      </ul>
    </template>
    <template #healthy-cell="{ row }">
      <DiscoveryPeerStatus :healthy="row.original.healthy" :verified="row.original.verified" :error="row.original.error" />
    </template>
    <template #addresses-cell="{ row }">
      <ul class="space-y-0.5">
        <li v-for="a in row.original.addresses" :key="a" class="font-mono text-xs">
          {{ a }}
        </li>
      </ul>
    </template>
    <template #actions-cell="{ row }">
      <div class="flex justify-end">
        <UButton
          v-if="configured(row.original)"
          :to="appRoutes.server(configured(row.original)!.id)"
          size="xs"
          color="neutral"
          variant="ghost"
          icon="i-lucide-arrow-right"
          :label="`Open ${configured(row.original)!.name}`"
        />
        <UButton
          v-else
          size="xs"
          icon="i-lucide-plus"
          label="Add"
          :aria-label="`Add ${row.original.name} as a server`"
          @click="emit('add', row.original)"
        />
      </div>
    </template>
  </UTable>
</template>
