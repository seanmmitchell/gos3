<script setup lang="ts">
import type { LifecycleRule } from '~/types/s3'
import { formToRule, newRuleForm, ruleToForm, validateRuleForm, type LifecycleRuleForm } from './-lifecycle'

/**
 * Lifecycle rule editor dialog. Opened through useOverlay(); emits
 * close(rule) or close(null).
 */
const props = withDefaults(defineProps<{
  rule?: LifecycleRule | null
  /** IDs of the other rules (uniqueness). */
  otherIds?: string[]
  /** Bucket versioning ever enabled (noncurrent-version actions apply). */
  versioned?: boolean
}>(), { rule: null, otherIds: () => [], versioned: false })

const emit = defineEmits<{ close: [value: LifecycleRule | null] }>()

const form = ref<LifecycleRuleForm>(props.rule ? ruleToForm(props.rule) : newRuleForm(props.otherIds))
const submitted = ref(false)
const errors = computed(() => validateRuleForm(form.value, props.otherIds))
const show = (k: keyof typeof errors.value) => (submitted.value ? errors.value[k] : undefined)

const expirationItems = [
  { label: 'Do not expire current versions', value: 'none' },
  { label: 'After a number of days', value: 'days' },
  { label: 'On a date', value: 'date' }
]

const today = new Date().toISOString().slice(0, 10)

function submit() {
  submitted.value = true
  if (Object.keys(errors.value).length) return
  emit('close', formToRule(form.value))
}
</script>

<template>
  <UModal :title="rule ? 'Edit lifecycle rule' : 'Add lifecycle rule'" :ui="{ content: 'sm:max-w-3xl', footer: 'justify-end' }">
    <template #body>
      <form id="lifecycle-rule-form" class="space-y-5" novalidate @submit.prevent="submit">
        <UAlert v-if="submitted && errors.actions" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="errors.actions" role="alert" />
        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Rule ID" name="id" help="Unique within the bucket (optional)." :error="show('id')">
            <UInput v-model="form.id" class="w-full" autofocus />
          </UFormField>
          <div class="sm:pt-6">
            <USwitch v-model="form.enabled" label="Enabled" description="Disabled rules are kept but not applied." />
          </div>
        </div>

        <fieldset class="space-y-3">
          <legend class="text-sm font-semibold text-highlighted">
            Filter
          </legend>
          <p class="text-xs text-muted">
            All conditions must match. Leave everything empty to apply the rule to the whole bucket.
          </p>
          <div class="grid gap-4 sm:grid-cols-3">
            <UFormField label="Key prefix" name="prefix" class="sm:col-span-3">
              <UInput v-model="form.prefix" class="w-full font-mono" placeholder="logs/" />
            </UFormField>
            <UFormField label="Larger than (bytes)" name="sizeGreaterThan" :error="show('size')">
              <UInputNumber v-model="form.sizeGreaterThan" :min="0" class="w-full" placeholder="any" />
            </UFormField>
            <UFormField label="Smaller than (bytes)" name="sizeLessThan">
              <UInputNumber v-model="form.sizeLessThan" :min="1" class="w-full" placeholder="any" />
            </UFormField>
          </div>
          <UFormField label="Object tags" name="tags" :error="show('tags')">
            <CommonKeyValueEditor v-model="form.tags" key-label="Tag key" value-label="Tag value" add-label="Add tag" :max-items="10" />
          </UFormField>
        </fieldset>

        <fieldset class="space-y-3">
          <legend class="text-sm font-semibold text-highlighted">
            Current versions
          </legend>
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Expiration" name="expirationMode">
              <USelect v-model="form.expirationMode" :items="expirationItems" value-key="value" class="w-full" />
            </UFormField>
            <UFormField v-if="form.expirationMode === 'days'" label="Days after creation" name="expirationDays" required :error="show('expirationDays')">
              <UInputNumber v-model="form.expirationDays" :min="1" class="w-full" />
            </UFormField>
            <UFormField v-else-if="form.expirationMode === 'date'" label="Date (UTC)" name="expirationDate" required :error="show('expirationDate')">
              <UInput v-model="form.expirationDate" type="date" :min="today" class="w-full" />
            </UFormField>
          </div>
          <UCheckbox
            v-model="form.expiredObjectDeleteMarker"
            :disabled="form.expirationMode !== 'none'"
            label="Remove expired delete markers"
            description="Delete markers whose noncurrent versions are all gone. Only without a days/date expiration."
          />
          <p v-if="show('expiredObjectDeleteMarker')" class="text-xs text-error">
            {{ show('expiredObjectDeleteMarker') }}
          </p>
        </fieldset>

        <fieldset class="space-y-3">
          <legend class="text-sm font-semibold text-highlighted">
            Noncurrent versions
          </legend>
          <p v-if="!versioned" class="text-xs text-muted">
            Versioning has never been enabled on this bucket; these settings only matter once it is.
          </p>
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Delete after (days noncurrent)" name="noncurrentDays" :error="show('noncurrentDays')">
              <UInputNumber v-model="form.noncurrentDays" :min="1" class="w-full" placeholder="keep" />
            </UFormField>
            <UFormField label="But keep newest versions" name="newerNoncurrentVersions" help="Optional, 1–100" :error="show('newerNoncurrentVersions')">
              <UInputNumber v-model="form.newerNoncurrentVersions" :min="1" :max="100" class="w-full" placeholder="none" />
            </UFormField>
          </div>
        </fieldset>

        <fieldset class="space-y-3">
          <legend class="text-sm font-semibold text-highlighted">
            Incomplete multipart uploads
          </legend>
          <UFormField label="Abort after (days)" name="abortDays" help="Frees storage of abandoned uploads. Not allowed with tag filters." :error="show('abortDays')">
            <UInputNumber v-model="form.abortDays" :min="1" class="w-full sm:w-48" placeholder="never" />
          </UFormField>
        </fieldset>

        <p v-if="form.transitions?.length || form.noncurrentVersionTransitions?.length" class="text-xs text-muted">
          This rule also has storage-class transitions; they are kept unchanged.
        </p>
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="emit('close', null)" />
      <UButton type="submit" form="lifecycle-rule-form" icon="i-lucide-check" :label="rule ? 'Update rule' : 'Add rule'" />
    </template>
  </UModal>
</template>
