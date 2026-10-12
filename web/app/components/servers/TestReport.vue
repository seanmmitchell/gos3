<script setup lang="ts">
/** Step-by-step result of "Test connection" (see testConnection in -test-connection.ts). */
import type { StepStatus, TestReport } from './-test-connection'

defineProps<{
  report: TestReport | null
  testing?: boolean
}>()

const STATUS: Record<StepStatus, { icon: string, class: string, label: string }> = {
  ok: { icon: 'i-lucide-circle-check', class: 'text-success', label: 'Passed' },
  warning: { icon: 'i-lucide-triangle-alert', class: 'text-warning', label: 'Warning' },
  error: { icon: 'i-lucide-circle-x', class: 'text-error', label: 'Failed' },
  skipped: { icon: 'i-lucide-circle-minus', class: 'text-dimmed', label: 'Skipped' }
}
</script>

<template>
  <section aria-live="polite" aria-label="Connection test results" class="space-y-3">
    <div v-if="testing" class="flex items-center gap-2 text-sm text-muted">
      <UIcon name="i-lucide-loader-circle" class="size-4 animate-spin" aria-hidden="true" />
      Testing the connection…
    </div>
    <template v-else-if="report">
      <div class="flex items-center gap-2 text-sm font-medium" :class="report.ok ? 'text-success' : 'text-error'">
        <UIcon :name="report.ok ? 'i-lucide-circle-check' : 'i-lucide-circle-x'" class="size-5" aria-hidden="true" />
        {{ report.ok ? 'Connection works' : 'Connection test failed' }}
        <UBadge v-if="report.detectedKind" :label="report.detectedKind === 'gos3' ? 'gos3 detected' : 'S3-compatible'" color="neutral" variant="subtle" size="sm" />
      </div>
      <ol class="space-y-2">
        <li v-for="step in report.steps" :key="step.id" class="flex gap-2 text-sm">
          <UIcon :name="STATUS[step.status].icon" class="mt-0.5 size-4 shrink-0" :class="STATUS[step.status].class" aria-hidden="true" />
          <div class="min-w-0 flex-1">
            <p>
              <span class="font-medium text-highlighted">{{ step.label }}</span>
              <span class="sr-only">: {{ STATUS[step.status].label }}.</span>
              <span v-if="!step.diagnosis" class="text-muted"> — {{ step.detail }}</span>
            </p>
            <ServersDiagnosisAlert v-if="step.diagnosis" :diagnosis="step.diagnosis" class="mt-1.5" />
          </div>
        </li>
      </ol>
    </template>
  </section>
</template>
