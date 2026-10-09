<script setup lang="ts">
/** Human readable byte size with the exact byte count as tooltip. */
const props = defineProps<{ bytes: number | null | undefined }>()
const exact = computed(() => (props.bytes == null ? '' : `${formatNumber(props.bytes)} bytes`))
</script>

<template>
  <span class="tabular-nums" :title="exact">{{ formatBytes(bytes) }}</span>
</template>
