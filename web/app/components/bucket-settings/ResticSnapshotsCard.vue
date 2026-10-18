<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'
import type { BucketSnapshots, Snapshot } from '~/types/admin'
import type { ResticAccess } from '~/components/admin/-capabilities'
import { AdminError } from '~/utils/admin/client'
import {
  RETENTION_FIELDS,
  RETENTION_PRESETS,
  SNAPSHOT_POLL_MS,
  defaultSnapshotForm,
  describeRetention,
  describeSchedule,
  describeTarget,
  isPartialSnapshot,
  presetRetention,
  retentionPreset,
  snapshotConfigFrom,
  snapshotFormDirty,
  snapshotFormFrom,
  sortSnapshots,
  validateSnapshotForm,
  type RetentionPresetName,
  type SnapshotForm
} from './-restic'
import RestoreSnapshotModal from './RestoreSnapshotModal.vue'

/**
 * Whole-bucket restic snapshots of a bucket (GET
 * /buckets/{name}/restic/snapshots): the schedule (interval, time of day,
 * consistency, prefix, retention with presets, target repository, prune),
 * its status (last and next run, last error, the running job), "Snapshot
 * now", and the snapshots with "Restore to bucket…" and "Forget" (not for
 * partial snapshots, which restore refuses). Editing, forgetting and
 * pruning need manageSnapshots, "Snapshot now" runSnapshots, restoring
 * restoreSnapshots and manageJobs (CreateCopyJob on the target). A new
 * schedule leaves the repository to the server unless one is chosen; the
 * saved schedule then shows the server's choice. Polls while a snapshot
 * job runs.
 */
const props = defineProps<{
  bucket: string
  access: ResticAccess
  /** The per-version archive is on (prune is then off by default: its lock stalls archive writes). */
  archiveOn?: boolean
  /** The bucket has no restic repository of its own (native buckets, "no repository"): snapshots go to a backing. */
  needsBacking?: boolean
  /** Connection ID, for links to the jobs page. */
  serverId?: string
}>()

const emit = defineEmits<{ changed: [] }>()

const { requireAdmin } = useConnection()
const { whoami } = useWhoAmI()
const confirm = useConfirm()
const overlay = useOverlay()
const toast = useToast()

const loader = useLoader(() => requireAdmin().getBucketSnapshots(props.bucket), { watch: [() => props.bucket] })
const data = computed<BucketSnapshots | undefined>(() => loader.data.value)
const config = computed(() => data.value?.config ?? null)
const status = computed(() => data.value?.status ?? null)
const snapshots = computed(() => sortSnapshots(data.value?.snapshots))
const unavailable = computed(() => {
  const e = loader.error.value
  if (!(e instanceof AdminError)) return undefined
  if (e.isAccessDenied) return 'denied'
  if (e.isNotFound) return 'unsupported'
  return undefined
})

// --- poll while a snapshot job runs -------------------------------------------------------

/** Set on unmount: a response arriving later must not arm a new poll. */
let disposed = false
let timer: ReturnType<typeof setTimeout> | undefined
function stopTimer() {
  if (timer) clearTimeout(timer)
  timer = undefined
}

/**
 * Arms the next poll while a snapshot job runs. The only place a timer is
 * set; it replaces any armed one, so a change of the running job never
 * starts a second chain.
 */
function schedule() {
  stopTimer()
  if (disposed || !status.value?.running) return
  timer = setTimeout(() => void poll(), SNAPSHOT_POLL_MS)
}

async function poll() {
  timer = undefined
  if (document.visibilityState !== 'hidden') {
    await loader.refresh()
    if (disposed) return
    // The job finished: the bucket's settings page may show new state.
    if (!status.value?.running) emit('changed')
  }
  schedule()
}

watch(() => status.value?.running, () => schedule(), { immediate: true })
onBeforeUnmount(() => {
  disposed = true
  stopTimer()
})

// --- schedule form ------------------------------------------------------------------------

