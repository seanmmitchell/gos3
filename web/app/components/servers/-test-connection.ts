// "Test connection" and credential verification with clear error mapping:
//   CORS / network / mixed content / timeout  vs  SignatureDoesNotMatch (secret)
//   vs InvalidAccessKeyId vs RequestTimeTooSkewed (clock) vs AccessDenied.
//
// Browsers report CORS failures and unreachable hosts identically (a
// TypeError). To tell them apart we send a `mode: 'no-cors'` request: it
// resolves (opaque response) when the host answered at all, and rejects when
// it is unreachable.
//
// Plain TS (explicit imports, injectable fetch) so it is unit-testable.
import type { Info, WhoAmI } from '~/types/admin'
import type { ConnectionKind } from '~/types/connection'
import { AdminClient, AdminError, probeServer, type FetchLike } from '~/utils/admin/client'
import { S3Client } from '~/utils/s3/client'
import { S3Error } from '~/utils/s3/errors'
import { normalizeEndpoint } from '~/utils/s3/url'
import { isMixedContent, type KindChoice } from './-form'

export type ProblemKind =
  | 'cors'
  | 'network'
  | 'mixed-content'
  | 'timeout'
  | 'signature'
  | 'access-key'
  | 'clock-skew'
  | 'access-denied'
  | 'auth'
  | 'not-gos3'
  | 'other'

export interface Diagnosis {
  kind: ProblemKind
  title: string
  message: string
  hint?: string
  code?: string
  status?: number
}

export interface TestDeps {
  /** fetch for the signed / info requests (tests). */
  fetch?: FetchLike
  /** Reachability check (no-cors request); default uses globalThis.fetch. */
  reachable?: (endpoint: string) => Promise<boolean>
  /** Page origin + protocol (default globalThis.location). */
  origin?: string
  protocol?: string
  /** Timeout of each step in ms (default 8000). */
  timeoutMs?: number
}

/** Resolves true when the host answers anything (opaque no-cors response). */
export async function defaultReachable(endpoint: string, timeoutMs = 5000): Promise<boolean> {
  const ctrl = new AbortController()
  const t = setTimeout(() => ctrl.abort(), timeoutMs)
  try {
    await globalThis.fetch(`${normalizeEndpoint(endpoint)}/`, { mode: 'no-cors', cache: 'no-store', signal: ctrl.signal, credentials: 'omit' })
    return true
  } catch {
    return false
  } finally {
    clearTimeout(t)
  }
}

function pageOrigin(deps: TestDeps): string {
  return deps.origin ?? globalThis.location?.origin ?? 'this origin'
}

function hostOf(endpoint: string): string {
  try {
    return new URL(normalizeEndpoint(endpoint)).host
  } catch {
    return endpoint
  }
}

/** S3 error code carried by an error (admin 401s put it in the message). */
export function errorCode(e: unknown): string | undefined {
  if (e instanceof S3Error) return e.code
  if (e instanceof AdminError) {
    if (e.code === 'Unauthenticated') {
      const m = /\b(SignatureDoesNotMatch|InvalidAccessKeyId|RequestTimeTooSkewed|ExpiredToken|InvalidToken|AccessDenied|AuthorizationHeaderMalformed|MissingSecurityHeader)\b/.exec(e.message)
      return m?.[1] ?? e.code
    }
    return e.code
  }
  return undefined
}

function isNetwork(e: unknown): boolean {
  return (e instanceof S3Error && e.code === 'NetworkError') || (e instanceof AdminError && e.code === 'NetworkError')
}

function isTimeout(e: unknown): boolean {
  return (e instanceof AdminError && e.code === 'Timeout') || (e instanceof S3Error && (e.code === 'Timeout' || e.code === 'Aborted'))
}

export interface DiagnoseContext {
  endpoint: string
  kind?: ConnectionKind
  /** Result of the no-cors reachability check, when it ran. */
  reachable?: boolean
  origin?: string
  protocol?: string
}

