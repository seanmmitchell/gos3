<script setup lang="ts">
/**
 * Copy / move / rename objects and folders (recursively) with progress.
 * S3 has no rename: objects are copied (CopyObject) and, for move/rename,
 * the sources are deleted afterwards (keeping their ACL). Existing
 * destination objects are only replaced after confirmation.
 */
import type { OpFailure, OpTarget, TransferDone, TransferPlanItem } from '~/composables/useBrowserOps'
import type { S3Client } from '~/utils/s3/client'

const open = defineModel<boolean>('open', { required: true })

const props = defineProps<{
  client: S3Client | undefined
  bucket: string
  targets: OpTarget[]
  mode: 'copy' | 'move' | 'rename'
  /** Folder the targets are listed in (their relative paths are kept). */
  fromPrefix: string
}>()

const emit = defineEmits<{ done: [info: TransferDone] }>()

const single = computed(() => (props.targets.length === 1 ? props.targets[0]! : null))
const isRename = computed(() => props.mode === 'rename' && !!single.value)

const newName = ref('')
const dstBucket = ref('')
const dstPrefix = ref('')
const bucketNames = ref<string[] | null>(null)
const phase = ref<'form' | 'planning' | 'confirm' | 'running' | 'finished'>('form')
const progress = ref({ done: 0, total: 0, current: '' })
const failures = ref<OpFailure[]>([])
const warnings = ref<OpFailure[]>([])
/** Planned transfer waiting for the overwrite confirmation. */
const pending = ref<{ plan: TransferPlanItem[], target: string, existing: string[], complete: boolean } | null>(null)
const error = ref<unknown>(null)
let controller: AbortController | null = null

const title = computed(() => {
  const what = single.value ? `“${relativeName(single.value.key, props.fromPrefix)}”` : `${props.targets.length} items`
  if (props.mode === 'rename') return `Rename ${what}`
  return `${props.mode === 'copy' ? 'Copy' : 'Move'} ${what}`
})

watch(open, async (v) => {
  if (!v) {
    controller?.abort()
    return
  }
  phase.value = 'form'
  failures.value = []
  warnings.value = []
  pending.value = null
  error.value = null
  progress.value = { done: 0, total: 0, current: '' }
  newName.value = single.value ? relativeName(single.value.key, props.fromPrefix) : ''
  dstBucket.value = props.bucket
  dstPrefix.value = props.fromPrefix
  if (props.mode !== 'rename' && bucketNames.value === null && props.client) {
    try {
      bucketNames.value = (await props.client.listBuckets()).buckets.map(b => b.name)
    } catch {
      bucketNames.value = [] // not allowed to list: free-text input
    }
  }
}, { immediate: true })

const normalizedPrefix = computed(() => {
  const p = dstPrefix.value.trim().replace(/^\/+/, '')
  return p && !p.endsWith('/') ? `${p}/` : p
})

