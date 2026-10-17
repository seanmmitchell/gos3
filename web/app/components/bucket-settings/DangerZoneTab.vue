<script setup lang="ts">
import { S3Error } from '~/utils/s3/errors'
import { canForceDeleteBucket, remoteAccess } from '~/components/admin/-capabilities'
import { BYPASS_GOVERNANCE_HELP, FORCE_DELETE_LOCKED_TITLE, forceDeleteBlock, type ForceDeleteBlock } from './-force-delete'

/**
 * Delete the bucket (S3 DeleteBucket, must be empty) or, for gos3 admins
 * with admin:ForceDeleteBucket, force-delete it with every version,
 * delete marker, pending upload and configuration. Object lock is honoured:
 * the server refuses (409) while versions are under legal hold or COMPLIANCE
 * retention, and under GOVERNANCE retention unless "Bypass governance
 * retention" is ticked; the refusal is shown in the card.
 *
 * A native bucket of an s3 backing keeps its objects in a remote bucket:
 * deleting them deletes them there. It can also be released (POST
 * /buckets/{name}/release, admin:ReleaseBucket): gos3 forgets it and the
 * remote bucket and its objects stay untouched.
 */
const props = defineProps<{
  bucket: string
  /** s3 backings: the bucket kind (native or wrapped), when known. */
  kind?: string
  /** s3 backings: the remote bucket of a native bucket, e.g. "s3://acme-photos/". */
  location?: string
}>()

const router = useRouter()
const { id, connection, requireS3, requireAdmin } = useConnection()
const { capabilities } = useWhoAmI()
const confirm = useConfirm()
const toast = useToast()

const canForce = computed(() => canForceDeleteBucket(connection.value?.kind, capabilities.value))
const native = computed(() => props.kind === 'native')
const canRelease = computed(() => native.value && remoteAccess(connection.value?.kind, capabilities.value).release)
/** "the remote bucket s3://acme-photos/" (or a generic phrase without a location). */
const remote = computed(() => (props.location ? `the remote bucket ${props.location}` : 'its remote bucket'))
const notEmpty = ref(false)

async function done(what = 'deleted') {
  toast.add({ title: `Bucket ${props.bucket} ${what}`, color: 'success', icon: 'i-lucide-circle-check' })
  if (id.value) await router.push(appRoutes.buckets(id.value))
}

const del = useAsyncAction(async () => {
  try {
    await requireS3().deleteBucket(props.bucket)
  } catch (e) {
    if (e instanceof S3Error && e.code === 'BucketNotEmpty') notEmpty.value = true
    throw e
  }
}, { errorTitle: 'Could not delete bucket', rethrow: true })

async function onDelete() {
  const ok = await confirm({
    title: `Delete bucket ${props.bucket}?`,
    description: native.value
      ? `The bucket must be empty (no objects, versions or delete markers). If gos3 created ${remote.value}, it is deleted too; an adopted remote bucket is kept. This cannot be undone.`
      : 'The bucket must be empty (no objects, versions or delete markers). This cannot be undone.',
    confirmLabel: 'Delete bucket',
    requireText: props.bucket,
    icon: 'i-lucide-trash-2'
  })
  if (!ok) return
  try {
    await del.run()
    await done()
  } catch {
    // toast shown by useAsyncAction
  }
}

const bypassGovernance = ref(false)
const lockBlock = ref<ForceDeleteBlock | undefined>()

const force = useAsyncAction(async () => {
  lockBlock.value = undefined
  try {
    await requireAdmin().deleteBucket(props.bucket, { force: true, bypassGovernanceRetention: bypassGovernance.value })
  } catch (e) {
    lockBlock.value = forceDeleteBlock(e)
    throw e
  }
}, { errorTitle: 'Could not force-delete bucket', rethrow: true })

