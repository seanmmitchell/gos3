<script setup lang="ts">
import type { AccessKey, User } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { canSimulate, identityPending, usersPageMode } from '~/components/admin/-capabilities'
import CreateKeyModal from '~/components/admin/CreateKeyModal.vue'
import NewAccessKeyModal from '~/components/admin/NewAccessKeyModal.vue'

/**
 * IAM user detail: enable/disable, description + policies, access keys
 * (create with one-time secret, activate/deactivate, delete), effective
 * permission check and delete.
 */
const route = useRoute()
const router = useRouter()
const id = computed(() => routeParam(route.params.id))
const name = computed(() => routeParam(route.params.name))
const { connection, requireAdmin } = useConnection()
const { capabilities, whoami, status } = useWhoAmI()
const overlay = useOverlay()
const confirm = useConfirm()

useHead({ title: () => `User ${name.value}` })

const mode = computed(() => usersPageMode(connection.value?.kind, capabilities.value))
const pending = computed(() => identityPending(connection.value?.kind, status.value))
const simulate = computed(() => canSimulate(connection.value?.kind, capabilities.value))

const user = useLoader(() => requireAdmin().getUser(name.value), { immediate: false })
const policies = useLoader(() => requireAdmin().listPolicies().catch(() => []), { immediate: false, initial: [] })
watch([mode, id, name], ([m]) => {
  if (m === 'manage') {
    void user.refresh()
    void policies.refresh()
  }
}, { immediate: true })

const notFound = computed(() => user.error.value instanceof AdminError && user.error.value.isNotFound)
const isSelf = computed(() => !!whoami.value && !whoami.value.isRoot && whoami.value.user === name.value)
const currentKey = computed(() => (isSelf.value ? whoami.value?.accessKey : undefined))

// --- details form ------------------------------------------------------------
const description = ref('')
const selectedPolicies = ref<string[]>([])
function syncForm(u: User | undefined) {
  description.value = u?.description ?? ''
  selectedPolicies.value = [...(u?.policies ?? [])]
}
watch(() => user.data.value, syncForm, { immediate: true })

const policyItems = computed(() => {
  const names = new Set((policies.data.value ?? []).map(p => p.name))
  for (const p of selectedPolicies.value) names.add(p)
  return [...names].sort()
})
const detailsDirty = computed(() => {
  const u = user.data.value
  if (!u) return false
  const a = [...(u.policies ?? [])].sort().join('\n')
  const b = [...selectedPolicies.value].sort().join('\n')
  return description.value !== (u.description ?? '') || a !== b
})
const descriptionError = computed(() => (description.value.length > 1024 ? 'At most 1024 characters' : undefined))

const saveDetails = useAsyncAction(async () => {
  const updated = await requireAdmin().updateUser(name.value, { description: description.value.trim(), policies: selectedPolicies.value })
  user.data.value = updated
  return updated
}, { successMessage: 'User updated', errorTitle: 'Could not update user' })

async function onSaveDetails() {
  if (descriptionError.value) return
  if (isSelf.value && !selectedPolicies.value.length && (user.data.value?.policies?.length ?? 0) > 0) {
    const ok = await confirm({ title: 'Remove all of your own policies?', description: 'You may lose access to this console.', confirmLabel: 'Save anyway', color: 'warning' })
    if (!ok) return
  }
  await saveDetails.run()
}

// --- enable / disable / delete -------------------------------------------
const setDisabled = useAsyncAction(async (disabled: boolean) => {
  const updated = await requireAdmin().updateUser(name.value, { disabled })
  user.data.value = updated
}, { successMessage: d => `User ${d ? 'disabled' : 'enabled'}`, errorTitle: 'Could not update user' })

async function toggleDisabled() {
  const u = user.data.value
  if (!u) return
  if (!u.disabled) {
    const ok = await confirm({
      title: `Disable ${u.name}?`,
      description: isSelf.value ? 'This is your own user: this session will stop working immediately.' : 'Requests signed with any of this user\'s keys will be rejected until the user is enabled again.',
      confirmLabel: 'Disable',
      color: 'warning',
      icon: 'i-lucide-user-x'
    })
    if (!ok) return
  }
  await setDisabled.run(!u.disabled)
}

const deleteUser = useAsyncAction(() => requireAdmin().deleteUser(name.value), { successMessage: () => `User ${name.value} deleted`, errorTitle: 'Could not delete user', rethrow: true })

