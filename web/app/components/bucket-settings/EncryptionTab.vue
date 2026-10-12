<script setup lang="ts">
/**
 * Default bucket encryption (?encryption): none, SSE-S3 (AES256) or SSE-KMS
 * with an optional key ID.
 */
const props = defineProps<{ bucket: string }>()
const emit = defineEmits<{ changed: [] }>()

type Mode = 'none' | 'AES256' | 'aws:kms'

const { requireS3 } = useConnection()
const confirm = useConfirm()

const mode = ref<Mode>('none')
const kmsKeyId = ref('')
const bucketKey = ref(false)
const original = ref('')

const loader = useLoader(async () => {
  const cfg = await requireS3().getBucketEncryption(props.bucket)
  const rule = cfg?.rules[0]
  mode.value = !rule ? 'none' : rule.sseAlgorithm === 'AES256' ? 'AES256' : 'aws:kms'
  kmsKeyId.value = rule?.kmsMasterKeyId ?? ''
  bucketKey.value = !!rule?.bucketKeyEnabled
  original.value = signature()
  return cfg
}, { watch: [() => props.bucket] })

function signature(): string {
  return JSON.stringify([mode.value, mode.value === 'aws:kms' ? kmsKeyId.value.trim() : '', mode.value === 'aws:kms' && bucketKey.value])
}
const dirty = computed(() => signature() !== original.value)

const items = [
  { label: 'None', value: 'none', description: 'Objects are stored as uploaded unless a request asks for encryption.' },
  { label: 'SSE-S3 (AES256)', value: 'AES256', description: 'Server-side encryption with server-managed keys.' },
  { label: 'SSE-KMS (aws:kms)', value: 'aws:kms', description: 'Server-side encryption with a key from a key management service.' }
]

const save = useAsyncAction(async () => {
  const s3 = requireS3()
  if (mode.value === 'none') {
    await s3.deleteBucketEncryption(props.bucket)
  } else {
    await s3.putBucketEncryption(props.bucket, {
      rules: [{
        sseAlgorithm: mode.value,
        kmsMasterKeyId: mode.value === 'aws:kms' && kmsKeyId.value.trim() ? kmsKeyId.value.trim() : undefined,
        bucketKeyEnabled: mode.value === 'aws:kms' ? bucketKey.value : undefined
      }]
    })
  }
  await loader.refresh()
  emit('changed')
}, { successMessage: 'Default encryption saved', errorTitle: 'Could not save encryption settings' })

async function onSave() {
  if (mode.value === 'none' && loader.data.value) {
    const ok = await confirm({ title: 'Remove default encryption?', description: 'New objects are no longer encrypted by default. Existing objects are not changed.', confirmLabel: 'Remove', color: 'warning' })
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
          <h3 class="font-semibold text-highlighted">
            Default encryption
          </h3>
          <p class="text-sm text-muted">
            Applied to new objects that are uploaded without encryption headers. Existing objects are not changed.
          </p>
        </div>
        <UBadge v-if="dirty" label="Unsaved" color="warning" variant="subtle" />
      </div>
    </template>
    <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Could not load encryption settings" retryable @retry="loader.refresh" />
    <USkeleton v-else-if="loader.loading.value && !original" class="h-32 w-full" />
    <form v-else class="space-y-4" novalidate @submit.prevent="onSave">
      <URadioGroup v-model="mode" :items="items" legend="Encryption type" />
      <div v-if="mode === 'aws:kms'" class="grid gap-4 sm:grid-cols-2">
        <UFormField label="KMS key ID" name="kmsKeyId" help="Key ID or ARN; empty = the server's default key.">
          <UInput v-model="kmsKeyId" class="w-full font-mono" placeholder="arn:aws:kms:…" />
        </UFormField>
        <div class="sm:pt-6">
          <USwitch v-model="bucketKey" label="Bucket key" description="Reduce KMS requests with a bucket-level key." />
        </div>
      </div>
      <div class="flex justify-end gap-2 border-t border-default pt-4">
        <UButton type="submit" icon="i-lucide-save" label="Save" :disabled="!dirty" :loading="save.loading.value" />
      </div>
    </form>
  </UCard>
</template>
