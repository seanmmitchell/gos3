<script setup lang="ts">
import type { RemoteAccess } from './-capabilities'
import { containerSpec, emptyContainerForm, validateContainerForm, type ContainerForm } from './-containers'
import { noErrors } from './-remote-buckets'

/**
 * Adds a container of wrapped buckets to an s3 backing (POST
 * /backings/{name}/containers; admin:ManageContainers): a new dedicated
 * remote bucket (also admin:CreateRemoteBucket), or a prefix of an existing
 * remote bucket (also admin:AdoptRemoteBucket), which must be empty unless it
 * already holds a gos3 store to attach. The server answers once the
 * container is loaded (at most 30 s).
 *
 * Opened through useOverlay(); emits close(container ID) once added, or
 * close(null) when cancelled.
 */
const props = defineProps<{
  /** The s3 backing. */
  backing: string
  /** What the identity may do (which sources are offered). */
  access: RemoteAccess
  /** Initial remote bucket (fixed when `fixed`). */
  bucket?: string
  /** The remote bucket comes from the browser and cannot be changed. */
  fixed?: boolean
  /** The remote bucket's region, when known. */
  region?: string
  /** Attach the gos3 store the location holds (a remote bucket with gos3 data). */
  attach?: boolean
}>()

const emit = defineEmits<{ close: [value: string | null] }>()

const { requireAdmin } = useConnection()
const formId = useId()

const canCreate = computed(() => props.access.create && !props.fixed)
const canUseExisting = computed(() => props.access.adopt)
const form = reactive<ContainerForm>(emptyContainerForm({
  bucket: props.bucket ?? '',
  region: props.region ?? '',
  source: props.fixed || !props.access.create ? 'existing' : 'create',
  attach: !!props.attach,
  // Inside an existing bucket a prefix keeps gos3's data apart from its other objects.
  prefix: props.fixed && !props.attach ? 'gos3/' : ''
}))

const sourceItems = computed(() => [
  { value: 'create' as const, label: 'Create a new remote bucket', description: 'A dedicated bucket for the container.', disabled: !canCreate.value },
  { value: 'existing' as const, label: 'An existing remote bucket', description: 'A prefix of a bucket of the account; it must be empty unless you attach a gos3 store.', disabled: !canUseExisting.value }
])

const submitted = ref(false)
const errors = computed(() => validateContainerForm(form))

const add = useAsyncAction(
  () => requireAdmin().addContainer(props.backing, containerSpec(form)),
  { successMessage: c => `Container ${c?.label || c?.id || form.bucket.trim()} added`, errorTitle: 'Could not add the container', toastErrors: false }
)

async function submit() {
  submitted.value = true
  if (!noErrors(errors.value) || add.loading.value) return
  const c = await add.run()
  if (!add.error.value) emit('close', c?.id ?? '')
}
</script>

<template>
  <UModal
    title="Add container"
    description="A container holds wrapped buckets in gos3's format: a dedicated remote bucket, or a prefix of an existing one."
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form :id="formId" class="space-y-4" novalidate @submit.prevent="submit">
        <URadioGroup v-if="!fixed" v-model="form.source" legend="Remote bucket" :items="sourceItems" />
        <UFormField
          label="Remote bucket"
          name="bucket"
          required
          :help="form.source === 'create' ? '3–63 lowercase letters, digits, dots and hyphens.' : 'An existing bucket of this backing\'s account.'"
          :error="submitted && errors.bucket"
        >
          <UInput
            v-model.trim="form.bucket"
            :disabled="fixed"
            class="w-full font-mono"
            placeholder="acme-gos3-archive"
            autocomplete="off"
            spellcheck="false"
            aria-label="Remote bucket"
            :autofocus="!fixed"
          />
        </UFormField>
        <UFormField
          label="Prefix"
          name="prefix"
          :help="form.source === 'create' ? 'Optional; leave empty to use the whole bucket.' : 'Must be empty unless you attach a gos3 store; leave empty to use the whole bucket.'"
          :error="submitted && errors.prefix"
        >
          <UInput v-model.trim="form.prefix" class="w-full font-mono" placeholder="gos3/" autocomplete="off" aria-label="Prefix" />
        </UFormField>
        <UFormField label="Region" name="region" help="Of the remote bucket; leave empty for the discovered or the backing's region." :error="submitted && errors.region">
          <UInput v-model.trim="form.region" class="w-full font-mono" placeholder="Default" autocomplete="off" aria-label="Region" />
        </UFormField>
        <UCheckbox
          v-if="form.source === 'existing'"
          v-model="form.attach"
          label="Attach an existing gos3 store"
          description="The location already holds gos3 data, for example moved from another server; its wrapped buckets are added to this backing."
        />
        <UFormField label="Label" name="label" help="Optional; shown to everyone who can create buckets. Don't put secrets in it." :error="submitted && errors.label">
          <UInput v-model="form.label" class="w-full" placeholder="photos-archive" autocomplete="off" aria-label="Label" />
        </UFormField>
        <CommonErrorAlert :error="add.error.value" title="Could not add the container" />
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="emit('close', null)" />
      <UButton type="submit" :form="formId" icon="i-lucide-package-plus" label="Add container" :loading="add.loading.value" />
    </template>
  </UModal>
</template>
