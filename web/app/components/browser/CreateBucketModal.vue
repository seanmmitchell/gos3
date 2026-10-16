<script setup lang="ts">
/**
 * "Create bucket" dialog: S3 name rules with live hints, region, storage
 * backing (gos3 servers with several backings), object lock and versioning
 * (PutBucketVersioning right after CreateBucket). On an s3 backing it asks
 * for the bucket kind (x-gos3-bucket-kind, the backing's default
 * preselected): wrapped buckets pick a container (x-gos3-container, the
 * default container preselected), native buckets the remote bucket to create
 * (x-gos3-remote-bucket, default the bucket name). Versioning and object lock
 * of a native bucket are set on its remote bucket (the server answers 501
 * when that service cannot). Identities that may adopt remote buckets get a
 * link to the backing page, which opens the adopt dialog prefilled. Opening
 * the dialog refreshes a whoami older than WHOAMI_FRESH_MS, so the offered
 * kinds and containers are current.
 *
 *   <BrowserCreateBucketModal v-model:open="open" :default-region="conn.region" :backings="whoami?.backings" @created="refresh" />
 *
 * Pass `backings` for gos3 connections only: the x-gos3-backing header is
 * sent only when the selector is shown.
 */
import type { AvailableBackings, BucketKind } from '~/types/admin'
import type { S3Client } from '~/utils/s3/client'
import { useConnectionsStore } from '~/stores/connections'

/** Age (ms) up to which the cached whoami (backings, containers) is used as is. */
const WHOAMI_FRESH_MS = 5000

const open = defineModel<boolean>('open', { required: true })

const props = defineProps<{
  client: S3Client | undefined
  defaultRegion?: string
  existingNames?: string[]
  /** gos3 WhoAmI.backings; the selector is shown when it offers a choice. */
  backings?: AvailableBackings | null
  /** Connection ID, for the "Adopt an existing remote bucket…" link. */
  serverId?: string
  /**
   * The identity may adopt remote buckets and open the backing page
   * (WhoAmI capabilities adoptRemote and viewStorage).
   */
  canAdopt?: boolean
}>()

const emit = defineEmits<{ created: [name: string] }>()

const name = ref('')
const region = ref('')
const objectLock = ref(false)
const versioning = ref(false)
const touched = ref(false)
const busy = ref(false)
const error = ref<unknown>(null)
const warning = ref('')

// Storage backing: the server default until the user picks one (whoami may
// arrive while the dialog is open).
const backingItems = computed(() => backingSelectItems(props.backings))
const pickedBacking = ref<string | null>(null)
const backing = computed<string>({
  get: () => pickedBacking.value ?? (backingItems.value.length ? defaultBacking(props.backings) : ''),
  set: (v) => {
    pickedBacking.value = v
  }
})
/** The backing the bucket is created on: the chosen one, else the server's default. */
const selectedBacking = computed(() => findBacking(backing.value || defaultBacking(props.backings), props.backings))
const backingHelp = computed(() => backingKindInfo(selectedBacking.value).description)

// Bucket kind (s3 backings): the backing's default until the user picks one.
const kindItems = computed(() => kindChoices(selectedBacking.value))
const pickedKind = ref<BucketKind | null>(null)
const kind = computed<BucketKind | undefined>({
  get: () => (pickedKind.value && kindItems.value.some(k => k.value === pickedKind.value) ? pickedKind.value : defaultBucketKind(selectedBacking.value) || undefined),
  set: (v) => {
    pickedKind.value = v ?? null
  }
})

// Container of a wrapped bucket: the backing's default until the user picks one.
const containerChoices = computed(() => containerItems(selectedBacking.value))
const defaultContainerId = computed(() => defaultContainer(selectedBacking.value) || containerChoices.value[0]?.value || '')
const pickedContainer = ref<string | null>(null)
const container = computed<string>({
  get: () => (pickedContainer.value && containerChoices.value.some(c => c.value === pickedContainer.value) ? pickedContainer.value : defaultContainerId.value),
  set: (v) => {
    pickedContainer.value = v
  }
})
const noContainer = computed(() => kind.value === 'wrapped' && !containerChoices.value.length)

