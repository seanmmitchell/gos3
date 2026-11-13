import { mountSuspended } from '@nuxt/test-utils/runtime'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, nextTick } from 'vue'
import { UApp } from '#components'
import ServersPage from '~/pages/servers/index.vue'
import { useConnectionsStore } from '~/stores/connections'

describe('server connections page', () => {
  const PageWithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(ServersPage) }) })
  beforeEach(() => {
    useConnectionsStore().resetState()
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('offline'))))
  })
  afterEach(() => vi.unstubAllGlobals())

  it('shows the empty state and opens the add form', async () => {
    const page = await mountSuspended(PageWithProviders)
    expect(page.text()).toContain('No servers yet')
    const add = page.findAll('button').find(button => button.text().includes('Add server'))
    expect(add).toBeDefined()
    await add!.trigger('click')
    expect(page.findComponent({ name: 'ServersFormModal' }).exists()).toBe(true)
  })

  it('filters a saved connection and opens its edit form', async () => {
    const store = useConnectionsStore()
    store.add({ name: 'Alpha', endpoint: 'http://alpha.example:9000' })
    store.add({ name: 'Beta', endpoint: 'http://beta.example:9000' })
    const page = await mountSuspended(PageWithProviders)
    expect(page.text()).toContain('Alpha')
    expect(page.text()).toContain('Beta')
    await page.find('input[aria-label="Filter servers"]').setValue('alpha')
    await nextTick()
    expect(page.text()).toContain('Alpha')
    expect(page.text()).not.toContain('Beta')
    await page.find('button[aria-label="Edit Alpha"]').trigger('click')
    expect(page.findComponent({ name: 'ServersFormModal' }).props('connection').name).toBe('Alpha')
  })
})
