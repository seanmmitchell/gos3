import { describe, expect, it } from 'vitest'
import { canNotify, readChoices, saveChoice } from '~/utils/update-notices'
import { emptySettingsForm, formToSettings, settingsSections, settingsToForm } from '~/components/admin/-settings-form'
import type { UpdateStatus } from '~/types/admin'

const status = (over: Partial<UpdateStatus> = {}): UpdateStatus => ({
  supported: true, enabled: true, comparable: true, installedVersion: 'v0.2.2', notifyFor: ['patch', 'minor', 'major'],
  lastCheck: '2026-10-25T12:00:00Z', lastSuccess: '2026-10-25T12:00:00Z', error: '',
  available: { version: 'v0.2.3', type: 'patch', url: 'https://github.com/seanmmitchell/gos3/releases/tag/v0.2.3' }, ...over
})
const storage = () => {
  const data = new Map<string, string>()
  return { getItem: (key: string) => data.get(key) ?? null, setItem: (key: string, value: string) => { data.set(key, value) } }
}

describe('release notice persistence', () => {
  it('dismisses this check only and isolates servers', () => {
    const store = storage()
    const s = status()
    expect(canNotify(s, {})).toBe(true)
    saveChoice(store, 'server-a', s, false)
    expect(canNotify(s, readChoices(store, 'server-a'))).toBe(false)
    expect(canNotify(s, readChoices(store, 'server-b'))).toBe(true)
    expect(canNotify(status({ lastCheck: '2026-10-26T12:00:00Z' }), readChoices(store, 'server-a'))).toBe(true)
    expect(canNotify(status({ available: { ...s.available!, version: 'v0.2.4' } }), readChoices(store, 'server-a'))).toBe(true)
  })
  it('skips exact releases across visits and checks, allowing newer ones', () => {
    const store = storage()
    saveChoice(store, 'server-a', status(), true)
    expect(canNotify(status({ lastCheck: 'tomorrow' }), readChoices(store, 'server-a'))).toBe(false)
    const next = status({ available: { ...status().available!, version: 'v0.2.4' } })
    expect(canNotify(next, readChoices(store, 'server-a'))).toBe(true)
    saveChoice(store, 'server-a', next, true)
    expect(readChoices(store, 'server-a').skipped).toEqual(['v0.2.3', 'v0.2.4'])
  })
  it('handles unavailable storage and suppresses unsupported, disabled and unknown builds', () => {
    expect(readChoices({ getItem: () => { throw Error('blocked') } }, 's')).toEqual({})
    for (const over of [{ enabled: false }, { comparable: false }, { supported: false }, { available: null }]) expect(canNotify(status(over), {})).toBe(false)
    expect(canNotify(status({ available: { ...status().available!, url: 'javascript:alert(1)' } }), {})).toBe(false)
    // A failed check still exposes its previous successful result.
    expect(canNotify(status({ error: 'GitHub is offline' }), {})).toBe(true)
  })
})

describe('update settings form', () => {
  it('round-trips disabled checks and an empty type selection, without adding settings to old servers', () => {
    const old = formToSettings(emptySettingsForm())
    expect(settingsSections(settingsToForm(old)).map(s => s.value)).not.toContain('updates')
    const s = { ...old, updates: { enabled: false, notifyFor: [] } }
    const form = settingsToForm(s)
    expect(settingsSections(form).map(s => s.value)).toContain('updates')
    expect(formToSettings(form).updates).toEqual(s.updates)
  })
})
