// Generic error helpers for UI code (auto-imported by Nuxt from app/utils/).
import { AdminError } from './admin/client'
import { S3Error } from './s3/errors'

export interface DescribedError {
  title: string
  message: string
  code?: string
  status?: number
  requestId?: string
  /** Short hint on how to fix it, when we know one. */
  hint?: string
}

const HINTS: Record<string, string> = {
  NetworkError: 'Make sure the server is reachable and its CORS settings allow this origin.',
  Timeout: 'The server did not answer in time.',
  InvalidAccessKeyId: 'Check the access key of this connection.',
  SignatureDoesNotMatch: 'Check the secret key of this connection (and the system clock).',
  RequestTimeTooSkewed: 'Your computer clock differs too much from the server clock.',
  Unauthenticated: 'The credentials were rejected. Check the access and secret keys.',
  AccessDenied: 'Your identity is not allowed to perform this action.',
  MissingETag: 'Add "ETag" to the server CORS ExposeHeaders setting.',
  NotGos3: 'Admin features are only available on gos3 servers.'
}

/** Normalises any thrown value into a displayable description. */
export function describeError(e: unknown): DescribedError {
  if (e instanceof S3Error) {
    return {
      title: e.status ? `${e.code} (${e.status})` : e.code,
      message: e.message,
      code: e.code,
      status: e.status,
      requestId: e.requestId,
      hint: HINTS[e.code]
    }
  }
  if (e instanceof AdminError) {
    const sigHint = e.code === 'Unauthenticated' ? HINTS[e.message.split(/[\s:]/)[0] ?? ''] : undefined
    return {
      title: e.status ? `${e.code} (${e.status})` : e.code,
      message: e.message,
      code: e.code,
      status: e.status,
      hint: sigHint ?? HINTS[e.code]
    }
  }
  if (e instanceof Error) return { title: e.name || 'Error', message: e.message }
  if (typeof e === 'string') return { title: 'Error', message: e }
  return { title: 'Error', message: 'An unknown error occurred' }
}

/** One-line message for toasts. */
export function errorMessage(e: unknown): string {
  const d = describeError(e)
  return d.hint ? `${d.message} ${d.hint}` : d.message
}

/** True for 401/403-type credential failures from either API. */
export function isAuthError(e: unknown): boolean {
  if (e instanceof AdminError) return e.status === 401
  if (e instanceof S3Error) {
    return ['InvalidAccessKeyId', 'SignatureDoesNotMatch', 'InvalidToken', 'ExpiredToken', 'Unauthorized'].includes(e.code)
  }
  return false
}

/** True for aborted requests (user cancelled) — usually not worth a toast. */
export function isAbortedError(e: unknown): boolean {
  if (e instanceof S3Error || e instanceof AdminError) return e.code === 'Aborted'
  return typeof e === 'object' && e !== null && (e as { name?: unknown }).name === 'AbortError'
}
