<script setup lang="ts">
import type { Backing, Placement, PlacementMember, ReplicationMode } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { canManageStorage } from '~/components/admin/-capabilities'
import { containerName, containerSelectItems, findContainer, preferredContainer } from '~/components/admin/-containers'
import {
  MODE_OPTIONS,
  NATIVE_PLACEMENT_NOTE,
  PLACEMENT_POLL_MS,
  addedReplicas,
  copyMembers,
  formatProgress,
  memberStateMeta,
  modeLabel,
  moveTargets,
  placementBusy,
  promoteAdvice,
  removedReplicas,
  replicaChoices,
  replicaNames,
  replicationChanged,
  roleLabel,
  validateReplication
} from '~/components/admin/-storage'

/**
 * Storage placement of a bucket on a gos3 server with several backings
 * (GET /buckets/{name}/placement): the primary copy, replicas and a move in
 * progress, with each member's sync state. With admin:UpdateBucketPlacement
 * (capability manageStorage) the replication mode and replicas can be
 * changed, the bucket moved to another backing (choosing the container of
 * the copy when the target is an s3 backing: copies there are wrapped
 * buckets), a replica promoted and replicas resynced. Native buckets of s3
 * backings keep their data in their remote bucket and can neither be
 * replicated nor moved. Polls while copies run or a move is in progress.
 */
const props = defineProps<{
  bucket: string
  /** s3 backings: the bucket kind (native or wrapped), when known. */
  kind?: string
}>()
const emit = defineEmits<{ changed: [] }>()

const { connection, requireAdmin } = useConnection()
const { capabilities } = useWhoAmI()
const confirm = useConfirm()

const canManage = computed(() => canManageStorage(connection.value?.kind, capabilities.value))
const native = computed(() => props.kind === 'native')

// --- loading ------------------------------------------------------------------

const placement = ref<Placement | null>(null)
const loading = ref(false)
const error = ref<unknown>(null)
const backings = ref<Backing[]>([])
const backingsError = ref<unknown>(null)
let seq = 0

async function load(opts: { withBackings?: boolean } = {}) {
  const my = ++seq
  loading.value = true
  try {
    const admin = requireAdmin()
    const [p, b] = await Promise.all([
      admin.getBucketPlacement(props.bucket),
      opts.withBackings
        ? admin.listBackings().then(r => r.backings ?? [], (e: unknown) => {
            // Optional: the forms then only offer the bucket's own members.
            backingsError.value = e
            return null
          })
        : Promise.resolve(null)
    ])
    if (my !== seq) return
    placement.value = p
    error.value = null
    if (b) {
      backings.value = b
      backingsError.value = null
    }
  } catch (e) {
    if (my === seq && !isAbortedError(e)) error.value = e
  } finally {
    if (my === seq) loading.value = false
  }
}

function refresh() {
  void load({ withBackings: true })
}

/** Applies a placement returned by a change and tells the page (bucket summary). */
function applied(p: Placement | undefined) {
  if (!p) return
  seq++ // a poll in flight must not overwrite the newer state
  loading.value = false
  placement.value = p
  syncForm(p)
  emit('changed')
}

watch(() => props.bucket, () => {
  placement.value = null
  error.value = null
  refresh()
}, { immediate: true })

const accessDenied = computed(() => error.value instanceof AdminError && error.value.isAccessDenied)

// --- polling while something is in progress --------------------------------------

const busy = computed(() => placementBusy(placement.value))
let pollTimer: ReturnType<typeof setTimeout> | undefined
function stopPolling() {
  if (pollTimer) clearTimeout(pollTimer)
  pollTimer = undefined
}
function schedulePoll() {
  stopPolling()
  if (!busy.value) return
  pollTimer = setTimeout(async () => {
    pollTimer = undefined
    if (typeof document === 'undefined' || document.visibilityState !== 'hidden') {
      const wasMoving = !!placement.value?.move
      await load()
      // A finished move or copy changes the backings' counts and the summary.
      if (wasMoving && !placement.value?.move) {
        emit('changed')
        void load({ withBackings: true })
      }
    }
    schedulePoll()
  }, PLACEMENT_POLL_MS)
}
watch(busy, (b) => {
  if (b) {
    if (!pollTimer) schedulePoll()
  } else {
    stopPolling()
  }
}, { immediate: true })
onBeforeUnmount(stopPolling)

