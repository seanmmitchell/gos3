<script setup lang="ts">
import type { Backing, ContainerInfo } from '~/types/admin'
import { remoteAccess } from './-capabilities'
import { canBeDefault, containerName, containerRemoveRefusal, containerRoleLabel, containerStateMeta, remoteLocationUri } from './-containers'
import AddContainerModal from './AddContainerModal.vue'

/**
 * The containers of an s3 backing's wrapped buckets (Backing.account): the
 * home (the backing's shared container) and added ones, with their
 * location, state and wrapped-bucket count. With admin:ManageContainers
 * (capability manageContainers) containers can be added, made the default of
 * new wrapped buckets and removed when empty (gos3 forgets them; their remote
 * data stays).
 */
const props = defineProps<{ backing: Backing }>()
const emit = defineEmits<{ changed: [] }>()

const { connection, requireAdmin } = useConnection()
const { capabilities } = useWhoAmI()
const overlay = useOverlay()
const confirm = useConfirm()
const access = computed(() => remoteAccess(connection.value?.kind, capabilities.value))
const canManage = computed(() => access.value.containers && (access.value.create || access.value.adopt))
const captionId = useId()

const containers = computed(() => props.backing.account?.containers ?? [])
const acting = ref<string | null>(null)

const setDefault = useAsyncAction(
  (id: string) => requireAdmin().setDefaultContainer(props.backing.name, id),
  { successMessage: id => `New wrapped buckets now go to ${containerName(containers.value.find(c => c.id === id) ?? { id })}`, errorTitle: 'Could not change the default container' }
)

async function onSetDefault(c: ContainerInfo) {
  acting.value = c.id
  try {
    await setDefault.run(c.id)
    if (!setDefault.error.value) emit('changed')
  } finally {
    acting.value = null
  }
}

const remove = useAsyncAction(
  (id: string) => requireAdmin().removeContainer(props.backing.name, id),
  { successMessage: id => `Container ${id} removed`, errorTitle: 'Could not remove the container' }
)

async function onRemove(c: ContainerInfo) {
  const ok = await confirm({
    title: `Remove container ${containerName(c)}?`,
    description: `gos3 forgets the empty container. Nothing remote is deleted: ${remoteLocationUri(c.location) || 'its location'} stays as it is.`,
    confirmLabel: 'Remove container',
    color: 'warning',
    icon: 'i-lucide-package-minus'
  })
  if (!ok) return
  acting.value = c.id
  try {
    await remove.run(c.id)
    if (!remove.error.value) emit('changed')
  } finally {
    acting.value = null
  }
}

async function onAdd() {
  const id = await overlay.create(AddContainerModal, { destroyOnClose: true }).open({ backing: props.backing.name, access: access.value })
  if (id !== null && id !== undefined) emit('changed')
}

function rowMenu(c: ContainerInfo) {
  const refusal = containerRemoveRefusal(c)
  return [[
    { label: 'Make default', icon: 'i-lucide-star', disabled: !canBeDefault(c), onSelect: () => void onSetDefault(c) },
    { label: 'Remove…', description: refusal || undefined, icon: 'i-lucide-package-minus', color: 'error' as const, disabled: !!refusal, onSelect: () => void onRemove(c) }
  ]]
}
</script>

<template>
  <UCard :ui="{ body: 'p-0 sm:p-0' }">
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 :id="captionId" class="font-semibold text-highlighted">
            Containers
          </h2>
          <p class="text-sm text-muted">
            Wrapped buckets are stored in gos3's format inside a container; many can share one.
          </p>
        </div>
        <UButton v-if="canManage" icon="i-lucide-package-plus" label="Add container…" @click="onAdd" />
      </div>
    </template>

    <div v-if="!containers.length" class="p-4 sm:px-6">
      <CommonEmptyState
        compact
        icon="i-lucide-package-x"
        title="No container"
        :description="`${backing.name} has no home and no container, so it cannot hold wrapped buckets (only native ones).${canManage ? ' Add a container: a new remote bucket, or an empty prefix of an existing one.' : ' An administrator can add one.'}`"
      />
    </div>

    <div v-else class="overflow-x-auto">
      <table class="w-full text-sm" :aria-labelledby="captionId">
        <thead class="border-b border-default text-left text-xs text-muted">
          <tr>
            <th scope="col" class="px-4 py-2 font-medium">
              Container
            </th>
            <th scope="col" class="px-4 py-2 font-medium">
              Location
            </th>
            <th scope="col" class="px-4 py-2 font-medium">
              State
            </th>
            <th scope="col" class="px-4 py-2 text-right font-medium">
              Buckets
            </th>
            <th scope="col" class="px-4 py-2 font-medium">
              Added
            </th>
            <th v-if="access.containers" scope="col" class="w-0 px-4 py-2">
              <span class="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody class="divide-y divide-default">
          <tr v-for="c in containers" :key="c.id" :data-testid="`container-${c.id}`">
            <th scope="row" class="px-4 py-2.5 text-left font-normal">
              <div class="flex flex-wrap items-center gap-1.5">
                <span class="font-medium text-highlighted">{{ containerName(c) }}</span>
                <UBadge v-if="c.default" label="Default" color="primary" variant="subtle" size="sm" icon="i-lucide-star" />
                <UBadge :label="containerRoleLabel(c.role)" color="neutral" variant="outline" size="sm" />
              </div>
              <span v-if="c.label" class="block font-mono text-xs text-muted">{{ c.id }}</span>
            </th>
            <td class="px-4 py-2.5">
              <span class="font-mono text-xs">{{ remoteLocationUri(c.location) }}</span>
              <span v-if="c.region" class="block text-xs text-muted">{{ c.region }}</span>
            </td>
            <td class="px-4 py-2.5">
              <div class="space-y-1">
                <UTooltip :text="containerStateMeta(c.state).description" :disabled="!containerStateMeta(c.state).description">
                  <UBadge
                    :label="containerStateMeta(c.state).label"
                    :color="containerStateMeta(c.state).color"
                    :icon="containerStateMeta(c.state).icon"
                    variant="subtle"
                    size="sm"
                    :ui="{ leadingIcon: c.state === 'loading' ? 'animate-spin' : '' }"
                  />
                </UTooltip>
                <p v-if="c.error" class="max-w-xs text-xs text-error" data-testid="container-error">
                  {{ c.error }}
                </p>
              </div>
            </td>
            <td class="px-4 py-2.5 text-right tabular-nums">
              {{ formatNumber(c.buckets) }}
            </td>
            <td class="px-4 py-2.5 text-sm">
              <CommonRelativeTime :value="c.added" class="text-muted" />
            </td>
            <td v-if="access.containers" class="px-4 py-2.5 text-right">
              <UDropdownMenu :items="rowMenu(c)" :content="{ align: 'end' }">
                <UButton
                  icon="i-lucide-ellipsis-vertical"
                  color="neutral"
                  variant="ghost"
                  size="sm"
                  :loading="acting === c.id"
                  :aria-label="`Actions for container ${containerName(c)}`"
                />
              </UDropdownMenu>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </UCard>
</template>
