<script setup lang="ts">
/**
 * Share link generator: presigned GET URL with an expiry of 1 hour to 7 days
 * (optionally forcing a download), with copy-to-clipboard.
 */
import type { S3Client } from '~/utils/s3/client'

const open = defineModel<boolean>('open', { required: true })

const props = defineProps<{
  client: S3Client | undefined
  bucket: string
  objectKey: string
  versionId?: string
}>()

const EXPIRY = [
  { label: '1 hour', value: 3600 },
  { label: '6 hours', value: 6 * 3600 },
  { label: '12 hours', value: 12 * 3600 },
  { label: '1 day', value: 86400 },
  { label: '3 days', value: 3 * 86400 },
  { label: '7 days (maximum)', value: 7 * 86400 }
]

const expires = ref(86400)
const forceDownload = ref(false)
const url = ref('')
const generatedAt = ref(0)
const busy = ref(false)
const error = ref<unknown>(null)
const urlId = useId()

const fileName = computed(() => baseName(props.objectKey))
const expiresAt = computed(() => (generatedAt.value ? new Date(generatedAt.value + expires.value * 1000) : null))

async function generate() {
  if (!props.client || !props.objectKey) return
  busy.value = true
  error.value = null
  try {
    url.value = await props.client.presignGetUrl(props.bucket, props.objectKey, {
      expiresSeconds: expires.value,
      versionId: props.versionId,
      responseContentDisposition: forceDownload.value ? attachmentDisposition(fileName.value) : undefined
    })
    generatedAt.value = Date.now()
  } catch (e) {
    error.value = e
    url.value = ''
  } finally {
    busy.value = false
  }
}

watch([open, expires, forceDownload, () => props.objectKey, () => props.versionId], () => {
  if (open.value) void generate()
}, { immediate: true })

function selectAll(e: FocusEvent) {
  (e.target as HTMLTextAreaElement | null)?.select()
}
</script>

<template>
  <UModal v-model:open="open" title="Share link" :ui="{ footer: 'justify-end' }">
    <template #description>
      Anyone with the link can download <span class="font-mono break-all">{{ objectKey }}</span><template v-if="versionId">
        (version <span class="font-mono">{{ truncateMiddle(versionId, 16) }}</span>)
      </template> until it expires.
    </template>
    <template #body>
      <div class="space-y-4">
        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Expires after">
            <USelect v-model="expires" :items="EXPIRY" class="w-full" />
          </UFormField>
          <div class="flex items-end pb-1">
            <USwitch v-model="forceDownload" label="Force download" description="Save instead of opening in the browser" />
          </div>
        </div>

        <CommonErrorAlert v-if="error" :error="error" title="Could not create the link" retryable @retry="generate" />

        <div v-else class="space-y-2">
          <label :for="urlId" class="block text-sm font-medium text-highlighted">Link</label>
          <div class="flex items-start gap-2">
            <UTextarea
              :id="urlId"
              :model-value="busy ? 'Generating…' : url"
              readonly
              :rows="4"
              autoresize
              class="w-full"
              :ui="{ base: 'font-mono text-xs break-all' }"
              @focus="selectAll"
            />
            <CommonCopyButton :value="url" label="Copy" variant="outline" size="sm" toast aria-label="Copy share link" :disabled="!url || busy" />
          </div>
          <p v-if="expiresAt" class="text-xs text-muted">
            Valid until {{ formatDate(expiresAt) }}. The link is signed with this connection's access key:
            disabling or deleting that key revokes it early.
          </p>
        </div>
      </div>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Close" @click="open = false" />
      <UButton icon="i-lucide-external-link" label="Open link" color="neutral" variant="outline" :to="url || undefined" target="_blank" rel="noopener" :disabled="!url || busy" />
    </template>
  </UModal>
</template>
