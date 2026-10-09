<script setup lang="ts">
import { buildCors } from '~/utils/s3/builders'
import { CORS_METHODS, corsRuleToForm, formToCorsRule, hasErrors, MAX_CORS_RULES, newCorsRule, validateCorsRule, type CorsRuleForm } from './-cors'

/**
 * Bucket CORS rules (?cors): one editor row per rule (origins, methods,
 * headers, exposed headers, max age) plus a read-only XML view.
 */
const props = defineProps<{ bucket: string }>()
const emit = defineEmits<{ changed: [] }>()

const { requireS3 } = useConnection()
const confirm = useConfirm()

const rules = ref<CorsRuleForm[]>([])
const originalJson = ref('[]')
const submitted = ref(false)
const view = ref<'editor' | 'xml'>('editor')
const viewItems = [
  { label: 'Editor', value: 'editor', icon: 'i-lucide-table-2' },
  { label: 'XML', value: 'xml', icon: 'i-lucide-code' }
]

const loader = useLoader(async () => {
  const list = await requireS3().getBucketCors(props.bucket)
  rules.value = list.map(corsRuleToForm)
  // Baseline in the same normalised shape the editor produces (method order, lists).
  originalJson.value = JSON.stringify(rules.value.map(formToCorsRule))
  submitted.value = false
  return list
}, { watch: [() => props.bucket] })

const s3Rules = computed(() => rules.value.map(formToCorsRule))
const dirty = computed(() => JSON.stringify(s3Rules.value) !== originalJson.value)
const errors = computed(() => rules.value.map(validateCorsRule))
const invalid = computed(() => errors.value.some(hasErrors))
const xml = computed(() => (s3Rules.value.length ? buildCors(s3Rules.value) : '<!-- No CORS configuration -->'))
const exists = computed(() => originalJson.value !== '[]')

function addRule() {
  if (rules.value.length >= MAX_CORS_RULES) return
  rules.value = [...rules.value, newCorsRule()]
}

function removeRule(i: number) {
  rules.value = rules.value.filter((_, j) => j !== i)
}

function toggleMethod(rule: CorsRuleForm, m: string, on: boolean | 'indeterminate') {
  rule.allowedMethods = on === true ? [...new Set([...rule.allowedMethods, m])] : rule.allowedMethods.filter(x => x !== m)
}

const save = useAsyncAction(async () => {
  const s3 = requireS3()
  if (s3Rules.value.length) await s3.putBucketCors(props.bucket, s3Rules.value)
  else await s3.deleteBucketCors(props.bucket)
  await loader.refresh()
  emit('changed')
}, { successMessage: 'CORS configuration saved', errorTitle: 'Could not save CORS configuration' })

async function onSave() {
  submitted.value = true
  if (invalid.value) return
  if (!s3Rules.value.length) {
    const ok = await confirm({ title: 'Remove all CORS rules?', description: 'Browsers on other origins will no longer be able to access this bucket (unless the server-wide CORS settings allow it).', confirmLabel: 'Remove', icon: 'i-lucide-trash-2' })
    if (!ok) return
  }
  await save.run()
}

async function onDeleteAll() {
  const ok = await confirm({ title: 'Delete the CORS configuration?', description: 'The server-wide CORS defaults apply again.', confirmLabel: 'Delete', icon: 'i-lucide-trash-2' })
  if (!ok) return
  rules.value = []
  await save.run()
}

async function reset() {
  if (dirty.value && !(await confirm({ title: 'Discard changes?', confirmLabel: 'Discard', color: 'warning' }))) return
  rules.value = (JSON.parse(originalJson.value) as Parameters<typeof corsRuleToForm>[0][]).map(corsRuleToForm)
  submitted.value = false
}

