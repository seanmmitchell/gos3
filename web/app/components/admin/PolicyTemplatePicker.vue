<script setup lang="ts">
import { IAM_POLICY_TEMPLATES, renderIamTemplate, templateParamErrors, type IamPolicyTemplate } from './-policies'

/**
 * Template chooser for custom identity policies. Emits `apply` with the
 * rendered policy JSON; the parent decides whether to replace its text.
 */
const emit = defineEmits<{ apply: [text: string, template: IamPolicyTemplate] }>()

const templateId = ref<IamPolicyTemplate['id']>('readonly-bucket')
const bucket = ref('')
const cidr = ref('')
const users = ref('')
const policies = ref('')
const touched = ref(false)

const items = IAM_POLICY_TEMPLATES.map(t => ({ label: t.label, value: t.id, description: t.description }))
const template = computed(() => IAM_POLICY_TEMPLATES.find(t => t.id === templateId.value)!)
const params = computed(() => ({ bucket: bucket.value.trim(), cidr: cidr.value.trim(), users: users.value.trim(), policies: policies.value.trim() }))
const errors = computed(() => templateParamErrors(template.value, params.value))

watch(templateId, () => (touched.value = false))

function apply() {
  touched.value = true
  if (Object.keys(errors.value).length) return
  emit('apply', renderIamTemplate(template.value.id, params.value), template.value)
  touched.value = false
}
</script>

<template>
  <div class="space-y-3 rounded-md border border-default bg-elevated/30 p-3">
    <div class="flex items-center gap-2 text-sm font-medium text-highlighted">
      <UIcon name="i-lucide-layout-template" class="size-4 text-primary" aria-hidden="true" />
      Start from a template
    </div>
    <div class="grid gap-3 md:grid-cols-[minmax(0,14rem)_1fr_auto] md:items-start">
      <UFormField label="Template" name="template">
        <USelect v-model="templateId" :items="items" value-key="value" class="w-full" />
      </UFormField>
      <UFormField
        v-if="template.params.includes('bucket')"
        label="Bucket"
        name="template-bucket"
        :required="template.required.includes('bucket')"
        :help="template.required.includes('bucket') ? 'Wildcards allowed, e.g. logs-*' : 'Empty = all buckets'"
        :error="touched && errors.bucket"
      >
        <UInput v-model="bucket" class="w-full" placeholder="photos" autocomplete="off" @keydown.enter.prevent="apply" />
      </UFormField>
      <UFormField
        v-else-if="template.params.includes('cidr')"
        label="Allowed networks"
        name="template-cidr"
        required
        help="Comma separated CIDRs, e.g. 10.0.0.0/8, 192.168.1.0/24"
        :error="touched && errors.cidr"
      >
        <UInput v-model="cidr" class="w-full font-mono" placeholder="10.0.0.0/8" autocomplete="off" @keydown.enter.prevent="apply" />
      </UFormField>
      <div v-else-if="template.params.includes('users')" class="grid gap-3 sm:grid-cols-2">
        <UFormField
          label="Users"
          name="template-users"
          required
          help="User name or pattern, e.g. team-* (arn:gos3:admin:::user/…)"
          :error="touched && errors.users"
        >
          <UInput v-model="users" class="w-full font-mono" placeholder="team-*" autocomplete="off" @keydown.enter.prevent="apply" />
        </UFormField>
        <UFormField
          label="Attachable policies"
          name="template-policies"
          help="Comma separated (admin:PolicyName); empty = none"
          :error="touched && errors.policies"
        >
          <UInput v-model="policies" class="w-full font-mono" placeholder="readonly, readwrite" autocomplete="off" @keydown.enter.prevent="apply" />
        </UFormField>
      </div>
      <div v-else aria-hidden="true" />
      <div class="md:pt-6">
        <UButton icon="i-lucide-wand-sparkles" label="Apply template" color="neutral" variant="outline" @click="apply" />
      </div>
    </div>
    <p class="text-xs text-muted">
      {{ template.description }}
    </p>
  </div>
</template>
