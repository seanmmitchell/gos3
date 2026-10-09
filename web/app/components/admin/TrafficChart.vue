<script setup lang="ts">
import type { MinuteBucket } from '~/types/admin'
import { areaPath, indexAtX, linePath, niceBytesMax, niceMax, xAt, yAt, type ChartBox, type SeriesColor, type TrafficSeries } from './-metrics'

/**
 * Per-minute traffic chart drawn with plain SVG (no chart dependency).
 *
 *   <AdminTrafficChart :points="minutes" :series="[{ key: 'requests', label: 'Requests', color: 'primary' }]" />
 *
 * Hover (or focus + arrow keys) shows the values of one minute.
 */
const props = withDefaults(defineProps<{
  points: MinuteBucket[]
  series: TrafficSeries[]
  /** Value formatting (axis + tooltip). */
  unit?: 'count' | 'bytes'
  title: string
  height?: number
}>(), { unit: 'count', height: 180 })

// Literal classes so Tailwind keeps them.
const COLOR_CLASS: Record<SeriesColor, string> = {
  primary: 'text-primary',
  error: 'text-error',
  info: 'text-info',
  warning: 'text-warning',
  success: 'text-success',
  secondary: 'text-secondary'
}
const DOT_CLASS: Record<SeriesColor, string> = {
  primary: 'bg-primary',
  error: 'bg-error',
  info: 'bg-info',
  warning: 'bg-warning',
  success: 'bg-success',
  secondary: 'bg-secondary'
}

const container = ref<HTMLElement | null>(null)
const width = ref(600)
let observer: ResizeObserver | undefined
onMounted(() => {
  if (!container.value) return
  width.value = container.value.clientWidth || 600
  if (typeof ResizeObserver !== 'undefined') {
    observer = new ResizeObserver(([entry]) => {
      if (entry) width.value = Math.max(200, Math.floor(entry.contentRect.width))
    })
    observer.observe(container.value)
  }
})
onBeforeUnmount(() => observer?.disconnect())

const box = computed<ChartBox>(() => ({ width: width.value, height: props.height, padLeft: 56, padRight: 8, padTop: 8, padBottom: 22 }))

const fmt = (v: number) => (props.unit === 'bytes' ? formatBytes(v) : formatCompact(v))
const fmtExact = (v: number) => (props.unit === 'bytes' ? formatBytes(v) : formatNumber(v))

const values = computed(() => props.series.map(s => props.points.map(p => p[s.key] ?? 0)))
const max = computed(() => {
  const m = Math.max(0, ...values.value.flat())
  return props.unit === 'bytes' ? niceBytesMax(m) : niceMax(m)
})
const ticks = computed(() => [0, 0.5, 1].map(f => ({ value: max.value * f, y: yAt(max.value * f, max.value, box.value) })))
const paths = computed(() => values.value.map(v => ({ line: linePath(v, max.value, box.value), area: areaPath(v, max.value, box.value) })))

const n = computed(() => props.points.length)
const xLabels = computed(() => {
  if (!n.value) return []
  const idx = [0, Math.floor((n.value - 1) / 2), n.value - 1]
  return idx.map((i, j) => ({
    x: xAt(i, n.value, box.value),
    anchor: j === 0 ? 'start' : j === 2 ? 'end' : 'middle',
    label: j === 2 ? 'now' : `-${n.value - 1 - i}m`
  }))
})

const hover = ref<number | null>(null)
function onMove(e: PointerEvent) {
  const svg = e.currentTarget as SVGSVGElement
  const rect = svg.getBoundingClientRect()
  hover.value = indexAtX(e.clientX - rect.left, n.value, box.value)
}
function onKey(e: KeyboardEvent) {
  if (!n.value) return
  const cur = hover.value ?? n.value - 1
  if (e.key === 'ArrowLeft') hover.value = Math.max(0, cur - 1)
  else if (e.key === 'ArrowRight') hover.value = Math.min(n.value - 1, cur + 1)
  else if (e.key === 'Home') hover.value = 0
  else if (e.key === 'End') hover.value = n.value - 1
  else if (e.key === 'Escape') hover.value = null
  else return
  e.preventDefault()
}

const hoverPoint = computed(() => (hover.value !== null ? props.points[hover.value] : undefined))
const hoverX = computed(() => (hover.value !== null ? xAt(hover.value, n.value, box.value) : 0))
const hoverText = computed(() => {
  const p = hoverPoint.value
  if (!p) return ''
  const time = formatDate(p.minute, { timeStyle: 'short' })
  return `${time}: ${props.series.map(s => `${s.label} ${fmtExact(p[s.key])}`).join(', ')}`
})