/** Maps any error of a connection attempt to a user-facing explanation. */
export function diagnose(e: unknown, ctx: DiagnoseContext): Diagnosis {
  const host = hostOf(ctx.endpoint)
  const origin = ctx.origin ?? 'this origin'
  const status = e instanceof S3Error || e instanceof AdminError ? e.status : undefined
  const code = errorCode(e)
  const raw = e instanceof Error ? e.message : String(e)

  if (isTimeout(e)) {
    return { kind: 'timeout', title: 'Timed out', message: `${host} did not answer in time.`, hint: 'Check the URL and port, and that no firewall drops the connection.', code, status }
  }
  if (isNetwork(e)) {
    if (isMixedContent(ctx.endpoint, ctx.protocol)) {
      return {
        kind: 'mixed-content',
        title: 'Blocked: insecure endpoint',
        message: `This page is served over HTTPS, so the browser refuses plain-HTTP requests to ${host}.`,
        hint: 'Use an https:// endpoint, or open this UI over http.',
        code,
        status
      }
    }
    if (ctx.reachable === true) {
      return {
        kind: 'cors',
        title: 'Blocked by CORS',
        message: `${host} is reachable, but the browser blocked the response because the server's CORS settings do not allow ${origin}.`,
        hint: ctx.kind === 'gos3'
          ? `Add ${origin} to the server's CORS allowed origins (cors.allowedOrigins, or Settings → CORS) and expose the ETag and x-amz-version-id headers.`
          : `Allow ${origin} in the service's CORS configuration (methods GET, PUT, POST, DELETE, HEAD; headers *; expose ETag). Some providers never allow ListBuckets from browsers.`,
        code,
        status
      }
    }
    return {
      kind: 'network',
      title: 'Server unreachable',
      message: ctx.reachable === false
        ? `Could not connect to ${host}. The server is down, the URL or port is wrong, or a firewall blocks it.`
        : `Could not reach ${host}: the server is down or unreachable, or its CORS settings block ${origin}.`,
      hint: 'Check the URL (scheme, host and port) and that the server is running.',
      code,
      status
    }
  }
  switch (code) {
    case 'SignatureDoesNotMatch':
      return { kind: 'signature', title: 'Wrong secret key', message: 'The server rejected the request signature: the secret key does not match the access key.', hint: 'Re-enter the secret key. A proxy rewriting the Host header or path also breaks signatures.', code, status }
    case 'InvalidAccessKeyId':
      return { kind: 'access-key', title: 'Unknown access key', message: 'The server does not know this access key (or it is inactive).', hint: 'Check the access key ID; make sure it belongs to this server and is active.', code, status }
    case 'RequestTimeTooSkewed':
      return { kind: 'clock-skew', title: 'Clock out of sync', message: 'The difference between your computer\'s clock and the server\'s clock is too large (more than 15 minutes).', hint: 'Synchronise the system clock (NTP) on this computer or on the server.', code, status }
    case 'ExpiredToken':
    case 'InvalidToken':
      return { kind: 'auth', title: 'Credentials expired', message: raw, hint: 'Use long-term access keys.', code, status }
    case 'AccessDenied':
      return { kind: 'access-denied', title: 'Access denied', message: 'The credentials are valid, but this identity may not perform the request.', hint: 'Ask an administrator to attach a policy that allows it.', code, status }
    case 'NotGos3':
      return { kind: 'not-gos3', title: 'Not a gos3 server', message: raw, hint: 'Choose "S3-compatible" as the server type.', code, status }
    case 'Unauthenticated':
    case 'Unauthorized':
      return { kind: 'auth', title: 'Credentials rejected', message: raw, hint: 'Check the access key and secret key.', code, status }
  }
  if (status === 301 || status === 307 || code === 'PermanentRedirect' || code === 'AuthorizationHeaderMalformed') {
    return { kind: 'other', title: 'Wrong region or endpoint', message: raw, hint: 'Set the region of this endpoint (e.g. eu-west-1) or use the regional endpoint URL.', code, status }
  }
  return { kind: 'other', title: code ? `${code}${status ? ` (${status})` : ''}` : 'Error', message: raw, code, status }
}

// -- kind detection ---------------------------------------------------------------

export interface DetectResult {
  /** gos3 / s3 when known; undefined when the host could not be reached. */
  kind?: ConnectionKind
  info?: Info
  /** /_gos3/info answered 401 (gos3 with publicInfo disabled). */
  protectedInfo?: boolean
  reachable?: boolean
  error?: AdminError
  latencyMs: number
}

