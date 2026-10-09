<script setup lang="ts">
import type { DropdownMenuItem, TableColumn } from '@nuxt/ui'
import type { User } from '~/types/admin'
import { canManageOwnKeys, canSimulate, identityPending, usersPageMode } from '~/components/admin/-capabilities'
import CreateUserModal from '~/components/admin/CreateUserModal.vue'
import NewAccessKeyModal from '~/components/admin/NewAccessKeyModal.vue'

/**
 * IAM users: list, create (optionally with an access key whose secret is
 * shown once), enable/disable and delete. Identities without
 * admin:CreateUser get their own access keys (self-service) instead.
 */
useHead({ title: 'Users' })

const route = useRoute()
const id = computed(() => routeParam(route.params.id))
const { connection, requireAdmin } = useConnection()
const { capabilities, whoami, isRoot, status, loading: whoamiLoading } = useWhoAmI()
const overlay = useOverlay()
const confirm = useConfirm()

const mode = computed(() => usersPageMode(connection.value?.kind, capabilities.value))
const pending = computed(() => identityPending(connection.value?.kind, status.value))
const showMyKeys = computed(() => canManageOwnKeys(connection.value?.kind, isRoot.value, !!whoami.value))
const simulate = computed(() => canSimulate(connection.value?.kind, capabilities.value))

const users = useLoader(() => requireAdmin().listUsers(), { immediate: false, initial: [] })
// Policies feed the create dialog; listing them may be denied — then the select is empty.
const policies = useLoader(() => requireAdmin().listPolicies().catch(() => []), { immediate: false, initial: [] })

watch([mode, id], ([m]) => {
  if (m === 'manage') {
    void users.refresh()
    void policies.refresh()
  }
}, { immediate: true })

const search = ref('')
const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  const list = [...(users.data.value ?? [])].sort((a, b) => a.name.localeCompare(b.name))
  if (!q) return list
  return list.filter(u =>
    u.name.toLowerCase().includes(q)
    || (u.description ?? '').toLowerCase().includes(q)
    || (u.policies ?? []).some(p => p.toLowerCase().includes(q))
    || (u.keys ?? []).some(k => k.accessKey.toLowerCase().includes(q))
  )
})
const userNames = computed(() => (users.data.value ?? []).map(u => u.name))
const policyNames = computed(() => (policies.data.value ?? []).map(p => p.name).sort())

const columns: TableColumn<User>[] = [
  { accessorKey: 'name', header: 'Name' },
  { accessorKey: 'disabled', header: 'Status' },
  { accessorKey: 'policies', header: 'Policies' },
  { id: 'keys', header: 'Access keys' },
  { accessorKey: 'createdAt', header: 'Created' },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, 'Actions') }
]

async function openCreate() {
  const res = await overlay.create(CreateUserModal, { destroyOnClose: true }).open({ policies: policyNames.value, existing: userNames.value })
  if (!res) return
  useToast().add({ title: `User ${res.user.name} created`, color: 'success', icon: 'i-lucide-circle-check' })
  void users.refresh()
  if (res.key && connection.value) {
    await overlay.create(NewAccessKeyModal, { destroyOnClose: true }).open({
      accessKey: res.key,
      user: res.user.name,
      endpoint: connection.value.endpoint,
      region: connection.value.region
    })
  }
}

const busy = ref<string | null>(null)

const setDisabled = useAsyncAction(async (u: User, disabled: boolean) => {
  busy.value = u.name
  try {
    await requireAdmin().updateUser(u.name, { disabled })
    await users.refresh()
  } finally {
    busy.value = null
  }
}, { successMessage: (u, disabled) => `User ${u.name} ${disabled ? 'disabled' : 'enabled'}`, errorTitle: 'Could not update user' })

const removeUser = useAsyncAction(async (u: User) => {
  busy.value = u.name
  try {
    await requireAdmin().deleteUser(u.name)
    await users.refresh()
  } finally {
    busy.value = null
  }
}, { successMessage: u => `User ${u.name} deleted`, errorTitle: 'Could not delete user' })

async function toggle(u: User) {
  if (!u.disabled) {
    const self = whoami.value?.user === u.name
    const ok = await confirm({
      title: `Disable ${u.name}?`,
      description: self
        ? 'This is your own user: this session will stop working immediately.'
        : 'Requests signed with any of this user\'s keys will be rejected until the user is enabled again.',
      confirmLabel: 'Disable',
      color: 'warning',
      icon: 'i-lucide-user-x'
    })
    if (!ok) return
  }
  await setDisabled.run(u, !u.disabled)
}

async function remove(u: User) {
  const keys = u.keys?.length ?? 0
  const ok = await confirm({
    title: `Delete user ${u.name}?`,
    description: `The user and ${keys} access key${keys === 1 ? '' : 's'} will be deleted. Applications using them stop working immediately.`,
    confirmLabel: 'Delete user',
    requireText: u.name,
    icon: 'i-lucide-trash-2'
  })
  if (ok) await removeUser.run(u)
}

function rowActions(u: User): DropdownMenuItem[][] {
  return [
    [{ label: 'Open', icon: 'i-lucide-user-cog', to: appRoutes.user(id.value, u.name) }],
    [
      u.disabled
        ? { label: 'Enable', icon: 'i-lucide-user-check', onSelect: () => void toggle(u) }
        : { label: 'Disable', icon: 'i-lucide-user-x', onSelect: () => void toggle(u) }
    ],
    [{ label: 'Delete', icon: 'i-lucide-trash-2', color: 'error', onSelect: () => void remove(u) }]
  ]
}

