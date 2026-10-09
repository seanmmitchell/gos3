<script setup lang="ts">
/** Health / verification badge of a discovered peer (error as tooltip). */
const props = defineProps<{
  healthy: boolean
  verified: boolean
  error?: string
}>()

const label = computed(() => (props.healthy ? 'Healthy' : props.verified ? 'Unhealthy' : 'Unverified'))
</script>

<template>
  <UTooltip :text="error" :disabled="!error">
    <span class="inline-flex items-center gap-1.5 text-sm" :tabindex="error ? 0 : undefined">
      <CommonStatusDot :state="healthy ? 'healthy' : 'unhealthy'" aria-hidden="true" />
      <span :class="healthy ? '' : 'text-muted'">{{ label }}</span>
      <UIcon v-if="verified" name="i-lucide-badge-check" class="size-4 text-success" aria-label="Verified via /_gos3/info" />
      <span v-if="error" class="sr-only">: {{ error }}</span>
    </span>
  </UTooltip>
</template>
