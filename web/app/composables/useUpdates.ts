import type { UpdateStatus } from '~/types/admin'

/** The layout polls only the connected server's cache, never GitHub. Settings
 * share this snapshot so a save immediately updates the notice. */
export function useUpdates() {
  const { connection, admin } = useConnection()
  const statuses = useState<Record<string, UpdateStatus | null>>('gos3-update-status', () => ({}))
  const errors = useState<Record<string, string>>('gos3-update-errors', () => ({}))
  const updateStatus = computed(() => connection.value ? statuses.value[connection.value.id] ?? null : null)
  const updateError = computed(() => connection.value ? errors.value[connection.value.id] ?? '' : '')
  async function refreshUpdates() {
    const c = connection.value
    const client = admin.value
    if (!c || c.kind !== 'gos3' || !client) return
    try {
      const result = await client.getUpdates()
      if (connection.value !== c || admin.value !== client) return
      statuses.value[c.id] = result
      errors.value[c.id] = ''
    } catch (e) {
      if (connection.value !== c) return
      // Old servers do not have this endpoint. Never turn a failure into a notice.
      statuses.value[c.id] = null
      errors.value[c.id] = errorMessage(e)
    }
  }
  return { updateStatus, updateError, refreshUpdates }
}
