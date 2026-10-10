<script setup lang="ts">
/** "3 minutes ago" with the absolute date as tooltip; refreshes every 30 s. */
const props = defineProps<{ value: string | number | Date | null | undefined }>()

// One shared timer for all instances (listings render thousands of them).
const now = useNow()

const iso = computed(() => formatISO(props.value))
const label = computed(() => formatRelativeTime(props.value, now.value))
const title = computed(() => formatDate(props.value, { dateStyle: 'full', timeStyle: 'long' }))
</script>

<template>
  <time v-if="iso" :datetime="iso" :title="title">{{ label }}</time>
  <span v-else class="text-muted">—</span>
</template>
