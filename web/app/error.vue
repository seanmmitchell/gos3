<script setup lang="ts">
import type { NuxtError } from '#app'

const props = defineProps<{ error: NuxtError }>()
const is404 = computed(() => props.error.statusCode === 404)
useHead({ title: is404.value ? 'Not found' : 'Error' })

function home() {
  void clearError({ redirect: '/' })
}
</script>

<template>
  <UApp>
    <NuxtLayout name="blank">
      <CommonEmptyState
        :icon="is404 ? 'i-lucide-map-pin-off' : 'i-lucide-triangle-alert'"
        :title="is404 ? 'Page not found' : 'Something went wrong'"
        :description="is404 ? 'The page you are looking for does not exist.' : error.message"
      >
        <UButton label="Go home" icon="i-lucide-house" @click="home" />
      </CommonEmptyState>
    </NuxtLayout>
  </UApp>
</template>
