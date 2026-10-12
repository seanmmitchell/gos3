<script setup lang="ts">
/**
 * Floating upload queue panel (bottom-right): overall progress, per-file
 * progress / speed / errors, cancel, retry and clear. Backed by useUploads().
 *
 * The panel floats over the page, so it renders an invisible spacer of its
 * own height at the end of the page content: the last rows of a listing can
 * always be scrolled above it instead of being covered (their row actions
 * would be unreachable). The file list collapses by itself once every upload
 * has finished without errors, and expands again when new uploads start.
 */
import type { UploadItem } from '~/composables/useUploads'

const uploads = useUploads()
const stats = uploads.stats
const expanded = ref(true)
const listId = useId()

const visible = computed(() => uploads.items.length > 0)

// Auto-collapse when a batch finishes cleanly; re-expand for a new batch.
watch(() => stats.value.pending, (pending, before) => {
  if (pending > 0 && !before) expanded.value = true
  else if (pending === 0 && before && !stats.value.failed) expanded.value = false
})

// Height of the floating panel, mirrored by the spacer.
const panel = ref<HTMLElement | null>(null)
const panelHeight = ref(0)
let resizeObserver: ResizeObserver | undefined
watch(panel, (el) => {
  resizeObserver?.disconnect()
  resizeObserver = undefined
  panelHeight.value = el?.offsetHeight ?? 0
  if (el && typeof ResizeObserver !== 'undefined') {
    resizeObserver = new ResizeObserver(() => (panelHeight.value = el.offsetHeight))
    resizeObserver.observe(el)
  }
})
onBeforeUnmount(() => resizeObserver?.disconnect())
const percent = computed(() => (stats.value.total ? Math.round((stats.value.loaded / stats.value.total) * 100) : 0))

const title = computed(() => {
  const s = stats.value
  if (s.pending) return `Uploading ${s.pending} file${s.pending === 1 ? '' : 's'}`
  if (s.failed) return `${s.failed} upload${s.failed === 1 ? '' : 's'} failed`
  return `${s.done} upload${s.done === 1 ? '' : 's'} complete`
})

// The clear button first clears finished uploads and keeps failed ones for a
// retry; once only failed uploads are left it dismisses those, so the panel
// can always be closed.
const hasFinished = computed(() => uploads.items.some(it => it.status === 'done' || it.status === 'canceled'))
function clearList() {
  if (hasFinished.value) uploads.clearFinished()
  else uploads.clearSettled()
}

const eta = computed(() => uploadEta(stats.value.total - stats.value.loaded, stats.value.speed))

// Newest first, but keep running/queued items at the top.
const ORDER: Record<UploadItem['status'], number> = { uploading: 0, queued: 1, error: 2, canceled: 3, done: 4 }
const sorted = computed(() => uploads.items.slice().sort((a, b) => ORDER[a.status] - ORDER[b.status] || b.id - a.id))
const MAX_ROWS = 200
const shown = computed(() => sorted.value.slice(0, MAX_ROWS))

function itemPercent(it: UploadItem): number {
  if (it.status === 'done') return 100
  return it.size ? Math.round((it.loaded / it.size) * 100) : 0
}

function itemColor(it: UploadItem): 'primary' | 'success' | 'error' | 'neutral' {
  if (it.status === 'done') return 'success'
  if (it.status === 'error') return 'error'
  if (it.status === 'canceled') return 'neutral'
  return 'primary'
}

const STATUS_LABEL: Record<UploadItem['status'], string> = {
  queued: 'Queued',
  uploading: 'Uploading',
  done: 'Done',
  error: 'Failed',
  canceled: 'Canceled'
}

// Warn before leaving the page while uploads are running.
function beforeUnload(e: BeforeUnloadEvent) {
  if (uploads.stats.value.pending > 0) {
    e.preventDefault()
    e.returnValue = ''
  }
}
onMounted(() => window.addEventListener('beforeunload', beforeUnload))
onBeforeUnmount(() => window.removeEventListener('beforeunload', beforeUnload))
</script>

