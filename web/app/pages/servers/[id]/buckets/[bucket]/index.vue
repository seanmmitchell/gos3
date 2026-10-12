<script setup lang="ts">
/**
 * /servers/:id/buckets/:bucket[?prefix=a/b/] → object browser
 * (/servers/:id/buckets/:bucket/objects/a/b/). Keeps appRoutes.bucket() links
 * (used by the layout breadcrumbs and other pages) working.
 */
useHead({ title: 'Bucket' })

const route = useRoute()
const id = routeParam(route.params.id)
const bucket = routeParam(route.params.bucket)
const prefix = typeof route.query.prefix === 'string' ? route.query.prefix : ''

await navigateTo(browserRoute(id, bucket, prefix), { replace: true })
</script>

<template>
  <div class="flex items-center gap-2 p-6 text-sm text-muted" role="status">
    <UIcon name="i-lucide-loader-circle" class="size-4 animate-spin" aria-hidden="true" />
    Opening bucket…
  </div>
</template>
