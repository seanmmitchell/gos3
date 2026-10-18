<script setup lang="ts">
import type { BucketEncryptionProvider, RepositoryInfo, ResticInfo, ResticPurpose } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { resticAccess } from '~/components/admin/-capabilities'
import { roleLabel } from '~/components/admin/-storage'
import { ARCHIVE_COST_WARNING, GOS3_CONFIG_PATH, archiveAvailability, archiveEnabled, archiveToggleBody, bucketLacksRepository, gos3ResticCommand, type Shell } from './-restic'
import ResticCredentialsModal from './ResticCredentialsModal.vue'
import ResticRepository from './ResticRepository.vue'
import ResticSnapshotsCard from './ResticSnapshotsCard.vue'

/**
 * "Backup & archive" (gos3 servers with restic snapshots):
 *
 * - Archive: the per-version archive toggle — restic as the bucket's
 *   encryption provider for every write (PUT encryption-provider with
 *   scope "all"), so every object version becomes a restic snapshot.
 *   Offered when the server reports restic available for the bucket, with
 *   the cost spelled out.
 * - Recovery: where each copy's archive repository (and the snapshot
 *   repository) lives, its recovery record and password source, and the
 *   restic commands for bash or PowerShell that list the snapshots without
 *   gos3 (GET /buckets/{name}/restic, no secrets). "Reveal credentials…"
 *   (capability revealResticCredentials) asks first, then shows the
 *   password and environment once in a dialog; nothing is stored.
 * - Whole-bucket snapshots (ResticSnapshotsCard).
 *
 * Reading needs admin:GetBucketEncryption / admin:GetBucketSnapshots on
 * the bucket; a 403 is explained per card.
 */
const props = defineProps<{
  bucket: string
  /** s3 backings: the bucket kind (native or wrapped), when known. */
  kind?: string
}>()
const emit = defineEmits<{ changed: [] }>()

const { id: serverId, connection, requireAdmin } = useConnection()
const { capabilities } = useWhoAmI()
const confirm = useConfirm()
const overlay = useOverlay()

const access = computed(() => resticAccess(connection.value?.kind, capabilities.value))

function deniedOrMissing(e: unknown): 'denied' | 'unsupported' | undefined {
  if (!(e instanceof AdminError)) return undefined
  if (e.isAccessDenied) return 'denied'
  if (e.isNotFound) return 'unsupported'
  return undefined
}

// --- archive ----------------------------------------------------------------------------

const providerLoader = useLoader(() => requireAdmin().getBucketEncryptionProvider(props.bucket), { watch: [() => props.bucket] })
const providerDoc = computed<BucketEncryptionProvider | undefined>(() => providerLoader.data.value)
const providerUnavailable = computed(() => deniedOrMissing(providerLoader.error.value))
const archiveOn = computed(() => archiveEnabled(providerDoc.value))
// Native buckets and buckets whose backings cannot hold restic have no archive (both directions of the toggle need restic).
const availability = computed(() => archiveAvailability(providerDoc.value, props.kind))

const toggle = useAsyncAction(
  (on: boolean) => requireAdmin().putBucketEncryptionProvider(props.bucket, archiveToggleBody(on)),
  { successMessage: on => (on ? 'Archive turned on' : 'Archive turned off'), errorTitle: 'Could not change the archive' }
)

async function onToggle(on: boolean | 'indeterminate') {
  if (on === 'indeterminate' || on === archiveOn.value) return
  const wasAes = providerDoc.value?.provider === 'AES256'
  const ok = on
    ? await confirm({
        title: 'Archive every object version in restic?',
        description: `${ARCHIVE_COST_WARNING}${wasAes ? ' SSE-S3 uploads are then stored in restic too, instead of native AES-256-GCM.' : ''} Objects written before stay as they are.`,
        confirmLabel: 'Turn on archive',
        color: 'primary',
        icon: 'i-lucide-archive'
      })
    : await confirm({
        title: 'Stop archiving every write?',
        description: 'New writes are archived only when they request SSE-S3 (restic stays the bucket’s SSE-S3 implementation). Archived versions and their snapshots stay readable.',
        confirmLabel: 'Turn off archive',
        color: 'warning',
        icon: 'i-lucide-archive-x'
      })
  if (!ok) return
  const doc = await toggle.run(on)
  if (!doc) return
  providerLoader.data.value = doc
  emit('changed')
  void resticLoader.refresh()
}

// --- recovery -----------------------------------------------------------------------------

const resticLoader = useLoader(() => requireAdmin().getBucketRestic(props.bucket), { watch: [() => props.bucket] })
const info = computed<ResticInfo | undefined>(() => resticLoader.data.value)
const resticUnavailable = computed(() => deniedOrMissing(resticLoader.error.value))
const archiveCopies = computed(() => info.value?.archive ?? [])
const tags = computed(() => info.value?.tags ?? { object: 'gos3-object', bucket: '', snapshot: 'gos3-snapshot' })

