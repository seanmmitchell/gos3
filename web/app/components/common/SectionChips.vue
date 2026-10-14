<script setup lang="ts" generic="T extends string">
/**
 * Row of chips under a page header that switches between the sections of a
 * long settings page, so each section is shown on its own instead of one
 * long scrolling page. Sticks to the top of the scrolling panel and scrolls
 * horizontally when the chips do not fit. The negative top / side offsets
 * cancel UDashboardPanel's body padding (p-4 sm:p-6), which sticky
 * positioning would otherwise leave as a gap above the bar.
 *
 * It is a tab list for assistive technology (role tablist / tab,
 * aria-selected, arrow keys, Home / End); the page renders the selected
 * section in an element with role="tabpanel".
 *
 *   <CommonSectionChips v-model="section" :items="items" label="Settings sections" panel-id="settings-panel" />
 *   <div id="settings-panel" role="tabpanel" aria-label="General">…</div>
 */
export interface SectionChip<V extends string = string> {
  value: V
  label: string
  icon?: string
  /** Problems in the section (e.g. invalid fields); shown as a red count when > 0. */
  issues?: number
  /** The section has unsaved changes (a dot next to the label). */
  changed?: boolean
}

const props = withDefaults(defineProps<{
  items: readonly SectionChip<T>[]
  /** Accessible name of the tab list, e.g. "Settings sections". */
  label: string
  /** id of the panel element the chips control (aria-controls). */
  panelId?: string
  /** Stick to the top of the scrolling panel (default true). */
  sticky?: boolean
}>(), { panelId: undefined, sticky: true })

const model = defineModel<T>({ required: true })

const list = useTemplateRef<HTMLElement>('list')

function tabs(): HTMLElement[] {
  return Array.from(list.value?.querySelectorAll<HTMLElement>('[role="tab"]') ?? [])
}

function select(i: number) {
  const item = props.items[i]
  if (!item) return
  model.value = item.value
  void nextTick(() => tabs()[i]?.focus())
}

function onKeydown(e: KeyboardEvent) {
  const n = props.items.length
  if (!n) return
  const cur = Math.max(0, props.items.findIndex(t => t.value === model.value))
  const next = e.key === 'ArrowRight'
    ? (cur + 1) % n
    : e.key === 'ArrowLeft'
      ? (cur - 1 + n) % n
      : e.key === 'Home'
        ? 0
        : e.key === 'End' ? n - 1 : -1
  if (next < 0) return
  e.preventDefault()
  select(next)
}

// Keep the selected chip visible when the row scrolls horizontally (also
// on load, e.g. ?tab= naming the last section on a phone). Only the row
// scrolls: scrollIntoView could move the page too.
function revealSelected() {
  const row = list.value
  const el = row?.querySelector<HTMLElement>('[aria-selected="true"]')
  if (!row || !el) return
  const r = row.getBoundingClientRect()
  const c = el.getBoundingClientRect()
  if (c.left < r.left) row.scrollLeft -= r.left - c.left + 16
  else if (c.right > r.right) row.scrollLeft += c.right - r.right + 16
}
watch(model, revealSelected, { flush: 'post' })
onMounted(revealSelected)

/** Screen-reader text for the badge / dot, e.g. "(2 issues, unsaved changes)". */
function srStatus(item: SectionChip<T>): string {
  const parts: string[] = []
  if (item.issues) parts.push(`${item.issues} issue${item.issues === 1 ? '' : 's'}`)
  if (item.changed) parts.push('unsaved changes')
  return `(${parts.join(', ')})`
}
</script>

<template>
  <div
    :class="sticky ? 'sticky -top-4 z-10 -mx-4 border-b border-default bg-default/95 px-4 backdrop-blur sm:-top-6 sm:-mx-6 sm:px-6' : ''"
    data-testid="section-chips"
  >
    <div
      ref="list"
      role="tablist"
      :aria-label="label"
      aria-orientation="horizontal"
      class="-mx-1 flex gap-2 overflow-x-auto px-1 py-2.5 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
      @keydown="onKeydown"
    >
      <UButton
        v-for="(item, i) in items"
        :key="item.value"
        role="tab"
        :aria-selected="item.value === model"
        :aria-controls="panelId"
        :tabindex="item.value === model ? 0 : -1"
        :color="item.value === model ? 'primary' : 'neutral'"
        :variant="item.value === model ? 'soft' : 'outline'"
        :icon="item.icon"
        size="sm"
        class="shrink-0 rounded-full"
        :class="item.value === model ? 'ring ring-inset ring-primary/25' : ''"
        @click="select(i)"
      >
        {{ item.label }}
        <template v-if="item.issues || item.changed" #trailing>
          <UBadge v-if="item.issues" :label="String(item.issues)" color="error" variant="solid" size="xs" class="rounded-full" aria-hidden="true" />
          <span v-else class="size-2 rounded-full bg-warning" aria-hidden="true" />
          <span class="sr-only">{{ srStatus(item) }}</span>
        </template>
      </UButton>
    </div>
  </div>
</template>