/** Screen reader summary: totals and peaks over the window. */
const summary = computed(() => {
  const parts = props.series.map((s, i) => {
    const v = values.value[i] ?? []
    const total = v.reduce((a, b) => a + b, 0)
    const peak = Math.max(0, ...v)
    return `${s.label}: ${fmtExact(total)} total, peak ${fmtExact(peak)} per minute`
  })
  return `${props.title}, last ${n.value} minutes. ${parts.join('; ')}.`
})
const tooltipLeft = computed(() => {
  const w = box.value.width
  return hoverX.value > w * 0.6 ? undefined : `${hoverX.value + 12}px`
})
const tooltipRight = computed(() => {
  const w = box.value.width
  return hoverX.value > w * 0.6 ? `${w - hoverX.value + 12}px` : undefined
})
</script>

<template>
  <figure class="space-y-2">
    <figcaption class="flex flex-wrap items-center justify-between gap-2">
      <span class="text-sm font-medium text-highlighted">{{ title }}</span>
      <span class="flex flex-wrap items-center gap-3 text-xs text-muted">
        <span v-for="s in series" :key="s.key" class="inline-flex items-center gap-1.5">
          <span class="size-2 rounded-full" :class="DOT_CLASS[s.color]" aria-hidden="true" />
          {{ s.label }}
        </span>
      </span>
    </figcaption>
    <div ref="container" class="relative w-full">
      <svg
        :width="box.width"
        :height="box.height"
        :viewBox="`0 0 ${box.width} ${box.height}`"
        class="block max-w-full touch-none rounded-md outline-none focus-visible:ring-2 focus-visible:ring-primary"
        role="img"
        :aria-label="summary"
        tabindex="0"
        @pointermove="onMove"
        @pointerleave="hover = null"
        @keydown="onKey"
        @blur="hover = null"
      >
        <g class="text-dimmed">
          <g v-for="t in ticks" :key="t.value">
            <line
              :x1="box.padLeft"
              :x2="box.width - box.padRight"
              :y1="t.y"
              :y2="t.y"
              stroke="currentColor"
              stroke-opacity="0.25"
              :stroke-dasharray="t.value ? '3 3' : undefined"
            />
            <text :x="box.padLeft - 6" :y="t.y" dy="0.32em" text-anchor="end" class="fill-current text-[10px] tabular-nums">{{ fmt(t.value) }}</text>
          </g>
          <text
            v-for="l in xLabels"
            :key="l.label"
            :x="l.x"
            :y="box.height - 6"
            :text-anchor="l.anchor"
            class="fill-current text-[10px]"
          >{{ l.label }}</text>
        </g>
        <g v-for="(s, i) in series" :key="s.key" :class="COLOR_CLASS[s.color]">
          <path :d="paths[i]?.area" fill="currentColor" fill-opacity="0.08" />
          <path :d="paths[i]?.line" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linejoin="round" stroke-linecap="round" />
        </g>
        <g v-if="hoverPoint">
          <line :x1="hoverX" :x2="hoverX" :y1="box.padTop" :y2="box.height - box.padBottom" class="text-muted" stroke="currentColor" stroke-opacity="0.5" />
          <circle
            v-for="s in series"
            :key="s.key"
            :cx="hoverX"
            :cy="yAt(hoverPoint[s.key], max, box)"
            r="3"
            fill="currentColor"
            :class="COLOR_CLASS[s.color]"
          />
        </g>
      </svg>
      <div
        v-if="hoverPoint"
        class="pointer-events-none absolute top-2 z-10 rounded-md border border-default bg-default/95 px-2 py-1.5 text-xs shadow-sm"
        :style="{ left: tooltipLeft, right: tooltipRight }"
        aria-hidden="true"
      >
        <div class="font-medium text-highlighted">
          {{ formatDate(hoverPoint.minute, { timeStyle: 'short' }) }}
        </div>
        <div v-for="s in series" :key="s.key" class="flex items-center gap-1.5 tabular-nums">
          <span class="size-2 rounded-full" :class="DOT_CLASS[s.color]" />
          {{ s.label }}: {{ fmtExact(hoverPoint[s.key]) }}
        </div>
      </div>
      <p class="sr-only" aria-live="polite">
        {{ hoverText }}
      </p>
    </div>
  </figure>
</template>
