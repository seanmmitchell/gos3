<script setup lang="ts">
import { CORS_METHODS, type CorsRuleErrors, type CorsRuleForm } from './-cors'

/** Editor for one bucket CORS rule (used by BucketSettingsCorsTab). */
const rule = defineModel<CorsRuleForm>({ required: true })

const props = withDefaults(defineProps<{
  index: number
  /** Validation errors to display (empty until the user tries to save). */
  errors?: CorsRuleErrors
}>(), { errors: () => ({}) })

const emit = defineEmits<{ remove: [] }>()

const baseId = useId()
const hasError = computed(() => Object.values(props.errors).some(Boolean))

function set<K extends keyof CorsRuleForm>(key: K, value: CorsRuleForm[K]) {
  rule.value = { ...rule.value, [key]: value }
}

const methodItems: string[] = [...CORS_METHODS]

function setMethods(v: unknown) {
  const list = Array.isArray(v) ? v.map(String) : []
  // Keep the canonical method order.
  set('allowedMethods', CORS_METHODS.filter(m => list.includes(m)))
}
</script>

<template>
  <div
    role="group"
    :aria-labelledby="`${baseId}-title`"
    class="space-y-3 rounded-md border p-3"
    :class="hasError ? 'border-error/60' : 'border-default'"
  >
    <p :id="`${baseId}-title`" class="text-sm font-medium text-highlighted">
      Rule {{ index + 1 }}<span v-if="rule.id" class="font-normal text-muted"> · {{ rule.id }}</span>
    </p>
    <div class="grid gap-3 md:grid-cols-2">
      <UFormField label="Allowed origins" :name="`${baseId}-origins`" required help="“*” or e.g. https://app.example.com (one * allowed)" :error="errors.allowedOrigins">
        <AdminListInput :model-value="rule.allowedOrigins" placeholder="https://app.example.com" @update:model-value="set('allowedOrigins', $event)" />
      </UFormField>
      <div class="space-y-1">
        <UCheckboxGroup
          :model-value="rule.allowedMethods"
          :items="methodItems"
          legend="Allowed methods"
          orientation="horizontal"
          :ui="{ legend: 'mb-2 text-sm font-medium text-default', fieldset: 'flex-wrap gap-x-4 gap-y-2' }"
          @update:model-value="setMethods"
        />
        <p v-if="errors.allowedMethods" class="text-sm text-error">
          {{ errors.allowedMethods }}
        </p>
      </div>
      <UFormField label="Allowed headers" :name="`${baseId}-headers`" help="Request headers (“*” = any)" :error="errors.allowedHeaders">
        <AdminListInput :model-value="rule.allowedHeaders" placeholder="*" @update:model-value="set('allowedHeaders', $event)" />
      </UFormField>
      <UFormField label="Exposed headers" :name="`${baseId}-expose`" help="Response headers scripts may read (e.g. ETag)" :error="errors.exposeHeaders">
        <AdminListInput :model-value="rule.exposeHeaders" placeholder="ETag" @update:model-value="set('exposeHeaders', $event)" />
      </UFormField>
      <UFormField label="Max age (seconds)" :name="`${baseId}-maxage`" :error="errors.maxAgeSeconds">
        <UInputNumber :model-value="rule.maxAgeSeconds" :min="0" :step="60" class="w-full" placeholder="none" @update:model-value="set('maxAgeSeconds', $event ?? null)" />
      </UFormField>
      <UFormField label="Rule ID" :name="`${baseId}-id`" help="Optional" :error="errors.id">
        <UInput :model-value="rule.id" class="w-full" placeholder="optional" @update:model-value="set('id', String($event ?? ''))" />
      </UFormField>
    </div>
    <div class="flex justify-end">
      <UButton size="xs" color="error" variant="ghost" icon="i-lucide-trash-2" :label="`Remove rule ${index + 1}`" @click="emit('remove')" />
    </div>
  </div>
</template>