const baseId = useId()
const fieldError = (i: number, k: keyof ReturnType<typeof validateCorsRule>) => (submitted.value ? errors.value[i]?.[k] : undefined)
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 class="font-semibold text-highlighted">
            CORS
          </h3>
          <p class="text-sm text-muted">
            Which web origins may access this bucket from a browser. Overrides the server-wide CORS defaults.
          </p>
        </div>
        <div class="flex items-center gap-2">
          <UBadge v-if="dirty" label="Unsaved" color="warning" variant="subtle" />
          <UTabs v-model="view" :items="viewItems" :content="false" size="xs" aria-label="CORS view" />
        </div>
      </div>
    </template>

    <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Could not load CORS configuration" retryable @retry="loader.refresh" />
    <USkeleton v-else-if="loader.loading.value && !loader.data.value" class="h-48 w-full" />
    <div v-else class="space-y-4">
      <template v-if="view === 'editor'">
        <CommonEmptyState v-if="!rules.length" compact icon="i-lucide-globe" title="No CORS rules" description="Browsers on other origins cannot read from or write to this bucket.">
          <UButton size="sm" icon="i-lucide-plus" label="Add rule" @click="addRule" />
        </CommonEmptyState>
        <div
          v-for="(rule, i) in rules"
          :key="i"
          role="group"
          :aria-labelledby="`${baseId}-rule-${i}`"
          class="space-y-3 rounded-md border border-default p-3"
          :class="submitted && hasErrors(errors[i] ?? {}) ? 'border-error/60' : ''"
        >
          <p :id="`${baseId}-rule-${i}`" class="text-sm font-medium text-highlighted">
            Rule {{ i + 1 }}<span v-if="rule.id" class="font-normal text-muted"> · {{ rule.id }}</span>
          </p>
          <div class="grid gap-3 md:grid-cols-2">
            <UFormField label="Allowed origins" :name="`cors-${i}-origins`" required help="“*” or e.g. https://app.example.com (one * allowed)" :error="fieldError(i, 'allowedOrigins')">
              <AdminListInput v-model="rule.allowedOrigins" placeholder="https://app.example.com" />
            </UFormField>
            <UFormField label="Allowed methods" :name="`cors-${i}-methods`" required :error="fieldError(i, 'allowedMethods')">
              <div class="flex flex-wrap gap-x-4 gap-y-2 pt-1" role="group" :aria-label="`Allowed methods of rule ${i + 1}`">
                <UCheckbox
                  v-for="m in CORS_METHODS"
                  :key="m"
                  :model-value="rule.allowedMethods.includes(m)"
                  :label="m"
                  @update:model-value="toggleMethod(rule, m, $event)"
                />
              </div>
            </UFormField>
            <UFormField label="Allowed headers" :name="`cors-${i}-headers`" help="Request headers (“*” = any)" :error="fieldError(i, 'allowedHeaders')">
              <AdminListInput v-model="rule.allowedHeaders" placeholder="*" />
            </UFormField>
            <UFormField label="Exposed headers" :name="`cors-${i}-expose`" help="Response headers scripts may read (e.g. ETag)" :error="fieldError(i, 'exposeHeaders')">
              <AdminListInput v-model="rule.exposeHeaders" placeholder="ETag" />
            </UFormField>
            <UFormField label="Max age (seconds)" :name="`cors-${i}-maxage`" :error="fieldError(i, 'maxAgeSeconds')">
              <UInputNumber v-model="rule.maxAgeSeconds" :min="0" :step="60" class="w-full" placeholder="none" />
            </UFormField>
            <UFormField label="Rule ID" :name="`cors-${i}-id`" help="Optional" :error="fieldError(i, 'id')">
              <UInput v-model="rule.id" class="w-full" placeholder="optional" />
            </UFormField>
          </div>
          <div class="flex justify-end">
            <UButton size="xs" color="error" variant="ghost" icon="i-lucide-trash-2" :label="`Remove rule ${i + 1}`" @click="removeRule(i)" />
          </div>
        </div>
        <UButton
          v-if="rules.length"
          size="sm"
          color="neutral"
          variant="outline"
          icon="i-lucide-plus"
          label="Add rule"
          :disabled="rules.length >= MAX_CORS_RULES"
          @click="addRule"
        />
      </template>
      <div v-else class="space-y-2">
        <div class="flex items-center justify-between">
          <span class="text-xs text-muted">Generated CORSConfiguration (read-only)</span>
          <CommonCopyButton :value="xml" aria-label="Copy CORS XML" />
        </div>
        <pre class="max-h-[28rem] overflow-auto whitespace-pre-wrap break-all rounded-md border border-default bg-elevated/50 p-3 font-mono text-xs">{{ xml }}</pre>
      </div>

      <div class="flex flex-wrap justify-between gap-2 border-t border-default pt-4">
        <UButton v-if="exists" color="error" variant="soft" icon="i-lucide-trash-2" label="Delete configuration" :loading="save.loading.value" @click="onDeleteAll" />
        <span v-else />
        <div class="flex gap-2">
          <UButton color="neutral" variant="ghost" icon="i-lucide-undo-2" label="Reset" :disabled="!dirty" @click="reset" />
          <UButton icon="i-lucide-save" label="Save CORS" :disabled="!dirty" :loading="save.loading.value" @click="onSave" />
        </div>
      </div>
    </div>
  </UCard>
</template>
