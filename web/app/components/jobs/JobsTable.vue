<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'
import type { Job, JobAction } from '~/types/admin'
import { JOB_ACTIONS, describeJob, formatJobEnd, jobActions, jobKindMeta, jobStateMeta } from './-jobs'
import JobProgress from './JobProgress.vue'

/**
 * Table of background jobs: kind, source → target (linked to the buckets'
 * settings), state with phase and next run, progress (JobProgress), the
 * last error, and — with `canManage` — the actions the job's state allows
 * (pause, resume, cancel, run now, delete; cancel and delete ask first).
 * Actions call the admin API here and emit `changed` with the updated job
 * (or the deleted job's ID), so the Jobs page and the bucket overview share
 * the same behaviour. `compact` leaves out the error and time columns.
 */
const props = withDefaults(defineProps<{
  jobs: Job[]
  /** Connection ID, for links to the buckets. */
  serverId: string
  /** Offer the actions (capability updateJobs, admin:UpdateJob; the server checks both buckets). */
  canManage?: boolean
  compact?: boolean
  /** Accessible name of the table. */
  label?: string
}>(), { canManage: false, compact: false, label: 'Jobs' })

const emit = defineEmits<{
  /** A job changed (the updated job) or was deleted (null, with its ID). */
  changed: [job: Job | null, id: string]
}>()

const { requireAdmin } = useConnection()
const confirm = useConfirm()

const acting = ref<string | null>(null)

const run = useAsyncAction<[Job, JobAction | 'delete'], Job | null>(
  async (job, action) => {
    if (action === 'delete') {
      await requireAdmin().deleteJob(job.id)
      return null
    }
    return requireAdmin().jobAction(job.id, action)
  },
  {
    successMessage: (job, action) => `${jobKindMeta(job.kind).label} job ${JOB_ACTIONS[action].done}`,
    errorTitle: 'Could not change the job'
  }
)

async function onAction(job: Job, action: JobAction | 'delete') {
  const d = describeJob(job)
  if (action === 'cancel') {
    const ok = await confirm({
      title: `Cancel this ${d.kindLabel.toLowerCase()} job?`,
      description: `${d.title}. The job stops for good; what it already ${job.kind.startsWith('restic-') ? 'wrote' : 'copied'} stays.`,
      confirmLabel: 'Cancel job',
      cancelLabel: 'Keep running',
      color: 'warning',
      icon: 'i-lucide-circle-x'
    })
    if (!ok) return
  } else if (action === 'delete') {
    const ok = await confirm({
      title: 'Delete this job from the history?',
      description: `${d.title}. Only the job's record is deleted; the buckets are not touched.`,
      confirmLabel: 'Delete job',
      icon: 'i-lucide-trash-2'
    })
    if (!ok) return
  }
  acting.value = job.id
  try {
    const r = await run.run(job, action)
    if (!run.error.value) emit('changed', r ?? null, job.id)
  } finally {
    acting.value = null
  }
}

function menu(job: Job): DropdownMenuItem[][] {
  const actions = jobActions(job)
  const main = actions.filter(a => a !== 'cancel' && a !== 'delete')
  const danger = actions.filter(a => a === 'cancel' || a === 'delete')
  const item = (a: JobAction | 'delete'): DropdownMenuItem => ({
    label: JOB_ACTIONS[a].label,
    icon: JOB_ACTIONS[a].icon,
    color: a === 'cancel' || a === 'delete' ? 'error' : undefined,
    onSelect: () => void onAction(job, a)
  })
  return [main.map(item), danger.map(item)].filter(g => g.length)
}

const rows = computed(() => props.jobs.map(job => ({ job, d: describeJob(job), state: jobStateMeta(job.state), kind: jobKindMeta(job.kind) })))
</script>

