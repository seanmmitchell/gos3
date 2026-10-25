import type { UpdateStatus } from '~/types/admin'

export interface NoticeChoices {
  dismissed?: { version: string, check: string | null }
  skipped?: string[]
}
export const noticeKey = (serverId: string) => `gos3.updates.v1.${serverId}`

export function readChoices(storage: Pick<Storage, 'getItem'>, serverId: string): NoticeChoices {
  try {
    const value = JSON.parse(storage.getItem(noticeKey(serverId)) ?? '{}') as NoticeChoices
    return value && typeof value === 'object' ? value : {}
  } catch { return {} }
}

export function canNotify(status: UpdateStatus | null, choices: NoticeChoices): boolean {
  const release = status?.available
  if (!status?.supported || !status.enabled || !status.comparable || !release) return false
  // Accept only this repository's release-note URLs, including for a remote server.
  if (!release.url.startsWith('https://github.com/seanmmitchell/gos3/releases/tag/')) return false
  if (Array.isArray(choices.skipped) && choices.skipped.includes(release.version)) return false
  return choices.dismissed?.version !== release.version || choices.dismissed.check !== status.lastCheck
}

export function saveChoice(storage: Pick<Storage, 'getItem' | 'setItem'>, serverId: string, status: UpdateStatus, skip: boolean): NoticeChoices {
  const value = readChoices(storage, serverId)
  if (!status.available) return value
  if (skip) {
    value.skipped = [...new Set([...(Array.isArray(value.skipped) ? value.skipped : []), status.available.version])]
  } else {
    value.dismissed = { version: status.available.version, check: status.lastCheck }
  }
  try { storage.setItem(noticeKey(serverId), JSON.stringify(value)) } catch { /* memory-only for this visit */ }
  return value
}
