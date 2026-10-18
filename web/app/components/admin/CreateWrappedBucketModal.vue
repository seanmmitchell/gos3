<script setup lang="ts">
import type { ContainerInfo } from '~/types/admin'
import { addedContainerFromError, noErrors, requestReachedServer, validateWrappedForm, wrappedRequest, type WrappedForm } from './-remote-buckets'
import { containerSelectItems, preferredContainer } from './-containers'

/**
 * Creates a wrapped bucket on an s3 backing (POST /backings/{name}/buckets
 * with kind wrapped; CreateBucket on the bucket): in one of the backing's
 * containers, or — when opened for a remote bucket of the browser — in a new
 * container inside that remote bucket (admin:ManageContainers and
 * admin:AdoptRemoteBucket). The caller becomes the bucket's owner.
 *
 * When a request that reached the server fails, the server may have kept a
 * new container (its error names it): the dialog emits `changed` so the page
 * reloads, re-reads the containers and offers the kept one under "An
 * existing container" for the retry.
 *
 * Opened through useOverlay(); emits close(name of the new gos3 bucket) once
 * created, or close(null) when cancelled.
 */
const props = defineProps<{
  /** The s3 backing. */
  backing: string
  /** The backing's containers (Backing.account.containers). */
  containers?: ContainerInfo[] | null
  /** The backing's default container. */
  defaultContainer?: string
  /** Offer a new container inside this existing remote bucket (preselected). */
  remoteBucket?: string
  /** Its region, when known. */
  region?: string
}>()

const emit = defineEmits<{
  close: [value: string | null]
  /** A failed request may have changed the backing (e.g. kept a new container). */
  changed: []
}>()

const { requireAdmin } = useConnection()
const formId = useId()

/** The containers offered, re-read after a failure. */
const containerList = ref<ContainerInfo[]>([...(props.containers ?? [])])
watch(() => props.containers, (v) => {
  containerList.value = [...(v ?? [])]
})
const items = computed(() => containerSelectItems(containerList.value))
const form = reactive<WrappedForm>({
  bucket: '',
  target: props.remoteBucket ? 'new' : 'existing',
  container: preferredContainer(props.containers, props.defaultContainer),
  remoteBucket: props.remoteBucket ?? '',
  // A prefix keeps gos3's data apart from the bucket's other objects.
  prefix: props.remoteBucket ? 'gos3/' : '',
  label: '',
  region: props.region ?? ''
})

const targetItems = computed(() => [
  { value: 'new' as const, label: `New container in ${props.remoteBucket}`, description: 'The prefix must be empty; the container can hold more wrapped buckets later.' },
  { value: 'existing' as const, label: 'An existing container', description: items.value.length ? 'Share a container with other wrapped buckets.' : 'This backing has no container yet.', disabled: !items.value.length }
])

const submitted = ref(false)
const errors = computed(() => validateWrappedForm(form))

const create = useAsyncAction(
  () => requireAdmin().createBucketOnBacking(props.backing, wrappedRequest(form)),
  { successMessage: s => `Wrapped bucket ${s?.name || form.bucket.trim()} created`, errorTitle: 'Could not create the bucket', toastErrors: false }
)

/** Set when a failed request kept a new container: the retry uses it. */
const keptContainer = ref('')

/** After a failure that reached the server: reload, and offer a kept container. */
async function afterFailure(e: unknown) {
  emit('changed')
  if (form.target !== 'new') return
  const kept = addedContainerFromError(e)
  try {
    containerList.value = await requireAdmin().listContainers(props.backing)
  } catch {
    // Listing may be denied: offer the container the error names.
  }
  if (kept && !containerList.value.some(c => c.id === kept)) {
    containerList.value = [...containerList.value, {
      id: kept,
      location: { bucket: form.remoteBucket.trim(), prefix: form.prefix.trim() },
      role: 'existing',
      buckets: 0,
      state: 'ready'
    }]
  }
  if (kept) {
    keptContainer.value = kept
    form.target = 'existing'
    form.container = kept
  }
}

async function submit() {
  submitted.value = true
  if (!noErrors(errors.value) || create.loading.value) return
  const s = await create.run()
  if (!create.error.value) {
    emit('close', s?.name || form.bucket.trim())
    return
  }
  if (requestReachedServer(create.error.value)) await afterFailure(create.error.value)
}
</script>

<template>
  <UModal
    title="Create wrapped bucket"
    description="Wrapped buckets store gos3's format inside a container, with every gos3 feature."
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form :id="formId" class="space-y-4" novalidate @submit.prevent="submit">
        <UFormField label="Bucket name" name="bucket" required help="The gos3 bucket name." :error="submitted && errors.bucket">
          <UInput v-model.trim="form.bucket" class="w-full font-mono" placeholder="my-bucket" autocomplete="off" spellcheck="false" aria-label="Bucket name" autofocus />
        </UFormField>
        <URadioGroup v-if="remoteBucket" v-model="form.target" legend="Container" :items="targetItems" />
        <template v-if="form.target === 'existing'">
          <UFormField v-if="items.length" label="Container" name="container" required :error="submitted && errors.container">
            <USelect v-model="form.container" :items="items" value-key="value" class="w-full" aria-label="Container" />
          </UFormField>
          <UAlert
            v-else
            color="warning"
            variant="subtle"
            icon="i-lucide-package-x"
            title="No container"
            description="This backing has no container for wrapped buckets. Add one under Containers first."
          />
        </template>
        <template v-else>
          <UFormField label="Prefix" name="prefix" help="Inside the remote bucket; must be empty. Leave empty to use the whole bucket." :error="submitted && errors.prefix">
            <UInput v-model.trim="form.prefix" class="w-full font-mono" placeholder="gos3/" autocomplete="off" aria-label="Prefix" />
          </UFormField>
          <UFormField label="Container label" name="label" help="Optional; shown to everyone who can create buckets." :error="submitted && errors.label">
            <UInput v-model="form.label" class="w-full" placeholder="photos-archive" autocomplete="off" aria-label="Container label" />
          </UFormField>
        </template>
        <CommonErrorAlert :error="create.error.value" title="Could not create the bucket" />
        <UAlert
          v-if="keptContainer && form.target === 'existing' && form.container === keptContainer"
          color="info"
          variant="subtle"
          icon="i-lucide-package-check"
          title="The new container was kept"
          :description="`Container ${keptContainer} was added before the bucket failed. Fix the problem and create the bucket in it.`"
          data-testid="kept-container"
        />
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="emit('close', null)" />
      <UButton
        type="submit"
        :form="formId"
        icon="i-lucide-package-plus"
        label="Create bucket"
        :loading="create.loading.value"
        :disabled="form.target === 'existing' && !items.length"
      />
    </template>
  </UModal>
</template>
