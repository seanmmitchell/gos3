<script setup lang="ts">
import type { CreateUserResponse } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { validateUserName } from './-policies'

/**
 * Create-user dialog (POST /users). Opened through useOverlay(); emits
 * close(response) after the user was created (the response carries the new
 * access key when "create access key now" was checked) or close(null).
 */
const props = withDefaults(defineProps<{
  /** Policy names offered in the multi-select (from GET /policies). */
  policies?: string[]
  /** Names that already exist (instant duplicate check). */
  existing?: string[]
}>(), { policies: () => [], existing: () => [] })

const emit = defineEmits<{ close: [value: CreateUserResponse | null] }>()

const { requireAdmin } = useConnection()

const name = ref('')
const description = ref('')
const selected = ref<string[]>([])
const disabled = ref(false)
const createKey = ref(true)
const submitted = ref(false)
const saving = ref(false)
const serverError = ref<unknown>(null)
const nameServerError = ref<string>()

const nameError = computed(() => {
  const n = name.value.trim()
  if (nameServerError.value) return nameServerError.value
  const e = validateUserName(n)
  if (e) return e
  if (props.existing.includes(n)) return `User "${n}" already exists`
  return undefined
})
const descriptionError = computed(() => (description.value.length > 1024 ? 'At most 1024 characters' : undefined))

watch(name, () => (nameServerError.value = undefined))

async function submit() {
  submitted.value = true
  serverError.value = null
  if (nameError.value || descriptionError.value) return
  saving.value = true
  try {
    const res = await requireAdmin().createUser({
      name: name.value.trim(),
      description: description.value.trim() || undefined,
      policies: selected.value,
      disabled: disabled.value,
      createKey: createKey.value
    })
    emit('close', res)
  } catch (e) {
    if (e instanceof AdminError && (e.code === 'InvalidName' || e.code === 'Conflict')) {
      nameServerError.value = e.code === 'Conflict' ? `User "${name.value.trim()}" already exists` : e.message
    } else {
      serverError.value = e
    }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <UModal title="Create user" description="IAM users sign requests with their own access keys." :ui="{ footer: 'justify-end' }">
    <template #body>
      <form id="create-user-form" class="space-y-4" novalidate @submit.prevent="submit">
        <CommonErrorAlert :error="serverError" title="Could not create user" />
        <UFormField
          label="User name"
          name="name"
          required
          help="1–64 characters: letters, digits and + = , . @ _ -"
          :error="(submitted || !!nameServerError) && nameError"
        >
          <UInput v-model="name" class="w-full" autocomplete="off" spellcheck="false" autofocus placeholder="alice" />
        </UFormField>
        <UFormField label="Description" name="description" :error="descriptionError">
          <UInput v-model="description" class="w-full" placeholder="Optional" />
        </UFormField>
        <UFormField label="Policies" name="policies" help="Without policies the user can only use resources granted by bucket policies.">
          <USelectMenu
            v-model="selected"
            :items="policies"
            multiple
            class="w-full"
            placeholder="Select policies"
            :search-input="{ placeholder: 'Filter policies…' }"
          />
        </UFormField>
        <div class="space-y-3">
          <UCheckbox v-model="createKey" label="Create an access key now" description="The secret key is shown once after the user is created." />
          <UCheckbox v-model="disabled" label="Create disabled" description="Disabled users cannot authenticate." />
        </div>
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" :disabled="saving" @click="emit('close', null)" />
      <UButton type="submit" form="create-user-form" icon="i-lucide-user-plus" label="Create user" :loading="saving" />
    </template>
  </UModal>
</template>
