import { mountSuspended } from '@nuxt/test-utils/runtime'
import { UApp } from '#components'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import DiscoverPage from '~/pages/discover.vue'
import { useConnectionsStore } from '~/stores/connections'

const WithProviders = defineComponent({ setup: () => () => h(UApp, null, { default: () => h(DiscoverPage) }) })

describe('discover page', () => {
  beforeEach(() => {
    useConnectionsStore().resetState()
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('offline'))))
  })
  afterEach(() => vi.unstubAllGlobals())

  it('explains discovery sources and validates an empty direct probe', async () => {
    const page = await mountSuspended(WithProviders)
    expect(page.text()).toContain('Connected gos3 servers')
    expect(page.text()).toContain('Probe URLs')
    await page.find('form').trigger('submit')
    expect(page.text()).toContain('Enter at least one URL or host')
  })

  it('caps a browser probe at 64 hosts before making requests', async () => {
    const page = await mountSuspended(WithProviders)
    await page.find('textarea').setValue(Array.from({ length: 65 }, (_, i) => `host-${i}.example`).join('\n'))
    await page.find('form').trigger('submit')
    expect(page.text()).toContain('At most 64 entries at once')
  })
})