function freshForm(): SnapshotForm {
  const def = whoami.value?.backings?.default ?? ''
  // A new schedule leaves the repository to the server; the backing field is prefilled in case one is chosen.
  return config.value ? snapshotFormFrom(config.value) : defaultSnapshotForm({ archiveOn: props.archiveOn, backing: def })
}
const form = ref<SnapshotForm>(freshForm())
const submitted = ref(false)
/** The form as last synced from the server: differing from it means the user is editing. */
const synced = ref(JSON.stringify(form.value))
function syncForm() {
  form.value = freshForm()
  synced.value = JSON.stringify(form.value)
}
watch([config, () => !!data.value, () => props.needsBacking, () => props.archiveOn, () => whoami.value?.backings?.default], () => {
  // Keep the form in step with the server unless the user is editing it.
  if (JSON.stringify(form.value) === synced.value) syncForm()
})
const dirty = computed(() => snapshotFormDirty(form.value, config.value))
const errors = computed(() => validateSnapshotForm(form.value))
const valid = computed(() => Object.values(errors.value).every(v => !v))

const preset = computed<RetentionPresetName>({
  get: () => retentionPreset(form.value.retention),
  set: (v) => {
    if (v !== 'custom') form.value.retention = presetRetention(v)
  }
})
const presetItems = [
  ...RETENTION_PRESETS.map(p => ({ value: p.value, label: p.label, description: p.description })),
  { value: 'custom' as const, label: 'Custom', description: 'Set the counts below.' }
]
const targetItems = computed(() => [
  // "Automatic" only for a new schedule: a saved one keeps its repository.
  ...(config.value
    ? []
    : [{
        value: 'auto',
        label: 'Automatic',
        description: props.needsBacking
          ? 'A repository on the default backing, since this bucket has none of its own.'
          : 'The bucket’s own repository (filesystem and wrapped buckets), else one on the default backing.'
      }]),
  {
    value: 'bucket',
    label: 'The bucket’s own repository',
    description: props.needsBacking ? 'Not available: this bucket has no restic repository of its own.' : 'Snapshots share data with the per-version archive (filesystem and wrapped buckets).',
    disabled: props.needsBacking && form.value.targetKind !== 'bucket'
  },
  { value: 'backing', label: 'A repository on a backing', description: 'Any bucket; the backing needs a system directory (filesystem) or a home with an encryption key (S3).' }
])
const backingItems = computed(() => (whoami.value?.backings?.available ?? []).map(b => ({ label: b.name, value: b.name })))

const save = useAsyncAction(
  () => requireAdmin().putSnapshotConfig(props.bucket, snapshotConfigFrom(form.value)),
  { successMessage: 'Snapshot schedule saved', errorTitle: 'Could not save the snapshot schedule' }
)
async function onSave() {
  submitted.value = true
  if (!valid.value || !dirty.value) return
  const saved = await save.run()
  if (!saved) return
  submitted.value = false
  if (loader.data.value) loader.data.value = { ...loader.data.value, config: saved }
  syncForm()
  emit('changed')
  void loader.refresh()
}

const remove = useAsyncAction(() => requireAdmin().deleteSnapshotConfig(props.bucket), {
  successMessage: 'Snapshot schedule removed',
  errorTitle: 'Could not remove the snapshot schedule'
})
async function onRemove() {
  const ok = await confirm({
    title: 'Remove the snapshot schedule?',
    description: 'No more snapshots are taken. Existing snapshots stay in the repository and can still be restored.',
    confirmLabel: 'Remove schedule',
    color: 'warning',
    icon: 'i-lucide-calendar-x'
  })
  if (!ok) return
  await remove.run()
  if (!remove.error.value) {
    emit('changed')
    await loader.refresh()
    syncForm()
  }
}

function resetForm() {
  syncForm()
  submitted.value = false
}

// --- run now, prune --------------------------------------------------------------------------

const runNow = useAsyncAction(() => requireAdmin().runBucketSnapshot(props.bucket), { errorTitle: 'Could not start a snapshot' })
async function onRunNow() {
  const job = await runNow.run()
  if (!job) return
  toast.add({
    title: 'Snapshot started',
    description: `Job ${job.id}`,
    color: 'success',
    icon: 'i-lucide-camera',
    actions: props.serverId ? [{ label: 'Open jobs', color: 'neutral', variant: 'outline', to: appRoutes.jobs(props.serverId, props.bucket) }] : undefined
  })
  if (loader.data.value) loader.data.value = { ...loader.data.value, status: { ...(loader.data.value.status ?? {}), running: job.id } }
}

