<script setup lang="ts">
/**
 * Object details slideover: HeadObject information, preview, metadata and
 * tag editors, versions, and actions (download, share, rename, copy, move,
 * delete). Actions that need page-level dialogs are emitted.
 */
import type { DropdownMenuItem, TabsItem } from '@nuxt/ui'
import type { BrowserObjectEntry, BrowserRowAction } from '~/composables/useBrowserPaths'
import type { HeadResult } from '~/types/s3'
import { UNADDRESSABLE_KEY, type S3Client } from '~/utils/s3/client'
import { S3Error } from '~/utils/s3/errors'

const open = defineModel<boolean>('open', { required: true })

const props = defineProps<{
  client: S3Client | undefined
  bucket: string
  objectKey: string
  versionId?: string
  /** Bucket versioning status ('' = never enabled, null = unknown). */
  versioning?: string | null
  readOnly?: boolean
}>()

const emit = defineEmits<{
  action: [action: BrowserRowAction, entry: BrowserObjectEntry]
  /** Show another version (undefined = latest). */
  selectVersion: [versionId: string | undefined]
  /** The object changed (metadata saved, version restored/deleted): refresh listings. */
  changed: []
  /** The key turned out to be a folder. */
  folder: [prefix: string]
}>()

const toast = useToast()
const head = ref<HeadResult | null>(null)
const loading = ref(false)
const error = ref<unknown>(null)
const isDeleteMarker = ref(false)
const tab = ref('overview')

const name = computed(() => baseName(props.objectKey))
/**
 * The key has '.' / '..' segments: it cannot be read through a URL, but it
 * can still be copied, moved or deleted (key in a header / request body).
 */
const unaddressable = computed(() => error.value instanceof S3Error && error.value.code === UNADDRESSABLE_KEY)
const versioned = computed(() => props.versioning === 'Enabled' || props.versioning === 'Suspended')

let seq = 0
async function load() {
  const my = ++seq
  if (!props.client || !props.objectKey) return
  loading.value = true
  error.value = null
  isDeleteMarker.value = false
  head.value = null
  try {
    const h = await props.client.headObject(props.bucket, props.objectKey, { versionId: props.versionId })
    if (my === seq) head.value = h
  } catch (e) {
    if (my !== seq) return
    if (e instanceof S3Error && e.status === 405 && props.versionId) {
      isDeleteMarker.value = true
    } else if (e instanceof S3Error && e.status === 404 && !props.versionId) {
      // Maybe a folder addressed without its trailing slash.
      try {
        const page = await props.client.listObjectsV2(props.bucket, { prefix: `${props.objectKey}/`, maxKeys: 1 })
        if (my === seq && page.keyCount > 0) {
          emit('folder', `${props.objectKey}/`)
          return
        }
      } catch {
        // ignore — show the original error
      }
      if (my === seq) error.value = e
    } else {
      error.value = e
    }
  } finally {
    if (my === seq) loading.value = false
  }
}

watch(() => [open.value, props.bucket, props.objectKey, props.versionId], () => {
  if (open.value) void load()
}, { immediate: true })

// Back to the overview when another object is opened.
watch(() => props.objectKey, () => (tab.value = 'overview'))

const entry = computed<BrowserObjectEntry>(() => ({
  type: 'object',
  id: entryId(props.objectKey, props.versionId),
  key: props.objectKey,
  name: name.value,
  size: head.value?.contentLength ?? 0,
  lastModified: head.value?.lastModified,
  storageClass: head.value?.storageClass,
  etag: head.value?.etag,
  versionId: props.versionId,
  isLatest: !props.versionId,
  isDeleteMarker: isDeleteMarker.value
}))

const tabs = computed<TabsItem[]>(() => [
  { label: 'Overview', value: 'overview', icon: 'i-lucide-info' },
  { label: 'Preview', value: 'preview', icon: 'i-lucide-eye', disabled: !head.value },
  { label: 'Metadata', value: 'metadata', icon: 'i-lucide-list', disabled: !head.value },
  { label: 'Tags', value: 'tags', icon: 'i-lucide-tags', disabled: !head.value },
  { label: 'Versions', value: 'versions', icon: 'i-lucide-history' }
])

const moreMenu = computed<DropdownMenuItem[][]>(() => {
  const act = (a: BrowserRowAction) => () => emit('action', a, entry.value)
  const groups: DropdownMenuItem[][] = [[{ label: 'Copy to…', icon: 'i-lucide-copy', onSelect: act('copy') }]]
  if (!props.versionId) {
    groups[0]!.unshift({ label: 'Rename…', icon: 'i-lucide-pencil', onSelect: act('rename') })
    groups[0]!.push({ label: 'Move to…', icon: 'i-lucide-folder-input', onSelect: act('move') })
  } else {
    groups[0]!.unshift({ label: 'Restore this version', icon: 'i-lucide-history', onSelect: act('restore') })
  }
  groups.push([{ label: props.versionId ? 'Delete version permanently…' : 'Delete…', icon: 'i-lucide-trash-2', color: 'error', onSelect: act('delete') }])
  return groups
})

