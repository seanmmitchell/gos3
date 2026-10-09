<script setup lang="ts">
/**
 * Import connections from a JSON file or pasted text: an export file
 * ({type:'gos3-connections', connections}), a servers.json ({servers}) or a
 * plain array. Entries matching an existing connection update it.
 */
import { useConnectionsStore } from '~/stores/connections'

const open = defineModel<boolean>('open', { required: true })
const emit = defineEmits<{ imported: [result: { added: number, updated: number }] }>()

const store = useConnectionsStore()
const toast = useToast()
const text = ref('')
const fileName = ref('')
const error = ref<string | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)

watch(open, (v) => {
  if (!v) return
  text.value = ''
  fileName.value = ''
  error.value = null
})

const preview = computed<{ count: number, error?: string }>(() => {
  if (!text.value.trim()) return { count: 0 }
  try {
    return { count: parseConnectionList(text.value).length }
  } catch (e) {
    return { count: 0, error: e instanceof Error ? `Invalid JSON: ${e.message}` : 'Invalid JSON' }
  }
})

async function onFile(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  if (file.size > 1024 * 1024) {
    error.value = 'The file is larger than 1 MiB.'
    return
  }
  fileName.value = file.name
  text.value = await file.text()
  error.value = null
  input.value = ''
}

function submit() {
  error.value = null
  if (preview.value.error) {
    error.value = preview.value.error
    return
  }
  if (!preview.value.count) {
    error.value = 'No server definitions found (each entry needs an "endpoint").'
    return
  }
  try {
    const r = store.importJson(text.value)
    toast.add({ title: 'Servers imported', description: `${r.added} added, ${r.updated} updated.`, color: 'success', icon: 'i-lucide-circle-check' })
    void store.refreshAll()
    emit('imported', r)
    open.value = false
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}
</script>

<template>
  <UModal v-model:open="open" title="Import servers" description="Load a JSON export or a servers.json file. Existing servers with the same ID or endpoint are updated." :ui="{ footer: 'justify-end gap-2' }">
    <template #body>
      <form id="import-form" class="space-y-4" @submit.prevent="submit">
        <div class="flex flex-wrap items-center gap-2">
          <input ref="fileInput" type="file" accept="application/json,.json" class="sr-only" aria-label="Choose a JSON file" @change="onFile">
          <UButton color="neutral" variant="outline" icon="i-lucide-file-json" label="Choose file…" @click="fileInput?.click()" />
          <span class="truncate text-sm text-muted">{{ fileName || 'or paste JSON below' }}</span>
        </div>
        <UFormField label="JSON" :error="error ?? preview.error" :help="preview.count ? `${preview.count} server${preview.count === 1 ? '' : 's'} found.` : undefined">
          <UTextarea v-model="text" :rows="10" class="w-full font-mono text-xs" placeholder="{ &quot;servers&quot;: [ { &quot;name&quot;: &quot;NAS&quot;, &quot;endpoint&quot;: &quot;http://10.0.0.5:9000&quot; } ] }" spellcheck="false" />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="open = false" />
      <UButton type="submit" form="import-form" icon="i-lucide-upload" label="Import" :disabled="!preview.count" />
    </template>
  </UModal>
</template>
