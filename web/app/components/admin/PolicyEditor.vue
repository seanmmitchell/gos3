<script setup lang="ts">
import type { NamedPolicy } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { ADMIN_RESOURCE_FORMS, emptyIdentityPolicy, formatPolicy, parsePolicyText, validatePolicyDocument, validatePolicyName } from './-policies'

/**
 * Create / edit / view an IAM identity policy (PUT /policies/{name}).
 * Built-in policies are shown read-only. Validation errors from the API
 * (InvalidPolicy / InvalidName) are shown inline next to the field.
 */
const props = withDefaults(defineProps<{
  mode: 'create' | 'edit' | 'view'
  policy?: NamedPolicy | null
  /** Built-in policy names (cannot be used for new policies). */
  builtinNames?: string[]
  /** Existing policy names (duplicate check on create). */
  existingNames?: string[]
}>(), { policy: null, builtinNames: () => [], existingNames: () => [] })

const emit = defineEmits<{
  saved: [policy: NamedPolicy]
  cancel: []
  dirty: [dirty: boolean]
}>()

const { requireAdmin } = useConnection()
const confirm = useConfirm()
const toast = useToast()

const name = ref('')
const description = ref('')
const text = ref('')
const original = ref({ description: '', text: '' })

function load() {
  name.value = props.policy?.name ?? ''
  description.value = props.policy?.description ?? ''
  text.value = props.policy ? formatPolicy(props.policy.document) : emptyIdentityPolicy()
  original.value = { description: description.value, text: text.value }
  nameServerError.value = undefined
  policyServerError.value = undefined
  serverError.value = null
}

const submitted = ref(false)
const saving = ref(false)
const nameServerError = ref<string>()
const policyServerError = ref<string>()
const serverError = ref<unknown>(null)

watch(() => props.policy, load, { immediate: true })
watch(name, () => (nameServerError.value = undefined))
watch(text, () => (policyServerError.value = undefined))

const readonly = computed(() => props.mode === 'view')
const dirty = computed(() => !readonly.value && (description.value !== original.value.description || text.value !== original.value.text || (props.mode === 'create' && !!name.value)))
watch(dirty, d => emit('dirty', d), { immediate: true })

const nameError = computed(() => {
  if (props.mode !== 'create') return undefined
  if (nameServerError.value) return nameServerError.value
  const n = name.value.trim()
  const e = validatePolicyName(n, props.builtinNames)
  if (e) return e
  if (props.existingNames.includes(n)) return `Policy "${n}" already exists`
  return undefined
})
const descriptionError = computed(() => (description.value.length > 1024 ? 'At most 1024 characters' : undefined))
const validateDoc = (v: unknown) => validatePolicyDocument(v, 'identity')
const showAdminHelp = ref(false)

async function applyTemplate(rendered: string) {
  if (text.value.trim() && text.value !== original.value.text && text.value !== emptyIdentityPolicy()) {
    const ok = await confirm({ title: 'Replace the current document?', description: 'Your unsaved changes to the policy document will be lost.', confirmLabel: 'Replace', color: 'warning' })
    if (!ok) return
  }
  // No toast: the editor shows the new document, and in the create dialog a
  // toast would cover the dialog's footer buttons.
  text.value = rendered
}

async function save() {
  submitted.value = true
  serverError.value = null
  const parsed = parsePolicyText(text.value, 'identity')
  if (nameError.value || descriptionError.value || parsed.error || !parsed.doc) return
  const policyName = props.mode === 'create' ? name.value.trim() : props.policy?.name ?? ''
  saving.value = true
  try {
    const saved = await requireAdmin().putPolicy(policyName, { description: description.value.trim() || undefined, document: parsed.doc })
    original.value = { description: description.value, text: text.value }
    toast.add({ title: props.mode === 'create' ? `Policy ${policyName} created` : `Policy ${policyName} saved`, color: 'success', icon: 'i-lucide-circle-check' })
    emit('saved', saved)
  } catch (e) {
    if (e instanceof AdminError && e.code === 'InvalidPolicy') policyServerError.value = e.message
    else if (e instanceof AdminError && e.code === 'InvalidName') nameServerError.value = e.message
    else serverError.value = e
  } finally {
    saving.value = false
  }
}

async function reset() {
  if (dirty.value && !(await confirm({ title: 'Discard changes?', confirmLabel: 'Discard', color: 'warning' }))) return
  load()
}

</script>

<template>
  <form class="space-y-4" novalidate @submit.prevent="save">
    <CommonErrorAlert :error="serverError" title="Could not save policy" />
    <UAlert
      v-if="mode === 'view'"
      color="info"
      variant="subtle"
      icon="i-lucide-lock"
      title="Built-in policy"
      description="Built-in policies cannot be changed. Create a custom policy to grant different permissions."
    />
    <div class="grid gap-4 md:grid-cols-2">
      <UFormField
        v-if="mode === 'create'"
        label="Policy name"
        name="name"
        required
        help="1–128 characters: letters, digits and + = , . @ _ -"
        :error="(submitted || !!nameServerError) && nameError"
      >
        <UInput v-model="name" class="w-full" autocomplete="off" spellcheck="false" placeholder="photos-readwrite" autofocus />
      </UFormField>
      <UFormField label="Description" name="description" :error="descriptionError" :class="mode === 'create' ? '' : 'md:col-span-2'">
        <UInput v-model="description" class="w-full" :readonly="readonly" placeholder="Optional" />
      </UFormField>
    </div>
    <AdminPolicyTemplatePicker v-if="!readonly" @apply="applyTemplate" />
    <div class="space-y-2">
      <CommonJsonEditor v-model="text" label="Policy document" :rows="18" :readonly="readonly" :validate="validateDoc" />
      <UCollapsible v-model:open="showAdminHelp">
        <UButton
          color="neutral"
          variant="link"
          size="xs"
          :icon="showAdminHelp ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'"
          label="Admin resources and condition keys"
          class="px-0"
        />
        <template #content>
          <div class="space-y-2 pt-1 text-xs text-muted" data-testid="admin-resource-help">
            <p>Admin actions (admin:…) are evaluated against these resources; "*" and arn:gos3:admin:::* match all of them. Wildcards are allowed in names.</p>
            <ul class="space-y-1">
              <li v-for="f in ADMIN_RESOURCE_FORMS" :key="f.arn">
                <code class="font-mono text-highlighted">{{ f.arn }}</code> — {{ f.description }}
              </li>
            </ul>
            <p>
              Condition key <code class="font-mono text-highlighted">admin:PolicyName</code> (multi-valued) lists the policies a CreateUser / UpdateUser request attaches, e.g. <code class="font-mono">"ForAllValues:StringEquals": {"admin:PolicyName": ["readonly"]}</code>.
            </p>
          </div>
        </template>
      </UCollapsible>
      <UAlert
        v-if="policyServerError"
        color="error"
        variant="subtle"
        icon="i-lucide-circle-alert"
        title="The server rejected the policy"
        :description="policyServerError"
        role="alert"
      />
    </div>
    <div v-if="!readonly" class="flex flex-wrap justify-end gap-2">
      <UButton v-if="mode === 'create'" color="neutral" variant="ghost" label="Cancel" :disabled="saving" @click="emit('cancel')" />
      <UButton v-else color="neutral" variant="ghost" icon="i-lucide-undo-2" label="Reset" :disabled="!dirty || saving" @click="reset" />
      <UButton
        type="submit"
        icon="i-lucide-save"
        :label="mode === 'create' ? 'Create policy' : 'Save policy'"
        :loading="saving"
        :disabled="mode === 'edit' && !dirty"
      />
    </div>
  </form>
</template>
