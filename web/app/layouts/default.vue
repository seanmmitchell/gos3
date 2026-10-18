<script setup lang="ts">
import type { BreadcrumbItem, NavigationMenuItem } from '@nuxt/ui'
import { useConnectionsStore } from '~/stores/connections'
import { canViewJobs, usersPageMode } from '~/components/admin/-capabilities'

const store = useConnectionsStore()
const route = useRoute()
const { connection } = useConnection()
const { capabilities } = useWhoAmI()
const breadcrumbOverride = useBreadcrumbsState()

const sidebarOpen = ref(false)
const collapsed = ref(false)

const serverItems = computed<NavigationMenuItem[]>(() => {
  const c = connection.value
  if (!c) return []
  const caps = capabilities.value
  const items: NavigationMenuItem[] = [
    { label: 'Overview', icon: 'i-lucide-layout-dashboard', to: appRoutes.server(c.id), exact: true },
    { label: 'Buckets', icon: 'i-lucide-database', to: appRoutes.buckets(c.id) }
  ]
  if (c.kind === 'gos3') {
    if (usersPageMode(c.kind, caps) === 'manage') items.push({ label: 'Users', icon: 'i-lucide-users', to: appRoutes.users(c.id) })
    if (caps.managePolicies) items.push({ label: 'Policies', icon: 'i-lucide-shield-check', to: appRoutes.policies(c.id) })
    if (caps.manageConfig) items.push({ label: 'Settings', icon: 'i-lucide-settings', to: appRoutes.settings(c.id) })
    // Backing pages (/servers/:id/backings/:name) belong to Storage.
    if (caps.viewStorage) items.push({ label: 'Storage', icon: 'i-lucide-hard-drive', to: appRoutes.storage(c.id), active: route.path.startsWith(`${appRoutes.server(c.id)}/backings/`) || undefined })
    if (canViewJobs(c.kind, caps)) items.push({ label: 'Jobs', icon: 'i-lucide-list-checks', to: appRoutes.jobs(c.id) })
    if (caps.discovery) items.push({ label: 'Discovery', icon: 'i-lucide-radar', to: appRoutes.discovery(c.id) })
  }
  return items
})

const globalItems = computed<NavigationMenuItem[]>(() => [
  { label: 'Servers', icon: 'i-lucide-server', to: appRoutes.servers(), exact: true },
  { label: 'Discover', icon: 'i-lucide-scan-search', to: appRoutes.discover() }
])

const SECTION_LABELS: Record<string, string> = {
  buckets: 'Buckets',
  users: 'Users',
  policies: 'Policies',
  settings: 'Settings',
  storage: 'Storage',
  jobs: 'Jobs',
  discovery: 'Discovery'
}

/** Breadcrumbs derived from the route (pages may override via useBreadcrumbs). */
const autoCrumbs = computed<BreadcrumbItem[]>(() => {
  const path = route.path
  const items: BreadcrumbItem[] = []
  if (path === '/') return [{ label: 'Home', icon: 'i-lucide-house' }]
  if (path === '/discover') return [{ label: 'Discover', icon: 'i-lucide-scan-search' }]
  if (!path.startsWith('/servers')) return []
  items.push({ label: 'Servers', icon: 'i-lucide-server', to: appRoutes.servers() })
  const id = routeParam(route.params.id)
  if (!id) return items
  items.push({ label: store.byId(id)?.name ?? id, to: appRoutes.server(id) })
  const rest = path.slice(appRoutes.server(id).length).split('/').filter(Boolean).map(s => decodeURIComponent(s))
  const section = rest[0]
  if (!section) return items
  if (section === 'backings') {
    // Backing pages live under the Storage page.
    items.push({ label: 'Storage', to: appRoutes.storage(id) })
    if (rest[1]) items.push({ label: rest[1] })
    return items
  }
  items.push({ label: SECTION_LABELS[section] ?? section, to: `${appRoutes.server(id)}/${section}` })
  if (section === 'buckets' && rest[1]) {
    const bucket = rest[1]
    items.push({ label: bucket, to: appRoutes.bucket(id, bucket) })
    if (rest[2] === 'settings') {
      items.push({ label: 'Settings' })
    } else if (rest[2] === 'objects') {
      const segs = rest.slice(3)
      let prefix = ''
      segs.forEach((s, i) => {
        if (i < segs.length - 1) {
          prefix += `${s}/`
          items.push({ label: s, to: appRoutes.bucket(id, bucket, prefix) })
        } else {
          items.push({ label: s })
        }
      })
    } else {
      const prefix = typeof route.query.prefix === 'string' ? route.query.prefix : ''
      for (const c of prefixBreadcrumbs(prefix)) items.push({ label: c.label, to: appRoutes.bucket(id, bucket, c.prefix) })
    }
  } else if (rest[1]) {
    items.push({ label: rest[1] })
  }
  return items
})

