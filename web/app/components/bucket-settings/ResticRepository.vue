<script setup lang="ts">
import type { RepositoryInfo, ResticTags } from '~/types/admin'
import { resticSnippet, snippetFromInfo, type Shell } from './-restic'

/**
 * One restic repository of a bucket (Backup & archive tab, Recovery card):
 * where it is, where its recovery record and password come from, the
 * environment variables restic needs, and the commands that list its
 * snapshots with the restic CLI alone (bash or PowerShell, no secrets: the
 * password is revealed separately). "Reveal credentials" is offered with
 * `canReveal`.
 */
const props = defineProps<{
  info: RepositoryInfo
  bucket: string
  /** archive: per-version snapshots; snapshots: whole-bucket snapshots. */
  purpose: 'archive' | 'snapshots'
  tags: ResticTags
  shell: Shell
  /** The backing holding the repository, when known. */
  backing?: string
  /** e.g. "Primary", "Replica". */
  role?: string
  canReveal?: boolean
  revealing?: boolean
  /** This server's URL, for the gos3 CLI's --server. */
  server?: string
}>()

const emit = defineEmits<{ reveal: [] }>()

const snippet = computed(() => resticSnippet(snippetFromInfo(props.shell, props.bucket, props.purpose, props.info, props.tags, props.server)))
</script>

<template>
  <div class="space-y-3 rounded-lg border border-default p-3" :data-testid="`restic-repo-${purpose}${backing ? `-${backing}` : ''}`">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <div class="flex flex-wrap items-center gap-2">
        <span v-if="backing" class="font-medium text-highlighted">{{ backing }}</span>
        <UBadge v-if="role" :label="role" color="neutral" variant="outline" size="sm" />
        <UBadge
          :label="info.initialized ? 'Initialized' : 'Not initialized yet'"
          :color="info.initialized ? 'success' : 'neutral'"
          variant="subtle"
          size="sm"
          :icon="info.initialized ? 'i-lucide-circle-check' : 'i-lucide-circle-dashed'"
        />
      </div>
      <UButton
        v-if="canReveal"
        color="warning"
        variant="outline"
        size="sm"
        icon="i-lucide-key-round"
        label="Reveal credentials…"
        :loading="revealing"
        @click="emit('reveal')"
      />
    </div>
    <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
      <dt class="text-muted">
        Repository
      </dt>
      <dd class="flex min-w-0 items-center gap-1 font-mono text-xs">
        <span class="truncate" :title="info.repository">{{ info.repository }}</span><CommonCopyButton :value="info.repository" aria-label="Copy repository" />
      </dd>
      <template v-if="info.recordLocation">
        <dt class="text-muted">
          Recovery record
        </dt>
        <dd class="flex min-w-0 items-center gap-1 font-mono text-xs">
          <span class="truncate" :title="info.recordLocation">{{ info.recordLocation }}</span><CommonCopyButton :value="info.recordLocation" aria-label="Copy record location" />
        </dd>
      </template>
      <dt class="text-muted">
        Password
      </dt>
      <dd class="text-xs break-words">
        {{ info.passwordSource || '—' }}
      </dd>
      <template v-if="info.envNames?.length">
        <dt class="text-muted">
          Environment
        </dt>
        <dd class="font-mono text-xs break-words">
          {{ info.envNames.join(', ') }}
        </dd>
      </template>
      <template v-if="info.options?.length">
        <dt class="text-muted">
          Options
        </dt>
        <dd class="font-mono text-xs break-words">
          {{ info.options.map(o => `-o ${o}`).join(' ') }}
        </dd>
      </template>
    </dl>
    <div class="relative">
      <pre class="overflow-x-auto rounded-md bg-elevated p-3 pr-10 text-xs leading-relaxed" :aria-label="`restic commands (${shell === 'bash' ? 'bash' : 'PowerShell'})`" data-testid="restic-snippet">{{ snippet }}</pre>
      <CommonCopyButton :value="snippet" class="absolute top-2 right-2" aria-label="Copy commands" />
    </div>
  </div>
</template>