/** Probes /_gos3/info to detect whether an endpoint is a gos3 server. Never throws. */
export async function detectKind(endpoint: string, deps: TestDeps = {}): Promise<DetectResult> {
  const p = await probeServer(endpoint, { timeoutMs: deps.timeoutMs ?? 5000, fetch: deps.fetch })
  if (p.ok && p.info) return { kind: 'gos3', info: p.info, reachable: true, latencyMs: p.latencyMs }
  const e = p.error
  if (e?.status === 401) return { kind: 'gos3', protectedInfo: true, reachable: true, error: e, latencyMs: p.latencyMs }
  if (e && !e.isNetworkError && e.code !== 'Aborted') return { kind: 's3', reachable: true, error: e, latencyMs: p.latencyMs }
  // gos3 always answers /_gos3/info with CORS "*": a network error means the
  // host is down, or it is reachable but is not gos3.
  const reachable = e?.code === 'Timeout' ? false : await (deps.reachable ?? defaultReachable)(endpoint)
  return { kind: reachable ? 's3' : undefined, reachable, error: e, latencyMs: p.latencyMs }
}

// -- credentials ------------------------------------------------------------------

export interface CredentialsInput {
  endpoint: string
  region: string
  accessKeyId: string
  secretAccessKey: string
  forcePathStyle: boolean
}

export interface VerifyResult {
  whoami?: WhoAmI
  bucketCount?: number
  /** ListBuckets was denied (credentials valid but restricted). */
  listDenied?: boolean
}

async function withTimeout<T>(ms: number, fn: (signal: AbortSignal) => Promise<T>): Promise<T> {
  const ctrl = new AbortController()
  const t = setTimeout(() => ctrl.abort(), ms)
  try {
    return await fn(ctrl.signal)
  } catch (e) {
    if (ctrl.signal.aborted && e instanceof S3Error && e.code === 'Aborted') {
      throw new S3Error({ code: 'Timeout', message: 'The request timed out.', status: 0, cause: e })
    }
    throw e
  } finally {
    clearTimeout(t)
  }
}

/**
 * Verifies credentials: gos3 → GET /whoami; generic S3 → ListBuckets
 * (AccessDenied counts as valid credentials). Throws the raw error.
 */
export async function verifyCredentials(c: CredentialsInput, kind: ConnectionKind, deps: TestDeps = {}): Promise<VerifyResult> {
  const timeoutMs = deps.timeoutMs ?? 8000
  if (kind === 'gos3') {
    const admin = new AdminClient(c, { fetch: deps.fetch, timeoutMs })
    return { whoami: await admin.whoami() }
  }
  const s3 = new S3Client({ ...c, fetch: deps.fetch })
  try {
    const r = await withTimeout(timeoutMs, signal => s3.listBuckets(signal))
    return { bucketCount: r.buckets.length }
  } catch (e) {
    if (e instanceof S3Error && e.code === 'AccessDenied') return { listDenied: true }
    throw e
  }
}

/** verifyCredentials + diagnosis (runs the reachability check on network errors). */
export async function verifyAndDiagnose(c: CredentialsInput, kind: ConnectionKind, deps: TestDeps = {}): Promise<{ ok: true, result: VerifyResult } | { ok: false, error: unknown, diagnosis: Diagnosis }> {
  try {
    return { ok: true, result: await verifyCredentials(c, kind, deps) }
  } catch (error) {
    const reachable = isNetwork(error) ? await (deps.reachable ?? defaultReachable)(c.endpoint) : undefined
    return { ok: false, error, diagnosis: diagnose(error, { endpoint: c.endpoint, kind, reachable, origin: pageOrigin(deps), protocol: deps.protocol ?? globalThis.location?.protocol }) }
  }
}

// -- full test --------------------------------------------------------------------

export type StepStatus = 'ok' | 'warning' | 'error' | 'skipped'

export interface TestStep {
  id: 'reach' | 'identity' | 'buckets'
  label: string
  status: StepStatus
  detail: string
  diagnosis?: Diagnosis
}

export interface TestReport {
  ok: boolean
  steps: TestStep[]
  kind?: ConnectionKind
  detectedKind?: ConnectionKind
  info?: Info
  whoami?: WhoAmI
  bucketCount?: number
}

