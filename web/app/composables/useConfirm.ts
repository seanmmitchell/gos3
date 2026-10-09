import ConfirmModal from '~/components/common/ConfirmModal.vue'

export interface ConfirmOptions {
  title: string
  description?: string
  confirmLabel?: string
  cancelLabel?: string
  /** Button color; destructive actions use 'error' (default). */
  color?: 'error' | 'primary' | 'warning' | 'neutral'
  icon?: string
  /** When set, the user must type this text to enable the confirm button. */
  requireText?: string
}

/**
 * Promise-based confirmation dialog.
 *
 *   const confirm = useConfirm()
 *   if (await confirm({ title: 'Delete bucket?', description: name, confirmLabel: 'Delete', requireText: name })) ...
 */
export function useConfirm() {
  const overlay = useOverlay()
  return async function confirm(opts: ConfirmOptions): Promise<boolean> {
    const modal = overlay.create(ConfirmModal, { destroyOnClose: true })
    const result = await modal.open(opts)
    return result === true
  }
}
