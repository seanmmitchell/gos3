// Detection of secret keys in preset connections (NUXT_PUBLIC_SERVERS and
// public/servers.json). Every UI file and the inlined runtime config are
// readable by anyone who can load the console, so presets must never carry
// secret keys: nuxt.config.ts fails the build (and server/plugins/presets.ts
// warns at runtime) when they do. Same field names as internal/webui.

/** Keys under which a preset may carry a secret key (web/app/utils/connections.ts accepts both). */
export const PRESET_SECRET_FIELDS = ['secretAccessKey', 'secretKey']

/** Env var that lets a build with preset secrets proceed (not recommended). */
export const ALLOW_PRESET_SECRETS_ENV = 'GOS3_UI_ALLOW_PRESET_SECRETS'

function walk(v: unknown, path: string, out: string[], depth = 0): void {
  if (depth > 8 || typeof v !== 'object' || v === null) return
  for (const [k, x] of Object.entries(v as Record<string, unknown>)) {
    const p = Array.isArray(v) ? `${path}[${k}]` : path ? `${path}.${k}` : k
    if (PRESET_SECRET_FIELDS.includes(k) && typeof x === 'string' && x.trim() !== '') out.push(p)
    else walk(x, p, out, depth + 1)
  }
}

/**
 * Paths of the non-empty secret key fields in a presets value: a JSON string
 * (NUXT_PUBLIC_SERVERS, servers.json) or an already-parsed value. Text that
 * is not valid JSON counts when it merely names a field with a value.
 */
export function presetSecretPaths(value: unknown): string[] {
  let v = value
  if (typeof value === 'string') {
    if (!value.trim()) return []
    try {
      v = JSON.parse(value)
    } catch {
      const m = new RegExp(`"(${PRESET_SECRET_FIELDS.join('|')})"\\s*:\\s*"[^"]`).exec(value)
      return m ? [m[1]!] : []
    }
  }
  const out: string[] = []
  walk(v, '', out)
  return out
}

/** True when the override env var is set to an explicit true value. */
export function presetSecretsAllowed(env: Record<string, string | undefined> = process.env): boolean {
  return ['1', 'true', 'on', 'yes'].includes((env[ALLOW_PRESET_SECRETS_ENV] ?? '').trim().toLowerCase())
}
