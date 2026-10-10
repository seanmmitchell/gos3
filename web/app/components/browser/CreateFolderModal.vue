<script setup lang="ts">
/**
 * "New folder" dialog: creates a zero-byte "prefix/" placeholder object.
 * Nested names ("a/b") create nested folders.
 */
import type { S3Client } from '~/utils/s3/client'

const open = defineModel<boolean>('open', { required: true })

const props = defineProps<{
  client: S3Client | undefined
  bucket: string
  /** Folder the new folder is created in ('' = bucket root). */
  prefix: string
}>()

const emit = defineEmits<{ created: [prefix: string] }>()

const name = ref('')
const touched = ref(false)
const busy = ref(false)
const error = ref<unknown>(null)

watch(open, (v) => {
  if (!v) return
  name.value = ''
  touched.value = false
  error.value = null
})

const nameError = computed(() => {
  if (!name.value) return touched.value ? 'Enter a folder name' : undefined
  return validateFolderName(name.value) ?? validateObjectKey(folderKey(props.prefix, name.value))
})
const key = computed(() => (name.value && !nameError.value ? folderKey(props.prefix, name.value) : ''))
// Spaces are kept (they are part of the key): point them out.
const help = computed(() => {
  if (!key.value) return 'Use “/” to create nested folders.'
  return /(^|\/)\s|\s(\/|$)/.test(name.value) ? `Key: “${key.value}” (the spaces at the start or end of a name are kept)` : `Key: ${key.value}`
})

async function submit() {
  touched.value = true
  if (!key.value || !props.client) return
  busy.value = true
  error.value = null
  try {
    await props.client.putObject(props.bucket, key.value, new Uint8Array(0), { contentType: 'application/x-directory' })
    useToast().add({ title: `Folder ${name.value.replace(/\/+$/, '')} created`, color: 'success', icon: 'i-lucide-circle-check' })
    emit('created', key.value)
    open.value = false
  } catch (e) {
    error.value = e
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UModal v-model:open="open" title="New folder" :ui="{ footer: 'justify-end' }">
    <template #description>
      Creates an empty folder in <span class="font-mono">{{ bucket }}/{{ prefix }}</span>.
    </template>
    <template #body>
      <form id="create-folder-form" class="space-y-4" novalidate @submit.prevent="submit">
        <UFormField label="Folder name" :error="nameError" :help="help" required>
          <UInput v-model="name" class="w-full font-mono" placeholder="reports/2026" autocomplete="off" autofocus spellcheck="false" @blur="touched = true" />
        </UFormField>
        <CommonErrorAlert v-if="error" :error="error" title="Could not create the folder" />
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="open = false" />
      <UButton type="submit" form="create-folder-form" icon="i-lucide-folder-plus" label="Create folder" :loading="busy" :disabled="!key || !client" />
    </template>
  </UModal>
</template>
