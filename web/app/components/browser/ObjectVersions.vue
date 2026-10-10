<script setup lang="ts">
/**
 * All versions and delete markers of one key: view, download, restore (copy
 * the version over the current one) and permanent deletion.
 */
import type { ObjectVersion } from '~/types/s3'
import type { S3Client } from '~/utils/s3/client'

const props = defineProps<{
  client: S3Client | undefined
  bucket: string
  objectKey: string
  /** Version currently shown in the details panel. */
  currentVersionId?: string
  versioning?: string | null
  readOnly?: boolean
}>()

const emit = defineEmits<{
  /** View a version (undefined = latest). */
  select: [versionId: string | undefined]
  /** Versions changed (restore / delete). */
  changed: [info: { deletedVersionId?: string }]
}>()

const confirm = useConfirm()
const toast = useToast()
const versions = ref<ObjectVersion[]>([])
const loading = ref(false)
const error = ref<unknown>(null)
const busyId = ref<string | null>(null)

// Only the latest load may write (the key can change while one runs).
let seq = 0
async function load() {
  const my = ++seq
  if (!props.client) return
  loading.value = true
  error.value = null
  try {
    const list = await props.client.listVersionsOfKey(props.bucket, props.objectKey)
    if (my === seq) versions.value = list
  } catch (e) {
    if (my === seq) error.value = e
  } finally {
    if (my === seq) loading.value = false
  }
}
watch(() => [props.bucket, props.objectKey], () => void load(), { immediate: true })
defineExpose({ reload: load })

async function restore(v: ObjectVersion) {
  if (!props.client) return
  busyId.value = v.versionId
  try {
    const r = await restoreVersion(props.client, props.bucket, props.objectKey, v.versionId)
    if (r.aclReset) toast.add({ title: 'Version restored, ACL reset', description: 'It is now the latest version. Its custom ACL grants cannot be carried over: it is private now.', color: 'warning', icon: 'i-lucide-shield-alert' })
    else toast.add({ title: 'Version restored', description: 'It is now the latest version.', color: 'success', icon: 'i-lucide-circle-check' })
    await load()
    emit('changed', {})
  } catch (e) {
    toast.add({ title: 'Could not restore the version', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
  } finally {
    busyId.value = null
  }
}

async function remove(v: ObjectVersion) {
  if (!props.client) return
  const undelete = v.isDeleteMarker && v.isLatest
  const ok = await confirm({
    title: undelete ? 'Remove the delete marker?' : 'Delete this version permanently?',
    description: undelete
      ? 'The previous version becomes the current version again (the object is “undeleted”).'
      : `Version ${v.versionId} of ${props.objectKey} is removed permanently. This cannot be undone.`,
    confirmLabel: undelete ? 'Remove marker' : 'Delete version',
    color: undelete ? 'primary' : 'error'
  })
  if (!ok) return
  busyId.value = v.versionId
  try {
    await props.client.deleteObject(props.bucket, props.objectKey, v.versionId)
    toast.add({ title: undelete ? 'Delete marker removed' : 'Version deleted', color: 'success', icon: 'i-lucide-circle-check' })
    await load()
    emit('changed', { deletedVersionId: v.versionId })
  } catch (e) {
    toast.add({ title: 'Could not delete the version', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
  } finally {
    busyId.value = null
  }
}

async function download(v: ObjectVersion) {
  if (!props.client) return
  try {
    browserTriggerDownload(await browserDownloadUrl(props.client, props.bucket, props.objectKey, v.versionId))
  } catch (e) {
    toast.add({ title: 'Download failed', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
  }
}

const isCurrent = (v: ObjectVersion) => (props.currentVersionId ? v.versionId === props.currentVersionId : v.isLatest)
</script>

<template>
  <div class="space-y-3">
    <UAlert
      v-if="!versioning && !loading && versions.length <= 1"
      color="neutral"
      variant="subtle"
      icon="i-lucide-info"
      title="Versioning is not enabled for this bucket"
      description="Enable versioning in the bucket settings to keep previous versions when objects are overwritten or deleted."
    />
    <div v-if="loading && !versions.length" class="space-y-2" aria-busy="true" aria-label="Loading versions">
      <USkeleton v-for="i in 3" :key="i" class="h-14 w-full" />
    </div>
    <CommonErrorAlert v-else-if="error" :error="error" title="Could not list versions" retryable @retry="load" />
    <CommonEmptyState v-else-if="!versions.length" compact icon="i-lucide-history" title="No versions found" />
    <ol v-else class="divide-y divide-default rounded-lg border border-default" aria-label="Object versions, newest first">
      <li
        v-for="v in versions"
        :key="v.versionId"
        class="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2"
        :class="isCurrent(v) ? 'bg-primary/5' : ''"
      >
        <UIcon :name="v.isDeleteMarker ? 'i-lucide-file-x-2' : 'i-lucide-file-clock'" class="size-4 shrink-0 text-muted" aria-hidden="true" />
        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-1.5">
            <span class="font-mono text-xs" :title="v.versionId">{{ truncateMiddle(v.versionId, 22) }}</span>
            <UBadge v-if="v.isLatest" color="primary" variant="subtle" size="sm" label="Latest" />
            <UBadge v-if="v.isDeleteMarker" color="error" variant="subtle" size="sm" label="Delete marker" />
            <UBadge v-if="isCurrent(v) && !v.isDeleteMarker" color="neutral" variant="outline" size="sm" label="Viewing" />
          </div>
          <p class="text-xs text-muted">
            <CommonRelativeTime :value="v.lastModified" /><template v-if="!v.isDeleteMarker">
              · {{ formatBytes(v.size) }}
            </template>
          </p>
        </div>
        <div class="flex items-center gap-0.5">
          <UButton
            v-if="!v.isDeleteMarker && !isCurrent(v)"
            size="xs"
            color="neutral"
            variant="ghost"
            icon="i-lucide-eye"
            :aria-label="`View version ${v.versionId}`"
            @click="emit('select', v.isLatest ? undefined : v.versionId)"
          />
          <UButton
            v-if="!v.isDeleteMarker"
            size="xs"
            color="neutral"
            variant="ghost"
            icon="i-lucide-download"
            :aria-label="`Download version ${v.versionId}`"
            @click="download(v)"
          />
          <template v-if="!readOnly">
            <UButton
              v-if="!v.isDeleteMarker && !v.isLatest"
              size="xs"
              color="neutral"
              variant="ghost"
              icon="i-lucide-history"
              :loading="busyId === v.versionId"
              :aria-label="`Restore version ${v.versionId}`"
              @click="restore(v)"
            />
            <UButton
              size="xs"
              :color="v.isDeleteMarker && v.isLatest ? 'primary' : 'error'"
              variant="ghost"
              :icon="v.isDeleteMarker && v.isLatest ? 'i-lucide-undo-2' : 'i-lucide-trash-2'"
              :loading="busyId === v.versionId"
              :aria-label="v.isDeleteMarker && v.isLatest ? 'Remove delete marker' : `Delete version ${v.versionId} permanently`"
              @click="remove(v)"
            />
          </template>
        </div>
      </li>
    </ol>
  </div>
</template>
