<script setup lang="ts">
import type { KeyValuePair } from '~/types/ui'

/** Bucket tags (?tagging), at most 50; an empty list deletes the tag set. */
const props = defineProps<{ bucket: string }>()
const emit = defineEmits<{ changed: [] }>()

const MAX_TAGS = 50
const { requireS3 } = useConnection()
const confirm = useConfirm()

const tags = ref<KeyValuePair[]>([])
const original = ref('[]')
const valid = ref(true)

const loader = useLoader(async () => {
  const list = await requireS3().getBucketTagging(props.bucket)
  tags.value = list.map(t => ({ key: t.key, value: t.value }))
  original.value = JSON.stringify(tags.value)
  return list
}, { watch: [() => props.bucket] })

const normalized = computed(() => tags.value.map(t => ({ key: t.key.trim(), value: t.value })))
const dirty = computed(() => JSON.stringify(normalized.value) !== original.value)

const validateKey = (k: string) => (k.length > 128 ? 'At most 128 characters' : k.toLowerCase().startsWith('aws:') ? 'The aws: prefix is reserved' : undefined)
const validateValue = (v: string) => (v.length > 256 ? 'At most 256 characters' : undefined)

const save = useAsyncAction(async () => {
  const s3 = requireS3()
  if (normalized.value.length) await s3.putBucketTagging(props.bucket, normalized.value)
  else await s3.deleteBucketTagging(props.bucket)
  await loader.refresh()
  emit('changed')
}, { successMessage: 'Bucket tags saved', errorTitle: 'Could not save tags' })

async function reset() {
  if (dirty.value && !(await confirm({ title: 'Discard changes?', confirmLabel: 'Discard', color: 'warning' }))) return
  tags.value = JSON.parse(original.value) as KeyValuePair[]
}
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 class="font-semibold text-highlighted">
            Tags
          </h3>
          <p class="text-sm text-muted">
            Key/value labels for cost allocation and organisation (up to {{ MAX_TAGS }}).
          </p>
        </div>
        <UBadge v-if="dirty" label="Unsaved" color="warning" variant="subtle" />
      </div>
    </template>
    <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Could not load tags" retryable @retry="loader.refresh" />
    <USkeleton v-else-if="loader.loading.value && !loader.data.value" class="h-24 w-full" />
    <div v-else class="space-y-4">
      <CommonKeyValueEditor
        v-model="tags"
        key-label="Key"
        value-label="Value"
        add-label="Add tag"
        :max-items="MAX_TAGS"
        :validate-key="validateKey"
        :validate-value="validateValue"
        @validity="valid = $event"
      />
      <div class="flex justify-end gap-2 border-t border-default pt-4">
        <UButton color="neutral" variant="ghost" icon="i-lucide-undo-2" label="Reset" :disabled="!dirty" @click="reset" />
        <UButton icon="i-lucide-save" label="Save tags" :disabled="!dirty || !valid" :loading="save.loading.value" @click="save.run()" />
      </div>
    </div>
  </UCard>
</template>
