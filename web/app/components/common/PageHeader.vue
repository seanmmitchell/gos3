<script setup lang="ts">
/**
 * Page title row: icon + title + description on the left, actions on the right.
 *
 *   <CommonPageHeader title="Buckets" description="…" icon="i-lucide-database">
 *     <template #actions><UButton label="Create bucket" /></template>
 *   </CommonPageHeader>
 */
defineProps<{
  title: string
  description?: string
  icon?: string
}>()

defineSlots<{
  actions?(): unknown
  badge?(): unknown
  default?(): unknown
}>()
</script>

<template>
  <header class="mb-6 flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
    <div class="flex min-w-0 items-start gap-3">
      <div
        v-if="icon"
        class="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary"
        aria-hidden="true"
      >
        <UIcon :name="icon" class="size-5" />
      </div>
      <div class="min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <h1 class="truncate text-xl font-semibold text-highlighted">
            {{ title }}
          </h1>
          <slot name="badge" />
        </div>
        <p v-if="description" class="mt-1 text-sm text-muted">
          {{ description }}
        </p>
        <slot />
      </div>
    </div>
    <div v-if="$slots.actions" class="flex shrink-0 flex-wrap items-center gap-2">
      <slot name="actions" />
    </div>
  </header>
</template>
