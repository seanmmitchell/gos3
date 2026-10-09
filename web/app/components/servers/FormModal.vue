<script setup lang="ts">
/**
 * Add / edit a server connection.
 *
 *   <ServersFormModal v-model:open="open" />                                 add
 *   <ServersFormModal v-model:open="open" :connection="c" />                  edit
 *   <ServersFormModal v-model:open="open" :prefill="input" :credentials-from="current" />
 *
 * - The server type is auto-detected by probing GET /_gos3/info (manual override).
 * - "Test connection" runs reachability + signed whoami/ListBuckets with clear
 *   error mapping (CORS vs network vs wrong secret vs unknown key vs clock skew).
 * - Presets and the embedded server keep their definition from configuration:
 *   only the credentials (and name/color where allowed) can be changed.
 */
import type { Connection, ConnectionInput } from '~/types/connection'
import { useConnectionsStore } from '~/stores/connections'
import {
  CONNECTION_COLORS,
  emptyServerForm,
  formFromConnection,
  formFromInput,
  formToInput,
  isDefinitionLocked,
  validateServerForm,
  type KindChoice,
  type ServerFormModel
} from './-form'
import { detectKind, testConnection, type DetectResult, type TestReport } from './-test-connection'

const open = defineModel<boolean>('open', { required: true })

const props = defineProps<{
  /** Edit this connection (omit to add a new one). */
  connection?: Connection
  /** Prefill for a new connection (discovery, duplicate). */
  prefill?: ConnectionInput
  /** Offer to reuse the credentials of this connection (discovery "add as server"). */
  credentialsFrom?: Connection
}>()

const emit = defineEmits<{ saved: [connection: Connection] }>()

const store = useConnectionsStore()
const toast = useToast()

const model = ref<ServerFormModel>(emptyServerForm())
const touched = ref<Partial<Record<keyof ServerFormModel, boolean>>>({})
const submitted = ref(false)
const showSecret = ref(false)
const reuseCredentials = ref(false)
const saving = ref(false)
const saveError = ref<unknown>(null)

const detection = ref<DetectResult | null>(null)
const detecting = ref(false)
let detectSeq = 0
/** Endpoint of the latest detection (avoids re-probing the same URL). */
let detectedFor = ''

const report = ref<TestReport | null>(null)
const testing = ref(false)

const editing = computed(() => !!props.connection)
const locked = computed(() => isDefinitionLocked(props.connection))
const lockedName = computed(() => props.connection?.source === 'preset')

const kindItems: { label: string, value: KindChoice, description: string }[] = [
  { label: 'Auto-detect', value: 'auto', description: 'Probe /_gos3/info' },
  { label: 'gos3', value: 'gos3', description: 'S3 + admin API' },
  { label: 'S3-compatible', value: 's3', description: 'AWS, MinIO, Ceph, …' }
]

watch(open, (v) => {
  if (!v) return
  model.value = props.connection ? formFromConnection(props.connection) : formFromInput(props.prefill ?? {})
  touched.value = {}
  submitted.value = false
  showSecret.value = false
  reuseCredentials.value = false
  saveError.value = null
  report.value = null
  detection.value = null
  detectedFor = ''
  if (!isDefinitionLocked(props.connection) && model.value.endpoint && !validateServerForm(model.value).endpoint) void detect()
}, { immediate: true })

// Reusing credentials copies them into the form (still editable afterwards).
watch(reuseCredentials, (v) => {
  if (!props.credentialsFrom) return
  if (v) {
    model.value.accessKeyId = props.credentialsFrom.accessKeyId
    model.value.secretAccessKey = props.credentialsFrom.secretAccessKey
  } else if (model.value.accessKeyId === props.credentialsFrom.accessKeyId && model.value.secretAccessKey === props.credentialsFrom.secretAccessKey) {
    model.value.accessKeyId = ''
    model.value.secretAccessKey = ''
  }
})

const errors = computed(() => validateServerForm(model.value))
function fieldError(k: keyof ServerFormModel): string | undefined {
  return submitted.value || touched.value[k] ? errors.value[k] : undefined
}
const valid = computed(() => Object.keys(errors.value).length === 0)

