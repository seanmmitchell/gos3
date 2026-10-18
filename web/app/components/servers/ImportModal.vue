<script setup lang="ts">
/**
 * Import connections from a JSON file or pasted text: an export file
 * ({type:'gos3-connections', connections}), a servers.json ({servers}) or a
 * plain array. Entries matching an existing connection update it, but an
 * import never points an existing connection (and its stored secret) at
 * another endpoint or kind, and never changes embedded/preset connections:
 * such entries are added as new connections. The preview lists what each
 * entry will do before the user confirms (planConnectionImport).
 */
import type { ImportField, ImportPlan, ImportStep } from '~/utils/connections'
import { useConnectionsStore } from '~/stores/connections'

const open = defineModel<boolean>('open', { required: true })
const emit = defineEmits<{ imported: [result: { added: number, updated: number, skipped: number }] }>()

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

const preview = computed<{ count: number, plan?: ImportPlan, error?: string }>(() => {
  if (!text.value.trim()) return { count: 0 }
  try {
    const plan = store.planImport(text.value)
    return { count: plan.steps.length, plan }
  } catch (e) {
    return { count: 0, error: e instanceof Error ? `Invalid JSON: ${e.message}` : 'Invalid JSON' }
  }
})

const FIELD_LABELS: Record<ImportField, string> = {
  name: 'name',
  region: 'region',
  accessKeyId: 'access key',
  secretAccessKey: 'secret key',
  forcePathStyle: 'path-style',
  serverId: 'server ID',
  color: 'color',
  remember: 'remember secret'
}

interface PreviewRow {
  key: string
  icon: string
  color: string
  title: string
  detail?: string
}

function label(c: { name?: string, endpoint: string }): string {
  return c.name ? `${c.name} (${c.endpoint})` : c.endpoint
}

function row(step: ImportStep, i: number): PreviewRow {
  const key = String(i)
  switch (step.action) {
    case 'add': {
      const title = `Add ${label(step.input)}${step.input.accessKeyId ? ' with the access key from the file' : ' without credentials'}`
      if (!step.instead) return { key, icon: 'i-lucide-plus', color: 'text-success', title }
      const of = step.instead.of
      const why = step.instead.reason === 'protected'
        ? `"${of.name}" is a built-in ${of.source === 'embedded' ? 'embedded' : 'preset'} server and is never changed by an import`
        : `"${of.name}" points to ${of.endpoint}${step.input.kind && step.input.kind !== of.kind ? ` (${of.kind})` : ''}; it keeps its endpoint and stored credentials${step.input.proxyConnectionId !== of.proxyConnectionId ? "; its fallback relay also stays unchanged" : ""}`
      return { key, icon: 'i-lucide-copy-plus', color: 'text-warning', title: `${title} as a separate server`, detail: `Matches ${why}.` }
    }
    case 'update':
      return {
        key,
        icon: 'i-lucide-pencil',
        color: 'text-info',
        title: `Update ${label(step.target)}`,
        detail: step.changes.map(ch => `${FIELD_LABELS[ch.field]}: ${ch.from} → ${ch.to}`).join('; ')
      }
    case 'skip':
      return step.reason === 'protected'
        ? { key, icon: 'i-lucide-lock', color: 'text-muted', title: `Skip ${label(step.target)}`, detail: 'Built-in servers (embedded or preset) are never changed by an import.' }
        : { key, icon: 'i-lucide-equal', color: 'text-muted', title: `Skip ${label(step.target)}`, detail: 'Already up to date.' }
  }
}

const rows = computed(() => preview.value.plan?.steps.map(row) ?? [])

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
    const description = `${r.added} added, ${r.updated} updated${r.skipped ? `, ${r.skipped} skipped` : ''}.`
    toast.add({ title: 'Servers imported', description, color: 'success', icon: 'i-lucide-circle-check' })
    void store.refreshAll()
    emit('imported', r)
    open.value = false
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    title="Import servers"
    description="Load a JSON export or a servers.json file. Existing servers with the same ID or endpoint are updated; an entry that points one of them elsewhere is added as a separate server."
    :ui="{ footer: 'justify-end gap-2' }"
  >
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
        <section v-if="rows.length" aria-labelledby="import-preview-title" class="space-y-2">
          <h3 id="import-preview-title" class="text-sm font-medium">
            What the import will do
          </h3>
          <ul class="max-h-60 space-y-2 overflow-y-auto text-sm" aria-label="Import preview">
            <li v-for="r in rows" :key="r.key" class="flex gap-2">
              <UIcon :name="r.icon" class="mt-0.5 size-4 shrink-0" :class="r.color" aria-hidden="true" />
              <div class="min-w-0">
                <p class="break-words">
                  {{ r.title }}
                </p>
                <p v-if="r.detail" class="break-words text-xs text-muted">
                  {{ r.detail }}
                </p>
              </div>
            </li>
          </ul>
        </section>
      </form>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Cancel" @click="open = false" />
      <UButton type="submit" form="import-form" icon="i-lucide-upload" label="Import" :disabled="!preview.count" />
    </template>
  </UModal>
</template>
