<script setup lang="ts">
/**
 * Inline preview of an object: images / video / audio through a presigned
 * URL (size-capped), text, JSON and Markdown through a ranged GET of the
 * first 256 KiB.
 */
import type { S3Client } from '~/utils/s3/client'

const props = defineProps<{
  client: S3Client | undefined
  bucket: string
  objectKey: string
  versionId?: string
  size: number
  contentType?: string
}>()

const kind = computed(() => previewKind(props.objectKey, props.contentType))
const mediaUrl = ref('')
const text = ref('')
const truncated = ref(false)
const loading = ref(false)
const error = ref<unknown>(null)
const force = ref(false)
const mediaFailed = ref(false)
const rendered = ref<'rendered' | 'source'>('rendered')

const tooLarge = computed(() => ['image', 'video', 'audio'].includes(kind.value) && props.size > PREVIEW_MEDIA_MAX_BYTES && !force.value)

const json = computed(() => (kind.value === 'json' && !truncated.value ? prettyJson(text.value) : { text: text.value, valid: false }))
// null when rendering failed: the source is shown instead of a broken preview.
const markdownHtml = computed<string | null>(() => {
  if (kind.value !== 'markdown') return ''
  try {
    return renderMarkdown(text.value)
  } catch {
    return null
  }
})

let seq = 0
async function load() {
  const my = ++seq
  mediaUrl.value = ''
  text.value = ''
  truncated.value = false
  error.value = null
  mediaFailed.value = false
  if (!props.client || kind.value === 'none' || tooLarge.value) return
  loading.value = true
  try {
    if (kind.value === 'image' || kind.value === 'video' || kind.value === 'audio') {
      const url = await props.client.presignGetUrl(props.bucket, props.objectKey, { versionId: props.versionId, expiresSeconds: 3600 })
      if (my === seq) mediaUrl.value = url
    } else {
      const r = await props.client.getObjectText(props.bucket, props.objectKey, { versionId: props.versionId, maxBytes: PREVIEW_TEXT_MAX_BYTES })
      if (my !== seq) return
      text.value = r.text
      truncated.value = r.truncated
    }
  } catch (e) {
    if (my === seq) error.value = e
  } finally {
    if (my === seq) loading.value = false
  }
}

watch(() => [props.bucket, props.objectKey, props.versionId, force.value], () => void load(), { immediate: true })
</script>

<template>
  <div class="space-y-3">
    <CommonEmptyState
      v-if="kind === 'none'"
      compact
      icon="i-lucide-eye-off"
      title="No preview available"
      :description="`Objects of type ${contentType || 'unknown'} cannot be previewed. Download the object to open it.`"
    />

    <CommonEmptyState
      v-else-if="tooLarge"
      compact
      icon="i-lucide-file-warning"
      title="Large file"
      :description="`This ${kind} is ${formatBytes(size)}. Loading the preview downloads the whole file.`"
    >
      <UButton size="sm" color="neutral" variant="outline" label="Load preview anyway" @click="force = true" />
    </CommonEmptyState>

    <div v-else-if="loading" class="space-y-2" aria-busy="true" aria-label="Loading preview">
      <USkeleton class="h-48 w-full" />
    </div>

    <CommonErrorAlert v-else-if="error" :error="error" title="Could not load the preview" retryable @retry="load" />

    <template v-else>
      <template v-if="kind === 'image' && mediaUrl">
        <div class="flex justify-center rounded-lg border border-default bg-[repeating-conic-gradient(var(--ui-bg-elevated)_0%_25%,transparent_0%_50%)] bg-size-[16px_16px] p-2">
          <img
            v-if="!mediaFailed"
            :src="mediaUrl"
            :alt="`Preview of ${baseName(objectKey)}`"
            class="max-h-[60vh] max-w-full object-contain"
            @error="mediaFailed = true"
          >
          <p v-else class="p-6 text-sm text-muted">
            The image could not be displayed (unsupported format or access denied).
          </p>
        </div>
      </template>
      <video v-else-if="kind === 'video' && mediaUrl" :src="mediaUrl" controls preload="metadata" class="max-h-[60vh] w-full rounded-lg bg-black">
        Your browser cannot play this video.
      </video>
      <audio v-else-if="kind === 'audio' && mediaUrl" :src="mediaUrl" controls preload="metadata" class="w-full">
        Your browser cannot play this audio file.
      </audio>

      <template v-else-if="kind === 'markdown' || kind === 'json' || kind === 'text'">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <p class="text-xs text-muted">
            <template v-if="truncated">
              Showing the first {{ formatBytes(PREVIEW_TEXT_MAX_BYTES) }} of {{ formatBytes(size) }}.
            </template>
            <template v-else-if="kind === 'json' && !json.valid">
              Not valid JSON; showing the raw text.
            </template>
            <template v-else-if="kind === 'markdown' && rendered === 'rendered' && markdownHtml === null">
              This Markdown could not be rendered; showing the raw text.
            </template>
          </p>
          <UTabs
            v-if="kind === 'markdown'"
            v-model="rendered"
            :items="[{ label: 'Rendered', value: 'rendered' }, { label: 'Source', value: 'source' }]"
            :content="false"
            size="xs"
            variant="link"
          />
        </div>
        <!-- renderMarkdown escapes all input and only emits a fixed tag set. -->
        <!-- eslint-disable vue/no-v-html -->
        <article
          v-if="kind === 'markdown' && rendered === 'rendered' && markdownHtml !== null"
          class="browser-markdown max-h-[60vh] overflow-auto rounded-lg border border-default p-4 text-sm"
          v-html="markdownHtml ?? ''"
        />
        <!-- eslint-enable vue/no-v-html -->
        <pre
          v-else
          class="max-h-[60vh] overflow-auto rounded-lg border border-default bg-elevated/50 p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-words"
          tabindex="0"
          :aria-label="`Contents of ${baseName(objectKey)}`"
        >{{ kind === 'json' ? json.text : text }}</pre>
        <p v-if="!text && !truncated" class="text-sm text-muted">
          The object is empty.
        </p>
      </template>
    </template>
  </div>
