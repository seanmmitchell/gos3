<script setup lang="ts">
import type { DiskStats } from '~/types/admin'
import { diskUsedPercent } from './-metrics'

/** Disk usage bar for the data directory (fs backend). */
const props = withDefaults(defineProps<{
  disk: DiskStats
  /** Accessible name of the meter (unique when several are shown). */
  label?: string
}>(), { label: 'Disk usage' })

const pct = computed(() => diskUsedPercent(props.disk.usedBytes, props.disk.totalBytes))
const barClass = computed(() => (pct.value >= 90 ? 'bg-error' : pct.value >= 75 ? 'bg-warning' : 'bg-primary'))
const valueText = computed(() => `${formatBytes(props.disk.usedBytes)} of ${formatBytes(props.disk.totalBytes)} used (${pct.value.toFixed(1)}%)`)
</script>

<template>
  <div class="space-y-2">
    <div class="flex flex-wrap items-baseline justify-between gap-2 text-sm">
      <span class="font-mono text-xs text-muted">{{ disk.path }}</span>
      <span class="tabular-nums text-muted">
        <span class="font-medium text-highlighted">{{ formatBytes(disk.freeBytes) }}</span> free
      </span>
    </div>
    <div
      class="h-3 w-full overflow-hidden rounded-full bg-elevated"
      role="meter"
      :aria-valuenow="Math.round(pct)"
      aria-valuemin="0"
      aria-valuemax="100"
      :aria-valuetext="valueText"
      :aria-label="label"
    >
      <div class="h-full rounded-full transition-[width]" :class="barClass" :style="{ width: `${pct}%` }" />
    </div>
    <p class="text-xs text-muted tabular-nums">
      {{ valueText }}
    </p>
  </div>
</template>
