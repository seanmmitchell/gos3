<script setup lang="ts">
/** Export connections as JSON (download or copy), optionally with secrets. */
import { useConnectionsStore } from '~/stores/connections'

const open = defineModel<boolean>('open', { required: true })

const store = useConnectionsStore()
const toast = useToast()
const includeSecrets = ref(false)

watch(open, (v) => {
  if (v) includeSecrets.value = false
})

const json = computed(() => (open.value ? store.exportJson({ includeSecrets: includeSecrets.value }) : ''))
const count = computed(() => store.connections.length)
const withSecrets = computed(() => store.connections.filter(c => c.secretAccessKey).length)

function download() {
  const blob = new Blob([json.value], { type: 'application/json' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `gos3-connections-${new Date().toISOString().slice(0, 10)}${includeSecrets.value ? '-with-secrets' : ''}.json`
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
  toast.add({ title: 'Export downloaded', icon: 'i-lucide-download', color: 'success' })
}

async function copy() {
  try {
    await navigator.clipboard.writeText(json.value)
    toast.add({ title: 'Copied to the clipboard', icon: 'i-lucide-clipboard-check', color: 'success' })
  } catch {
    toast.add({ title: 'Could not copy', description: 'Your browser blocked clipboard access; use Download instead.', color: 'error', icon: 'i-lucide-circle-alert' })
  }
}
</script>

<template>
  <UModal v-model:open="open" title="Export servers" :description="`${count} server${count === 1 ? '' : 's'} as JSON — import the file in another browser or ship it as servers.json.`" :ui="{ footer: 'justify-end gap-2' }">
    <template #body>
      <div class="space-y-4">
        <USwitch
          v-model="includeSecrets"
          label="Include access keys and secret keys"
          :description="`${withSecrets} of ${count} connections currently have a secret key on this device.`"
        />
        <UAlert
          v-if="includeSecrets"
          color="warning"
          variant="subtle"
          icon="i-lucide-shield-alert"
          title="The file will contain plain-text secrets"
          description="Anyone who gets the file can use these credentials. Store it securely and delete it after use."
        />
        <label for="export-json" class="sr-only">Exported JSON</label>
        <UTextarea id="export-json" :model-value="json" readonly :rows="10" class="w-full font-mono text-xs" autoresize :maxrows="16" />
      </div>
    </template>
    <template #footer>
      <UButton color="neutral" variant="ghost" label="Close" @click="open = false" />
      <UButton color="neutral" variant="outline" icon="i-lucide-copy" label="Copy" @click="copy" />
      <UButton icon="i-lucide-download" label="Download" @click="download" />
    </template>
  </UModal>
</template>
