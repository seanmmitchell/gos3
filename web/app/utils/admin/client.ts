// Typed client for the gos3 admin REST API (docs/admin-api.md).
//
//   const admin = new AdminClient(connection)        // endpoint/region/keys
//   const me = await admin.whoami()
//   const { users } = await admin.listUsers()
//
// Requests are SigV4-signed with aws4fetch (service "s3", the connection's
// region). The payload is always signed: X-Amz-Content-Sha256 carries the hex
// SHA-256 of the exact body bytes that are sent (of the empty string when
// there is no body), never UNSIGNED-PAYLOAD, so a captured request cannot be
// replayed with another body. gos3 rejects admin requests with a body whose
// payload is unsigned. Errors are thrown as AdminError.
import { TransportError } from '../transport'
import { AwsClient } from '../s3/signing'
import type {
  AccessKey,
  AccessKeyStatus,
  AdminErrorCode,
  BackingList,
  BackingConfiguration,
  BucketCompression,
  BucketEncryptionProvider,
  BucketEncryptionProviderInput,
  BucketSnapshots,
  BucketSummary,
  ContainerInfo,
  ContainerSpec,
  CopyJobDryRun,
  CopyJobSpec,
  CreateAccessKeyRequest,
  CreateOnBacking,
  CreateRemoteBucketRequest,
  CreateUserRequest,
  CreateUserResponse,
  Decision,
  DiscoveryState,
  HealthStatus,
  Info,
  Job,
  JobAction,
  ListJobsOptions,
  ListRemoteBucketsOptions,
  MoveBucketRequest,
  NamedPolicy,
  Peer,
  Placement,
  PutPolicyRequest,
  RemoteBucket,
  RemoteBucketList,
  ReplicationMode,
  ResticCredentials,
  ResticCredentialsRequest,
  ResticInfo,
  RestoreSnapshotRequest,
  ScanRequest,
  ScanResult,
  ServerInfo,
  Settings,
  SimulateRequest,
  SnapshotConfig,
  Stats,
  User,
  UserUpdate,
  WhoAmI,
  UpdateStatus
} from '~/types/admin'
import { sha256Hex } from '../s3/hash'
import { child, text, tryParseXml } from '../s3/xml'
import { normalizeEndpoint } from '../s3/url'

export const ADMIN_PREFIX = '/_gos3/admin/v1'
export const INFO_PATH = '/_gos3/info'

/**
 * Timeout of requests that may wait for a remote S3 service: adding a
 * container (the server waits up to 30 s for it to load), creating buckets
 * on an s3 backing and probing remote buckets for gos3 data.
 */
export const REMOTE_TIMEOUT_MS = 90000

export type FetchLike = (input: Request) => Promise<Response>

export class AdminError extends Error {
  /** Admin error code (see AdminErrorCode) or a server-provided code. */
  readonly code: AdminErrorCode | string
  /** HTTP status (0 for network errors / timeouts / aborts). */
  readonly status: number

  constructor(code: AdminErrorCode | string, message: string, status: number, cause?: unknown) {
    super(message, cause !== undefined ? { cause } : undefined)
    this.name = 'AdminError'
    this.code = code
    this.status = status
  }

  get isUnauthenticated(): boolean {
    return this.status === 401 || this.code === 'Unauthenticated'
  }

  get isAccessDenied(): boolean {
    return this.status === 403 || this.code === 'AccessDenied'
  }

  get isNotFound(): boolean {
    return this.status === 404
  }

  get isNetworkError(): boolean {
    return this.code === 'NetworkError' || this.code === 'Timeout'
  }
}

export function isAdminError(e: unknown): e is AdminError {
  return e instanceof AdminError
}

const STATUS_CODES: Record<number, AdminErrorCode> = {
  400: 'InvalidRequest',
  401: 'Unauthenticated',
  403: 'AccessDenied',
  404: 'NotFound',
  405: 'MethodNotAllowed',
  409: 'Conflict',
  500: 'InternalError',
  501: 'NotImplemented',
  502: 'ProbeFailed'
}

