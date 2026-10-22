<script setup lang="ts">
/**
 * Sign-in page (blank layout).
 *
 *   /connect?server=<id>&redirect=<path>   sign in to an existing connection
 *   /connect                               the embedded server when it needs
 *                                          credentials, else "connect to a new server"
 *   /connect?new=1                         always "connect to a new server"
 *   /connect?endpoint=<url>&name=<name>    new server, prefilled
 *
 * Credentials are validated before they are stored: GET /whoami for gos3,
 * ListBuckets for generic S3 (AccessDenied still counts as valid). Errors are
 * explained (CORS vs network vs wrong secret vs unknown key vs clock skew).
 *
 * Verified credentials are stored only with the endpoint they were verified
 * against: a new connection, or an existing one for that same endpoint
 * (reusableConnection). A link's ?endpoint=/&name= prefill is pointed out;
 * when its host is neither this page's nor a configured server's, the user
 * must confirm it before the form is submitted. Nothing is submitted
 * automatically.
 */
import { completeConnection } from '~/utils/connections'
import { ConnectionTransport } from '~/utils/transport'
import type { Connection } from '~/types/connection'
import { useConnectionsStore } from '~/stores/connections'
import {
  emptyServerForm,
  formToInput,
  linkPrefillCheck,
  reusableConnection,
  safeRedirect,
  validateRegion,
  validateServerForm,
  type KindChoice,
  type ServerFormModel
} from '~/components/servers/-form'
import { detectKind, verifyAndDiagnose, type Diagnosis } from '~/components/servers/-test-connection'

definePageMeta({ layout: 'blank' })

const route = useRoute()
const router = useRouter()
const store = useConnectionsStore()
const toast = useToast()

const relayItems = computed(() => [
  { label: 'Disabled (direct only)', value: 'direct' },
  ...store.connections.filter(c => c.id !== target.value?.id && c.kind === 'gos3' && !c.proxyConnectionId && hasCredentials(c) && store.capabilitiesOf(c.id).proxy)
    .map(c => ({ label: c.name, value: c.id }))
])

const queryString = (v: unknown): string => (typeof v === 'string' ? v : Array.isArray(v) && typeof v[0] === 'string' ? v[0] : '')
const queryServer = computed(() => queryString(route.query.server))
const redirect = computed(() => safeRedirect(route.query.redirect))

const target = computed<Connection | undefined>(() => {
  if (queryServer.value) return store.byId(queryServer.value)
  if (route.query.new !== undefined || route.query.endpoint !== undefined) return undefined
  const emb = store.byId(EMBEDDED_CONNECTION_ID)
  return emb && !hasCredentials(emb) ? emb : undefined
})
const unknownServer = computed(() => !!queryServer.value && !target.value)
const mode = computed<'signin' | 'new'>(() => (target.value ? 'signin' : 'new'))

useHead({ title: () => (target.value ? `Sign in to ${target.value.name}` : 'Connect to a server') })

// -- form state --------------------------------------------------------------------

const model = ref<ServerFormModel>(emptyServerForm())
const showSecret = ref(false)
const showAdvanced = ref(false)
const submitted = ref(false)
const busy = ref(false)
const diagnosis = ref<Diagnosis | null>(null)
/** Endpoint and name the link's query string prefilled ('' when none). */
const linkPrefill = ref({ endpoint: '', name: '' })
/** The user confirmed the prefilled host (required when it is unknown). */
const trustPrefill = ref(false)

function reset() {
  const t = target.value
  model.value = emptyServerForm()
  linkPrefill.value = { endpoint: '', name: '' }
  if (t) {
    model.value.accessKeyId = t.accessKeyId
    model.value.remember = t.remember
    model.value.proxyConnectionId = t.proxyConnectionId
  } else {
    const current = store.active
    if (current && relayItems.value.some(item => item.value === current.id)) model.value.proxyConnectionId = current.id
    linkPrefill.value = { endpoint: queryString(route.query.endpoint).trim(), name: queryString(route.query.name) }
    model.value.endpoint = linkPrefill.value.endpoint
    model.value.name = linkPrefill.value.name
    // Show the prefilled display name (it lives under "More options").
    if (linkPrefill.value.name) showAdvanced.value = true
  }
  trustPrefill.value = false
  submitted.value = false
  diagnosis.value = null
  showSecret.value = false
}
watch(() => target.value?.id, reset, { immediate: true })

