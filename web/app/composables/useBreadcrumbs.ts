import type { BreadcrumbItem } from '@nuxt/ui'

/**
 * Breadcrumbs shown in the layout header. The default layout derives them
 * from the route; pages can override (e.g. with object-key segments):
 *
 *   useBreadcrumbs(() => [{ label: 'Buckets', to: ... }, { label: bucket }])
 *
 * The override is cleared automatically when the page unmounts.
 */
export function useBreadcrumbsState() {
  return useState<BreadcrumbItem[] | null>('gos3-breadcrumbs', () => null)
}

export function useBreadcrumbs(items: MaybeRefOrGetter<BreadcrumbItem[]>) {
  const state = useBreadcrumbsState()
  watchEffect(() => {
    state.value = toValue(items)
  })
  onBeforeUnmount(() => {
    state.value = null
  })
}
