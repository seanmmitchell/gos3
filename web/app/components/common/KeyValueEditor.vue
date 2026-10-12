<script setup lang="ts">
import type { KeyValuePair } from '~/types/ui'

/**
 * Editable list of key/value pairs (object metadata, tags).
 *
 *   <CommonKeyValueEditor v-model="tags" :max-items="10" key-label="Tag" :validate-key="k => …" />
 */
const model = defineModel<KeyValuePair[]>({ required: true })

const props = withDefaults(defineProps<{
  keyLabel?: string
  valueLabel?: string
  keyPlaceholder?: string
  valuePlaceholder?: string
  maxItems?: number
  readonly?: boolean
  addLabel?: string
  /** Return an error message for an invalid key. */
  validateKey?: (key: string) => string | undefined
  /** Return an error message for an invalid value. */
  validateValue?: (value: string) => string | undefined
}>(), {
  keyLabel: 'Key',
  valueLabel: 'Value',
  keyPlaceholder: 'key',
  valuePlaceholder: 'value',
  maxItems: undefined,
  readonly: false,
  addLabel: 'Add',
  validateKey: undefined,
  validateValue: undefined
})

const emit = defineEmits<{ validity: [valid: boolean] }>()
const baseId = useId()

const errors = computed(() => {
  const seen = new Map<string, number>()
  return model.value.map((p, i) => {
    const out: { key?: string, value?: string } = {}
    const k = p.key.trim()
    if (!k) out.key = 'Required'
    else if (seen.has(k)) out.key = 'Duplicate key'
    else out.key = props.validateKey?.(k)
    seen.set(k, i)
    out.value = props.validateValue?.(p.value)
    return out
  })
})

const valid = computed(() => errors.value.every(e => !e.key && !e.value))
watch(valid, v => emit('validity', v), { immediate: true })

const canAdd = computed(() => !props.readonly && (props.maxItems === undefined || model.value.length < props.maxItems))

function add() {
  if (!canAdd.value) return
  model.value = [...model.value, { key: '', value: '' }]
  nextTick(() => {
    const el = document.getElementById(`${baseId}-k-${model.value.length - 1}`)
    el?.focus()
  })
}

function removeAt(i: number) {
  model.value = model.value.filter((_, j) => j !== i)
}

function setAt(i: number, field: 'key' | 'value', v: string) {
  model.value = model.value.map((p, j) => (j === i ? { ...p, [field]: v } : p))
}
</script>

<template>
  <div class="space-y-2">
    <div v-if="model.length" class="hidden grid-cols-[1fr_1fr_auto] gap-2 text-xs font-medium text-muted sm:grid">
      <span>{{ keyLabel }}</span>
      <span>{{ valueLabel }}</span>
      <span class="w-8" />
    </div>
    <div
      v-for="(pair, i) in model"
      :key="i"
      class="grid grid-cols-1 gap-2 sm:grid-cols-[1fr_1fr_auto]"
    >
      <div>
        <label :for="`${baseId}-k-${i}`" class="sr-only">{{ keyLabel }} {{ i + 1 }}</label>
        <UInput
          :id="`${baseId}-k-${i}`"
          :model-value="pair.key"
          :placeholder="keyPlaceholder"
          :readonly="readonly"
          :color="errors[i]?.key ? 'error' : undefined"
          :aria-invalid="!!errors[i]?.key"
          class="w-full font-mono"
          size="sm"
          @update:model-value="setAt(i, 'key', String($event ?? ''))"
        />
        <p v-if="errors[i]?.key && pair.key" class="mt-0.5 text-xs text-error">
          {{ errors[i]?.key }}
        </p>
      </div>
      <div>
        <label :for="`${baseId}-v-${i}`" class="sr-only">{{ valueLabel }} {{ i + 1 }}</label>
        <UInput
          :id="`${baseId}-v-${i}`"
          :model-value="pair.value"
          :placeholder="valuePlaceholder"
          :readonly="readonly"
          :color="errors[i]?.value ? 'error' : undefined"
          :aria-invalid="!!errors[i]?.value"
          class="w-full font-mono"
          size="sm"
          @update:model-value="setAt(i, 'value', String($event ?? ''))"
        />
        <p v-if="errors[i]?.value" class="mt-0.5 text-xs text-error">
          {{ errors[i]?.value }}
        </p>
      </div>
      <UButton
        v-if="!readonly"
        icon="i-lucide-x"
        color="neutral"
        variant="ghost"
        size="sm"
        square
        :aria-label="`Remove ${keyLabel.toLowerCase()} ${pair.key || i + 1}`"
        @click="removeAt(i)"
      />
    </div>
    <p v-if="!model.length" class="text-sm text-muted">
      None.
    </p>
    <UButton
      v-if="!readonly"
      icon="i-lucide-plus"
      :label="addLabel"
      size="xs"
      color="neutral"
      variant="outline"
      :disabled="!canAdd"
      @click="add"
    />
    <span v-if="maxItems !== undefined && !readonly" class="ml-2 text-xs text-muted">{{ model.length }} / {{ maxItems }}</span>
  </div>
</template>
