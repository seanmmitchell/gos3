<script setup lang="ts">
import type { CannedACL } from '~/types/s3'
import { S3Error } from '~/utils/s3/errors'
import { deletePublicAccessBlock, getPublicAccessBlock, PAB_FIELDS, PAB_NONE, putPublicAccessBlock, type PublicAccessBlockConfig } from './-public-access-block'

/** Canned bucket ACL and the public access block toggles. */
const props = defineProps<{ bucket: string }>()
const emit = defineEmits<{ changed: [] }>()

const { requireS3 } = useConnection()
const confirm = useConfirm()

// --- canned ACL -------------------------------------------------------------
const ACL_ITEMS: { label: string, value: CannedACL, description: string }[] = [
  { label: 'private', value: 'private', description: 'Owner only (recommended).' },
  { label: 'public-read', value: 'public-read', description: 'Anyone can list the bucket and read objects.' },
  { label: 'public-read-write', value: 'public-read-write', description: 'Anyone can list, read, write and delete objects.' },
  { label: 'authenticated-read', value: 'authenticated-read', description: 'Any authenticated user can read.' }
]
const acl = ref<CannedACL | 'custom'>('private')
const aclLoader = useLoader(async () => {
  const r = await requireS3().getBucketAcl(props.bucket)
  acl.value = r.canned
  return r
}, { watch: [() => props.bucket] })
const aclDirty = computed(() => !!aclLoader.data.value && acl.value !== aclLoader.data.value.canned)
const aclItems = computed(() => (acl.value === 'custom' || aclLoader.data.value?.canned === 'custom'
  ? [...ACL_ITEMS, { label: 'custom', value: 'custom' as const, description: 'Grants that do not match a canned ACL.' }]
  : ACL_ITEMS))

const saveAcl = useAsyncAction(async () => {
  if (acl.value === 'custom') return
  await requireS3().putBucketAcl(props.bucket, acl.value)
  await aclLoader.refresh()
  emit('changed')
}, { successMessage: 'Bucket ACL saved', errorTitle: 'Could not save the ACL' })

async function onSaveAcl() {
  if (acl.value === 'public-read' || acl.value === 'public-read-write') {
    const ok = await confirm({
      title: `Make ${props.bucket} ${acl.value}?`,
      description: acl.value === 'public-read-write'
        ? 'Anyone can upload, overwrite and delete objects in this bucket without credentials.'
        : 'Anyone can list and download every object in this bucket without credentials.',
      confirmLabel: 'Make public',
      color: 'warning',
      icon: 'i-lucide-globe',
      requireText: acl.value === 'public-read-write' ? props.bucket : undefined
    })
    if (!ok) return
  }
  await saveAcl.run()
}

// --- public access block -----------------------------------------------------
const pab = ref<PublicAccessBlockConfig>({ ...PAB_NONE })
const pabExists = ref(false)
const pabUnsupported = ref(false)
const pabOriginal = ref('')
const pabLoader = useLoader(async () => {
  pabUnsupported.value = false
  try {
    const cfg = await getPublicAccessBlock(requireS3(), props.bucket)
    pab.value = { ...(cfg ?? PAB_NONE) }
    pabExists.value = cfg !== null
  } catch (e) {
    if (e instanceof S3Error && (e.code === 'NotImplemented' || e.status === 501)) {
      pabUnsupported.value = true
      pab.value = { ...PAB_NONE }
    } else {
      throw e
    }
  }
  pabOriginal.value = JSON.stringify(pab.value)
  return pab.value
}, { watch: [() => props.bucket] })
const pabDirty = computed(() => JSON.stringify(pab.value) !== pabOriginal.value)
const allBlocked = computed(() => PAB_FIELDS.every(f => pab.value[f.key]))

const savePab = useAsyncAction(async () => {
  const s3 = requireS3()
  const any = PAB_FIELDS.some(f => pab.value[f.key])
  if (any) await putPublicAccessBlock(s3, props.bucket, pab.value)
  else if (pabExists.value) await deletePublicAccessBlock(s3, props.bucket)
  await pabLoader.refresh()
  emit('changed')
}, { successMessage: 'Public access block saved', errorTitle: 'Could not save public access block' })

function setAll(v: boolean) {
  pab.value = { blockPublicAcls: v, ignorePublicAcls: v, blockPublicPolicy: v, restrictPublicBuckets: v }
}
</script>

<template>
  <div class="space-y-6">
    <UCard>
      <template #header>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h3 class="font-semibold text-highlighted">
              Access control list
            </h3>
            <p class="text-sm text-muted">
              Canned ACL of the bucket. Prefer bucket policies for fine-grained access.
            </p>
          </div>
          <UBadge v-if="aclDirty" label="Unsaved" color="warning" variant="subtle" />
        </div>
      </template>
      <CommonErrorAlert v-if="aclLoader.error.value" :error="aclLoader.error.value" title="Could not load the ACL" retryable @retry="aclLoader.refresh" />
      <USkeleton v-else-if="aclLoader.loading.value && !aclLoader.data.value" class="h-24 w-full" />
      <form v-else class="space-y-4" novalidate @submit.prevent="onSaveAcl">
        <URadioGroup v-model="acl" :items="aclItems" legend="Canned ACL" />
        <UAlert
          v-if="acl === 'public-read' || acl === 'public-read-write'"
          color="warning"
          variant="subtle"
          icon="i-lucide-globe"
          title="Public bucket"
          description="Anonymous users can access this bucket. A public access block with “Block public ACLs” rejects this setting."
        />
        <div class="flex justify-end border-t border-default pt-4">
          <UButton type="submit" icon="i-lucide-save" label="Save ACL" :disabled="!aclDirty || acl === 'custom'" :loading="saveAcl.loading.value" />
        </div>
      </form>
    </UCard>

    <UCard>
      <template #header>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h3 class="font-semibold text-highlighted">
              Block public access
            </h3>
            <p class="text-sm text-muted">
              Guard rails that override public ACLs and bucket policies.
            </p>
          </div>
          <UBadge v-if="pabDirty" label="Unsaved" color="warning" variant="subtle" />
          <UBadge v-else-if="allBlocked" label="All public access blocked" color="success" variant="subtle" icon="i-lucide-shield-check" />
        </div>
      </template>
      <CommonErrorAlert v-if="pabLoader.error.value" :error="pabLoader.error.value" title="Could not load the public access block" retryable @retry="pabLoader.refresh" />
      <USkeleton v-else-if="pabLoader.loading.value && !pabOriginal" class="h-32 w-full" />
      <CommonEmptyState v-else-if="pabUnsupported" compact icon="i-lucide-ban" title="Not supported" description="This server does not implement the public access block API." />
      <div v-else class="space-y-4">
        <div class="flex flex-wrap gap-2">
          <UButton size="xs" color="neutral" variant="outline" label="Block all" icon="i-lucide-shield-check" @click="setAll(true)" />
          <UButton size="xs" color="neutral" variant="ghost" label="Allow all" @click="setAll(false)" />
        </div>
        <div class="grid gap-4 md:grid-cols-2">
          <USwitch
            v-for="f in PAB_FIELDS"
            :key="f.key"
            v-model="pab[f.key]"
            :label="f.label"
            :description="f.description"
          />
        </div>
        <div class="flex justify-end border-t border-default pt-4">
          <UButton icon="i-lucide-save" label="Save" :disabled="!pabDirty" :loading="savePab.loading.value" @click="savePab.run()" />
        </div>
      </div>
    </UCard>
  </div>
</template>
