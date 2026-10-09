<script setup lang="ts">
import type { VersioningStatus } from '~/types/s3'

/**
 * Bucket versioning: enable or suspend. A bucket can never return to
 * "unversioned"; buckets with object lock cannot suspend versioning.
 */
const props = defineProps<{
  bucket: string
  status: VersioningStatus | null
  objectLock: boolean
}>()

const emit = defineEmits<{ changed: [] }>()

const { requireS3 } = useConnection()
const confirm = useConfirm()

const put = useAsyncAction(async (status: 'Enabled' | 'Suspended') => {
  await requireS3().putBucketVersioning(props.bucket, status)
  emit('changed')
}, { successMessage: s => `Versioning ${s === 'Enabled' ? 'enabled' : 'suspended'}`, errorTitle: 'Could not change versioning' })

async function enable() {
  if (props.status === '') {
    const ok = await confirm({
      title: 'Enable versioning?',
      description: 'Every overwrite and delete will keep the previous version (which uses storage until it expires or is deleted). Once enabled, versioning can be suspended but never fully turned off.',
      confirmLabel: 'Enable versioning',
      color: 'primary',
      icon: 'i-lucide-history'
    })
    if (!ok) return
  }
  await put.run('Enabled')
}

async function suspend() {
  const ok = await confirm({
    title: 'Suspend versioning?',
    description: 'Existing versions are kept. New writes replace the "null" version instead of creating new versions, so overwritten data can no longer be recovered.',
    confirmLabel: 'Suspend',
    color: 'warning',
    icon: 'i-lucide-pause'
  })
  if (ok) await put.run('Suspended')
}

const label = computed(() => (props.status === 'Enabled' ? 'Enabled' : props.status === 'Suspended' ? 'Suspended' : props.status === '' ? 'Never enabled' : 'Unknown'))
const color = computed(() => (props.status === 'Enabled' ? 'success' : props.status === 'Suspended' ? 'warning' : 'neutral'))
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 class="font-semibold text-highlighted">
            Versioning
          </h3>
          <p class="text-sm text-muted">
            Keep every version of every object to recover from overwrites and deletes.
          </p>
        </div>
        <UBadge :label="label" :color="color" variant="subtle" />
      </div>
    </template>
    <div class="space-y-4">
      <UAlert
        v-if="objectLock"
        color="info"
        variant="subtle"
        icon="i-lucide-shield-alert"
        title="Object lock is enabled"
        description="Versioning is required for object lock and cannot be suspended on this bucket."
      />
      <UAlert
        v-else-if="status === 'Suspended'"
        color="warning"
        variant="subtle"
        icon="i-lucide-triangle-alert"
        title="Versioning is suspended"
        description="Existing versions are kept, but new writes overwrite the null version."
      />
      <ul class="list-disc space-y-1 pl-5 text-sm text-muted">
        <li>Deleting an object in a versioned bucket adds a delete marker; older versions remain and can be restored.</li>
        <li>Noncurrent versions use storage — add a lifecycle rule to expire them.</li>
        <li>Once enabled, a bucket can only switch between “Enabled” and “Suspended”.</li>
      </ul>
      <div class="flex flex-wrap gap-2">
        <UButton
          v-if="status !== 'Enabled'"
          icon="i-lucide-history"
          label="Enable versioning"
          :loading="put.loading.value"
          :disabled="status === null"
          @click="enable"
        />
        <UButton
          v-else
          color="warning"
          variant="soft"
          icon="i-lucide-pause"
          label="Suspend versioning"
          :loading="put.loading.value"
          :disabled="objectLock"
          @click="suspend"
        />
      </div>
    </div>
  </UCard>
</template>
