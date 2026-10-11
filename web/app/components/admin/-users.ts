// Helpers for the IAM user pages.
//
// The file name starts with "-" so Nuxt's component scanner ignores it
// (`ignorePrefix`); import it explicitly:
//
//   import { userDetailsUpdate } from '~/components/admin/-users'
import type { User, UserUpdate } from '~/types/admin'

function samePolicies(a: readonly string[] | null | undefined, b: readonly string[]): boolean {
  return [...(a ?? [])].sort().join('\n') === [...b].sort().join('\n')
}

/**
 * The PATCH /users/{name} body for the details form. `policies` is sent only
 * when the selection differs from the user's current policies: attaching
 * policies is authorized against the admin:PolicyName condition key, so a
 * description-only edit must not resend them (a delegated administrator
 * limited to certain policies could otherwise not edit descriptions).
 */
export function userDetailsUpdate(user: Pick<User, 'policies'> | null | undefined, description: string, selected: readonly string[]): UserUpdate {
  const update: UserUpdate = { description: description.trim() }
  if (!user || !samePolicies(user.policies, selected)) update.policies = [...selected]
  return update
}