// -- kind detection ------------------------------------------------------------

async function detect(): Promise<DetectResult | null> {
  const endpoint = model.value.endpoint
  if (validateServerForm(model.value).endpoint) return null
  const seq = ++detectSeq
  detectedFor = endpoint
  detecting.value = true
  try {
    const r = await detectKind(endpoint)
    if (seq !== detectSeq) return null
    detection.value = r
    return r
  } finally {
    if (seq === detectSeq) detecting.value = false
  }
}

let detectTimer: ReturnType<typeof setTimeout> | undefined
watch(() => model.value.endpoint, (endpoint) => {
  if (endpoint === detectedFor) return
  detection.value = null
  report.value = null
  if (detectTimer) clearTimeout(detectTimer)
  if (locked.value) return
  detectTimer = setTimeout(() => void detect(), 700)
})
onBeforeUnmount(() => {
  if (detectTimer) clearTimeout(detectTimer)
})

const detectedInfo = computed(() => detection.value?.info ?? report.value?.info)
const detectionText = computed(() => {
  const d = detection.value
  if (detecting.value) return 'Detecting server type…'
  if (!d) return ''
  if (d.info) return `gos3 ${d.info.version} · “${d.info.name}” · region ${d.info.region || 'default'}`
  if (d.protectedInfo) return 'gos3 server (identity requires authentication)'
  if (d.kind === 's3') return 'S3-compatible server (not gos3)'
  return 'Server not reachable from this browser yet — the type cannot be detected.'
})

// Prefill name/region from the identity document when the user left them empty.
const namePlaceholder = computed(() => detectedInfo.value?.name || (props.prefill?.name ?? 'My server'))
const regionPlaceholder = computed(() => detectedInfo.value?.region || 'us-east-1')

// -- duplicates ------------------------------------------------------------------

const duplicate = computed<Connection | undefined>(() => {
  if (editing.value || errors.value.endpoint) return undefined
  const serverId = detectedInfo.value?.id ?? model.value.serverId
  return (serverId ? store.findByServerId(serverId) : undefined) ?? store.findByEndpoint(model.value.endpoint)
})

// -- test --------------------------------------------------------------------------

async function runTest() {
  submitted.value = true
  if (errors.value.endpoint || errors.value.region) return
  testing.value = true
  report.value = null
  try {
    report.value = await testConnection({
      endpoint: model.value.endpoint,
      region: model.value.region.trim() || detectedInfo.value?.region || 'us-east-1',
      accessKeyId: model.value.accessKeyId.trim(),
      secretAccessKey: model.value.secretAccessKey,
      forcePathStyle: model.value.forcePathStyle,
      kind: model.value.kind
    })
  } finally {
    testing.value = false
  }
}

// -- save ----------------------------------------------------------------------------

async function save() {
  submitted.value = true
  if (!valid.value) return
  saving.value = true
  saveError.value = null
  try {
    let det = detection.value
    if (model.value.kind === 'auto' && !det && !locked.value) det = await detect()
    const info = det?.info ?? report.value?.info
    const input = formToInput(model.value, det?.kind ?? report.value?.detectedKind)
    input.name ??= info?.name || props.prefill?.name
    if (!model.value.region.trim() && info?.region) input.region = info.region
    if (info?.id) input.serverId = info.id
    if (input.kind !== 'gos3' && !model.value.serverId) input.serverId = undefined

    let saved: Connection
    if (props.connection) {
      const c = props.connection
      const patch: Partial<Omit<Connection, 'id'>> = locked.value
        ? {
            accessKeyId: input.accessKeyId ?? '',
            secretAccessKey: input.secretAccessKey ?? '',
            remember: input.remember ?? false,
            color: input.color,
            ...(lockedName.value ? {} : { name: input.name ?? c.name })
          }
        : {
            name: input.name ?? c.name,
            endpoint: input.endpoint,
            region: input.region ?? c.region,
            accessKeyId: input.accessKeyId ?? '',
            secretAccessKey: input.secretAccessKey ?? '',
            kind: input.kind ?? c.kind,
            forcePathStyle: input.forcePathStyle ?? c.forcePathStyle,
            remember: input.remember ?? false,
            color: input.color,
            serverId: input.serverId ?? c.serverId
          }
      saved = store.update(c.id, patch)
    } else {
      saved = store.add({ ...input, source: props.prefill?.source === 'discovered' ? 'discovered' : 'user' })
    }
    void store.refreshStatus(saved.id)
    toast.add({ title: editing.value ? `${saved.name} updated` : `${saved.name} added`, color: 'success', icon: 'i-lucide-circle-check' })
    emit('saved', saved)
    open.value = false
  } catch (e) {
    saveError.value = e
  } finally {
    saving.value = false
  }
}

