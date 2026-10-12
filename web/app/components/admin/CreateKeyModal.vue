<script setup lang="ts">
import type { CreateAccessKeyRequest } from '~/types/admin'
import { validateAccessKeyId, validateSecretKey } from './-credentials'

/**
 * Asks for the description (and, for admins, an optional caller-supplied key
 * pair) of a new access key. Opened through useOverlay(); emits
 * close(request) or close(null) when cancelled. The caller creates the key.
 */
const props = withDefaults(defineProps<{
  title?: string
  /** Allow specifying the access key / secret (admin endpoint only). */
  allowCustom?: boolean
}>(), { title: 'Create access key', allowCustom: false })

const emit = defineEmits<{ close: [value: CreateAccessKeyRequest | null] }>()

const description = ref('')
const custom = ref(false)
const accessKey = ref('')
const secretKey = ref('')
const submitted = ref(false)

const errors = computed(() => {
  const e: { description?: string, accessKey?: string, secretKey?: string } = {}
  if (description.value.length > 1024) e.description = 'At most 1024 characters'
  if (props.allowCustom && custom.value) {
    e.accessKey = validateAccessKeyId(accessKey.value.trim())
    e.secretKey = validateSecretKey(secretKey.value)
  }
  return e
})
const valid = computed(() => !errors.value.description && !errors.value.accessKey && !errors.value.secretKey)

function submit() {
  submitted.value = true
  if (!valid.value) return
  const req: CreateAccessKeyRequest = {}
  if (description.value.trim()) req.description = description.value.trim()
  if (props.allowCustom && custom.value) {
    req.accessKey = accessKey.value.trim()
    req.secretKey = secretKey.value
  }
  emit('close', req)
}
</script>

<template>
  <UModal :title="title" description="The secret key is shown once after creation." :ui="{ footer: 'justify-end' }">
    <template #body>
      <form id="create-key-form" class="space-y-4" novalidate @submit.prevent="submit">
        <UFormField label="Description" name="description" help="Optional, e.g. the application using the key." :error="submitted && errors.description">
          <UInput v-model="description" class="w-full" placeholder="backup job" autofocus />
        </UFormField>
        <template v-if="allowCustom">
          <USwitch v-model="custom" label="Specify the key pair" description="Leave off to generate a random key pair (recommended)." />
          <template v-if="custom">
            <UFormField label="Access key ID" name="accessKey" required :error="submitted && errors.accessKey">
              <UInput v-model="accessKey" class="w-full font-mono" autocomplete="off" spellcheck="false" />
            </UFormField>
            <UFormField label="Secret access key" name="secretKey" required :error="submitted && errors.secretKey">
              <UInput v-model="secretKey" type="password" class="w-full font-mono" autocomplete="new-password" />
            </UFormField>
          </template>
        </template>
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="emit('close', null)" />
      <UButton type="submit" form="create-key-form" icon="i-lucide-key-round" label="Create key" />
    </template>
  </UModal>
</template>