// --- derived ------------------------------------------------------------------------

const members = computed(() => placement.value?.members ?? [])
const move = computed(() => placement.value?.move ?? null)
const backingHealth = computed(() => new Map(backings.value.map(b => [b.name, b])))

function memberError(m: PlacementMember): string {
  if (m.lastError) return m.lastError
  if (!m.healthy) return backingHealth.value.get(m.backing)?.lastError ?? ''
  return ''
}

// --- change replication -------------------------------------------------------------

const mode = ref<ReplicationMode>('single')
const replicas = ref<string[]>([])

function syncForm(p: Placement | null) {
  if (!p) return
  mode.value = p.mode
  replicas.value = replicaNames(p)
}

// Keep the form in step with the server unless the user is editing it.
watch(placement, (next, prev) => {
  if (!next) return
  if (!prev || prev.bucket !== next.bucket || !replicationChanged(prev, mode.value, replicas.value)) syncForm(next)
})

watch(mode, (m) => {
  if (m === 'single') replicas.value = []
})

const modeItems = MODE_OPTIONS.map(o => ({ value: o.value, label: o.label, description: o.description }))
const choices = computed(() => (placement.value ? replicaChoices(backings.value, placement.value) : []))
const formError = computed(() => validateReplication(mode.value, replicas.value))
const dirty = computed(() => !!placement.value && replicationChanged(placement.value, mode.value, replicas.value))

function toggleReplica(name: string, on: boolean | 'indeterminate') {
  const set = new Set(replicas.value)
  if (on === true) set.add(name)
  else set.delete(name)
  replicas.value = choices.value.map(c => c.value).filter(v => set.has(v))
}

const save = useAsyncAction(
  () => requireAdmin().updateBucketPlacement(props.bucket, mode.value, [...replicas.value]),
  { successMessage: 'Replication updated', errorTitle: 'Could not change replication' }
)

async function onSaveReplication() {
  const p = placement.value
  if (!p || formError.value || !dirty.value) return
  const removed = removedReplicas(replicaNames(p), replicas.value)
  if (removed.length) {
    const ok = await confirm({
      title: `Remove ${removed.length === 1 ? 'replica' : 'replicas'} ${removed.join(', ')}?`,
      description: `The ${removed.length === 1 ? 'copy' : 'copies'} of ${props.bucket} on ${removed.join(', ')} will be deleted in the background. The primary copy on ${p.primary} is kept.`,
      confirmLabel: removed.length === 1 ? 'Remove replica' : 'Remove replicas',
      icon: 'i-lucide-trash-2'
    })
    if (!ok) return
  }
  applied(await save.run())
}

function resetReplication() {
  syncForm(placement.value)
}

const addedNote = computed(() => {
  const added = placement.value ? addedReplicas(replicaNames(placement.value), replicas.value) : []
  return added.length ? `A full copy to ${added.join(', ')} starts right away; the bucket stays available.` : ''
})

// --- move ------------------------------------------------------------------------------

const moveTo = ref('')
const moveFrom = ref('')
const targets = computed(() => (placement.value ? moveTargets(backings.value, placement.value) : []))
const targetItems = computed(() => targets.value.map(t => ({ label: t.disabledReason ? `${t.label} (${t.disabledReason})` : t.label, value: t.value, disabled: !!t.disabledReason })))
const sourceItems = computed(() => copyMembers(placement.value).map(m => ({ label: `${m.backing} (${roleLabel(m.role).toLowerCase()})`, value: m.backing })))

