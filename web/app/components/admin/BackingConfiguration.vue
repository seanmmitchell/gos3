<script setup lang="ts">
import type { Backing, BackingConfiguration, BackingDefinition } from '~/types/admin'
import { copyBackingConfiguration, validateBackingConfiguration } from './-backing-form'

const props = defineProps<{ backings: Backing[] }>()
const emit = defineEmits<{ saved: [] }>()
const { requireAdmin } = useConnection()
const expanded = ref(false)
const form = ref<BackingConfiguration>()
const original = ref<BackingConfiguration>()
let savedRows = new WeakSet<BackingDefinition>()
const loading = ref(false)
const error = ref<unknown>(null)
const submitted = ref(false)
const dirty = computed(() => JSON.stringify(form.value) !== JSON.stringify(original.value))
const validation = computed(() => form.value ? validateBackingConfiguration(form.value) : undefined)
const names = computed(() => ['default', ...(form.value?.backings.map(b => b.name).filter(Boolean) ?? [])])
const save = useAsyncAction(async () => {
  const result = await requireAdmin().putBackingConfiguration(copyBackingConfiguration(form.value!))
  setForm(result)
  emit('saved')
}, { successMessage: 'Backing configuration applied', errorTitle: 'Could not configure backings', toastErrors: false })

function inUse(name: string) {
  const b = props.backings.find(b => b.name === name)
  return !!b && (b.buckets > 0 || b.replicas > 0)
}

function setForm(result: BackingConfiguration) {
  original.value = copyBackingConfiguration(result)
  form.value = copyBackingConfiguration(result)
  savedRows = new WeakSet(form.value.backings)
}

async function load() {
  loading.value = true
  error.value = null
  try {
    const result = await requireAdmin().getBackingConfiguration()
    setForm(result)
    submitted.value = false
    save.error.value = null
  } catch (e) {
    error.value = e
  } finally {
    loading.value = false
  }
}

async function toggle() {
  expanded.value = !expanded.value
  if (expanded.value && !form.value) await load()
}

function remove(index: number) {
  const b = form.value!.backings[index]!
  if (form.value!.defaultBacking === b.name) form.value!.defaultBacking = 'default'
  form.value!.backings.splice(index, 1)
}

async function submit() {
  submitted.value = true
  if (!validation.value && form.value && !save.loading.value) await save.run()
}
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h2 class="font-semibold">Backing configuration</h2>
        <UButton color="neutral" variant="outline" icon="i-lucide-settings-2" :label="expanded ? 'Hide configuration' : 'Configure backings'" :aria-expanded="expanded" @click="toggle" />
      </div>
    </template>
    <p v-if="!expanded" class="text-sm text-muted">Add storage, change empty backings, or choose where new buckets are created. Changes apply immediately.</p>
    <div v-else class="space-y-4">
      <CommonErrorAlert :error="error" title="Could not load backing configuration" retryable @retry="load" />
      <USkeleton v-if="loading" class="h-32" />
      <form v-else-if="form" class="space-y-5" @submit.prevent="submit">
        <p class="text-sm text-muted">The system backing named default is configured at startup. Move buckets and remove replicas before changing or removing other backings. New filesystem backings must use an empty server directory.</p>
        <UAlert v-if="!form.persistent" color="warning" title="Changes are temporary" description="This server has no data directory for saved settings. Backing configuration will be lost on restart." />
        <CommonErrorAlert :error="save.error.value" title="Could not configure backings" />
        <UAlert v-if="submitted && validation" color="error" :title="validation" />
        <fieldset :disabled="save.loading.value" class="space-y-4">
          <div v-for="(b, index) in form.backings" :key="index" class="rounded-lg border border-default p-4 space-y-3">
            <div class="grid gap-3 sm:grid-cols-2">
              <UFormField label="Backing name" required>
                <UInput v-model="b.name" :disabled="savedRows.has(b)" placeholder="archive" class="w-full" aria-label="Backing name" />
              </UFormField>
              <UFormField label="Backend" required>
                <USelect v-model="b.backend" :disabled="inUse(b.name)" :items="[{ label: 'Filesystem', value: 'fs' }, { label: 'Memory', value: 'memory' }]" class="w-full" aria-label="Backend" @update:model-value="b.path = ''" />
              </UFormField>
            </div>
            <UFormField v-if="b.backend === 'fs'" label="Directory on the server" required>
              <UInput v-model="b.path" :disabled="inUse(b.name)" placeholder="/mnt/archive" class="w-full" aria-label="Directory on the server" />
            </UFormField>
            <p v-else class="text-sm text-muted">Memory backing contents are lost when the server restarts.</p>
            <div class="flex items-center justify-between gap-3">
              <span class="text-xs text-muted">{{ inUse(b.name) ? 'In use — move buckets and remove replicas before editing.' : 'Changes take effect when you save.' }}</span>
              <UButton color="error" variant="ghost" label="Remove" icon="i-lucide-trash-2" :disabled="inUse(b.name)" @click="remove(index)" />
            </div>
          </div>
          <UButton color="neutral" variant="outline" label="Add backing" icon="i-lucide-plus" :disabled="form.backings.length >= 64" @click="form.backings.push({ name: '', backend: 'fs', path: '' })" />
          <UFormField label="Default backing for new buckets" help="Existing buckets stay where they are.">
            <USelect v-model="form.defaultBacking" :items="names" class="w-full sm:w-72" aria-label="Default backing for new buckets" />
          </UFormField>
        </fieldset>
        <div class="flex flex-wrap gap-2">
          <UButton type="submit" label="Save backing configuration" icon="i-lucide-save" :loading="save.loading.value" :disabled="!dirty" />
          <UButton color="neutral" variant="ghost" label="Reload configuration" :disabled="save.loading.value" @click="load" />
        </div>
      </form>
    </div>
  </UCard>
</template>