export interface TestInput extends CredentialsInput {
  kind: KindChoice
}

/**
 * Tests a connection definition step by step:
 *   1. reach    — unsigned GET /_gos3/info (detects gos3 vs generic S3)
 *   2. identity — signed GET /whoami (gos3 only)
 *   3. buckets  — signed ListBuckets
 */
export async function testConnection(input: TestInput, deps: TestDeps = {}): Promise<TestReport> {
  const steps: TestStep[] = []
  const origin = pageOrigin(deps)
  const protocol = deps.protocol ?? globalThis.location?.protocol
  const host = hostOf(input.endpoint)

  const det = await detectKind(input.endpoint, deps)
  const kind: ConnectionKind | undefined = input.kind === 'auto' ? det.kind : input.kind
  const report: TestReport = { ok: false, steps, kind, detectedKind: det.kind, info: det.info }

  if (det.info) {
    steps.push({ id: 'reach', label: 'Reach server', status: 'ok', detail: `gos3 ${det.info.version} "${det.info.name}" answered in ${det.latencyMs} ms.` })
  } else if (det.protectedInfo) {
    steps.push({ id: 'reach', label: 'Reach server', status: 'ok', detail: 'gos3 server found (its identity document requires authentication).' })
  } else if (det.kind === 's3') {
    const note = input.kind === 'gos3' ? ' It does not look like a gos3 server.' : ''
    steps.push({
      id: 'reach',
      label: 'Reach server',
      status: input.kind === 'gos3' ? 'warning' : 'ok',
      detail: det.error && !det.error.isNetworkError ? `${host} answered (not a gos3 server).${note}` : `${host} is reachable (not a gos3 server, or /_gos3/info is blocked).${note}`
    })
  } else {
    const diagnosis = diagnose(det.error ?? new AdminError('NetworkError', 'unreachable', 0), { endpoint: input.endpoint, kind, reachable: det.reachable, origin, protocol })
    steps.push({ id: 'reach', label: 'Reach server', status: 'error', detail: diagnosis.message, diagnosis })
    steps.push({ id: 'buckets', label: 'Sign in', status: 'skipped', detail: 'Skipped: the server is not reachable.' })
    return report
  }

  if (!input.accessKeyId || !input.secretAccessKey) {
    steps.push({ id: 'buckets', label: 'Sign in', status: 'skipped', detail: 'No credentials entered — only reachability was tested.' })
    report.ok = true
    return report
  }

  const creds: CredentialsInput = { endpoint: input.endpoint, region: input.region, accessKeyId: input.accessKeyId, secretAccessKey: input.secretAccessKey, forcePathStyle: input.forcePathStyle }

  if (kind === 'gos3') {
    const r = await verifyAndDiagnose(creds, 'gos3', deps)
    if (r.ok) {
      const w = r.result.whoami!
      report.whoami = w
      report.info ??= w.server
      const who = w.isRoot ? `${w.user || 'root'} (root)` : w.user
      steps.push({ id: 'identity', label: 'Sign in (admin API)', status: 'ok', detail: `Signed in as ${who}${w.policies?.length ? ` · policies: ${w.policies.join(', ')}` : ''}.` })
    } else {
      steps.push({ id: 'identity', label: 'Sign in (admin API)', status: 'error', detail: r.diagnosis.message, diagnosis: r.diagnosis })
      steps.push({ id: 'buckets', label: 'List buckets', status: 'skipped', detail: 'Skipped: sign-in failed.' })
      return report
    }
  }

  const b = await verifyAndDiagnose(creds, 's3', deps)
  if (b.ok) {
    if (b.result.listDenied) {
      steps.push({ id: 'buckets', label: 'List buckets', status: 'warning', detail: 'Credentials accepted, but this identity may not list buckets.' })
    } else {
      report.bucketCount = b.result.bucketCount
      steps.push({ id: 'buckets', label: 'List buckets', status: 'ok', detail: `Signed S3 request succeeded: ${b.result.bucketCount} bucket${b.result.bucketCount === 1 ? '' : 's'}.` })
    }
  } else {
    steps.push({ id: 'buckets', label: 'List buckets', status: 'error', detail: b.diagnosis.message, diagnosis: b.diagnosis })
  }
  report.ok = !steps.some(s => s.status === 'error')
  return report
}
