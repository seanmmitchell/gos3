<script setup lang="ts">
/** Discovery configuration and identity of a gos3 server (GET /discovery). */
import type { DiscoveryState } from '~/types/admin'

const props = defineProps<{ state: DiscoveryState }>()

const flags = computed(() => {
  const c = props.state.config
  return [
    { label: 'Enabled', on: props.state.enabled },
    { label: 'Advertise', on: c.advertise },
    { label: 'mDNS', on: c.mdns },
    { label: 'Beacon', on: c.beacon },
    { label: 'Scans allowed', on: c.allowScan }
  ]
})
</script>

<template>
  <div class="grid gap-4 lg:grid-cols-2">
    <UCard :ui="{ body: 'space-y-3' }">
      <template #header>
        <h2 class="text-sm font-semibold text-highlighted">
          Configuration
        </h2>
      </template>
      <ul class="flex flex-wrap gap-1.5" aria-label="Discovery mechanisms">
        <li v-for="f in flags" :key="f.label">
          <UBadge :label="f.label" :color="f.on ? 'success' : 'neutral'" :variant="f.on ? 'subtle' : 'outline'" :icon="f.on ? 'i-lucide-check' : 'i-lucide-x'" size="sm" />
          <!-- The state is otherwise only shown by color and icon. -->
          <span class="sr-only">{{ f.on ? ': on' : ': off' }}</span>
        </li>
      </ul>
      <dl class="grid grid-cols-1 gap-x-4 gap-y-2 text-sm sm:grid-cols-2">
        <div>
          <dt class="text-xs text-muted">
            Beacon group
          </dt>
          <dd class="font-mono text-xs leading-5">
            {{ state.config.beaconGroup || '—' }}
          </dd>
        </div>
        <div>
          <dt class="text-xs text-muted">
            Beacon interval
          </dt>
          <dd>{{ formatDuration(state.config.beaconInterval) }}</dd>
        </div>
        <div>
          <dt class="text-xs text-muted">
            Peer TTL
          </dt>
          <dd>{{ formatDuration(state.config.peerTtl) }}</dd>
        </div>
        <div>
          <dt class="text-xs text-muted">
            Max scan hosts
          </dt>
          <dd>{{ formatNumber(state.config.maxScanHosts) }}</dd>
        </div>
        <div class="sm:col-span-2">
          <dt class="text-xs text-muted">
            Seeds
          </dt>
          <dd class="font-mono text-xs leading-5">
            {{ state.config.seeds?.length ? state.config.seeds.join(', ') : '—' }}
          </dd>
        </div>
        <div class="sm:col-span-2">
          <dt class="text-xs text-muted">
            DNS SRV domains
          </dt>
          <dd class="font-mono text-xs leading-5">
            {{ state.config.dnsSrv?.length ? state.config.dnsSrv.join(', ') : '—' }}
          </dd>
        </div>
      </dl>
    </UCard>
    <UCard :ui="{ body: 'space-y-3' }">
      <template #header>
        <h2 class="text-sm font-semibold text-highlighted">
          This server
        </h2>
      </template>
      <dl class="grid grid-cols-1 gap-x-4 gap-y-2 text-sm sm:grid-cols-2">
        <div>
          <dt class="text-xs text-muted">
            Name
          </dt>
          <dd class="font-medium">
            {{ state.self.name }}
          </dd>
        </div>
        <div>
          <dt class="text-xs text-muted">
            Server ID
          </dt>
          <dd class="flex items-center gap-1 font-mono text-xs leading-5">
            {{ state.self.id }}
            <CommonCopyButton :value="state.self.id" aria-label="Copy server ID" />
          </dd>
        </div>
        <div>
          <dt class="text-xs text-muted">
            Version
          </dt>
          <dd>{{ state.self.version }}</dd>
        </div>
        <div>
          <dt class="text-xs text-muted">
            Region
          </dt>
          <dd class="font-mono text-xs leading-5">
            {{ state.self.region }}
          </dd>
        </div>
        <div class="sm:col-span-2">
          <dt class="text-xs text-muted">
            Advertised URLs
          </dt>
          <dd>
            <ul class="space-y-0.5">
              <li v-for="u in [state.self.endpoints?.s3, ...(state.self.endpoints?.all ?? [])].filter((x, i, a) => x && a.indexOf(x) === i)" :key="u" class="font-mono text-xs">
                {{ u }}
              </li>
            </ul>
          </dd>
        </div>
      </dl>
    </UCard>
  </div>
</template>
