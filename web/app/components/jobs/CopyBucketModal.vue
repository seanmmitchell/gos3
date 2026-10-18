<script setup lang="ts">
import type { BucketKind, CopyJobDryRun, CopyJobSpec, Job } from '~/types/admin'
import { S3Error } from '~/utils/s3/errors'
import {
  MAX_CONCURRENCY,
  TARGET_STEP_FIELDS,
  copyHints,
  copyJobSpec,
  describeSpec,
  emptyCopyForm,
  newTargetBacking,
  stepValid,
  targetKind,
  validateCopyForm,
  versionsItems,
  type CopyContext,
  type CopyForm,
  type ExistingTargetInfo
} from './-copy-job'

/**
 * "Copy / Mirror" wizard for one bucket (the source, fixed): copies it once
 * or continuously (POST /jobs, admin:CreateCopyJob on both buckets).
 *
 * 1. Target: a new bucket (name, backing, and on s3 backings the bucket
 *    kind with its container or remote bucket, object lock) or an existing
 *    one, each with an optional prefix; the source prefix.
 * 2. Options: versions ("all" disabled with the reason when the target
 *    cannot keep them), bucket settings, an existing target's handling,
 *    encryption, retention, compression, mirror options, parallelism.
 * 3. Review: a dry run (POST /jobs with dryRun) shows the normalized job
 *    and the server's warnings; "Create job" then creates it.
 *
 * Opened through useOverlay(); emits close(the new job) once created, or
 * close(null) when cancelled.
 */
const props = defineProps<{
  /** The source bucket. */
  bucket: string
  /** The source's versioning ('Enabled' | 'Suspended' | ''), when the caller knows it. */
  versioning?: string | null
  /** The source has object lock, when the caller knows it. */
  objectLock?: boolean | null
  /** Connection ID, for the "Open jobs" link. */
  serverId?: string
}>()

const emit = defineEmits<{ close: [value: Job | null] }>()

const { requireS3, requireAdmin } = useConnection()
const { whoami } = useWhoAmI()
const toast = useToast()
const formId = useId()

type Step = 'target' | 'options' | 'review'
const STEPS: { value: Step, label: string }[] = [
  { value: 'target', label: 'Target' },
  { value: 'options', label: 'Options' },
  { value: 'review', label: 'Review' }
]
const step = ref<Step>('target')
const stepIndex = computed(() => STEPS.findIndex(s => s.value === step.value))

// --- context: source facts, bucket names, backings, an existing target -------------------

const sourceVersioning = ref<string | null>(props.versioning ?? null)
const sourceLock = ref<boolean | null>(props.objectLock ?? null)
const buckets = ref<string[]>([])
const bucketsLoaded = ref(false)

async function loadContext() {
  const s3 = requireS3()
  const [list, v, lock] = await Promise.all([
    s3.listBuckets().catch(() => null),
    props.versioning === undefined || props.versioning === null ? s3.getBucketVersioning(props.bucket).then(r => r.status as string, () => null) : Promise.resolve(props.versioning),
    props.objectLock === undefined || props.objectLock === null ? s3.getObjectLockConfiguration(props.bucket).then(r => !!r?.enabled, () => null) : Promise.resolve(props.objectLock)
  ])
  buckets.value = (list?.buckets ?? []).map(b => b.name).sort()
  bucketsLoaded.value = !!list
  sourceVersioning.value = v
  sourceLock.value = lock
  // Suggest a free name once the bucket names are known (unless edited).
  if (!nameEdited.value && form.target === 'new') form.targetBucket = emptyCopyForm(ctx.value).targetBucket
  if (!lockEdited.value) form.objectLock = !!lock
}

const targetInfo = ref<ExistingTargetInfo | null>(null)
const targetInfoLoading = ref(false)
let targetSeq = 0
async function loadTargetInfo(name: string) {
  const my = ++targetSeq
  targetInfo.value = null
  if (!name || name === props.bucket || (bucketsLoaded.value && !buckets.value.includes(name))) return
  targetInfoLoading.value = true
  const s3 = requireS3()
  const isNotImplemented = (e: unknown) => e instanceof S3Error && (e.code === 'NotImplemented' || e.status === 501)
  let unsupported = false
  const [head, v, lock] = await Promise.all([
    s3.headBucket(name).catch(() => null),
    s3.getBucketVersioning(name).then(r => r.status as string, (e: unknown) => {
      if (isNotImplemented(e)) unsupported = true
      return null
    }),
    s3.getObjectLockConfiguration(name).then(r => !!r?.enabled, () => null)
  ])
  if (my !== targetSeq) return
  targetInfo.value = { kind: head?.kind, versioning: v, versioningUnsupported: unsupported, objectLock: lock }
  targetInfoLoading.value = false
}

