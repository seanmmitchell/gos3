<script setup lang="ts">
import type { NamedPolicy } from '~/types/admin'
import { AdminError } from '~/utils/admin/client'
import { canSimulate, identityPending, policiesPageMode } from '~/components/admin/-capabilities'

/**
 * Policy detail: built-in policies are read-only; custom policies are
 * edited with the JSON editor + templates and can be deleted (409 when
 * still attached to users). Includes the authorization simulator.
 */
const route = useRoute()
const router = useRouter()
const id = computed(() => routeParam(route.params.id))
const name = computed(() => routeParam(route.params.name))
const { connection, requireAdmin } = useConnection()
const { capabilities, status } = useWhoAmI()
const confirm = useConfirm()
const toast = useToast()

useHead({ title: () => `Policy ${name.value}` })

const mode = computed(() => policiesPageMode(connection.value?.kind, capabilities.value))
const pending = computed(() => identityPending(connection.value?.kind, status.value))
const simulate = computed(() => canSimulate(connection.value?.kind, capabilities.value))

const policy = useLoader(() => requireAdmin().getPolicy(name.value), { immediate: false })
const all = useLoader(() => requireAdmin().listPolicies().catch(() => []), { immediate: false, initial: [] })
const users = useLoader(() => (capabilities.value.manageUsers ? requireAdmin().listUsers() : Promise.resolve([])).catch(() => []), { immediate: false, initial: [] })

watch([mode, id, name], ([m]) => {
  if (m !== 'unavailable') {
    void policy.refresh()
    void all.refresh()
    void users.refresh()
  }
}, { immediate: true })

const notFound = computed(() => policy.error.value instanceof AdminError && policy.error.value.isNotFound)
const editorMode = computed<'edit' | 'view'>(() => (mode.value === 'manage' && policy.data.value && !policy.data.value.builtin ? 'edit' : 'view'))
const builtinNames = computed(() => (all.data.value ?? []).filter(p => p.builtin).map(p => p.name))
const attachedUsers = computed(() => (users.data.value ?? []).filter(u => (u.policies ?? []).includes(name.value)).map(u => u.name).sort())
const userNames = computed(() => (users.data.value ?? []).map(u => u.name))

const dirty = ref(false)

function onSaved(p: NamedPolicy) {
  policy.data.value = p
  dirty.value = false
}

const deleting = ref(false)
async function onDelete() {
  const attached = attachedUsers.value
  const ok = await confirm({
    title: `Delete policy ${name.value}?`,
    description: attached.length ? `It is attached to ${attached.join(', ')}. Detach it first.` : 'Users will lose the permissions granted by this policy.',
    confirmLabel: 'Delete policy',
    requireText: name.value,
    icon: 'i-lucide-trash-2'
  })
  if (!ok) return
  deleting.value = true
  try {
    await requireAdmin().deletePolicy(name.value)
    dirty.value = false
    toast.add({ title: `Policy ${name.value} deleted`, color: 'success', icon: 'i-lucide-circle-check' })
    await router.push(appRoutes.policies(id.value))
  } catch (e) {
    if (e instanceof AdminError && e.code === 'Conflict') {
      toast.add({ title: 'Policy is in use', description: `${e.message} Detach it from all users, then try again.`, color: 'warning', icon: 'i-lucide-link', duration: 8000 })
    } else {
      toast.add({ title: 'Could not delete policy', description: errorMessage(e), color: 'error', icon: 'i-lucide-circle-alert' })
    }
  } finally {
    deleting.value = false
  }
}

onBeforeRouteLeave(async () => {
  if (!dirty.value) return true
  return confirm({ title: 'Discard unsaved changes?', description: 'The policy has unsaved changes.', confirmLabel: 'Discard', color: 'warning' })
})
</script>

<template>
  <div class="space-y-6">
    <CommonPageHeader :title="name" icon="i-lucide-shield-check" :description="policy.data.value?.description || 'IAM identity policy'">
      <template v-if="policy.data.value" #badge>
        <UBadge
          :label="policy.data.value.builtin ? 'Built-in' : 'Custom'"
          :color="policy.data.value.builtin ? 'info' : 'neutral'"
          :icon="policy.data.value.builtin ? 'i-lucide-lock' : undefined"
          variant="subtle"
          size="sm"
        />
        <UBadge v-if="dirty" label="Unsaved changes" color="warning" variant="subtle" size="sm" />
      </template>
      <template #actions>
        <UButton color="neutral" variant="ghost" icon="i-lucide-arrow-left" label="All policies" :to="appRoutes.policies(id)" />
        <UButton
          v-if="editorMode === 'edit'"
          color="error"
          variant="soft"
          icon="i-lucide-trash-2"
          label="Delete"
          :loading="deleting"
          @click="onDelete"
        />
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
      description="IAM policies are only available on gos3 servers to identities with admin permissions."
    />
    <CommonEmptyState v-else-if="notFound" icon="i-lucide-shield-off" title="Policy not found" :description="`There is no policy named “${name}”.`">
      <UButton size="sm" color="neutral" variant="outline" icon="i-lucide-arrow-left" label="All policies" :to="appRoutes.policies(id)" />
    </CommonEmptyState>
    <CommonErrorAlert v-else-if="policy.error.value" :error="policy.error.value" title="Could not load policy" retryable @retry="policy.refresh" />
    <USkeleton v-else-if="!policy.data.value" class="h-96 w-full" />

    <template v-else>
      <div class="grid gap-6 xl:grid-cols-[minmax(0,1fr)_18rem]">
        <UCard>
          <AdminPolicyEditor :mode="editorMode" :policy="policy.data.value" :builtin-names="builtinNames" @saved="onSaved" @dirty="dirty = $event" />
        </UCard>
        <UCard>
          <template #header>
            <h2 class="font-semibold text-highlighted">
              Attached to
            </h2>
          </template>
          <p v-if="!capabilities.manageUsers" class="text-sm text-muted">
            Listing users requires admin:ListUsers.
          </p>
          <p v-else-if="!attachedUsers.length" class="text-sm text-muted">
            No users have this policy.
          </p>
          <ul v-else class="space-y-1 text-sm">
            <li v-for="u in attachedUsers" :key="u">
              <NuxtLink :to="appRoutes.user(id, u)" class="inline-flex items-center gap-1.5 rounded hover:text-primary focus-visible:outline-2 focus-visible:outline-primary">
                <UIcon name="i-lucide-user" class="size-3.5 text-muted" aria-hidden="true" />{{ u }}
              </NuxtLink>
            </li>
          </ul>
          <dl v-if="!policy.data.value.builtin" class="mt-4 space-y-2 border-t border-default pt-4 text-sm">
            <div>
              <dt class="text-muted">
                Created
              </dt>
              <dd><CommonRelativeTime :value="policy.data.value.createdAt" /></dd>
            </div>
            <div>
              <dt class="text-muted">
                Updated
              </dt>
              <dd><CommonRelativeTime :value="policy.data.value.updatedAt" /></dd>
            </div>
          </dl>
        </UCard>
      </div>
      <AdminSimulatorPanel v-if="simulate" :users="attachedUsers.length ? [...attachedUsers, ...userNames.filter(u => !attachedUsers.includes(u))] : userNames" />
    </template>
  </div>
</template>
