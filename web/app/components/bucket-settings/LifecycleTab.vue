<script setup lang="ts">
import type { LifecycleRule, VersioningStatus } from '~/types/s3'
import { describeActions, describeFilter } from './-lifecycle'
import LifecycleRuleModal from './LifecycleRuleModal.vue'

/**
 * Lifecycle rules (?lifecycle). Every add / edit / toggle / delete writes
 * the whole configuration immediately (S3 has no per-rule API); an empty
 * list deletes the configuration.
 */
const props = defineProps<{
  bucket: string
  versioning: VersioningStatus | null
}>()
const emit = defineEmits<{ changed: [] }>()

const { requireS3 } = useConnection()
const overlay = useOverlay()
const confirm = useConfirm()

const loader = useLoader(() => requireS3().getBucketLifecycle(props.bucket), { watch: [() => props.bucket], initial: [] })
const rules = computed(() => loader.data.value ?? [])
const versioned = computed(() => props.versioning === 'Enabled' || props.versioning === 'Suspended')

const write = useAsyncAction(async (next: LifecycleRule[]) => {
  const s3 = requireS3()
  if (next.length) await s3.putBucketLifecycle(props.bucket, next)
  else await s3.deleteBucketLifecycle(props.bucket)
  await loader.refresh()
  emit('changed')
}, { successMessage: 'Lifecycle configuration saved', errorTitle: 'Could not save lifecycle configuration' })

async function edit(index: number | null) {
  const current = index === null ? null : rules.value[index] ?? null
  const otherIds = rules.value.filter((_, i) => i !== index).map(r => r.id ?? '').filter(Boolean)
  const rule = await overlay.create(LifecycleRuleModal, { destroyOnClose: true }).open({ rule: current, otherIds, versioned: versioned.value })
  if (!rule) return
  const next = [...rules.value]
  if (index === null) next.push(rule)
  else next[index] = rule
  await write.run(next)
}

async function toggle(index: number) {
  const next = rules.value.map((r, i) => (i === index ? { ...r, status: r.status === 'Enabled' ? 'Disabled' as const : 'Enabled' as const } : r))
  await write.run(next)
}

async function remove(index: number) {
  const r = rules.value[index]
  const ok = await confirm({
    title: `Delete rule ${r?.id || index + 1}?`,
    description: rules.value.length === 1 ? 'This is the last rule; the lifecycle configuration will be removed.' : 'The other rules stay unchanged.',
    confirmLabel: 'Delete rule',
    icon: 'i-lucide-trash-2'
  })
  if (ok) await write.run(rules.value.filter((_, i) => i !== index))
}
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 class="font-semibold text-highlighted">
            Lifecycle
          </h3>
          <p class="text-sm text-muted">
            Expire objects and old versions automatically and clean up abandoned uploads.
          </p>
        </div>
        <UButton icon="i-lucide-plus" label="Add rule" :loading="write.loading.value" @click="edit(null)" />
      </div>
    </template>
    <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Could not load lifecycle rules" retryable @retry="loader.refresh" />
    <div v-else-if="loader.loading.value && !rules.length" class="space-y-2">
      <USkeleton v-for="n in 2" :key="n" class="h-16 w-full" />
    </div>
    <CommonEmptyState v-else-if="!rules.length" compact icon="i-lucide-timer" title="No lifecycle rules" description="Objects are kept until they are deleted.">
      <UButton size="sm" icon="i-lucide-plus" label="Add rule" @click="edit(null)" />
    </CommonEmptyState>
    <ul v-else class="divide-y divide-default" aria-label="Lifecycle rules">
      <li v-for="(r, i) in rules" :key="r.id ?? i" class="flex flex-col gap-2 py-3 sm:flex-row sm:items-start sm:justify-between">
        <div class="min-w-0 space-y-1">
          <div class="flex flex-wrap items-center gap-2">
            <span class="font-medium text-highlighted">{{ r.id || `Rule ${i + 1}` }}</span>
            <UBadge :label="r.status" :color="r.status === 'Enabled' ? 'success' : 'neutral'" variant="subtle" size="sm" />
          </div>
          <p class="text-xs text-muted">
            <UIcon name="i-lucide-filter" class="mr-1 inline size-3" aria-hidden="true" />{{ describeFilter(r) }}
          </p>
          <ul class="list-disc pl-5 text-sm text-default">
            <li v-for="a in describeActions(r)" :key="a">
              {{ a }}
            </li>
          </ul>
        </div>
        <div class="flex shrink-0 gap-1">
          <UButton size="xs" color="neutral" variant="ghost" :icon="r.status === 'Enabled' ? 'i-lucide-pause' : 'i-lucide-play'" :label="r.status === 'Enabled' ? 'Disable' : 'Enable'" :disabled="write.loading.value" @click="toggle(i)" />
          <UButton size="xs" color="neutral" variant="ghost" icon="i-lucide-pencil" label="Edit" :disabled="write.loading.value" @click="edit(i)" />
          <UButton size="xs" color="error" variant="ghost" icon="i-lucide-trash-2" :aria-label="`Delete rule ${r.id || i + 1}`" :disabled="write.loading.value" @click="remove(i)" />
        </div>
      </li>
    </ul>
    <p class="mt-4 text-xs text-muted">
      Rules are applied by the server's lifecycle worker (Server settings → Lifecycle worker).
    </p>
  </UCard>
</template>
