<script setup lang="ts">
/**
 * Drag & drop target for files and folders. Shows an overlay while files are
 * dragged over it and emits the flattened file list (folders are walked
 * recursively, relative paths preserved).
 *
 *   <BrowserDropZone :target-label="prefix || bucket" @files="onFiles"> … </BrowserDropZone>
 */
import type { CollectResult } from '~/composables/useBrowserDrop'

const props = withDefaults(defineProps<{
  disabled?: boolean
  /** Shown in the overlay: "Drop to upload to …". */
  targetLabel?: string
}>(), { disabled: false, targetLabel: undefined })

const emit = defineEmits<{ files: [result: CollectResult] }>()

const active = ref(false)
const reading = ref(false)
// dragenter/dragleave fire for every child element; count the depth.
let depth = 0

function onDragEnter(e: DragEvent) {
  if (props.disabled || !dragHasFiles(e.dataTransfer)) return
  e.preventDefault()
  depth++
  active.value = true
}

function onDragOver(e: DragEvent) {
  if (props.disabled || !dragHasFiles(e.dataTransfer)) return
  e.preventDefault()
  if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy'
}

function onDragLeave(e: DragEvent) {
  if (props.disabled || !dragHasFiles(e.dataTransfer)) return
  depth = Math.max(0, depth - 1)
  if (depth === 0) active.value = false
}

async function onDrop(e: DragEvent) {
  if (props.disabled || !dragHasFiles(e.dataTransfer)) return
  e.preventDefault()
  depth = 0
  active.value = false
  // collectDroppedFiles reads the items synchronously before awaiting.
  const pending = collectDroppedFiles(e.dataTransfer)
  reading.value = true
  try {
    const result = await pending
    if (result.files.length || result.errors.length) emit('files', result)
  } finally {
    reading.value = false
  }
}
</script>

<template>
  <div
    class="relative"
    @dragenter="onDragEnter"
    @dragover="onDragOver"
    @dragleave="onDragLeave"
    @drop="onDrop"
  >
    <slot />
    <Transition
      enter-active-class="transition-opacity duration-150"
      leave-active-class="transition-opacity duration-150"
      enter-from-class="opacity-0"
      leave-to-class="opacity-0"
    >
      <div
        v-if="active || reading"
        class="pointer-events-none absolute inset-0 z-20 flex items-center justify-center rounded-xl border-2 border-dashed border-primary bg-primary/10 backdrop-blur-[1px]"
        role="status"
        aria-live="polite"
      >
        <div class="flex flex-col items-center gap-2 rounded-lg bg-default/90 px-6 py-4 text-center shadow-lg">
          <UIcon :name="reading ? 'i-lucide-loader-circle' : 'i-lucide-upload'" class="size-8 text-primary" :class="reading ? 'animate-spin' : ''" aria-hidden="true" />
          <p class="font-medium text-highlighted">
            {{ reading ? 'Reading dropped folders…' : 'Drop files or folders to upload' }}
          </p>
          <p v-if="targetLabel && !reading" class="max-w-xs truncate font-mono text-xs text-muted">
            to {{ targetLabel }}
          </p>
        </div>
      </div>
    </Transition>
  </div>
</template>
