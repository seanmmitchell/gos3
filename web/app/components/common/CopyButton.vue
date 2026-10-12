<script setup lang="ts">
/** Copies `value` to the clipboard; shows a check icon briefly. */
const props = withDefaults(defineProps<{
  value: string
  label?: string
  /** Accessible name when there is no visible label. */
  ariaLabel?: string
  size?: 'xs' | 'sm' | 'md' | 'lg'
  variant?: 'ghost' | 'outline' | 'soft' | 'subtle' | 'link' | 'solid'
  color?: 'neutral' | 'primary'
  toast?: boolean
}>(), {
  label: undefined,
  ariaLabel: 'Copy to clipboard',
  size: 'xs',
  variant: 'ghost',
  color: 'neutral',
  toast: false
})

const copied = ref(false)
const toastApi = useToast()
let timer: ReturnType<typeof setTimeout> | undefined

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    // Fallback for insecure contexts (http:// on a LAN IP).
    try {
      const ta = document.createElement('textarea')
      ta.value = text
      ta.setAttribute('readonly', '')
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      const ok = document.execCommand('copy')
      document.body.removeChild(ta)
      return ok
    } catch {
      return false
    }
  }
}

async function copy() {
  const ok = await copyText(props.value)
  if (!ok) {
    toastApi.add({ title: 'Could not copy to clipboard', color: 'error', icon: 'i-lucide-circle-alert' })
    return
  }
  copied.value = true
  if (props.toast) toastApi.add({ title: 'Copied to clipboard', color: 'success', icon: 'i-lucide-clipboard-check' })
  clearTimeout(timer)
  timer = setTimeout(() => (copied.value = false), 1500)
}

onBeforeUnmount(() => clearTimeout(timer))
</script>

<template>
  <UButton
    :icon="copied ? 'i-lucide-check' : 'i-lucide-copy'"
    :label="label"
    :size="size"
    :variant="variant"
    :color="copied ? 'success' : color"
    :aria-label="label ? undefined : ariaLabel"
    :square="!label"
    @click="copy"
  />
</template>
