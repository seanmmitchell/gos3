<script setup lang="ts">
import { S3Error } from '~/utils/s3/errors'
import { canForceDeleteBucket } from '~/components/admin/-capabilities'

/**
 * Delete the bucket (S3 DeleteBucket, must be empty) or, for gos3 admins
 * with admin:ForceDeleteBucket, force-delete it with every version,
 * delete marker, pending upload and configuration.
 */
const props = defineProps<{ bucket: string }>()

const router = useRouter()
const { id, connection, requireS3, requireAdmin } = useConnection()
const { capabilities } = useWhoAmI()
const confirm = useConfirm()
const toast = useToast()

const canForce = computed(() => canForceDeleteBucket(connection.value?.kind, capabilities.value))
const notEmpty = ref(false)

async function done() {
  toast.add({ title: `Bucket ${props.bucket} deleted`, color: 'success', icon: 'i-lucide-circle-check' })
  if (id.value) await router.push(appRoutes.buckets(id.value))
}

const del = useAsyncAction(async () => {
  try {
    await requireS3().deleteBucket(props.bucket)
  } catch (e) {
    if (e instanceof S3Error && e.code === 'BucketNotEmpty') notEmpty.value = true
    throw e
  }
}, { errorTitle: 'Could not delete bucket', rethrow: true })

async function onDelete() {
  const ok = await confirm({
    title: `Delete bucket ${props.bucket}?`,
    description: 'The bucket must be empty (no objects, versions or delete markers). This cannot be undone.',
    confirmLabel: 'Delete bucket',
    requireText: props.bucket,
    icon: 'i-lucide-trash-2'
  })
  if (!ok) return
  try {
    await del.run()
    await done()
  } catch {
    // toast shown by useAsyncAction
  }
}

const force = useAsyncAction(() => requireAdmin().deleteBucket(props.bucket, { force: true }), { errorTitle: 'Could not force-delete bucket', rethrow: true })

async function onForceDelete() {
  const ok = await confirm({
    title: `Force delete ${props.bucket}?`,
    description: 'Every object, every version, all delete markers, pending multipart uploads and all bucket configuration will be permanently deleted.',
    confirmLabel: 'Delete everything',
    requireText: props.bucket,
    icon: 'i-lucide-bomb'
  })
  if (!ok) return
  try {
    await force.run()
    await done()
  } catch {
    // toast shown by useAsyncAction
  }
}
</script>

<template>
  <UCard :ui="{ root: 'ring-error/40' }">
    <template #header>
      <h3 class="font-semibold text-error">
        Danger zone
      </h3>
    </template>
    <div class="divide-y divide-default">
      <div class="flex flex-col gap-3 pb-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <p class="font-medium text-highlighted">
            Delete bucket
          </p>
          <p class="text-sm text-muted">
            Only empty buckets can be deleted with the S3 API.
          </p>
          <p v-if="notEmpty" class="mt-1 text-sm text-warning" role="status">
            The bucket is not empty. Delete its objects{{ canForce ? ' or use force delete' : '' }} first.
          </p>
        </div>
        <UButton color="error" variant="soft" icon="i-lucide-trash-2" label="Delete bucket" :loading="del.loading.value" @click="onDelete" />
      </div>
      <div v-if="canForce" class="flex flex-col gap-3 pt-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <p class="font-medium text-highlighted">
            Force delete
          </p>
          <p class="text-sm text-muted">
            Removes all versions, delete markers, uploads and configuration, then the bucket (admin:ForceDeleteBucket).
          </p>
        </div>
        <UButton color="error" icon="i-lucide-bomb" label="Force delete" :loading="force.loading.value" @click="onForceDelete" />
      </div>
    </div>
  </UCard>
</template>