async function onDelete() {
  const keys = user.data.value?.keys?.length ?? 0
  const ok = await confirm({
    title: `Delete user ${name.value}?`,
    description: `The user and ${keys} access key${keys === 1 ? '' : 's'} will be deleted. ${isSelf.value ? 'This is your own user — this session will stop working.' : 'Applications using them stop working immediately.'}`,
    confirmLabel: 'Delete user',
    requireText: name.value,
    icon: 'i-lucide-trash-2'
  })
  if (!ok) return
  try {
    await deleteUser.run()
    await router.push(appRoutes.users(id.value))
  } catch {
    // toast already shown
  }
}

// --- access keys ---------------------------------------------------------------
const keys = computed(() => [...(user.data.value?.keys ?? [])].sort((a, b) => a.createdAt.localeCompare(b.createdAt)))
const busyKey = ref<string | null>(null)

const createKey = useAsyncAction(async () => {
  const req = await overlay.create(CreateKeyModal, { destroyOnClose: true }).open({ title: `Create access key for ${name.value}`, allowCustom: true })
  if (!req) return null
  const key = await requireAdmin().createAccessKey(name.value, req)
  void user.refresh()
  return key
}, { errorTitle: 'Could not create access key' })

async function onCreateKey() {
  const key = await createKey.run()
  if (!key || !connection.value) return
  await overlay.create(NewAccessKeyModal, { destroyOnClose: true }).open({
    accessKey: key,
    user: name.value,
    endpoint: connection.value.endpoint,
    region: connection.value.region
  })
}

const toggleKey = useAsyncAction(async (k: AccessKey) => {
  busyKey.value = k.accessKey
  try {
    await requireAdmin().updateAccessKey(k.accessKey, k.status === 'Active' ? 'Inactive' : 'Active')
    await user.refresh()
  } finally {
    busyKey.value = null
  }
}, { successMessage: k => `Access key ${k.accessKey} ${k.status === 'Active' ? 'deactivated' : 'activated'}`, errorTitle: 'Could not update access key' })

async function onToggleKey(k: AccessKey) {
  if (k.status === 'Active') {
    const ok = await confirm({
      title: `Deactivate ${k.accessKey}?`,
      description: 'Requests signed with this key will be rejected until it is activated again.',
      confirmLabel: 'Deactivate',
      color: 'warning'
    })
    if (!ok) return
  }
  await toggleKey.run(k)
}

const deleteKey = useAsyncAction(async (k: AccessKey) => {
  busyKey.value = k.accessKey
  try {
    await requireAdmin().deleteAccessKey(k.accessKey)
    await user.refresh()
  } finally {
    busyKey.value = null
  }
}, { successMessage: k => `Access key ${k.accessKey} deleted`, errorTitle: 'Could not delete access key' })

async function onDeleteKey(k: AccessKey) {
  const ok = await confirm({
    title: `Delete access key ${k.accessKey}?`,
    description: 'Applications using this key will stop working immediately. This cannot be undone.',
    confirmLabel: 'Delete key',
    icon: 'i-lucide-trash-2'
  })
  if (ok) await deleteKey.run(k)
}

</script>