const breadcrumbs = computed<BreadcrumbItem[]>(() => {
  const list = breadcrumbOverride.value ?? autoCrumbs.value
  // The current page is not a link.
  return list.map((it, i) => (i === list.length - 1 ? { ...it, to: undefined } : it))
})

// Refresh the active server status when it changes.
watch(() => connection.value?.id, (id) => {
  if (id) void store.ensureStatus(id)
}, { immediate: true })
</script>

<template>
  <UDashboardGroup storage="local" storage-key="gos3-dashboard" unit="rem">
    <UDashboardSidebar
      id="main"
      v-model:open="sidebarOpen"
      v-model:collapsed="collapsed"
      collapsible
      resizable
      :min-size="12"
      :default-size="16"
      :max-size="32"
      :collapsed-size="4.25"
      class="peer bg-elevated/25"
      :ui="{ footer: 'lg:border-t lg:border-default' }"
    >
      <!-- Nuxt UI's handle is an invisible 0px line; this one shows itself on
           hover and while dragging, and has a wider grab area. -->
      <template #resize-handle="{ onMouseDown, onTouchStart, onDoubleClick, ui }">
        <UDashboardResizeHandle
          aria-controls="gos3-dashboard-sidebar-main"
          aria-label="Resize sidebar"
          title="Drag to resize · double-click to reset"
          data-slot="handle"
          :class="ui.handle({ class: 'z-10 before:-left-2 before:-right-2 after:absolute after:inset-y-0 after:-left-px after:w-0.5 after:transition-colors hover:after:bg-primary/50 peer-data-[dragging=true]:after:bg-primary' })"
          @mousedown="onMouseDown"
          @touchstart="onTouchStart"
          @dblclick="onDoubleClick"
        />
      </template>

      <template #header="{ collapsed: isCollapsed }">
        <LayoutAppLogo :collapsed="isCollapsed" :class="isCollapsed ? 'mx-auto' : ''" />
      </template>

      <template #default="{ collapsed: isCollapsed }">
        <LayoutServerSwitcher :collapsed="isCollapsed" />

        <!-- UNavigationMenu renders its own <nav>; aria-label falls through to it. -->
        <UNavigationMenu
          v-if="serverItems.length"
          aria-label="Server navigation"
          :collapsed="isCollapsed"
          :items="serverItems"
          orientation="vertical"
          tooltip
        />

        <UNavigationMenu
          aria-label="Global navigation"
          class="mt-auto"
          :collapsed="isCollapsed"
          :items="globalItems"
          orientation="vertical"
          tooltip
        />
      </template>

      <template #footer="{ collapsed: isCollapsed }">
        <LayoutIdentityMenu :collapsed="isCollapsed" />
      </template>
    </UDashboardSidebar>

    <UDashboardPanel id="content">
      <template #header>
        <UDashboardNavbar :ui="{ left: 'min-w-0' }">
          <template #left>
            <UDashboardSidebarCollapse class="hidden lg:inline-flex" />
            <UBreadcrumb v-if="breadcrumbs.length" :items="breadcrumbs" class="min-w-0" :ui="{ list: 'flex-wrap' }" />
          </template>
          <template #right>
            <slot name="header-actions" />
            <UColorModeButton />
          </template>
        </UDashboardNavbar>
      </template>

      <template #body>
        <main id="main-content" class="mx-auto w-full max-w-7xl">
          <slot />
        </main>
      </template>
    </UDashboardPanel>
  </UDashboardGroup>
</template>
