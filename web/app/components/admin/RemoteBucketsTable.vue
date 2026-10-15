<script setup lang="ts">
import type { Backing, RemoteBucket, RemoteBucketList } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { remoteAccess, type RemoteAccess } from './-capabilities'
import {
  REMOTE_PAGE_SIZE,
  describeUse,
  incompleteListMessage,
  nativeBucketOf,
  remoteBucketActions,
  remoteNameNote,
  remoteStatusMeta,
  type RemoteBucketAction
} from './-remote-buckets'
import AddContainerModal from './AddContainerModal.vue'
import AdoptRemoteBucketModal from './AdoptRemoteBucketModal.vue'
import CreateRemoteBucketModal from './CreateRemoteBucketModal.vue'
import CreateWrappedBucketModal from './CreateWrappedBucketModal.vue'

/**
 * The buckets of an s3 backing's account (GET
 * /backings/{name}/remote-buckets, admin:ListRemoteBuckets), page by page,
 * with a name prefix filter and an optional check of unmanaged buckets for
 * gos3 data. Each bucket shows what it is to gos3 (status) and its uses,
 * linking to the gos3 bucket of a native one; its actions depend on the
 * status and the identity's capabilities: adopt, use as a container, create a
 * wrapped bucket in it, attach a gos3 store, release a native bucket. The
 * toolbar creates remote buckets and adopts buckets by name (also when the
 * credentials cannot list the account's buckets).
 *
 * The capabilities are evaluated for all backings; a grant scoped to this
 * backing (arn:gos3:admin:::backing/<name>) does not show in them. So a 403
 * from the list is shown as "no access" here, and once the list loads the
 * actions are offered even without the capabilities (the server's 403 is
 * shown if one is refused).
 */
const props = defineProps<{
  backing: Backing
  /** Connection ID, for links to gos3 buckets. */
  serverId: string
}>()

const emit = defineEmits<{
  /** gos3 buckets or containers changed: the backing should be reloaded. */
  changed: []
  /** Show another tab of the backing page. */
  navigate: [tab: 'containers']
}>()

const { connection, requireAdmin } = useConnection()
const { capabilities } = useWhoAmI()
const overlay = useOverlay()
const confirm = useConfirm()
const captionId = useId()

// --- listing ----------------------------------------------------------------------------

const prefix = ref('')
const appliedPrefix = ref('')
const probe = ref(false)
/** Continuation tokens of the pages shown so far ('' = the first page). */
const tokens = ref<string[]>([''])
const page = computed(() => tokens.value.length - 1)

const list = ref<RemoteBucketList | null>(null)
const loading = ref(false)
const error = ref<unknown>(null)
let seq = 0

async function load() {
  const my = ++seq
  loading.value = true
  error.value = null
  try {
    const r = await requireAdmin().listRemoteBuckets(props.backing.name, {
      prefix: appliedPrefix.value,
      continuationToken: tokens.value[page.value] || undefined,
      maxBuckets: REMOTE_PAGE_SIZE,
      probe: probe.value
    })
    if (my === seq) list.value = r
  } catch (e) {
    if (my === seq && !isAbortedError(e)) error.value = e
  } finally {
    if (my === seq) loading.value = false
  }
}

/** Back to the first page with the current filter and probe setting. */
function reload() {
  tokens.value = ['']
  void load()
}

function applyFilter() {
  appliedPrefix.value = prefix.value.trim()
  reload()
}

function clearFilter() {
  prefix.value = ''
  applyFilter()
}

function nextPage() {
  const t = list.value?.continuationToken
  if (!t) return
  tokens.value = [...tokens.value, t]
  void load()
}

function previousPage() {
  if (page.value === 0) return
  tokens.value = tokens.value.slice(0, -1)
  void load()
}

watch(probe, reload)
watch(() => props.backing.name, () => {
  list.value = null
  reload()
}, { immediate: true })

const buckets = computed(() => list.value?.buckets ?? [])
const incomplete = computed(() => incompleteListMessage(list.value))
const listDenied = computed(() => error.value instanceof AdminError && error.value.isAccessDenied)

/**
 * What the identity may do here: its capabilities, or — once this
 * backing's remote buckets could be listed (perhaps by a grant scoped to the
 * backing) — every action, refused by the server where not allowed.
 */
const access = computed<RemoteAccess>(() => {
  const caps = remoteAccess(connection.value?.kind, capabilities.value)
  return list.value && !error.value ? { ...caps, browse: true, create: true, adopt: true, containers: true } : caps
})

// --- actions --------------------------------------------------------------------------

/** Reloads the list and tells the page (bucket counts, containers). */
function afterChange() {
  emit('changed')
  void load()
}

/** Adopts a remote bucket of the list (fixed) or one typed by name. */
async function adopt(opts: { remoteBucket?: string, fixed?: boolean, region?: string, bucket?: string } = {}) {
  const name = await overlay.create(AdoptRemoteBucketModal, { destroyOnClose: true }).open({
    backing: props.backing.name,
    remoteBucket: opts.remoteBucket ?? '',
    fixed: !!opts.fixed,
    region: opts.region,
    bucketName: opts.bucket
  })
  if (name) afterChange()
}

