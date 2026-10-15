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
  /**
   * admin:ListRemoteBuckets at service level: browse the buckets of an s3
   * backing's account and list its containers. Absent from servers without
   * s3 accounts: treated as false.
   */
  browseRemote?: boolean
  /** admin:CreateRemoteBucket: create buckets in an s3 backing's account. */
  createRemote?: boolean
  /**
   * admin:AdoptRemoteBucket: adopt an existing remote bucket as a native
   * gos3 bucket, or use one as a container.
   */
  adoptRemote?: boolean
  /** admin:ManageContainers: add, remove and choose the default container of wrapped buckets. */
  manageContainers?: boolean
}

export type CapabilityName = keyof Capabilities

export interface WhoAmI {
  user: string
  accessKey: string
  isRoot: boolean
  policies: string[]
  capabilities: Capabilities
  server: Info
  /**
   * Storage backings new buckets can be created on (every authenticated
   * caller; CreateBucket header x-gos3-backing). Absent from older servers;
   * `available` is empty on servers without storage backings.
   */
  backings?: AvailableBackings
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
  /** s3 backings only: how the bucket is stored (omitted on other backings). */
  kind?: BucketKind | string
  /**
   * s3 backings only: the remote bucket of a native bucket or the container
   * of a wrapped one, e.g. "s3://acme-photos/".
   */
  location?: string
}

// ---------------------------------------------------------------------------
// Storage backings & bucket placement
// ---------------------------------------------------------------------------

/** "fs" | "memory" | "s3" (a connection to a remote S3 service's account) */
export type BackingKind = 'fs' | 'memory' | 's3'

/**
 * How a bucket of an s3 backing is stored:
 * - native:  objects stored as-is in a remote bucket of its own, readable by
 *            other S3 tools; versioning and object lock are those of the
 *            remote bucket
 * - wrapped: gos3's own format inside a container (a remote bucket or a
 *            prefix of one, shared by many wrapped buckets); every gos3
 *            feature works
 */
export type BucketKind = 'native' | 'wrapped'

/** A container as offered to bucket creators (WhoAmI.backings.available[].containers). */
export interface ContainerChoice {
  /** "home" for the backing's shared container. */
  id: string
  /** Set by administrators; shown to every caller. */
  label?: string
  /** The container wrapped buckets get when none is chosen. */
  default?: boolean
}

/** A backing new buckets can be created on (WhoAmI.backings.available). */
export interface AvailableBacking {
  name: string
  kind: BackingKind | string
  /** Kind "s3" only: the bucket kinds offered (x-gos3-bucket-kind). */
  kinds?: (BucketKind | string)[] | null
  /** Kind "s3" only: the kind of buckets created without x-gos3-bucket-kind. */
  defaultKind?: BucketKind | string
  /** Kind "s3" only: the containers of wrapped buckets (x-gos3-container). */
  containers?: ContainerChoice[] | null
}

/** WhoAmI.backings */
export interface AvailableBackings {
  /** Backing used when CreateBucket names none. */
  default: string
  available: AvailableBacking[] | null
}

