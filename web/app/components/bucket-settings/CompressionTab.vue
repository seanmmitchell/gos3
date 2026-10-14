<script setup lang="ts">
import type { BucketCompression } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { bucketCompressionAccess } from '~/components/admin/-capabilities'
import { compressionConflict, compressionModeText } from './-compression'

/**
 * Bucket compression (gos3 admin API /buckets/{name}/compression): the
 * bucket's own setting, whether new objects are actually compressed and the
 * administrator's server-wide mode. "enforced" and "disabled" modes refuse
 * (409) a setting that contradicts them, so that direction of the switch is
 * disabled. Without a service-level admin:PutBucketCompression the controls
 * stay usable (a grant may be scoped to this bucket) and a 403 is shown.
 */
const props = defineProps<{ bucket: string }>()
const emit = defineEmits<{ changed: [] }>()

const { connection, requireAdmin } = useConnection()
const { capabilities } = useWhoAmI()

const access = computed(() => bucketCompressionAccess(connection.value?.kind, capabilities.value))

const loader = useLoader(() => requireAdmin().getBucketCompression(props.bucket), { watch: [() => props.bucket] })
const doc = computed<BucketCompression | undefined>(() => loader.data.value)

/** The last refused change (403 / 409), shown in the card until the next attempt. */
const actionError = ref<unknown>(null)

async function apply(fn: () => Promise<BucketCompression | undefined>) {
  actionError.value = null
  try {
    const updated = await fn()
    if (updated) loader.data.value = updated
    else await loader.refresh()
    emit('changed')
  } catch (e) {
    actionError.value = e
    throw e
  }
}

const put = useAsyncAction(
  (enabled: boolean) => apply(() => requireAdmin().putBucketCompression(props.bucket, enabled)),
  { successMessage: enabled => `Compression ${enabled ? 'enabled' : 'disabled'} for ${props.bucket}`, errorTitle: 'Could not change compression' }
)

const reset = useAsyncAction(
  () => apply(async () => {
    await requireAdmin().deleteBucketCompression(props.bucket)
    return undefined
  }),
  { successMessage: 'Compression reset to the server default', errorTitle: 'Could not reset compression' }
)

const busy = computed(() => put.loading.value || reset.loading.value)
const conflict = computed(() => compressionConflict(doc.value?.serverMode, doc.value?.enabled))
const mode = computed(() => compressionModeText(doc.value?.serverMode))

function onToggle(v: boolean) {
  if (v === doc.value?.enabled) return
  void put.run(v)
}

/** A 404 here (the bucket exists: the page checked) means the server has no compression API. */
const unsupported = computed(() => loader.error.value instanceof AdminError && loader.error.value.isNotFound)
const refused = computed(() => (actionError.value instanceof AdminError && (actionError.value.status === 409 || actionError.value.isAccessDenied) ? actionError.value : null))
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 class="font-semibold text-highlighted">
            Compression
          </h3>
          <p class="text-sm text-muted">
            Store new objects compressed. Clients read them back unchanged, with the same ETags, checksums and sizes. Existing objects are not rewritten.
          </p>
        </div>
        <UBadge
          v-if="doc"
          :label="doc.effective ? 'Compressing new objects' : 'Not compressing'"
          :color="doc.effective ? 'success' : 'neutral'"
          variant="subtle"
          :icon="doc.effective ? 'i-lucide-file-archive' : undefined"
          data-testid="compression-effective"
        />
      </div>
    </template>

    <UAlert
      v-if="unsupported"
      color="neutral"
      variant="subtle"
      icon="i-lucide-info"
      title="Not available"
      description="This server does not support bucket compression."
    />
    <CommonErrorAlert v-else-if="loader.error.value" :error="loader.error.value" title="Could not load compression settings" retryable @retry="loader.refresh" />
    <USkeleton v-else-if="!doc" class="h-32 w-full" />
    <div v-else class="space-y-4">
      <UAlert
        v-if="doc.serverMode === 'enforced'"
        color="info"
        variant="subtle"
        icon="i-lucide-shield-check"
        title="Compression is enforced by the server administrator"
        description="Every bucket compresses new objects; this bucket cannot opt out. Ask the administrator to change the server's compression mode."
      />
      <UAlert
        v-else-if="doc.serverMode === 'disabled'"
        color="warning"
        variant="subtle"
        icon="i-lucide-ban"
        title="Compression is disabled by the server administrator"
        description="New objects are stored uncompressed and buckets cannot enable compression. Objects already stored compressed stay readable."
      />

      <USwitch
        :model-value="doc.enabled"
        label="Compress new objects in this bucket"
        :description="conflict ?? 'Small objects and already compressed media types are stored as is (server policy).'"
        :disabled="busy || !!conflict"
        :loading="put.loading.value"
        data-testid="compression-toggle"
        @update:model-value="onToggle"
      />

      <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
        <dt class="text-muted">
          Bucket setting
        </dt>
        <dd>{{ doc.enabled ? 'Enabled' : 'Disabled' }}</dd>
        <dt class="text-muted">
          Server mode
        </dt>
        <dd>
          <span class="font-medium">{{ mode.label }}</span>
          <span class="text-muted"> · {{ mode.description }}</span>
        </dd>
        <dt class="text-muted">
          Algorithm
        </dt>
        <dd>{{ doc.algorithm || 'deflate' }}, level {{ doc.level }}</dd>
        <dt class="text-muted">
          Minimum size
        </dt>
        <dd><CommonByteSize :bytes="doc.minSize" /></dd>
      </dl>

      <UAlert
        v-if="refused"
        color="error"
        variant="subtle"
        icon="i-lucide-circle-alert"
        :title="refused.status === 409 ? 'The server refused this setting' : 'Not allowed'"
        :description="refused.isAccessDenied ? `${refused.message} Changing compression needs admin:PutBucketCompression on this bucket.` : refused.message"
        role="alert"
        data-testid="compression-refused"
      />
      <p v-else-if="access === 'try'" class="text-xs text-muted">
        Your identity is not allowed admin:PutBucketCompression server-wide; changes work only if it is granted for this bucket.
      </p>

      <div class="flex flex-wrap items-center justify-between gap-2 border-t border-default pt-4">
        <p class="text-xs text-muted">
          Resetting removes the bucket's own setting: it is then compressed only when the administrator enforces compression.
        </p>
        <UButton
          color="neutral"
          variant="outline"
          icon="i-lucide-rotate-ccw"
          label="Reset to server default"
          :loading="reset.loading.value"
          :disabled="busy"
          @click="reset.run()"
        />
      </div>
    </div>
  </UCard>
</template>