/**
 * The link's endpoint while it is still in the form: its host, and whether
 * it is unknown (neither this page's host nor a configured server's).
 */
const prefillCheck = computed(() => {
  const ep = linkPrefill.value.endpoint
  if (mode.value !== 'new' || !ep || model.value.endpoint !== ep) return undefined
  return linkPrefillCheck(ep, globalThis.location?.origin, store.connections)
})
// A confirmation is for the host it named.
watch(() => model.value.endpoint, () => {
  trustPrefill.value = false
})

const errors = computed(() => {
  if (mode.value === 'signin') {
    const e: Partial<Record<keyof ServerFormModel, string>> = {}
    if (!model.value.accessKeyId.trim()) e.accessKeyId = 'Enter the access key'
    if (!model.value.secretAccessKey) e.secretAccessKey = 'Enter the secret key'
    return e
  }
  const e = validateServerForm(model.value, { requireCredentials: true })
  if (prefillCheck.value?.unknown && !trustPrefill.value) e.endpoint ??= `Confirm that you trust ${prefillCheck.value.origin}, or enter another address`
  return e
})
const shown = computed(() => (submitted.value ? errors.value : {}))

const status = computed(() => (target.value ? store.statusOf(target.value.id) : undefined))
onMounted(() => {
  if (target.value) void store.ensureStatus(target.value.id)
})

const kindItems: { label: string, value: KindChoice }[] = [
  { label: 'Auto-detect', value: 'auto' },
  { label: 'gos3', value: 'gos3' },
  { label: 'S3-compatible', value: 's3' }
]

// -- submit --------------------------------------------------------------------------

async function submit() {
  submitted.value = true
  diagnosis.value = null
  if (Object.keys(errors.value).length) return
  busy.value = true
  try {
    if (mode.value === 'signin') await signIn(target.value!)
    else await connectNew()
  } finally {
    busy.value = false
  }
}

async function signIn(c: Connection) {
  const creds = {
    endpoint: c.endpoint,
    region: c.region,
    accessKeyId: model.value.accessKeyId.trim(),
    secretAccessKey: model.value.secretAccessKey,
    forcePathStyle: c.forcePathStyle
  }
  const transport = new ConnectionTransport({ ...c, ...creds })
  const r = await verifyAndDiagnose(creds, c.kind, { fetch: req => transport.send(req), timeoutMs: 20000 })
  if (!r.ok) {
    diagnosis.value = r.diagnosis
    return
  }
  store.setCredentials(c.id, { accessKeyId: creds.accessKeyId, secretAccessKey: creds.secretAccessKey, remember: model.value.remember })
  void store.refreshStatus(c.id)
  const who = r.result.whoami ? (r.result.whoami.isRoot ? `${r.result.whoami.user || 'root'} (root)` : r.result.whoami.user) : creds.accessKeyId
  toast.add({ title: `Signed in to ${c.name}`, description: `as ${who}`, color: 'success', icon: 'i-lucide-circle-check' })
  await router.replace(redirect.value ?? appRoutes.server(c.id))
}

async function connectNew() {
  const transport = new ConnectionTransport(completeConnection({ ...formToInput(model.value), id: 'connect-form' }))
  const deps = { fetch: (req: Request) => transport.send(req), timeoutMs: 20000 }
  let detected = undefined as Awaited<ReturnType<typeof detectKind>> | undefined
  if (model.value.kind === 'auto') detected = await detectKind(model.value.endpoint, deps)
  const input = formToInput(model.value, detected?.kind)
  if (detected?.info) {
    input.name ??= detected.info.name
    if (!model.value.region.trim() && detected.info.region && !validateRegion(detected.info.region)) input.region = detected.info.region
    input.serverId = detected.info.id
  }
  const r = await verifyAndDiagnose({
    endpoint: input.endpoint,
    region: input.region ?? 'us-east-1',
    accessKeyId: input.accessKeyId ?? '',
    secretAccessKey: input.secretAccessKey ?? '',
    forcePathStyle: input.forcePathStyle ?? true
  }, input.kind ?? 's3', deps)
  if (!r.ok) {
    diagnosis.value = r.diagnosis
    return
  }
  // Only a connection for the endpoint just verified may take the keys: a
  // stored connection that merely claims the same server ID (IDs are not
  // authenticated) would sign them for its own, possibly hostile, host.
  const match = reusableConnection(store.connections, input)
  const existing = match?.proxyConnectionId === input.proxyConnectionId ? match : undefined
  const c: Connection = existing
    ? store.setCredentials(existing.id, { accessKeyId: input.accessKeyId ?? '', secretAccessKey: input.secretAccessKey ?? '', remember: input.remember })
    : store.add({ ...input, source: 'user' })
  void store.refreshStatus(c.id)
  toast.add({ title: `Connected to ${c.name}`, color: 'success', icon: 'i-lucide-circle-check' })
  await router.replace(redirect.value ?? appRoutes.server(c.id))
}