watch(placement, (p) => {
  if (!p) return
  if (!moveFrom.value || !copyMembers(p).some(m => m.backing === moveFrom.value)) moveFrom.value = p.primary
  if (moveTo.value && !targets.value.some(t => t.value === moveTo.value && !t.disabledReason)) moveTo.value = ''
}, { immediate: true })

// Container of the copy on an s3 target (copies there are wrapped buckets).
const targetBacking = computed(() => backingHealth.value.get(moveTo.value))
const targetContainers = computed(() => (targetBacking.value?.kind === 's3' ? targetBacking.value.account?.containers ?? [] : []))
const containerChoices = computed(() => containerSelectItems(targetContainers.value))
const moveContainer = ref('')
watch(moveTo, () => {
  moveContainer.value = preferredContainer(targetContainers.value, targetBacking.value?.account?.defaultContainer)
})

const moveAction = useAsyncAction<[string, string, string], Placement>(
  (to, from, container) => requireAdmin().moveBucket(props.bucket, to, from && from !== placement.value?.primary ? from : undefined, container || undefined),
  { successMessage: to => `Move to ${to} started`, errorTitle: 'Could not start the move' }
)

async function onMove() {
  const p = placement.value
  if (!p || !moveTo.value) return
  const from = moveFrom.value || p.primary
  const isPrimary = from === p.primary
  const container = containerChoices.value.length ? moveContainer.value : ''
  const into = container ? ` (container ${containerName(findContainer(targetContainers.value, container) ?? { id: container })})` : ''
  const ok = await confirm({
    title: `Move ${props.bucket} from ${from} to ${moveTo.value}?`,
    description: `${moveTo.value}${into} is filled in the background while the bucket stays readable and writable. A short cutover then makes ${moveTo.value} take ${from}'s place and the copy on ${from} is deleted.${isPrimary ? ' In-progress multipart uploads are not moved and will fail.' : ''}`,
    confirmLabel: 'Start move',
    color: 'primary',
    icon: 'i-lucide-arrow-right-left'
  })
  if (!ok) return
  const r = await moveAction.run(moveTo.value, from, container)
  if (r) {
    moveTo.value = ''
    applied(r)
  }
}

const cancelMoveAction = useAsyncAction(() => requireAdmin().cancelBucketMove(props.bucket), {
  successMessage: 'Move canceled',
  errorTitle: 'Could not cancel the move'
})

async function onCancelMove() {
  const m = move.value
  if (!m) return
  const ok = await confirm({
    title: `Cancel the move to ${m.to}?`,
    description: `The bucket stays on ${m.from}; the partial copy on ${m.to} is deleted.`,
    confirmLabel: 'Cancel move',
    cancelLabel: 'Keep moving',
    color: 'warning',
    icon: 'i-lucide-circle-x'
  })
  if (ok) applied(await cancelMoveAction.run())
}

// --- promote & resync ---------------------------------------------------------------

const acting = ref<string | null>(null)

const promoteAction = useAsyncAction<[string, boolean], Placement>(
  (backing, force) => requireAdmin().promoteReplica(props.bucket, backing, force),
  { successMessage: backing => `${backing} is now the primary`, errorTitle: 'Could not promote the replica' }
)

async function onPromote(backing: string, force = false) {
  const p = placement.value
  if (!p) return
  const advice = promoteAdvice(p, backing)
  const ok = force
    ? await confirm({
        title: `Force ${backing} to become the primary?`,
        description: `${backing} becomes the primary immediately${advice.reason ? ` although ${advice.reason}` : ''}. Changes not yet copied to ${backing} are lost; the old primary ${p.primary} is then re-copied from ${backing}.`,
        confirmLabel: 'Force promote',
        requireText: backing,
        icon: 'i-lucide-triangle-alert'
      })
    : await confirm({
        title: `Make ${backing} the primary?`,
        description: `Pending changes are copied to ${backing} during a short cutover (writes wait), then it becomes the primary and ${p.primary} a replica. Nothing is lost.`,
        confirmLabel: 'Promote',
        color: 'primary',
        icon: 'i-lucide-crown'
      })
  if (!ok) return
  acting.value = backing
  try {
    applied(await promoteAction.run(backing, force))
  } finally {
    acting.value = null
  }
}

