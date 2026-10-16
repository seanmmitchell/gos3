<script setup lang="ts">
import type { Job, Snapshot } from '~/types/admin'
import { emptyRestoreForm, restoreRequest, validateRestoreForm, type RestoreForm } from './-restic'

/**
 * "Restore to bucket…" for one whole-bucket snapshot (POST
 * /buckets/{name}/restic/snapshots/{id}/restore, admin:RestoreBucketSnapshot
 * on the bucket and admin:CreateCopyJob on the target): into a new bucket
 * (on a chosen backing) or an existing one (refused unless empty, or
 * merged), optionally only keys with a prefix, with or without the
 * snapshot's bucket settings. The restore runs as a restic-restore job.
 *
 * Opened through useOverlay(); emits close(the job) once started, or
 * close(null) when cancelled.
 */
const props = defineProps<{
  bucket: string
  snapshot: Snapshot
  /** Connection ID, for the "Open jobs" link. */
  serverId?: string
}>()

const emit = defineEmits<{ close: [value: Job | null] }>()

const { requireS3, requireAdmin } = useConnection()
const { whoami } = useWhoAmI()
const toast = useToast()
const formId = useId()

const buckets = ref<string[]>([])
const bucketsLoaded = ref(false)
const form = reactive<RestoreForm>(emptyRestoreForm(props.bucket))
const nameEdited = ref(false)

void requireS3().listBuckets().then((r) => {
  buckets.value = r.buckets.map(b => b.name).sort()
  bucketsLoaded.value = true
  if (!nameEdited.value && form.target === 'new') form.bucket = emptyRestoreForm(props.bucket, buckets.value).bucket
}, () => {
  // Optional: the dialog then takes typed names.
})

watch(() => form.target, (t) => {
  form.bucket = t === 'new' ? emptyRestoreForm(props.bucket, buckets.value).bucket : ''
})

const backingItems = computed(() => backingSelectItems(whoami.value?.backings))
const backingModel = computed<string>({
  get: () => form.backing || defaultBacking(whoami.value?.backings),
  set: (v) => {
    form.backing = v
  }
})

const submitted = ref(false)
const errors = computed(() => validateRestoreForm(form, bucketsLoaded.value ? buckets.value : []))
const valid = computed(() => !errors.value.bucket && !errors.value.prefix)

const targetItems = [
  { value: 'new', label: 'New bucket', description: 'Created by the restore.' },
  { value: 'existing', label: 'Existing bucket', description: 'Another bucket, or this one.' }
]
const onExistingItems = [
  { value: 'fail', label: 'Refuse unless empty', description: 'The restore fails if the bucket holds objects.' },
  { value: 'merge', label: 'Merge', description: 'Restored objects are written next to (or over) the bucket’s objects.' }
]
const configItems = [
  { value: 'safe', label: 'Restore bucket settings', description: 'The snapshot’s CORS, tags, policy and other safe settings.' },
  { value: 'none', label: 'Objects only' }
]

const restore = useAsyncAction(
  () => requireAdmin().restoreSnapshot(props.bucket, props.snapshot.id, restoreRequest(form)),
  { errorTitle: 'Could not start the restore', toastErrors: false }
)

async function submit() {
  submitted.value = true
  if (!valid.value || restore.loading.value) return
  const job = await restore.run()
  if (!job) return
  toast.add({
    title: 'Restore started',
    description: `Snapshot ${props.snapshot.shortId || props.snapshot.id.slice(0, 8)} → ${form.bucket.trim()}`,
    color: 'success',
    icon: 'i-lucide-circle-check',
    actions: props.serverId ? [{ label: 'Open jobs', color: 'neutral', variant: 'outline', to: appRoutes.jobs(props.serverId, props.bucket) }] : undefined
  })
  emit('close', job)
}
</script>

<template>
  <UModal
    title="Restore snapshot to a bucket"
    :description="`Snapshot ${snapshot.shortId || snapshot.id.slice(0, 8)} of ${bucket}, taken ${formatDate(snapshot.time)}.`"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form :id="formId" class="space-y-4" novalidate data-testid="restore-form" @submit.prevent="submit">
        <URadioGroup v-model="form.target" legend="Target" :items="targetItems" orientation="horizontal" variant="table" />
        <UFormField v-if="form.target === 'new'" label="New bucket name" name="bucket" required :error="submitted && errors.bucket">
          <UInput v-model.trim="form.bucket" class="w-full font-mono" autocomplete="off" spellcheck="false" aria-label="New bucket name" @update:model-value="nameEdited = true" />
        </UFormField>
        <UFormField v-else label="Bucket" name="bucket" required :error="submitted && errors.bucket">
          <USelectMenu v-if="bucketsLoaded" v-model="form.bucket" :items="buckets" class="w-full" placeholder="Choose a bucket" :search-input="{ placeholder: 'Filter buckets…' }" aria-label="Target bucket" />
          <UInput v-else v-model.trim="form.bucket" class="w-full font-mono" autocomplete="off" spellcheck="false" aria-label="Target bucket" />
        </UFormField>
        <UFormField v-if="form.target === 'new' && backingItems.length" label="Storage backing" name="backing">
          <USelect v-model="backingModel" :items="backingItems" value-key="value" class="w-full" aria-label="Storage backing" />
        </UFormField>
        <UFormField label="Prefix" name="prefix" help="Restore only keys starting with this prefix; empty = every key." :error="submitted && errors.prefix">
          <UInput v-model="form.prefix" class="w-full font-mono" placeholder="photos/2024/" autocomplete="off" spellcheck="false" aria-label="Prefix" />
        </UFormField>
        <URadioGroup v-if="form.target === 'existing'" v-model="form.onExisting" legend="If the bucket holds objects" :items="onExistingItems" />
        <URadioGroup v-model="form.configs" legend="Bucket settings" :items="configItems" />
        <CommonErrorAlert v-if="restore.error.value" :error="restore.error.value" title="Could not start the restore" />
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="emit('close', null)" />
      <UButton type="submit" :form="formId" icon="i-lucide-archive-restore" label="Start restore" :loading="restore.loading.value" />
    </template>
  </UModal>
</template>
