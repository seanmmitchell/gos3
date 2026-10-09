<script setup lang="ts">
import type { OpStats } from '~/types/admin'
import { formatLatencyMs, nextSort, opErrorRate, opErrors, sortOps, type OpSortKey, type SortDir } from './-metrics'

/** Sortable per-operation metrics table (requests, errors, bytes, latency percentiles). */
const props = defineProps<{ ops: OpStats[] | null | undefined }>()

const sort = ref<{ key: OpSortKey, dir: SortDir }>({ key: 'requests', dir: 'desc' })
const rows = computed(() => sortOps(props.ops, sort.value.key, sort.value.dir))

const columns: { key: OpSortKey, label: string, numeric: boolean }[] = [
  { key: 'op', label: 'Operation', numeric: false },
  { key: 'requests', label: 'Requests', numeric: true },
  { key: 'errors', label: 'Errors', numeric: true },
  { key: 'errorRate', label: 'Error rate', numeric: true },
  { key: 'bytesIn', label: 'In', numeric: true },
  { key: 'bytesOut', label: 'Out', numeric: true },
  { key: 'avgLatencyMs', label: 'Avg', numeric: true },
  { key: 'p50LatencyMs', label: 'p50', numeric: true },
  { key: 'p99LatencyMs', label: 'p99', numeric: true }
]

function ariaSort(key: OpSortKey): 'ascending' | 'descending' | 'none' {
  if (sort.value.key !== key) return 'none'
  return sort.value.dir === 'asc' ? 'ascending' : 'descending'
}

function toggle(key: OpSortKey) {
  sort.value = nextSort(sort.value, key)
}
</script>

<template>
  <div class="overflow-x-auto rounded-md border border-default">
    <table class="w-full min-w-[720px] text-sm">
      <caption class="sr-only">
        Requests per operation. Column headers are buttons that sort the table.
      </caption>
      <thead class="bg-elevated/50">
        <tr>
          <th
            v-for="c in columns"
            :key="c.key"
            scope="col"
            :aria-sort="ariaSort(c.key)"
            class="px-3 py-2 font-medium text-muted"
            :class="c.numeric ? 'text-right' : 'text-left'"
          >
            <button
              type="button"
              class="inline-flex items-center gap-1 rounded hover:text-highlighted focus-visible:outline-2 focus-visible:outline-primary"
              :class="c.numeric ? 'flex-row-reverse' : ''"
              @click="toggle(c.key)"
            >
              {{ c.label }}
              <UIcon
                v-if="sort.key === c.key"
                :name="sort.dir === 'asc' ? 'i-lucide-arrow-up' : 'i-lucide-arrow-down'"
                class="size-3.5"
                aria-hidden="true"
              />
              <UIcon v-else name="i-lucide-arrow-up-down" class="size-3.5 opacity-40" aria-hidden="true" />
            </button>
          </th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="o in rows" :key="o.op" class="hover:bg-elevated/30">
          <th scope="row" class="px-3 py-2 text-left font-mono text-xs font-normal text-highlighted">
            {{ o.op }}
          </th>
          <td class="px-3 py-2 text-right tabular-nums">
            {{ formatNumber(o.requests) }}
          </td>
          <td class="px-3 py-2 text-right tabular-nums" :class="opErrors(o) ? 'text-error' : ''">
            <span :title="`4xx: ${formatNumber(o.errors4xx)}, 5xx: ${formatNumber(o.errors5xx)}`">{{ formatNumber(opErrors(o)) }}</span>
          </td>
          <td class="px-3 py-2 text-right tabular-nums">
            {{ formatPercent(opErrorRate(o), 1) }}
          </td>
          <td class="px-3 py-2 text-right tabular-nums">
            {{ formatBytes(o.bytesIn) }}
          </td>
          <td class="px-3 py-2 text-right tabular-nums">
            {{ formatBytes(o.bytesOut) }}
          </td>
          <td class="px-3 py-2 text-right tabular-nums">
            {{ formatLatencyMs(o.avgLatencyMs) }}
          </td>
          <td class="px-3 py-2 text-right tabular-nums">
            {{ formatLatencyMs(o.p50LatencyMs) }}
          </td>
          <td class="px-3 py-2 text-right tabular-nums">
            {{ formatLatencyMs(o.p99LatencyMs) }}
          </td>
        </tr>
        <tr v-if="!rows.length">
          <td :colspan="columns.length" class="px-3 py-6 text-center text-muted">
            No requests recorded yet.
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
