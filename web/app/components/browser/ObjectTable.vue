<script setup lang="ts">
/**
 * Folder/object (and version) listing with multi-select (shift-click ranges),
 * sortable columns and per-row action menus.
 */
import type { DropdownMenuItem, TableColumn } from '@nuxt/ui'
import type { BrowserEntry, BrowserRowAction, BrowserSort, BrowserSortKey } from '~/composables/useBrowserPaths'
import type { BrowserSelection } from '~/composables/useBrowserSelection'

const props = withDefaults(defineProps<{
  entries: BrowserEntry[]
  selection: BrowserSelection
  /** Link for a row (folder route or object details route). */
  linkFor: (entry: BrowserEntry) => string
  versionsMode?: boolean
  loading?: boolean
  /** Hide write actions (copy / move / delete …). */
  readOnly?: boolean
}>(), { versionsMode: false, loading: false, readOnly: false })

const sort = defineModel<BrowserSort>('sort', { required: true })

const emit = defineEmits<{ action: [action: BrowserRowAction, entry: BrowserEntry] }>()

const columns = computed<TableColumn<BrowserEntry>[]>(() => {
  const cols: TableColumn<BrowserEntry>[] = [
    { id: 'select', header: '', meta: { class: { th: 'w-0 pe-0', td: 'w-0 pe-0' } } },
    { id: 'name', header: 'Name' }
  ]
  if (props.versionsMode) cols.push({ id: 'version', header: 'Version' })
  cols.push(
    { id: 'size', header: 'Size', meta: { class: { th: 'text-right', td: 'text-right' } } },
    { id: 'modified', header: 'Last modified', meta: { class: { th: 'hidden md:table-cell', td: 'hidden md:table-cell' } } },
    { id: 'class', header: 'Storage class', meta: { class: { th: 'hidden lg:table-cell', td: 'hidden lg:table-cell' } } },
    { id: 'actions', header: '', meta: { class: { th: 'w-0', td: 'w-0 text-right' } } }
  )
  return cols
})

function setSort(by: BrowserSortKey) {
  sort.value = sort.value.by === by ? { by, desc: !sort.value.desc } : { by, desc: by !== 'name' }
}

function sortIcon(by: BrowserSortKey) {
  if (sort.value.by !== by) return 'i-lucide-arrow-up-down'
  return sort.value.desc ? 'i-lucide-arrow-down' : 'i-lucide-arrow-up'
}

function sortLabel(by: BrowserSortKey, label: string): string {
  if (sort.value.by !== by) return `${label}: not sorted, activate to sort`
  return `${label}: sorted ${sort.value.desc ? 'descending' : 'ascending'}, activate to reverse`
}

// Header checkbox (indeterminate when only some rows are selected).
const headerBox = ref<HTMLInputElement | null>(null)
watchEffect(() => {
  if (headerBox.value) headerBox.value.indeterminate = props.selection.coverage.value === 'some'
})

function onRowCheck(e: MouseEvent, id: string) {
  props.selection.toggle(id, e.shiftKey)
}

function onRowKey(e: KeyboardEvent, id: string) {
  // Space toggles natively (click event); keep shift+space ranges working.
  if (e.key === ' ' && e.shiftKey) {
    e.preventDefault()
    props.selection.toggle(id, true)
  }
}

