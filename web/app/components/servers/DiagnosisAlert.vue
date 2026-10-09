<script setup lang="ts">
/**
 * Explains why connecting to a server failed (CORS vs network vs wrong
 * secret vs unknown key vs clock skew …), see -test-connection.ts.
 *
 *   <ServersDiagnosisAlert :diagnosis="d" />
 */
import type { Diagnosis, ProblemKind } from './-test-connection'

const props = withDefaults(defineProps<{
  diagnosis: Diagnosis
  color?: 'error' | 'warning'
}>(), { color: 'error' })

const ICONS: Record<ProblemKind, string> = {
  'cors': 'i-lucide-shield-alert',
  'network': 'i-lucide-wifi-off',
  'mixed-content': 'i-lucide-lock-open',
  'timeout': 'i-lucide-timer-off',
  'signature': 'i-lucide-key-round',
  'access-key': 'i-lucide-key-round',
  'clock-skew': 'i-lucide-clock-alert',
  'access-denied': 'i-lucide-ban',
  'auth': 'i-lucide-user-x',
  'not-gos3': 'i-lucide-server-off',
  'other': 'i-lucide-circle-alert'
}

const icon = computed(() => ICONS[props.diagnosis.kind] ?? 'i-lucide-circle-alert')
</script>

<template>
  <UAlert :color="color" variant="subtle" :icon="icon" :title="diagnosis.title" role="alert">
    <template #description>
      <p>{{ diagnosis.message }}</p>
      <p v-if="diagnosis.hint" class="mt-1 font-medium">
        {{ diagnosis.hint }}
      </p>
      <p v-if="diagnosis.code && diagnosis.kind !== 'other'" class="mt-1 font-mono text-xs opacity-80">
        {{ diagnosis.code }}<template v-if="diagnosis.status">
          · HTTP {{ diagnosis.status }}
        </template>
      </p>
    </template>
  </UAlert>
</template>
