<script setup lang="ts">
/**
 * Network scan form: targets (CIDRs, hosts, URLs) × ports × schemes, with
 * timeout and concurrency. Emits a discovery.ScanRequest; the parent runs it
 * (admin API or the UI host's /api/discovery/scan) and passes `running`.
 */
import type { ScanRequest } from '~/types/admin'
import { defaultScanForm, estimateHosts, estimateProbes, scanFormErrors, splitList, toScanRequest, type ScanFormModel } from './-scan-form'

const props = withDefaults(defineProps<{
  running?: boolean
  /** Host limit (server's discovery.maxScanHosts or the UI host's cap). */
  maxHosts?: number
  submitLabel?: string
  idPrefix?: string
  /** Placeholder targets, e.g. the local /24. */
  placeholder?: string
}>(), { running: false, maxHosts: 1024, submitLabel: 'Start scan', idPrefix: 'scan', placeholder: '192.168.1.0/24\nnas.local\nhttp://10.0.0.5:9000' })

const emit = defineEmits<{
  submit: [request: ScanRequest]
  cancel: []
}>()

const model = ref<ScanFormModel>(defaultScanForm())
const submitted = ref(false)
const errors = computed(() => scanFormErrors(model.value, { maxHosts: props.maxHosts }))
const shown = computed(() => (submitted.value ? errors.value : {}))
const hosts = computed(() => estimateHosts(splitList(model.value.targets)))
const probes = computed(() => estimateProbes(model.value))

function submit() {
  submitted.value = true
  if (Object.keys(errors.value).length) return
  emit('submit', toScanRequest(model.value))
}
</script>

<template>
  <form class="space-y-4" novalidate :aria-busy="running" @submit.prevent="submit">
    <UFormField
      label="Targets"
      :error="shown.targets"
      :help="`One per line: IPv4 CIDRs, host names or IPs (optionally with :port), or base URLs. At most ${maxHosts} hosts.`"
      required
    >
      <UTextarea
        :id="`${idPrefix}-targets`"
        v-model="model.targets"
        :rows="4"
        autoresize
        :maxrows="10"
        class="w-full font-mono"
        :placeholder="placeholder"
        spellcheck="false"
        :disabled="running"
      />
    </UFormField>
    <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <UFormField label="Ports" :error="shown.ports" help="Comma separated (default 9000).">
        <UInput v-model="model.ports" class="w-full font-mono" placeholder="9000" inputmode="numeric" :disabled="running" />
      </UFormField>
      <!-- A fieldset, not UFormField: the form field would label both checkboxes "Schemes". -->
      <fieldset class="min-w-0">
        <legend class="block text-sm font-medium text-default">
          Schemes
        </legend>
        <div class="mt-1 flex h-8 items-center gap-4">
          <UCheckbox v-model="model.http" label="http" :disabled="running" />
          <UCheckbox v-model="model.https" label="https" :disabled="running" />
        </div>
        <p v-if="shown.schemes" class="mt-2 text-sm text-error">
          {{ shown.schemes }}
        </p>
      </fieldset>
      <UFormField label="Timeout per probe (s)" :error="shown.timeoutSeconds">
        <UInputNumber v-model="model.timeoutSeconds" :min="0.01" :max="30" :step="0.5" :format-options="{ maximumFractionDigits: 2 }" class="w-full" :disabled="running" />
      </UFormField>
      <UFormField label="Concurrency" :error="shown.concurrency">
        <UInputNumber v-model="model.concurrency" :min="1" :max="512" class="w-full" :disabled="running" />
      </UFormField>
    </div>
    <div class="flex flex-wrap items-center gap-3">
      <UButton type="submit" icon="i-lucide-scan-search" :label="running ? 'Scanning…' : submitLabel" :loading="running" />
      <UButton v-if="running" color="neutral" variant="outline" icon="i-lucide-square" label="Cancel" @click="emit('cancel')" />
      <span v-if="hosts" class="text-xs text-muted">≈ {{ formatNumber(hosts) }} host{{ hosts === 1 ? '' : 's' }}, {{ formatNumber(probes) }} probe{{ probes === 1 ? '' : 's' }}</span>
    </div>
  </form>
</template>
