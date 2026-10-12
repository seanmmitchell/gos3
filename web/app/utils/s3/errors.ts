// S3 error type and parsing of S3 XML error bodies.
import { child, text, tryParseXml } from './xml'

/** Error codes used for failures that never reached / were not answered by S3. */
export const CLIENT_ERROR_CODES = ['NetworkError', 'Aborted', 'Timeout', 'InvalidResponse'] as const

export class S3Error extends Error {
  /** S3 error code (e.g. "NoSuchKey") or a status-derived / client code. */
  readonly code: string
  /** HTTP status (0 for network errors / aborts). */
  readonly status: number
  readonly requestId?: string
  readonly hostId?: string
  readonly resource?: string
  /** x-amz-bucket-region / <Region> hints for redirects. */
  readonly region?: string
  /** Extra elements of the <Error> body (e.g. Key, BucketName, Endpoint). */
  readonly details: Record<string, string>

  constructor(init: {
    code: string
    message: string
    status: number
    requestId?: string
    hostId?: string
    resource?: string
    region?: string
    details?: Record<string, string>
    cause?: unknown
  }) {
    super(init.message, init.cause !== undefined ? { cause: init.cause } : undefined)
    this.name = 'S3Error'
    this.code = init.code
    this.status = init.status
    this.requestId = init.requestId
    this.hostId = init.hostId
    this.resource = init.resource
    this.region = init.region
    this.details = init.details ?? {}
  }

  get isNotFound(): boolean {
    return this.status === 404
  }

  get isAccessDenied(): boolean {
    return this.status === 403
  }

  get isNetworkError(): boolean {
    return this.code === 'NetworkError'
  }

  get isAborted(): boolean {
    return this.code === 'Aborted'
  }
}

export function isS3Error(e: unknown): e is S3Error {
  return e instanceof S3Error
}

const STATUS_CODES: Record<number, [string, string]> = {
  301: ['PermanentRedirect', 'The bucket must be addressed using a different endpoint.'],
  304: ['NotModified', 'Not modified.'],
  307: ['TemporaryRedirect', 'Temporary redirect.'],
  400: ['BadRequest', 'Bad request.'],
  401: ['Unauthorized', 'Unauthorized.'],
  403: ['AccessDenied', 'Access denied.'],
  404: ['NotFound', 'Not found.'],
  405: ['MethodNotAllowed', 'The specified method is not allowed against this resource.'],
  409: ['Conflict', 'Conflict.'],
  411: ['MissingContentLength', 'Content-Length is required.'],
  412: ['PreconditionFailed', 'At least one of the preconditions you specified did not hold.'],
  416: ['InvalidRange', 'The requested range is not satisfiable.'],
  429: ['SlowDown', 'Too many requests.'],
  500: ['InternalError', 'We encountered an internal error. Please try again.'],
  501: ['NotImplemented', 'A header or operation you provided implies functionality that is not implemented.'],
  502: ['BadGateway', 'Bad gateway.'],
  503: ['ServiceUnavailable', 'Service unavailable. Please try again.'],
  504: ['GatewayTimeout', 'Gateway timeout.']
}

/** Code/message derived from an HTTP status (HEAD errors have no body). */
export function statusError(status: number): { code: string, message: string } {
  const e = STATUS_CODES[status]
  if (e) return { code: e[0], message: e[1] }
  return { code: status >= 500 ? 'InternalError' : 'UnknownError', message: `HTTP ${status}` }
}

/**
 * Parses an S3 error body (`<Error><Code/>…</Error>`) into an S3Error. Falls
 * back to a status-derived code when the body is empty or not XML.
 */
export function parseS3Error(status: number, body: string, headers?: Headers): S3Error {
  const requestId = headers?.get('x-amz-request-id') ?? undefined
  const hostId = headers?.get('x-amz-id-2') ?? undefined
  const regionHeader = headers?.get('x-amz-bucket-region') ?? undefined
  const root = tryParseXml(body)
  const errEl = root?.localName === 'Error' ? root : child(root, 'Error')
  if (errEl) {
    const details: Record<string, string> = {}
    for (let n = errEl.firstElementChild; n; n = n.nextElementSibling) {
      if (!['Code', 'Message', 'RequestId', 'HostId', 'Resource'].includes(n.localName)) {
        details[n.localName] = n.textContent ?? ''
      }
    }
    const fallback = statusError(status)
    return new S3Error({
      code: text(errEl, 'Code') || fallback.code,
      message: text(errEl, 'Message') || fallback.message,
      status,
      requestId: text(errEl, 'RequestId') || requestId,
      hostId: text(errEl, 'HostId') || hostId,
      resource: text(errEl, 'Resource') || undefined,
      region: text(errEl, 'Region') || regionHeader,
      details
    })
  }
  const fallback = statusError(status)
  return new S3Error({ ...fallback, status, requestId, hostId, region: regionHeader })
}

/** Error thrown when fetch itself fails (unreachable host, CORS, TLS …). */
export function networkError(cause: unknown, url?: string): S3Error {
  if (isAbortError(cause)) {
    return new S3Error({ code: 'Aborted', message: 'The request was aborted.', status: 0, cause })
  }
  const where = url ? ` (${safeOrigin(url)})` : ''
  return new S3Error({
    code: 'NetworkError',
    message: `Could not reach the server${where}. Check that it is running and that its CORS settings allow this origin.`,
    status: 0,
    cause
  })
}

/** True for DOMException AbortError / our Aborted S3Error. */
export function isAbortError(e: unknown): boolean {
  if (e instanceof S3Error) return e.code === 'Aborted'
  return typeof e === 'object' && e !== null && 'name' in e && (e as { name: unknown }).name === 'AbortError'
}

function safeOrigin(url: string): string {
  try {
    return new URL(url).origin
  } catch {
    return url
  }
}