const prune = useAsyncAction(() => requireAdmin().pruneBucketSnapshots(props.bucket), { successMessage: 'Prune started', errorTitle: 'Could not start the prune' })
async function onPrune() {
  const ok = await confirm({
    title: 'Prune the snapshot repository?',
    description: `Removes data that no snapshot references any more, so forgotten snapshots free their space. Prune holds restic's exclusive lock while it runs${props.archiveOn ? ': archive writes to this bucket wait until it finishes' : ''}.`,
    confirmLabel: 'Prune',
    color: 'primary',
    icon: 'i-lucide-scissors'
  })
  if (ok) await prune.run()
}

// --- snapshot actions --------------------------------------------------------------------------

const forget = useAsyncAction<[Snapshot], void>(
  s => requireAdmin().forgetSnapshot(props.bucket, s.id),
  { successMessage: s => `Snapshot ${s.shortId || s.id.slice(0, 8)} forgotten`, errorTitle: 'Could not forget the snapshot' }
)
const acting = ref<string | null>(null)

async function onForget(s: Snapshot) {
  const short = s.shortId || s.id.slice(0, 8)
  const ok = await confirm({
    title: `Forget snapshot ${short}?`,
    description: `The snapshot of ${formatDate(s.time)} is removed from the repository; its data is freed by the next prune. Per-version archive snapshots are never touched.`,
    confirmLabel: 'Forget snapshot',
    requireText: short,
    icon: 'i-lucide-trash-2'
  })
  if (!ok) return
  acting.value = s.id
  try {
    await forget.run(s)
    if (!forget.error.value) await loader.refresh()
  } finally {
    acting.value = null
  }
}

async function onRestore(s: Snapshot) {
  const job = await overlay.create(RestoreSnapshotModal, { destroyOnClose: true }).open({ bucket: props.bucket, snapshot: s, serverId: props.serverId })
  if (job) emit('changed')
}

function snapshotMenu(s: Snapshot): DropdownMenuItem[][] {
  const groups: DropdownMenuItem[][] = []
  // Partial snapshots are links of a chain: restore refuses them, and the chain's complete snapshot needs them.
  if (isPartialSnapshot(s)) return groups
  if (props.access.restore) groups.push([{ label: 'Restore to bucket…', icon: 'i-lucide-archive-restore', onSelect: () => void onRestore(s) }])
  if (props.access.snapshots) groups.push([{ label: 'Forget…', icon: 'i-lucide-trash-2', color: 'error', onSelect: () => void onForget(s) }])
  return groups
}
const hasActions = computed(() => props.access.restore || props.access.snapshots)
</script>

