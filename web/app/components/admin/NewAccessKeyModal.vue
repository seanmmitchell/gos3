<script setup lang="ts">
import type { AccessKey } from '~/types/admin'
import { credentialsFileSnippet, downloadText, envSnippet, profileName } from './-credentials'

/**
 * Shows a freshly created access key with its secret — the only time the
 * secret is ever available — with copy and download (.env / AWS credentials
 * file) helpers. Opened through useOverlay(); emits close() when done.
 */
const props = defineProps<{
  accessKey: AccessKey
  /** Owner of the key (used as the AWS profile name). */
  user?: string
  endpoint: string
  region: string
}>()

const emit = defineEmits<{ close: [] }>()

const saved = ref(false)
const secret = computed(() => props.accessKey.secretKey ?? '')
const input = computed(() => ({
  accessKey: props.accessKey.accessKey,
  secretKey: secret.value,
  endpoint: props.endpoint,
  region: props.region,
  profile: props.user
}))
const env = computed(() => envSnippet(input.value))
const credentials = computed(() => credentialsFileSnippet(input.value))
const baseName = computed(() => profileName(props.user || props.accessKey.accessKey))
</script>

<template>
  <UModal
    title="Access key created"
    description="Copy or download the secret key now. It cannot be shown again."
    :dismissible="false"
    :close="false"
    :ui="{ content: 'sm:max-w-2xl', footer: 'justify-end' }"
  >
    <template #body>
      <div class="space-y-4">
        <UAlert
          color="warning"
          variant="subtle"
          icon="i-lucide-triangle-alert"
          title="This is the only time the secret key is shown"
          description="Store it in a password manager or your application's configuration. If you lose it, delete the key and create a new one."
        />
        <dl class="grid gap-3">
          <div>
            <dt class="text-xs font-medium text-muted">
              Access key ID
            </dt>
            <dd class="mt-1 flex items-center gap-2 rounded-md border border-default bg-elevated/50 px-3 py-2">
              <code class="min-w-0 flex-1 break-all font-mono text-sm text-highlighted">{{ accessKey.accessKey }}</code>
              <CommonCopyButton :value="accessKey.accessKey" aria-label="Copy access key ID" />
            </dd>
          </div>
          <div>
            <dt class="text-xs font-medium text-muted">
              Secret access key
            </dt>
            <dd class="mt-1 flex items-center gap-2 rounded-md border border-warning/50 bg-warning/5 px-3 py-2">
              <code class="min-w-0 flex-1 break-all font-mono text-sm text-highlighted" data-testid="secret-key">{{ secret }}</code>
              <CommonCopyButton :value="secret" aria-label="Copy secret access key" />
            </dd>
          </div>
        </dl>
        <div class="space-y-2">
          <p class="text-sm font-medium text-default">
            Configuration snippets
          </p>
          <div class="grid gap-3 md:grid-cols-2">
            <div class="min-w-0 space-y-1">
              <div class="flex items-center justify-between gap-2">
                <span class="text-xs text-muted">.env</span>
                <div class="flex items-center gap-1">
                  <CommonCopyButton :value="env" aria-label="Copy .env snippet" />
                  <UButton
                    size="xs"
                    color="neutral"
                    variant="ghost"
                    icon="i-lucide-download"
                    label="Download"
                    :aria-label="`Download ${baseName}.env`"
                    @click="downloadText(`${baseName}.env`, env)"
                  />
                </div>
              </div>
              <pre class="overflow-x-auto rounded-md border border-default bg-elevated/50 p-2 font-mono text-xs">{{ env }}</pre>
            </div>
            <div class="min-w-0 space-y-1">
              <div class="flex items-center justify-between gap-2">
                <span class="text-xs text-muted">AWS credentials / config</span>
                <div class="flex items-center gap-1">
                  <CommonCopyButton :value="credentials" aria-label="Copy AWS credentials snippet" />
                  <UButton
                    size="xs"
                    color="neutral"
                    variant="ghost"
                    icon="i-lucide-download"
                    label="Download"
                    :aria-label="`Download ${baseName}-credentials.ini`"
                    @click="downloadText(`${baseName}-credentials.ini`, credentials)"
                  />
                </div>
              </div>
              <pre class="max-h-48 overflow-auto rounded-md border border-default bg-elevated/50 p-2 font-mono text-xs">{{ credentials }}</pre>
            </div>
          </div>
        </div>
        <div class="flex items-center gap-2">
          <UCheckbox v-model="saved" label="I have stored the secret key" />
        </div>
      </div>
    </template>
    <template #footer>
      <UButton label="Done" :disabled="!saved" @click="emit('close')" />
    </template>
  </UModal>
</template>