// Remote bucket of a native bucket: follows the bucket name until edited.
const remoteName = ref('')
const remoteEdited = ref(false)
const remoteBucket = computed<string>({
  get: () => (remoteEdited.value ? remoteName.value : name.value),
  set: (v) => {
    remoteName.value = v
    remoteEdited.value = true
  }
})
const remoteError = computed(() => (kind.value === 'native' && remoteBucket.value ? validateBucketName(remoteBucket.value) : undefined))

/** Link to the backing's remote bucket browser, opening "Adopt by name…" prefilled. */
const adoptLink = computed(() => {
  const b = selectedBacking.value
  if (!props.canAdopt || !props.serverId || !b || !kindItems.value.some(k => k.value === 'native')) return undefined
  return appRoutes.backing(props.serverId, b.name, 'remote', { adopt: kind.value === 'native' ? remoteBucket.value : '', bucket: name.value })
})

watch(open, (v) => {
  if (!v) return
  name.value = ''
  region.value = props.defaultRegion ?? ''
  pickedBacking.value = null
  pickedKind.value = null
  pickedContainer.value = null
  remoteName.value = ''
  remoteEdited.value = false
  objectLock.value = false
  versioning.value = false
  touched.value = false
  error.value = null
  warning.value = ''
})

// Kinds and containers come from whoami: refresh it when it is not fresh.
const store = useConnectionsStore()
const { id: connectionId } = useConnection()
watch(open, (v) => {
  if (v && props.backings && connectionId.value) void store.ensureStatus(connectionId.value, WHOAMI_FRESH_MS)
}, { immediate: true })

// Object lock requires versioning.
watch(objectLock, (v) => {
  if (v) versioning.value = true
})

const rules = computed(() => {
  const n = name.value
  return [
    { label: '3–63 characters', ok: n.length >= 3 && n.length <= 63 },
    { label: 'Lowercase letters, digits, dots (.) and hyphens (-) only', ok: n.length > 0 && /^[a-z0-9.-]+$/.test(n) },
    { label: 'Starts and ends with a letter or digit', ok: /^[a-z0-9]/.test(n) && /[a-z0-9]$/.test(n) },
    { label: 'No two adjacent dots, not an IP address', ok: n.length > 0 && !n.includes('..') && !/^\d{1,3}(\.\d{1,3}){3}$/.test(n) }
  ]
})

const nameError = computed(() => {
  if (!name.value) return touched.value ? 'Enter a bucket name' : undefined
  const e = validateBucketName(name.value)
  if (e) return e
  if (props.existingNames?.includes(name.value)) return 'You already have a bucket with this name'
  return undefined
})

const canSubmit = computed(() => !!name.value && !nameError.value && !remoteError.value && !noContainer.value && !busy.value && !!props.client)

async function submit() {
  touched.value = true
  if (!canSubmit.value || !props.client) return
  busy.value = true
  error.value = null
  warning.value = ''
  const bucket = name.value
  const k = kindItems.value.length ? kind.value : undefined
  try {
    await props.client.createBucket(bucket, {
      region: region.value.trim() || undefined,
      objectLock: objectLock.value,
      backing: backingItems.value.length ? backing.value : undefined,
      kind: k,
      // The remote name only when it differs from the bucket name (the server's default).
      remoteBucket: k === 'native' && remoteBucket.value && remoteBucket.value !== bucket ? remoteBucket.value : undefined,
      // Always the shown container: whoami's default flag may be stale.
      container: k === 'wrapped' && container.value ? container.value : undefined
    })
  } catch (e) {
    error.value = e
    busy.value = false
    return
  }
  if (versioning.value && !objectLock.value) {
    try {
      await props.client.putBucketVersioning(bucket, 'Enabled')
    } catch (e) {
      warning.value = `The bucket was created, but versioning could not be enabled: ${errorMessage(e)}`
    }
  }
  busy.value = false
  useToast().add({ title: `Bucket ${bucket} created`, color: 'success', icon: 'i-lucide-circle-check' })
  emit('created', bucket)
  if (warning.value) {
    useToast().add({ title: 'Versioning not enabled', description: warning.value, color: 'warning', icon: 'i-lucide-triangle-alert' })
  }
  open.value = false
}
</script>

