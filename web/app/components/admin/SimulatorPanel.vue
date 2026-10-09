<script setup lang="ts">
import type { Decision } from '~/types/admin'
import { parseConditionLines, SIMULATOR_ACTIONS } from './-policies'

/**
 * Authorization simulator (POST /authz/simulate): "may <user> do <action>
 * on <bucket>/<key>?" — evaluates identity policies, the bucket policy and
 * canned ACLs exactly like a real request.
 *
 *   <AdminSimulatorPanel :users="['alice', 'bob']" />
 *   <AdminSimulatorPanel fixed-user="alice" />
 */
const props = withDefaults(defineProps<{
  /** Users offered in the user picker. */
  users?: string[]
  /** Simulate for this user only (hides the picker). */
  fixedUser?: string
  title?: string
  description?: string
}>(), {
  users: () => [],
  fixedUser: undefined,
  title: 'Effective permissions',
  description: 'Check whether a request would be allowed, including bucket policies and ACLs.'
})

const { requireAdmin } = useConnection()

const ANONYMOUS = '(anonymous)'
const user = ref(props.fixedUser ?? props.users[0] ?? ANONYMOUS)
const action = ref('s3:GetObject')
const bucket = ref('')
const key = ref('')
const conditionsText = ref('')
const showConditions = ref(false)
const result = ref<{ decision: Decision, request: string } | null>(null)
const errorMsg = ref<string>()

watch(() => props.fixedUser, (v) => {
  if (v) user.value = v
})

const userItems = computed(() => [ANONYMOUS, ...props.users.filter(u => u !== ANONYMOUS)])
const actionItems = computed(() => [...SIMULATOR_ACTIONS])
const conditions = computed(() => parseConditionLines(conditionsText.value))
const ids = { action: useId(), bucket: useId(), key: useId(), cond: useId() }

const sim = useAsyncAction(async () => {
  const u = props.fixedUser ?? (user.value === ANONYMOUS ? '' : user.value)
  const req = {
    user: u,
    action: action.value.trim(),
    bucket: bucket.value.trim() || undefined,
    key: key.value.trim() || undefined,
    conditions: Object.keys(conditions.value.conditions).length ? conditions.value.conditions : undefined
  }
  const decision = await requireAdmin().simulate(req)
  const target = req.bucket ? `${req.bucket}${req.key ? `/${req.key}` : ''}` : '*'
  result.value = { decision, request: `${u || 'anonymous'} → ${req.action} on ${target}` }
  return decision
}, { errorTitle: 'Simulation failed' })

async function run() {
  errorMsg.value = undefined
  if (!action.value.trim()) {
    errorMsg.value = 'Choose an action'
    return
  }
  if (conditions.value.error) {
    errorMsg.value = conditions.value.error
    return
  }
  await sim.run()
}

function onCreateAction(item: string) {
  action.value = item
}
</script>

<template>
  <UCard>
    <template #header>
      <div class="flex items-start gap-2">
        <UIcon name="i-lucide-flask-conical" class="mt-0.5 size-5 text-primary" aria-hidden="true" />
        <div>
          <h2 class="font-semibold text-highlighted">
            {{ title }}
          </h2>
          <p class="text-sm text-muted">
            {{ description }}
          </p>
        </div>
      </div>
    </template>
    <form class="space-y-3" novalidate @submit.prevent="run">
      <div class="grid gap-3 sm:grid-cols-2">
        <UFormField v-if="!fixedUser" label="User" name="user">
          <USelectMenu v-model="user" :items="userItems" class="w-full" :search-input="{ placeholder: 'Filter users…' }" />
        </UFormField>
        <UFormField label="Action" name="action" :error="errorMsg === 'Choose an action' ? errorMsg : undefined" :class="fixedUser ? 'sm:col-span-2' : ''">
          <USelectMenu
            :id="ids.action"
            v-model="action"
            :items="actionItems"
            class="w-full"
            create-item
            :search-input="{ placeholder: 'Search or type an action…' }"
            @create="onCreateAction"
          />
        </UFormField>
        <UFormField label="Bucket" name="bucket" help="Empty = no specific bucket.">
          <UInput :id="ids.bucket" v-model="bucket" class="w-full" placeholder="photos" autocomplete="off" />
        </UFormField>
        <UFormField label="Object key" name="key">
          <UInput :id="ids.key" v-model="key" class="w-full" placeholder="2024/cat.jpg" autocomplete="off" />
        </UFormField>
      </div>
      <UCollapsible v-model:open="showConditions">
        <UButton
          color="neutral"
          variant="link"
          size="xs"
          :icon="showConditions ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'"
          label="Condition context"
          class="px-0"
        />
        <template #content>
          <UFormField
            class="pt-2"
            label="Conditions"
            name="conditions"
            help="One key=value per line, multiple values comma separated, e.g. aws:SourceIp=10.0.0.7"
            :error="conditions.error"
          >
            <UTextarea :id="ids.cond" v-model="conditionsText" :rows="3" class="w-full font-mono" placeholder="aws:SecureTransport=true" />
          </UFormField>
        </template>
      </UCollapsible>
      <div class="flex justify-end">
        <UButton type="submit" icon="i-lucide-play" label="Simulate" :loading="sim.loading.value" />
      </div>
    </form>
    <div v-if="result" class="mt-4" aria-live="polite">
      <UAlert
        :color="result.decision.allowed ? 'success' : 'error'"
        variant="subtle"
        :icon="result.decision.allowed ? 'i-lucide-circle-check' : 'i-lucide-ban'"
        :title="result.decision.allowed ? 'Allowed' : result.decision.explicitDeny ? 'Explicitly denied' : 'Denied (no matching allow)'"
      >
        <template #description>
          <p class="font-mono text-xs">
            {{ result.request }}
          </p>
          <p v-if="result.decision.reason" class="mt-1">
            {{ result.decision.reason }}
          </p>
        </template>
      </UAlert>
    </div>
  </UCard>
</template>
