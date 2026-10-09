<script setup lang="ts">
import type { AccessKey } from '~/types/admin'
import CreateKeyModal from './CreateKeyModal.vue'
import NewAccessKeyModal from './NewAccessKeyModal.vue'

/**
 * Self-service access keys of the signed-in IAM user (/whoami/keys): list,
 * create (secret shown once) and delete. The key signing this session
 * cannot be deleted.
 */
const { connection, requireAdmin } = useConnection()
const { whoami, refresh: refreshWhoAmI } = useWhoAmI()
const overlay = useOverlay()
const confirm = useConfirm()

const { data, loading, error, refresh } = useLoader(() => requireAdmin().listOwnKeys(), {
  watch: [() => connection.value?.id],
  initial: []
})
const keys = computed(() => data.value ?? [])
const busy = ref<string | null>(null)

const create = useAsyncAction(async () => {
  const req = await overlay.create(CreateKeyModal, { destroyOnClose: true }).open({ title: 'Create my access key' })
  if (!req) return null
  const key = await requireAdmin().createOwnKey(req.description)
  await refresh()
  return key
}, { errorTitle: 'Could not create access key' })

async function onCreate() {
  const key = await create.run()
  if (!key || !connection.value) return
  await overlay.create(NewAccessKeyModal, { destroyOnClose: true }).open({
    accessKey: key,
    user: whoami.value?.user,
    endpoint: connection.value.endpoint,
    region: connection.value.region
  })
}

const remove = useAsyncAction(async (key: AccessKey) => {
  busy.value = key.accessKey
  try {
    await requireAdmin().deleteOwnKey(key.accessKey)
    await refresh()
  } finally {
    busy.value = null
  }
}, { successMessage: key => `Access key ${key.accessKey} deleted`, errorTitle: 'Could not delete access key' })

async function onDelete(key: AccessKey) {
  const ok = await confirm({
    title: `Delete access key ${key.accessKey}?`,
    description: 'Applications using this key will stop working immediately.',
    confirmLabel: 'Delete key',
    icon: 'i-lucide-trash-2'
  })
  if (ok) {
    await remove.run(key)
    void refreshWhoAmI()
  }
}
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 class="font-semibold text-highlighted">
            My access keys
          </h2>
          <p class="text-sm text-muted">
            Keys of <span class="font-medium">{{ whoami?.user || 'this identity' }}</span>. The key signing this session cannot be deleted here.
          </p>
        </div>
        <UButton icon="i-lucide-plus" label="Create key" :loading="create.loading.value" @click="onCreate" />
      </div>
    </template>
    <CommonErrorAlert v-if="error" :error="error" retryable class="mb-4" @retry="refresh" />
    <AdminAccessKeysTable
      v-else
      :keys="keys"
      :loading="loading"
      :can-toggle="false"
      :current-access-key="whoami?.accessKey"
      :busy="busy"
      @delete="onDelete"
    />
  </UCard>
</template>
