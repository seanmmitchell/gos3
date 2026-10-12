<script setup lang="ts">
const appName = useRuntimeConfig().public.appName || 'gos3'
useHead({
  titleTemplate: (title?: string) => (title ? `${title} · ${appName}` : appName)
})
</script>

<template>
  <UApp :toaster="{ position: 'bottom-right' }" :tooltip="{ delayDuration: 300 }">
    <a
      href="#main-content"
      class="sr-only z-50 rounded bg-primary px-3 py-2 text-inverted focus:not-sr-only focus:fixed focus:left-2 focus:top-2"
    >Skip to content</a>
    <NuxtLoadingIndicator color="var(--ui-primary)" />
    <NuxtLayout>
      <NuxtPage />
    </NuxtLayout>
  </UApp>
</template>
