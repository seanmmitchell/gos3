<script setup lang="ts">
/**
 * Editable list of strings (domains, origins, seeds, headers…) as tags.
 * Enter, comma, Tab, blur or paste add items; pasted text is split on
 * whitespace and commas. Duplicates are ignored.
 *
 *   <UFormField label="Domains" :error="errors.domains">
 *     <AdminListInput v-model="form.domains" placeholder="s3.example.com" />
 *   </UFormField>
 */
const model = defineModel<string[]>({ required: true })

withDefaults(defineProps<{
  placeholder?: string
  disabled?: boolean
  /** Maximum number of items. */
  max?: number
  /** Render items in a monospace font. */
  mono?: boolean
}>(), { placeholder: 'Add and press Enter', disabled: false, max: undefined, mono: true })

function onUpdate(v: unknown) {
  const items = Array.isArray(v) ? v.map(x => String(x)) : []
  // Split pasted multi-value strings ("a, b c") and drop duplicates / empties.
  const out: string[] = []
  for (const it of items) {
    for (const part of it.split(/[\s,]+/)) {
      const p = part.trim()
      if (p && !out.includes(p)) out.push(p)
    }
  }
  model.value = out
}
</script>

<template>
  <UInputTags
    :model-value="model"
    :placeholder="placeholder"
    :disabled="disabled"
    :max="max"
    add-on-paste
    add-on-blur
    add-on-tab
    :delimiter="','"
    class="w-full"
    :ui="{ item: mono ? 'font-mono' : undefined }"
    @update:model-value="onUpdate"
  />
</template>