function fileIcon(e: BrowserEntry): string {
  if (e.type === 'folder') return 'i-lucide-folder'
  if (e.isDeleteMarker) return 'i-lucide-file-x-2'
  const ext = extName(e.key)
  if (['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'svg', 'bmp', 'ico'].includes(ext)) return 'i-lucide-file-image'
  if (['mp4', 'webm', 'mov', 'mkv', 'avi', 'm4v'].includes(ext)) return 'i-lucide-file-video'
  if (['mp3', 'wav', 'ogg', 'flac', 'm4a', 'aac', 'opus'].includes(ext)) return 'i-lucide-file-audio'
  if (['zip', 'gz', 'tgz', 'tar', 'bz2', 'xz', 'zst', '7z', 'rar'].includes(ext)) return 'i-lucide-file-archive'
  if (['json', 'yaml', 'yml', 'toml', 'xml'].includes(ext)) return 'i-lucide-file-json'
  if (['js', 'ts', 'go', 'py', 'rs', 'java', 'c', 'cpp', 'h', 'sh', 'vue', 'html', 'css', 'rb', 'php'].includes(ext)) return 'i-lucide-file-code'
  if (['csv', 'tsv', 'xls', 'xlsx', 'ods'].includes(ext)) return 'i-lucide-file-spreadsheet'
  if (['txt', 'md', 'log', 'pdf', 'doc', 'docx', 'rtf', 'odt'].includes(ext)) return 'i-lucide-file-text'
  return 'i-lucide-file'
}

function menu(e: BrowserEntry): DropdownMenuItem[][] {
  const act = (a: BrowserRowAction) => () => emit('action', a, e)
  if (e.type === 'folder') {
    const groups: DropdownMenuItem[][] = [[
      { label: 'Open', icon: 'i-lucide-folder-open', onSelect: act('open') },
      { label: 'Download contents', icon: 'i-lucide-download', onSelect: act('download') }
    ]]
    if (!props.readOnly) {
      groups.push([
        { label: 'Rename…', icon: 'i-lucide-pencil', onSelect: act('rename') },
        { label: 'Copy to…', icon: 'i-lucide-copy', onSelect: act('copy') },
        { label: 'Move to…', icon: 'i-lucide-folder-input', onSelect: act('move') }
      ])
      groups.push([{ label: props.versionsMode ? 'Delete all versions…' : 'Delete…', icon: 'i-lucide-trash-2', color: 'error', onSelect: act('delete') }])
    }
    return groups
  }
  if (e.isDeleteMarker) {
    return props.readOnly
      ? [[{ label: 'Details', icon: 'i-lucide-panel-right-open', onSelect: act('details') }]]
      : [
          [{ label: 'Details', icon: 'i-lucide-panel-right-open', onSelect: act('details') }],
          [{ label: e.isLatest ? 'Remove delete marker (undelete)' : 'Delete marker permanently', icon: 'i-lucide-undo-2', color: e.isLatest ? 'primary' : 'error', onSelect: act('delete') }]
        ]
  }
  const groups: DropdownMenuItem[][] = [[
    { label: 'Details', icon: 'i-lucide-panel-right-open', onSelect: act('details') },
    { label: 'Download', icon: 'i-lucide-download', onSelect: act('download') },
    { label: 'Share link…', icon: 'i-lucide-link', onSelect: act('share') }
  ]]
  if (!props.readOnly) {
    const edit: DropdownMenuItem[] = []
    if (e.versionId && !e.isLatest) edit.push({ label: 'Restore this version', icon: 'i-lucide-history', onSelect: act('restore') })
    if (!e.versionId) edit.push({ label: 'Rename…', icon: 'i-lucide-pencil', onSelect: act('rename') })
    edit.push({ label: 'Copy to…', icon: 'i-lucide-copy', onSelect: act('copy') })
    if (!e.versionId) edit.push({ label: 'Move to…', icon: 'i-lucide-folder-input', onSelect: act('move') })
    groups.push(edit)
    groups.push([{ label: e.versionId ? 'Delete version permanently…' : 'Delete…', icon: 'i-lucide-trash-2', color: 'error', onSelect: act('delete') }])
  }
  return groups
}

function rowClass(row: { original: BrowserEntry }): string {
  const e = row.original
  const classes: string[] = []
  if (props.selection.isSelected(e.id)) classes.push('bg-primary/5')
  if (e.type === 'object' && e.versionId && !e.isLatest) classes.push('text-muted')
  return classes.join(' ')
}
</script>

<template>
  <div class="overflow-x-auto rounded-lg border border-default">
    <UTable
      :data="entries"
      :columns="columns"
      :loading="loading"
      :get-row-id="(row: BrowserEntry) => row.id"
      caption="Folders and objects"
      :meta="{ class: { tr: rowClass } }"
      :ui="{ caption: 'sr-only', td: 'py-2', th: 'py-2' }"
    >
      <template #select-header>
        <input
          ref="headerBox"
          type="checkbox"
          class="size-4 cursor-pointer rounded accent-(--ui-primary)"
          :checked="selection.coverage.value === 'all'"
          :disabled="!entries.length"
          aria-label="Select all listed items"
          @change="selection.toggleAll()"
        >
      </template>
      <template #name-header>
        <UButton color="neutral" variant="ghost" size="xs" class="-ms-2 text-sm font-semibold text-highlighted" label="Name" :trailing-icon="sortIcon('name')" :aria-label="sortLabel('name', 'Name')" @click="setSort('name')" />
      </template>
      <template #size-header>
        <UButton color="neutral" variant="ghost" size="xs" class="-me-2 text-sm font-semibold text-highlighted" label="Size" :trailing-icon="sortIcon('size')" :aria-label="sortLabel('size', 'Size')" @click="setSort('size')" />
      </template>
      <template #modified-header>
        <UButton color="neutral" variant="ghost" size="xs" class="-ms-2 text-sm font-semibold text-highlighted" label="Last modified" :trailing-icon="sortIcon('modified')" :aria-label="sortLabel('modified', 'Last modified')" @click="setSort('modified')" />
      </template>

      <template #select-cell="{ row }">
        <input
          type="checkbox"
          class="size-4 cursor-pointer rounded accent-(--ui-primary)"
          :checked="selection.isSelected(row.original.id)"
          :aria-label="`Select ${row.original.name}${row.original.type === 'object' && row.original.versionId ? ` version ${row.original.versionId}` : ''}`"
          @click="onRowCheck($event, row.original.id)"
          @keydown="onRowKey($event, row.original.id)"
        >
      </template>

      <template #name-cell="{ row }">
        <div class="flex min-w-0 max-w-[28rem] items-center gap-2 lg:max-w-[36rem]">
          <UIcon
            :name="fileIcon(row.original)"
            class="size-4 shrink-0"
            :class="row.original.type === 'folder' ? 'text-primary' : 'text-muted'"
            aria-hidden="true"
          />
          <NuxtLink
            :to="linkFor(row.original)"
            class="truncate hover:underline focus-visible:underline"
            :class="[
              row.original.type === 'folder' ? 'font-medium text-highlighted' : 'text-default',
              row.original.type === 'object' && row.original.isDeleteMarker ? 'line-through' : ''
            ]"
            :title="row.original.key"
          >
            {{ row.original.name }}<span v-if="row.original.type === 'folder'" class="text-muted">/</span>
          </NuxtLink>
          <template v-if="row.original.type === 'object' && row.original.versionId">
            <UBadge v-if="row.original.isLatest" color="primary" variant="subtle" size="sm" label="Latest" />
            <UBadge v-if="row.original.isDeleteMarker" color="error" variant="subtle" size="sm" label="Delete marker" />
          </template>
        </div>
      </template>

      <template #version-cell="{ row }">
        <span v-if="row.original.type === 'object' && row.original.versionId" class="font-mono text-xs" :title="row.original.versionId">
          {{ truncateMiddle(row.original.versionId, 14) }}
        </span>
      </template>

      <template #size-cell="{ row }">
        <span v-if="row.original.type === 'folder' || row.original.isDeleteMarker" class="text-muted">—</span>
        <CommonByteSize v-else :bytes="row.original.size" class="text-sm tabular-nums" />
      </template>

      <template #modified-cell="{ row }">
        <CommonRelativeTime v-if="row.original.type === 'object'" :value="row.original.lastModified" class="text-sm text-muted" />
      </template>

      <template #class-cell="{ row }">
        <span v-if="row.original.type === 'object' && row.original.storageClass" class="text-xs text-muted">{{ row.original.storageClass }}</span>
      </template>

      <template #actions-cell="{ row }">
        <UDropdownMenu :items="menu(row.original)" :content="{ align: 'end' }">
          <UButton
            icon="i-lucide-ellipsis-vertical"
            color="neutral"
            variant="ghost"
            size="sm"
            :aria-label="`Actions for ${row.original.name}`"
          />
        </UDropdownMenu>
      </template>
    </UTable>
  </div>
</template>
