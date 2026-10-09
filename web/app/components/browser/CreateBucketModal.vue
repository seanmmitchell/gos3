<script setup lang="ts">
/**
 * "Create bucket" dialog: S3 name rules with live hints, region, object lock
 * and versioning (PutBucketVersioning right after CreateBucket).
 *
 *   <BrowserCreateBucketModal v-model:open="open" :default-region="conn.region" @created="refresh" />
 */
import type { S3Client } from '~/utils/s3/client'

const open = defineModel<boolean>('open', { required: true })

const props = defineProps<{
  client: S3Client | undefined
  defaultRegion?: string
  existingNames?: string[]
}>()

const emit = defineEmits<{ created: [name: string] }>()

const name = ref('')
const region = ref('')
const objectLock = ref(false)
const versioning = ref(false)
const touched = ref(false)
const busy = ref(false)
const error = ref<unknown>(null)
const warning = ref('')

watch(open, (v) => {
  if (!v) return
  name.value = ''
  region.value = props.defaultRegion ?? ''
  objectLock.value = false
  versioning.value = false
  touched.value = false
  error.value = null
  warning.value = ''
})

// Object lock requires versioning.
watch(objectLock, (v) => {
  if (v) versioning.value = true
})

const rules = computed(() => {
  const n = name.value
  return [
    { label: '3–63 characters', ok: n.length >= 3 && n.length <= 63 },
    { label: 'Lowercase letters, digits, dots (.) and hyphens (-) only', ok: n.length > 0 && /^[a-z0-9.-]+$/.test(n) },
    { label: 'Starts and ends with a letter or digit', ok: /^[a-z0-9]/.test(n) && /[a-z0-9]$/.test(n) },
    { label: 'No two adjacent dots, not an IP address', ok: n.length > 0 && !n.includes('..') && !/^\d{1,3}(\.\d{1,3}){3}$/.test(n) }
  ]
})

const nameError = computed(() => {
  if (!name.value) return touched.value ? 'Enter a bucket name' : undefined
  const e = validateBucketName(name.value)
  if (e) return e
  if (props.existingNames?.includes(name.value)) return 'You already have a bucket with this name'
  return undefined
})

const canSubmit = computed(() => !!name.value && !nameError.value && !busy.value && !!props.client)

async function submit() {
  touched.value = true
  if (!canSubmit.value || !props.client) return
  busy.value = true
  error.value = null
  warning.value = ''
  const bucket = name.value
  try {
    await props.client.createBucket(bucket, { region: region.value.trim() || undefined, objectLock: objectLock.value })
  } catch (e) {
    error.value = e
    busy.value = false
    return
  }
  if (versioning.value && !objectLock.value) {
    try {
      await props.client.putBucketVersioning(bucket, 'Enabled')
    } catch (e) {
      warning.value = `The bucket was created, but versioning could not be enabled: ${errorMessage(e)}`
    }
  }
  busy.value = false
  useToast().add({ title: `Bucket ${bucket} created`, color: 'success', icon: 'i-lucide-circle-check' })
  emit('created', bucket)
  if (warning.value) {
    useToast().add({ title: 'Versioning not enabled', description: warning.value, color: 'warning', icon: 'i-lucide-triangle-alert' })
  }
  open.value = false
}
</script>

<template>
  <UModal
    v-model:open="open"
    title="Create bucket"
    description="Buckets hold objects. Names are global on the server and cannot be changed later."
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form id="create-bucket-form" class="space-y-5" novalidate @submit.prevent="submit">
        <UFormField label="Bucket name" :error="nameError" required>
          <UInput
            v-model.trim="name"
            class="w-full font-mono"
            placeholder="my-bucket"
            autocomplete="off"
            autofocus
            spellcheck="false"
            :aria-invalid="!!nameError"
            @blur="touched = true"
          />
        </UFormField>
        <ul class="-mt-3 space-y-0.5 text-xs" aria-label="Bucket name rules">
          <li v-for="r in rules" :key="r.label" class="flex items-center gap-1.5" :class="r.ok ? 'text-success' : name ? 'text-error' : 'text-muted'">
            <UIcon :name="r.ok ? 'i-lucide-check' : name ? 'i-lucide-x' : 'i-lucide-dot'" class="size-3.5 shrink-0" aria-hidden="true" />
            <span>{{ r.label }}<span class="sr-only">{{ r.ok ? ' (satisfied)' : ' (not satisfied)' }}</span></span>
          </li>
        </ul>

        <UFormField label="Region" help="Leave empty to use the server's default region.">
          <UInput v-model.trim="region" class="w-full font-mono" placeholder="us-east-1" autocomplete="off" />
        </UFormField>

        <div class="space-y-3 rounded-lg border border-default p-3">
          <USwitch
            v-model="versioning"
            label="Versioning"
            description="Keep every version of every object; deletes create delete markers."
            :disabled="objectLock"
          />
          <USwitch
            v-model="objectLock"
            label="Object lock"
            description="Allows WORM retention and legal holds. Can only be enabled at creation and implies versioning."
          />
        </div>

        <CommonErrorAlert v-if="error" :error="error" title="Could not create the bucket" />
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="open = false" />
      <UButton type="submit" form="create-bucket-form" icon="i-lucide-plus" label="Create bucket" :loading="busy" :disabled="!canSubmit" />
    </template>
  </UModal>
</template>