async function onForceDelete() {
  const governance = bypassGovernance.value ? ', including versions under GOVERNANCE retention' : ''
  const ok = await confirm({
    title: `Force delete ${props.bucket}?`,
    description: native.value
      ? `Every object, every version and all delete markers in ${remote.value} will be permanently deleted${governance}, also for other tools using it, then pending uploads and the bucket's configuration. A remote bucket gos3 created is deleted when empty; an adopted one is kept (empty).`
      : `Every object, every version, all delete markers, pending multipart uploads and all bucket configuration will be permanently deleted${governance}.`,
    confirmLabel: 'Delete everything',
    requireText: props.bucket,
    icon: 'i-lucide-bomb'
  })
  if (!ok) return
  try {
    await force.run()
    await done()
  } catch {
    // toast shown by useAsyncAction
  }
}

const release = useAsyncAction(() => requireAdmin().releaseBucket(props.bucket), { errorTitle: 'Could not release the bucket', rethrow: true })

async function onRelease() {
  const ok = await confirm({
    title: `Release ${props.bucket}?`,
    description: `gos3 forgets the bucket and the settings it keeps for it (policy, CORS, …). Nothing remote is deleted: ${remote.value} and its objects stay as they are, other tools keep working with them, and an administrator can adopt it again.`,
    confirmLabel: 'Release bucket',
    color: 'warning',
    icon: 'i-lucide-unlink'
  })
  if (!ok) return
  try {
    await release.run()
    await done('released')
  } catch {
    // toast shown by useAsyncAction
  }
}
</script>

<template>
  <UCard :ui="{ root: 'ring-error/40' }">
    <template #header>
      <h3 class="font-semibold text-error">
        Danger zone
      </h3>
    </template>
    <div class="divide-y divide-default">
      <div class="flex flex-col gap-3 pb-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <p class="font-medium text-highlighted">
            Delete bucket
          </p>
          <p class="text-sm text-muted">
            Only empty buckets can be deleted with the S3 API.<template v-if="native">
              A remote bucket gos3 created is deleted with it; an adopted one is kept.
            </template>
          </p>
          <p v-if="notEmpty" class="mt-1 text-sm text-warning" role="status">
            The bucket is not empty. Delete its objects{{ canForce ? ' or use force delete' : '' }} first.
          </p>
        </div>
        <UButton color="error" variant="soft" icon="i-lucide-trash-2" label="Delete bucket" :loading="del.loading.value" @click="onDelete" />
      </div>
      <div v-if="canRelease" class="flex flex-col gap-3 py-4 sm:flex-row sm:items-center sm:justify-between" data-testid="release-bucket">
        <div>
          <p class="font-medium text-highlighted">
            Release bucket
          </p>
          <p class="text-sm text-muted">
            gos3 forgets this native bucket; {{ remote }} and its objects are not touched (admin:ReleaseBucket). Remove replicas first.
          </p>
        </div>
        <UButton color="warning" variant="soft" icon="i-lucide-unlink" label="Release bucket" :loading="release.loading.value" @click="onRelease" />
      </div>
      <div v-if="canForce" class="flex flex-col gap-3 pt-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <p class="font-medium text-highlighted">
            Force delete
          </p>
          <p class="text-sm text-muted">
            Removes all versions, delete markers, uploads and configuration, then the bucket (admin:ForceDeleteBucket).
          </p>
          <p v-if="native" class="mt-1 text-sm text-warning" data-testid="force-delete-native">
            This is a native bucket: its objects are deleted in {{ remote }}, also for other tools using it. A remote bucket gos3 created is deleted too; an adopted one is kept.
          </p>
          <UCheckbox v-model="bypassGovernance" class="mt-2" label="Bypass governance retention" :description="BYPASS_GOVERNANCE_HELP" data-testid="bypass-governance" />
          <UAlert
            v-if="lockBlock"
            class="mt-2"
            color="warning"
            variant="subtle"
            icon="i-lucide-lock"
            :title="FORCE_DELETE_LOCKED_TITLE"
            :description="lockBlock.governance && !bypassGovernance ? `${lockBlock.message} Tick “Bypass governance retention” to delete them.` : lockBlock.message"
            data-testid="force-delete-locked"
          />
        </div>
        <UButton color="error" icon="i-lucide-bomb" label="Force delete" :loading="force.loading.value" @click="onForceDelete" />
      </div>
    </div>
  </UCard>
</template>
