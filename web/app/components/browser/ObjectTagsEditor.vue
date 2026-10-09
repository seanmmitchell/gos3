<script setup lang="ts">
/** Object tags (GetObjectTagging / PutObjectTagging / DeleteObjectTagging). */
import type { KeyValuePair } from '~/types/ui'
import type { S3Client } from '~/utils/s3/client'

const props = defineProps<{
  client: S3Client | undefined
  bucket: string
  objectKey: string
  versionId?: string
  readOnly?: boolean
}>()

const tags = ref<KeyValuePair[]>([])
const original = ref<KeyValuePair[]>([])
const valid = ref(true)
const loading = ref(false)
const saving = ref(false)
const loadError = ref<unknown>(null)
const saveError = ref<unknown>(null)

async function load() {
  if (!props.client) return
  loading.value = true
  loadError.value = null
  try {
    const t = await props.client.getObjectTagging(props.bucket, props.objectKey, props.versionId)
    original.value = t.map(x => ({ key: x.key, value: x.value }))
    tags.value = original.value.map(x => ({ ...x }))
  } catch (e) {
    loadError.value = e
  } finally {
    loading.value = false
  }
}
watch(() => [props.bucket, props.objectKey, props.versionId], () => void load(), { immediate: true })

const dirty = computed(() => JSON.stringify(tags.value) !== JSON.stringify(original.value))

function validateKey(k: string) {
  if (k.length > 128) return 'At most 128 characters'
  if (k.toLowerCase().startsWith('aws:')) return 'The aws: prefix is reserved'
  return undefined
}
function validateValue(v: string) {
  return v.length > 256 ? 'At most 256 characters' : undefined
}

async function save() {
  if (!props.client || !valid.value || !dirty.value) return
  saving.value = true
  saveError.value = null
  try {
    const list = tags.value.map(t => ({ key: t.key.trim(), value: t.value }))
    if (list.length) await props.client.putObjectTagging(props.bucket, props.objectKey, list, props.versionId)
    else await props.client.deleteObjectTagging(props.bucket, props.objectKey, props.versionId)
    original.value = list
    tags.value = list.map(x => ({ ...x }))
    useToast().add({ title: list.length ? 'Tags saved' : 'Tags removed', color: 'success', icon: 'i-lucide-circle-check' })
  } catch (e) {
    saveError.value = e
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="space-y-3">
    <div v-if="loading" class="space-y-2" aria-busy="true" aria-label="Loading tags">
      <USkeleton class="h-8 w-full" />
      <USkeleton class="h-8 w-2/3" />
    </div>
    <CommonErrorAlert v-else-if="loadError" :error="loadError" title="Could not load tags" retryable @retry="load" />
    <form v-else class="space-y-3" novalidate @submit.prevent="save">
      <p class="text-xs text-muted">
        Up to 10 tags. Tags can drive lifecycle rules and access policies.
      </p>
      <CommonKeyValueEditor
        v-model="tags"
        key-label="Tag"
        value-label="Value"
        :max-items="10"
        :readonly="readOnly"
        :validate-key="validateKey"
        :validate-value="validateValue"
        add-label="Add tag"
        @validity="valid = $event"
      />
      <CommonErrorAlert v-if="saveError" :error="saveError" title="Could not save tags" />
      <div v-if="!readOnly" class="flex justify-end gap-2">
        <UButton color="neutral" variant="ghost" label="Reset" :disabled="!dirty || saving" @click="tags = original.map(x => ({ ...x }))" />
        <UButton type="submit" label="Save tags" icon="i-lucide-save" :loading="saving" :disabled="!dirty || !valid" />
      </div>
    </form>
  </div>
</template>
