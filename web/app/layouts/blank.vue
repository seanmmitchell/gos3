<script setup lang="ts">
/** Minimal centered layout (connect page, errors). */
</script>

<template>
  <div class="flex min-h-dvh flex-col bg-default">
    <header class="flex items-center justify-between px-4 py-3 sm:px-6">
      <LayoutAppLogo />
      <UColorModeButton />
    </header>
    <main id="main-content" class="flex flex-1 items-start justify-center px-4 pb-12 pt-6 sm:items-center sm:px-6">
      <div class="w-full max-w-xl">
        <slot />
      </div>
    </main>
  </div>
</template>
