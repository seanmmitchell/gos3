<script setup lang="ts">
/**
 * Object browser.
 *
 *   …/objects/            bucket root
 *   …/objects/a/b/        folder "a/b/" (trailing slash)
 *   …/objects/a/b/c.txt   folder "a/b/" with the details panel of "a/b/c.txt"
 *                         (?versionId=… for a specific version)
 *
 * The page instance is kept while navigating inside one bucket (page key =
 * server + bucket), so listings, selection and toggles survive opening and
 * closing the details panel.
 */
import type { BreadcrumbItem, DropdownMenuItem } from '@nuxt/ui'
import type { BrowserEntry, BrowserRowAction, BrowserSort } from '~/composables/useBrowserPaths'
import type { CollectResult, DroppedFile } from '~/composables/useBrowserDrop'
import type { OpTarget, TransferDone } from '~/composables/useBrowserOps'
import { S3Error } from '~/utils/s3/errors'

definePageMeta({
  key: route => `${String(route.params.id)}::${String(route.params.bucket)}`
})

const PAGE_SIZE = 1000
/** Stop auto-loading on scroll beyond this many rows (the button still works). */
const AUTO_LOAD_LIMIT = 20000

const route = useRoute()
const router = useRouter()
const toast = useToast()
const confirm = useConfirm()
const uploads = useUploads()
const { connection, s3, requireS3 } = useConnection()

const serverId = computed(() => routeParam(route.params.id))
const bucket = computed(() => routeParam(route.params.bucket))
const target = computed(() => parseBrowserPath(route.path, route.query))
const prefix = computed(() => target.value.prefix)
const openKey = computed(() => (target.value.kind === 'object' ? target.value.key : ''))
const versionIdParam = computed(() => (typeof route.query.versionId === 'string' && route.query.versionId ? route.query.versionId : undefined))

useHead({
  title: () => (openKey.value ? baseName(openKey.value) : prefix.value ? `${bucket.value}/${prefix.value}` : bucket.value)
})

useBreadcrumbs(() => {
  const items: BreadcrumbItem[] = [
    { label: 'Servers', icon: 'i-lucide-server', to: appRoutes.servers() },
    { label: connection.value?.name ?? serverId.value, to: appRoutes.server(serverId.value) },
    { label: 'Buckets', to: appRoutes.buckets(serverId.value) }
  ]
  for (const c of browserCrumbs(serverId.value, bucket.value, prefix.value)) items.push({ label: c.label, to: c.to })
  if (openKey.value) items.push({ label: baseName(openKey.value) })
  return items
})

// ---------------------------------------------------------------------------
// Bucket versioning (for delete wording and the versions toggle)
// ---------------------------------------------------------------------------

/** 'Enabled' | 'Suspended' | '' (never enabled) | null (unknown / not allowed). */
const versioning = ref<string | null>(null)
watch([s3, bucket], async ([client, b]) => {
  versioning.value = null
  if (!client) return
  try {
    const v = await client.getBucketVersioning(b)
    if (b === bucket.value) versioning.value = v.status
  } catch {
    versioning.value = null
  }
}, { immediate: true })
const versioned = computed(() => versioning.value === 'Enabled' || versioning.value === 'Suspended')

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

const showVersions = ref(false)
// Search text is cleared when another folder is opened (useBrowserSearch).
const { search, mode: searchMode, serverQuery, listPrefix } = useBrowserSearch(prefix)

const entries = shallowRef<BrowserEntry[]>([])
const listing = reactive({
  loading: false,
  loadingMore: false,
  error: null as unknown,
  truncated: false,
  token: undefined as string | undefined,
  keyMarker: undefined as string | undefined,
  versionIdMarker: undefined as string | undefined,
  loaded: false
})
let listAbort: AbortController | null = null