function activeKeys(u: User): number {
  return (u.keys ?? []).filter(k => k.status === 'Active').length
}
</script>

<template>
  <div class="space-y-6">
    <CommonPageHeader
      :title="mode === 'self' ? 'My access keys' : 'Users'"
      icon="i-lucide-users"
      :description="mode === 'self' ? 'Manage the access keys of your own IAM user.' : 'IAM users, their policies and access keys.'"
    >
      <template v-if="mode === 'manage' && !pending" #actions>
        <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" aria-label="Refresh users" :loading="users.loading.value" @click="users.refresh" />
        <UButton icon="i-lucide-user-plus" label="Create user" @click="openCreate" />
      </template>
    </CommonPageHeader>

    <div v-if="pending" class="space-y-3" aria-busy="true">
      <USkeleton class="h-10 w-full" />
      <USkeleton class="h-40 w-full" />
    </div>
    <CommonEmptyState
      v-else-if="mode === 'unavailable'"
      icon="i-lucide-server-cog"
      title="Not available for this server"
      description="IAM users are managed through the gos3 admin API. Generic S3 services manage identities in their own console."
    />

    <template v-else-if="mode === 'self'">
      <div v-if="whoamiLoading && !whoami" class="space-y-2">
        <USkeleton class="h-10 w-full" />
        <USkeleton class="h-24 w-full" />
      </div>
      <CommonEmptyState
        v-else-if="isRoot || !whoami"
        icon="i-lucide-key-round"
        title="No self-service keys"
        :description="isRoot ? 'The root credentials come from the server configuration and have no IAM access keys.' : 'Your identity could not be determined.'"
      />
      <AdminMyAccessKeys v-else />
    </template>

    <template v-else>
      <UCard :ui="{ body: 'p-0 sm:p-0' }">
        <template #header>
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h2 class="font-semibold text-highlighted">
              {{ formatNumber(users.data.value?.length ?? 0) }} user{{ users.data.value?.length === 1 ? '' : 's' }}
            </h2>
            <UInput
              v-model="search"
              icon="i-lucide-search"
              placeholder="Filter by name, policy or key"
              aria-label="Filter users"
              class="w-full sm:w-72"
              size="sm"
            />
          </div>
        </template>
        <div v-if="users.error.value" class="p-4">
          <CommonErrorAlert :error="users.error.value" title="Could not list users" retryable @retry="users.refresh" />
        </div>
        <UTable v-else :data="filtered" :columns="columns" :loading="users.loading.value && !users.data.value?.length">
          <template #name-cell="{ row }">
            <NuxtLink :to="appRoutes.user(id, row.original.name)" class="rounded font-medium text-highlighted hover:text-primary focus-visible:outline-2 focus-visible:outline-primary">
              {{ row.original.name }}
            </NuxtLink>
            <p v-if="row.original.description" class="max-w-xs truncate text-xs text-muted">
              {{ row.original.description }}
            </p>
          </template>
          <template #disabled-cell="{ row }">
            <UBadge
              :label="row.original.disabled ? 'Disabled' : 'Enabled'"
              :color="row.original.disabled ? 'neutral' : 'success'"
              variant="subtle"
              size="sm"
            />
          </template>
          <template #policies-cell="{ row }">
            <div class="flex max-w-sm flex-wrap gap-1">
              <UBadge
                v-for="p in row.original.policies ?? []"
                :key="p"
                :label="p"
                color="neutral"
                variant="outline"
                size="sm"
              />
              <span v-if="!row.original.policies?.length" class="text-xs text-dimmed">none</span>
            </div>
          </template>
          <template #keys-cell="{ row }">
            <span class="tabular-nums">{{ row.original.keys?.length ?? 0 }}</span>
            <span v-if="(row.original.keys?.length ?? 0) !== activeKeys(row.original)" class="ml-1 text-xs text-muted">({{ activeKeys(row.original) }} active)</span>
          </template>
          <template #createdAt-cell="{ row }">
            <CommonRelativeTime :value="row.original.createdAt" />
          </template>
          <template #actions-cell="{ row }">
            <div class="flex justify-end">
              <UDropdownMenu :items="rowActions(row.original)" :content="{ align: 'end' }">
                <UButton
                  icon="i-lucide-ellipsis-vertical"
                  color="neutral"
                  variant="ghost"
                  size="sm"
                  :loading="busy === row.original.name"
                  :aria-label="`Actions for ${row.original.name}`"
                />
              </UDropdownMenu>
            </div>
          </template>
          <template #empty>
            <div class="p-4">
              <CommonEmptyState
                v-if="search"
                compact
                icon="i-lucide-search-x"
                title="No matching users"
                :description="`Nothing matches “${search}”.`"
              />
              <CommonEmptyState v-else compact icon="i-lucide-users" title="No users yet" description="Create IAM users to give applications and people their own credentials.">
                <UButton size="sm" icon="i-lucide-user-plus" label="Create user" @click="openCreate" />
              </CommonEmptyState>
            </div>
          </template>
        </UTable>
      </UCard>

      <AdminMyAccessKeys v-if="showMyKeys" />

      <AdminSimulatorPanel v-if="simulate" :users="userNames" />
    </template>
  </div>
</template>
