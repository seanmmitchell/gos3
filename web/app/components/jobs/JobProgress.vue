<script setup lang="ts">
import type { Job } from '~/types/admin'
import { copyStatusOf, jobProgress, jobTiming, snapshotStatusOf } from './-jobs'

/**
 * Progress of one job: for copy and mirror jobs a bar from bytesDone /
 * totalBytes (indeterminate while the total is being computed), the bytes,
 * and the ETA or, for a mirror, its lag and pending changes; for restic
 * jobs the keys and bytes processed and the snapshot written.
 */
const props = defineProps<{ job: Job }>()

const progress = computed(() => jobProgress(props.job))
const copy = computed(() => copyStatusOf(props.job))
const restic = computed(() => snapshotStatusOf(props.job))
const timing = computed(() => jobTiming(props.job))

const bytesLine = computed(() => {
  const p = progress.value
  if (!p) return ''
  if (p.total) return `${formatBytes(p.done)} of ${formatBytes(p.total)}`
  return p.done ? `${formatBytes(p.done)} copied` : ''
})

const counts = computed(() => {
  const c = copy.value
  if (c) {
    const parts: string[] = []
    if (c.keysCopied || c.keysScanned) parts.push(`${formatNumber(c.keysCopied)} of ${formatNumber(c.totalKeys || c.keysScanned)} keys`)
    if (c.keysDeleted) parts.push(`${formatNumber(c.keysDeleted)} deleted`)
    if (c.conflicts) parts.push(`${formatNumber(c.conflicts)} conflict${c.conflicts === 1 ? '' : 's'}`)
    return parts.join(' · ')
  }
  const r = restic.value
  if (r) {
    const parts: string[] = []
    if (r.keys) parts.push(`${formatNumber(r.keys)} keys`)
    if (r.bytes) parts.push(formatBytes(r.bytes))
    if (r.forgotten) parts.push(`${formatNumber(r.forgotten)} forgotten`)
    if (r.snapshot) parts.push(`snapshot ${r.snapshot.slice(0, 8)}`)
    return parts.join(' · ')
  }
  return ''
})
const speed = computed(() => (props.job.state === 'running' && copy.value?.bytesPerSecond ? `${formatBytes(copy.value.bytesPerSecond)}/s` : ''))
</script>

<template>
  <div class="min-w-40 space-y-1" data-testid="job-progress">
    <template v-if="progress">
      <div class="flex items-center gap-2">
        <UProgress
          :model-value="progress.percent"
          size="sm"
          class="flex-1"
          :color="job.state === 'failed' ? 'error' : job.state === 'paused' ? 'warning' : 'primary'"
          :aria-label="progress.percent === null ? 'Progress unknown' : `Progress ${progress.percent}%`"
        />
        <span class="w-12 text-right text-xs tabular-nums text-muted" data-testid="job-percent">{{ progress.percent === null ? '—' : `${progress.percent}%` }}</span>
      </div>
      <p v-if="bytesLine || speed" class="text-xs tabular-nums text-muted">
        {{ bytesLine }}<template v-if="speed">
          · {{ speed }}
        </template>
      </p>
    </template>
    <p v-if="counts" class="text-xs tabular-nums text-muted">
      {{ counts }}
    </p>
    <p v-if="timing" class="text-xs text-muted" data-testid="job-timing">
      {{ timing }}
    </p>
  </div>
</template>