/** Builds an AdminError from a non-2xx response body (JSON envelope, S3 XML or text). */
export function parseAdminError(status: number, body: string): AdminError {
  const fallbackCode = STATUS_CODES[status] ?? (status >= 500 ? 'InternalError' : 'InvalidRequest')
  const trimmed = body.trim()
  if (trimmed.startsWith('{')) {
    try {
      const j = JSON.parse(trimmed) as { error?: { code?: unknown, message?: unknown }, code?: unknown, message?: unknown }
      const e = j.error ?? j
      const code = typeof e.code === 'string' && e.code ? e.code : fallbackCode
      const message = typeof e.message === 'string' && e.message ? e.message : `HTTP ${status}`
      return new AdminError(code, message, status)
    } catch {
      // fall through
    }
  }
  const xml = tryParseXml(trimmed)
  const errEl = xml?.localName === 'Error' ? xml : child(xml, 'Error')
  if (errEl) {
    return new AdminError(text(errEl, 'Code') || fallbackCode, text(errEl, 'Message') || `HTTP ${status}`, status)
  }
  const message = trimmed && trimmed.length < 300 && !trimmed.startsWith('<') ? trimmed : `HTTP ${status}`
  return new AdminError(fallbackCode, message, status)
}

function networkAdminError(e: unknown, url: string, timedOut: boolean): AdminError {
  if (e instanceof TransportError) return new AdminError(e.code, e.message, e.status, e)
  if (timedOut) return new AdminError('Timeout', `Timed out connecting to ${originOf(url)}.`, 0, e)
  if (typeof e === 'object' && e !== null && (e as { name?: unknown }).name === 'AbortError') {
    return new AdminError('Aborted', 'The request was aborted.', 0, e)
  }
  return new AdminError(
    'NetworkError',
    `Could not reach ${originOf(url)}. Check that the server is running and that its CORS settings allow this origin.`,
    0,
    e
  )
}

function originOf(url: string): string {
  try {
    return new URL(url).origin
  } catch {
    return url
  }
}

/** Runs fn with an AbortSignal that fires on timeout or when outer aborts. */
async function withTimeout<T>(timeoutMs: number | undefined, outer: AbortSignal | undefined, fn: (signal: AbortSignal, timedOut: () => boolean) => Promise<T>): Promise<T> {
  const ctrl = new AbortController()
  let timedOut = false
  const onAbort = () => ctrl.abort()
  if (outer?.aborted) ctrl.abort()
  outer?.addEventListener('abort', onAbort, { once: true })
  const timer = timeoutMs && timeoutMs > 0
    ? setTimeout(() => {
        timedOut = true
        ctrl.abort()
      }, timeoutMs)
    : undefined
  try {
    return await fn(ctrl.signal, () => timedOut)
  } finally {
    if (timer) clearTimeout(timer)
    outer?.removeEventListener('abort', onAbort)
  }
}

/** Validates an Info document. */
export function isGos3Info(v: unknown): v is Info {
  if (typeof v !== 'object' || v === null) return false
  const o = v as Record<string, unknown>
  return o.service === 'gos3' && typeof o.id === 'string' && typeof o.name === 'string'
}

export interface FetchInfoOptions {
  timeoutMs?: number
  signal?: AbortSignal
  fetch?: FetchLike
}

/**
 * Unsigned GET <baseUrl>/_gos3/info (health checks & discovery). Throws
 * AdminError: NotGos3 when the server answers but is not gos3,
 * Unauthenticated when publicInfo is disabled.
 */
export async function fetchInfo(baseUrl: string, opts: FetchInfoOptions = {}): Promise<Info> {
  const url = normalizeEndpoint(baseUrl) + INFO_PATH
  const doFetch = opts.fetch ?? (req => globalThis.fetch(req))
  return withTimeout(opts.timeoutMs ?? 4000, opts.signal, async (signal, timedOut) => {
    let res: Response
    let body: string
    try {
      res = await doFetch(new Request(url, { method: 'GET', headers: { Accept: 'application/json' }, signal, cache: 'no-store' }))
      body = await res.text()
    } catch (e) {
      throw networkAdminError(e, url, timedOut())
    }
    if (!res.ok) {
      if (res.status === 404) throw new AdminError('NotGos3', `${originOf(url)} is not a gos3 server (no /_gos3/info).`, 404)
      throw parseAdminError(res.status, body)
    }
    let json: unknown
    try {
      json = JSON.parse(body)
    } catch {
      throw new AdminError('NotGos3', `${originOf(url)} did not return a gos3 identity document.`, res.status)
    }
    if (!isGos3Info(json)) throw new AdminError('NotGos3', `${originOf(url)} is not a gos3 server.`, res.status)
    return json
  })
}