</template>

<style scoped>
.browser-markdown :deep(h1) { font-size: 1.5rem; font-weight: 600; margin: 0.75rem 0 0.5rem; }
.browser-markdown :deep(h2) { font-size: 1.25rem; font-weight: 600; margin: 0.75rem 0 0.5rem; }
.browser-markdown :deep(h3),
.browser-markdown :deep(h4),
.browser-markdown :deep(h5),
.browser-markdown :deep(h6) { font-size: 1rem; font-weight: 600; margin: 0.75rem 0 0.25rem; }
.browser-markdown :deep(p) { margin: 0.5rem 0; }
.browser-markdown :deep(ul) { list-style: disc; padding-left: 1.5rem; margin: 0.5rem 0; }
.browser-markdown :deep(ol) { list-style: decimal; padding-left: 1.5rem; margin: 0.5rem 0; }
.browser-markdown :deep(a) { color: var(--ui-primary); text-decoration: underline; }
.browser-markdown :deep(code) { font-family: var(--font-mono, ui-monospace, monospace); font-size: 0.85em; background: var(--ui-bg-elevated); padding: 0.1em 0.3em; border-radius: 0.25rem; }
.browser-markdown :deep(pre) { background: var(--ui-bg-elevated); padding: 0.75rem; border-radius: 0.5rem; overflow: auto; margin: 0.5rem 0; }
.browser-markdown :deep(pre code) { background: transparent; padding: 0; }
.browser-markdown :deep(blockquote) { border-left: 3px solid var(--ui-border-accented); padding-left: 0.75rem; color: var(--ui-text-muted); margin: 0.5rem 0; }
.browser-markdown :deep(hr) { border-color: var(--ui-border); margin: 1rem 0; }
/* Wide tables scroll on their own instead of stretching the preview. */
.browser-markdown :deep(table) { display: block; width: max-content; max-width: 100%; overflow-x: auto; border-collapse: collapse; margin: 0.5rem 0; }
.browser-markdown :deep(th),
.browser-markdown :deep(td) { border: 1px solid var(--ui-border); padding: 0.375rem 0.75rem; text-align: left; vertical-align: top; }
.browser-markdown :deep(th) { font-weight: 600; background: var(--ui-bg-elevated); }
.browser-markdown :deep(tbody tr:nth-child(even)) { background: color-mix(in oklab, var(--ui-bg-elevated) 50%, transparent); }
</style>
