<script setup lang="ts">
import type { Decision } from '~/types/admin'
import { ADMIN_SERVICE_ARN, adminArn, adminConditionKeys, adminResourceType, buildSimulateRequest, isAdminAction, parseConditionLines, SIMULATOR_ACTIONS, validateAdminResource, type AdminResourceType } from './-policies'

/**
 * Authorization simulator (POST /authz/simulate): "may <user> do <action>
 * on <bucket>/<key>?" — evaluates identity policies, the bucket policy and
 * canned ACLs exactly like a real request. Admin actions take an admin
 * resource instead of a bucket/key (arn:gos3:admin:::user/<name>,
 * …:::policy/<name>, …:::bucket/<name>, …:::backing/<name>; empty = service
 * level).
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
const resource = ref('')
const conditionsText = ref('')
const showConditions = ref(false)
const result = ref<{ decision: Decision, request: string } | null>(null)
const errorMsg = ref<string>()

watch(() => props.fixedUser, (v) => {
  if (v) user.value = v
})
// Users usually arrive after mount: preselect the first one until the user picks.
const userTouched = ref(false)
watch(() => props.users, (list) => {
  if (!props.fixedUser && !userTouched.value && list.length && user.value === ANONYMOUS) user.value = list[0]!
})

const userItems = computed(() => [ANONYMOUS, ...props.users.filter(u => u !== ANONYMOUS)])
const actionItems = computed(() => [...SIMULATOR_ACTIONS])
const conditions = computed(() => parseConditionLines(conditionsText.value))
const adminAction = computed(() => isAdminAction(action.value))
const resourceType = computed(() => adminResourceType(action.value))
const RESOURCE_EXAMPLES: Record<AdminResourceType, string> = { user: 'alice', policy: 'readonly', bucket: 'photos', backing: 'cloud' }
const resourcePlaceholder = computed(() => (resourceType.value ? adminArn(resourceType.value, RESOURCE_EXAMPLES[resourceType.value]) : ADMIN_SERVICE_ARN))
const resourceHelp = computed(() => resourceType.value
  ? `Empty = service level. ${action.value.trim()} is evaluated against ${adminArn(resourceType.value, '<name>')}.`
  : 'Empty = service level (this action has no per-target resource).')
const resourceError = computed(() => (adminAction.value ? validateAdminResource(resource.value) : undefined))
const conditionsHelp = computed(() => {
  const extra = adminAction.value ? adminConditionKeys(action.value) : []
  const base = 'One key=value per line, multiple values comma separated, e.g. aws:SourceIp=10.0.0.7'
  return extra.length ? `${base}. ${action.value.trim()} also sets ${extra.join(', ')}, e.g. ${extra[0]}=readonly,readwrite` : base
})

const sim = useAsyncAction(async () => {
  const u = props.fixedUser ?? (user.value === ANONYMOUS ? '' : user.value)
  const req = buildSimulateRequest({
    user: u,
    action: action.value,
    bucket: bucket.value,
    key: key.value,
    resource: resource.value,
    conditions: conditions.value.conditions
  })
  const decision = await requireAdmin().simulate(req)
  const target = req.resource ?? (req.bucket ? `${req.bucket}${req.key ? `/${req.key}` : ''}` : '*')
  result.value = { decision, request: `${u || 'anonymous'} → ${req.action} on ${target}` }
  return decision
  // Errors are shown inline (next to the inputs that caused them) rather than
  // as a toast that disappears, e.g. "bucket does not exist".
}, { toastErrors: false })

async function run() {
  errorMsg.value = undefined
  // Never leave the previous verdict next to inputs it was not computed for.
  result.value = null
  if (!action.value.trim()) {
    errorMsg.value = 'Choose an action'
    return
  }
  if (conditions.value.error) {
    errorMsg.value = conditions.value.error
    return
  }
  if (resourceError.value) return
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
          <USelectMenu v-model="user" :items="userItems" class="w-full" :search-input="{ placeholder: 'Filter users…' }" @update:model-value="userTouched = true" />
        </UFormField>
        <UFormField label="Action" name="action" :error="errorMsg === 'Choose an action' ? errorMsg : undefined" :class="fixedUser ? 'sm:col-span-2' : ''">
          <USelectMenu
            v-model="action"
            :items="actionItems"
            class="w-full"
            create-item
            :search-input="{ placeholder: 'Search or type an action…' }"
            @create="onCreateAction"
          />
        </UFormField>
        <UFormField v-if="adminAction" label="Admin resource" name="resource" :help="resourceHelp" :error="resourceError" class="sm:col-span-2">
          <UInput v-model="resource" class="w-full font-mono" :placeholder="resourcePlaceholder" autocomplete="off" spellcheck="false" />
        </UFormField>
        <template v-else>
          <UFormField label="Bucket" name="bucket" help="Empty = no specific bucket.">
            <UInput v-model="bucket" class="w-full" placeholder="photos" autocomplete="off" />
          </UFormField>
          <UFormField label="Object key" name="key">
            <UInput v-model="key" class="w-full" placeholder="2024/cat.jpg" autocomplete="off" />
          </UFormField>
        </template>
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
            :help="conditionsHelp"
            :error="conditions.error"
          >
            <UTextarea v-model="conditionsText" :rows="3" class="w-full font-mono" placeholder="aws:SecureTransport=true" />
          </UFormField>
        </template>
      </UCollapsible>
      <div class="flex justify-end">
        <UButton type="submit" icon="i-lucide-play" label="Simulate" :loading="sim.loading.value" />
      </div>
    </form>
    <CommonErrorAlert v-if="sim.error.value" class="mt-4" :error="sim.error.value" title="Simulation failed" />
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