/** A storage backend of the server (GET /backings). */
export interface Backing {
  /** "default" is the backing configured by storage.backend / storage.path. */
  name: string
  kind: BackingKind | string
  /** "" for memory and s3. */
  path: string
  /**
   * Kind "s3" only: the home where gos3 keeps the backing's records, e.g.
   * "s3://bucket/prefix/" (no endpoint, no secrets), or "local:" when the
   * records are kept in the server's system directory.
   */
  location?: string
  /** Kind "s3" only: the bucket kinds offered. */
  kinds?: (BucketKind | string)[] | null
  /** Kind "s3" only: the kind of new buckets without an explicit kind. */
  defaultKind?: BucketKind | string
  /** Kind "s3" only: the account (home, containers, bucket counts). */
  account?: AccountInfo
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

/** Backend of a configured backing (GET/PUT /backings/config). */
export type BackingBackend = 'fs' | 'memory' | 's3'

/** S3 addressing style: '' = automatic. */
export type S3Addressing = '' | 'path' | 'virtual'

/**
 * Connection and home of an s3 backing (BackingDefinition.s3). Secrets
 * are write-only: GET reports whether they are set (and whether they come
 * from the environment); a blank value on PUT keeps the stored one.
 *
 * The home bucket and prefix are the backing's identity: changing them
 * replaces the backing. The connection fields and the default kind can be
 * changed while it holds buckets.
 */
export interface S3BackingDefinition {
  /** Service URL, http(s) without a path. */
  endpoint: string
  region: string
  /**
   * Where gos3 keeps the backing's records and its shared container of
   * wrapped buckets; "" = records in the server's system directory.
   */
  homeBucket: string
  /** Key prefix inside the home bucket ("" = the whole bucket); needs homeBucket. */
  homePrefix: string
  /** Kind of buckets created without one: "wrapped" (needs a home), "native", or "" (wrapped with a home, else native). */
  defaultKind: BucketKind | ''
  addressing: S3Addressing
  /** Multipart part size in bytes (0 / absent = server default). */
  partSize?: number
  accessKey: string
  /** Write-only; blank keeps the stored secret. */
  secretKey?: string
  /** Write-only, base64 of 32 bytes; blank keeps the stored key. */
  encryptionKey?: string
  /** Read-only: a secret key is stored or set in the environment. */
  secretKeyConfigured?: boolean
  /** Read-only: an encryption key is stored or set in the environment. */
  encryptionKeyConfigured?: boolean
  /** Read-only: "env" when the secret key comes from the environment. */
  secretKeySource?: 'env' | string
  /** Read-only: "env" when the encryption key comes from the environment. */
  encryptionKeySource?: 'env' | string
  /** Read-only: "env" when the access key comes from the environment. */
  accessKeySource?: 'env' | string
  /** Read-only: the fields filled from GOS3_BACKING_<NAME>_* (config key names, e.g. "secret_key"). */
  envFields?: string[] | null
}

/** One configured backing (GET/PUT /backings/config). */
export interface BackingDefinition {
  name: string
  backend: BackingBackend
  /** fs only ("" otherwise). */
  path: string
  /** s3 only. */
  s3?: S3BackingDefinition
}

/** GET/PUT /backings/config */
export interface BackingConfiguration {
  backings: BackingDefinition[]
  defaultBacking: string
  revision: string
  persistent: boolean
}

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

/** Where a move puts the copy on an s3 backing (when it chose a container). */
export interface MoveTarget {
  container?: string
}

export interface PlacementMove {
  from: string
  to: string
  startedAt: Timestamp
  phase: MovePhase
  /** Present when the move chose a container on an s3 `to` backing. */
  target?: MoveTarget | null
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

/**
 * POST /buckets/{name}/placement/move body (`from` defaults to the primary;
 * `container` chooses the container of the copy on an s3 `to` backing,
 * default its default container).
 */
export interface MoveBucketRequest {
  to: string
  from?: string
  container?: string
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
// Remote buckets and containers (s3 backings)
// ---------------------------------------------------------------------------

/**
 * What a bucket of an s3 backing's account is to gos3, in priority order:
 * - native:       a native gos3 bucket (see uses)
 * - home:         the backing's home (records and shared container)
 * - container:    holds a container of wrapped buckets
 * - claimed:      used by another backing of this server
 * - gos3:         holds gos3 data (found by the probe)
 * - inaccessible: the credentials cannot read it
 * - unmanaged:    not used by gos3
 */
export type RemoteBucketStatus = 'native' | 'home' | 'container' | 'claimed' | 'gos3' | 'inaccessible' | 'unmanaged'

/** One use of a remote bucket by gos3. */
export interface RemoteBucketUse {
  role: 'native' | 'home' | 'container' | 'claimed' | string
  /** role native: the gos3 bucket. */
  bucket?: string
  /** role native: gos3 created the remote bucket (else it was adopted). */
  created?: boolean
  /** role home / container: the container's ID. */
  containerId?: string
  /** role home / container: the container's prefix in the remote bucket. */
  prefix?: string
  /** role claimed: the other backing using it. */
  backing?: string
}

/** A bucket of an s3 backing's account (GET /backings/{name}/remote-buckets). */
export interface RemoteBucket {
  name: string
  region?: string
  createdAt?: Timestamp
  status: RemoteBucketStatus | string
  uses?: RemoteBucketUse[] | null
  /** Extra detail, e.g. why it is inaccessible. */
  note?: string
}

/** GET /backings/{name}/remote-buckets */
export interface RemoteBucketList {
  buckets: RemoteBucket[] | null
  /**
   * false when the credentials may not list the account's buckets: then only
   * the locations the backing knows are listed (others can be adopted by name).
   */
  complete: boolean
  /** Why the list is incomplete, e.g. "AccessDenied …". */
  listError?: string
  /** Omitted on the last page. */
  continuationToken?: string
}

/** Query of GET /backings/{name}/remote-buckets. */
export interface ListRemoteBucketsOptions {
  prefix?: string
  continuationToken?: string
  /** 1–1000. */
  maxBuckets?: number
  /** Check unmanaged buckets for gos3 data (one request each). */
  probe?: boolean
  signal?: AbortSignal
}

/** POST /backings/{name}/remote-buckets body: a new unmanaged remote bucket. */
export interface CreateRemoteBucketRequest {
  name: string
  region?: string
  objectLock?: boolean
}

/** A remote bucket and prefix. */
export interface RemoteLocation {
  bucket: string
  prefix?: string
}

/**
 * - home:      the backing's home (implicit, ID "home")
 * - dedicated: a remote bucket gos3 created for it
 * - existing:  a prefix of an existing remote bucket
 */
export type ContainerRole = 'home' | 'dedicated' | 'existing'

export type ContainerState = 'loading' | 'ready' | 'failed'

/** A container of wrapped buckets (GET /backings/{name}/containers). */
export interface ContainerInfo {
  /** "home" or a generated ID like "c-7f3a". */
  id: string
  label?: string
  location: RemoteLocation
  region?: string
  role: ContainerRole | string
  /** Wrapped buckets get this container when none is chosen. */
  default?: boolean
  /** Wrapped buckets in it. */
  buckets: number
  state: ContainerState | string
  /** Why it failed ("" otherwise). */
  error?: string
  added?: Timestamp
}

/** POST /backings/{name}/containers body (also CreateOnBacking.newContainer). */
export interface ContainerSpec {
  bucket: string
  prefix?: string
  region?: string
  /** Create the remote bucket (a dedicated container); else it must exist. */
  createBucket?: boolean
  /** Use a location that already holds a gos3 store (e.g. moved from another server). */
  attach?: boolean
  label?: string
}

/**
 * POST /backings/{name}/buckets body: adopt a remote bucket, create a native
 * bucket with a new remote bucket, or create a wrapped bucket.
 */
export interface CreateOnBacking {
  /** gos3 bucket name. */
  bucket: string
  /** Default: the backing's defaultKind. */
  kind?: BucketKind
  /** native: the remote bucket (default: the gos3 name). */
  remoteBucket?: string
  /** native: the remote region (default: discovered / the backing's). */
  region?: string
  /** native: use the existing remote bucket instead of creating one. */
  adopt?: boolean
  /** native: create the remote bucket with object lock (not with adopt). */
  objectLock?: boolean
  /** wrapped: the container (default: the backing's default container). */
  container?: string
  /** wrapped: create or register a container first (not with container). */
  newContainer?: ContainerSpec
}

/** Backing.account of an s3 backing. */
export interface AccountInfo {
  /** null when the records are kept in the server's system directory. */
  home: RemoteLocation | null
  records: 'home' | 'local' | string
  defaultKind: BucketKind | string
  /** "" when there is no container. */
  defaultContainer: string
  kinds: (BucketKind | string)[] | null
  nativeBuckets: number
  wrappedBuckets: number
  /** The home first. */
  containers: ContainerInfo[] | null
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

/** Implementations of SSE-S3 a gos3 bucket can use. */
export type EncryptionProviderName = 'AES256' | 'restic'

/**
 * GET/PUT /buckets/{name}/encryption-provider: the bucket's implementation
 * of SSE-S3 (native AES-256-GCM or restic), used for default encryption and
 * for uploads that request AES256. Restic repositories live on the backing
 * holding each copy of the bucket.
 */
export interface BucketEncryptionProvider {
  provider: EncryptionProviderName
  /**
   * Providers the bucket can use now (restic needs the restic executable on
   * the server and a filesystem or S3 backing for every copy). Absent when the
   * server cannot tell: then every provider is offered.
   */
  available?: EncryptionProviderName[]
  /** Backings holding the bucket (primary first). */
  backings?: string[]
}
export interface BucketEncryptionProviderInput { provider: EncryptionProviderName }