export interface ProbeResult {
  ok: boolean
  info?: Info
  error?: AdminError
  latencyMs: number
}

/** fetchInfo that never throws; reports latency. */
export async function probeServer(baseUrl: string, opts: FetchInfoOptions = {}): Promise<ProbeResult> {
  const t0 = typeof performance !== 'undefined' ? performance.now() : Date.now()
  const elapsed = () => Math.round((typeof performance !== 'undefined' ? performance.now() : Date.now()) - t0)
  try {
    const info = await fetchInfo(baseUrl, opts)
    return { ok: true, info, latencyMs: elapsed() }
  } catch (e) {
    const error = e instanceof AdminError ? e : new AdminError('NetworkError', String(e), 0, e)
    return { ok: false, error, latencyMs: elapsed() }
  }
}

/** Normalises iam.Decision regardless of JSON key casing. */
export function normalizeDecision(raw: unknown): Decision {
  const o = (typeof raw === 'object' && raw !== null ? raw : {}) as Record<string, unknown>
  const pick = (a: string, b: string) => (o[a] !== undefined ? o[a] : o[b])
  return {
    allowed: pick('allowed', 'Allowed') === true,
    explicitDeny: pick('explicitDeny', 'ExplicitDeny') === true,
    reason: String(pick('reason', 'Reason') ?? '')
  }
}

export interface AdminConnection {
  endpoint: string
  region?: string
  accessKeyId: string
  secretAccessKey: string
  sessionToken?: string
}

export interface AdminClientOptions {
  fetch?: FetchLike
  /** Request timeout in ms (default 30000; 0 = none). */
  timeoutMs?: number
  /** Fixed signing time (tests). */
  datetime?: string
}

export interface RequestOptions {
  query?: Record<string, string | number | boolean | undefined>
  body?: unknown
  signal?: AbortSignal
  /** Response handling: JSON (default), none (204), text. */
  expect?: 'json' | 'none' | 'text'
  /** Skip /_gos3/admin/v1 prefix (path is absolute from the endpoint). */
  raw?: boolean
  /** Per-request timeout override in ms (0 = none). */
  timeoutMs?: number
}

const enc = encodeURIComponent

export class AdminClient {
  readonly endpoint: string
  readonly region: string
  private readonly aws: AwsClient
  private readonly fetchFn: FetchLike
  private readonly timeoutMs: number
  private readonly datetime?: string

  constructor(conn: AdminConnection, opts: AdminClientOptions = {}) {
    this.endpoint = normalizeEndpoint(conn.endpoint)
    this.region = conn.region || 'us-east-1'
    this.aws = new AwsClient({
      accessKeyId: conn.accessKeyId,
      secretAccessKey: conn.secretAccessKey,
      sessionToken: conn.sessionToken,
      service: 's3',
      region: this.region,
      retries: 0
    })
    this.fetchFn = opts.fetch ?? (req => globalThis.fetch(req))
    this.timeoutMs = opts.timeoutMs ?? 30000
    this.datetime = opts.datetime
  }

