// Snippets for newly created access keys (shown exactly once) and a small
// browser download helper.
//
// File name starts with "-" so Nuxt does not register it as a component.

export interface CredentialSnippetInput {
  accessKey: string
  secretKey: string
  endpoint: string
  region: string
  /** Profile name for ~/.aws/credentials (default "gos3"). */
  profile?: string
}

/** Profile names: keep it simple for INI section headers. */
export function profileName(s: string | undefined): string {
  const v = (s ?? '').trim().replace(/[^\w.-]+/g, '-').replace(/^-+|-+$/g, '')
  return v || 'gos3'
}

/** `.env` snippet (AWS SDK / CLI environment variables). */
export function envSnippet(i: CredentialSnippetInput): string {
  return [
    `AWS_ACCESS_KEY_ID=${i.accessKey}`,
    `AWS_SECRET_ACCESS_KEY=${i.secretKey}`,
    `AWS_REGION=${i.region || 'us-east-1'}`,
    `AWS_ENDPOINT_URL=${i.endpoint}`,
    ''
  ].join('\n')
}

/** `~/.aws/credentials` + `~/.aws/config` snippet for a named profile. */
export function credentialsFileSnippet(i: CredentialSnippetInput): string {
  const p = profileName(i.profile)
  return [
    '# ~/.aws/credentials',
    `[${p}]`,
    `aws_access_key_id = ${i.accessKey}`,
    `aws_secret_access_key = ${i.secretKey}`,
    '',
    '# ~/.aws/config',
    `[profile ${p}]`,
    `region = ${i.region || 'us-east-1'}`,
    `endpoint_url = ${i.endpoint}`,
    's3 =',
    '    addressing_style = path',
    ''
  ].join('\n')
}

/** Triggers a browser download of a text file. */
export function downloadText(filename: string, content: string, type = 'text/plain'): void {
  const blob = new Blob([content], { type: `${type};charset=utf-8` })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.rel = 'noopener'
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

/** Caller-supplied access key IDs: 3–128 of [A-Za-z0-9._-] (server rule). */
export function validateAccessKeyId(s: string): string | undefined {
  if (s.length < 3 || s.length > 128) return 'Must be 3–128 characters'
  if (!/^[\w.-]+$/.test(s)) return 'Only letters, digits and . _ - are allowed'
  return undefined
}

/** Caller-supplied secret keys: 8–128 printable ASCII characters, no spaces. */
export function validateSecretKey(s: string): string | undefined {
  if (s.length < 8 || s.length > 128) return 'Must be 8–128 characters'
  if (!/^[\x21-\x7e]+$/.test(s)) return 'Only printable ASCII characters without spaces'
  return undefined
}
