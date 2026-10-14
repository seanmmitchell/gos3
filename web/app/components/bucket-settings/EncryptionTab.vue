<script setup lang="ts">
import type { BucketEncryptionProvider, EncryptionProviderName } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'

/**
 * Bucket encryption: two independent settings, each loaded and saved on
 * its own so a failure never leaves one half-applied.
 *
 * - Default encryption (S3 ?encryption, every server): whether uploads
 *   without encryption headers are encrypted. Generic S3 offers None,
 *   SSE-S3 (AES256) and SSE-KMS with a key ID and bucket key; gos3 offers
 *   None and SSE-S3. An AWS KMS rule stored by older gos3 builds is applied
 *   as SSE-S3 by the server; it is shown with a warning and pre-selected as
 *   SSE-S3 (unsaved) so saving replaces it.
 * - SSE-S3 implementation (gos3 admin API /buckets/{name}/encryption-provider):
 *   native AES-256-GCM or restic, used by default encryption and by uploads
 *   that request AES256, so it stays visible when default encryption is
 *   off. Restic is offered only when the server reports it available for
 *   the bucket. Without admin:GetBucketEncryption (403), or on servers
 *   without the endpoint (404), the card says so and default encryption
 *   keeps working through the S3 API alone.
 */
const props = defineProps<{ bucket: string }>()
const emit = defineEmits<{ changed: [] }>()

type DefaultMode = 'none' | 'AES256' | 'aws:kms'

const { requireS3, requireAdmin, isGos3 } = useConnection()
const confirm = useConfirm()

// --- Default encryption (S3 API) ---------------------------------------

const mode = ref<DefaultMode>('none')
const kmsKeyId = ref('')
const bucketKey = ref(false)
/** gos3 bucket with an AWS KMS default stored by an older build. */
const legacyKms = ref(false)
const original = ref('')

const loader = useLoader(async () => {
  const cfg = await requireS3().getBucketEncryption(props.bucket)
  const rule = cfg?.rules[0]
  mode.value = !rule ? 'none' : rule.sseAlgorithm === 'AES256' ? 'AES256' : 'aws:kms'
  kmsKeyId.value = rule?.kmsMasterKeyId ?? ''
  bucketKey.value = !!rule?.bucketKeyEnabled
  original.value = signature()
  legacyKms.value = isGos3.value && mode.value === 'aws:kms'
  // gos3 applies a legacy KMS default as SSE-S3: offer saving it as such.
  if (legacyKms.value) mode.value = 'AES256'
  return cfg
}, { watch: [() => props.bucket] })

/** The saved form of the settings: mode-specific fields count only in their mode. */
function signature(): string {
  const kms = mode.value === 'aws:kms'
  return JSON.stringify([mode.value, kms ? kmsKeyId.value.trim() : '', kms && bucketKey.value])
}
const dirty = computed(() => signature() !== original.value)

const items = computed(() => [
  { label: 'None', value: 'none', description: 'Uploads are encrypted only when the request asks for it.' },
  { label: 'SSE-S3 (AES256)', value: 'AES256', description: 'Encrypt new uploads with server-managed keys.' },
  ...(isGos3.value
    ? []
    : [{ label: 'SSE-KMS (aws:kms)', value: 'aws:kms', description: 'Encrypt with a key from the service’s key management system.' }])
])

const save = useAsyncAction(async () => {
  const s3 = requireS3()
  if (mode.value === 'none') {
    await s3.deleteBucketEncryption(props.bucket)
  } else {
    const kms = mode.value === 'aws:kms'
    await s3.putBucketEncryption(props.bucket, {
      rules: [{
        sseAlgorithm: mode.value,
        kmsMasterKeyId: kms && kmsKeyId.value.trim() ? kmsKeyId.value.trim() : undefined,
        bucketKeyEnabled: kms ? bucketKey.value : undefined
      }]
    })
  }
  await loader.refresh()
  emit('changed')
}, { successMessage: 'Default encryption saved', errorTitle: 'Could not save default encryption' })

async function onSave() {
  if (mode.value === 'none' && loader.data.value) {
    const ok = await confirm({ title: 'Remove default encryption?', description: 'New uploads are no longer encrypted by default. Existing objects and their keys are not changed.', confirmLabel: 'Remove', color: 'warning' })
    if (!ok) return
  }
  await save.run()
}

// --- SSE-S3 implementation (gos3 admin API) -----------------------------

const ALL_PROVIDERS: readonly EncryptionProviderName[] = ['AES256', 'restic']

const provider = ref<EncryptionProviderName>('AES256')
const providerLoader = useLoader(async () => {
  if (!isGos3.value) return null
  const doc = await requireAdmin().getBucketEncryptionProvider(props.bucket)
  provider.value = doc.provider
  return doc
}, { watch: [() => props.bucket, isGos3] })
const providerDoc = computed<BucketEncryptionProvider | null | undefined>(() => providerLoader.data.value)

/** Why the provider card cannot be used: 404 = older server, 403 = not allowed. */
const providerUnavailable = computed(() => {
  const e = providerLoader.error.value
  if (!(e instanceof AdminError)) return undefined
  if (e.isNotFound) return 'unsupported'
  if (e.isAccessDenied) return 'denied'
  return undefined
})
const available = computed(() => providerDoc.value?.available ?? ALL_PROVIDERS)
const resticAvailable = computed(() => available.value.includes('restic'))
const providerDirty = computed(() => !!providerDoc.value && provider.value !== providerDoc.value.provider)

