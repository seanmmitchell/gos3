<script setup lang="ts">
/** "3 minutes ago" with the absolute date as tooltip; refreshes every 30 s. */
const props = defineProps<{ value: string | number | Date | null | undefined }>()

const now = ref(Date.now())
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  timer = setInterval(() => (now.value = Date.now()), 30000)
})
onBeforeUnmount(() => clearInterval(timer))

const iso = computed(() => formatISO(props.value))
const label = computed(() => formatRelativeTime(props.value, now.value))
const title = computed(() => formatDate(props.value, { dateStyle: 'full', timeStyle: 'long' }))
</script>

<template>
  <time v-if="iso" :datetime="iso" :title="title">{{ label }}</time>
  <span v-else class="text-muted">—</span>
</template>