const resyncAction = useAsyncAction<[string | undefined], Placement>(
  backing => requireAdmin().resyncBucket(props.bucket, backing),
  { successMessage: backing => (backing ? `Resync of ${backing} started` : 'Resync of all replicas started'), errorTitle: 'Could not start the resync' }
)

async function onResync(backing?: string) {
  acting.value = backing ?? '*'
  try {
    applied(await resyncAction.run(backing))
  } finally {
    acting.value = null
  }
}

function memberMenu(m: PlacementMember) {
  const p = placement.value
  if (!p || m.role !== 'replica') return []
  const advice = promoteAdvice(p, m.backing)
  const items = [
    { label: 'Promote to primary', icon: 'i-lucide-crown', disabled: !!move.value || advice.normalRefused, onSelect: () => void onPromote(m.backing) },
    { label: 'Resync', icon: 'i-lucide-refresh-cw', onSelect: () => void onResync(m.backing) }
  ]
  const groups = [items]
  if (advice.suggestForce) {
    groups.push([{ label: 'Force promote…', icon: 'i-lucide-triangle-alert', disabled: !!move.value, onSelect: () => void onPromote(m.backing, true) }])
  }
  return groups
}

const hasReplicas = computed(() => members.value.some(m => m.role === 'replica'))
</script>