async function download() {
  if (!props.client) return
  try {
    browserTriggerDownload(await browserDownloadUrl(props.client, props.bucket, props.objectKey, props.versionId))
  } catch (e) {
    toast.add({ title: 'Download failed', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
  }
}

const s3Uri = computed(() => `s3://${props.bucket}/${props.objectKey}`)

interface Field {
  label: string
  value?: string
  mono?: boolean
  copy?: boolean
  title?: string
}

const fields = computed<Field[]>(() => {
  const h = head.value
  if (!h) return []
  const f: Field[] = [
    { label: 'Size', value: `${formatBytes(h.contentLength)} (${formatNumber(h.contentLength)} bytes)` },
    { label: 'Last modified', value: h.lastModified ? formatDate(h.lastModified, { dateStyle: 'medium', timeStyle: 'medium' }) : undefined, title: formatISO(h.lastModified) },
    { label: 'Content-Type', value: h.contentType, mono: true },
    { label: 'ETag', value: h.etag, mono: true, copy: true },
    { label: 'Version ID', value: h.versionId && h.versionId !== 'null' ? h.versionId : (versioned.value ? 'null' : undefined), mono: true, copy: !!h.versionId && h.versionId !== 'null' },
    { label: 'Storage class', value: h.storageClass ?? 'STANDARD' }
  ]
  if (h.partsCount) f.push({ label: 'Parts', value: `${h.partsCount} (multipart upload)` })
  for (const [alg, v] of Object.entries(h.checksums)) f.push({ label: `Checksum ${alg}${h.checksumType ? ` (${h.checksumType.toLowerCase().replace('_', ' ')})` : ''}`, value: v, mono: true, copy: true })
  if (h.serverSideEncryption) f.push({ label: 'Encryption', value: h.serverSideEncryption + (h.sseKmsKeyId ? ` · ${h.sseKmsKeyId}` : ''), mono: true })
  if (h.objectLockMode) f.push({ label: 'Retention', value: `${h.objectLockMode} until ${formatDate(h.objectLockRetainUntilDate)}` })
  if (h.objectLockLegalHold) f.push({ label: 'Legal hold', value: h.objectLockLegalHold })
  if (h.cacheControl) f.push({ label: 'Cache-Control', value: h.cacheControl, mono: true })
  if (h.contentDisposition) f.push({ label: 'Content-Disposition', value: h.contentDisposition, mono: true })
  if (h.contentEncoding) f.push({ label: 'Content-Encoding', value: h.contentEncoding, mono: true })
  if (h.contentLanguage) f.push({ label: 'Content-Language', value: h.contentLanguage })
  if (h.expires) f.push({ label: 'Expires', value: h.expires })
  if (h.tagCount !== undefined) f.push({ label: 'Tags', value: String(h.tagCount) })
  if (h.replicationStatus) f.push({ label: 'Replication', value: h.replicationStatus })
  if (h.websiteRedirectLocation) f.push({ label: 'Redirect', value: h.websiteRedirectLocation, mono: true })
  return f
})

const metaEntries = computed(() => Object.entries(head.value?.metadata ?? {}))

function onMetadataSaved() {
  void load()
  emit('changed')
}

function onVersionsChanged(info: { deletedVersionId?: string }) {
  emit('changed')
  if (info.deletedVersionId && info.deletedVersionId === props.versionId) emit('selectVersion', undefined)
  else void load()
}
</script>

<template>
  <USlideover
    v-model:open="open"
    :title="name || 'Object'"
    side="right"
    :ui="{ content: 'max-w-2xl w-full', body: 'p-0 sm:p-0' }"
  >
    <template #description>
      <span class="flex min-w-0 items-center gap-1">
        <span class="truncate font-mono text-xs" :title="objectKey">{{ bucket }}/{{ objectKey }}</span>
        <CommonCopyButton :value="s3Uri" aria-label="Copy S3 URI" />
      </span>
    </template>

    <template #body>
      <div class="flex flex-wrap items-center gap-2 border-b border-default px-4 py-3 sm:px-6">
        <UButton icon="i-lucide-download" label="Download" size="sm" :disabled="!head" @click="download" />
        <UButton
          icon="i-lucide-link"
          label="Share"
          size="sm"
          color="neutral"
          variant="outline"
          :disabled="!head"
          @click="emit('action', 'share', entry)"
        />
        <UDropdownMenu v-if="!readOnly" :items="moreMenu" :content="{ align: 'start' }">
          <UButton icon="i-lucide-ellipsis" label="More" size="sm" color="neutral" variant="outline" :disabled="!head && !isDeleteMarker && !unaddressable" />
        </UDropdownMenu>
        <UBadge v-if="versionId" color="warning" variant="subtle" class="ml-auto" :title="versionId">
          Viewing version {{ truncateMiddle(versionId, 14) }}
        </UBadge>
        <UButton
          v-if="versionId"
          size="xs"
          color="neutral"
          variant="link"
          label="Show latest"
          @click="emit('selectVersion', undefined)"
        />
      </div>

      <div class="px-4 py-4 sm:px-6">
        <UTabs v-model="tab" :items="tabs" :content="false" variant="link" size="sm" class="mb-4 overflow-x-auto" />

        <div v-if="tab === 'overview'">
          <div v-if="loading" class="space-y-2" aria-busy="true" aria-label="Loading object details">
            <USkeleton v-for="i in 6" :key="i" class="h-6 w-full" />
          </div>
          <UAlert
            v-else-if="isDeleteMarker"
            color="warning"
            variant="subtle"
            icon="i-lucide-file-x-2"
            title="This version is a delete marker"
            description="It hides the object. Remove the delete marker in the Versions tab to restore the previous version."
          />
          <CommonErrorAlert
            v-else-if="error"
            :error="error"
            :title="error instanceof S3Error && error.status === 404 ? 'Object not found' : 'Could not read the object'"
            retryable
            @retry="load"
          />
          <template v-else-if="head">
            <dl class="grid grid-cols-1 gap-x-4 gap-y-2 text-sm sm:grid-cols-[10rem_1fr]">
              <template v-for="f in fields" :key="f.label">
                <dt class="text-muted">
                  {{ f.label }}
                </dt>
                <dd class="flex min-w-0 items-start gap-1">
                  <span :class="[f.mono ? 'font-mono text-xs leading-5' : '', 'break-all']" :title="f.title">{{ f.value ?? '—' }}</span>
                  <CommonCopyButton v-if="f.copy && f.value" :value="f.value" :aria-label="`Copy ${f.label}`" />
                </dd>
              </template>
            </dl>
            <UAlert
              v-if="head.headersHidden"
              class="mt-4"
              color="neutral"
              variant="subtle"
              icon="i-lucide-eye-off"
              title="Some headers are hidden"
              description="The server's CORS configuration does not expose every response header to this page: user metadata, Content-Disposition and Content-Encoding may be missing here."
            />
            <div v-if="metaEntries.length" class="mt-5">
              <h3 class="mb-2 text-sm font-medium text-highlighted">
                User metadata
              </h3>
              <dl class="grid grid-cols-1 gap-x-4 gap-y-1 text-sm sm:grid-cols-[10rem_1fr]">
                <template v-for="[k, v] in metaEntries" :key="k">
                  <dt class="truncate font-mono text-xs leading-5 text-muted" :title="k">
                    {{ k }}
                  </dt>
                  <dd class="font-mono text-xs leading-5 break-all">
                    {{ v }}
                  </dd>
                </template>
              </dl>
            </div>
          </template>
        </div>

        <BrowserObjectPreview
          v-else-if="tab === 'preview' && head"
          :client="client"
          :bucket="bucket"
          :object-key="objectKey"
          :version-id="versionId"
          :size="head.contentLength"
          :content-type="head.contentType"
        />

        <div v-else-if="tab === 'metadata' && head">
          <UAlert
            v-if="versionId"
            class="mb-3"
            color="neutral"
            variant="subtle"
            icon="i-lucide-info"
            title="Old versions are read-only"
            description="Metadata can only be changed on the latest version."
          />
          <BrowserObjectMetadataEditor
            :client="client"
            :bucket="bucket"
            :object-key="objectKey"
            :head="head"
            :versioned="versioned"
            :read-only="readOnly || !!versionId"
            @saved="onMetadataSaved"
          />
        </div>

        <BrowserObjectTagsEditor
          v-else-if="tab === 'tags' && head"
          :client="client"
          :bucket="bucket"
          :object-key="objectKey"
          :version-id="versionId"
          :read-only="readOnly"
        />

        <BrowserObjectVersions
          v-else-if="tab === 'versions'"
          :client="client"
          :bucket="bucket"
          :object-key="objectKey"
          :current-version-id="versionId"
          :versioning="versioning"
          :read-only="readOnly"
          @select="emit('selectVersion', $event)"
          @changed="onVersionsChanged"
        />
      </div>
    </template>
  </USlideover>
</template>
