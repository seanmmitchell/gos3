import type { BackingConfiguration } from '~/types/admin'

export function validateBackingConfiguration(form: BackingConfiguration): string | undefined {
  if (form.backings.length > 64) return 'At most 64 additional backings are allowed.'
  const names = new Set(['default'])
  const paths = new Set<string>()
  for (const b of form.backings) {
    if (!/^[a-z0-9][a-z0-9_-]{0,31}$/.test(b.name)) return 'Backing names need 1–32 lowercase letters, digits, underscores or dashes, starting with a letter or digit.'
    if (names.has(b.name)) return `Backing name "${b.name}" is already used. The name "default" is reserved.`
    names.add(b.name)
    if (b.backend === 'fs') {
      if (!b.path.trim()) return `Enter a server directory for "${b.name}".`
      if (paths.has(b.path)) return 'Every filesystem backing needs its own directory.'
      paths.add(b.path)
    } else if (b.backend !== 'memory' || b.path) return 'Memory backings cannot have a directory.'
  }
  if (!names.has(form.defaultBacking)) return 'Choose an existing backing for new buckets.'
}

export function copyBackingConfiguration(form: BackingConfiguration): BackingConfiguration {
  return { ...form, backings: form.backings.map(b => ({ ...b })) }
}
