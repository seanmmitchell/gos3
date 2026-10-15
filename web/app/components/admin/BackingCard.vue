<script setup lang="ts">
import type { Backing } from '~/types/admin'

/**
 * One storage backing on the server Storage page: name, kind, path (location
 * and kind description for s3), default
 * badge, health (with the last error), last check, bucket / replica counts
 * and disk usage (fs backings).
 */
const props = defineProps<{ backing: Backing }>()

const headingId = useId()
const counts = computed(() => {
  const b = props.backing
  return `${formatNumber(b.buckets)} bucket${b.buckets === 1 ? '' : 's'} · ${formatNumber(b.replicas)} replica${b.replicas === 1 ? '' : 's'}`
})
</script>

<template>
  <UCard :aria-labelledby="headingId" :ui="{ root: backing.healthy ? '' : 'ring-error/40', body: 'space-y-3' }" data-testid="backing-card">
    <template #header>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div class="flex min-w-0 items-center gap-2">
          <UTooltip :text="backing.lastError || (backing.healthy ? 'Healthy' : 'Unhealthy')">
            <span tabindex="0" class="inline-flex rounded focus-visible:outline-2 focus-visible:outline-primary">
              <CommonStatusDot :state="backing.healthy ? 'healthy' : 'unhealthy'" size="md" />
            </span>
          </UTooltip>
          <h2 :id="headingId" class="truncate font-semibold text-highlighted">
            {{ backing.name }}
          </h2>
          <UBadge v-if="backing.default" label="Default" color="primary" variant="subtle" size="sm" icon="i-lucide-star" />
        </div>
        <UBadge
          :label="backing.mode ? `${backing.kind} · ${backing.mode}` : backing.kind"
          :title="backingKindInfo(backing).label"
          color="neutral"
          variant="outline"
          size="sm"
          class="font-mono"
        />
      </div>
    </template>

    <template v-if="backing.kind === 's3'">
      <p v-if="backing.location" class="truncate font-mono text-xs text-muted" :title="backing.location" data-testid="backing-location">
        {{ backing.location }}
      </p>
      <p class="text-xs text-muted">
        {{ backingKindInfo(backing).description }}
      </p>
    </template>
    <p v-else-if="backing.path" class="truncate font-mono text-xs text-muted" :title="backing.path">
      {{ backing.path }}
    </p>
    <p v-else class="text-xs text-muted">
      In memory (lost on restart)
    </p>

    <UAlert
      v-if="!backing.healthy"
      color="error"
      variant="subtle"
      icon="i-lucide-unplug"
      title="Unhealthy"
      :description="backing.lastError || 'The last health check or operation failed.'"
    />

    <dl class="grid grid-cols-2 gap-2 text-sm">
      <div>
        <dt class="text-xs text-muted">
          Contents
        </dt>
        <dd class="tabular-nums text-highlighted">
          {{ counts }}
        </dd>
      </div>
      <div>
        <dt class="text-xs text-muted">
          Checked
        </dt>
        <dd>
          <CommonRelativeTime :value="backing.checkedAt" />
        </dd>
      </div>
    </dl>

    <AdminDiskUsage v-if="backing.disk" :disk="backing.disk" :label="`Disk usage of ${backing.name}`" />
  </UCard>
</template>
