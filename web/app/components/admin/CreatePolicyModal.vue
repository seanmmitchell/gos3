<script setup lang="ts">
import type { NamedPolicy } from '~/types/admin'

/**
 * Wide dialog around AdminPolicyEditor in create mode. Opened through
 * useOverlay(); emits close(policy) once created, close(null) on cancel.
 */
defineProps<{
  builtinNames?: string[]
  existingNames?: string[]
}>()

const emit = defineEmits<{ close: [value: NamedPolicy | null] }>()
</script>

<template>
  <UModal
    title="Create policy"
    description="Identity policies are attached to users and grant or deny S3 and admin actions."
    :ui="{ content: 'sm:max-w-4xl' }"
  >
    <template #body>
      <AdminPolicyEditor
        mode="create"
        :builtin-names="builtinNames"
        :existing-names="existingNames"
        @saved="emit('close', $event)"
        @cancel="emit('close', null)"
      />
    </template>
  </UModal>
</template>