const title = computed(() => (editing.value ? `Edit ${props.connection?.name}` : 'Add server'))
const description = computed(() => editing.value
  ? (locked.value ? 'This server is defined by configuration: only the credentials and display options can be changed.' : 'Change the connection settings of this server.')
  : 'Connect to a gos3 server or any S3-compatible service.')
</script>

<template>
  <UModal v-model:open="open" :title="title" :description="description" :ui="{ content: 'sm:max-w-2xl', footer: 'justify-between gap-2' }">
    <template #body>
      <form id="server-form" class="space-y-6" novalidate @submit.prevent="save">
        <fieldset class="space-y-4">
          <legend class="mb-2 text-sm font-semibold text-highlighted">
            Server
          </legend>

          <UFormField label="Endpoint URL" :error="fieldError('endpoint')" :help="locked ? 'Defined by configuration.' : 'Base URL of the S3 API, e.g. http://10.0.0.5:9000 or https://s3.eu-west-1.amazonaws.com'" required>
            <UInput
              v-model.trim="model.endpoint"
              class="w-full font-mono"
              placeholder="http://10.0.0.5:9000"
              autocomplete="url"
              spellcheck="false"
              inputmode="url"
              :disabled="locked"
              :autofocus="!editing && !prefill"
              :aria-invalid="!!fieldError('endpoint')"
              @blur="touched.endpoint = true"
            />
          </UFormField>
          <p v-if="detectionText && !locked" class="-mt-2 flex items-center gap-1.5 text-xs text-muted" aria-live="polite">
            <UIcon
              :name="detecting ? 'i-lucide-loader-circle' : detection?.kind === 'gos3' ? 'i-lucide-badge-check' : detection?.kind === 's3' ? 'i-lucide-database' : 'i-lucide-circle-help'"
              class="size-3.5 shrink-0"
              :class="detecting ? 'animate-spin' : ''"
              aria-hidden="true"
            />
            {{ detectionText }}
          </p>

          <UAlert
            v-if="duplicate"
            color="warning"
            variant="subtle"
            icon="i-lucide-copy"
            :title="`Already configured as “${duplicate.name}”`"
            description="You can still add it again, e.g. with different credentials."
          />

          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Display name" :error="fieldError('name')" :help="lockedName ? 'Defined by configuration.' : undefined">
              <UInput v-model="model.name" class="w-full" :placeholder="namePlaceholder" autocomplete="off" :disabled="lockedName" @blur="touched.name = true" />
            </UFormField>
            <UFormField label="Region" :error="fieldError('region')" help="Signing region; empty uses the server's region.">
              <UInput v-model.trim="model.region" class="w-full font-mono" :placeholder="regionPlaceholder" autocomplete="off" spellcheck="false" :disabled="locked" @blur="touched.region = true" />
            </UFormField>
          </div>

          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Server type" :help="model.kind === 'auto' ? 'Detected from GET /_gos3/info.' : undefined">
              <USelect v-model="model.kind" :items="kindItems" value-key="value" class="w-full" :disabled="locked" />
            </UFormField>
            <div class="sm:pt-6">
              <USwitch
                v-model="model.forcePathStyle"
                label="Path-style addressing"
                description="Recommended for gos3, MinIO and IP endpoints. Off: virtual-hosted buckets (AWS)."
                :disabled="locked"
              />
            </div>
          </div>

          <div role="radiogroup" aria-label="Accent color" class="flex flex-wrap items-center gap-2">
            <span class="mr-1 text-sm text-default">Color</span>
            <button
              type="button"
              role="radio"
              :aria-checked="!model.color"
              aria-label="No color"
              class="flex size-6 items-center justify-center rounded-full border border-default focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
              :class="!model.color ? 'ring-2 ring-primary ring-offset-2 ring-offset-default' : ''"
              @click="model.color = ''"
            >
              <UIcon name="i-lucide-ban" class="size-3.5 text-dimmed" aria-hidden="true" />
            </button>
            <button
              v-for="c in CONNECTION_COLORS"
              :key="c.value"
              type="button"
              role="radio"
              :aria-checked="model.color === c.value"
              :aria-label="c.label"
              :title="c.label"
              class="size-6 rounded-full focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
              :class="model.color === c.value ? 'ring-2 ring-primary ring-offset-2 ring-offset-default' : ''"
              :style="{ backgroundColor: c.value }"
              @click="model.color = c.value"
            />
          </div>
        </fieldset>

        <fieldset class="space-y-4">
          <legend class="mb-2 text-sm font-semibold text-highlighted">
            Credentials
          </legend>
          <USwitch
            v-if="credentialsFrom && hasCredentials(credentialsFrom)"
            v-model="reuseCredentials"
            :label="`Use the same credentials as ${credentialsFrom.name}`"
            description="Handy for clusters sharing IAM users or root credentials."
          />
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Access key ID" :error="fieldError('accessKeyId')">
              <UInput v-model.trim="model.accessKeyId" class="w-full font-mono" autocomplete="username" spellcheck="false" placeholder="AKIA…" @blur="touched.accessKeyId = true" />
            </UFormField>
            <UFormField label="Secret access key" :error="fieldError('secretAccessKey')">
              <UInput
                v-model="model.secretAccessKey"
                :type="showSecret ? 'text' : 'password'"
                class="w-full font-mono"
                autocomplete="current-password"
                spellcheck="false"
                :ui="{ trailing: 'pe-1' }"
                @blur="touched.secretAccessKey = true"
              >
                <template #trailing>
                  <UButton
                    color="neutral"
                    variant="link"
                    size="sm"
                    :icon="showSecret ? 'i-lucide-eye-off' : 'i-lucide-eye'"
                    :aria-label="showSecret ? 'Hide secret key' : 'Show secret key'"
                    :aria-pressed="showSecret"
                    @click="showSecret = !showSecret"
                  />
                </template>
              </UInput>
            </UFormField>
          </div>
          <p class="-mt-2 text-xs text-muted">
            Leave both empty to add the server now and sign in later. Requests are signed in this browser; keys are never sent anywhere else.
          </p>
          <USwitch v-model="model.remember" label="Remember the secret key on this device" />
          <UAlert
            v-if="model.remember"
            color="warning"
            variant="subtle"
            icon="i-lucide-shield-alert"
            title="Stored in this browser"
            description="The secret key is saved unencrypted in this browser's local storage. Anyone with access to this browser profile can read it. When off, it is kept only until the browser session ends."
          />
        </fieldset>

        <ServersTestReport :report="report" :testing="testing" />
        <CommonErrorAlert v-if="saveError" :error="saveError" title="Could not save the server" />
      </form>
    </template>

    <template #footer>
      <UButton
        color="neutral"
        variant="outline"
        icon="i-lucide-plug-zap"
        label="Test connection"
        :loading="testing"
        :disabled="!!errors.endpoint"
        @click="runTest"
      />
      <div class="flex gap-2">
        <UButton color="neutral" variant="ghost" label="Cancel" @click="open = false" />
        <UButton type="submit" form="server-form" :icon="editing ? 'i-lucide-save' : 'i-lucide-plus'" :label="editing ? 'Save' : 'Add server'" :loading="saving" />
      </div>
    </template>
  </UModal>
</template>
