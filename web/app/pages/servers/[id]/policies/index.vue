<script setup lang="ts">
import type { DropdownMenuItem, TableColumn } from '@nuxt/ui'
import type { NamedPolicy } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { canListUsers, canSimulate, identityPending, policiesPageMode } from '~/components/admin/-capabilities'
import CreatePolicyModal from '~/components/admin/CreatePolicyModal.vue'

/**
 * IAM policies: built-in (read-only) and custom policies, create with
 * templates, delete (409 when still attached), authorization simulator.
 */
useHead({ title: 'Policies' })

const route = useRoute()
const router = useRouter()
const id = computed(() => routeParam(route.params.id))
const { connection, requireAdmin } = useConnection()
const { capabilities, status } = useWhoAmI()
const overlay = useOverlay()
const confirm = useConfirm()
const toast = useToast()

const mode = computed(() => policiesPageMode(connection.value?.kind, capabilities.value))
const pending = computed(() => identityPending(connection.value?.kind, status.value))
const canManage = computed(() => mode.value === 'manage')
const simulate = computed(() => canSimulate(connection.value?.kind, capabilities.value))

const policies = useLoader(() => requireAdmin().listPolicies(), { immediate: false, initial: [] })
// Users (to show where policies are attached); optional.
const users = useLoader(() => (canListUsers(capabilities.value) ? requireAdmin().listUsers() : Promise.resolve([])).catch(() => []), { immediate: false, initial: [] })

watch([mode, id], ([m]) => {
  if (m !== 'unavailable') {
    void policies.refresh()
    void users.refresh()
  }
}, { immediate: true })

const attachedTo = computed(() => {
  const map = new Map<string, string[]>()
  for (const u of users.data.value ?? []) {
    for (const p of u.policies ?? []) map.set(p, [...(map.get(p) ?? []), u.name])
  }
  return map
})

const filter = ref<'all' | 'builtin' | 'custom'>('all')
const filterItems = [
  { label: 'All', value: 'all' },
  { label: 'Built-in', value: 'builtin' },
  { label: 'Custom', value: 'custom' }
]
const rows = computed(() => {
  const list = [...(policies.data.value ?? [])].sort((a, b) => Number(b.builtin) - Number(a.builtin) || a.name.localeCompare(b.name))
  if (filter.value === 'builtin') return list.filter(p => p.builtin)
  if (filter.value === 'custom') return list.filter(p => !p.builtin)
  return list
})
const builtinNames = computed(() => (policies.data.value ?? []).filter(p => p.builtin).map(p => p.name))
const allNames = computed(() => (policies.data.value ?? []).map(p => p.name))
const userNames = computed(() => (users.data.value ?? []).map(u => u.name))

const columns: TableColumn<NamedPolicy>[] = [
  { accessorKey: 'name', header: 'Name' },
  { accessorKey: 'builtin', header: 'Type' },
  { id: 'statements', header: 'Statements' },
  { id: 'attached', header: 'Attached to' },
  { accessorKey: 'updatedAt', header: 'Updated' },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, 'Actions') }
]

function statementCount(p: NamedPolicy): number {
  const st = p.document?.Statement as unknown
  return Array.isArray(st) ? st.length : st ? 1 : 0
}

async function openCreate() {
  const created = await overlay.create(CreatePolicyModal, { destroyOnClose: true }).open({ builtinNames: builtinNames.value, existingNames: allNames.value })
  if (!created) return
  await router.push(appRoutes.policy(id.value, created.name))
}

