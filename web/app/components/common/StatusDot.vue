<script setup lang="ts">
import type { ConnectionState } from '~/types/connection'

/** Small colored dot for connection / health states (with screen-reader text). */
const props = withDefaults(defineProps<{
  state: ConnectionState | 'healthy' | 'unhealthy'
  size?: 'xs' | 'sm' | 'md'
  /** Show the state label next to the dot. */
  showLabel?: boolean
}>(), { size: 'sm', showLabel: false })

const META: Record<string, { color: string, label: string }> = {
  online: { color: 'bg-success', label: 'Online' },
  healthy: { color: 'bg-success', label: 'Healthy' },
  offline: { color: 'bg-error', label: 'Offline' },
  unhealthy: { color: 'bg-error', label: 'Unhealthy' },
  unauthorized: { color: 'bg-warning', label: 'Credentials rejected' },
  error: { color: 'bg-error', label: 'Error' },
  checking: { color: 'bg-info', label: 'Checking…' },
  unknown: { color: 'bg-neutral-400 dark:bg-neutral-500', label: 'Unknown' }
}

const meta = computed(() => META[props.state] ?? META.unknown!)
const sizeClass = computed(() => ({ xs: 'size-1.5', sm: 'size-2', md: 'size-2.5' })[props.size])
</script>

<template>
  <span class="inline-flex items-center gap-1.5" :title="meta.label">
    <span class="relative inline-flex" :class="sizeClass" aria-hidden="true">
      <span
        v-if="state === 'checking'"
        class="absolute inline-flex size-full animate-ping rounded-full opacity-60"
        :class="meta.color"
      />
      <span class="relative inline-flex rounded-full" :class="[sizeClass, meta.color]" />
    </span>
    <span v-if="showLabel" class="text-xs text-muted">{{ meta.label }}</span>
    <span v-else class="sr-only">{{ meta.label }}</span>
  </span>
</template>
