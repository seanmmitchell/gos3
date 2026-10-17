<script setup lang="ts">
import type { ResticCredentials, ResticTags } from '~/types/admin'
import { MASK, isSecretEnv, maskCredentials, resticSnippet, snippetFromCredentials, type Shell } from './-restic'

/**
 * Shows revealed restic credentials once (POST
 * /buckets/{name}/restic/credentials): the repository, its password, the
 * further environment variables and ready-to-paste commands for bash and
 * PowerShell, each with a copy button. Secrets (the password and secret
 * variables, also inside the commands) are masked until "Show secrets";
 * the copy buttons always copy the real values. The values live only in
 * this dialog: they are never stored and are gone when it closes.
 *
 * Opened through useOverlay() with destroyOnClose; emits close().
 */
const props = defineProps<{
  bucket: string
  credentials: ResticCredentials
  tags: ResticTags
  /** This server's URL, for the gos3 CLI's --server. */
  server?: string
}>()

const emit = defineEmits<{ close: [] }>()

const shell = ref<Shell>('bash')
const showPassword = ref(false)
const env = computed(() => Object.entries(props.credentials.env ?? {}).sort(([a], [b]) => a.localeCompare(b)))
/** The real commands (copied). */
const snippet = computed(() => resticSnippet(snippetFromCredentials(shell.value, props.bucket, props.credentials, props.tags, props.server)))
/** The commands as shown: secrets masked until shown. */
const shownSnippet = computed(() => (showPassword.value ? snippet.value : resticSnippet(snippetFromCredentials(shell.value, props.bucket, maskCredentials(props.credentials), props.tags, props.server))))
</script>

<template>
  <UModal
    title="Restic credentials"
    :description="`${credentials.purpose === 'snapshots' ? 'Whole-bucket snapshot' : 'Archive'} repository of ${bucket} on ${credentials.backing}. Shown once: they are not stored, and closing this dialog discards them.`"
    :ui="{ content: 'sm:max-w-2xl', footer: 'justify-end' }"
    :dismissible="false"
  >
    <template #body>
      <div class="space-y-4" data-testid="restic-credentials">
        <UAlert
          color="warning"
          variant="subtle"
          icon="i-lucide-shield-alert"
          title="Keep these secret"
          description="Anyone with the password and access to the repository can read every archived object. The server logged this disclosure."
        />
        <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
          <dt class="text-muted">
            Repository
          </dt>
          <dd class="flex min-w-0 items-center gap-1 font-mono text-xs">
            <span class="truncate" :title="credentials.repository">{{ credentials.repository }}</span><CommonCopyButton :value="credentials.repository" aria-label="Copy repository" />
          </dd>
          <dt class="text-muted">
            Password
          </dt>
          <dd class="flex min-w-0 items-center gap-1 font-mono text-xs">
            <span class="truncate" data-testid="restic-password">{{ showPassword ? credentials.password : MASK }}</span>
            <UButton
              :icon="showPassword ? 'i-lucide-eye-off' : 'i-lucide-eye'"
              color="neutral"
              variant="ghost"
              size="xs"
              :aria-label="showPassword ? 'Hide secrets' : 'Show secrets'"
              :aria-pressed="showPassword"
              data-testid="show-secrets"
              @click="showPassword = !showPassword"
            />
            <CommonCopyButton :value="credentials.password" aria-label="Copy password" />
          </dd>
          <template v-for="[name, value] in env" :key="name">
            <dt class="font-mono text-xs text-muted">
              {{ name }}
            </dt>
            <dd class="flex min-w-0 items-center gap-1 font-mono text-xs">
              <span class="truncate">{{ isSecretEnv(name) && !showPassword ? MASK : value }}</span><CommonCopyButton :value="value" :aria-label="`Copy ${name}`" />
            </dd>
          </template>
        </dl>

        <div class="space-y-2">
          <div class="flex items-center justify-between gap-2">
            <h3 class="text-sm font-medium text-highlighted">
              Commands
            </h3>
            <UFieldGroup size="xs" aria-label="Shell">
              <UButton :color="shell === 'bash' ? 'primary' : 'neutral'" :variant="shell === 'bash' ? 'subtle' : 'outline'" label="bash" :aria-pressed="shell === 'bash'" @click="shell = 'bash'" />
              <UButton :color="shell === 'powershell' ? 'primary' : 'neutral'" :variant="shell === 'powershell' ? 'subtle' : 'outline'" label="PowerShell" :aria-pressed="shell === 'powershell'" @click="shell = 'powershell'" />
            </UFieldGroup>
          </div>
          <div class="relative">
            <pre class="max-h-72 overflow-auto rounded-md bg-elevated p-3 pr-10 text-xs leading-relaxed" data-testid="restic-credentials-snippet">{{ shownSnippet }}</pre>
            <CommonCopyButton :value="snippet" class="absolute top-2 right-2" aria-label="Copy commands" />
          </div>
          <p class="text-xs text-muted">
            The commands set the variables for the current shell session only.
          </p>
        </div>
      </div>
    </template>
    <template #footer>
      <UButton label="Done" @click="emit('close')" />
    </template>
  </UModal>
</template>