const deleting = ref<string | null>(null)
async function remove(p: NamedPolicy) {
  const users = attachedTo.value.get(p.name) ?? []
  const ok = await confirm({
    title: `Delete policy ${p.name}?`,
    description: users.length
      ? `It is attached to ${users.join(', ')}. Detach it from these users first.`
      : 'Users will lose the permissions granted by this policy.',
    confirmLabel: 'Delete policy',
    requireText: p.name,
    icon: 'i-lucide-trash-2'
  })
  if (!ok) return
  deleting.value = p.name
  try {
    await requireAdmin().deletePolicy(p.name)
    toast.add({ title: `Policy ${p.name} deleted`, color: 'success', icon: 'i-lucide-circle-check' })
    await policies.refresh()
  } catch (e) {
    if (e instanceof AdminError && e.code === 'Conflict') {
      toast.add({
        title: `Policy ${p.name} is in use`,
        description: `${e.message} Detach it from all users, then try again.`,
        color: 'warning',
        icon: 'i-lucide-link',
        duration: 8000
      })
    } else {
      toast.add({ title: 'Could not delete policy', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
    }
  } finally {
    deleting.value = null
  }
}

function rowActions(p: NamedPolicy): DropdownMenuItem[][] {
  const items: DropdownMenuItem[][] = [[{ label: p.builtin || !canManage.value ? 'View' : 'Edit', icon: p.builtin || !canManage.value ? 'i-lucide-eye' : 'i-lucide-pencil', to: appRoutes.policy(id.value, p.name) }]]
  if (!p.builtin && canManage.value) items.push([{ label: 'Delete', icon: 'i-lucide-trash-2', color: 'error', onSelect: () => void remove(p) }])
  return items
}
</script>

<template>
  <div class="space-y-6">
    <CommonPageHeader title="Policies" icon="i-lucide-shield-check" description="Identity policies grant users S3 and admin permissions.">
      <template v-if="mode !== 'unavailable'" #actions>
        <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" aria-label="Refresh policies" :loading="policies.loading.value" @click="policies.refresh" />
        <UButton v-if="canManage" icon="i-lucide-plus" label="Create policy" @click="openCreate" />
      </template>
    </CommonPageHeader>

    <div v-if="pending" class="space-y-3" aria-busy="true">
      <USkeleton class="h-10 w-full" />
      <USkeleton class="h-40 w-full" />
    </div>
    <CommonEmptyState
      v-else-if="mode === 'unavailable'"
      icon="i-lucide-lock"
      title="Not available"
      :description="connection?.kind === 'gos3' ? 'Your identity has no admin permissions on this server.' : 'IAM policies are managed through the gos3 admin API.'"
    />

    <template v-else>
      <UAlert
        v-if="mode === 'readonly'"
        color="neutral"
        variant="subtle"
        icon="i-lucide-eye"
        title="Read-only"
        description="Creating or changing policies requires the admin:PutPolicy permission."
      />
      <UCard :ui="{ body: 'p-0 sm:p-0' }">
        <template #header>
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h2 class="font-semibold text-highlighted">
              {{ formatNumber(policies.data.value?.length ?? 0) }} policies
            </h2>
            <UTabs v-model="filter" :items="filterItems" :content="false" size="xs" aria-label="Filter policies" />
          </div>
        </template>
        <div v-if="policies.error.value" class="p-4">
          <CommonErrorAlert :error="policies.error.value" title="Could not list policies" retryable @retry="policies.refresh" />
        </div>
        <UTable v-else :data="rows" :columns="columns" :loading="policies.loading.value && !policies.data.value?.length">
          <template #name-cell="{ row }">
            <NuxtLink :to="appRoutes.policy(id, row.original.name)" class="rounded font-medium text-highlighted hover:text-primary focus-visible:outline-2 focus-visible:outline-primary">
              {{ row.original.name }}
            </NuxtLink>
            <p v-if="row.original.description" class="max-w-md truncate text-xs text-muted">
              {{ row.original.description }}
            </p>
          </template>
          <template #builtin-cell="{ row }">
            <UBadge
              :label="row.original.builtin ? 'Built-in' : 'Custom'"
              :color="row.original.builtin ? 'info' : 'neutral'"
              :icon="row.original.builtin ? 'i-lucide-lock' : undefined"
              variant="subtle"
              size="sm"
            />
          </template>
          <template #statements-cell="{ row }">
            <span class="tabular-nums">{{ statementCount(row.original) }}</span>
          </template>
          <template #attached-cell="{ row }">
            <span v-if="!canListUsers(capabilities)" class="text-xs text-dimmed">—</span>
            <span v-else-if="!(attachedTo.get(row.original.name)?.length)" class="text-xs text-dimmed">No users</span>
            <span v-else class="text-sm" :title="attachedTo.get(row.original.name)?.join(', ')">
              {{ attachedTo.get(row.original.name)?.length }} user{{ attachedTo.get(row.original.name)?.length === 1 ? '' : 's' }}
            </span>
          </template>
          <template #updatedAt-cell="{ row }">
            <CommonRelativeTime v-if="!row.original.builtin" :value="row.original.updatedAt" />
            <span v-else class="text-xs text-dimmed">—</span>
          </template>
          <template #actions-cell="{ row }">
            <div class="flex justify-end">
              <UDropdownMenu :items="rowActions(row.original)" :content="{ align: 'end' }">
                <UButton
                  icon="i-lucide-ellipsis-vertical"
                  color="neutral"
                  variant="ghost"
                  size="sm"
                  :loading="deleting === row.original.name"
                  :aria-label="`Actions for ${row.original.name}`"
                />
              </UDropdownMenu>
            </div>
          </template>
          <template #empty>
            <div class="p-4">
              <CommonEmptyState compact icon="i-lucide-shield" :title="filter === 'custom' ? 'No custom policies' : 'No policies'" description="Custom policies let you grant exactly the permissions a user needs.">
                <UButton v-if="canManage" size="sm" icon="i-lucide-plus" label="Create policy" @click="openCreate" />
              </CommonEmptyState>
            </div>
          </template>
        </UTable>
      </UCard>

      <AdminSimulatorPanel v-if="simulate" :users="userNames" />
    </template>
  </div>
</template>