const ctx = computed<CopyContext>(() => ({
  source: { bucket: props.bucket, versioning: sourceVersioning.value, objectLock: sourceLock.value },
  buckets: buckets.value,
  backings: whoami.value?.backings ?? null,
  target: form.target === 'existing' ? targetInfo.value : null
}))

// --- form ------------------------------------------------------------------------------

const form = reactive<CopyForm>(emptyCopyForm({ source: { bucket: props.bucket, versioning: props.versioning ?? null, objectLock: props.objectLock ?? null }, buckets: [] }))
const nameEdited = ref(false)
const lockEdited = ref(false)
const lastNewName = ref(form.targetBucket)

void loadContext()

watch(() => form.target, (t, prev) => {
  if (t === prev) return
  if (t === 'existing') {
    lastNewName.value = form.targetBucket
    form.targetBucket = ''
  } else {
    form.targetBucket = lastNewName.value
    form.onExisting = 'fail'
  }
})
watch(() => [form.target, form.targetBucket] as const, ([t, name]) => {
  if (t === 'existing') void loadTargetInfo(name)
  else targetInfo.value = null
})
watch(() => form.mode, (m) => {
  if (m !== 'mirror' && form.onExisting === 'adopt') form.onExisting = 'fail'
})

// Storage of a new target: backing, kind (s3 backings), container or remote bucket.
const backingItems = computed(() => backingSelectItems(whoami.value?.backings))
const backingModel = computed<string>({
  get: () => form.backing || defaultBacking(whoami.value?.backings),
  set: (v) => {
    form.backing = v
    form.kind = ''
    form.container = ''
  }
})
const selectedBacking = computed(() => newTargetBacking(form, ctx.value))
const kindItems = computed(() => kindChoices(selectedBacking.value))
const kindModel = computed<BucketKind | undefined>({
  get: () => targetKind(form, ctx.value) || undefined,
  set: (v) => {
    form.kind = v ?? ''
  }
})
const containerChoices = computed(() => containerItems(selectedBacking.value))
const containerModel = computed<string>({
  get: () => form.container || defaultContainer(selectedBacking.value) || containerChoices.value[0]?.value || '',
  set: (v) => {
    form.container = v
  }
})
const existingItems = computed(() => buckets.value.filter(b => b !== props.bucket))

const kind = computed(() => targetKind(form, ctx.value))
const errors = computed(() => validateCopyForm(form, ctx.value))
const versionItems = computed(() => versionsItems(form, ctx.value))
const hints = computed(() => copyHints(form, ctx.value))
const showTargetErrors = ref(false)
const showOptionErrors = ref(false)

const modeItems = [
  { value: 'copy', label: 'Copy', description: 'Copy everything once, then compare again; later changes are not followed.' },
  { value: 'mirror', label: 'Mirror', description: 'Copy everything, then keep the target in step with the source until the job is canceled.' }
]
const targetItems = [
  { value: 'new', label: 'New bucket', description: 'Created by the job, on the backing you choose.' },
  { value: 'existing', label: 'Existing bucket', description: 'Must be empty unless you choose otherwise under Options.' }
]
const configItems = [
  { value: 'safe', label: 'Safe settings', description: 'CORS, tags, default encryption, ACL, compression, the policy (with its ARNs rewritten); lifecycle for copies only.' },
  { value: 'all', label: 'All settings', description: 'Every bucket setting the target supports.' },
  { value: 'none', label: 'None', description: 'Objects only.' }
]
const onExistingItems = computed(() => [
  { value: 'fail', label: 'Refuse unless empty', description: 'The job fails if the target holds objects.' },
  { value: 'merge', label: 'Merge', description: 'Copy into the target without deleting anything in it.' },
  { value: 'adopt', label: 'Take over', description: 'A mirror takes the target over as if it had created it.', disabled: form.mode !== 'mirror' }
])
const encryptionItems = [
  { value: 'preserve', label: 'Preserve', description: 'Each version keeps its encryption; the target must offer the same provider.' },
  { value: 'target', label: 'Use the target’s settings', description: 'Versions are stored as the target’s default encryption says.' }
]
const compressionItems = [
  { value: 'preserve', label: 'Preserve' },
  { value: 'target', label: 'Use the target’s settings' }
]

// --- steps -----------------------------------------------------------------------------