<template>
  <div v-if="visible" class="shrink-0" aria-hidden="true" data-testid="upload-panel-spacer" :style="{ height: `${panelHeight + 16}px` }" />
  <section
    v-if="visible"
    ref="panel"
    class="fixed inset-x-2 bottom-2 z-40 flex max-h-[70vh] flex-col overflow-hidden rounded-xl border border-default bg-default shadow-xl sm:inset-x-auto sm:right-4 sm:bottom-4 sm:w-[26rem]"
    aria-label="Uploads"
  >
    <header class="flex items-center gap-2 border-b border-default px-3 py-2">
      <UIcon
        :name="stats.pending ? 'i-lucide-loader-circle' : stats.failed ? 'i-lucide-circle-alert' : 'i-lucide-circle-check'"
        class="size-4 shrink-0"
        :class="stats.pending ? 'animate-spin text-primary' : stats.failed ? 'text-error' : 'text-success'"
        aria-hidden="true"
      />
      <div class="min-w-0 flex-1">
        <p class="truncate text-sm font-medium text-highlighted" aria-live="polite">
          {{ title }}
        </p>
        <p class="truncate text-xs text-muted">
          {{ formatBytes(stats.loaded) }} / {{ formatBytes(stats.total) }}
          <template v-if="stats.pending && stats.speed > 0">
            · {{ formatBytes(stats.speed) }}/s<template v-if="eta">
              · {{ eta }} left
            </template>
          </template>
        </p>
      </div>
      <UButton
        v-if="stats.failed"
        size="xs"
        color="neutral"
        variant="ghost"
        icon="i-lucide-rotate-cw"
        label="Retry failed"
        @click="uploads.retryFailed()"
      />
      <UButton
        v-if="stats.pending"
        size="xs"
        color="neutral"
        variant="ghost"
        icon="i-lucide-circle-stop"
        aria-label="Cancel all uploads"
        @click="uploads.cancelAll()"
      />
      <UButton
        v-else
        size="xs"
        color="neutral"
        variant="ghost"
        icon="i-lucide-x"
        :aria-label="hasFinished ? 'Clear finished uploads' : 'Dismiss failed uploads'"
        @click="clearList"
      />
      <UButton
        size="xs"
        color="neutral"
        variant="ghost"
        :icon="expanded ? 'i-lucide-chevron-down' : 'i-lucide-chevron-up'"
        :aria-label="expanded ? 'Collapse upload list' : 'Expand upload list'"
        :aria-expanded="expanded"
        :aria-controls="listId"
        @click="expanded = !expanded"
      />
    </header>
    <UProgress v-if="stats.pending" :model-value="percent" size="xs" :ui="{ base: 'rounded-none' }" :aria-label="`Overall upload progress ${percent}%`" />

    <ul v-show="expanded" :id="listId" class="min-h-0 flex-1 divide-y divide-default overflow-y-auto">
      <li v-for="it in shown" :key="it.id" class="px-3 py-2">
        <div class="flex items-start gap-2">
          <UIcon
            :name="it.status === 'done' ? 'i-lucide-file-check' : it.status === 'error' ? 'i-lucide-file-x' : 'i-lucide-file-up'"
            class="mt-0.5 size-4 shrink-0"
            :class="it.status === 'done' ? 'text-success' : it.status === 'error' ? 'text-error' : 'text-muted'"
            aria-hidden="true"
          />
          <div class="min-w-0 flex-1">
            <p class="truncate text-sm text-highlighted" :title="`${it.bucket}/${it.key}`">
              {{ it.name }}
            </p>
            <p class="truncate text-xs text-muted" :title="it.key">
              <span class="font-mono">{{ it.bucket }}/{{ it.key }}</span>
            </p>
            <div v-if="it.status === 'uploading' || it.status === 'queued'" class="mt-1.5 flex items-center gap-2">
              <UProgress
                :model-value="it.status === 'queued' ? 0 : itemPercent(it)"
                size="xs"
                :color="itemColor(it)"
                class="flex-1"
                :aria-label="`${it.name}: ${itemPercent(it)}%`"
              />
              <span class="w-24 shrink-0 text-right text-xs tabular-nums text-muted">
                <template v-if="it.status === 'queued'">Queued</template>
                <template v-else>{{ itemPercent(it) }}%<template v-if="it.speed > 0"> · {{ formatBytes(it.speed) }}/s</template></template>
              </span>
            </div>
            <p v-else class="mt-0.5 text-xs" :class="it.status === 'error' ? 'text-error' : 'text-muted'">
              <template v-if="it.status === 'error'">
                {{ it.error }}
              </template>
              <template v-else-if="it.status === 'done'">
                {{ formatBytes(it.size) }}<template v-if="it.multipart"> · multipart</template><template v-if="it.speed > 0"> · {{ formatBytes(it.speed) }}/s</template>
              </template>
              <template v-else>
                {{ STATUS_LABEL[it.status] }}
              </template>
            </p>
          </div>
          <div class="flex shrink-0 items-center">
            <UButton
              v-if="it.status === 'uploading' || it.status === 'queued'"
              size="xs"
              color="neutral"
              variant="ghost"
              icon="i-lucide-x"
              :aria-label="`Cancel upload of ${it.name}`"
              @click="uploads.cancel(it.id)"
            />
            <template v-else>
              <UButton
                v-if="it.status === 'error' || it.status === 'canceled'"
                size="xs"
                color="neutral"
                variant="ghost"
                icon="i-lucide-rotate-cw"
                :aria-label="`Retry upload of ${it.name}`"
                @click="uploads.retry(it.id)"
              />
              <UButton
                size="xs"
                color="neutral"
                variant="ghost"
                icon="i-lucide-trash"
                :aria-label="`Remove ${it.name} from the list`"
                @click="uploads.remove(it.id)"
              />
            </template>
          </div>
        </div>
      </li>
      <li v-if="sorted.length > MAX_ROWS" class="px-3 py-2 text-xs text-muted">
        … and {{ sorted.length - MAX_ROWS }} more
      </li>
    </ul>
  </section>
</template>
