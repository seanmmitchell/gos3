<script setup lang="ts">
import type { BucketEncryptionProvider } from '~/types/admin'
const props = defineProps<{ bucket: string }>()
const emit = defineEmits<{ changed: [] }>()
type Mode = 'none' | 'AES256' | 'restic' | 'aws:kms'
const { requireS3, requireAdmin, isGos3 } = useConnection()
const confirm = useConfirm()
const mode = ref<Mode>('none')
const kmsKeyId = ref('')
const bucketKey = ref(false)
const repository = ref('')
const region = ref('')
const accessKey = ref('')
const secretKey = ref('')
const sessionToken = ref('')
const password = ref('')
const provider = ref<BucketEncryptionProvider>()
const original = ref('')

const loader = useLoader(async () => {
  const cfg = await requireS3().getBucketEncryption(props.bucket)
  provider.value = isGos3.value ? await requireAdmin().getBucketEncryptionProvider(props.bucket) : undefined
  const rule = cfg?.rules[0]
  mode.value = !rule ? 'none' : rule.sseAlgorithm === 'AES256' ? provider.value?.provider ?? 'AES256' : 'aws:kms'
  kmsKeyId.value = rule?.kmsMasterKeyId ?? ''
  bucketKey.value = !!rule?.bucketKeyEnabled
  repository.value = provider.value?.repository ?? ''
  region.value = provider.value?.region ?? ''
  accessKey.value = secretKey.value = sessionToken.value = password.value = ''
  original.value = signature()
  return cfg
}, { watch: [() => props.bucket] })

function signature() {
  return JSON.stringify([mode.value, kmsKeyId.value, bucketKey.value, repository.value, region.value, accessKey.value, secretKey.value, sessionToken.value, password.value])
}
const dirty = computed(() => signature() !== original.value)
const items = computed(() => [
  { label: 'None', value: 'none', description: 'New uploads use encryption only when the client requests it.' },
  { label: 'SSE-S3 (AES256)', value: 'AES256', description: 'Encrypt object data using server-managed keys.' },
  ...(isGos3.value
    ? [{ label: 'Restic', value: 'restic', description: 'Encrypt and archive object data in a local or remote restic repository; retrieve it on download.' }]
    : [{ label: 'SSE-KMS (aws:kms)', value: 'aws:kms', description: 'Encrypt through the S3 service’s key management system.' }])
])
const save = useAsyncAction(async () => {
  const s3 = requireS3()
  if (mode.value === 'none') {
    await s3.deleteBucketEncryption(props.bucket)
  } else {
    if (isGos3.value) {
      if (mode.value === 'aws:kms') throw new Error('gos3 does not implement AWS KMS. Select AES256 or restic.')
      const result = await requireAdmin().putBucketEncryptionProvider(props.bucket, {
        provider: mode.value,
        repository: repository.value.trim(), region: region.value.trim(),
        accessKey: accessKey.value || undefined, secretKey: secretKey.value || undefined,
        sessionToken: sessionToken.value || undefined, password: password.value || undefined
      })
      provider.value = result
      accessKey.value = secretKey.value = sessionToken.value = password.value = ''
      if (!result.ready) throw new Error(result.error ?? 'Repository is unavailable. Settings were saved; correct them and retry.')
    }
    await s3.putBucketEncryption(props.bucket, { rules: [{
      sseAlgorithm: mode.value === 'restic' ? 'AES256' : mode.value,
      kmsMasterKeyId: mode.value === 'aws:kms' ? kmsKeyId.value.trim() || undefined : undefined,
      bucketKeyEnabled: mode.value === 'aws:kms' ? bucketKey.value : undefined
    }] })
  }
  await loader.refresh()
  emit('changed')
}, { successMessage: 'Default encryption saved', errorTitle: 'Could not save encryption settings' })

async function onSave() {
  if (mode.value === 'none' && loader.data.value) {
    const ok = await confirm({ title: 'Remove default encryption?', description: 'New uploads will no longer be encrypted by default. Existing encrypted objects and their keys remain available.', confirmLabel: 'Remove', color: 'warning' })
    if (!ok) return
  }
  await save.run()
}
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 class="font-semibold text-highlighted">Default encryption</h3>
          <p class="text-sm text-muted">Applies to new uploads. Existing objects are not rewritten when this setting changes.</p>
        </div>
        <UBadge v-if="dirty" label="Unsaved" color="warning" variant="subtle" />
      </div>
    </template>
    <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Could not load encryption settings" retryable @retry="loader.refresh" />
    <USkeleton v-else-if="loader.loading.value && !original" class="h-32 w-full" />
    <form v-else class="space-y-4" novalidate @submit.prevent="onSave">
      <CommonErrorAlert v-if="save.error.value" :error="save.error.value" title="Could not save encryption settings" />
      <UAlert v-if="isGos3 && mode === 'aws:kms'" color="warning" title="Unsupported legacy KMS setting" description="gos3 does not implement AWS KMS. Choose AES256 or restic to enable actual encryption for new uploads." />
      <URadioGroup v-model="mode" :items="items" legend="Encryption type" />
      <div v-if="mode === 'restic'" class="space-y-4">
        <UFormField label="Repository" name="repository" help="Leave empty for local storage, or enter s3:https://host/bucket/prefix. Use a dedicated repository for this bucket. The location is fixed after setup.">
          <UInput v-model="repository" class="w-full font-mono" placeholder="Local repository" :disabled="provider?.passwordConfigured" />
        </UFormField>
        <div v-if="repository" class="grid gap-4 sm:grid-cols-2">
          <UFormField label="S3 region" name="region"><UInput v-model="region" placeholder="us-east-1" /></UFormField>
          <UFormField label="S3 access key" name="accessKey" :help="provider?.credentialsConfigured ? 'Configured. Leave blank to retain.' : undefined"><UInput v-model="accessKey" autocomplete="off" /></UFormField>
          <UFormField label="S3 secret key" name="secretKey"><UInput v-model="secretKey" type="password" autocomplete="new-password" placeholder="Leave blank to retain" /></UFormField>
          <UFormField label="S3 session token (optional)" name="sessionToken"><UInput v-model="sessionToken" type="password" autocomplete="new-password" /></UFormField>
          <UFormField label="Repository password" name="password" help="For an existing repository, enter its password. For a new one, leave blank and gos3 generates and retains a password."><UInput v-model="password" type="password" autocomplete="new-password" placeholder="Server-managed" /></UFormField>
        </div>
        <p class="text-sm text-muted">Restic must be installed on the server. Downloads retrieve the object into an encrypted temporary cache before serving it. Snapshots remain in the archive after S3 objects are deleted.</p>
        <p class="text-sm text-muted">Back up the server’s protected system directory with its keys and metadata. A repository alone does not preserve the S3 object names, versions, and permissions.</p>
      </div>
      <div v-if="!isGos3 && mode === 'aws:kms'" class="grid gap-4 sm:grid-cols-2">
        <UFormField label="KMS key ID" name="kmsKeyId"><UInput v-model="kmsKeyId" class="w-full font-mono" /></UFormField>
        <USwitch v-model="bucketKey" label="Bucket key" />
      </div>
      <p v-if="isGos3" class="text-sm text-muted">Older gos3 versions only recorded encryption settings. Those existing objects remain unencrypted until uploaded or copied again.</p>
      <div class="flex justify-end gap-2 border-t border-default pt-4">
        <UButton type="submit" icon="i-lucide-save" label="Save" :disabled="!dirty" :loading="save.loading.value" />
      </div>
    </form>
  </UCard>
</template>