<template>
  <UCard data-testid="restic-snapshots">
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 class="font-semibold text-highlighted">
            Whole-bucket snapshots
          </h3>
          <p class="text-sm text-muted">
            Scheduled restic snapshots of the whole bucket: each holds the objects as files, a manifest of keys and versions, and the bucket’s settings, so <code class="font-mono text-xs">restic restore</code> alone recovers the content.
          </p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <UBadge v-if="config" :label="describeSchedule(config)" :color="config.enabled ? 'primary' : 'neutral'" variant="subtle" icon="i-lucide-calendar-clock" data-testid="snapshot-schedule" />
          <UButton
            v-if="access.run && config"
            icon="i-lucide-camera"
            label="Snapshot now"
            size="sm"
            :loading="runNow.loading.value"
            :disabled="!!status?.running"
            @click="onRunNow"
          />
          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-rotate-cw" aria-label="Reload snapshots" :loading="loader.loading.value" @click="loader.refresh" />
        </div>
      </div>
    </template>

    <UAlert
      v-if="unavailable === 'denied'"
      color="neutral"
      variant="subtle"
      icon="i-lucide-lock"
      title="Not allowed"
      description="Viewing this bucket's snapshots needs admin:GetBucketSnapshots on it."
      data-testid="snapshots-denied"
    />
    <UAlert
      v-else-if="unavailable === 'unsupported'"
      color="neutral"
      variant="subtle"
      icon="i-lucide-info"
      title="Not available"
      description="This server does not support whole-bucket snapshots."
    />
    <CommonErrorAlert v-else-if="loader.error.value && !data" :error="loader.error.value" title="Could not load the snapshots" retryable @retry="loader.refresh" />
    <USkeleton v-else-if="!data" class="h-40 w-full" />

    <div v-else class="space-y-6">
      <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Refresh failed; showing the last known state" retryable @retry="loader.refresh" />

      <!-- Status -->
      <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm sm:grid-cols-[auto_1fr_auto_1fr]" data-testid="snapshot-status">
        <dt class="text-muted">
          Last success
        </dt>
        <dd><CommonRelativeTime :value="status?.lastSuccessAt" /></dd>
        <dt class="text-muted">
          Next run
        </dt>
        <dd>
          <CommonRelativeTime v-if="config?.enabled && status?.nextRunAt" :value="status.nextRunAt" />
          <span v-else class="text-muted">{{ config ? (config.enabled ? '—' : 'Paused') : 'Not scheduled' }}</span>
        </dd>
        <dt class="text-muted">
          Last attempt
        </dt>
        <dd><CommonRelativeTime :value="status?.lastAttemptAt" /></dd>
        <dt class="text-muted">
          Repository
        </dt>
        <dd class="min-w-0 truncate font-mono text-xs" :title="data.repository?.repository">
          {{ data.repository?.repository || '—' }}
        </dd>
      </dl>
      <UAlert
        v-if="status?.running"
        color="info"
        variant="subtle"
        icon="i-lucide-loader-circle"
        title="A snapshot is running"
        :description="`Job ${status.running}.`"
        :actions="serverId ? [{ label: 'Open jobs', color: 'neutral', variant: 'outline', to: appRoutes.jobs(serverId, bucket) }] : undefined"
        :ui="{ icon: 'animate-spin' }"
        data-testid="snapshot-running"
      />
      <UAlert v-if="status?.lastError" color="error" variant="subtle" icon="i-lucide-triangle-alert" title="The last snapshot failed" :description="status.lastError" data-testid="snapshot-error" />

      <!-- Schedule -->
      <section v-if="access.snapshots" aria-label="Snapshot schedule" class="space-y-4 border-t border-default pt-4">
        <div class="flex items-center justify-between gap-2">
          <h4 class="text-sm font-semibold text-highlighted">
            Schedule
          </h4>
          <UBadge v-if="config && dirty" label="Unsaved" color="warning" variant="subtle" size="sm" />
          <UBadge v-else-if="!config" label="Not configured" color="neutral" variant="subtle" size="sm" />
        </div>
        <form class="space-y-4" novalidate data-testid="snapshot-form" @submit.prevent="onSave">
          <USwitch v-model="form.enabled" label="Take snapshots on schedule" description="Off keeps the settings; “Snapshot now” still works." />
          <div class="grid gap-3 sm:grid-cols-3">
            <UFormField label="Every" name="interval" help="1h to 30d, e.g. 24h, 12h, 7d." :error="submitted && errors.interval">
              <UInput v-model.trim="form.interval" class="w-full font-mono" placeholder="24h" aria-label="Interval" />
            </UFormField>
            <UFormField label="At (UTC)" name="at" help="For intervals of 24h or more; empty = any time." :error="submitted && errors.at">
              <UInput v-model.trim="form.at" class="w-full font-mono" placeholder="03:00" aria-label="Time of day (UTC)" />
            </UFormField>
            <UFormField label="Prefix" name="prefix" help="Only keys with this prefix; empty = all." :error="submitted && errors.prefix">
              <UInput v-model="form.prefix" class="w-full font-mono" placeholder="photos/" aria-label="Snapshot prefix" />
            </UFormField>
          </div>
          <USwitch v-model="form.consistent" label="Consistent" description="Versioned buckets: capture the versions current when the snapshot starts, even while writes continue." />

          <fieldset class="space-y-3 rounded-lg border border-default p-3" data-testid="retention">
            <legend class="px-1 text-sm font-medium text-highlighted">
              Retention
            </legend>
            <URadioGroup v-model="preset" :items="presetItems" orientation="horizontal" variant="card" aria-label="Retention preset" />
            <div class="grid grid-cols-3 gap-3 sm:grid-cols-7">
              <UFormField v-for="f in RETENTION_FIELDS" :key="f.key" :label="f.label" :name="f.key">
                <UInputNumber v-model="form.retention[f.key]" :min="0" class="w-full" :aria-label="`Keep ${f.label.toLowerCase()}`" />
              </UFormField>
              <UFormField label="Within" name="keepWithin" :error="submitted && errors.keepWithin">
                <UInput v-model.trim="form.retention.keepWithin" class="w-full font-mono" placeholder="0s" aria-label="Keep all within" />
              </UFormField>
            </div>
            <p v-if="errors.retention" class="text-sm text-error" role="alert">
              {{ errors.retention }}
            </p>
            <p v-else class="text-xs text-muted">
              Keeps {{ describeRetention(form.retention) }}. Applied by gos3 to this bucket’s whole-bucket snapshots only, never to per-version archive snapshots.
            </p>
          </fieldset>

          <fieldset class="space-y-3">
            <URadioGroup v-model="form.targetKind" legend="Repository" :items="targetItems" />
            <p v-if="config" class="text-xs text-muted" data-testid="snapshot-target">
              Saved: {{ describeTarget(config.target) }}
            </p>
            <div v-if="form.targetKind === 'backing'" class="grid gap-3 sm:grid-cols-2">
              <UFormField label="Backing" name="backing" :error="submitted && errors.backing">
                <USelect v-if="backingItems.length" v-model="form.backing" :items="backingItems" value-key="value" class="w-full" placeholder="Choose a backing" aria-label="Snapshot backing" />
                <UInput v-else v-model.trim="form.backing" class="w-full font-mono" placeholder="default" aria-label="Snapshot backing" />
              </UFormField>
              <UFormField v-if="form.repo" label="Repository name" name="repo" help="Generated by the server; kept when the bucket moves.">
                <UInput :model-value="form.repo" class="w-full font-mono" disabled aria-label="Repository name" />
              </UFormField>
            </div>
          </fieldset>

          <div class="space-y-2">
            <USwitch
              v-model="form.pruneEnabled"
              label="Prune automatically"
              @update:model-value="form.pruneDefault = false"
              :description="archiveOn ? 'Off by default while the archive is on: prune takes restic’s exclusive lock, and archive writes wait until it finishes.' : 'Frees the space of forgotten snapshots.'"
            />
            <UFormField v-if="form.pruneEnabled" label="Prune every" name="pruneInterval" :error="submitted && errors.pruneInterval" class="sm:w-60">
              <UInput v-model.trim="form.pruneInterval" class="w-full font-mono" placeholder="168h" aria-label="Prune interval" />
            </UFormField>
          </div>

          <div class="flex flex-wrap justify-between gap-2 border-t border-default pt-4">
            <div class="flex gap-2">
              <UButton v-if="config" color="neutral" variant="outline" icon="i-lucide-calendar-x" label="Remove schedule" :loading="remove.loading.value" @click="onRemove" />
              <UButton color="neutral" variant="outline" icon="i-lucide-scissors" label="Prune now" :loading="prune.loading.value" @click="onPrune" />
            </div>
            <div class="flex gap-2">
              <UButton v-if="config && dirty" color="neutral" variant="ghost" label="Reset" @click="resetForm" />
              <UButton type="submit" icon="i-lucide-save" :label="config ? 'Save schedule' : 'Create schedule'" :loading="save.loading.value" :disabled="!dirty" />
            </div>
          </div>
        </form>
      </section>
      <section v-else-if="config" aria-label="Snapshot schedule" class="space-y-1 border-t border-default pt-4 text-sm" data-testid="snapshot-readonly">
        <p>
          <span class="text-muted">Schedule:</span> {{ describeSchedule(config) }}<template v-if="config.prefix">
            , keys under <span class="font-mono text-xs">{{ config.prefix }}</span>
          </template>
        </p>
        <p><span class="text-muted">Retention:</span> {{ describeRetention(snapshotFormFrom(config).retention) || '—' }}</p>
        <p><span class="text-muted">Repository:</span> {{ describeTarget(config.target) }}</p>
        <p class="text-xs text-muted">
          Changing the schedule needs admin:PutBucketSnapshots.
        </p>
      </section>
      <p v-else class="border-t border-default pt-4 text-sm text-muted">
        No snapshot schedule. Setting one up needs admin:PutBucketSnapshots.
      </p>

      <!-- Snapshots -->
      <section aria-label="Snapshots" class="space-y-2 border-t border-default pt-4">
        <h4 class="text-sm font-semibold text-highlighted">
          Snapshots <span class="font-normal text-muted">({{ snapshots.length }})</span>
        </h4>
        <UAlert
          v-if="data.error"
          color="warning"
          variant="subtle"
          icon="i-lucide-triangle-alert"
          title="Could not list the snapshots"
          :description="data.error"
          data-testid="snapshots-error"
        />
        <p v-else-if="!snapshots.length" class="text-sm text-muted" data-testid="no-snapshots">
          No whole-bucket snapshots yet.
        </p>
        <div v-else class="overflow-x-auto">
          <table class="w-full text-sm" aria-label="Whole-bucket snapshots" data-testid="snapshot-list">
            <thead class="border-b border-default text-left text-xs text-muted">
              <tr>
                <th scope="col" class="px-3 py-2 font-medium">
                  Taken
                </th>
                <th scope="col" class="px-3 py-2 font-medium">
                  ID
                </th>
                <th scope="col" class="px-3 py-2 text-right font-medium">
                  Files
                </th>
                <th scope="col" class="px-3 py-2 text-right font-medium">
                  Added
                </th>
                <th v-if="hasActions" scope="col" class="w-0 px-3 py-2">
                  <span class="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody class="divide-y divide-default">
              <tr v-for="s in snapshots" :key="s.id" :data-testid="`snapshot-${s.shortId || s.id}`">
                <th scope="row" class="px-3 py-2 text-left font-normal">
                  <CommonRelativeTime :value="s.time" />
                  <UBadge
                    v-if="isPartialSnapshot(s)"
                    label="partial"
                    color="warning"
                    variant="subtle"
                    size="sm"
                    class="ms-2"
                    title="Part of a chain written when the spool budget was reached; restore the complete snapshot instead."
                  />
                </th>
                <td class="px-3 py-2">
                  <span class="inline-flex items-center gap-1 font-mono text-xs">{{ s.shortId || s.id.slice(0, 8) }}<CommonCopyButton :value="s.id" aria-label="Copy snapshot ID" /></span>
                </td>
                <td class="px-3 py-2 text-right text-xs tabular-nums text-muted">
                  <template v-if="s.summary">
                    {{ formatNumber(s.summary.totalFilesProcessed) }}
                    <span class="block">{{ formatNumber(s.summary.filesNew) }} new · {{ formatNumber(s.summary.filesChanged) }} changed</span>
                  </template>
                  <template v-else>
                    —
                  </template>
                </td>
                <td class="px-3 py-2 text-right text-xs tabular-nums">
                  {{ s.summary ? formatBytes(s.summary.dataAdded) : '—' }}
                </td>
                <td v-if="hasActions" class="px-3 py-2 text-right">
                  <UDropdownMenu v-if="snapshotMenu(s).length" :items="snapshotMenu(s)" :content="{ align: 'end' }">
                    <UButton
                      icon="i-lucide-ellipsis-vertical"
                      color="neutral"
                      variant="ghost"
                      size="sm"
                      :loading="acting === s.id"
                      :aria-label="`Actions for snapshot ${s.shortId || s.id.slice(0, 8)}`"
                    />
                  </UDropdownMenu>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </div>
  </UCard>
</template>
