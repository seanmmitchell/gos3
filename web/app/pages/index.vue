<script setup lang="ts">
/**
 * Dashboard: a live status card per configured server (gos3: /_gos3/info +
 * whoami; generic S3: ListBuckets), bucket / object statistics when allowed,
 * quick links, and a first-run guide (embedded vs standalone, discovery).
 */
import type { Connection } from '~/types/connection'
import { useConnectionsStore } from '~/stores/connections'
import { defaultFirst, useDefaultServer } from '~/components/servers/-default'

useHead({ title: 'Dashboard' })

const store = useConnectionsStore()
const toast = useToast()
const config = useRuntimeConfig()
const { defaultId, setDefault } = useDefaultServer()
const local = useDiscoveryLocal()

const connections = computed(() => defaultFirst(store.sorted, defaultId.value))
const embedded = computed(() => store.byId(EMBEDDED_CONNECTION_ID))
const embeddedNeedsSignIn = computed(() => !!embedded.value && !hasCredentials(embedded.value))
const appName = computed(() => (config.public.appName as string) || 'gos3')

const summary = computed(() => {
  let online = 0
  let problems = 0
  for (const c of store.connections) {
    const s = store.statusOf(c.id).state
    if (s === 'online') online++
    else if (s === 'offline' || s === 'unauthorized' || s === 'error') problems++
  }
  return { online, problems, total: store.connections.length }
})

const refreshing = ref(false)
async function refreshAll() {
  refreshing.value = true
  try {
    await store.refreshAll()
  } finally {
    refreshing.value = false
  }
}

onMounted(() => {
  if (!local.embedded.value) void local.detect()
})

const formOpen = ref(false)
const editing = ref<Connection | undefined>()
const importOpen = ref(false)

function openAdd() {
  editing.value = undefined
  formOpen.value = true
}

function openEdit(c: Connection) {
  editing.value = c
  formOpen.value = true
}

function makeDefault(c: Connection) {
  setDefault(c.id)
  store.setActive(c.id)
  toast.add({ title: `${c.name} is now the default server`, icon: 'i-lucide-star', color: 'success' })
}
</script>

<template>
  <div>
    <CommonPageHeader
      title="Dashboard"
      icon="i-lucide-layout-grid"
      :description="summary.total ? `${summary.total} server${summary.total === 1 ? '' : 's'} · ${summary.online} online${summary.problems ? ` · ${summary.problems} with problems` : ''}` : `Welcome to the ${appName} console.`"
    >
      <template v-if="summary.total" #actions>
        <UButton icon="i-lucide-plus" label="Add server" @click="openAdd" />
        <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" label="Refresh" :loading="refreshing" @click="refreshAll" />
      </template>
    </CommonPageHeader>

    <div class="space-y-6">
      <!-- Embedded UI whose hosting server has no credentials yet. -->
      <UCard v-if="embedded && embeddedNeedsSignIn" class="border-primary/40 bg-primary/5" :ui="{ body: 'flex flex-col gap-4 sm:flex-row sm:items-center' }">
        <div class="flex size-12 shrink-0 items-center justify-center rounded-full bg-primary/15 text-primary" aria-hidden="true">
          <UIcon name="i-lucide-log-in" class="size-6" />
        </div>
        <div class="min-w-0 flex-1">
          <h2 class="text-base font-semibold text-highlighted">
            Sign in to {{ embedded.name }}
          </h2>
          <p class="text-sm text-muted">
            This console is served by the gos3 server at <code class="font-mono">{{ embedded.endpoint }}</code>. Sign in with an access key to browse buckets and manage the server.
          </p>
        </div>
        <UButton :to="appRoutes.connect(embedded.id)" size="lg" icon="i-lucide-log-in" label="Sign in" class="shrink-0" />
      </UCard>

      <!-- First run -->
      <template v-if="!summary.total">
        <UCard :ui="{ body: 'space-y-5' }">
          <div>
            <h2 class="text-lg font-semibold text-highlighted">
              Get started
            </h2>
            <p class="mt-1 text-sm text-muted">
              Manage one or more S3 servers from this browser. Requests are signed locally with your access keys; nothing is sent to a third party.
            </p>
          </div>
          <div class="grid gap-4 md:grid-cols-2">
            <div class="rounded-lg border border-default p-4">
              <p class="flex items-center gap-2 font-medium text-highlighted">
                <UIcon name="i-lucide-box" class="size-4 text-primary" aria-hidden="true" /> Embedded
              </p>
              <p class="mt-1 text-sm text-muted">
                Every gos3 binary serves this console at <code class="font-mono">/_gos3/ui/</code> and offers its own server as the default connection — just sign in.
              </p>
            </div>
            <div class="rounded-lg border border-default p-4">
              <p class="flex items-center gap-2 font-medium text-highlighted">
                <UIcon name="i-lucide-monitor" class="size-4 text-primary" aria-hidden="true" /> Standalone
              </p>
              <p class="mt-1 text-sm text-muted">
                Run the console on its own to manage many servers — gos3 or any S3-compatible service. The servers must allow this origin in their CORS settings.
                <template v-if="local.available.value">
                  This UI server can also discover gos3 servers on its local network.
                </template>
              </p>
            </div>
          </div>
          <div class="flex flex-wrap gap-2">
            <UButton icon="i-lucide-plus" label="Add server" @click="openAdd" />
            <UButton color="neutral" variant="outline" icon="i-lucide-scan-search" label="Discover servers" :to="appRoutes.discover()" />
            <UButton color="neutral" variant="ghost" icon="i-lucide-upload" label="Import JSON" @click="importOpen = true" />
          </div>
        </UCard>
      </template>

      <section v-else aria-label="Servers">
        <ul class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
          <li v-for="c in connections" :key="c.id">
            <ServersStatusCard :connection="c" :is-default="defaultId === c.id" @edit="openEdit" @set-default="makeDefault" />
          </li>
          <li>
            <button
              type="button"
              class="flex h-full min-h-48 w-full flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-default p-6 text-muted transition-colors hover:border-primary hover:text-primary focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
              @click="openAdd"
            >
              <UIcon name="i-lucide-plus" class="size-6" aria-hidden="true" />
              <span class="font-medium">Add server</span>
              <span class="text-xs">gos3 or any S3-compatible service</span>
            </button>
          </li>
        </ul>
        <p class="mt-4 text-sm text-muted">
          Looking for more gos3 servers?
          <NuxtLink :to="appRoutes.discover()" class="font-medium text-primary hover:underline">
            Discover servers on your network
          </NuxtLink>
          or
          <NuxtLink :to="appRoutes.servers()" class="font-medium text-primary hover:underline">
            manage connections
          </NuxtLink>.
        </p>
      </section>
    </div>

    <ServersFormModal v-model:open="formOpen" :connection="editing" />
    <ServersImportModal v-model:open="importOpen" />
  </div>
</template>