const renameTo = computed(() => {
  const n = newName.value.trim().replace(/^\/+/, '')
  if (!n || !single.value) return ''
  const base = `${props.fromPrefix}${n}`
  return single.value.isFolder ? base.replace(/\/*$/, '/') : base
})

const formError = computed<string | undefined>(() => {
  if (isRename.value) {
    if (!newName.value.trim()) return 'Enter a new name'
    if (renameTo.value === single.value!.key) return 'The new name is the same as the current one'
    // Renaming a folder into its own subfolder would overwrite objects before they are copied.
    if (single.value!.isFolder && renameTo.value.startsWith(single.value!.key)) return `Cannot rename “${single.value!.key}” into itself`
    return validateObjectKey(renameTo.value)
  }
  if (!dstBucket.value.trim()) return 'Choose a destination bucket'
  const sameBucket = dstBucket.value.trim() === props.bucket
  if (sameBucket && normalizedPrefix.value === props.fromPrefix && !props.targets.some(t => t.versionId)) {
    return 'Choose a different destination folder'
  }
  if (sameBucket) {
    const inside = props.targets.find(t => t.isFolder && normalizedPrefix.value.startsWith(t.key))
    if (inside) return `Cannot ${props.mode === 'copy' ? 'copy' : 'move'} “${inside.key}” into itself`
  }
  if (normalizedPrefix.value && validateObjectKey(normalizedPrefix.value)) return `Invalid destination folder: ${validateObjectKey(normalizedPrefix.value)}`
  return undefined
})

const bucketItems = computed(() => (bucketNames.value ?? []).map(n => ({ label: n, value: n })))

async function run() {
  if (formError.value || !props.client) return
  const client = props.client
  controller = new AbortController()
  const signal = controller.signal
  error.value = null
  failures.value = []
  warnings.value = []
  phase.value = 'planning'
  const target = isRename.value ? props.bucket : dstBucket.value.trim()
  try {
    const plan = await planTransfer(client, props.bucket, props.targets, {
      fromPrefix: props.fromPrefix,
      toPrefix: normalizedPrefix.value,
      renameTo: isRename.value ? renameTo.value : undefined
    }, signal)
    if (!plan.length) {
      error.value = new Error('Nothing to transfer: the selected folders are empty.')
      phase.value = 'form'
      return
    }
    const overlap = findTransferOverlap(plan, target)
    if (overlap) {
      error.value = new Error(`“${overlap.dstKey}” would be overwritten before it is copied: the destination is inside the source. Choose another destination.`)
      phase.value = 'form'
      return
    }
    // Ask before replacing existing objects (as uploads do).
    const others = plan.filter(p => !(p.src.bucket === target && p.src.key === p.dstKey))
    const found = await findExistingDestinations(client, target, others, { signal })
    if (found.existing.length) {
      pending.value = { plan, target, existing: found.existing, complete: found.complete }
      phase.value = 'confirm'
      return
    }
    await execute(client, plan, target, signal)
  } catch (e) {
    if (isAbortedError(e) || signal.aborted) {
      phase.value = 'finished'
      return
    }
    error.value = e
    phase.value = 'form'
  } finally {
    if (phase.value !== 'confirm') controller = null
  }
}

/** Runs the transfer after the overwrite confirmation. */
async function confirmOverwrite() {
  const p = pending.value
  if (!p || !props.client) return
  pending.value = null
  const signal = (controller ??= new AbortController()).signal
  try {
    await execute(props.client, p.plan, p.target, signal)
  } catch (e) {
    if (isAbortedError(e) || signal.aborted) {
      phase.value = 'finished'
      return
    }
    error.value = e
    phase.value = 'form'
  } finally {
    controller = null
  }
}

function backToForm() {
  pending.value = null
  controller = null
  phase.value = 'form'
}

async function execute(client: S3Client, plan: TransferPlanItem[], target: string, signal: AbortSignal) {
  phase.value = 'running'
  progress.value = { done: 0, total: plan.length, current: '' }
  const result = await runTransfer(client, plan, target, {
    move: props.mode !== 'copy',
    signal,
    onProgress: p => (progress.value = { done: p.done, total: p.total ?? plan.length, current: p.current ?? '' })
  })
  failures.value = result.failures
  warnings.value = result.warnings ?? []
  phase.value = 'finished'
  emit('done', { mode: props.mode, dstBucket: target, plan, result })
  if (!result.failures.length && !warnings.value.length && !signal.aborted) {
    const verb = props.mode === 'copy' ? 'Copied' : props.mode === 'move' ? 'Moved' : 'Renamed'
    useToast().add({ title: `${verb} ${result.done} object${result.done === 1 ? '' : 's'}`, color: 'success', icon: 'i-lucide-circle-check' })
    open.value = false
  }
}

function cancel() {
  if (controller) controller.abort()
  else open.value = false
}

/** Overwrite consequences, as for uploads. */
const versionedHint = 'Existing objects are replaced. In versioned buckets the current versions are kept as previous versions; otherwise their content is lost.'

const percent = computed(() => (progress.value.total ? Math.round((progress.value.done / progress.value.total) * 100) : 0))
</script>

<template>
  <UModal v-model:open="open" :title="title" :dismissible="phase !== 'running' && phase !== 'planning'" :ui="{ footer: 'justify-end' }">
    <template #description>
      <template v-if="mode === 'copy'">
        Objects are copied server-side; folders are copied recursively.
      </template>
      <template v-else>
        Objects are copied to the new location and then deleted from the old one.
      </template>
    </template>
    <template #body>
      <form v-if="phase === 'form'" id="transfer-form" class="space-y-4" novalidate @submit.prevent="run">
        <template v-if="isRename">
          <UFormField
            label="New name"
            :error="newName ? formError : undefined"
            :help="renameTo ? `New key: ${renameTo}` : undefined"
            required
          >
            <UInput v-model="newName" class="w-full font-mono" autocomplete="off" autofocus spellcheck="false" />
          </UFormField>
          <p v-if="single?.isFolder" class="text-xs text-muted">
            Every object in the folder is copied and deleted; this can take a while for large folders.
          </p>
        </template>
        <template v-else>
          <UFormField label="Destination bucket" required>
            <USelect
              v-if="bucketItems.length"
              v-model="dstBucket"
              :items="bucketItems"
              class="w-full"
            />
            <UInput v-else v-model.trim="dstBucket" class="w-full font-mono" autocomplete="off" placeholder="bucket-name" />
          </UFormField>
          <UFormField
            label="Destination folder"
            :error="formError && dstBucket ? formError : undefined"
            :help="`Items keep their names: ${normalizedPrefix || '(bucket root)'}…`"
          >
            <UInput v-model="dstPrefix" class="w-full font-mono" placeholder="(bucket root)" autocomplete="off" spellcheck="false" />
          </UFormField>
        </template>
        <CommonErrorAlert v-if="error" :error="error" title="Transfer failed" />
      </form>

      <div v-else-if="phase === 'confirm' && pending" class="space-y-3">
        <UAlert
          color="warning"
          variant="subtle"
          icon="i-lucide-triangle-alert"
          :title="`${formatNumber(pending.existing.length)}${pending.complete ? '' : '+'} object${pending.existing.length === 1 ? '' : 's'} already exist${pending.existing.length === 1 ? 's' : ''} at the destination`"
          :description="versionedHint"
        />
        <ul class="max-h-48 space-y-1 overflow-y-auto font-mono text-xs" aria-label="Existing destination objects">
          <li v-for="k in pending.existing.slice(0, 100)" :key="k" class="truncate" :title="k">
            {{ k }}
          </li>
          <li v-if="pending.existing.length > 100" class="text-muted">
            …and {{ formatNumber(pending.existing.length - 100) }} more
          </li>
        </ul>
      </div>

      <div v-else class="space-y-3" aria-live="polite">
        <p class="text-sm">
          <template v-if="phase === 'planning'">
            Listing objects…
          </template>
          <template v-else>
            {{ formatNumber(progress.done) }} of {{ formatNumber(progress.total) }} objects
          </template>
        </p>
        <UProgress :model-value="phase === 'planning' ? null : percent" :aria-label="`Progress ${percent}%`" />
        <p v-if="phase === 'running' && progress.current" class="truncate font-mono text-xs text-muted" :title="progress.current">
          {{ progress.current }}
        </p>
        <div v-if="phase === 'finished' && failures.length" class="space-y-2">
          <UAlert
            color="warning"
            variant="subtle"
            icon="i-lucide-triangle-alert"
            :title="`${failures.length} object${failures.length === 1 ? '' : 's'} could not be transferred`"
          />
          <ul class="max-h-48 space-y-1 overflow-y-auto text-xs">
            <li v-for="f in failures.slice(0, 100)" :key="`${f.key}:${f.versionId ?? ''}`">
              <span class="font-mono">{{ f.key }}</span> — <span class="text-error">{{ f.message }}</span>
            </li>
          </ul>
        </div>
        <div v-if="phase === 'finished' && warnings.length" class="space-y-2">
          <UAlert
            color="warning"
            variant="subtle"
            icon="i-lucide-shield-alert"
            :title="`${warnings.length} object${warnings.length === 1 ? '' : 's'} transferred with warnings`"
          />
          <ul class="max-h-48 space-y-1 overflow-y-auto text-xs">
            <li v-for="w in warnings.slice(0, 100)" :key="`${w.key}:${w.versionId ?? ''}`">
              <span class="font-mono">{{ w.key }}</span> — <span class="text-warning">{{ w.message }}</span>
            </li>
          </ul>
        </div>
        <p v-else-if="phase === 'finished' && !failures.length" class="text-sm text-muted">
          Canceled after {{ progress.done }} object{{ progress.done === 1 ? '' : 's' }}.
        </p>
      </div>
    </template>
    <template #footer>
      <template v-if="phase === 'form'">
        <UButton color="neutral" variant="ghost" label="Cancel" @click="open = false" />
        <UButton
          type="submit"
          form="transfer-form"
          :icon="mode === 'copy' ? 'i-lucide-copy' : mode === 'move' ? 'i-lucide-folder-input' : 'i-lucide-pencil'"
          :label="mode === 'copy' ? 'Copy' : mode === 'move' ? 'Move' : 'Rename'"
          :disabled="!!formError || !client"
        />
      </template>
      <template v-else-if="phase === 'confirm'">
        <UButton color="neutral" variant="ghost" label="Back" @click="backToForm" />
        <UButton color="warning" icon="i-lucide-replace" :label="mode === 'copy' ? 'Copy and overwrite' : mode === 'move' ? 'Move and overwrite' : 'Rename and overwrite'" @click="confirmOverwrite" />
      </template>
      <UButton v-else-if="phase !== 'finished'" color="neutral" variant="outline" label="Stop" icon="i-lucide-circle-stop" @click="cancel" />
      <UButton v-else label="Close" @click="open = false" />
    </template>
  </UModal>
</template>