const providerItems = computed(() => [
  { label: 'Native (AES-256-GCM)', value: 'AES256', description: 'Encrypt object data on the server’s storage with per-bucket keys.' },
  {
    label: 'Restic',
    value: 'restic',
    disabled: !resticAvailable.value,
    description: resticAvailable.value
      ? 'Encrypt and archive object data in restic repositories; downloads retrieve it from the archive.'
      : 'Not available for this bucket: restic must be installed on the server and every copy of the bucket must be on a filesystem backing.'
  }
])

const saveProvider = useAsyncAction(async () => {
  providerLoader.data.value = await requireAdmin().putBucketEncryptionProvider(props.bucket, { provider: provider.value })
  emit('changed')
}, { successMessage: 'SSE-S3 implementation saved', errorTitle: 'Could not change the SSE-S3 implementation' })
</script>

<template>
  <div class="space-y-4">
    <UCard>
      <template #header>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h3 class="font-semibold text-highlighted">
              Default encryption
            </h3>
            <p class="text-sm text-muted">
              Applies to new uploads without encryption headers. Existing objects are not rewritten when this setting changes.
            </p>
          </div>
          <UBadge v-if="dirty" label="Unsaved" color="warning" variant="subtle" />
        </div>
      </template>
      <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Could not load encryption settings" retryable @retry="loader.refresh" />
      <USkeleton v-else-if="loader.loading.value && !original" class="h-32 w-full" />
      <form v-else class="space-y-4" novalidate data-testid="default-encryption" @submit.prevent="onSave">
        <UAlert
          v-if="legacyKms"
          color="warning"
          variant="subtle"
          icon="i-lucide-triangle-alert"
          title="Legacy AWS KMS setting"
          description="An older gos3 version stored AWS KMS as this bucket’s default, which gos3 does not implement. New uploads use SSE-S3 (AES256) instead. Save to replace the stored setting."
          data-testid="legacy-kms"
        />
        <URadioGroup v-model="mode" :items="items" legend="Encryption type" />
        <div v-if="mode === 'aws:kms'" class="grid gap-4 sm:grid-cols-2">
          <UFormField label="KMS key ID" name="kmsKeyId" help="Key ID or ARN; empty = the server's default key.">
            <UInput v-model="kmsKeyId" class="w-full font-mono" placeholder="arn:aws:kms:…" />
          </UFormField>
          <div class="sm:pt-6">
            <USwitch v-model="bucketKey" label="Bucket key" description="Reduce KMS requests with a bucket-level key." />
          </div>
        </div>
        <p v-if="isGos3" class="text-sm text-muted">
          Older gos3 versions only recorded encryption settings. Those existing objects remain unencrypted until uploaded or copied again.
        </p>
        <div class="flex justify-end gap-2 border-t border-default pt-4">
          <UButton type="submit" icon="i-lucide-save" label="Save default encryption" :disabled="!dirty" :loading="save.loading.value" />
        </div>
      </form>
    </UCard>

    <UCard v-if="isGos3" data-testid="encryption-provider">
      <template #header>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h3 class="font-semibold text-highlighted">
              SSE-S3 implementation
            </h3>
            <p class="text-sm text-muted">
              How this bucket stores objects encrypted with SSE-S3: by default encryption above, or because the upload asked for AES256.
            </p>
          </div>
          <UBadge v-if="providerDirty" label="Unsaved" color="warning" variant="subtle" />
        </div>
      </template>
      <UAlert
        v-if="providerUnavailable === 'unsupported'"
        color="neutral"
        variant="subtle"
        icon="i-lucide-info"
        title="Not available"
        description="This server does not support choosing the SSE-S3 implementation; SSE-S3 uses its built-in encryption."
        data-testid="provider-unsupported"
      />
      <UAlert
        v-else-if="providerUnavailable === 'denied'"
        color="neutral"
        variant="subtle"
        icon="i-lucide-lock"
        title="Not allowed"
        description="Viewing or changing the SSE-S3 implementation needs admin:GetBucketEncryption and admin:PutBucketEncryption on this bucket. Default encryption above works without them."
        data-testid="provider-denied"
      />
      <CommonErrorAlert v-else-if="providerLoader.error.value" :error="providerLoader.error.value" title="Could not load the SSE-S3 implementation" retryable @retry="providerLoader.refresh" />
      <USkeleton v-else-if="!providerDoc" class="h-24 w-full" />
      <form v-else class="space-y-4" novalidate @submit.prevent="saveProvider.run()">
        <URadioGroup v-model="provider" :items="providerItems" legend="Implementation" />
        <div v-if="provider === 'restic'" class="space-y-2">
          <p v-if="providerDoc.backings?.length" class="text-sm text-muted">
            Assigned backings: <span class="font-medium text-highlighted">{{ providerDoc.backings.join(', ') }}</span>.
          </p>
          <p class="text-sm text-muted">
            Restic storage is inherited from this bucket’s placement. Configure local or S3 repositories and credentials on the assigned backings under server Storage settings. Replicas and moves use their destination backing’s settings.
          </p>
          <p class="text-sm text-muted">
            Downloads retrieve the object into an encrypted temporary cache before serving it. Snapshots remain in the archive after S3 objects are deleted. Back up the server’s protected system directory with its keys and metadata: a repository alone does not preserve object names, versions and permissions.
          </p>
        </div>
        <p class="text-sm text-muted">
          The server prepares the keys or repository before switching; if that fails, the current implementation stays in effect. Existing objects keep the implementation they were stored with.
        </p>
        <div class="flex justify-end gap-2 border-t border-default pt-4">
          <UButton type="submit" icon="i-lucide-save" label="Save implementation" :disabled="!providerDirty" :loading="saveProvider.loading.value" />
        </div>
      </form>
    </UCard>
  </div>
</template>
