<script setup lang="ts">
import type { Backing, BackingBackend, BackingConfiguration, BackingDefinition, S3Addressing, S3BackingDefinition } from '~/types/admin'
import {
  ADDRESSING_ITEMS,
  BACKEND_ITEMS,
  DEFAULT_KIND_ITEMS,
  HOME_HELP,
  copyBackingConfiguration,
  defaultKindValue,
  generateEncryptionKey,
  secretPlaceholder,
  setBackend,
  setDefaultKind,
  setHomeBucket,
  toPutBody,
  validateBackingConfiguration
} from './-backing-form'

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
const validation = computed(() => form.value ? validateBackingConfiguration(form.value, original.value) : undefined)
const names = computed(() => ['default', ...(form.value?.backings.map(b => b.name).filter(Boolean) ?? [])])
const save = useAsyncAction(async () => {
  const result = await requireAdmin().putBackingConfiguration(toPutBody(form.value!))
  setForm(result)
  emit('saved')
}, { successMessage: 'Backing configuration applied', errorTitle: 'Could not configure backings', toastErrors: false })

/** The saved s3 section of a row that was loaded (not added in the form). */
function loadedS3(b: BackingDefinition): S3BackingDefinition | undefined {
  return savedRows.has(b) ? original.value?.backings.find(x => x.name === b.name)?.s3 : undefined
}

function inUse(name: string) {
  const b = props.backings.find(b => b.name === name)
  return !!b && (b.buckets > 0 || b.replicas > 0)
}

/** Rows whose encryption key input shows the key (after Generate or Show). */
const revealed = reactive(new Set<BackingDefinition>())

function generateKey(b: BackingDefinition) {
  b.s3!.encryptionKey = generateEncryptionKey()
  revealed.add(b)
}

function toggleReveal(b: BackingDefinition) {
  if (revealed.has(b)) revealed.delete(b)
  else revealed.add(b)
}

/** Part size in MiB for the form (undefined = server default). */
function partSizeMiB(s3: S3BackingDefinition): number | undefined {
  return s3.partSize ? s3.partSize / (1024 * 1024) : undefined
}

function setPartSizeMiB(s3: S3BackingDefinition, v: string | number | null | undefined) {
  const n = Number(v)
  if (v === '' || v == null || !Number.isFinite(n) || n <= 0) delete s3.partSize
  else s3.partSize = Math.round(n * 1024 * 1024)
}

function setAddressing(s3: S3BackingDefinition, v: string) {
  s3.addressing = (v === 'auto' ? '' : v) as S3Addressing
}