const dryRun = ref<CopyJobDryRun | null>(null)
const dryRunError = ref<unknown>(null)
const checking = ref(false)
const reviewedSpec = ref<CopyJobSpec | null>(null)

async function runDryRun() {
  const spec = copyJobSpec(form, ctx.value)
  reviewedSpec.value = spec
  dryRun.value = null
  dryRunError.value = null
  checking.value = true
  try {
    dryRun.value = await requireAdmin().dryRunCopyJob(spec)
  } catch (e) {
    dryRunError.value = e
  } finally {
    checking.value = false
  }
}

function next() {
  if (step.value === 'target') {
    showTargetErrors.value = true
    if (!stepValid(errors.value, TARGET_STEP_FIELDS)) return
    step.value = 'options'
  } else if (step.value === 'options') {
    showOptionErrors.value = true
    if (!stepValid(errors.value)) return
    step.value = 'review'
    void runDryRun()
  }
}

function back() {
  if (step.value === 'review') step.value = 'options'
  else if (step.value === 'options') step.value = 'target'
}

const create = useAsyncAction(
  () => requireAdmin().createCopyJob(reviewedSpec.value ?? copyJobSpec(form, ctx.value)),
  { errorTitle: 'Could not create the job', toastErrors: false }
)

async function submit() {
  if (step.value !== 'review') {
    next()
    return
  }
  if (!dryRun.value || create.loading.value) return
  const job = await create.run()
  if (!job) return
  const label = form.mode === 'mirror' ? 'Mirror' : 'Copy'
  toast.add({
    title: `${label} job created`,
    description: `${props.bucket} → ${form.targetBucket.trim()}`,
    color: 'success',
    icon: 'i-lucide-circle-check',
    actions: props.serverId ? [{ label: 'Open jobs', color: 'neutral', variant: 'outline', to: appRoutes.jobs(props.serverId, props.bucket) }] : undefined
  })
  emit('close', job)
}

const reviewLines = computed(() => describeSpec(dryRun.value?.spec ?? reviewedSpec.value ?? copyJobSpec(form, ctx.value)))
const warnings = computed(() => dryRun.value?.warnings ?? [])
const fieldError = (show: boolean, msg: string | undefined) => (show ? msg : undefined)
</script>

