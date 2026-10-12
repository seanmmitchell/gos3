<script setup lang="ts">
/**
 * Lightweight JSON editor: monospace textarea with optional line numbers,
 * live validation and a "Format" button. No heavy editor dependency.
 *
 *   <CommonJsonEditor v-model="policyText" label="Policy" @validity="ok = $event" />
 */
const model = defineModel<string>({ required: true })

const props = withDefaults(defineProps<{
  label?: string
  rows?: number
  readonly?: boolean
  placeholder?: string
  lineNumbers?: boolean
  /** Allow an empty document (treated as valid). */
  allowEmpty?: boolean
  /** Extra validation on the parsed value; return an error message or undefined. */
  validate?: (value: unknown) => string | undefined
}>(), {
  label: undefined,
  rows: 14,
  readonly: false,
  placeholder: '{\n  \n}',
  lineNumbers: true,
  allowEmpty: false,
  validate: undefined
})

const emit = defineEmits<{ validity: [valid: boolean] }>()

const id = useId()
const textarea = ref<HTMLTextAreaElement | null>(null)
const gutter = ref<HTMLElement | null>(null)

const error = computed<string | undefined>(() => {
  const text = model.value ?? ''
  if (!text.trim()) return props.allowEmpty ? undefined : 'Document is empty'
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch (e) {
    return e instanceof Error ? e.message : 'Invalid JSON'
  }
  return props.validate?.(parsed)
})

watch(error, e => emit('validity', !e), { immediate: true })

const lineCount = computed(() => Math.max(1, (model.value ?? '').split('\n').length))

function format() {
  try {
    model.value = JSON.stringify(JSON.parse(model.value), null, 2)
  } catch {
    // keep as is; the error is displayed
  }
}

function syncScroll() {
  if (gutter.value && textarea.value) gutter.value.scrollTop = textarea.value.scrollTop
}

function onKeydown(e: KeyboardEvent) {
  if (e.key !== 'Tab' || props.readonly || e.shiftKey || e.ctrlKey || e.metaKey || e.altKey) return
  // Insert two spaces instead of moving focus. Escape then Tab still leaves the field.
  const el = textarea.value
  if (!el || tabEscapes.value) return
  e.preventDefault()
  const { selectionStart: s, selectionEnd: end } = el
  model.value = `${model.value.slice(0, s)}  ${model.value.slice(end)}`
  nextTick(() => el.setSelectionRange(s + 2, s + 2))
}

// After Escape, the next Tab moves focus (keyboard users are never trapped).
const tabEscapes = ref(false)
function onKeyup(e: KeyboardEvent) {
  if (e.key === 'Escape') tabEscapes.value = true
  else if (e.key !== 'Tab') tabEscapes.value = false
}
</script>

<template>
  <div class="space-y-1.5">
    <div class="flex items-center justify-between gap-2">
      <label v-if="label" :for="id" class="text-sm font-medium text-default">{{ label }}</label>
      <span v-else />
      <div class="flex items-center gap-1">
        <slot name="toolbar" />
        <UButton
          v-if="!readonly"
          size="xs"
          color="neutral"
          variant="ghost"
          icon="i-lucide-wand-sparkles"
          label="Format"
          :disabled="!!error && !!model.trim()"
          @click="format"
        />
        <CommonCopyButton :value="model" aria-label="Copy JSON" />
      </div>
    </div>
    <div
      class="flex overflow-hidden rounded-md border bg-default focus-within:ring-2 focus-within:ring-primary"
      :class="error && model.trim() ? 'border-error' : 'border-default'"
    >
      <div
        v-if="lineNumbers"
        ref="gutter"
        class="select-none overflow-hidden border-r border-default bg-elevated/50 px-2 py-2 text-right font-mono text-xs leading-5 text-dimmed"
        aria-hidden="true"
        :style="{ maxHeight: `${rows * 1.25 + 1}rem` }"
      >
        <div v-for="n in lineCount" :key="n">
          {{ n }}
        </div>
      </div>
      <textarea
        :id="id"
        ref="textarea"
        v-model="model"
        :rows="rows"
        :readonly="readonly"
        :placeholder="placeholder"
        :aria-invalid="!!error"
        :aria-describedby="`${id}-status`"
        spellcheck="false"
        autocapitalize="off"
        autocomplete="off"
        class="block w-full resize-y bg-transparent px-3 py-2 font-mono text-xs leading-5 text-default outline-none placeholder:text-dimmed"
        :style="{ minHeight: `${Math.min(rows, 6) * 1.25 + 1}rem` }"
        @scroll="syncScroll"
        @keydown="onKeydown"
        @keyup="onKeyup"
      />
    </div>
    <p :id="`${id}-status`" class="flex items-center gap-1 text-xs" :class="error ? 'text-error' : 'text-success'" aria-live="polite">
      <UIcon :name="error ? 'i-lucide-circle-alert' : 'i-lucide-circle-check'" class="size-3.5" aria-hidden="true" />
      {{ error ?? 'Valid JSON' }}
    </p>
  </div>
</template>
