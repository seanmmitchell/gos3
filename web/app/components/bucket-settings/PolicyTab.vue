<script setup lang="ts">
import { S3Error } from '~/utils/s3/errors'
import { formatPolicy, validatePolicyDocument } from '~/components/admin/-policies'
import { applyBucketTemplate, BUCKET_POLICY_TEMPLATES, type BucketPolicyTemplateId } from './-policy-templates'

/**
 * Bucket policy editor (?policy) with templates, validation, policy status
 * (public or not) and delete.
 */
const props = defineProps<{ bucket: string }>()
const emit = defineEmits<{ changed: [] }>()

const { requireS3 } = useConnection()
const confirm = useConfirm()
const toast = useToast()

const text = ref('')
const original = ref('')
const exists = ref(false)
const isPublic = ref<boolean | null>(null)
const valid = ref(true)
const serverError = ref<string>()

const loader = useLoader(async () => {
  const s3 = requireS3()
  const [policy, status] = await Promise.all([
    s3.getBucketPolicy(props.bucket),
    s3.getBucketPolicyStatus(props.bucket).catch(() => null)
  ])
  exists.value = policy !== null
  text.value = policy ? prettify(policy) : ''
  original.value = text.value
  isPublic.value = status?.isPublic ?? null
  serverError.value = undefined
  return policy
}, { watch: [() => props.bucket] })

function prettify(s: string): string {
  try {
    return formatPolicy(JSON.parse(s))
  } catch {
    return s
  }
}

const dirty = computed(() => text.value !== original.value)
const validate = (v: unknown) => validatePolicyDocument(v, 'bucket', props.bucket)
watch(text, () => (serverError.value = undefined))

// Templates
const templateId = ref<BucketPolicyTemplateId>('public-read')
const templateUser = ref('')
const templateItems = BUCKET_POLICY_TEMPLATES.map(t => ({ label: t.label, value: t.id, description: t.description }))
const template = computed(() => BUCKET_POLICY_TEMPLATES.find(t => t.id === templateId.value)!)
const templateUserError = computed(() => (template.value.needsUser && !/^[\w+=,.@-]{1,64}$/.test(templateUser.value.trim()) ? 'Enter an IAM user name' : undefined))
const templateTouched = ref(false)

function addTemplate() {
  templateTouched.value = true
  if (templateUserError.value) return
  const hadPolicy = !!text.value.trim()
  text.value = applyBucketTemplate(text.value, templateId.value, props.bucket, { user: templateUser.value.trim() })
  templateTouched.value = false
  toast.add({
    title: hadPolicy ? 'Template statements added' : 'Template applied',
    description: 'Review the policy, then save.',
    color: 'success',
    icon: 'i-lucide-circle-check'
  })
}

const save = useAsyncAction(async () => {
  const doc = JSON.parse(text.value) as unknown
  try {
    await requireS3().putBucketPolicy(props.bucket, JSON.stringify(doc))
  } catch (e) {
    if (e instanceof S3Error && (e.code === 'MalformedPolicy' || e.code === 'InvalidPolicyDocument' || e.code === 'InvalidArgument')) {
      serverError.value = e.message
    }
    throw e
  }
  await loader.refresh()
  emit('changed')
}, { successMessage: 'Bucket policy saved', errorTitle: 'Could not save bucket policy' })

async function onSave() {
  if (!valid.value) return
  const doc = JSON.parse(text.value) as { Statement?: unknown }
  const statements = Array.isArray(doc.Statement) ? doc.Statement : [doc.Statement]
  const grantsPublic = statements.some((s) => {
    const st = s as { Effect?: string, Principal?: unknown }
    return st?.Effect === 'Allow' && (st.Principal === '*' || JSON.stringify(st.Principal ?? '').includes('"*"'))
  })
  if (grantsPublic) {
    const ok = await confirm({
      title: 'Grant public access?',
      description: `This policy allows anonymous access to ${props.bucket}. Anyone on the network who knows the URLs can use it.`,
      confirmLabel: 'Save public policy',
      color: 'warning',
      icon: 'i-lucide-globe'
    })
    if (!ok) return
  }
  await save.run()
}

