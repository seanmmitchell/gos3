<script setup lang="ts">
/**
 * Storage backings of one gos3 server (GET /backings, admin:ListBackings):
 * health, bucket / replica counts and disk usage per backing, auto-refreshed.
 * Bucket placement is managed per bucket (bucket settings → Placement).
 */
import { AdminError } from '~/utils/admin/client'
import { BACKINGS_REFRESH_MS, sortBackings } from '~/components/admin/-storage'

useHead({ title: 'Storage' })

const route = useRoute()
const id = computed(() => routeParam(route.params.id))
const { connection, isGos3, requireAdmin } = useConnection()
const { can } = useWhoAmI()

const backings = useLoader(() => requireAdmin().listBackings(), { immediate: false })
const state = computed(() => backings.data.value)
const list = computed(() => sortBackings(state.value?.backings))
const unhealthy = computed(() => list.value.filter(b => !b.healthy))
const accessDenied = computed(() => backings.error.value instanceof AdminError && backings.error.value.isAccessDenied)
const lastLoaded = ref<number | null>(null)

async function refresh() {
  const r = await backings.refresh()
  if (r) lastLoaded.value = Date.now()
}

// -- auto refresh ------------------------------------------------------------------

const autoRefresh = ref(true)
let timer: ReturnType<typeof setInterval> | undefined
function stopTimer() {
  if (timer) clearInterval(timer)
  timer = undefined
}
watch([autoRefresh, () => !!state.value], ([on, loaded]) => {
  stopTimer()
  if (!on || !loaded) return
  timer = setInterval(() => {
    if (document.visibilityState === 'visible' && !backings.loading.value) void refresh()
  }, BACKINGS_REFRESH_MS)
}, { immediate: true })
onBeforeUnmount(stopTimer)

// Reload when the route switches to another server.
watch([id, isGos3], ([, gos3]) => {
  backings.data.value = undefined
  lastLoaded.value = null
  if (gos3) void refresh()
}, { immediate: true })
</script>

<template>
  <div>
    <CommonPageHeader
      title="Storage"
      icon="i-lucide-hard-drive"
      :description="connection ? `Storage backings of ${connection.name}. Bucket placement and replication are set in each bucket's settings.` : undefined"
    >
      <template #actions>
        <USwitch v-if="isGos3 && state" v-model="autoRefresh" label="Auto-refresh" size="sm" />
        <UButton
          v-if="isGos3"
          color="neutral"
          variant="outline"
          icon="i-lucide-refresh-cw"
          label="Refresh"
          :loading="backings.loading.value"
          @click="refresh"
        />
        <UButton color="neutral" variant="ghost" icon="i-lucide-database" label="Buckets" :to="appRoutes.buckets(id)" />
      </template>
    </CommonPageHeader>

    <div class="space-y-6">
      <CommonEmptyState
        v-if="!isGos3"
        icon="i-lucide-server-off"
        title="Storage backings need a gos3 server"
        description="This connection is a generic S3 server."
      />

      <template v-else>
        <CommonEmptyState
          v-if="accessDenied"
          icon="i-lucide-lock"
          title="No access to storage"
          description="Your identity is not allowed to list this server's storage backings (admin:ListBackings). Ask an administrator for the diagnostics or admin policy."
        />
        <CommonErrorAlert v-else-if="backings.error.value && !state" :error="backings.error.value" title="Could not load the storage backings" retryable @retry="refresh" />

        <div v-if="!state && backings.loading.value" class="grid gap-4 md:grid-cols-2 xl:grid-cols-3" aria-busy="true" aria-label="Loading storage backings">
          <USkeleton v-for="i in 3" :key="i" class="h-48" />
        </div>

        <template v-if="state">
          <AdminBackingConfiguration v-if="can('manageConfig')" :key="id" :backings="list" @saved="refresh" />
          <CommonErrorAlert v-if="backings.error.value" :error="backings.error.value" title="Refresh failed; showing the last known state" retryable @retry="refresh" />
          <UAlert
            v-if="unhealthy.length"
            color="error"
            variant="subtle"
            icon="i-lucide-triangle-alert"
            :title="`${unhealthy.length} backing${unhealthy.length === 1 ? ' is' : 's are'} unhealthy`"
            :description="`${unhealthy.map(b => b.name).join(', ')}. Buckets whose primary copy is there cannot be read or written; replicas there are marked offline.`"
            data-testid="unhealthy-backings"
          />

          <div class="flex flex-wrap items-center gap-x-4 gap-y-2 text-sm text-muted">
            <span>New buckets are created on <span class="font-medium text-highlighted">{{ state.defaultBacking }}</span>.</span>
            <UBadge
              :label="state.autoFailover ? 'Automatic failover on' : 'Automatic failover off'"
              :color="state.autoFailover ? 'success' : 'neutral'"
              variant="subtle"
              size="sm"
              icon="i-lucide-repeat"
            />
            <span v-if="lastLoaded" class="ms-auto text-xs">
              Updated <CommonRelativeTime :value="lastLoaded" /><template v-if="autoRefresh">
                · refreshing every {{ BACKINGS_REFRESH_MS / 1000 }} s
              </template>
            </span>
          </div>

          <CommonEmptyState
            v-if="!list.length"
            icon="i-lucide-hard-drive"
            title="No storage backings"
            description="The server reported no backings."
          />
          <section v-else aria-label="Storage backings" class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
            <AdminBackingCard v-for="b in list" :key="b.name" :backing="b" />
          </section>

          <p v-if="can('manageStorage')" class="text-sm text-muted">
            To move a bucket to another backing or replicate it, open the bucket's settings and choose <span class="font-medium text-highlighted">Placement</span>.
          </p>
        </template>
      </template>
    </div>
  </div>
</template>
