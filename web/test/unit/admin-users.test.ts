import { describe, expect, it } from 'vitest'
import { userDetailsUpdate } from '~/components/admin/-users'

describe('userDetailsUpdate', () => {
  it('leaves policies out when only the description changed', () => {
    expect(userDetailsUpdate({ policies: ['readonly', 'team'] }, '  new text ', ['team', 'readonly'])).toEqual({ description: 'new text' })
    expect(userDetailsUpdate({ policies: null }, 'd', [])).toEqual({ description: 'd' })
    expect(userDetailsUpdate({ policies: [] }, 'd', [])).toEqual({ description: 'd' })
  })

  it('sends the full selection when policies changed', () => {
    expect(userDetailsUpdate({ policies: ['readonly'] }, 'd', ['readonly', 'team'])).toEqual({ description: 'd', policies: ['readonly', 'team'] })
    expect(userDetailsUpdate({ policies: ['readonly'] }, 'd', [])).toEqual({ description: 'd', policies: [] })
  })

  it('sends policies when the current user is unknown', () => {
    expect(userDetailsUpdate(undefined, 'd', ['x'])).toEqual({ description: 'd', policies: ['x'] })
  })

  it('does not alias the selection', () => {
    const selected = ['a']
    const u = userDetailsUpdate({ policies: [] }, 'd', selected)
    selected.push('b')
    expect(u.policies).toEqual(['a'])
  })
})