async function createRemote() {
  const name = await overlay.create(CreateRemoteBucketModal, { destroyOnClose: true }).open({ backing: props.backing.name })
  if (name) {
    // Show the new bucket: filter by its name.
    prefix.value = name
    applyFilter()
  }
}

async function addContainer(rb: RemoteBucket, attach: boolean) {
  const id = await overlay.create(AddContainerModal, { destroyOnClose: true }).open({
    backing: props.backing.name,
    access: access.value,
    bucket: rb.name,
    fixed: true,
    region: rb.region,
    attach
  })
  if (id !== null && id !== undefined) afterChange()
}

async function createWrapped(rb: RemoteBucket) {
  const name = await overlay.create(CreateWrappedBucketModal, { destroyOnClose: true }).open({
    backing: props.backing.name,
    containers: props.backing.account?.containers ?? [],
    defaultContainer: props.backing.account?.defaultContainer,
    remoteBucket: rb.name,
    region: rb.region,
    // A failed request may still have added the container.
    onChanged: afterChange
  })
  if (name) afterChange()
}

const releasing = ref<string | null>(null)

const release = useAsyncAction(
  (bucket: string) => requireAdmin().releaseBucket(bucket),
  { successMessage: bucket => `Bucket ${bucket} released`, errorTitle: 'Could not release the bucket' }
)

async function onRelease(rb: RemoteBucket) {
  const bucket = nativeBucketOf(rb)
  if (!bucket) return
  const ok = await confirm({
    title: `Release bucket ${bucket}?`,
    description: `gos3 forgets the native bucket ${bucket} and the settings it keeps for it (policy, CORS, …). Nothing remote is deleted: the remote bucket ${rb.name} and its objects stay as they are, other tools keep working with them, and it can be adopted again.`,
    confirmLabel: 'Release bucket',
    color: 'warning',
    icon: 'i-lucide-unlink'
  })
  if (!ok) return
  releasing.value = rb.name
  try {
    await release.run(bucket)
    if (!release.error.value) afterChange()
  } finally {
    releasing.value = null
  }
}

function run(action: RemoteBucketAction, rb: RemoteBucket) {
  switch (action) {
    case 'release': void onRelease(rb); break
    case 'adopt': void adopt({ remoteBucket: rb.name, fixed: true, region: rb.region }); break
    case 'use-as-container': void addContainer(rb, false); break
    case 'attach-container': void addContainer(rb, true); break
    case 'create-wrapped': void createWrapped(rb); break
    case 'containers': emit('navigate', 'containers'); break
  }
}

function rowMenu(rb: RemoteBucket) {
  const bucket = nativeBucketOf(rb)
  return [remoteBucketActions(rb, access.value).map(a => (a.action === 'open' && bucket
    ? { label: a.label, icon: a.icon, to: appRoutes.bucket(props.serverId, bucket) }
    : { label: a.label, icon: a.icon, color: a.color, onSelect: () => run(a.action, rb) }))]
}

</script>

