<script setup lang="ts">
import type { ObjectLockConfiguration, ObjectLockMode, VersioningStatus } from '~/types/s3'

/**
 * Object lock (?object-lock): status, enabling on a versioned bucket
 * (irreversible) and the default retention (mode + days or years).
 */
const props = defineProps<{
  bucket: string
  versioning: VersioningStatus | null
}>()
const emit = defineEmits<{ changed: [] }>()

const { requireS3 } = useConnection()
const confirm = useConfirm()

const retention = ref<'none' | ObjectLockMode>('none')
const unit = ref<'days' | 'years'>('days')
const amount = ref<number | null>(30)
const original = ref('')
const submitted = ref(false)

const loader = useLoader(async () => {
  const cfg = await requireS3().getObjectLockConfiguration(props.bucket)
  const dr = cfg?.rule?.defaultRetention
  retention.value = dr?.mode ?? 'none'
  unit.value = dr?.years !== undefined ? 'years' : 'days'
  amount.value = dr?.years ?? dr?.days ?? 30
  original.value = signature()
  submitted.value = false
  return cfg
}, { watch: [() => props.bucket] })

const enabled = computed(() => !!loader.data.value?.enabled)

function signature(): string {
  return JSON.stringify(retention.value === 'none' ? ['none'] : [retention.value, unit.value, amount.value])
}
const dirty = computed(() => signature() !== original.value)
const amountError = computed(() => {
  if (retention.value === 'none') return undefined
  const n = amount.value
  if (n === null || !Number.isInteger(n) || n < 1) return 'Must be a whole number ≥ 1'
  if (unit.value === 'years' && n > 100) return 'At most 100 years'
  if (unit.value === 'days' && n > 36500) return 'At most 36500 days'
  return undefined
})

const retentionItems = [
  { label: 'No default retention', value: 'none', description: 'Objects are only locked when a request sets a retention or legal hold.' },
  { label: 'Governance', value: 'GOVERNANCE', description: 'Users with s3:BypassGovernanceRetention can still delete or shorten the lock.' },
  { label: 'Compliance', value: 'COMPLIANCE', description: 'Nobody — not even root — can delete a locked version until the period ends.' }
]
const unitItems = [{ label: 'Days', value: 'days' }, { label: 'Years', value: 'years' }]

const enableLock = useAsyncAction(async () => {
  await requireS3().putObjectLockConfiguration(props.bucket, { enabled: true })
  await loader.refresh()
  emit('changed')
}, { successMessage: 'Object lock enabled', errorTitle: 'Could not enable object lock' })

async function onEnable() {
  const ok = await confirm({
    title: 'Enable object lock?',
    description: 'Object lock cannot be disabled again, and versioning can no longer be suspended on this bucket.',
    confirmLabel: 'Enable object lock',
    color: 'warning',
    icon: 'i-lucide-shield-alert',
    requireText: props.bucket
  })
  if (ok) await enableLock.run()
}

const save = useAsyncAction(async () => {
  const cfg: ObjectLockConfiguration = { enabled: true }
  if (retention.value !== 'none' && amount.value) {
    cfg.rule = { defaultRetention: { mode: retention.value, [unit.value]: amount.value } }
  }
  await requireS3().putObjectLockConfiguration(props.bucket, cfg)
  await loader.refresh()
  emit('changed')
}, { successMessage: 'Default retention saved', errorTitle: 'Could not save default retention' })

async function onSave() {
  submitted.value = true
  if (amountError.value) return
  if (retention.value === 'COMPLIANCE') {
    const ok = await confirm({
      title: 'Use compliance mode?',
      description: `Every new object version will be undeletable for ${amount.value} ${unit.value}. This cannot be bypassed by any user.`,
      confirmLabel: 'Use compliance mode',
      color: 'warning',
      icon: 'i-lucide-shield-alert'
    })
    if (!ok) return
  }
  await save.run()
}
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 class="font-semibold text-highlighted">
            Object lock
          </h3>
          <p class="text-sm text-muted">
            Write-once-read-many (WORM) protection for object versions.
          </p>
        </div>
        <UBadge :label="enabled ? 'Enabled' : 'Disabled'" :color="enabled ? 'primary' : 'neutral'" variant="subtle" :icon="enabled ? 'i-lucide-shield-check' : undefined" />
      </div>
    </template>
    <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Could not load object lock configuration" retryable @retry="loader.refresh" />
    <USkeleton v-else-if="loader.loading.value && !original" class="h-32 w-full" />
    <div v-else-if="!enabled" class="space-y-4">
      <p class="text-sm text-muted">
        Object lock is not enabled for this bucket. It can be enabled on a bucket with versioning enabled; it cannot be turned off afterwards.
      </p>
      <UAlert
        v-if="versioning !== 'Enabled'"
        color="info"
        variant="subtle"
        icon="i-lucide-history"
        title="Versioning required"
        description="Enable versioning first (Versioning tab)."
      />
      <UButton
        color="warning"
        variant="soft"
        icon="i-lucide-shield-alert"
        label="Enable object lock"
        :disabled="versioning !== 'Enabled'"
        :loading="enableLock.loading.value"
        @click="onEnable"
      />
    </div>
    <form v-else class="space-y-4" novalidate @submit.prevent="onSave">
      <URadioGroup v-model="retention" :items="retentionItems" legend="Default retention" />
      <div v-if="retention !== 'none'" class="grid gap-4 sm:grid-cols-[10rem_10rem]">
        <UFormField label="Period" name="retention-amount" required :error="submitted && amountError">
          <UInputNumber v-model="amount" :min="1" class="w-full" />
        </UFormField>
        <UFormField label="Unit" name="retention-unit">
          <USelect v-model="unit" :items="unitItems" value-key="value" class="w-full" />
        </UFormField>
      </div>
      <div class="flex justify-end gap-2 border-t border-default pt-4">
        <UButton type="submit" icon="i-lucide-save" label="Save default retention" :disabled="!dirty" :loading="save.loading.value" />
      </div>
    </form>
  </UCard>
</template>