<template>
  <div class="space-y-6">
    <CommonPageHeader :title="name" icon="i-lucide-user" :description="user.data.value?.description || 'IAM user'">
      <template v-if="user.data.value" #badge>
        <UBadge
          :label="user.data.value.disabled ? 'Disabled' : 'Enabled'"
          :color="user.data.value.disabled ? 'neutral' : 'success'"
          variant="subtle"
          size="sm"
        />
        <UBadge v-if="isSelf" label="you" color="info" variant="subtle" size="sm" />
      </template>
      <template v-if="mode === 'manage' && user.data.value" #actions>
        <UButton
          color="neutral"
          variant="outline"
          :icon="user.data.value.disabled ? 'i-lucide-user-check' : 'i-lucide-user-x'"
          :label="user.data.value.disabled ? 'Enable' : 'Disable'"
          :loading="setDisabled.loading.value"
          @click="toggleDisabled"
        />
        <UButton color="error" variant="soft" icon="i-lucide-trash-2" label="Delete" :loading="deleteUser.loading.value" @click="onDelete" />
      </template>
    </CommonPageHeader>

    <div v-if="pending" class="space-y-3" aria-busy="true">
      <USkeleton class="h-10 w-full" />
      <USkeleton class="h-40 w-full" />
    </div>
    <CommonEmptyState
      v-else-if="mode !== 'manage'"
      icon="i-lucide-lock"
      title="Not allowed"
      :description="mode === 'unavailable' ? 'IAM users are only available on gos3 servers.' : 'Managing users requires the admin:CreateUser permission.'"
    >
      <UButton v-if="mode === 'self'" size="sm" color="neutral" variant="outline" label="My access keys" :to="appRoutes.users(id)" />
    </CommonEmptyState>

    <CommonEmptyState v-else-if="notFound" icon="i-lucide-user-x" title="User not found" :description="`There is no user named “${name}”.`">
      <UButton size="sm" color="neutral" variant="outline" icon="i-lucide-arrow-left" label="All users" :to="appRoutes.users(id)" />
    </CommonEmptyState>

    <CommonErrorAlert v-else-if="user.error.value" :error="user.error.value" title="Could not load user" retryable @retry="user.refresh" />

    <div v-else-if="!user.data.value" class="space-y-4">
      <USkeleton class="h-40 w-full" />
      <USkeleton class="h-48 w-full" />
    </div>

    <template v-else>
      <div class="grid gap-6 xl:grid-cols-[minmax(0,1fr)_20rem]">
        <UCard>
          <template #header>
            <h2 class="font-semibold text-highlighted">
              Details
            </h2>
          </template>
          <form class="space-y-4" novalidate @submit.prevent="onSaveDetails">
            <UFormField label="Description" name="description" :error="descriptionError">
              <UInput v-model="description" class="w-full" placeholder="Optional" />
            </UFormField>
            <UFormField label="Policies" name="policies" help="Identity policies attached to this user.">
              <USelectMenu
                v-model="selectedPolicies"
                :items="policyItems"
                multiple
                class="w-full"
                placeholder="No policies"
                :search-input="{ placeholder: 'Filter policies…' }"
              />
            </UFormField>
            <div v-if="selectedPolicies.length" class="flex flex-wrap gap-1.5">
              <UBadge v-for="p in selectedPolicies" :key="p" color="neutral" variant="outline" size="sm">
                <NuxtLink :to="appRoutes.policy(id, p)" class="hover:text-primary">
                  {{ p }}
                </NuxtLink>
              </UBadge>
            </div>
            <div class="flex justify-end gap-2">
              <UButton color="neutral" variant="ghost" label="Reset" :disabled="!detailsDirty" @click="syncForm(user.data.value)" />
              <UButton type="submit" icon="i-lucide-save" label="Save" :disabled="!detailsDirty || !!descriptionError" :loading="saveDetails.loading.value" />
            </div>
          </form>
        </UCard>
        <UCard>
          <dl class="space-y-3 text-sm">
            <div>
              <dt class="text-muted">
                Created
              </dt>
              <dd><CommonRelativeTime :value="user.data.value.createdAt" /></dd>
            </div>
            <div>
              <dt class="text-muted">
                Last updated
              </dt>
              <dd><CommonRelativeTime :value="user.data.value.updatedAt" /></dd>
            </div>
            <div>
              <dt class="text-muted">
                Access keys
              </dt>
              <dd class="tabular-nums">
                {{ keys.length }} ({{ keys.filter(k => k.status === 'Active').length }} active)
              </dd>
            </div>
          </dl>
        </UCard>
      </div>

      <UCard>
        <template #header>
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div>
              <h2 class="font-semibold text-highlighted">
                Access keys
              </h2>
              <p class="text-sm text-muted">
                Secrets are shown only once, when a key is created.
              </p>
            </div>
            <UButton icon="i-lucide-plus" label="Create key" :loading="createKey.loading.value" @click="onCreateKey" />
          </div>
        </template>
        <AdminAccessKeysTable
          :keys="keys"
          :loading="user.loading.value"
          :current-access-key="currentKey"
          :busy="busyKey"
          @toggle="onToggleKey"
          @delete="onDeleteKey"
        />
      </UCard>

      <AdminSimulatorPanel
        v-if="simulate"
        :fixed-user="name"
        title="Effective permissions"
        :description="`Check what ${name} may do, including bucket policies and ACLs.`"
      />
    </template>
  </div>
</template>