const others = computed(() => store.sorted.filter(c => c.id !== target.value?.id).slice(0, 6))
</script>

<template>
  <UCard :ui="{ body: 'space-y-5', footer: 'flex flex-wrap items-center justify-between gap-2 text-sm' }">
    <template #header>
      <div class="flex items-start gap-3">
        <div class="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary" aria-hidden="true">
          <UIcon :name="mode === 'signin' ? 'i-lucide-log-in' : 'i-lucide-plug'" class="size-5" />
        </div>
        <div class="min-w-0">
          <h1 class="text-lg font-semibold text-highlighted">
            {{ mode === 'signin' ? `Sign in to ${target!.name}` : 'Connect to a server' }}
          </h1>
          <p v-if="mode === 'signin'" class="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted">
            <code class="truncate font-mono text-xs">{{ target!.endpoint }}</code>
            <UBadge :label="target!.kind === 'gos3' ? 'gos3' : 'S3'" :color="target!.kind === 'gos3' ? 'primary' : 'neutral'" variant="outline" size="sm" />
            <span v-if="status?.info" class="text-xs">v{{ status.info.version }}</span>
            <CommonStatusDot v-if="status && status.state !== 'unknown'" :state="status.state" show-label size="xs" />
          </p>
          <p v-else class="text-sm text-muted">
            A gos3 server or any S3-compatible service. Your keys stay in this browser.
          </p>
        </div>
      </div>
    </template>

    <UAlert
      v-if="unknownServer"
      color="warning"
      variant="subtle"
      icon="i-lucide-circle-help"
      title="Unknown server"
      :description="`No connection “${queryServer}” exists in this browser. Enter its details below or pick it from the server list.`"
    />

    <UAlert
      v-if="prefillCheck"
      :color="prefillCheck.unknown ? 'warning' : 'info'"
      variant="subtle"
      icon="i-lucide-link"
      title="Prefilled by a link"
      data-testid="link-prefill"
    >
      <template #description>
        <p>
          The link you followed filled in the server address <code class="font-mono">{{ prefillCheck.origin }}</code>{{ linkPrefill.name ? ` and the name “${linkPrefill.name}”` : '' }}.
          <template v-if="prefillCheck.downgrade">
            This host is otherwise reached over https: this link uses plain http, so your access key ID and signed requests would travel unencrypted. Only continue if you trust it.
          </template>
          <template v-else-if="prefillCheck.unknown">
            It is neither this site nor one of your configured servers. Your access key ID and requests signed with your keys will go to this host: only continue if you trust it.
          </template>
          <template v-else>
            Check that it is the server you expect.
          </template>
        </p>
        <UCheckbox
          v-if="prefillCheck.unknown"
          v-model="trustPrefill"
          class="mt-2"
          :label="`I trust ${prefillCheck.origin}`"
          data-testid="trust-prefill"
        />
      </template>
    </UAlert>

    <form id="connect-form" class="space-y-4" novalidate @submit.prevent="submit">
      <template v-if="mode === 'new'">
        <UFormField label="Endpoint URL" :error="shown.endpoint" help="e.g. http://10.0.0.5:9000 or https://s3.eu-west-1.amazonaws.com" required>
          <UInput v-model.trim="model.endpoint" class="w-full font-mono" placeholder="http://10.0.0.5:9000" inputmode="url" autocomplete="url" spellcheck="false" :autofocus="!linkPrefill.endpoint" />
        </UFormField>
      </template>

      <UFormField v-if="mode === 'new'" label="Fallback relay" help="Try direct access first, then use this gos3 server when the browser cannot reach the destination.">
        <USelect :model-value="model.proxyConnectionId ?? 'direct'" :items="relayItems" class="w-full" @update:model-value="model.proxyConnectionId = $event === 'direct' ? undefined : $event" />
      </UFormField>

      <UFormField label="Access key ID" :error="shown.accessKeyId" required>
        <UInput v-model.trim="model.accessKeyId" class="w-full font-mono" autocomplete="username" spellcheck="false" :autofocus="mode === 'signin'" />
      </UFormField>
      <UFormField label="Secret access key" :error="shown.secretAccessKey" required>
        <UInput
          v-model="model.secretAccessKey"
          :type="showSecret ? 'text' : 'password'"
          class="w-full font-mono"
          autocomplete="current-password"
          spellcheck="false"
          :ui="{ trailing: 'pe-1' }"
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

      <template v-if="mode === 'new'">
        <UButton
          color="neutral"
          variant="link"
          size="sm"
          class="px-0"
          :icon="showAdvanced ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'"
          label="More options"
          :aria-expanded="showAdvanced"
          aria-controls="connect-advanced"
          @click="showAdvanced = !showAdvanced"
        />
        <div v-show="showAdvanced" id="connect-advanced" class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Display name" :error="shown.name">
            <UInput v-model="model.name" class="w-full" placeholder="Detected from the server" />
          </UFormField>
          <UFormField label="Region" :error="shown.region">
            <UInput v-model.trim="model.region" class="w-full font-mono" placeholder="us-east-1" spellcheck="false" />
          </UFormField>
          <UFormField label="Server type">
            <USelect v-model="model.kind" :items="kindItems" value-key="value" class="w-full" />
          </UFormField>
          <div class="sm:pt-6">
            <USwitch v-model="model.forcePathStyle" label="Path-style addressing" />
          </div>
        </div>
      </template>

      <USwitch v-model="model.remember" label="Remember me on this device" description="Keeps the secret key in this browser's local storage (unencrypted). Otherwise it is forgotten when the browser session ends." />

      <ServersDiagnosisAlert v-if="diagnosis" :diagnosis="diagnosis" />

      <UButton type="submit" block size="lg" :icon="mode === 'signin' ? 'i-lucide-log-in' : 'i-lucide-plug'" :label="mode === 'signin' ? 'Sign in' : 'Connect'" :loading="busy" />
    </form>

    <div v-if="others.length" class="space-y-2">
      <p class="text-xs font-medium uppercase tracking-wide text-muted">
        {{ mode === 'signin' ? 'Other servers' : 'Configured servers' }}
      </p>
      <ul class="flex flex-wrap gap-2">
        <li v-for="c in others" :key="c.id">
          <UButton
            size="xs"
            color="neutral"
            variant="outline"
            :label="c.name"
            :to="hasCredentials(c) ? appRoutes.server(c.id) : appRoutes.connect(c.id, redirect)"
          >
            <template #leading>
              <CommonStatusDot :state="store.statusOf(c.id).state" size="xs" />
            </template>
          </UButton>
        </li>
      </ul>
    </div>

    <template #footer>
      <NuxtLink :to="appRoutes.servers()" class="inline-flex items-center gap-1 text-muted hover:text-highlighted">
        <UIcon name="i-lucide-arrow-left" class="size-4" aria-hidden="true" /> Manage servers
      </NuxtLink>
      <div class="flex items-center gap-3">
        <NuxtLink v-if="mode === 'signin'" :to="appRoutes.newServer()" class="text-muted hover:text-highlighted">
          Different server
        </NuxtLink>
        <NuxtLink :to="appRoutes.discover()" class="inline-flex items-center gap-1 text-muted hover:text-highlighted">
          <UIcon name="i-lucide-scan-search" class="size-4" aria-hidden="true" /> Discover
        </NuxtLink>
      </div>
    </template>
  </UCard>
</template>
