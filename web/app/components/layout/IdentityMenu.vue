<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'
import { useConnectionsStore } from '~/stores/connections'

/** Sidebar footer: current identity on the active server + session actions. */
withDefaults(defineProps<{ collapsed?: boolean }>(), { collapsed: false })

const store = useConnectionsStore()
const toast = useToast()
const { connection, connected } = useConnection()
const { whoami, refresh, loading } = useWhoAmI()

const identity = computed(() => {
  if (!connection.value) return 'Not connected'
  if (!connected.value) return 'Signed out'
  if (whoami.value) return whoami.value.isRoot ? `${whoami.value.user || 'root'} (root)` : whoami.value.user
  return connection.value.accessKeyId
})

const items = computed<DropdownMenuItem[][]>(() => {
  const c = connection.value
  const groups: DropdownMenuItem[][] = []
  if (c) {
    groups.push([{ type: 'label', label: identity.value, description: c.name }])
    const actions: DropdownMenuItem[] = [
      { label: 'Switch server', icon: 'i-lucide-arrow-left-right', to: appRoutes.servers() }
    ]
    if (connected.value) {
      actions.push({ label: 'Refresh status', icon: 'i-lucide-refresh-cw', loading: loading.value, onSelect: () => void refresh() })
    } else {
      actions.push({ label: 'Connect', icon: 'i-lucide-log-in', to: appRoutes.connect(c.id) })
    }
    groups.push(actions)
    if (connected.value) {
      groups.push([{
        label: 'Disconnect',
        icon: 'i-lucide-log-out',
        color: 'error',
        onSelect: () => {
          store.clearCredentials(c.id)
          toast.add({ title: `Disconnected from ${c.name}`, description: 'The secret key was forgotten on this device.', icon: 'i-lucide-log-out' })
          void navigateTo(appRoutes.servers())
        }
      }])
    }
  } else {
    groups.push([
      { label: 'Add server', icon: 'i-lucide-plus', to: appRoutes.newServer() },
      { label: 'Discover servers', icon: 'i-lucide-radar', to: appRoutes.discover() }
    ])
  }
  return groups
})
</script>

<template>
  <UDropdownMenu :items="items" :content="{ align: 'start', side: 'top', collisionPadding: 12 }" :ui="{ content: 'w-60' }">
    <UButton
      color="neutral"
      variant="ghost"
      block
      :square="collapsed"
      class="data-[state=open]:bg-elevated"
      :class="collapsed ? '' : 'justify-start'"
      :aria-label="`Account: ${identity}`"
    >
      <UAvatar icon="i-lucide-user-round" size="xs" />
      <span v-if="!collapsed" class="min-w-0 flex-1 text-left">
        <span class="block truncate text-sm font-medium text-highlighted">{{ identity }}</span>
        <span v-if="whoami?.policies?.length" class="block truncate text-xs text-muted">{{ whoami.policies.join(', ') }}</span>
      </span>
      <UIcon v-if="!collapsed" name="i-lucide-chevrons-up-down" class="size-4 shrink-0 text-dimmed" />
    </UButton>
  </UDropdownMenu>
</template>
