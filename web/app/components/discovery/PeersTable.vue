<script setup lang="ts">
/**
 * Peer table of one gos3 server (GET /discovery) or of a scan result.
 * Peers already configured (a connection at an address they were observed
 * at) link to their connection; others offer "Add". A peer reporting a
 * configured server's ID from another address is flagged, not linked.
 */
import type { TableColumn } from '@nuxt/ui'
import type { Peer } from '~/types/admin'
import { useConnectionsStore } from '~/stores/connections'
import { findConfigured, findIdClaim } from './-aggregate'

withDefaults(defineProps<{
  peers: readonly Peer[]
  loading?: boolean
  emptyText?: string
}>(), { loading: false, emptyText: 'No peers found yet.' })

const emit = defineEmits<{ add: [peer: Peer] }>()

const store = useConnectionsStore()
const configured = (p: Peer) => findConfigured(p, store.connections)
const idClaim = (p: Peer) => findIdClaim(p, store.connections)

const columns: TableColumn<Peer>[] = [
  { accessorKey: 'name', header: 'Server' },
  { accessorKey: 'sources', header: 'Sources' },
  { accessorKey: 'healthy', header: 'Status' },
  { accessorKey: 'lastSeen', header: 'Last seen' },
  { accessorKey: 'addresses', header: 'Addresses' },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, 'Actions') }
]
</script>

<template>
  <UTable :data="[...peers]" :columns="columns" :loading="loading" class="rounded-md border border-default" :ui="{ td: 'py-2 align-top' }">
    <template #empty>
      <p class="py-6 text-center text-sm text-muted">
        {{ emptyText }}
      </p>
    </template>
    <template #name-cell="{ row }">
      <div class="min-w-40">
        <p class="flex flex-wrap items-center gap-1.5 font-medium text-highlighted">
          {{ row.original.name || row.original.id }}
          <UTooltip v-if="idClaim(row.original)" :text="`Reports the server ID of ${idClaim(row.original)!.name} (${idClaim(row.original)!.endpoint}) but was found at another address. Server IDs are not authenticated: it may be the same server under another address, or a host spoofing it.`">
            <UBadge :label="`ID of ${idClaim(row.original)!.name}`" color="warning" variant="subtle" size="sm" icon="i-lucide-shield-question" data-testid="id-claim-badge" />
          </UTooltip>
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
    <template #sources-cell="{ row }">
      <DiscoverySourceBadges :sources="row.original.sources" />
    </template>
    <template #healthy-cell="{ row }">
      <DiscoveryPeerStatus :healthy="row.original.healthy" :verified="row.original.verified" :error="row.original.error" />
    </template>
    <template #lastSeen-cell="{ row }">
      <CommonRelativeTime :value="row.original.lastSeen" class="whitespace-nowrap text-sm" />
    </template>
    <template #addresses-cell="{ row }">
      <ul class="space-y-0.5">
        <li v-for="a in row.original.addresses ?? []" :key="a" class="font-mono text-xs">
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
          label="Add as server"
          :aria-label="`Add ${row.original.name || row.original.id} as a server`"
          @click="emit('add', row.original)"
        />
      </div>
    </template>
  </UTable>
</template>
