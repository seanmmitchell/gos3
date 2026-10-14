<script setup lang="ts">
/**
 * Compact metric tile.
 *
 *   <AdminStatCard label="Requests" :value="formatNumber(n)" icon="i-lucide-activity" hint="since start" />
 */
withDefaults(defineProps<{
  label: string
  value: string
  icon?: string
  hint?: string
  /** Emphasise the value (e.g. errors > 0). */
  color?: 'default' | 'error' | 'warning' | 'success'
  loading?: boolean
}>(), { icon: undefined, hint: undefined, color: 'default', loading: false })

const VALUE_CLASS = {
  default: 'text-highlighted',
  error: 'text-error',
  warning: 'text-warning',
  success: 'text-success'
} as const
</script>

<template>
  <UCard :ui="{ body: 'p-4 sm:p-4' }">
    <div class="flex items-start justify-between gap-2">
      <dl class="min-w-0">
        <dt class="truncate text-xs font-medium text-muted">
          {{ label }}
        </dt>
        <dd class="mt-1">
          <USkeleton v-if="loading" class="h-7 w-20" />
          <span v-else class="text-2xl font-semibold tabular-nums" :class="VALUE_CLASS[color]">{{ value }}</span>
        </dd>
        <dd v-if="hint" class="mt-0.5 truncate text-xs text-dimmed" :title="hint">
          {{ hint }}
        </dd>
      </dl>
      <UIcon v-if="icon" :name="icon" class="size-5 shrink-0 text-dimmed" aria-hidden="true" />
    </div>
  </UCard>
</template>
