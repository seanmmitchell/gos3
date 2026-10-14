// Types mirroring the gos3 admin REST API (docs/admin-api.md) and the public
// discovery identity document (docs/discovery.md). Field names follow the Go
// JSON tags exactly.
//
// Conventions:
// - Timestamps are RFC3339 strings.
// - Go `time.Duration` fields are integers in **nanoseconds** (see `Duration`).

/** A Go time.Duration serialised as integer nanoseconds. */
export type Duration = number

/** RFC3339 timestamp string. */
export type Timestamp = string

// ---------------------------------------------------------------------------
// Discovery identity (public GET /_gos3/info)
// ---------------------------------------------------------------------------

export interface Endpoints {
  /** e.g. "http://10.0.0.5:9000" */
  s3: string
  /** e.g. "http://10.0.0.5:9000/_gos3/admin/v1" */
  admin: string
  /** e.g. "http://10.0.0.5:9000/_gos3/ui/" (omitted/"" when disabled) */
  ui?: string
  /** Every advertised base URL (multi-homed hosts). */
  all?: string[]
}

/** Public identity document served at GET /_gos3/info. */
export interface Info {
  /** Always "gos3". */
  service: string
  /** Stable server ID (16 hex chars). */
  id: string
  name: string
  version: string
  /** Admin API version, "v1". */
  apiVersion: string
  region: string
  endpoints: Endpoints
  /** e.g. "versioning", "object-lock", "multipart", "admin", "ui", "discovery", "lifecycle" */
  features: string[]
  startedAt: Timestamp
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

/** Capabilities derived by the server from the caller's admin:* permissions. */
export interface Capabilities {
  /** any admin:* allowed */
  admin: boolean
  /** admin:ServerInfo */
  viewInfo: boolean
  /** admin:GetStats */
  viewStats: boolean
  /** admin:UpdateConfig */
  manageConfig: boolean
  /** admin:CreateUser */
  manageUsers: boolean
  /**
   * admin:ListUsers. True for delegated user managers whose other user
   * actions are scoped to some users (and so do not show in manageUsers).
   * Absent from older servers: use canListUsers(), which falls back to
   * manageUsers.
   */
  listUsers?: boolean
  /** admin:PutPolicy */
  managePolicies: boolean
  /** admin:ForceDeleteBucket */
  manageBuckets: boolean
  /**
   * admin:ListBackings (server Storage page, bucket Placement tab). Absent
   * from servers without storage backings: treated as false.
   */
  viewStorage?: boolean
  /**
   * admin:UpdateBucketPlacement (change replication, move, promote, resync).
   * Absent from servers without storage backings: treated as false.
   */
  manageStorage?: boolean
  /** admin:GetDiscovery */
  discovery: boolean
  /** admin:ScanNetwork */
  scan: boolean
  /**
   * admin:ProbeNetwork (POST /discovery/probe). Absent from older servers,
   * which gated probes on admin:GetDiscovery: use canProbe()
   * (~/components/admin/-capabilities), which falls back to `discovery`.
   */
  probe?: boolean
  /**
   * admin:CreateOwnAccessKey explicitly allowed (POST /whoami/keys); always
   * false for root. Absent from older servers, which allowed it unless
   * explicitly denied: use canCreateOwnKeys(), which then assumes allowed.
   */
  createOwnKeys?: boolean
  /**
   * admin:PutBucketCompression allowed at service level (bucket
   * compression settings). Absent from servers without compression: use
   * bucketCompressionAccess() (~/components/admin/-capabilities).
   */
  manageCompression?: boolean
}

export type CapabilityName = keyof Capabilities

export interface WhoAmI {
  user: string
  accessKey: string
  isRoot: boolean
  policies: string[]
  capabilities: Capabilities
  server: Info
}

// ---------------------------------------------------------------------------
// Server info & stats
// ---------------------------------------------------------------------------

export interface ServerInfo extends Info {
  /** "fs" | "memory" */
  backend: string
  dataPath: string
  goVersion: string
  os: string
  arch: string
  numCPU: number
  uptimeSeconds: number
}

/** metrics.OpStats — aggregate of one API operation (e.g. "PutObject"). */
export interface OpStats {
  op: string
  requests: number
  errors4xx: number
  errors5xx: number
  bytesIn: number
  bytesOut: number
  avgLatencyMs: number
  p50LatencyMs: number
  p99LatencyMs: number
}

/** metrics.MinuteBucket — one minute of traffic. */
export interface MinuteBucket {
  minute: Timestamp
  requests: number
  errors: number
  bytesIn: number
  bytesOut: number
}

/** metrics.Snapshot — point-in-time view of all request metrics. */
export interface MetricsSnapshot {
  startedAt: Timestamp
  uptimeSeconds: number
  requests: number
  errors4xx: number
  errors5xx: number
  bytesIn: number
  bytesOut: number
  inFlight: number
  /** Sorted by op. May be null when empty. */
  ops: OpStats[] | null
  /** Per-minute counts for the last 60 minutes, oldest first. May be null. */
  recent: MinuteBucket[] | null
}

export interface StorageStats {
  buckets: number
  objects: number
  bytes: number
  versions: number
  versionBytes: number
  deleteMarkers: number
  pendingUploads: number
  /**
   * Physical bytes stored for current versions (compressed data counts
   * its compressed size); `bytes` is the logical size. Absent from servers
   * without compression.
   */
  storedBytes?: number
  /** Physical bytes stored for all versions (see storedBytes). */
  storedVersionBytes?: number
  /** Versions whose data is stored compressed. */
  compressedVersions?: number
  /** Cached up to 30 s. */
  computedAt: Timestamp
}

export interface RuntimeStats {
  goroutines: number
  heapAllocBytes: number
  sysBytes: number
  gcPauseTotalNs: number
}

export interface DiskStats {
  path: string
  totalBytes: number
  freeBytes: number
  usedBytes: number
}

export interface Stats {
  metrics: MetricsSnapshot
  storage: StorageStats
  runtime: RuntimeStats
  /** fs backend only, else null. */
  disk: DiskStats | null
}

// ---------------------------------------------------------------------------
// Runtime settings (config.Settings)
// ---------------------------------------------------------------------------

export interface CORSSettings {
  allowedOrigins: string[] | null
  allowedHeaders: string[] | null
  exposeHeaders: string[] | null
  maxAgeSeconds: number
}

/** discovery.Config */
export interface DiscoveryConfig {
  enabled: boolean
  advertise: boolean
  mdns: boolean
  beacon: boolean
  /** UDP multicast group:port, default "239.255.73.83:7373". */
  beaconGroup: string
  /** nanoseconds */
  beaconInterval: Duration
  seeds: string[] | null
  dnsSrv: string[] | null
  /** nanoseconds */
  peerTtl: Duration
  allowScan: boolean
  maxScanHosts: number
}

export type LogLevel = 'debug' | 'info' | 'warn' | 'error'
export type LogFormat = 'text' | 'json'

export interface LoggingSettings {
  level: LogLevel | string
  format: LogFormat | string
  accessLog: boolean
}

export interface LifecycleSettings {
  enabled: boolean
  intervalSeconds: number
}

/**
 * Server-wide compression policy set by the administrator:
 * - optional: each bucket decides (off unless enabled for the bucket)
 * - enforced: every bucket compresses new objects; buckets cannot opt out
 * - disabled: nothing new is compressed and buckets cannot enable it
 *   (objects already stored compressed stay readable)
 */
export type CompressionMode = 'optional' | 'enforced' | 'disabled'

/** config.CompressionSettings */
export interface CompressionSettings {
  mode: CompressionMode | string
  /** DEFLATE level, 1 (fastest, default) to 9 (smallest). */
  level: number
  /** Objects smaller than this (when the size is known up front) stay uncompressed. */
  minSize: number
  /**
   * Media types stored uncompressed (already compressed): "type/subtype",
   * "type/*" or a prefix ending in "*". Case-insensitive.
   */
  skipContentTypes: string[] | null
}

/** config.Settings — GET/PUT /config. PUT takes the full document. */
export interface Settings {
  name: string
  region: string
  publicUrl: string
  domains: string[] | null
  publicInfo: boolean
  uiRedirect: boolean
  cors: CORSSettings
  discovery: DiscoveryConfig
  logging: LoggingSettings
  lifecycle: LifecycleSettings
  metricsPublic: boolean
  /** Absent from servers without compression support. */
  compression?: CompressionSettings
}

// ---------------------------------------------------------------------------
// IAM: users, access keys, policies
// ---------------------------------------------------------------------------

export type AccessKeyStatus = 'Active' | 'Inactive'

export interface AccessKey {
  accessKey: string
  /** Only populated in the response that created the key. */
  secretKey?: string
  status: AccessKeyStatus
  description?: string
  createdAt: Timestamp
}

export interface User {
  name: string
  description?: string
  disabled: boolean
  policies: string[] | null
  /** secretKey is always empty here. */
  keys: AccessKey[] | null
  createdAt: Timestamp
  updatedAt: Timestamp
}

/** iam.UserSpec (POST /users body). */
export interface UserSpec {
  name: string
  description?: string
  policies: string[]
  disabled: boolean
}

/** POST /users body: UserSpec plus optional createKey. */
export interface CreateUserRequest extends UserSpec {
  createKey?: boolean
}

export interface CreateUserResponse {
  user: User
  key?: AccessKey
}

/** iam.UserUpdate (PATCH /users/{name}); omitted fields are unchanged. */
export interface UserUpdate {
  description?: string
  policies?: string[]
  disabled?: boolean
}

export interface CreateAccessKeyRequest {
  description?: string
  /** Optional caller-supplied key pair (empty = generated). */
  accessKey?: string
  secretKey?: string
}

/** A JSON value that may be a single string or an array of strings. */
export type StringOrSlice = string | string[]

/** Policy principal: "*" or {"AWS": "..." | [...]}. */
export type Principal = '*' | { AWS: StringOrSlice }

export type PolicyEffect = 'Allow' | 'Deny'

/** Condition block: operator → condition key → value(s). */
export type PolicyCondition = Record<string, Record<string, StringOrSlice>>

export interface PolicyStatement {
  Sid?: string
  Effect: PolicyEffect
  Principal?: Principal
  NotPrincipal?: Principal
  Action?: StringOrSlice
  NotAction?: StringOrSlice
  Resource?: StringOrSlice
  NotResource?: StringOrSlice
  Condition?: PolicyCondition
}

/** Alias kept for symmetry with the Go type name. */
export type Statement = PolicyStatement

export interface PolicyDocument {
  Version?: string
  Id?: string
  Statement: PolicyStatement[]
}

export interface NamedPolicy {
  name: string
  description?: string
  builtin: boolean
  document: PolicyDocument
  createdAt: Timestamp
  updatedAt: Timestamp
}

export interface PutPolicyRequest {
  description?: string
  document: PolicyDocument
}

export interface SimulateRequest {
  /** IAM user name; "" = anonymous. */
  user: string
  /** e.g. "s3:GetObject" */
  action: string
  bucket?: string
  key?: string
  /**
   * Admin resource for admin:* actions (arn:gos3:admin:::user/<name>,
   * …:::policy/<name>, …:::bucket/<name>); omitted = service level. Not
   * combined with bucket/key.
   */
  resource?: string
  conditions?: Record<string, string[]>
}

/**
 * iam.Decision. The Go struct has no JSON tags, so the server may send
 * {"Allowed","ExplicitDeny","Reason"}; AdminClient.simulate normalises
 * either casing into this shape.
 */
export interface Decision {
  allowed: boolean
  explicitDeny: boolean
  reason: string
}

// ---------------------------------------------------------------------------
// Buckets (admin helpers)
// ---------------------------------------------------------------------------

export interface BucketConfigs {
  policy: boolean
  cors: boolean
  lifecycle: boolean
  tagging: boolean
  encryption: boolean
  website: boolean
  objectLock: boolean
  /** Canned ACL, e.g. "private". */
  acl: string
}

/** storage.BucketUsage */
export interface BucketUsage {
  objects: number
  bytes: number
  versions: number
  versionBytes: number
  deleteMarkers: number
  pendingUploads: number
  /** Physical bytes of current versions (see StorageStats.storedBytes). */
  storedBytes?: number
  /** Physical bytes of all versions. */
  storedVersionBytes?: number
  /** Versions whose data is stored compressed. */
  compressedVersions?: number
  /** When the server computed these figures (admin BucketSummary only; cached up to 30 s). */
  computedAt?: Timestamp
}

/** The "compression" member of a BucketSummary. */
export interface BucketCompressionSummary {
  /** The bucket's own setting. */
  enabled: boolean
  /** Whether new objects are compressed under the server's policy. */
  effective: boolean
}

/** GET/PUT /buckets/{name}/compression */
export interface BucketCompression extends BucketCompressionSummary {
  /** The administrator's compression.mode. */
  serverMode: CompressionMode | string
  /** e.g. "deflate" */
  algorithm: string
  level: number
  minSize: number
}

export interface BucketSummary {
  name: string
  createdAt: Timestamp
  region: string
  versioning: 'Enabled' | 'Suspended' | ''
  objectLock: boolean
  owner: string
  configs: BucketConfigs
  /** Bucket policy grants anonymous access or ACL public-*. */
  public: boolean
  usage: BucketUsage | null
  /** Absent from servers without compression support. */
  compression?: BucketCompressionSummary
  /** Where the bucket is stored (servers with storage backings only). */
  placement?: PlacementSummary
}

// ---------------------------------------------------------------------------
// Storage backings & bucket placement
// ---------------------------------------------------------------------------

/** "fs" | "memory" */
export type BackingKind = 'fs' | 'memory'

/** A storage backend of the server (GET /backings). */
export interface Backing {
  /** "default" is the backing configured by storage.backend / storage.path. */
  name: string
  kind: BackingKind | string
  /** "" for memory. */
  path: string
  /** New buckets are created here. */
  default: boolean
  /** The last health check or operation succeeded. */
  healthy: boolean
  /** Last error that marked it unhealthy ("" when healthy). */
  lastError: string
  /** Last health check (omitted before the first). */
  checkedAt?: Timestamp
  /** Buckets whose primary copy is here. */
  buckets: number
  /** Replica copies (and move targets) of other buckets here. */
  replicas: number
  /** fs only, else null. */
  disk: DiskStats | null
}

/** GET /backings */
export interface BackingList {
  defaultBacking: string
  autoFailover: boolean
  backings: Backing[] | null
}

export type ReplicationMode = 'single' | 'active-passive' | 'active-active'

export type PlacementRole = 'primary' | 'replica' | 'move-target'

/**
 * - in-sync:  receives every write synchronously, has no pending changes, or is the primary
 * - syncing:  initial or requested full copy running
 * - lagging:  changes are pending, copied in the background
 * - degraded: copying failed; retried with back-off (see lastError)
 * - offline:  the backing is unhealthy
 */
export type MemberState = 'in-sync' | 'syncing' | 'lagging' | 'degraded' | 'offline'

/** Progress of a running full copy. */
export interface CopyProgress {
  keys: number
  versions: number
  bytes: number
}

export interface PlacementMember {
  backing: string
  role: PlacementRole
  state: MemberState
  /** The backing's health. */
  healthy: boolean
  /** Changed keys not yet copied to this member. */
  pending: number
  /** Running full copy, else null. */
  progress: CopyProgress | null
  /** Last completed full copy (omitted when none). */
  lastSyncAt?: Timestamp
  lastError: string
}

export type MovePhase = 'copying' | 'cutover'

export interface PlacementMove {
  from: string
  to: string
  startedAt: Timestamp
  phase: MovePhase
}

/** GET /buckets/{name}/placement */
export interface Placement {
  bucket: string
  mode: ReplicationMode
  primary: string
  /** Primary first, then replicas, then a move target. */
  members: PlacementMember[] | null
  move: PlacementMove | null
}

/** PUT /buckets/{name}/placement body. `single` needs an empty `replicas`. */
export interface UpdatePlacementRequest {
  mode: ReplicationMode
  replicas: string[]
}

/** POST /buckets/{name}/placement/move body (`from` defaults to the primary). */
export interface MoveBucketRequest {
  to: string
  from?: string
}

/** POST /buckets/{name}/placement/promote body. */
export interface PromoteReplicaRequest {
  backing: string
  force?: boolean
}

/** Placement as summarised in BucketSummary. */
export interface PlacementSummary {
  mode: ReplicationMode
  primary: string
  replicas: string[] | null
  moving: boolean
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

export type DiscoverySource = 'mdns' | 'beacon' | 'static' | 'dns' | 'scan' | 'probe'

/** discovery.Peer — a discovered gos3 server. */
export interface Peer {
  id: string
  name: string
  version: string
  region: string
  endpoints: Endpoints
  features?: string[]
  sources: DiscoverySource[]
  /** Base URLs this peer was observed at. */
  addresses: string[]
  firstSeen: Timestamp
  lastSeen: Timestamp
  healthy: boolean
  verified: boolean
  error?: string
}

/** GET /discovery */
export interface DiscoveryState {
  enabled: boolean
  config: DiscoveryConfig
  self: Info
  peers: Peer[] | null
}

/** discovery.ScanRequest */
export interface ScanRequest {
  /** CIDRs ("10.0.0.0/24"), hosts ("nas.local") or base URLs. */
  targets: string[]
  /** Default [9000]. */
  ports?: number[]
  /** Default ["http"]. */
  schemes?: string[]
  /** Per-probe timeout in nanoseconds (default 1.5 s). */
  timeout?: Duration
  /** Default 64. */
  concurrency?: number
}

/** discovery.ScanResult */
export interface ScanResult {
  found: Peer[] | null
  probed: number
  /** Go duration string, e.g. "1.234s". */
  elapsed: string
}

// ---------------------------------------------------------------------------
// Errors & misc
// ---------------------------------------------------------------------------

export type AdminErrorCode =
  | 'InvalidRequest'
  | 'InvalidName'
  | 'InvalidPolicy'
  | 'InvalidSettings'
  | 'Unauthenticated'
  | 'AccessDenied'
  | 'NotFound'
  | 'MethodNotAllowed'
  | 'Conflict'
  | 'InternalError'
  | 'ProbeFailed'
  | 'NotImplemented'
  // client-side codes
  | 'NetworkError'
  | 'Timeout'
  | 'Aborted'
  | 'InvalidResponse'
  | 'NotGos3'

/** Error envelope of every non-2xx admin response. */
export interface AdminErrorBody {
  error: { code: string, message: string }
}

export interface HealthStatus {
  status: 'ok' | 'unavailable' | string
  error?: string
}

/** Per-bucket implementation of SSE-S3, with storage inherited from placement. */
export interface BucketEncryptionProvider {
 provider: 'AES256' | 'restic'
 backings?: string[]
 ready?: boolean
 error?: string
}
export interface BucketEncryptionProviderInput { provider: 'AES256' | 'restic' }
export interface ResticBacking {
 mode: 'local' | 's3'
 endpoint: string
 bucket: string
 prefix: string
 region: string
 credentialsConfigured: boolean
 sessionTokenConfigured: boolean
 locked: boolean
}
export interface ResticBackingInput {
 mode: 'local' | 's3'
 endpoint?: string
 bucket?: string
 prefix?: string
 region?: string
 accessKey?: string
 secretKey?: string
 sessionToken?: string
 clearSessionToken?: boolean
}