async function loadPage(reset: boolean): Promise<void> {
  const client = s3.value
  if (!client) return
  if (reset) {
    listAbort?.abort()
    listing.loading = true
    listing.loadingMore = false
    listing.error = null
  } else {
    if (!listing.truncated || listing.loading || listing.loadingMore) return
    listing.loadingMore = true
  }
  const ctrl = new AbortController()
  listAbort = ctrl
  const b = bucket.value
  const p = listPrefix.value
  const base = prefix.value
  try {
    let rows: BrowserEntry[]
    if (showVersions.value) {
      const page = await client.listObjectVersions(b, {
        prefix: p || undefined,
        delimiter: '/',
        maxKeys: PAGE_SIZE,
        keyMarker: reset ? undefined : listing.keyMarker,
        versionIdMarker: reset ? undefined : listing.versionIdMarker
      }, ctrl.signal)
      rows = toBrowserVersionEntries(page.commonPrefixes, page.versions, base)
      listing.truncated = page.isTruncated && !!(page.nextKeyMarker || page.nextVersionIdMarker)
      listing.keyMarker = page.nextKeyMarker
      listing.versionIdMarker = page.nextVersionIdMarker
    } else {
      const page = await client.listObjectsV2(b, {
        prefix: p || undefined,
        delimiter: '/',
        maxKeys: PAGE_SIZE,
        continuationToken: reset ? undefined : listing.token
      }, ctrl.signal)
      rows = toBrowserEntries(page.commonPrefixes, page.objects, base)
      listing.truncated = page.isTruncated && !!page.nextContinuationToken
      listing.token = page.nextContinuationToken
    }
    if (ctrl.signal.aborted) return
    entries.value = reset ? rows : mergeBrowserEntries(entries.value, rows)
    listing.loaded = true
  } catch (e) {
    if (ctrl.signal.aborted || isAbortedError(e)) return
    if (reset) {
      listing.error = e
      entries.value = []
      listing.truncated = false
    } else {
      toast.add({ title: 'Could not load more items', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
    }
  } finally {
    if (listAbort === ctrl) {
      listing.loading = false
      listing.loadingMore = false
      listAbort = null
    }
  }
}

function refresh() {
  return loadPage(true)
}
function loadMore() {
  return loadPage(false)
}

// Declared before the listing watcher (which clears it); the getter is lazy.
const selection = useBrowserSelection(() => visible.value.map(e => e.id))

watch([s3, bucket, listPrefix, showVersions], () => {
  selection.clear()
  void refresh()
}, { immediate: true })

onBeforeUnmount(() => {
  listAbort?.abort()
})

// Sorting + client-side filter.
const sort = ref<BrowserSort>({ by: 'name', desc: false })
const visible = computed(() => sortBrowserEntries(filterBrowserEntries(entries.value, searchMode.value === 'filter' ? search.value : ''), sort.value))
const entryById = computed(() => new Map(entries.value.map(e => [e.id, e])))

const counts = computed(() => {
  let folders = 0
  let objects = 0
  let bytes = 0
  for (const e of entries.value) {
    if (e.type === 'folder') folders++
    else {
      objects++
      bytes += e.size
    }
  }
  return { folders, objects, bytes }
})

// Infinite scroll: load the next page while the sentinel is visible.
const sentinel = ref<HTMLElement | null>(null)
const sentinelVisible = ref(false)
let observer: IntersectionObserver | undefined
onMounted(() => {
  if (typeof IntersectionObserver === 'undefined') return
  observer = new IntersectionObserver(es => (sentinelVisible.value = es.some(e => e.isIntersecting)), { rootMargin: '400px' })
  watch(sentinel, (el, old) => {
    if (old) observer?.unobserve(old)
    if (el) observer?.observe(el)
  }, { immediate: true })
})
onBeforeUnmount(() => observer?.disconnect())
watch(() => [sentinelVisible.value, listing.truncated, listing.loading, listing.loadingMore], () => {
  if (sentinelVisible.value && listing.truncated && !listing.loading && !listing.loadingMore && !listing.error && entries.value.length < AUTO_LOAD_LIMIT) {
    void loadMore()
  }
})

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

watch(entries, list => selection.prune(list.map(e => e.id)))

const selectedEntries = computed(() => selection.ids.value.map(id => entryById.value.get(id)).filter((e): e is BrowserEntry => !!e))

function targetOf(e: BrowserEntry): OpTarget {
  return { key: e.key, isFolder: e.type === 'folder', versionId: e.type === 'object' ? e.versionId : undefined }
}

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

function linkFor(e: BrowserEntry): string {
  if (e.type === 'folder') return browserRoute(serverId.value, bucket.value, e.key)
  return browserObjectRoute(serverId.value, bucket.value, e.key, entryVersionParam(e))
}

const parentLink = computed(() => (prefix.value ? browserRoute(serverId.value, bucket.value, parentPrefix(prefix.value)) : appRoutes.buckets(serverId.value)))
const crumbs = computed<BreadcrumbItem[]>(() => browserCrumbs(serverId.value, bucket.value, prefix.value).map((c, i, all) => ({
  label: c.label,
  icon: i === 0 ? 'i-lucide-database' : undefined,
  to: i === all.length - 1 ? undefined : c.to
})))

// ---------------------------------------------------------------------------
// Uploads
// ---------------------------------------------------------------------------

const fileInput = ref<HTMLInputElement | null>(null)
const folderInput = ref<HTMLInputElement | null>(null)
onMounted(() => {
  if (folderInput.value) folderInput.value.webkitdirectory = true
})

const uploadMenu = computed<DropdownMenuItem[]>(() => [
  { label: 'Upload files', icon: 'i-lucide-file-up', onSelect: () => fileInput.value?.click() },
  { label: 'Upload folder', icon: 'i-lucide-folder-up', onSelect: () => folderInput.value?.click() }
])

async function enqueueFiles(files: DroppedFile[]) {
  if (!files.length) return
  const client = s3.value
  if (!client) return
  const base = prefix.value
  const items: { file: File, key: string }[] = []
  const invalid: string[] = []
  for (const f of files) {
    const key = uploadKey(base, f.relativePath)
    if (validateObjectKey(key)) invalid.push(f.relativePath)
    else items.push({ file: f.file, key })
  }
  if (invalid.length) {
    toast.add({ title: `${invalid.length} file${invalid.length === 1 ? '' : 's'} skipped`, description: `Invalid object key: ${invalid.slice(0, 3).join(', ')}${invalid.length > 3 ? '…' : ''}`, color: 'warning', icon: 'i-lucide-triangle-alert' })
  }
  if (!items.length) return
  const target = { connectionId: serverId.value, bucket: bucket.value }
  // Ask before replacing existing objects. The check asks the server: the
  // listed rows only cover loaded pages of this level (not subfolders of a
  // folder upload, nor files beyond the first pages).
  let found: { existing: string[], complete: boolean }
  try {
    found = await findExistingKeys(client, target.bucket, items.map(i => i.key))
  } catch {
    found = { existing: [], complete: false }
  }
  if (found.existing.length || !found.complete) {
    const n = found.existing.length
    const consequence = versioned.value
      ? 'The bucket is versioned: the current versions are kept as previous versions.'
      : 'Objects with the same name are replaced.'
    const ok = await confirm({
      title: found.complete
        ? `Overwrite ${n} existing object${n === 1 ? '' : 's'}?`
        : n ? `Overwrite ${n} or more existing objects?` : 'Overwrite existing objects?',
      description: found.complete
        ? consequence
        : `Not every destination could be checked (too many objects, or no permission to read them), so existing objects may be overwritten. ${consequence}`,
      confirmLabel: 'Upload and overwrite',
      color: 'warning'
    })
    if (!ok) return
  }
  uploads.enqueue({ client, ...target, files: items })
}

function onPicked(e: Event) {
  const input = e.target as HTMLInputElement
  const files = filesFromInput(input.files)
  input.value = ''
  void enqueueFiles(files)
}

function onDropped(r: CollectResult) {
  if (r.errors.length) {
    toast.add({ title: 'Some dropped items could not be read', description: r.errors.slice(0, 3).join('; '), color: 'warning', icon: 'i-lucide-triangle-alert' })
  }
  if (r.truncated) {
    toast.add({ title: 'Too many files', description: `Only the first ${r.files.length} files are uploaded.`, color: 'warning', icon: 'i-lucide-triangle-alert' })
  }
  void enqueueFiles(r.files)
}

// Refresh the listing when uploads into this folder finish (debounced, max 5 s apart).
let uploadRefreshTimer: ReturnType<typeof setTimeout> | undefined
let uploadRefreshFirst = 0
const offUploads = uploads.onSettled((item) => {
  if (item.status !== 'done' || item.connectionId !== serverId.value || item.bucket !== bucket.value || !item.key.startsWith(prefix.value)) return
  const now = Date.now()
  if (!uploadRefreshFirst) uploadRefreshFirst = now
  clearTimeout(uploadRefreshTimer)
  const wait = now - uploadRefreshFirst > 5000 ? 0 : 800
  uploadRefreshTimer = setTimeout(() => {
    uploadRefreshFirst = 0
    void refresh()
  }, wait)
})
onBeforeUnmount(() => {
  offUploads()
  clearTimeout(uploadRefreshTimer)
})

// ---------------------------------------------------------------------------
// Dialog state
// ---------------------------------------------------------------------------

const folderOpen = ref(false)
const shareOpen = ref(false)
const shareTarget = ref<{ key: string, versionId?: string }>({ key: '' })
const transferOpen = ref(false)
const transferMode = ref<'copy' | 'move' | 'rename'>('copy')
const transferTargets = ref<OpTarget[]>([])

function openTransfer(mode: 'copy' | 'move' | 'rename', targets: OpTarget[]) {
  if (!targets.length) return
  transferMode.value = mode
  transferTargets.value = targets
  transferOpen.value = true
}

async function onTransferDone(info: TransferDone) {
  selection.clear()
  await refresh()
  if (info.mode === 'copy' || !openKey.value) return
  const moved = info.plan.find(p => p.src.key === openKey.value)
  if (moved && !info.result.failures.some(f => f.key === moved.src.key)) {
    await router.replace(browserObjectRoute(serverId.value, info.dstBucket, moved.dstKey))
  }
}

function onFolderCreated() {
  void refresh()
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

const busy = ref<{ label: string, done: number, total?: number } | null>(null)
let busyAbort: AbortController | null = null

async function deleteFlow(targets: OpTarget[]) {
  if (!targets.length) return
  const client = s3.value
  if (!client) return
  const folders = targets.filter(t => t.isFolder).length
  const objects = targets.length - folders
  const purge = showVersions.value
  const permanent = purge || targets.some(t => t.versionId)
  const parts: string[] = []
  if (objects) parts.push(`${objects} ${purge || targets.some(t => t.versionId) ? 'version' : 'object'}${objects === 1 ? '' : 's'}`)
  if (folders) parts.push(`${folders} folder${folders === 1 ? '' : 's'} (with everything inside)`)
  const single = targets.length === 1 ? targets[0]! : null
  let description: string
  if (permanent) description = 'The selected versions are deleted permanently. This cannot be undone.'
  else if (versioned.value) description = 'The bucket is versioned: delete markers are created and previous versions are kept (use “Show versions” to restore them).'
  else description = 'Deleted objects cannot be recovered.'
  if (purge && folders) description += ' Folders are purged including every version and delete marker.'
  const ok = await confirm({
    title: single ? `Delete ${single.isFolder ? 'folder' : single.versionId ? 'version of' : ''} ${relativeName(single.key, prefix.value)}?`.replace(/\s+/g, ' ') : `Delete ${parts.join(' and ')}?`,
    description,
    confirmLabel: permanent ? 'Delete permanently' : 'Delete',
    requireText: folders || targets.length > 20 || (permanent && targets.length > 1) ? 'delete' : undefined
  })
  if (!ok) return
  busyAbort = new AbortController()
  busy.value = { label: 'Deleting', done: 0 }
  try {
    const r = await deleteTargets(client, bucket.value, targets, {
      allVersions: purge,
      signal: busyAbort.signal,
      onProgress: p => (busy.value = { label: 'Deleting', done: p.done, total: p.total })
    })
    if (r.failures.length) {
      toast.add({
        title: `${r.failures.length} item${r.failures.length === 1 ? '' : 's'} could not be deleted`,
        description: r.failures.slice(0, 3).map(f => `${f.key}: ${f.message}`).join('; '),
        color: 'error',
        icon: 'i-lucide-circle-alert'
      })
    } else {
      toast.add({ title: `Deleted ${r.done} item${r.done === 1 ? '' : 's'}`, color: 'success', icon: 'i-lucide-circle-check' })
    }
    // Close the details panel when its object (or version) was deleted.
    if (openKey.value && targets.some(t => (t.isFolder ? openKey.value.startsWith(t.key) : t.key === openKey.value && (!t.versionId || t.versionId === versionIdParam.value)))) {
      await router.push(browserRoute(serverId.value, bucket.value, prefix.value))
    }
  } catch (e) {
    if (!isAbortedError(e)) toast.add({ title: 'Delete failed', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
  } finally {
    busy.value = null
    busyAbort = null
    selection.clear()
    void refresh()
  }
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

const MAX_BULK_DOWNLOAD = 500

async function downloadFlow(targets: OpTarget[]) {
  const client = s3.value
  if (!client || !targets.length) return
  busyAbort = new AbortController()
  const signal = busyAbort.signal
  busy.value = { label: 'Preparing download', done: 0 }
  try {
    const items: { key: string, versionId?: string }[] = []
    for (const t of targets) {
      if (t.isFolder) {
        for (const k of await listKeysUnder(client, bucket.value, t.key, signal)) if (!k.endsWith('/')) items.push({ key: k })
      } else {
        const e = entryById.value.get(entryId(t.key, t.versionId))
        if (!(e?.type === 'object' && e.isDeleteMarker)) items.push({ key: t.key, versionId: t.versionId })
      }
      if (items.length > MAX_BULK_DOWNLOAD) break
    }
    busy.value = null
    if (!items.length) {
      toast.add({ title: 'Nothing to download', color: 'neutral', icon: 'i-lucide-info' })
      return
    }
    if (items.length > MAX_BULK_DOWNLOAD) {
      toast.add({ title: 'Too many files', description: `Bulk downloads are limited to ${MAX_BULK_DOWNLOAD} files. Use an S3 client (e.g. aws s3 sync) for large folders.`, color: 'warning', icon: 'i-lucide-triangle-alert' })
      return
    }
    if (items.length > 1) {
      const ok = await confirm({
        title: `Download ${items.length} files?`,
        description: 'The files are downloaded one after another. Your browser may ask you to allow multiple downloads.',
        confirmLabel: 'Download',
        color: 'primary',
        icon: 'i-lucide-download'
      })
      if (!ok) return
    }
    busy.value = { label: 'Downloading', done: 0, total: items.length }
    const r = await browserDownloadAll(items, signal)
    if (r.failures.length) {
      toast.add({ title: `${r.failures.length} download${r.failures.length === 1 ? '' : 's'} failed`, description: r.failures[0]!.message, color: 'error', icon: 'i-lucide-circle-alert' })
    }
  } catch (e) {
    if (!isAbortedError(e)) toast.add({ title: 'Download failed', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
  } finally {
    busy.value = null
    busyAbort = null
  }
}

function browserDownloadAll(items: { key: string, versionId?: string }[], signal: AbortSignal) {
  return downloadSequentially(requireS3(), bucket.value, items, {
    signal,
    delayMs: items.length > 1 ? 700 : 0,
    onProgress: p => (busy.value = { label: 'Downloading', done: p.done, total: p.total })
  })
}

function stopBusy() {
  busyAbort?.abort()
}

// ---------------------------------------------------------------------------
// Row / details actions
// ---------------------------------------------------------------------------

async function onAction(action: BrowserRowAction, e: BrowserEntry) {
  const t = targetOf(e)
  switch (action) {
    case 'open':
      await router.push(linkFor(e))
      break
    case 'details':
      await router.push(browserObjectRoute(serverId.value, bucket.value, e.key, e.type === 'object' ? entryVersionParam(e) : undefined))
      break
    case 'download':
      await downloadFlow([t])
      break
    case 'share':
      shareTarget.value = { key: e.key, versionId: t.versionId }
      shareOpen.value = true
      break
    case 'copy':
    case 'move':
    case 'rename':
      openTransfer(action, [t])
      break
    case 'delete':
      await deleteFlow([t])
      break
    case 'restore':
      if (!t.versionId) return
      try {
        const r = await restoreVersion(requireS3(), bucket.value, e.key, t.versionId)
        if (r.aclReset) toast.add({ title: 'Version restored, ACL reset', description: `${e.key} now has this content as its latest version. The version had custom ACL grants, which cannot be carried over: it is private now.`, color: 'warning', icon: 'i-lucide-shield-alert' })
        else toast.add({ title: 'Version restored', description: `${e.key} now has this content as its latest version.`, color: 'success', icon: 'i-lucide-circle-check' })
        void refresh()
      } catch (err) {
        toast.add({ title: 'Could not restore the version', description: errorMessage(err), color: 'error', icon: 'i-lucide-circle-alert' })
      }
      break
  }
}

const bulkTargets = computed(() => selectedEntries.value.map(targetOf))
const bulkHasVersions = computed(() => bulkTargets.value.some(t => t.versionId))

// ---------------------------------------------------------------------------
// Details panel (driven by the route)
// ---------------------------------------------------------------------------

// Keep the last key while the slideover animates closed.
const detailsKey = ref(openKey.value)
const detailsVersion = ref(versionIdParam.value)
watch([openKey, versionIdParam], ([k, v]) => {
  if (k) {
    detailsKey.value = k
    detailsVersion.value = v
  }
})
const detailsOpen = computed({
  get: () => !!openKey.value,
  set: (v: boolean) => {
    if (!v && openKey.value) void router.push(browserRoute(serverId.value, bucket.value, prefix.value))
  }
})

function selectVersion(v: string | undefined) {
  void router.replace(browserObjectRoute(serverId.value, bucket.value, detailsKey.value, v))
}

function onFolderKey(p: string) {
  void router.replace(browserRoute(serverId.value, bucket.value, p))
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

const searchInput = ref<{ inputRef?: HTMLInputElement | null } | null>(null)
defineShortcuts({
  '/': () => searchInput.value?.inputRef?.focus()
})

const listError = computed(() => listing.error)
const errorKind = computed(() => {
  const e = listing.error
  if (!(e instanceof S3Error)) return 'other'
  if (e.code === 'NoSuchBucket' || (e.status === 404 && !e.code.startsWith('NoSuchKey'))) return 'nobucket'
  if (e.status === 403) return 'denied'
  return 'other'
})

const emptyFolder = computed(() => listing.loaded && !listing.loading && !listing.error && !entries.value.length)
</script>

<template>
  <div>
    <BrowserDropZone :disabled="!s3" :target-label="`${bucket}/${prefix}`" @files="onDropped">
      <CommonPageHeader :title="bucket" icon="i-lucide-database">
        <template #badge>
          <UBadge v-if="versioning === 'Enabled'" color="success" variant="subtle" size="sm" label="Versioned" icon="i-lucide-history" />
          <UBadge v-else-if="versioning === 'Suspended'" color="warning" variant="subtle" size="sm" label="Versioning suspended" />
        </template>
        <nav aria-label="Folder path" class="mt-1 flex min-w-0 items-center gap-1">
          <UButton
            :to="parentLink"
            icon="i-lucide-corner-left-up"
            color="neutral"
            variant="ghost"
            size="xs"
            :aria-label="prefix ? 'Up one folder' : 'Back to buckets'"
          />
          <UBreadcrumb :items="crumbs" class="min-w-0" :ui="{ list: 'flex-wrap', link: 'text-xs font-mono' }" />
          <CommonCopyButton v-if="prefix" :value="prefix" aria-label="Copy folder path" />
        </nav>
        <template #actions>
          <UButton
            icon="i-lucide-refresh-cw"
            color="neutral"
            variant="outline"
            aria-label="Refresh listing"
            :loading="listing.loading"
            @click="refresh()"
          />
          <UButton
            :to="appRoutes.bucketSettings(serverId, bucket)"
            icon="i-lucide-settings-2"
            color="neutral"
            variant="outline"
            label="Settings"
            class="hidden sm:inline-flex"
          />
          <UButton
            icon="i-lucide-folder-plus"
            color="neutral"
            variant="outline"
            label="New folder"
            :disabled="!s3"
            @click="folderOpen = true"
          />
          <UFieldGroup>
            <UButton icon="i-lucide-upload" label="Upload" :disabled="!s3" @click="fileInput?.click()" />
            <UDropdownMenu :items="uploadMenu" :content="{ align: 'end' }">
              <UButton icon="i-lucide-chevron-down" :disabled="!s3" aria-label="More upload options" />
            </UDropdownMenu>
          </UFieldGroup>
        </template>
      </CommonPageHeader>

      <input ref="fileInput" type="file" multiple class="hidden" aria-hidden="true" tabindex="-1" @change="onPicked">
      <input ref="folderInput" type="file" multiple class="hidden" aria-hidden="true" tabindex="-1" @change="onPicked">

      <!-- Toolbar -->
      <div class="mb-3 flex flex-col gap-2 lg:flex-row lg:items-center">
        <div class="flex min-w-0 flex-1 items-center gap-2">
          <UInput
            ref="searchInput"
            v-model="search"
            :icon="searchMode === 'prefix' ? 'i-lucide-text-search' : 'i-lucide-search'"
            :placeholder="searchMode === 'prefix' ? 'Search keys starting with…' : 'Filter loaded items…'"
            :aria-label="searchMode === 'prefix' ? 'Search object keys by prefix on the server' : 'Filter listed items by name'"
            class="min-w-0 flex-1 sm:max-w-sm"
            :ui="{ trailing: 'pe-1' }"
          >
            <template #trailing>
              <UButton v-if="search" color="neutral" variant="link" size="sm" icon="i-lucide-x" aria-label="Clear search" @click="search = ''" />
              <UKbd v-else value="/" class="hidden sm:inline-flex" />
            </template>
          </UInput>
          <USelect
            v-model="searchMode"
            :items="[{ label: 'Filter', value: 'filter' }, { label: 'Prefix search', value: 'prefix' }]"
            aria-label="Search mode"
            class="w-36 shrink-0"
          />
        </div>
        <div class="flex items-center gap-3">
          <USwitch
            v-model="showVersions"
            label="Show versions"
            size="sm"
            :description="versioning === '' ? 'Bucket is not versioned' : undefined"
          />
        </div>
      </div>

      <!-- Selection / progress bar -->
      <div
        v-if="selection.count.value > 0 || busy"
        class="sticky top-0 z-10 mb-3 flex flex-wrap items-center gap-2 rounded-lg border border-primary/30 bg-primary/5 px-3 py-2 backdrop-blur"
        role="region"
        aria-label="Selection actions"
      >
        <template v-if="busy">
          <UIcon name="i-lucide-loader-circle" class="size-4 animate-spin text-primary" aria-hidden="true" />
          <span class="text-sm" aria-live="polite">
            {{ busy.label }}<template v-if="busy.total"> {{ busy.done }} / {{ busy.total }}</template><template v-else-if="busy.done"> ({{ busy.done }})</template>…
          </span>
          <UButton size="xs" color="neutral" variant="outline" label="Stop" icon="i-lucide-circle-stop" class="ml-auto" @click="stopBusy" />
        </template>
        <template v-else>
          <span class="text-sm font-medium" aria-live="polite">{{ selection.count.value }} selected</span>
          <UButton size="xs" color="neutral" variant="ghost" label="Clear" @click="selection.clear()" />
          <div class="ml-auto flex flex-wrap items-center gap-1">
            <UButton size="xs" color="neutral" variant="outline" icon="i-lucide-download" label="Download" @click="downloadFlow(bulkTargets)" />
            <UButton size="xs" color="neutral" variant="outline" icon="i-lucide-copy" label="Copy to…" @click="openTransfer('copy', bulkTargets)" />
            <UButton
              v-if="!bulkHasVersions"
              size="xs"
              color="neutral"
              variant="outline"
              icon="i-lucide-folder-input"
              label="Move to…"
              @click="openTransfer('move', bulkTargets)"
            />
            <UButton size="xs" color="error" variant="soft" icon="i-lucide-trash-2" :label="showVersions ? 'Delete permanently' : 'Delete'" @click="deleteFlow(bulkTargets)" />
          </div>
        </template>
      </div>

      <!-- Listing -->
      <div v-if="listError" class="space-y-3">
        <CommonErrorAlert
          :error="listError"
          :title="errorKind === 'nobucket' ? `Bucket ${bucket} does not exist` : errorKind === 'denied' ? 'You are not allowed to list this folder' : 'Could not list objects'"
          retryable
          @retry="refresh()"
        />
        <p v-if="errorKind === 'denied'" class="text-sm text-muted">
          Your identity may still be allowed to read or upload individual objects. You can upload files or open an object by its key from a direct link.
        </p>
        <UButton v-if="errorKind === 'nobucket'" :to="appRoutes.buckets(serverId)" icon="i-lucide-arrow-left" label="Back to buckets" color="neutral" variant="outline" />
      </div>

      <div v-else-if="listing.loading && !entries.length" class="space-y-2" aria-busy="true" aria-label="Loading objects">
        <USkeleton v-for="i in 8" :key="i" class="h-10 w-full" />
      </div>

      <CommonEmptyState
        v-else-if="emptyFolder"
        :icon="serverQuery ? 'i-lucide-search-x' : 'i-lucide-folder-open'"
        :title="serverQuery ? 'No keys match this prefix' : prefix ? 'This folder is empty' : 'This bucket is empty'"
        :description="serverQuery ? `Nothing starts with “${prefix}${serverQuery}”.` : 'Drag and drop files or folders here, or use the buttons below.'"
      >
        <template v-if="!serverQuery">
          <UButton icon="i-lucide-upload" label="Upload files" @click="fileInput?.click()" />
          <UButton icon="i-lucide-folder-up" label="Upload folder" color="neutral" variant="outline" @click="folderInput?.click()" />
          <UButton icon="i-lucide-folder-plus" label="New folder" color="neutral" variant="outline" @click="folderOpen = true" />
        </template>
        <UButton v-else color="neutral" variant="outline" label="Clear search" @click="search = ''" />
      </CommonEmptyState>

      <CommonEmptyState
        v-else-if="listing.loaded && !visible.length"
        compact
        icon="i-lucide-search-x"
        title="No loaded items match the filter"
        :description="listing.truncated ? 'More items exist on the server: load more, or switch to prefix search.' : `No name contains “${search}”.`"
      >
        <UButton color="neutral" variant="outline" label="Clear filter" @click="search = ''" />
        <UButton v-if="listing.truncated" color="neutral" variant="outline" label="Search on server" @click="searchMode = 'prefix'" />
      </CommonEmptyState>

      <template v-else-if="entries.length">
        <BrowserObjectTable
          v-model:sort="sort"
          :entries="visible"
          :selection="selection"
          :link-for="linkFor"
          :versions-mode="showVersions"
          :loading="listing.loading"
          @action="onAction"
        />
        <div class="mt-3 flex flex-wrap items-center justify-between gap-2 text-xs text-muted">
          <span aria-live="polite">
            {{ formatNumber(counts.folders) }} folder{{ counts.folders === 1 ? '' : 's' }},
            {{ formatNumber(counts.objects) }} {{ showVersions ? 'version' : 'object' }}{{ counts.objects === 1 ? '' : 's' }}
            ({{ formatBytes(counts.bytes) }})<template v-if="listing.truncated"> loaded · more available</template>
            <template v-if="visible.length !== entries.length"> · {{ formatNumber(visible.length) }} shown</template>
          </span>
          <UButton
            v-if="listing.truncated"
            size="sm"
            color="neutral"
            variant="outline"
            icon="i-lucide-chevrons-down"
            label="Load more"
            :loading="listing.loadingMore"
            @click="loadMore()"
          />
        </div>
        <div ref="sentinel" class="h-px" aria-hidden="true" />
      </template>
    </BrowserDropZone>

    <BrowserUploadPanel />

    <BrowserObjectDetails
      v-model:open="detailsOpen"
      :client="s3"
      :bucket="bucket"
      :object-key="detailsKey"
      :version-id="detailsVersion"
      :versioning="versioning"
      @action="onAction"
      @select-version="selectVersion"
      @changed="refresh()"
      @folder="onFolderKey"
    />

    <BrowserCreateFolderModal v-model:open="folderOpen" :client="s3" :bucket="bucket" :prefix="prefix" @created="onFolderCreated" />

    <BrowserShareModal
      v-model:open="shareOpen"
      :client="s3"
      :bucket="bucket"
      :object-key="shareTarget.key"
      :version-id="shareTarget.versionId"
    />

    <BrowserTransferModal
      v-model:open="transferOpen"
      :client="s3"
      :bucket="bucket"
      :targets="transferTargets"
      :mode="transferMode"
      :from-prefix="prefix"
      @done="onTransferDone"
    />
  </div>
</template>
