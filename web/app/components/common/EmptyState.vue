<script setup lang="ts">
/**
 * Empty / placeholder state with optional actions slot.
 *
 *   <CommonEmptyState icon="i-lucide-database" title="No buckets yet" description="…">
 *     <UButton label="Create bucket" />
 *   </CommonEmptyState>
 */
withDefaults(defineProps<{
  title: string
  description?: string
  icon?: string
  /** Compact variant for use inside cards / tables. */
  compact?: boolean
}>(), {
  icon: 'i-lucide-inbox',
  description: undefined,
  compact: false
})
</script>

<template>
  <div
    class="flex flex-col items-center justify-center rounded-lg border border-dashed border-default text-center"
    :class="compact ? 'gap-2 px-4 py-6' : 'gap-3 px-6 py-12'"
    role="status"
  >
    <div
      class="flex items-center justify-center rounded-full bg-elevated text-muted"
      :class="compact ? 'size-9' : 'size-12'"
      aria-hidden="true"
    >
      <UIcon :name="icon" :class="compact ? 'size-4' : 'size-6'" />
    </div>
    <div>
      <p class="font-medium text-highlighted" :class="compact ? 'text-sm' : 'text-base'">
        {{ title }}
      </p>
      <p v-if="description" class="mx-auto mt-1 max-w-md text-sm text-muted">
        {{ description }}
      </p>
    </div>
    <div v-if="$slots.default" class="mt-1 flex flex-wrap items-center justify-center gap-2">
      <slot />
    </div>
  </div>
</template>
