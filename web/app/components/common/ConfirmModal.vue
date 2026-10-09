<script setup lang="ts">
/**
 * Confirmation dialog backing useConfirm(). Emits close(true|false).
 * Opened through useOverlay(), which binds v-model:open on the root UModal.
 */
const props = withDefaults(defineProps<{
  title: string
  description?: string
  confirmLabel?: string
  cancelLabel?: string
  color?: 'error' | 'primary' | 'warning' | 'neutral'
  icon?: string
  requireText?: string
}>(), {
  description: undefined,
  confirmLabel: 'Confirm',
  cancelLabel: 'Cancel',
  color: 'error',
  icon: undefined,
  requireText: undefined
})

const emit = defineEmits<{ close: [value: boolean] }>()

const typed = ref('')
const canConfirm = computed(() => !props.requireText || typed.value === props.requireText)
const inputId = useId()

function confirm() {
  if (canConfirm.value) emit('close', true)
}
</script>

<template>
  <UModal
    :title="title"
    :description="description"
    :close="false"
    :ui="{ footer: 'justify-end' }"
  >
    <template v-if="icon || requireText" #body>
      <div class="flex items-start gap-3">
        <UIcon
          v-if="icon"
          :name="icon"
          class="mt-0.5 size-5 shrink-0"
          :class="color === 'error' ? 'text-error' : color === 'warning' ? 'text-warning' : 'text-primary'"
          aria-hidden="true"
        />
        <form v-if="requireText" class="w-full space-y-2" @submit.prevent="confirm">
          <label :for="inputId" class="block text-sm text-muted">
            Type <code class="rounded bg-elevated px-1 py-0.5 font-mono text-highlighted">{{ requireText }}</code> to confirm.
          </label>
          <UInput :id="inputId" v-model="typed" class="w-full" autocomplete="off" autofocus />
        </form>
        <p v-else class="text-sm text-muted">
          This action cannot be undone.
        </p>
      </div>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" :label="cancelLabel" @click="emit('close', false)" />
      <UButton :color="color" :label="confirmLabel" :disabled="!canConfirm" @click="confirm" />
    </template>
  </UModal>
</template>
