<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'
import { useConnectionsStore } from '~/stores/connections'

/** Sidebar dropdown to switch the active server (with status dots). */
withDefaults(defineProps<{ collapsed?: boolean }>(), { collapsed: false })

const store = useConnectionsStore()
const { connection } = useConnection()

type Item = DropdownMenuItem & { connId?: string }

const items = computed<Item[][]>(() => [
  store.sorted.map(c => ({
    label: c.name,
    description: c.id === connection.value?.id ? `${c.endpoint} · active` : c.endpoint,
    connId: c.id,
    color: c.id === connection.value?.id ? ('primary' as const) : undefined,
    onSelect: () => {
      void navigateTo(hasCredentials(c) ? appRoutes.server(c.id) : appRoutes.connect(c.id))
    }
  })),
  [
    { label: 'Manage servers', icon: 'i-lucide-server-cog', to: appRoutes.servers() },
    { label: 'Add server', icon: 'i-lucide-plus', to: appRoutes.connect() },
    { label: 'Discover servers', icon: 'i-lucide-radar', to: appRoutes.discover() }
  ]
])

const state = computed(() => (connection.value ? store.statusOf(connection.value.id).state : 'unknown'))
</script>

<template>
  <UDropdownMenu
    :items="items"
    :content="{ align: 'start', collisionPadding: 12 }"
    :ui="{ content: collapsed ? 'w-64' : 'w-(--reka-dropdown-menu-trigger-width) min-w-56' }"
  >
    <UButton
      color="neutral"
      variant="ghost"
      block
      :square="collapsed"
      class="data-[state=open]:bg-elevated"
      :class="collapsed ? '' : 'justify-between py-2'"
      :aria-label="connection ? `Active server: ${connection.name}. Switch server` : 'Select a server'"
    >
      <span class="flex min-w-0 items-center gap-2">
        <UIcon v-if="!connection" name="i-lucide-server" class="size-5 shrink-0 text-muted" />
        <CommonStatusDot v-else :state="state" />
        <span v-if="!collapsed" class="min-w-0 text-left">
          <span class="block truncate text-sm font-medium text-highlighted">{{ connection?.name ?? 'No server selected' }}</span>
          <span v-if="connection" class="block truncate text-xs text-muted">{{ connection.endpoint }}</span>
        </span>
      </span>
      <UIcon v-if="!collapsed" name="i-lucide-chevrons-up-down" class="size-4 shrink-0 text-dimmed" />
    </UButton>

    <template #item-leading="{ item }">
      <CommonStatusDot v-if="item.connId" :state="store.statusOf(item.connId).state" />
    </template>
  </UDropdownMenu>
</template>