/**
 * The bucket has no repository of its own (native buckets, or a primary
 * reporting "no repository"): snapshots go to a backing. A repository not
 * created yet still counts as the bucket's own.
 */
const needsBacking = computed(() => bucketLacksRepository(info.value?.archive, props.kind))

const shell = ref<Shell>('bash')
const revealing = ref<string | null>(null)
/** This server's URL, the gos3 CLI's online source (--server). */
const serverUrl = computed(() => connection.value?.endpoint ?? '')
const cliOnline = computed(() => gos3ResticCommand(shell.value, 'env', [['--bucket', props.bucket], ['--shell', shell.value]], { server: serverUrl.value }))
const cliOffline = computed(() => gos3ResticCommand(shell.value, 'env', [['--bucket', props.bucket], ['--shell', shell.value]], { config: GOS3_CONFIG_PATH }))

const reveal = useAsyncAction(
  (backing: string | undefined, purpose: ResticPurpose) => requireAdmin().getResticCredentials(props.bucket, { backing, purpose }),
  { errorTitle: 'Could not reveal the credentials' }
)

async function onReveal(backing: string | undefined, purpose: ResticPurpose) {
  const ok = await confirm({
    title: 'Reveal the repository password?',
    description: `The password and access keys of the ${purpose === 'snapshots' ? 'snapshot' : 'archive'} repository${backing ? ` on ${backing}` : ''} are shown once. Anyone who has them can read every archived object of ${props.bucket}. The server logs this disclosure.`,
    confirmLabel: 'Reveal credentials',
    color: 'warning',
    icon: 'i-lucide-key-round',
    irreversible: false
  })
  if (!ok) return
  revealing.value = `${purpose}:${backing ?? ''}`
  try {
    const creds = await reveal.run(backing, purpose)
    if (!creds) return
    await overlay.create(ResticCredentialsModal, { destroyOnClose: true }).open({ bucket: props.bucket, credentials: creds, tags: tags.value, server: serverUrl.value })
  } finally {
    revealing.value = null
  }
}

function snapshotRepo(): RepositoryInfo | null {
  return info.value?.snapshotRepository ?? null
}
</script>

