<script setup lang="ts">
/**
 * Edits system headers (Content-Type, Cache-Control, Content-Disposition)
 * and user metadata (x-amz-meta-*). S3 cannot change metadata in place: the
 * object is copied onto itself with metadata directive REPLACE (in versioned
 * buckets this creates a new version). CopyObject does not copy the ACL, so
 * a canned ACL (e.g. public-read) is read first and passed on.
 */
import type { HeadResult } from '~/types/s3'
import type { KeyValuePair } from '~/types/ui'
import type { S3Client } from '~/utils/s3/client'

const props = defineProps<{
  client: S3Client | undefined
  bucket: string
  objectKey: string
  head: HeadResult
  versioned?: boolean
  readOnly?: boolean
}>()

const emit = defineEmits<{ saved: [] }>()

const contentType = ref('')
const cacheControl = ref('')
const contentDisposition = ref('')
const meta = ref<KeyValuePair[]>([])
const metaValid = ref(true)
const busy = ref(false)
const error = ref<unknown>(null)

function reset() {
  contentType.value = props.head.contentType ?? ''
  cacheControl.value = props.head.cacheControl ?? ''
  contentDisposition.value = props.head.contentDisposition ?? ''
  meta.value = Object.entries(props.head.metadata).map(([key, value]) => ({ key, value }))
  error.value = null
}
watch(() => props.head, reset, { immediate: true })

const dirty = computed(() => {
  const h = props.head
  if (contentType.value !== (h.contentType ?? '')) return true
  if (cacheControl.value !== (h.cacheControl ?? '')) return true
  if (contentDisposition.value !== (h.contentDisposition ?? '')) return true
  const cur = Object.entries(h.metadata)
  if (cur.length !== meta.value.length) return true
  return meta.value.some(p => h.metadata[p.key.trim().toLowerCase()] !== p.value)
})

const TOO_BIG = 2 * 1024
const metaSize = computed(() => new TextEncoder().encode(meta.value.map(p => p.key + p.value).join('')).length)

function validateMetaKey(k: string): string | undefined {
  if (!/^[A-Za-z0-9!#$%&'*+.^_`|~-]+$/.test(k)) return 'Use letters, digits and - _ . only'
  return undefined
}

const tooLargeToCopy = computed(() => props.head.contentLength > 5 * 1024 ** 3)

async function save() {
  if (!props.client || !metaValid.value || !dirty.value) return
  busy.value = true
  error.value = null
  const h = props.head
  // Preserve headers that REPLACE would otherwise drop.
  const headers: Record<string, string> = {}
  if (h.contentEncoding) headers['Content-Encoding'] = h.contentEncoding
  if (h.contentLanguage) headers['Content-Language'] = h.contentLanguage
  if (h.expires) headers.Expires = h.expires
  if (h.serverSideEncryption) headers['x-amz-server-side-encryption'] = h.serverSideEncryption
  if (h.sseKmsKeyId) headers['x-amz-server-side-encryption-aws-kms-key-id'] = h.sseKmsKeyId
  try {
    const acl = await aclToKeep(props.client, { bucket: props.bucket, key: props.objectKey })
    await props.client.copyObject(
      { bucket: props.bucket, key: props.objectKey },
      { bucket: props.bucket, key: props.objectKey },
      {
        metadataDirective: 'REPLACE',
        contentType: contentType.value.trim() || undefined,
        cacheControl: cacheControl.value.trim() || undefined,
        contentDisposition: contentDisposition.value.trim() || undefined,
        metadata: Object.fromEntries(meta.value.map(p => [p.key.trim().toLowerCase(), p.value])),
        storageClass: h.storageClass && h.storageClass !== 'STANDARD' ? h.storageClass : undefined,
        acl: acl === 'custom' ? undefined : acl,
        headers
      }
    )
    if (acl === 'custom') {
      useToast().add({ title: 'Metadata saved, ACL reset', description: 'The object had custom ACL grants, which cannot be carried over: it is private now.', color: 'warning', icon: 'i-lucide-shield-alert' })
    } else {
      useToast().add({ title: 'Metadata saved', color: 'success', icon: 'i-lucide-circle-check' })
    }
    emit('saved')
  } catch (e) {
    error.value = e
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <form class="space-y-4" novalidate @submit.prevent="save">
    <UAlert
      v-if="tooLargeToCopy && !readOnly"
      color="warning"
      variant="subtle"
      icon="i-lucide-triangle-alert"
      title="Objects larger than 5 GiB cannot be copied, so their metadata cannot be edited."
    />
    <div class="grid gap-3">
      <UFormField label="Content-Type">
        <UInput v-model="contentType" class="w-full font-mono" placeholder="application/octet-stream" :readonly="readOnly" autocomplete="off" />
      </UFormField>
      <UFormField label="Cache-Control">
        <UInput v-model="cacheControl" class="w-full font-mono" placeholder="max-age=3600" :readonly="readOnly" autocomplete="off" />
      </UFormField>
      <UFormField label="Content-Disposition">
        <UInput v-model="contentDisposition" class="w-full font-mono" placeholder="inline" :readonly="readOnly" autocomplete="off" />
      </UFormField>
    </div>

    <fieldset class="space-y-2">
      <legend class="mb-2 text-sm font-medium text-highlighted">
        User metadata <span class="font-normal text-muted">(x-amz-meta-*)</span>
      </legend>
      <CommonKeyValueEditor
        v-model="meta"
        key-label="Name"
        value-label="Value"
        key-placeholder="name"
        :readonly="readOnly"
        :validate-key="validateMetaKey"
        add-label="Add metadata"
        @validity="metaValid = $event"
      />
      <p v-if="metaSize > TOO_BIG" class="text-xs text-error">
        User metadata must not exceed 2 KiB ({{ formatBytes(metaSize) }}).
      </p>
    </fieldset>

    <CommonErrorAlert v-if="error" :error="error" title="Could not save metadata" />

    <div v-if="!readOnly" class="flex flex-wrap items-center justify-end gap-2">
      <p v-if="versioned && dirty" class="mr-auto text-xs text-muted">
        Saving creates a new version of the object.
      </p>
      <UButton color="neutral" variant="ghost" label="Reset" :disabled="!dirty || busy" @click="reset" />
      <UButton type="submit" label="Save metadata" icon="i-lucide-save" :loading="busy" :disabled="!dirty || !metaValid || metaSize > TOO_BIG || tooLargeToCopy" />
    </div>
  </form>
</template>
