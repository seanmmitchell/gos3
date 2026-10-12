<script setup lang="ts">
/**
 * Renders any error (S3Error, AdminError, Error, string) as an alert with
 * code, message, hint and request ID. Emits `retry` when retryable.
 */
const props = withDefaults(defineProps<{
  error: unknown
  title?: string
  retryable?: boolean
}>(), { title: undefined, retryable: false })

const emit = defineEmits<{ retry: [] }>()

const d = computed(() => describeError(props.error))
const actions = computed(() =>
  props.retryable
    ? [{ label: 'Retry', icon: 'i-lucide-refresh-cw', color: 'neutral' as const, variant: 'outline' as const, onClick: () => emit('retry') }]
    : undefined
)
</script>

<template>
  <UAlert
    v-if="error"
    color="error"
    variant="subtle"
    icon="i-lucide-circle-alert"
    :title="title ?? d.title"
    :actions="actions"
    role="alert"
  >
    <template #description>
      <p>{{ d.message }}</p>
      <p v-if="d.hint" class="mt-1 opacity-80">
        {{ d.hint }}
      </p>
      <p v-if="title && d.code" class="mt-1 font-mono text-xs opacity-70">
        {{ d.code }}<template v-if="d.status">
          · HTTP {{ d.status }}
        </template>
      </p>
      <p v-if="d.requestId" class="mt-1 font-mono text-xs opacity-70">
        Request ID: {{ d.requestId }}
      </p>
    </template>
  </UAlert>
</template>
