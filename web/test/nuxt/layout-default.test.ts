import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import DefaultLayout from '~/layouts/default.vue'
import { useConnectionsStore } from '~/stores/connections'
import { clearClientCache } from '~/utils/clients'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(DefaultLayout, null, { default: () => h('div', 'Page content') }) }) })

describe('default dashboard layout', () => {
  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    clearClientCache()
    const store = useConnectionsStore()
    store.resetState()
    store.add({ id: 'nas', name: 'NAS', endpoint: 'http://nas.example:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 's3' })
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('offline'))))
  })
  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('renders global navigation and bucket-settings breadcrumbs', async () => {
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/buckets/photos/settings' })
    expect(page.text()).toContain('Page content')
    expect(page.text()).toContain('NAS')
    expect(page.text()).toContain('Buckets')
    expect(page.text()).toContain('photos')
    expect(page.text()).toContain('Settings')
    page.unmount()
  })

  it('labels backing routes as Storage and shows a global Discover link', async () => {
    const page = await mountSuspended(WithProviders, { route: '/servers/nas/backings/archive' })
    expect(page.text()).toContain('Storage')
    expect(page.text()).toContain('archive')
    expect(page.text()).toContain('Discover')
    page.unmount()
  })
})