<template>
  <UCard :ui="{ body: 'p-0 sm:p-0' }">
    <template #header>
      <div class="space-y-3">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h2 :id="captionId" class="font-semibold text-highlighted">
              Remote buckets
            </h2>
            <p class="text-sm text-muted">
              The buckets of this backing's account and what gos3 uses them for.
            </p>
          </div>
          <div class="flex flex-wrap gap-2">
            <UButton v-if="access.create" color="neutral" variant="outline" icon="i-lucide-plus" label="Create remote bucket…" @click="createRemote" />
            <UButton v-if="access.adopt" icon="i-lucide-cloud-download" label="Adopt by name…" @click="adopt()" />
          </div>
        </div>
        <div class="flex flex-col gap-3 sm:flex-row sm:items-center">
          <form class="flex w-full gap-2 sm:max-w-sm" role="search" @submit.prevent="applyFilter">
            <UInput
              v-model.trim="prefix"
              icon="i-lucide-search"
              placeholder="Name starts with…"
              aria-label="Filter remote buckets by name prefix"
              class="w-full"
              autocomplete="off"
              spellcheck="false"
            />
            <UButton type="submit" color="neutral" variant="outline" label="Filter" />
            <UButton v-if="appliedPrefix" color="neutral" variant="ghost" icon="i-lucide-x" aria-label="Clear filter" @click="clearFilter" />
          </form>
          <USwitch v-model="probe" label="Check for gos3 data" description="One extra request per unmanaged bucket." size="sm" />
          <UButton class="sm:ms-auto" color="neutral" variant="ghost" icon="i-lucide-rotate-cw" aria-label="Reload remote buckets" :loading="loading" @click="load" />
        </div>
      </div>
    </template>

    <div v-if="incomplete || error" class="space-y-3 p-4 sm:px-6">
      <UAlert
        v-if="listDenied"
        color="neutral"
        variant="subtle"
        icon="i-lucide-lock"
        title="No access to this backing's remote buckets"
        :description="`Listing them needs admin:ListRemoteBuckets on arn:gos3:admin:::backing/${backing.name}.${access.adopt ? ' You can still adopt a remote bucket by name.' : ''}`"
        data-testid="remote-denied"
      />
      <UAlert
        v-if="incomplete"
        color="warning"
        variant="subtle"
        icon="i-lucide-list-x"
        title="Incomplete list"
        :description="incomplete"
        :actions="access.adopt ? [{ label: 'Adopt by name…', color: 'neutral', variant: 'outline', onClick: () => adopt() }] : undefined"
        data-testid="remote-incomplete"
      />
      <CommonErrorAlert v-if="!listDenied" :error="error" title="Could not list the remote buckets" retryable @retry="load" />
    </div>

    <div v-if="!list && loading" class="space-y-2 p-4" aria-busy="true" aria-label="Loading remote buckets">
      <USkeleton v-for="i in 4" :key="i" class="h-10 w-full" />
    </div>

    <div v-else-if="list" class="overflow-x-auto">
      <table class="w-full text-sm" :aria-labelledby="captionId" :aria-busy="loading">
        <thead class="border-b border-default text-left text-xs text-muted">
          <tr>
            <th scope="col" class="px-4 py-2 font-medium">
              Remote bucket
            </th>
            <th scope="col" class="px-4 py-2 font-medium">
              Status
            </th>
            <th scope="col" class="px-4 py-2 font-medium">
              Used for
            </th>
            <th scope="col" class="px-4 py-2 font-medium">
              Created
            </th>
            <th scope="col" class="w-0 px-4 py-2">
              <span class="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody class="divide-y divide-default">
          <tr v-if="!buckets.length">
            <td colspan="5" class="px-4 py-8 text-center text-muted">
              {{ appliedPrefix ? `No remote bucket name starts with “${appliedPrefix}”.` : 'No remote buckets.' }}
            </td>
          </tr>
          <tr v-for="rb in buckets" :key="rb.name" :data-testid="`remote-${rb.name}`">
            <th scope="row" class="px-4 py-2.5 text-left font-normal">
              <span class="font-mono text-highlighted">{{ rb.name }}</span>
              <span v-if="rb.region" class="block text-xs text-muted">{{ rb.region }}</span>
            </th>
            <td class="px-4 py-2.5">
              <div class="space-y-1">
                <UTooltip :text="remoteStatusMeta(rb.status).description" :disabled="!remoteStatusMeta(rb.status).description">
                  <UBadge
                    :label="remoteStatusMeta(rb.status).label"
                    :color="remoteStatusMeta(rb.status).color"
                    :icon="remoteStatusMeta(rb.status).icon"
                    variant="subtle"
                    size="sm"
                    data-testid="remote-status"
                  />
                </UTooltip>
                <p v-if="rb.note" class="max-w-xs text-xs text-muted">
                  {{ rb.note }}
                </p>
                <p v-if="(rb.status === 'unmanaged' || rb.status === 'gos3') && remoteNameNote(rb.name)" class="max-w-xs text-xs text-warning" data-testid="remote-name-note">
                  {{ remoteNameNote(rb.name) }}
                </p>
              </div>
            </td>
            <td class="px-4 py-2.5">
              <ul v-if="rb.uses?.length" class="space-y-0.5 text-xs">
                <li v-for="(u, i) in rb.uses" :key="i">
                  <NuxtLink v-if="u.role === 'native' && u.bucket" :to="appRoutes.bucket(serverId, u.bucket)" class="text-primary hover:underline focus-visible:underline">
                    {{ describeUse(u) }}
                  </NuxtLink>
                  <span v-else class="text-muted">{{ describeUse(u) }}</span>
                </li>
              </ul>
              <span v-else class="text-muted">—</span>
            </td>
            <td class="px-4 py-2.5 text-sm">
              <CommonRelativeTime :value="rb.createdAt" class="text-muted" />
            </td>
            <td class="px-4 py-2.5 text-right">
              <UDropdownMenu v-if="rowMenu(rb)[0]!.length" :items="rowMenu(rb)" :content="{ align: 'end' }">
                <UButton
                  icon="i-lucide-ellipsis-vertical"
                  color="neutral"
                  variant="ghost"
                  size="sm"
                  :loading="releasing === rb.name"
                  :aria-label="`Actions for remote bucket ${rb.name}`"
                />
              </UDropdownMenu>
            </td>
          </tr>
        </tbody>
      </table>
      <div v-if="page > 0 || list.continuationToken" class="flex items-center justify-between gap-2 border-t border-default px-4 py-2">
        <UButton color="neutral" variant="outline" size="sm" icon="i-lucide-chevron-left" label="Previous" :disabled="page === 0 || loading" @click="previousPage" />
        <span class="text-xs text-muted">Page {{ page + 1 }}</span>
        <UButton color="neutral" variant="outline" size="sm" trailing-icon="i-lucide-chevron-right" label="Next" :disabled="!list.continuationToken || loading" @click="nextPage" />
      </div>
    </div>
  </UCard>
</template>
