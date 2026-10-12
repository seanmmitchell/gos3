<script setup lang="ts">
/**
 * Probe one URL for a gos3 server. The parent supplies `probe` (admin API
 * POST /discovery/probe, the UI host, or a direct browser fetch) and gets
 * "add" for the resulting peer.
 */
import type { Peer } from '~/types/admin'
import { validateEndpoint } from '~/components/servers/-form'

const props = withDefaults(defineProps<{
  probe: (url: string) => Promise<Peer>
  idPrefix?: string
  help?: string
}>(), { idPrefix: 'probe', help: 'Base URL of the server, e.g. http://10.0.0.5:9000.' })

const emit = defineEmits<{ add: [peer: Peer] }>()

const url = ref('')
const submitted = ref(false)
const running = ref(false)
const result = ref<Peer | null>(null)
const error = ref<unknown>(null)
const urlError = computed(() => (submitted.value ? validateEndpoint(url.value) : undefined))

async function submit() {
  submitted.value = true
  if (validateEndpoint(url.value)) return
  running.value = true
  error.value = null
  result.value = null
  try {
    result.value = await props.probe(url.value.trim())
  } catch (e) {
    error.value = e
  } finally {
    running.value = false
  }
}
</script>

<template>
  <div class="space-y-3">
    <form class="flex flex-col gap-2 sm:flex-row sm:items-start" novalidate @submit.prevent="submit">
      <UFormField label="URL" :error="urlError" :help="help" class="flex-1">
        <UInput :id="`${idPrefix}-url`" v-model.trim="url" class="w-full font-mono" placeholder="http://10.0.0.5:9000" inputmode="url" spellcheck="false" autocomplete="off" />
      </UFormField>
      <UButton type="submit" icon="i-lucide-radio-tower" label="Probe" :loading="running" class="sm:mt-6" />
    </form>
    <CommonErrorAlert v-if="error" :error="error" title="Probe failed" />
    <div v-if="result" class="flex flex-wrap items-center gap-3 rounded-md border border-default p-3" aria-live="polite">
      <UIcon name="i-lucide-badge-check" class="size-5 text-success" aria-hidden="true" />
      <div class="min-w-0 flex-1">
        <p class="font-medium text-highlighted">
          {{ result.name }} <span class="font-mono text-xs text-muted">{{ result.id }}</span>
        </p>
        <p class="text-xs text-muted">
          gos3 {{ result.version }} · {{ result.region || 'default region' }} · {{ result.endpoints?.s3 || result.addresses?.[0] }}
        </p>
      </div>
      <UButton size="sm" icon="i-lucide-plus" label="Add as server" @click="emit('add', result)" />
    </div>
  </div>
</template>