<template>
  <UModal
    v-model:open="open"
    title="Create bucket"
    description="Buckets hold objects. Names are global on the server and cannot be changed later."
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <form id="create-bucket-form" class="space-y-5" novalidate @submit.prevent="submit">
        <UFormField label="Bucket name" :error="nameError" required>
          <UInput
            v-model.trim="name"
            class="w-full font-mono"
            placeholder="my-bucket"
            autocomplete="off"
            autofocus
            spellcheck="false"
            :aria-invalid="!!nameError"
            @blur="touched = true"
          />
        </UFormField>
        <ul class="-mt-3 space-y-0.5 text-xs" aria-label="Bucket name rules">
          <li v-for="r in rules" :key="r.label" class="flex items-center gap-1.5" :class="r.ok ? 'text-success' : name ? 'text-error' : 'text-muted'">
            <UIcon :name="r.ok ? 'i-lucide-check' : name ? 'i-lucide-x' : 'i-lucide-dot'" class="size-3.5 shrink-0" aria-hidden="true" />
            <span>{{ r.label }}<span class="sr-only">{{ r.ok ? ' (satisfied)' : ' (not satisfied)' }}</span></span>
          </li>
        </ul>

        <UFormField label="Region" help="Leave empty to use the server's default region.">
          <UInput v-model.trim="region" class="w-full font-mono" placeholder="us-east-1" autocomplete="off" />
        </UFormField>

        <UFormField v-if="backingItems.length" label="Storage backing" :help="backingHelp">
          <USelect v-model="backing" :items="backingItems" value-key="value" class="w-full" />
        </UFormField>

        <div v-if="kindItems.length" class="space-y-3 rounded-lg border border-default p-3" data-testid="bucket-kind">
          <URadioGroup v-model="kind" legend="Bucket kind" :items="kindItems" />
          <UFormField v-if="kind === 'wrapped'" label="Container" help="Where the bucket is stored in gos3's format; many buckets can share a container.">
            <USelect v-if="containerChoices.length" v-model="container" :items="containerChoices" value-key="value" class="w-full" aria-label="Container" />
            <p v-else class="flex items-start gap-1.5 text-sm text-warning" role="status" data-testid="no-container">
              <UIcon name="i-lucide-package-x" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
              <span>This backing has no container; an administrator can add one. Choose native instead, or ask for a container.</span>
            </p>
          </UFormField>
          <UFormField
            v-else-if="kind === 'native'"
            label="Remote bucket"
            help="Created in the backing's account for this bucket; defaults to the bucket name."
            :error="remoteError"
          >
            <UInput
              v-model.trim="remoteBucket"
              class="w-full font-mono"
              :placeholder="name || 'my-bucket'"
              autocomplete="off"
              spellcheck="false"
              aria-label="Remote bucket"
              :aria-invalid="!!remoteError"
            />
          </UFormField>
          <ULink v-if="adoptLink" :to="adoptLink" class="inline-flex items-center gap-1 text-sm text-primary hover:underline" @click="open = false">
            <UIcon name="i-lucide-cloud-download" class="size-4" aria-hidden="true" />
            Adopt an existing remote bucket…
          </ULink>
        </div>

        <div class="space-y-3 rounded-lg border border-default p-3">
          <USwitch
            v-model="versioning"
            label="Versioning"
            description="Keep every version of every object; deletes create delete markers."
            :disabled="objectLock"
          />
          <USwitch
            v-model="objectLock"
            label="Object lock"
            description="Allows WORM retention and legal holds. Can only be enabled at creation and implies versioning."
          />
          <p v-if="kind === 'native'" class="flex items-start gap-1.5 text-xs text-muted">
            <UIcon name="i-lucide-info" class="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span>Versioning and object lock are set on the remote bucket; the server refuses them (Not Implemented) when its S3 service cannot.</span>
          </p>
        </div>

        <CommonErrorAlert v-if="error" :error="error" title="Could not create the bucket" />
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="open = false" />
      <UButton type="submit" form="create-bucket-form" icon="i-lucide-plus" label="Create bucket" :loading="busy" :disabled="!canSubmit" />
    </template>
  </UModal>
</template>
