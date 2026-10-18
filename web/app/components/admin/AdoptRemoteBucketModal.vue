<script setup lang="ts">
import { adoptRequest, noErrors, suggestedBucketName, validateAdoptForm, type AdoptForm } from './-remote-buckets'

/**
 * Adopts an existing remote bucket of an s3 backing as a native gos3 bucket
 * (POST /backings/{name}/buckets with adopt: true; admin:AdoptRemoteBucket
 * and CreateBucket on the new bucket). Its objects stay as they are, so
 * other tools keep working; versioning and object lock follow the remote
 * bucket. The remote bucket is fixed when opened from a row of the remote
 * bucket browser, typed otherwise ("Adopt by name…").
 *
 * Opened through useOverlay(); emits close(name of the new gos3 bucket)
 * once adopted, or close(null) when cancelled.
 */
const props = defineProps<{
  /** The s3 backing. */
  backing: string
  /** The remote bucket (initial value; fixed when `fixed`). */
  remoteBucket?: string
  /** The remote bucket comes from the browser and cannot be changed. */
  fixed?: boolean
  /** The remote bucket's region, when known. */
  region?: string
  /** Initial gos3 name (default: derived from the remote name). */
  bucketName?: string
}>()

const emit = defineEmits<{ close: [value: string | null] }>()

const { requireAdmin } = useConnection()
const formId = useId()

const form = reactive<AdoptForm>({
  remoteBucket: props.remoteBucket ?? '',
  bucket: props.bucketName || suggestedBucketName(props.remoteBucket ?? ''),
  region: props.region ?? ''
})
// The gos3 name follows the remote name until it is edited.
const nameEdited = ref(!!props.bucketName)
watch(() => form.remoteBucket, (v) => {
  if (!nameEdited.value) form.bucket = suggestedBucketName(v.trim())
})

const submitted = ref(false)
const errors = computed(() => validateAdoptForm(form))

const adopt = useAsyncAction(
  () => requireAdmin().createBucketOnBacking(props.backing, adoptRequest(form)),
  { successMessage: s => `${form.remoteBucket.trim()} adopted as bucket ${s?.name || form.bucket.trim()}`, errorTitle: 'Could not adopt the bucket', toastErrors: false }
)

async function submit() {
  submitted.value = true
  if (!noErrors(errors.value) || adopt.loading.value) return
  const summary = await adopt.run()
  if (!adopt.error.value) emit('close', summary?.name || form.bucket.trim())
}
</script>

<template>
  <UModal
    :title="fixed && remoteBucket ? `Adopt ${remoteBucket}` : 'Adopt a remote bucket'"
    description="The remote bucket becomes a native gos3 bucket on this backing."
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form :id="formId" class="space-y-4" novalidate @submit.prevent="submit">
        <UFormField label="Remote bucket" name="remoteBucket" required help="An existing bucket of this backing's account." :error="submitted && errors.remoteBucket">
          <UInput
            v-model.trim="form.remoteBucket"
            :disabled="fixed"
            class="w-full font-mono"
            placeholder="acme-photos"
            autocomplete="off"
            spellcheck="false"
            aria-label="Remote bucket"
            :autofocus="!fixed"
          />
        </UFormField>
        <UFormField label="gos3 bucket name" name="bucket" required help="The name S3 clients use for it on this server." :error="submitted && errors.bucket">
          <UInput
            v-model.trim="form.bucket"
            class="w-full font-mono"
            placeholder="photos"
            autocomplete="off"
            spellcheck="false"
            aria-label="gos3 bucket name"
            :autofocus="fixed"
            @update:model-value="nameEdited = true"
          />
        </UFormField>
        <UFormField label="Region" name="region" help="Of the remote bucket; leave empty to discover it." :error="submitted && errors.region">
          <UInput v-model.trim="form.region" class="w-full font-mono" placeholder="Discovered" autocomplete="off" aria-label="Region" />
        </UFormField>
        <UAlert
          color="info"
          variant="subtle"
          icon="i-lucide-info"
          description="Objects stay as they are, so other tools keep working with the remote bucket. Versioning and object lock follow the remote bucket. Remote buckets that hold gos3 data or are used elsewhere are refused."
        />
        <CommonErrorAlert :error="adopt.error.value" title="Could not adopt the bucket" />
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="emit('close', null)" />
      <UButton type="submit" :form="formId" icon="i-lucide-cloud-download" label="Adopt bucket" :loading="adopt.loading.value" />
    </template>
  </UModal>
</template>
