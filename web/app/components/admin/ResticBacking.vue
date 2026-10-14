<script setup lang="ts">
import type { ResticBacking } from '~/types/admin'
const props = defineProps<{ backing: string }>()
const emit = defineEmits<{ close: [] }>()
const { requireAdmin } = useConnection()
const { can } = useWhoAmI()
const form = reactive({ mode: 'local' as 'local' | 's3', endpoint: '', bucket: '', prefix: '', region: '', accessKey: '', secretKey: '', sessionToken: '', clearSessionToken: false })
const original = ref('')
const signature = () => JSON.stringify(form)
const dirty = computed(() => signature() !== original.value)
function hydrate(doc: ResticBacking) {
  Object.assign(form, { mode: doc.mode, endpoint: doc.endpoint, bucket: doc.bucket, prefix: doc.prefix, region: doc.region, accessKey: '', secretKey: '', sessionToken: '', clearSessionToken: false })
  original.value = signature()
}
const loader = useLoader(async () => {
  const doc = await requireAdmin().getBackingRestic(props.backing)
  hydrate(doc)
  return doc
}, { watch: [() => props.backing] })
const locked = computed(() => loader.data.value?.locked ?? false)
const save = useAsyncAction(async () => {
  const input = form.mode === 'local' ? { mode: 'local' as const } : {
    mode: 's3' as const, endpoint: form.endpoint.trim(), bucket: form.bucket.trim(), prefix: form.prefix.trim(), region: form.region.trim(),
    accessKey: form.accessKey || undefined, secretKey: form.secretKey || undefined, sessionToken: form.sessionToken || undefined, clearSessionToken: form.clearSessionToken
  }
  const doc = await requireAdmin().putBackingRestic(props.backing, input)
  hydrate(doc)
  loader.data.value = doc
}, { successMessage: 'Backing restic storage saved', errorTitle: 'Could not save backing restic storage' })
</script>

<template>
  <UCard data-testid="restic-backing-settings">
    <template #header>
      <div class="flex items-center justify-between gap-2">
        <div>
          <h2 class="font-semibold text-highlighted">Restic storage · {{ backing }}</h2>
          <p class="text-sm text-muted">Buckets placed on this backing inherit these repository settings when they use restic encryption.</p>
        </div>
        <UButton label="Close" color="neutral" variant="ghost" @click="emit('close')" />
      </div>
    </template>
    <CommonErrorAlert v-if="loader.error.value" :error="loader.error.value" title="Could not load backing restic storage" retryable @retry="loader.refresh" />
    <USkeleton v-else-if="!loader.data.value" class="h-32" />
    <form v-else class="space-y-4" @submit.prevent="save.run()">
      <CommonErrorAlert v-if="save.error.value" :error="save.error.value" title="Could not save backing restic storage" />
      <URadioGroup v-model="form.mode" legend="Repository storage" :disabled="locked || !can('manageConfig')" :items="[{ label: 'Local backing storage', value: 'local' }, { label: 'S3 storage', value: 's3' }]" />
      <p v-if="form.mode === 'local'" class="text-sm text-muted">Each bucket uses a separate repository inside this backing’s protected system directory.</p>
      <div v-else class="grid gap-4 sm:grid-cols-2">
        <UFormField label="S3 endpoint"><UInput v-model="form.endpoint" class="w-full" placeholder="https://s3.example.com" :disabled="locked || !can('manageConfig')" /></UFormField>
        <UFormField label="S3 bucket"><UInput v-model="form.bucket" class="w-full" placeholder="archives" :disabled="locked || !can('manageConfig')" /></UFormField>
        <UFormField label="Repository prefix" help="Each bucket gets its own repository beneath this prefix."><UInput v-model="form.prefix" class="w-full" placeholder="gos3" :disabled="locked || !can('manageConfig')" /></UFormField>
        <UFormField label="S3 region"><UInput v-model="form.region" class="w-full" placeholder="us-east-1" :disabled="!can('manageConfig')" /></UFormField>
        <UFormField label="S3 access key" :help="loader.data.value.credentialsConfigured ? 'Configured. Leave blank to retain.' : undefined"><UInput v-model="form.accessKey" class="w-full" autocomplete="off" :disabled="!can('manageConfig')" /></UFormField>
        <UFormField label="S3 secret key"><UInput v-model="form.secretKey" class="w-full" type="password" autocomplete="new-password" placeholder="Leave blank to retain" :disabled="!can('manageConfig')" /></UFormField>
        <UFormField label="S3 session token (optional)" :help="loader.data.value.sessionTokenConfigured ? 'Configured. Leave blank to retain.' : undefined"><UInput v-model="form.sessionToken" class="w-full" type="password" autocomplete="new-password" :disabled="!can('manageConfig')" /></UFormField>
        <USwitch v-if="loader.data.value.sessionTokenConfigured" v-model="form.clearSessionToken" label="Clear saved session token" :disabled="!can('manageConfig')" />
      </div>
      <p class="text-sm text-muted">Repository passwords are managed by the server for each bucket copy. Replicas and placement moves use their destination backing’s storage and credentials.</p>
      <p v-if="locked" class="text-sm text-muted">This backing has initialized repositories. To change their location, configure another backing and move the buckets using Placement. Credentials can still be updated.</p>
      <div class="flex justify-end border-t border-default pt-4">
        <UButton type="submit" label="Save backing storage" icon="i-lucide-save" :disabled="!dirty || !can('manageConfig')" :loading="save.loading.value" />
      </div>
    </form>
  </UCard>
</template>