  /**
   * Absolute URL of an admin API path (e.g. '/users'). Throws AdminError
   * 'InvalidArgument' for a path with '.' / '..' segments (e.g. a user name
   * '..'), which the URL parser would resolve to another endpoint.
   */
  url(path: string, query?: RequestOptions['query'], raw = false): string {
    if (path.split(/[?#]/, 1)[0]!.split('/').some(s => s === '.' || s === '..')) {
      throw new AdminError('InvalidArgument', `Invalid admin API path “${path}”; the request was not sent.`, 0)
    }
    const u = new URL(this.endpoint + (raw ? '' : ADMIN_PREFIX) + path)
    for (const [k, v] of Object.entries(query ?? {})) if (v !== undefined) u.searchParams.set(k, String(v))
    return u.toString()
  }

  /** Signs and sends a request; throws AdminError on non-2xx. */
  async request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
    const url = this.url(path, opts.query, opts.raw)
    const headers: Record<string, string> = { Accept: opts.expect === 'text' ? 'text/plain' : 'application/json' }
    let body: Uint8Array<ArrayBuffer> | undefined
    if (opts.body !== undefined) {
      body = new TextEncoder().encode(JSON.stringify(opts.body))
      headers['Content-Type'] = 'application/json'
    }
    // Sign the payload (C1): aws4fetch puts this value into the canonical
    // request (and SignedHeaders) instead of UNSIGNED-PAYLOAD. The hashed
    // bytes are the bytes sent.
    headers['X-Amz-Content-Sha256'] = await sha256Hex(body ?? '')
    return withTimeout(opts.timeoutMs ?? this.timeoutMs, opts.signal, async (signal, timedOut) => {
      let res: Response
      let text: string
      try {
        const req = await this.aws.sign(url, { method, headers, body, signal, aws: { datetime: this.datetime } } as RequestInit)
        res = await this.fetchFn(req)
        text = await res.text()
      } catch (e) {
        throw networkAdminError(e, url, timedOut())
      }
      if (!res.ok) throw parseAdminError(res.status, text)
      if (opts.expect === 'none' || res.status === 204) return undefined as T
      if (opts.expect === 'text') return text as T
      if (!text.trim()) return undefined as T
      try {
        return JSON.parse(text) as T
      } catch (e) {
        throw new AdminError('InvalidResponse', `Invalid JSON from ${path}`, res.status, e)
      }
    })
  }

  // Identity ---------------------------------------------------------------

  whoami(signal?: AbortSignal): Promise<WhoAmI> {
    return this.request('GET', '/whoami', { signal })
  }

  async listOwnKeys(): Promise<AccessKey[]> {
    const r = await this.request<{ keys: AccessKey[] | null }>('GET', '/whoami/keys')
    return r.keys ?? []
  }

  createOwnKey(description?: string): Promise<AccessKey> {
    return this.request('POST', '/whoami/keys', { body: description ? { description } : {} })
  }

  deleteOwnKey(accessKey: string): Promise<void> {
    return this.request('DELETE', `/whoami/keys/${enc(accessKey)}`, { expect: 'none' })
  }

  // Server -----------------------------------------------------------------

  serverInfo(signal?: AbortSignal): Promise<ServerInfo> {
    return this.request('GET', '/info', { signal })
  }

  stats(signal?: AbortSignal): Promise<Stats> {
    return this.request('GET', '/stats', { signal })
  }

  getConfig(): Promise<Settings> {
    return this.request('GET', '/config')
  }

  /** Server-cached release information; no config permission required. */
  getUpdates(): Promise<UpdateStatus> {
    return this.request('GET', '/updates')
  }

  /** PUT the full settings document; returns the updated settings. */
  putConfig(settings: Settings): Promise<Settings> {
    return this.request('PUT', '/config', { body: settings })
  }

  /** Prometheus metrics text (signed; requires admin:GetStats unless public). */
  metricsText(): Promise<string> {
    return this.request('GET', '/_gos3/metrics', { raw: true, expect: 'text' })
  }

  health(): Promise<HealthStatus> {
    return this.request('GET', '/_gos3/health', { raw: true })
  }

  ready(): Promise<HealthStatus> {
    return this.request('GET', '/_gos3/ready', { raw: true })
  }

  // Users & keys -----------------------------------------------------------

  async listUsers(): Promise<User[]> {
    const r = await this.request<{ users: User[] | null }>('GET', '/users')
    return r.users ?? []
  }

  createUser(req: CreateUserRequest): Promise<CreateUserResponse> {
    return this.request('POST', '/users', { body: req })
  }

  getUser(name: string): Promise<User> {
    return this.request('GET', `/users/${enc(name)}`)
  }

  updateUser(name: string, update: UserUpdate): Promise<User> {
    return this.request('PATCH', `/users/${enc(name)}`, { body: update })
  }

  deleteUser(name: string): Promise<void> {
    return this.request('DELETE', `/users/${enc(name)}`, { expect: 'none' })
  }

  createAccessKey(user: string, req: CreateAccessKeyRequest = {}): Promise<AccessKey> {
    return this.request('POST', `/users/${enc(user)}/keys`, { body: req })
  }

  updateAccessKey(accessKey: string, status: AccessKeyStatus): Promise<void> {
    return this.request('PATCH', `/keys/${enc(accessKey)}`, { body: { status }, expect: 'none' })
  }

  deleteAccessKey(accessKey: string): Promise<void> {
    return this.request('DELETE', `/keys/${enc(accessKey)}`, { expect: 'none' })
  }

  // Policies ---------------------------------------------------------------

  async listPolicies(): Promise<NamedPolicy[]> {
    const r = await this.request<{ policies: NamedPolicy[] | null }>('GET', '/policies')
    return r.policies ?? []
  }

  getPolicy(name: string): Promise<NamedPolicy> {
    return this.request('GET', `/policies/${enc(name)}`)
  }

  putPolicy(name: string, req: PutPolicyRequest): Promise<NamedPolicy> {
    return this.request('PUT', `/policies/${enc(name)}`, { body: req })
  }

  deletePolicy(name: string): Promise<void> {
    return this.request('DELETE', `/policies/${enc(name)}`, { expect: 'none' })
  }

  async simulate(req: SimulateRequest): Promise<Decision> {
    return normalizeDecision(await this.request<unknown>('POST', '/authz/simulate', { body: req }))
  }

  // Buckets ----------------------------------------------------------------

  async listBuckets(opts: { usage?: boolean } = {}): Promise<BucketSummary[]> {
    const r = await this.request<{ buckets: BucketSummary[] | null }>('GET', '/buckets', { query: { usage: opts.usage ? 'true' : undefined } })
    return r.buckets ?? []
  }

  getBucket(name: string): Promise<BucketSummary> {
    return this.request('GET', `/buckets/${enc(name)}`)
  }

  /**
   * force=true deletes all versions, uploads and configs first. Object lock
   * is honoured: 409 when versions are under legal hold or COMPLIANCE
   * retention, or under GOVERNANCE retention unless
   * bypassGovernanceRetention (only sent with force) is set.
   */
  deleteBucket(name: string, opts: { force?: boolean, bypassGovernanceRetention?: boolean } = {}): Promise<void> {
    return this.request('DELETE', `/buckets/${enc(name)}`, {
      query: {
        force: opts.force ? 'true' : undefined,
        bypassGovernanceRetention: opts.force && opts.bypassGovernanceRetention ? 'true' : undefined
      },
      expect: 'none'
    })
  }

  /**
   * Forgets a native bucket of an s3 backing without touching the remote
   * bucket or its objects (204). 409 while it has replicas or a move in
   * progress; 400 for buckets that are not native.
   */
  releaseBucket(name: string): Promise<void> {
    return this.request('POST', `/buckets/${enc(name)}/release`, { expect: 'none' })
  }

  // Storage backings & bucket placement ------------------------------------

  getBackingConfiguration(): Promise<BackingConfiguration> {
    return this.request('GET', '/backings/config')
  }

  putBackingConfiguration(body: BackingConfiguration): Promise<BackingConfiguration> {
    return this.request('PUT', '/backings/config', { body })
  }

  /** GET /backings (admin:ListBackings); `backings` is never null. */
  async listBackings(signal?: AbortSignal): Promise<BackingList> {
    const r = await this.request<BackingList>('GET', '/backings', { signal })
    return { ...r, backings: r.backings ?? [] }
  }

  /** GET /buckets/{name}/placement (admin:GetBucketPlacement). */
  getBucketPlacement(name: string, signal?: AbortSignal): Promise<Placement> {
    return this.request('GET', `/buckets/${enc(name)}/placement`, { signal })
  }

  /**
   * Sets the replication mode and the replica backings (the primary stays).
   * `single` needs an empty `replicas`, the other modes at least one. New
   * replicas start a full copy; removed replicas lose their copy.
   */
  updateBucketPlacement(name: string, mode: ReplicationMode, replicas: string[]): Promise<Placement> {
    return this.request('PUT', `/buckets/${enc(name)}/placement`, { body: { mode, replicas } })
  }

  /**
   * Moves the copy on `from` (default: the primary) to `to`, online (202).
   * `container` chooses the container of the copy on an s3 `to` backing
   * (default: that backing's default container).
   */
  moveBucket(name: string, to: string, from?: string, container?: string): Promise<Placement> {
    const body: MoveBucketRequest = { to }
    if (from) body.from = from
    if (container) body.container = container
    return this.request('POST', `/buckets/${enc(name)}/placement/move`, { body })
  }

  /** Cancels the move in progress (409 once its cutover started). */
  cancelBucketMove(name: string): Promise<Placement> {
    return this.request('DELETE', `/buckets/${enc(name)}/placement/move`)
  }

  /**
   * Makes a replica the primary. `force` switches immediately, losing the
   * changes not yet copied to the replica.
   */
  promoteReplica(name: string, backing: string, force = false): Promise<Placement> {
    return this.request('POST', `/buckets/${enc(name)}/placement/promote`, { body: force ? { backing, force: true } : { backing } })
  }

  /** Compares the replica (default: every replica) with the primary and copies every difference (202). */
  resyncBucket(name: string, backing?: string): Promise<Placement> {
    return this.request('POST', `/buckets/${enc(name)}/placement/resync`, { body: backing ? { backing } : {} })
  }

  /** The bucket's compression setting, its effect and the server's policy. */
  getBucketCompression(name: string): Promise<BucketCompression> {
    return this.request('GET', `/buckets/${enc(name)}/compression`)
  }

  /**
   * Sets the bucket's compression. 409 Conflict when it contradicts the
   * administrator's compression.mode (enabling while "disabled",
   * disabling while "enforced").
   */
  putBucketCompression(name: string, enabled: boolean): Promise<BucketCompression> {
    return this.request('PUT', `/buckets/${enc(name)}/compression`, { body: { enabled } })
  }

  /** Removes the bucket's setting: back to the server default (204, idempotent). */
  deleteBucketCompression(name: string): Promise<void> {
    return this.request('DELETE', `/buckets/${enc(name)}/compression`, { expect: 'none' })
  }

  getBucketEncryptionProvider(name: string): Promise<BucketEncryptionProvider> {
    return this.request('GET', `/buckets/${enc(name)}/encryption-provider`)
  }

  /**
   * Sets the bucket's encryption provider; `scope: "all"` (restic only)
   * archives every write in the bucket's restic repository. The server
   * prepares the keys or repository first (400 and no change on failure).
   */
  putBucketEncryptionProvider(name: string, body: BucketEncryptionProviderInput): Promise<BucketEncryptionProvider> {
    return this.request('PUT', `/buckets/${enc(name)}/encryption-provider`, { body, timeoutMs: REMOTE_TIMEOUT_MS })
  }

  // Restic: recovery information, credentials, whole-bucket snapshots ------

  /** Where the bucket's restic data lives and how to recover it without gos3 (no secrets; admin:GetBucketEncryption). */
  getBucketRestic(name: string, signal?: AbortSignal): Promise<ResticInfo> {
    return this.request('GET', `/buckets/${enc(name)}/restic`, { signal })
  }

  /**
   * The repository password and environment for the restic CLI
   * (admin:GetResticCredentials; every disclosure is logged by the server).
   * 404 when the bucket has no repository of that purpose on that backing.
   */
  getResticCredentials(name: string, req: ResticCredentialsRequest = {}): Promise<ResticCredentials> {
    return this.request('POST', `/buckets/${enc(name)}/restic/credentials`, { body: req })
  }

  /** The snapshot schedule, its status, repository and snapshots (admin:GetBucketSnapshots); `snapshots` is never null. */
  async getBucketSnapshots(name: string, signal?: AbortSignal): Promise<BucketSnapshots> {
    // Listing runs `restic snapshots`, which may reach a remote repository.
    const r = await this.request<BucketSnapshots>('GET', `/buckets/${enc(name)}/restic/snapshots`, { signal, timeoutMs: REMOTE_TIMEOUT_MS })
    return { ...r, snapshots: r.snapshots ?? [] }
  }

  /** Saves the snapshot schedule (validated; the repository is prepared). Returns it with the generated repo name. */
  putSnapshotConfig(name: string, config: SnapshotConfig): Promise<SnapshotConfig> {
    return this.request('PUT', `/buckets/${enc(name)}/restic/snapshots/config`, { body: config, timeoutMs: REMOTE_TIMEOUT_MS })
  }

  /** Stops scheduling snapshots (existing snapshots stay; 204). */
  deleteSnapshotConfig(name: string): Promise<void> {
    return this.request('DELETE', `/buckets/${enc(name)}/restic/snapshots/config`, { expect: 'none' })
  }

  /** Takes a whole-bucket snapshot now: 202 with the restic-snapshot job (409 while one runs). */
  runBucketSnapshot(name: string): Promise<Job> {
    return this.request('POST', `/buckets/${enc(name)}/restic/snapshots`)
  }

  /** Forgets one whole-bucket snapshot of this bucket (never a per-version archive snapshot; 204). */
  forgetSnapshot(name: string, id: string): Promise<void> {
    return this.request('DELETE', `/buckets/${enc(name)}/restic/snapshots/${enc(id)}`, { expect: 'none', timeoutMs: REMOTE_TIMEOUT_MS })
  }

  /** Restores a snapshot into a bucket: 202 with the restic-restore job. */
  restoreSnapshot(name: string, id: string, req: RestoreSnapshotRequest): Promise<Job> {
    return this.request('POST', `/buckets/${enc(name)}/restic/snapshots/${enc(id)}/restore`, { body: req, timeoutMs: REMOTE_TIMEOUT_MS })
  }

  /** Removes data no snapshot references: 202 with the restic-prune job (archive writes wait for its lock). */
  pruneBucketSnapshots(name: string): Promise<Job> {
    return this.request('POST', `/buckets/${enc(name)}/restic/prune`)
  }

  // Jobs: copy, mirror and restic jobs --------------------------------------

  /** Jobs, newest first (admin:ListJobs); `bucket` matches the source or target. Never null. */
  async listJobs(opts: ListJobsOptions = {}): Promise<Job[]> {
    const r = await this.request<{ jobs: Job[] | null }>('GET', '/jobs', {
      query: { kind: opts.kind || undefined, state: opts.state || undefined, bucket: opts.bucket || undefined },
      signal: opts.signal
    })
    return r.jobs ?? []
  }

  /** The jobs whose source or target is the bucket (admin:GetJob on the bucket). Never null. */
  async listBucketJobs(name: string, signal?: AbortSignal): Promise<Job[]> {
    const r = await this.request<{ jobs: Job[] | null }>('GET', `/buckets/${enc(name)}/jobs`, { signal })
    return r.jobs ?? []
  }

  getJob(id: string, signal?: AbortSignal): Promise<Job> {
    return this.request('GET', `/jobs/${enc(id)}`, { signal })
  }

  /**
   * Creates a copy or mirror job (admin:CreateCopyJob on both buckets): 201
   * with the job. The server validates the spec and may create the target.
   */
  createCopyJob(spec: CopyJobSpec): Promise<Job> {
    const { dryRun: _dryRun, ...body } = spec
    return this.request('POST', '/jobs', { body, timeoutMs: REMOTE_TIMEOUT_MS })
  }

  /** Validates and normalizes a copy or mirror job without creating anything; `warnings` is never null. */
  async dryRunCopyJob(spec: CopyJobSpec): Promise<CopyJobDryRun> {
    const r = await this.request<CopyJobDryRun>('POST', '/jobs', { body: { ...spec, dryRun: true }, timeoutMs: REMOTE_TIMEOUT_MS })
    return { spec: r?.spec ?? spec, warnings: r?.warnings ?? [] }
  }

  /**
   * pause, resume (also retries a failed job from its checkpoint), cancel,
   * or run (starts a waiting job now). 409 when the job's state does not
   * allow it.
   */
  jobAction(id: string, action: JobAction): Promise<Job> {
    return this.request('POST', `/jobs/${enc(id)}/${action}`)
  }

  /** Deletes a finished job (204; 409 unless succeeded, failed or canceled). */
  deleteJob(id: string): Promise<void> {
    return this.request('DELETE', `/jobs/${enc(id)}`, { expect: 'none' })
  }

  // Remote buckets and containers (s3 backings) ----------------------------

  /**
   * One page of the buckets of an s3 backing's account
   * (admin:ListRemoteBuckets); `buckets` is never null. With `probe`, every
   * unmanaged bucket is checked for gos3 data (one request each).
   */
  async listRemoteBuckets(backing: string, opts: ListRemoteBucketsOptions = {}): Promise<RemoteBucketList> {
    const r = await this.request<RemoteBucketList>('GET', `/backings/${enc(backing)}/remote-buckets`, {
      query: {
        prefix: opts.prefix || undefined,
        continuationToken: opts.continuationToken || undefined,
        maxBuckets: opts.maxBuckets,
        probe: opts.probe ? 'true' : undefined
      },
      signal: opts.signal,
      // Probing checks every unmanaged bucket of the page.
      timeoutMs: opts.probe ? REMOTE_TIMEOUT_MS : undefined
    })
    return { ...r, buckets: r.buckets ?? [] }
  }

  /** Creates an unmanaged remote bucket (admin:CreateRemoteBucket); 409 when it exists. */
  createRemoteBucket(backing: string, req: CreateRemoteBucketRequest): Promise<RemoteBucket> {
    return this.request('POST', `/backings/${enc(backing)}/remote-buckets`, { body: req })
  }

  /** The containers of an s3 backing's wrapped buckets, the home first (admin:ListRemoteBuckets). */
  async listContainers(backing: string, signal?: AbortSignal): Promise<ContainerInfo[]> {
    const r = await this.request<{ containers: ContainerInfo[] | null }>('GET', `/backings/${enc(backing)}/containers`, { signal })
    return r.containers ?? []
  }

  /**
   * Adds a container (admin:ManageContainers, plus CreateRemoteBucket with
   * `createBucket` or AdoptRemoteBucket without). Resolves once it is
   * loaded; the server waits up to 30 s (503 after that).
   */
  addContainer(backing: string, spec: ContainerSpec): Promise<ContainerInfo> {
    return this.request('POST', `/backings/${enc(backing)}/containers`, { body: spec, timeoutMs: REMOTE_TIMEOUT_MS })
  }

  /** Makes `id` the default container of new wrapped buckets; returns every container. */
  async setDefaultContainer(backing: string, id: string): Promise<ContainerInfo[]> {
    const r = await this.request<{ containers: ContainerInfo[] | null }>('PUT', `/backings/${enc(backing)}/containers/default`, { body: { id } })
    return r.containers ?? []
  }

  /** Forgets an empty container, leaving its remote data alone (204; 409 while it holds buckets, 400 for the home). */
  removeContainer(backing: string, id: string): Promise<void> {
    return this.request('DELETE', `/backings/${enc(backing)}/containers/${enc(id)}`, { expect: 'none' })
  }

  /**
   * Creates a bucket on an s3 backing through the admin API: adopts an
   * existing remote bucket (`adopt`), creates a native bucket with a new
   * remote bucket, or a wrapped bucket (in `container` or a `newContainer`).
   * The caller becomes the owner.
   */
  createBucketOnBacking(backing: string, req: CreateOnBacking): Promise<BucketSummary> {
    return this.request('POST', `/backings/${enc(backing)}/buckets`, { body: req, timeoutMs: REMOTE_TIMEOUT_MS })
  }

  // Discovery --------------------------------------------------------------

  discovery(signal?: AbortSignal): Promise<DiscoveryState> {
    return this.request('GET', '/discovery', { signal })
  }

  probe(url: string): Promise<Peer> {
    return this.request('POST', '/discovery/probe', { body: { url } })
  }

  scan(req: ScanRequest, signal?: AbortSignal): Promise<ScanResult> {
    return this.request('POST', '/discovery/scan', { body: req, signal, timeoutMs: 300000 })
  }
}
