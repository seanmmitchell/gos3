<script setup lang="ts">
import type { Backing, BackingChoices } from '~/types/admin'

/**
 * Storage backings new buckets can be created on (WhoAmI.backings), with
 * the default marked. Identities with admin:ListBackings also see each
 * remote backing's endpoint, bucket and prefix (GET /backings).
 *
 *   <AdminBackingsList :backings="whoami.backings" :can-list="can('listBackings')" />
 */
const props = defineProps<{
  backings: BackingChoices
  /** admin:ListBackings: load endpoint details. */
  canList?: boolean
}>()

const { requireAdmin } = useConnection()

const details = useLoader(async () => (props.canList ? (await requireAdmin().listBackings()).backings ?? [] : []), {
  watch: [() => props.canList]
})

const rows = computed(() => {
  const byName = new Map<string, Backing>((details.data.value ?? []).map(b => [b.name, b]))
  const def = defaultBacking(props.backings)
  return props.backings.available.map((b) => {
    const d = byName.get(b.name)
    const where = d?.endpoint ? [d.endpoint, d.bucket, d.prefix].filter(Boolean).join(' · ') : ''
    return { ...b, info: backingModeInfo(b.mode), isDefault: b.name === def, where }
  })
})
</script>

<template>
  <ul class="space-y-1.5 text-sm">
    <li v-for="r in rows" :key="r.name" class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
      <UIcon :name="r.info.icon" class="size-4 shrink-0 text-muted" aria-hidden="true" />
      <span class="font-mono text-xs">{{ r.name }}</span>
      <UBadge :label="r.info.label" color="neutral" variant="subtle" size="sm" :title="r.info.description" />
      <UBadge v-if="r.isDefault" label="Default" color="primary" variant="subtle" size="sm" />
      <code v-if="r.where" class="w-full min-w-0 truncate ps-6 font-mono text-xs text-muted" :title="r.where">{{ r.where }}</code>
    </li>
  </ul>
</template>
