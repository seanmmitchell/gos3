<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { AccessKey } from '~/types/admin'

/**
 * Access keys of a user (admin) or of the caller (self-service). The parent
 * performs the actions; `busy` holds the access key currently being changed.
 */
const props = withDefaults(defineProps<{
  keys: AccessKey[]
  loading?: boolean
  /** Show activate / deactivate (admin endpoint only). */
  canToggle?: boolean
  canDelete?: boolean
  /** Access key used by this session: cannot be deleted / deactivated here. */
  currentAccessKey?: string
  busy?: string | null
}>(), { loading: false, canToggle: true, canDelete: true, currentAccessKey: undefined, busy: null })

const emit = defineEmits<{
  toggle: [key: AccessKey]
  delete: [key: AccessKey]
}>()

const columns: TableColumn<AccessKey>[] = [
  { accessorKey: 'accessKey', header: 'Access key' },
  { accessorKey: 'description', header: 'Description' },
  { accessorKey: 'status', header: 'Status' },
  { accessorKey: 'createdAt', header: 'Created' },
  { id: 'actions', header: () => h('span', { class: 'sr-only' }, 'Actions') }
]

const isCurrent = (k: AccessKey) => !!props.currentAccessKey && k.accessKey === props.currentAccessKey
</script>

<template>
  <UTable :data="keys" :columns="columns" :loading="loading" class="rounded-md border border-default" :ui="{ td: 'py-2' }">
    <template #accessKey-cell="{ row }">
      <div class="flex items-center gap-1">
        <code class="font-mono text-xs text-highlighted">{{ row.original.accessKey }}</code>
        <CommonCopyButton :value="row.original.accessKey" :aria-label="`Copy access key ${row.original.accessKey}`" />
        <UBadge v-if="isCurrent(row.original)" label="this session" size="sm" color="info" variant="subtle" />
      </div>
    </template>
    <template #description-cell="{ row }">
      <span :class="row.original.description ? '' : 'text-dimmed'">{{ row.original.description || '—' }}</span>
    </template>
    <template #status-cell="{ row }">
      <UBadge
        :label="row.original.status"
        :color="row.original.status === 'Active' ? 'success' : 'neutral'"
        variant="subtle"
        size="sm"
      />
    </template>
    <template #createdAt-cell="{ row }">
      <CommonRelativeTime :value="row.original.createdAt" />
    </template>
    <template #actions-cell="{ row }">
      <div class="flex justify-end gap-1">
        <UButton
          v-if="canToggle"
          size="xs"
          color="neutral"
          variant="ghost"
          :icon="row.original.status === 'Active' ? 'i-lucide-pause' : 'i-lucide-play'"
          :label="row.original.status === 'Active' ? 'Deactivate' : 'Activate'"
          :loading="busy === row.original.accessKey"
          :disabled="isCurrent(row.original) && row.original.status === 'Active'"
          @click="emit('toggle', row.original)"
        />
        <UButton
          v-if="canDelete"
          size="xs"
          color="error"
          variant="ghost"
          icon="i-lucide-trash-2"
          :aria-label="`Delete access key ${row.original.accessKey}`"
          :disabled="isCurrent(row.original) || busy === row.original.accessKey"
          @click="emit('delete', row.original)"
        />
      </div>
    </template>
    <template #empty>
      <CommonEmptyState compact icon="i-lucide-key-round" title="No access keys" description="Create a key to let this identity sign S3 requests." />
    </template>
  </UTable>
</template>