<template>
  <div class="space-y-6">
    <CommonEmptyState
      v-if="accessDenied"
      icon="i-lucide-lock"
      title="No access to the placement"
      description="Your identity is not allowed to read this bucket's placement (admin:GetBucketPlacement)."
    />
    <CommonErrorAlert v-else-if="error && !placement" :error="error" title="Could not load the placement" retryable @retry="refresh" />

    <div v-if="!placement && loading" class="space-y-3" aria-busy="true" aria-label="Loading placement">
      <USkeleton class="h-32 w-full" />
      <USkeleton class="h-40 w-full" />
    </div>

    <template v-if="placement">
      <CommonErrorAlert v-if="error" :error="error" title="Refresh failed; showing the last known state" retryable @retry="refresh" />

      <UAlert
        v-if="move"
        color="info"
        variant="subtle"
        icon="i-lucide-arrow-right-left"
        :title="`Moving from ${move.from} to ${move.to}`"
        :actions="canManage && move.phase === 'copying' ? [{ label: 'Cancel move', icon: 'i-lucide-x', color: 'neutral', variant: 'outline', loading: cancelMoveAction.loading.value, onClick: onCancelMove }] : undefined"
        data-testid="move-banner"
      >
        <template #description>
          <p v-if="move.phase === 'copying'">
            Copying to {{ move.to }}<template v-if="move.target?.container">
              (container {{ move.target.container }})
            </template>; the bucket stays readable and writable.
          </p>
          <p v-else>
            Cutover: writes wait briefly while {{ move.to }} takes {{ move.from }}'s place. It can no longer be canceled.
          </p>
          <p class="mt-1 text-xs opacity-80">
            Phase {{ move.phase }} · started <CommonRelativeTime :value="move.startedAt" />
          </p>
        </template>
      </UAlert>

      <UCard :ui="{ body: 'p-0 sm:p-0' }">
        <template #header>
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div>
              <h3 class="font-semibold text-highlighted">
                Placement
              </h3>
              <p class="text-sm text-muted">
                Primary copy on <span class="font-medium text-highlighted">{{ placement.primary }}</span><template v-if="hasReplicas">
                  , replicated to {{ members.filter(m => m.role === 'replica').map(m => m.backing).join(', ') }}
                </template>.
              </p>
            </div>
            <div class="flex flex-wrap items-center gap-2">
              <UBadge :label="modeLabel(placement.mode)" :color="placement.mode === 'single' ? 'neutral' : 'primary'" variant="subtle" data-testid="placement-mode" />
              <UButton
                v-if="canManage && hasReplicas"
                color="neutral"
                variant="outline"
                size="sm"
                icon="i-lucide-refresh-cw"
                label="Resync all"
                :loading="acting === '*'"
                @click="onResync()"
              />
              <UButton
                color="neutral"
                variant="ghost"
                size="sm"
                icon="i-lucide-rotate-cw"
                aria-label="Reload placement"
                :loading="loading"
                @click="refresh"
              />
            </div>
          </div>
        </template>

        <div class="overflow-x-auto">
          <table class="w-full text-sm" aria-label="Placement members">
            <thead class="border-b border-default text-left text-xs text-muted">
              <tr>
                <th scope="col" class="px-4 py-2 font-medium">
                  Backing
                </th>
                <th scope="col" class="px-4 py-2 font-medium">
                  State
                </th>
                <th scope="col" class="px-4 py-2 text-right font-medium">
                  Pending
                </th>
                <th scope="col" class="px-4 py-2 font-medium">
                  Copy progress
                </th>
                <th scope="col" class="px-4 py-2 font-medium">
                  Last full sync
                </th>
                <th v-if="canManage" scope="col" class="w-0 px-4 py-2">
                  <span class="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody class="divide-y divide-default">
              <tr v-for="m in members" :key="`${m.role}:${m.backing}`" :data-testid="`member-${m.backing}`">
                <th scope="row" class="px-4 py-2.5 text-left font-normal">
                  <div class="flex items-center gap-2">
                    <CommonStatusDot :state="m.healthy ? 'healthy' : 'unhealthy'" />
                    <span class="font-medium text-highlighted">{{ m.backing }}</span>
                    <UBadge
                      :label="roleLabel(m.role)"
                      :color="m.role === 'primary' ? 'primary' : 'neutral'"
                      :variant="m.role === 'primary' ? 'subtle' : 'outline'"
                      size="sm"
                      :icon="m.role === 'primary' ? 'i-lucide-crown' : m.role === 'move-target' ? 'i-lucide-arrow-right-left' : 'i-lucide-copy'"
                    />
                  </div>
                </th>
                <td class="px-4 py-2.5">
                  <div class="space-y-1">
                    <UTooltip :text="memberStateMeta(m.state).description" :disabled="!memberStateMeta(m.state).description">
                      <UBadge
                        :label="memberStateMeta(m.state).label"
                        :color="memberStateMeta(m.state).color"
                        :icon="memberStateMeta(m.state).icon"
                        variant="subtle"
                        size="sm"
                        :ui="{ leadingIcon: m.state === 'syncing' ? 'animate-spin' : '' }"
                      />
                    </UTooltip>
                    <p v-if="memberError(m)" class="max-w-xs text-xs text-error" data-testid="member-error">
                      {{ memberError(m) }}
                    </p>
                  </div>
                </td>
                <td class="px-4 py-2.5 text-right tabular-nums">
                  {{ m.role === 'primary' ? '—' : formatNumber(m.pending) }}
                </td>
                <td class="px-4 py-2.5 text-xs tabular-nums text-muted">
                  {{ m.progress ? formatProgress(m.progress, formatNumber, formatBytes) : '—' }}
                </td>
                <td class="px-4 py-2.5 text-sm">
                  <span v-if="m.role === 'primary'" class="text-muted">—</span>
                  <CommonRelativeTime v-else :value="m.lastSyncAt" />
                </td>
                <td v-if="canManage" class="px-4 py-2.5 text-right">
                  <UDropdownMenu v-if="m.role === 'replica'" :items="memberMenu(m)" :content="{ align: 'end' }">
                    <UButton
                      icon="i-lucide-ellipsis-vertical"
                      color="neutral"
                      variant="ghost"
                      size="sm"
                      :loading="acting === m.backing"
                      :aria-label="`Actions for replica ${m.backing}`"
                    />
                  </UDropdownMenu>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </UCard>

      <UAlert
        v-if="!canManage"
        color="neutral"
        variant="subtle"
        icon="i-lucide-lock"
        title="Read-only"
        description="Changing replication, moving the bucket, promoting and resyncing replicas need admin:UpdateBucketPlacement."
        data-testid="placement-readonly"
      />

      <UAlert
        v-else-if="native"
        color="neutral"
        variant="subtle"
        icon="i-lucide-cloud"
        title="Native bucket"
        :description="NATIVE_PLACEMENT_NOTE"
        data-testid="placement-native"
      />

      <template v-else>
        <CommonErrorAlert v-if="backingsError" :error="backingsError" title="Could not list the storage backings; only the bucket's own backings are offered" />

        <UCard>
          <template #header>
            <h3 class="font-semibold text-highlighted">
              Replication
            </h3>
            <p class="text-sm text-muted">
              Keep copies of this bucket on other backings. The primary stays on {{ placement.primary }}.
            </p>
          </template>
          <form class="space-y-4" data-testid="replication-form" @submit.prevent="onSaveReplication">
            <URadioGroup v-model="mode" legend="Mode" :items="modeItems" :disabled="!!move" />
            <fieldset v-if="mode !== 'single'" class="space-y-2">
              <legend class="mb-1 text-sm font-medium text-highlighted">
                Replica backings
              </legend>
              <p v-if="!choices.length" class="text-sm text-muted">
                There is no other backing to replicate to.
              </p>
              <div v-for="c in choices" :key="c.value" :data-testid="`replica-${c.value}`">
                <UCheckbox
                  :model-value="replicas.includes(c.value)"
                  :label="c.label"
                  :description="c.healthy ? undefined : 'Unhealthy: the copy starts once it recovers.'"
                  :disabled="!!move"
                  @update:model-value="toggleReplica(c.value, $event)"
                />
              </div>
            </fieldset>
            <p v-if="dirty && formError" class="text-sm text-error" role="alert">
              {{ formError }}
            </p>
            <p v-else-if="addedNote" class="text-sm text-muted">
              {{ addedNote }}
            </p>
            <p v-if="move" class="text-sm text-muted">
              Replication cannot change while a move is in progress.
            </p>
            <div class="flex flex-wrap gap-2">
              <UButton type="submit" icon="i-lucide-save" label="Save replication" :loading="save.loading.value" :disabled="!dirty || !!formError || !!move" />
              <UButton v-if="dirty" color="neutral" variant="ghost" label="Reset" @click="resetReplication" />
            </div>
          </form>
        </UCard>

        <UCard>
          <template #header>
            <h3 class="font-semibold text-highlighted">
              Move to another backing
            </h3>
            <p class="text-sm text-muted">
              Copies the bucket online, then switches over and deletes the old copy.
            </p>
          </template>
          <p v-if="move" class="text-sm text-muted">
            A move is already in progress.
          </p>
          <p v-else-if="!targets.length" class="text-sm text-muted" data-testid="no-move-targets">
            Every backing already holds a copy of this bucket.
          </p>
          <form v-else class="flex flex-col gap-3 sm:flex-row sm:items-end" data-testid="move-form" @submit.prevent="onMove">
            <UFormField label="Copy to move" name="move-from" class="sm:w-56">
              <USelect v-model="moveFrom" :items="sourceItems" class="w-full" />
            </UFormField>
            <UFormField label="Target backing" name="move-to" class="sm:w-56">
              <USelect v-model="moveTo" :items="targetItems" placeholder="Choose a backing" class="w-full" />
            </UFormField>
            <UFormField v-if="containerChoices.length" label="Container" name="move-container" class="sm:w-56" help="Copies on S3 backings are wrapped buckets.">
              <USelect v-model="moveContainer" :items="containerChoices" value-key="value" class="w-full" aria-label="Container" />
            </UFormField>
            <UButton type="submit" icon="i-lucide-arrow-right-left" label="Move" :loading="moveAction.loading.value" :disabled="!moveTo || (containerChoices.length > 0 && !moveContainer)" />
          </form>
        </UCard>
      </template>
    </template>
  </div>
</template>