<template>
  <div class="overflow-x-auto">
    <table class="w-full text-sm" :aria-label="label" data-testid="jobs-table">
      <thead class="border-b border-default text-left text-xs text-muted">
        <tr>
          <th scope="col" class="px-4 py-2 font-medium">
            Job
          </th>
          <th scope="col" class="px-4 py-2 font-medium">
            State
          </th>
          <th scope="col" class="px-4 py-2 font-medium">
            Progress
          </th>
          <th v-if="!compact" scope="col" class="px-4 py-2 font-medium">
            Last error
          </th>
          <th v-if="!compact" scope="col" class="px-4 py-2 font-medium">
            Created
          </th>
          <th v-if="canManage" scope="col" class="w-0 px-4 py-2">
            <span class="sr-only">Actions</span>
          </th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="r in rows" :key="r.job.id" :data-testid="`job-${r.job.id}`" :data-state="r.job.state">
          <th scope="row" class="px-4 py-2.5 text-left font-normal">
            <div class="flex items-start gap-2">
              <UIcon :name="r.kind.icon" class="mt-0.5 size-4 shrink-0 text-muted" aria-hidden="true" />
              <div class="min-w-0">
                <p class="flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
                  <span class="text-xs font-medium uppercase tracking-wide text-muted">{{ r.kind.label }}</span>
                  <template v-if="r.d.source && (r.job.kind === 'copy' || r.job.kind === 'mirror')">
                    <ULink :to="appRoutes.bucketSettings(serverId, r.d.source.bucket)" class="font-mono text-xs text-highlighted hover:underline">{{ formatJobEnd(r.d.source) }}</ULink>
                    <UIcon name="i-lucide-arrow-right" class="size-3.5 text-muted" aria-hidden="true" /><span class="sr-only">to</span>
                    <ULink v-if="r.d.target" :to="appRoutes.bucketSettings(serverId, r.d.target.bucket)" class="font-mono text-xs text-highlighted hover:underline">{{ formatJobEnd(r.d.target) }}</ULink>
                  </template>
                  <span v-else class="text-xs text-highlighted">{{ r.d.title }}</span>
                </p>
                <p v-if="r.job.createdBy && !compact" class="text-xs text-muted">
                  by {{ r.job.createdBy }}
                </p>
              </div>
            </div>
          </th>
          <td class="px-4 py-2.5">
            <div class="space-y-1">
              <UBadge
                :label="r.state.label"
                :color="r.state.color"
                :icon="r.state.icon"
                variant="subtle"
                size="sm"
                :title="r.state.description || undefined"
                :ui="{ leadingIcon: r.job.state === 'running' ? 'animate-spin' : '' }"
                data-testid="job-state"
              />
              <p v-if="r.job.phase && !['succeeded', 'canceled'].includes(r.job.state)" class="text-xs text-muted">
                {{ r.job.phase }}
              </p>
              <p v-if="r.job.state === 'waiting' && r.job.nextRunAt" class="text-xs text-muted">
                next run <CommonRelativeTime :value="r.job.nextRunAt" />
              </p>
              <p v-if="r.job.failures" class="text-xs text-warning">
                {{ r.job.failures }} failed run{{ r.job.failures === 1 ? '' : 's' }}
              </p>
              <p v-if="compact && r.job.lastError" class="line-clamp-2 max-w-xs text-xs break-words text-error" :title="r.job.lastError" data-testid="job-error">
                {{ r.job.lastError }}
              </p>
            </div>
          </td>
          <td class="px-4 py-2.5">
            <JobProgress :job="r.job" />
          </td>
          <td v-if="!compact" class="px-4 py-2.5">
            <p v-if="r.job.lastError" class="line-clamp-3 max-w-xs text-xs break-words text-error" :title="r.job.lastError" data-testid="job-error">
              {{ r.job.lastError }}
            </p>
            <span v-else class="text-muted">—</span>
          </td>
          <td v-if="!compact" class="px-4 py-2.5 text-sm whitespace-nowrap">
            <CommonRelativeTime :value="r.job.createdAt" />
          </td>
          <td v-if="canManage" class="px-4 py-2.5 text-right">
            <UDropdownMenu v-if="menu(r.job).length" :items="menu(r.job)" :content="{ align: 'end' }">
              <UButton
                icon="i-lucide-ellipsis-vertical"
                color="neutral"
                variant="ghost"
                size="sm"
                :loading="acting === r.job.id"
                :aria-label="`Actions for ${r.d.title}`"
              />
            </UDropdownMenu>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