<template>
  <UModal
    :title="`Copy or mirror ${bucket}`"
    description="Copies the bucket's objects into another bucket, on any backing, as a background job."
    :ui="{ content: 'sm:max-w-2xl', footer: 'justify-between' }"
  >
    <template #body>
      <ol class="mb-4 flex items-center gap-2 text-xs" aria-label="Steps">
        <li
          v-for="(s, i) in STEPS"
          :key="s.value"
          class="flex items-center gap-1.5"
          :class="i === stepIndex ? 'font-medium text-highlighted' : i < stepIndex ? 'text-muted' : 'text-dimmed'"
          :aria-current="i === stepIndex ? 'step' : undefined"
        >
          <span
            class="inline-flex size-5 items-center justify-center rounded-full border text-[11px]"
            :class="i === stepIndex ? 'border-primary bg-primary text-inverted' : 'border-default'"
          >{{ i + 1 }}</span>
          {{ s.label }}
          <UIcon v-if="i < STEPS.length - 1" name="i-lucide-chevron-right" class="size-3.5 text-dimmed" aria-hidden="true" />
        </li>
      </ol>

      <form :id="formId" class="space-y-5" novalidate data-testid="copy-wizard" :data-step="step" @submit.prevent="submit">
        <!-- 1. Target -->
        <template v-if="step === 'target'">
          <URadioGroup v-model="form.mode" legend="Job" :items="modeItems" orientation="horizontal" variant="table" />
          <UFormField label="Source prefix" name="sourcePrefix" help="Copy only keys starting with this prefix; empty = the whole bucket." :error="fieldError(showTargetErrors, errors.sourcePrefix)">
            <UInput v-model="form.sourcePrefix" class="w-full font-mono" placeholder="photos/2024/" autocomplete="off" spellcheck="false" aria-label="Source prefix" />
          </UFormField>

          <URadioGroup v-model="form.target" legend="Target" :items="targetItems" orientation="horizontal" variant="table" />

          <template v-if="form.target === 'new'">
            <UFormField label="New bucket name" name="targetBucket" required :error="fieldError(showTargetErrors, errors.targetBucket)">
              <UInput
                v-model.trim="form.targetBucket"
                class="w-full font-mono"
                placeholder="my-bucket-copy"
                autocomplete="off"
                spellcheck="false"
                aria-label="New bucket name"
                @update:model-value="nameEdited = true"
              />
            </UFormField>
            <UFormField v-if="backingItems.length" label="Storage backing" name="backing">
              <USelect v-model="backingModel" :items="backingItems" value-key="value" class="w-full" aria-label="Storage backing" />
            </UFormField>
            <div v-if="kindItems.length" class="space-y-3 rounded-lg border border-default p-3" data-testid="target-kind">
              <URadioGroup v-model="kindModel" legend="Bucket kind" :items="kindItems" />
              <UFormField v-if="kind === 'wrapped'" label="Container" name="container" :error="errors.container">
                <USelect v-if="containerChoices.length" v-model="containerModel" :items="containerChoices" value-key="value" class="w-full" aria-label="Container" />
              </UFormField>
              <UFormField
                v-else-if="kind === 'native'"
                label="Remote bucket"
                name="remoteBucket"
                help="Created in the backing's account; defaults to the bucket name."
                :error="fieldError(showTargetErrors, errors.remoteBucket)"
              >
                <UInput v-model.trim="form.remoteBucket" class="w-full font-mono" :placeholder="form.targetBucket || 'my-bucket'" autocomplete="off" spellcheck="false" aria-label="Remote bucket" />
              </UFormField>
            </div>
            <USwitch
              v-model="form.objectLock"
              label="Object lock"
              :description="sourceLock ? `${bucket} has object lock; keep it so locked versions keep their retention.` : 'Allows WORM retention on the new bucket; can only be enabled at creation.'"
              @update:model-value="lockEdited = true"
            />
          </template>
          <template v-else>
            <UFormField label="Bucket" name="targetBucket" required :error="fieldError(showTargetErrors, errors.targetBucket)" :help="bucketsLoaded ? undefined : 'The bucket list could not be loaded; type the name.'">
              <USelectMenu
                v-if="bucketsLoaded"
                v-model="form.targetBucket"
                :items="existingItems"
                class="w-full"
                placeholder="Choose a bucket"
                :search-input="{ placeholder: 'Filter buckets…' }"
                aria-label="Target bucket"
              />
              <UInput v-else v-model.trim="form.targetBucket" class="w-full font-mono" placeholder="other-bucket" autocomplete="off" spellcheck="false" aria-label="Target bucket" />
            </UFormField>
            <p v-if="targetInfoLoading" class="text-xs text-muted">
              Checking the bucket…
            </p>
            <p v-else-if="targetInfo?.kind" class="text-xs text-muted">
              {{ bucketKindInfo(targetInfo.kind).label }} bucket · versioning {{ targetInfo.versioningUnsupported ? 'not supported' : targetInfo.versioning === 'Enabled' ? 'enabled' : targetInfo.versioning === 'Suspended' ? 'suspended' : 'off' }}
            </p>
          </template>
          <UFormField label="Target prefix" name="targetPrefix" help="Keys are written under this prefix; empty = the same keys." :error="fieldError(showTargetErrors, errors.targetPrefix)">
            <UInput v-model="form.targetPrefix" class="w-full font-mono" placeholder="backup/" autocomplete="off" spellcheck="false" aria-label="Target prefix" />
          </UFormField>
        </template>

        <!-- 2. Options -->
        <template v-else-if="step === 'options'">
          <URadioGroup v-model="form.versions" legend="Versions" :items="versionItems" data-testid="versions" />
          <p v-if="showOptionErrors && errors.versions" class="-mt-3 text-sm text-error" role="alert">
            {{ errors.versions }}
          </p>
          <URadioGroup v-model="form.configs" legend="Bucket settings" :items="configItems" />
          <template v-if="form.target === 'existing'">
            <URadioGroup v-model="form.onExisting" legend="If the target holds objects" :items="onExistingItems" />
            <p v-if="showOptionErrors && errors.onExisting" class="-mt-3 text-sm text-error" role="alert">
              {{ errors.onExisting }}
            </p>
          </template>
          <URadioGroup v-model="form.encryption" legend="Encryption" :items="encryptionItems" />
          <div class="space-y-3">
            <UCheckbox
              v-model="form.allowPlaintext"
              label="Allow plaintext copies"
              description="Encrypted versions may be stored unencrypted when the target cannot encrypt them (e.g. native buckets)."
            />
            <UCheckbox
              v-if="sourceLock || form.dropRetention"
              v-model="form.dropRetention"
              label="Allow dropping retention"
              description="Locked versions may be copied to a target without object lock; their retention and legal holds are not copied."
            />
            <p v-if="showOptionErrors && errors.dropRetention" class="text-sm text-error" role="alert">
              {{ errors.dropRetention }}
            </p>
          </div>
          <UFormField label="Compression" name="compression">
            <USelect v-model="form.compression" :items="compressionItems" value-key="value" class="w-full sm:w-72" aria-label="Compression" />
          </UFormField>

          <fieldset v-if="form.mode === 'mirror'" class="space-y-3 rounded-lg border border-default p-3">
            <legend class="px-1 text-sm font-medium text-highlighted">
              Mirror
            </legend>
            <USwitch v-model="form.propagateDeletes" label="Propagate deletes" description="Deletes in the source are applied to the target." />
            <USwitch v-model="form.deleteExtraneous" label="Delete extra target keys" description="Keys in the target that the source does not have are deleted." />
            <div class="grid gap-3 sm:grid-cols-2">
              <UFormField label="Poll interval" name="pollInterval" help="How often a native source is compared (≥ 30s)." :error="fieldError(showOptionErrors, errors.pollInterval)">
                <UInput v-model.trim="form.pollInterval" class="w-full font-mono" placeholder="5m" aria-label="Poll interval" />
              </UFormField>
              <UFormField label="Verify interval" name="verifyInterval" help="Full comparison besides change notifications." :error="fieldError(showOptionErrors, errors.verifyInterval)">
                <UInput v-model.trim="form.verifyInterval" class="w-full font-mono" placeholder="24h" aria-label="Verify interval" />
              </UFormField>
            </div>
          </fieldset>
          <USwitch v-else v-model="form.finalPass" label="Final pass" description="Compare source and target again after the first pass and copy what changed meanwhile." />

          <UFormField label="Keys copied in parallel" name="concurrency" :error="fieldError(showOptionErrors, errors.concurrency)">
            <UInputNumber v-model="form.concurrency" :min="1" :max="MAX_CONCURRENCY" class="w-32" aria-label="Keys copied in parallel" />
          </UFormField>

          <ul v-if="hints.length" class="space-y-1 text-sm text-muted" data-testid="copy-hints">
            <li v-for="h in hints" :key="h" class="flex items-start gap-1.5">
              <UIcon name="i-lucide-info" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
              <span>{{ h }}</span>
            </li>
          </ul>
        </template>

        <!-- 3. Review -->
        <template v-else>
          <div v-if="checking" class="flex items-center gap-2 text-sm text-muted" role="status">
            <UIcon name="i-lucide-loader-circle" class="size-4 animate-spin" aria-hidden="true" />
            Checking the job with the server (dry run)…
          </div>
          <CommonErrorAlert v-else-if="dryRunError" :error="dryRunError" title="The server refused this job" retryable @retry="runDryRun" />
          <template v-else-if="dryRun">
            <UAlert
              v-for="w in warnings"
              :key="w"
              color="warning"
              variant="subtle"
              icon="i-lucide-triangle-alert"
              :description="w"
              data-testid="dry-run-warning"
            />
            <p v-if="!warnings.length" class="flex items-center gap-1.5 text-sm text-success" data-testid="dry-run-ok">
              <UIcon name="i-lucide-circle-check" class="size-4" aria-hidden="true" />
              The server accepted the job. Nothing has been created yet.
            </p>
          </template>
          <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 rounded-lg border border-default p-3 text-sm" data-testid="dry-run-summary">
            <template v-for="l in reviewLines" :key="l.label">
              <dt class="text-muted">
                {{ l.label }}
              </dt>
              <dd class="break-words text-highlighted">
                {{ l.value }}
              </dd>
            </template>
          </dl>
          <CommonErrorAlert v-if="create.error.value" :error="create.error.value" title="Could not create the job" />
        </template>
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="emit('close', null)" />
      <div class="flex gap-2">
        <UButton v-if="step !== 'target'" color="neutral" variant="outline" icon="i-lucide-arrow-left" label="Back" @click="back" />
        <UButton v-if="step !== 'review'" type="submit" :form="formId" trailing-icon="i-lucide-arrow-right" :label="step === 'options' ? 'Check' : 'Next'" />
        <UButton
          v-else
          type="submit"
          :form="formId"
          icon="i-lucide-play"
          :label="form.mode === 'mirror' ? 'Start mirror' : 'Start copy'"
          :loading="create.loading.value"
          :disabled="!dryRun || checking"
        />
      </div>
    </template>
  </UModal>
</template>
