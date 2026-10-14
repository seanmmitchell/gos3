// Snippets for newly created access keys (shown exactly once) and a small
// browser download helper.
//
// File name starts with "-" so Nuxt does not register it as a component.
import { AdminError } from '~/utils/admin/client'

/** Shown next to the custom access key ID field and in the 403 explanation. */
export const CUSTOM_KEY_PERMISSION_HINT = 'Choosing the access key ID needs admin:CreateAccessKey on the whole service; admins scoped to some users can only create generated key pairs.'

/**
 * The error to show when creating a key with a caller-chosen key pair
 * failed: a 403 is explained (scoped admins may not choose key IDs);
 * anything else is returned unchanged.
 */
export function explainCreateKeyError(e: unknown, customPair: boolean): unknown {
  if (!customPair || !(e instanceof AdminError) || !e.isAccessDenied) return e
  return new AdminError(e.code, `${CUSTOM_KEY_PERMISSION_HINT} Turn off “Specify the key pair” to have one generated.`, e.status, e)
}

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

/**
 * Why `i` cannot be written into a snippet, or undefined when it can. The
 * key values come from the server's key-creation response, which nothing
 * authenticates (an on-path attacker or hostile server can forge it); the
 * region may come from a server's /_gos3/info. A CR/LF in any of them would
 * add lines such as `credential_process = …` or `NODE_OPTIONS=…` to a file
 * the admin installs, so every value must match the format gos3 issues.
 */
export function snippetProblem(i: CredentialSnippetInput): string | undefined {
  if (validateAccessKeyId(i.accessKey)) return 'The server returned an access key ID in an unexpected format.'
  if (validateSecretKey(i.secretKey)) return 'The server returned a secret key in an unexpected format.'
  if (i.region && !/^[a-z0-9][a-z0-9-]{0,62}$/i.test(i.region)) return 'The connection\'s region has an unexpected format.'
  // eslint-disable-next-line no-control-regex
  if (!/^https?:\/\/[^\s\x00-\x1f\x7f]+$/i.test(i.endpoint)) return 'The connection\'s endpoint has an unexpected format.'
  return undefined
}

function checkSnippetInput(i: CredentialSnippetInput): void {
  const problem = snippetProblem(i)
  if (problem) throw new Error(problem)
}

/** `.env` snippet (AWS SDK / CLI environment variables). Throws for input snippetProblem rejects. */
export function envSnippet(i: CredentialSnippetInput): string {
  checkSnippetInput(i)
  return [
    `AWS_ACCESS_KEY_ID=${i.accessKey}`,
    `AWS_SECRET_ACCESS_KEY=${i.secretKey}`,
    `AWS_REGION=${i.region || 'us-east-1'}`,
    `AWS_ENDPOINT_URL=${i.endpoint}`,
    ''
  ].join('\n')
}

/** `~/.aws/credentials` + `~/.aws/config` snippet for a named profile. Throws for input snippetProblem rejects. */
export function credentialsFileSnippet(i: CredentialSnippetInput): string {
  checkSnippetInput(i)
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

/** Caller-supplied secret keys: 16–128 printable ASCII characters, no spaces (server rule). */
export function validateSecretKey(s: string): string | undefined {
  if (s.length < 16 || s.length > 128) return 'Must be 16–128 characters'
  if (!/^[\x21-\x7e]+$/.test(s)) return 'Only printable ASCII characters without spaces'
  return undefined
}
