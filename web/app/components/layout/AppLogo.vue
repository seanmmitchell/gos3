<script setup lang="ts">
/** gos3 wordmark (inline SVG, offline-safe). */
withDefaults(defineProps<{ collapsed?: boolean }>(), { collapsed: false })
const appName = useRuntimeConfig().public.appName || 'gos3'
</script>

<template>
  <NuxtLink to="/" class="flex items-center gap-2 rounded-md focus-visible:outline-2 focus-visible:outline-primary" :aria-label="`${appName} home`">
    <svg viewBox="0 0 32 32" class="size-7 shrink-0" aria-hidden="true">
      <rect width="32" height="32" rx="7" class="fill-primary" />
      <ellipse cx="16" cy="10" rx="8" ry="3" fill="none" stroke="white" stroke-width="2" />
      <path d="M8 10v12c0 1.7 3.6 3 8 3s8-1.3 8-3V10" fill="none" stroke="white" stroke-width="2" />
      <path d="M8 16c0 1.7 3.6 3 8 3s8-1.3 8-3" fill="none" stroke="white" stroke-width="2" />
    </svg>
    <span v-if="!collapsed" class="text-lg font-semibold tracking-tight text-highlighted">{{ appName }}</span>
  </NuxtLink>
</template>