const remove = useAsyncAction(async () => {
  await requireS3().deleteBucketPolicy(props.bucket)
  await loader.refresh()
  emit('changed')
}, { successMessage: 'Bucket policy deleted', errorTitle: 'Could not delete bucket policy' })

async function onDelete() {
  const ok = await confirm({
    title: 'Delete the bucket policy?',
    description: 'Access granted or denied by this policy will no longer apply.',
    confirmLabel: 'Delete policy',
    icon: 'i-lucide-trash-2'
  })
  if (ok) await remove.run()
}

async function reset() {
  if (dirty.value && !(await confirm({ title: 'Discard changes?', confirmLabel: 'Discard', color: 'warning' }))) return
  text.value = original.value
}
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 class="font-semibold text-highlighted">
            Bucket policy
          </h3>
          <p class="text-sm text-muted">
            Resource policy evaluated together with users' identity policies. Explicit denies always win.
          </p>
        </div>
        <div class="flex items-center gap-2">
          <UBadge v-if="isPublic === true" label="Public" color="warning" variant="subtle" icon="i-lucide-globe" />
          <UBadge v-else-if="exists" label="Not public" color="success" variant="subtle" icon="i-lucide-lock" />
          <UBadge v-if="dirty" label="Unsaved" color="warning" variant="subtle" />
        </div>
      </div>
    </template>

    <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Could not load the bucket policy" retryable @retry="loader.refresh" />
    <USkeleton v-else-if="loader.loading.value && !original && !text" class="h-64 w-full" />
    <div v-else class="space-y-4">
      <div class="space-y-3 rounded-md border border-default bg-elevated/30 p-3">
        <div class="flex items-center gap-2 text-sm font-medium text-highlighted">
          <UIcon name="i-lucide-layout-template" class="size-4 text-primary" aria-hidden="true" />
          Templates
        </div>
        <div class="grid gap-3 md:grid-cols-[minmax(0,16rem)_1fr_auto] md:items-start">
          <UFormField label="Template" name="bucket-policy-template">
            <USelect v-model="templateId" :items="templateItems" value-key="value" class="w-full" />
          </UFormField>
          <UFormField v-if="template.needsUser" label="IAM user" name="bucket-policy-user" required :error="templateTouched && templateUserError">
            <UInput v-model="templateUser" class="w-full" placeholder="alice" autocomplete="off" @keydown.enter.prevent="addTemplate" />
          </UFormField>
          <div v-else class="hidden md:block" />
          <div class="md:pt-6">
            <UButton color="neutral" variant="outline" icon="i-lucide-plus" :label="text.trim() ? 'Add to policy' : 'Use template'" @click="addTemplate" />
          </div>
        </div>
        <p class="text-xs text-muted">
          {{ template.description }} Statements are added to the current policy (same Sid = replaced).
        </p>
      </div>

      <CommonEmptyState
        v-if="!exists && !text"
        compact
        icon="i-lucide-file-lock"
        title="No bucket policy"
        description="Only identity policies and ACLs control access. Start from a template or write a policy."
      >
        <UButton size="sm" color="neutral" variant="outline" icon="i-lucide-pencil" label="Write a policy" @click="text = formatPolicy({ Version: '2012-10-17', Statement: [] })" />
      </CommonEmptyState>
      <template v-else>
        <CommonJsonEditor v-model="text" label="Policy document" :rows="18" :validate="validate" @validity="valid = $event" />
        <UAlert
          v-if="serverError"
          color="error"
          variant="subtle"
          icon="i-lucide-circle-alert"
          title="The server rejected the policy"
          :description="serverError"
          role="alert"
        />
      </template>

      <div class="flex flex-wrap justify-between gap-2">
        <UButton
          v-if="exists"
          color="error"
          variant="soft"
          icon="i-lucide-trash-2"
          label="Delete policy"
          :loading="remove.loading.value"
          @click="onDelete"
        />
        <span v-else />
        <div class="flex gap-2">
          <UButton color="neutral" variant="ghost" icon="i-lucide-undo-2" label="Reset" :disabled="!dirty" @click="reset" />
          <UButton icon="i-lucide-save" label="Save policy" :disabled="!dirty || !valid || !text.trim()" :loading="save.loading.value" @click="onSave" />
        </div>
      </div>
    </div>
  </UCard>
</template>