function setForm(result: BackingConfiguration) {
  original.value = copyBackingConfiguration(result)
  form.value = copyBackingConfiguration(result)
  savedRows = new WeakSet(form.value.backings)
  revealed.clear()
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
        <p class="text-sm text-muted">The system backing named default is configured at startup. Move buckets and remove replicas before changing or removing other backings. New filesystem backings need an empty server directory; a new S3 backing's home must be an empty bucket or prefix.</p>
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
                <USelect :model-value="b.backend" :disabled="inUse(b.name)" :items="[...BACKEND_ITEMS]" class="w-full" aria-label="Backend" @update:model-value="setBackend(b, $event as BackingBackend)" />
              </UFormField>
            </div>
            <UFormField v-if="b.backend === 'fs'" label="Directory on the server" required>
              <UInput v-model="b.path" :disabled="inUse(b.name)" placeholder="/mnt/archive" class="w-full" aria-label="Directory on the server" />
            </UFormField>
            <div v-else-if="b.backend === 's3' && b.s3" class="space-y-3" data-testid="s3-fields">
              <div class="grid gap-3 sm:grid-cols-2">
                <UFormField label="Endpoint" required help="URL of the S3 service, without a path.">
                  <UInput v-model="b.s3.endpoint" placeholder="https://s3.eu-central-1.amazonaws.com" class="w-full font-mono" aria-label="Endpoint" autocomplete="off" />
                </UFormField>
                <UFormField label="Region">
                  <UInput v-model="b.s3.region" placeholder="us-east-1" class="w-full" aria-label="Region" autocomplete="off" />
                </UFormField>
                <UFormField label="Home bucket" :help="HOME_HELP">
                  <UInput
                    :model-value="b.s3.homeBucket"
                    :disabled="inUse(b.name)"
                    placeholder="gos3-archive"
                    class="w-full font-mono"
                    aria-label="Home bucket"
                    autocomplete="off"
                    @update:model-value="setHomeBucket(b.s3, String($event ?? ''), loadedS3(b))"
                  />
                </UFormField>
                <UFormField label="Home prefix" help="Optional; gos3's keys inside the home bucket start with it.">
                  <UInput v-model="b.s3.homePrefix" :disabled="inUse(b.name) || (!b.s3.homeBucket && !b.s3.homePrefix)" placeholder="gos3/" class="w-full font-mono" aria-label="Home prefix" autocomplete="off" />
                </UFormField>
                <UFormField label="Access key" required>
                  <UInput v-model="b.s3.accessKey" class="w-full font-mono" aria-label="Access key" autocomplete="off" />
                </UFormField>
                <UFormField label="Secret key" :required="!secretPlaceholder(b.name, b.s3, 'secret')">
                  <UInput v-model="b.s3.secretKey" type="password" :placeholder="secretPlaceholder(b.name, b.s3, 'secret')" class="w-full font-mono" aria-label="Secret key" autocomplete="new-password" />
                </UFormField>
              </div>
              <UFormField label="Default bucket kind" help="Native buckets keep their objects as-is in a remote bucket of their own; wrapped buckets use gos3's format inside a container (needs a home or an added container). Users can choose per bucket.">
                <USelect :model-value="defaultKindValue(b.s3)" :items="[...DEFAULT_KIND_ITEMS]" class="w-full sm:w-96" aria-label="Default bucket kind" @update:model-value="setDefaultKind(b.s3, String($event))" />
              </UFormField>
              <details class="rounded-md border border-default" data-testid="s3-advanced">
                <summary class="cursor-pointer px-3 py-2 text-sm font-medium text-highlighted">Advanced</summary>
                <div class="space-y-3 px-3 pb-3">
                  <div class="grid gap-3 sm:grid-cols-2">
                    <UFormField label="Addressing">
                      <USelect :model-value="b.s3.addressing || 'auto'" :items="[...ADDRESSING_ITEMS]" class="w-full" aria-label="Addressing" @update:model-value="setAddressing(b.s3, String($event))" />
                    </UFormField>
                    <UFormField label="Part size (MiB)" help="Multipart part size, 5–5120 MiB; empty = server default.">
                      <UInput :model-value="partSizeMiB(b.s3)" type="number" min="5" max="5120" placeholder="Default" class="w-full" aria-label="Part size (MiB)" @update:model-value="setPartSizeMiB(b.s3, $event)" />
                    </UFormField>
                  </div>
                  <UFormField label="Encryption key" help="Base64 of 32 bytes.">
                    <div class="flex gap-2">
                      <UInput
                        v-model="b.s3.encryptionKey"
                        :type="revealed.has(b) ? 'text' : 'password'"
                        :placeholder="secretPlaceholder(b.name, b.s3, 'encryption')"
                        class="w-full font-mono"
                        aria-label="Encryption key"
                        autocomplete="new-password"
                      />
                      <UButton color="neutral" variant="outline" :icon="revealed.has(b) ? 'i-lucide-eye-off' : 'i-lucide-eye'" :aria-label="revealed.has(b) ? 'Hide encryption key' : 'Show encryption key'" @click="toggleReveal(b)" />
                      <UButton color="neutral" variant="outline" icon="i-lucide-key-round" label="Generate" @click="generateKey(b)" />
                    </div>
                  </UFormField>
                  <UAlert color="warning" variant="subtle" icon="i-lucide-triangle-alert" description="Needed for AES256 and restic on this backing. Losing it makes encrypted objects unreadable — store it safely." />
                </div>
              </details>
            </div>
            <p v-else class="text-sm text-muted">Memory backing contents are lost when the server restarts.</p>
            <div class="flex items-center justify-between gap-3">
              <span class="text-xs text-muted">{{ !inUse(b.name) ? 'Changes take effect when you save.' : b.backend === 's3' ? 'In use — connection settings and the default bucket kind can change; move buckets and remove replicas before changing the home bucket or prefix.' : 'In use — move buckets and remove replicas before editing.' }}</span>
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
