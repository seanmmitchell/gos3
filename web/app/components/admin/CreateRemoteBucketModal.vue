<script setup lang="ts">
import { createRemoteRequest, noErrors, validateCreateRemoteForm, type CreateRemoteForm } from './-remote-buckets'

/**
 * Creates a bucket in an s3 backing's account (POST
 * /backings/{name}/remote-buckets; admin:CreateRemoteBucket). It is created
 * unmanaged: adopt it, use it as a container or create a wrapped bucket in it
 * afterwards. 409 when it exists.
 *
 * Opened through useOverlay(); emits close(remote bucket name) once created,
 * or close(null) when cancelled.
 */
const props = defineProps<{
  /** The s3 backing. */
  backing: string
}>()

const emit = defineEmits<{ close: [value: string | null] }>()

const { requireAdmin } = useConnection()
const formId = useId()

const form = reactive<CreateRemoteForm>({ name: '', region: '', objectLock: false })
const submitted = ref(false)
const errors = computed(() => validateCreateRemoteForm(form))

const create = useAsyncAction(
  () => requireAdmin().createRemoteBucket(props.backing, createRemoteRequest(form)),
  { successMessage: r => `Remote bucket ${r?.name || form.name.trim()} created`, errorTitle: 'Could not create the remote bucket', toastErrors: false }
)

async function submit() {
  submitted.value = true
  if (!noErrors(errors.value) || create.loading.value) return
  const r = await create.run()
  if (!create.error.value) emit('close', r?.name || form.name.trim())
}
</script>

<template>
  <UModal
    title="Create remote bucket"
    :description="`A new bucket in the account of ${backing}. gos3 does not use it until you adopt it or put a container in it.`"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form :id="formId" class="space-y-4" novalidate @submit.prevent="submit">
        <UFormField label="Remote bucket name" name="name" required help="3–63 lowercase letters, digits, dots and hyphens." :error="submitted && errors.name">
          <UInput v-model.trim="form.name" class="w-full font-mono" placeholder="acme-photos" autocomplete="off" spellcheck="false" aria-label="Remote bucket name" autofocus />
        </UFormField>
        <UFormField label="Region" name="region" help="Leave empty to use the backing's region." :error="submitted && errors.region">
          <UInput v-model.trim="form.region" class="w-full font-mono" placeholder="Backing's region" autocomplete="off" aria-label="Region" />
        </UFormField>
        <USwitch
          v-model="form.objectLock"
          label="Object lock"
          description="Create the remote bucket with object lock (implies versioning). Some services only allow it at creation."
        />
        <CommonErrorAlert :error="create.error.value" title="Could not create the remote bucket" />
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="emit('close', null)" />
      <UButton type="submit" :form="formId" icon="i-lucide-plus" label="Create remote bucket" :loading="create.loading.value" />
    </template>
  </UModal>
</template>