<template>
  <div class="space-y-4">
    <!-- Archive -->
    <UCard data-testid="restic-archive">
      <template #header>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h3 class="font-semibold text-highlighted">
              Archive
            </h3>
            <p class="text-sm text-muted">
              Keep every object version as a restic snapshot in the bucket’s repository, recoverable with restic alone.
            </p>
          </div>
          <UBadge
            v-if="providerDoc"
            :label="archiveOn ? 'Archiving every write' : providerDoc.provider === 'restic' ? 'SSE-S3 writes only' : 'Off'"
            :color="archiveOn ? 'success' : 'neutral'"
            variant="subtle"
            :icon="archiveOn ? 'i-lucide-archive' : undefined"
            data-testid="archive-scope"
          />
        </div>
      </template>
      <UAlert
        v-if="providerUnavailable === 'denied'"
        color="neutral"
        variant="subtle"
        icon="i-lucide-lock"
        title="Not allowed"
        description="The archive setting needs admin:GetBucketEncryption and admin:PutBucketEncryption on this bucket."
        data-testid="archive-denied"
      />
      <UAlert
        v-else-if="providerUnavailable === 'unsupported'"
        color="neutral"
        variant="subtle"
        icon="i-lucide-info"
        title="Not available"
        description="This server does not support the restic archive."
      />
      <CommonErrorAlert v-else-if="providerLoader.error.value" :error="providerLoader.error.value" title="Could not load the archive setting" retryable @retry="providerLoader.refresh" />
      <USkeleton v-else-if="!providerDoc" class="h-16 w-full" />
      <div v-else class="space-y-3">
        <USwitch
          :model-value="archiveOn"
          :disabled="!availability.available || toggle.loading.value"
          :loading="toggle.loading.value"
          label="Archive every object version in restic"
          :description="availability.available ? 'Turns restic on for every write in this bucket, not only for writes that request SSE-S3.' : availability.reason"
          data-testid="archive-toggle"
          @update:model-value="onToggle"
        />
        <UAlert
          v-if="availability.available"
          color="warning"
          variant="subtle"
          icon="i-lucide-triangle-alert"
          title="Cost"
          :description="ARCHIVE_COST_WARNING"
          data-testid="archive-cost"
        />
        <p class="text-xs text-muted">
          Never forget or prune per-version archive snapshots (tag <code class="font-mono">{{ tags.object }}</code>) by hand: gos3 reads them while the objects exist.
        </p>
      </div>
    </UCard>

    <!-- Recovery -->
    <UCard data-testid="restic-recovery">
      <template #header>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h3 class="font-semibold text-highlighted">
              Recovery
            </h3>
            <p class="text-sm text-muted">
              Where this bucket’s restic repositories are, and how to read them with the restic CLI when gos3 is not running.
            </p>
          </div>
          <div class="flex items-center gap-2">
            <UFieldGroup size="xs" aria-label="Shell">
              <UButton :color="shell === 'bash' ? 'primary' : 'neutral'" :variant="shell === 'bash' ? 'subtle' : 'outline'" label="bash" :aria-pressed="shell === 'bash'" @click="shell = 'bash'" />
              <UButton :color="shell === 'powershell' ? 'primary' : 'neutral'" :variant="shell === 'powershell' ? 'subtle' : 'outline'" label="PowerShell" :aria-pressed="shell === 'powershell'" @click="shell = 'powershell'" />
            </UFieldGroup>
            <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-rotate-cw" aria-label="Reload recovery information" :loading="resticLoader.loading.value" @click="resticLoader.refresh" />
          </div>
        </div>
      </template>
      <UAlert
        v-if="resticUnavailable === 'denied'"
        color="neutral"
        variant="subtle"
        icon="i-lucide-lock"
        title="Not allowed"
        description="The recovery information needs admin:GetBucketEncryption on this bucket."
        data-testid="recovery-denied"
      />
      <UAlert
        v-else-if="resticUnavailable === 'unsupported'"
        color="neutral"
        variant="subtle"
        icon="i-lucide-info"
        title="Not available"
        description="This server does not report restic recovery information."
      />
      <CommonErrorAlert v-else-if="resticLoader.error.value" :error="resticLoader.error.value" title="Could not load the recovery information" retryable @retry="resticLoader.refresh" />
      <USkeleton v-else-if="!info" class="h-32 w-full" />
      <div v-else class="space-y-4">
        <section aria-label="Archive repositories" class="space-y-2">
          <h4 class="text-sm font-semibold text-highlighted">
            Archive repositories
          </h4>
          <p v-if="!archiveCopies.length" class="text-sm text-muted">
            The server reported no repository.
          </p>
          <template v-for="c in archiveCopies" :key="`${c.role}:${c.backing}`">
            <ResticRepository
              v-if="c.info"
              :info="c.info"
              :bucket="bucket"
              purpose="archive"
              :tags="tags"
              :shell="shell"
              :backing="c.backing"
              :role="roleLabel(c.role)"
              :can-reveal="access.reveal"
              :server="serverUrl"
              :revealing="revealing === `archive:${c.backing}`"
              @reveal="onReveal(c.backing, 'archive')"
            />
            <p v-else class="flex items-start gap-1.5 rounded-lg border border-default p-3 text-sm text-muted" :data-testid="`restic-repo-none-${c.backing}`">
              <UIcon name="i-lucide-archive-x" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
              <span><span class="font-medium text-highlighted">{{ c.backing }}</span> ({{ roleLabel(c.role).toLowerCase() }}): {{ c.error || 'no repository' }}</span>
            </p>
          </template>
        </section>
        <UAlert
          v-if="info.snapshotsError"
          color="warning"
          variant="subtle"
          icon="i-lucide-triangle-alert"
          title="Snapshot repository unavailable"
          :description="info.snapshotsError"
          data-testid="snapshots-repo-error"
        />
        <section v-if="snapshotRepo()" aria-label="Snapshot repository" class="space-y-2">
          <h4 class="text-sm font-semibold text-highlighted">
            Whole-bucket snapshot repository
          </h4>
          <ResticRepository
            :info="snapshotRepo()!"
            :bucket="bucket"
            purpose="snapshots"
            :tags="tags"
            :shell="shell"
            :can-reveal="access.reveal"
            :server="serverUrl"
            :revealing="revealing === 'snapshots:'"
            @reveal="onReveal(undefined, 'snapshots')"
          />
        </section>
        <div class="space-y-1 text-xs text-muted" data-testid="restic-cli">
          <p>
            The commands contain no secrets. Get the password with “Reveal credentials…”<template v-if="!access.reveal">
              (needs admin:GetResticCredentials)
            </template>, or with the gos3 CLI, online with your gos3 access key (<code class="font-mono">--access-key</code>/<code class="font-mono">--secret-key</code>, or <code class="font-mono">AWS_ACCESS_KEY_ID</code>/<code class="font-mono">AWS_SECRET_ACCESS_KEY</code>):
          </p>
          <p class="flex items-center gap-1">
            <code class="min-w-0 truncate font-mono">{{ cliOnline }}</code><CommonCopyButton :value="cliOnline" aria-label="Copy the online gos3 command" />
          </p>
          <p>or offline, on the server:</p>
          <p class="flex items-center gap-1">
            <code class="min-w-0 truncate font-mono">{{ cliOffline }}</code><CommonCopyButton :value="cliOffline" aria-label="Copy the offline gos3 command" />
          </p>
        </div>
      </div>
    </UCard>

    <ResticSnapshotsCard
      :bucket="bucket"
      :access="access"
      :archive-on="archiveOn"
      :needs-backing="needsBacking"
      :server-id="serverId"
      @changed="resticLoader.refresh(); emit('changed')"
    />
  </div>
</template>
