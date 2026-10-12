<script setup lang="ts">
/** Badges for discovery mechanisms (mdns, beacon, static, dns, scan, probe). */
import { sourceLabel } from './-aggregate'

defineProps<{ sources: readonly string[] | null | undefined }>()

type BadgeColor = 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'neutral'
const COLORS: Record<string, BadgeColor> = {
  mdns: 'info',
  beacon: 'primary',
  static: 'neutral',
  dns: 'secondary',
  scan: 'warning',
  probe: 'success'
}
</script>

<template>
  <span class="inline-flex flex-wrap gap-1">
    <UBadge
      v-for="s in sources ?? []"
      :key="s"
      :label="sourceLabel(s)"
      :color="COLORS[s] ?? 'neutral'"
      variant="subtle"
      size="sm"
    />
  </span>
</template>
