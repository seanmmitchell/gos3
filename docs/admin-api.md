# gos3 admin API (v1)

Base path: `/_gos3/admin/v1` on the S3 listener. JSON in/out
(`Content-Type: application/json`). Timestamps are RFC3339. Go
`time.Duration` fields (e.g. discovery `beaconInterval`, `peerTtl`, scan
`timeout`) are **integers in nanoseconds**.

## Authentication & authorization

- Requests are signed with AWS SigV4 (Authorization header **or** presigned
  query) using the same access keys as the S3 API. Service name `s3`, any
  region.
- **The body must be signed.** Every `POST`, `PUT` and `PATCH`, and any other
  request that carries a body, must use the Authorization header with
  `X-Amz-Content-Sha256` set to the hex SHA-256 of the exact body bytes (of
  the empty string when there is none), or signed aws-chunked streaming
  (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD[-TRAILER]`). `UNSIGNED-PAYLOAD`,
  `STREAMING-UNSIGNED-PAYLOAD-TRAILER` and presigned URLs are refused for
  them with 400 `InvalidRequest` ("admin API requests with a body must sign
  the payload: …"): their signature does not cover the body, so a captured
  request could be replayed with another body. A body that does not match
  its signed hash is 400 `InvalidRequest` (`XAmzContentSHA256Mismatch: …`).
  Bodiless `GET` and `DELETE` requests may use `UNSIGNED-PAYLOAD` or a
  presigned URL.
- Each endpoint requires an IAM action `admin:<Action>` (listed below),
  checked with `iam.Manager.Authorize` against the endpoint's resource (see
  [Resources and condition keys](#resources-and-condition-keys)). Root is
  always allowed; the built-in `admin` policy allows all; `diagnostics`
  allows `ServerInfo`, `GetStats`, `GetDiscovery`, `ListBackings`,
  `GetBucketPlacement` (not `ProbeNetwork` or
  `ScanNetwork`).
- Endpoints marked *any* only require a valid (non-anonymous) identity.
  `DELETE /whoami/keys/{accessKey}` is refused with 403 `AccessDenied` only
  when an explicit Deny matches `admin:DeleteOwnAccessKey` (a Deny of
  `admin:*` matches too). `POST /whoami/keys` needs an explicit Allow of
  `admin:CreateOwnAccessKey` (only the built-in `admin` policy grants it), so
  a stolen key cannot mint keys that would survive revoking it; a Deny still
  wins over the Allow.
- `aws:SourceIp` / `aws:SecureTransport` conditions and the remote address
  in the admin access log use `X-Forwarded-For` / `X-Forwarded-Proto` only
  when `server.trust_proxy_headers` is on and the peer is in
  `server.trusted_proxies` (or that list is empty), with the same rules as
  the S3 API (see [deployment.md](deployment.md#reverse-proxy)).
- CORS: preflight `OPTIONS` is answered without auth using the global
  `cors` settings; actual responses carry the matching CORS headers.
- Request bodies are limited to 1 MiB and decoded strictly: unknown fields and
  trailing data are `400 InvalidRequest` (`InvalidSettings` on `PUT /config`).
  A body that sends no data for 60 s fails; a response sent before the body
  was read closes the connection.
- Responses carry `Cache-Control: no-store`. Admin requests are counted in
  the metrics as op `Admin` and appear in the access log.

### Resources and condition keys

Admin actions are evaluated against these resources (glob-matched like s3
ARNs, policy variables such as `${aws:username}` included):

| Resource | Actions |
|---|---|
| `arn:gos3:admin:::user/<name>` | CreateUser (the new user's name), GetUser, UpdateUser, DeleteUser, CreateAccessKey (the target user; choosing the access key ID also needs it at service level), UpdateAccessKey / DeleteAccessKey (the key's owner; an unknown key is evaluated at service level) |
| `arn:gos3:admin:::policy/<name>` | GetPolicy, PutPolicy, DeletePolicy |
| `arn:gos3:admin:::bucket/<name>` | GetBucket, ForceDeleteBucket, GetBucketCompression, PutBucketCompression, GetBucketPlacement, UpdateBucketPlacement |
| service level | every other action (list actions, config, stats, discovery, simulate, self-service) |

The policy resources `*` and `arn:gos3:admin:::*` match every admin
resource, service level included, so policies written before per-target
resources existed keep their meaning. A service-level request matches only
those two. s3 ARNs (`arn:aws:s3:::*`, `arn:*:*:::*`, …) never match admin
resources and `arn:gos3:admin:::…` never matches s3 resources. Likewise an
Allow with `NotResource` covers only the namespaces its list names: `"Action":
"*"` with `"NotResource": ["arn:aws:s3:::secret/*"]` grants every s3 action
outside `secret` and no admin action (a Deny with `NotResource` applies to
everything not listed, as on AWS). New policy
documents may only use `arn:gos3:admin:::*` and
`arn:gos3:admin:::<type>/<name>` with type `user`, `policy` or `bucket` (or
a wildcard pattern of them); other `arn:gos3:` resources are 400
`InvalidPolicy`.

Condition key `admin:PolicyName` (multi-valued) holds the policy names a
`POST /users` or `PATCH /users/{name}` body attaches; it is absent when the
body attaches none (no `policies`, or an empty list). Use it with
`ForAllValues:StringEquals` to limit which policies a delegated administrator
may attach, or with `ForAnyValue:StringEquals` in a Deny. See
[security.md](security.md#scoping-admin-permissions).

## Errors

Non-2xx responses: `{"error": {"code": "<Code>", "message": "<text>"}}`.

| HTTP | code |
|---|---|
| 400 | `InvalidRequest`, `InvalidName`, `InvalidPolicy`, `InvalidSettings` |
| 401 | `Unauthenticated` (anonymous or bad signature — message is `<S3Code>: <text>`, e.g. `SignatureDoesNotMatch: ...`, `AccessDenied: anonymous requests are not allowed; ...`) |
| 403 | `AccessDenied` |
| 404 | `NotFound` |
| 405 | `MethodNotAllowed` |
| 409 | `Conflict` (exists / in use / bucket not empty) |
| 429 | `TooManyRequests` (`POST /discovery/scan`: 2 scans, or `POST /discovery/probe`: 4 probes, are already running on this server) |
| 500 | `InternalError` |
| 502 | `ProbeFailed` (`POST /discovery/probe`: the target did not answer as a gos3 server; the message ends in `unreachable`, `timed out`, `HTTP <status>` or `not a gos3 server`) |
| 503 | `InternalError` (request canceled or timed out) |

## Endpoints

### Identity
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/whoami` | *any* | `WhoAmI` |
| GET | `/whoami/keys` | *any* | `{"keys": AccessKey[]}` (own keys, no secrets) |
| POST | `/whoami/keys` | CreateOwnAccessKey (explicit Allow needed) | body `{"description"?}` → 201 `AccessKey` (with `secretKey`). Not for root (400). Logged at Info with the user, the new access key ID and the key that signed the request. |
| DELETE | `/whoami/keys/{accessKey}` | *any* unless `admin:DeleteOwnAccessKey` is denied | 204 (own keys only; cannot delete the key used to sign this request) |

```jsonc
// WhoAmI
{
  "user": "alice", "accessKey": "AKIA…", "isRoot": false,
  "policies": ["readwrite"],
  "capabilities": {           // derived by evaluating the actions below at
                              // service level (grants scoped to some users,
                              // policies or buckets do not show here)
    "admin": false,           // any admin:* allowed (self-service excluded)
    "viewInfo": true,         // admin:ServerInfo
    "viewStats": false,       // admin:GetStats
    "manageConfig": false,    // admin:UpdateConfig
    "manageUsers": false,     // admin:CreateUser
    "listUsers": false,       // admin:ListUsers (true for delegated user managers too)
    "managePolicies": false,  // admin:PutPolicy
    "manageBuckets": false,   // admin:ForceDeleteBucket
    "viewStorage": false,     // admin:ListBackings
    "manageStorage": false,   // admin:UpdateBucketPlacement
    "discovery": false,       // admin:GetDiscovery
    "scan": false,            // admin:ScanNetwork
    "probe": false,           // admin:ProbeNetwork
    "createOwnKeys": false,   // admin:CreateOwnAccessKey explicitly allowed (always false for root)
    "manageCompression": false // admin:PutBucketCompression
  },
  "server": Info              // same as GET /_gos3/info
}
```

### Server
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/info` | ServerInfo | `ServerInfo` |
| GET | `/stats` | GetStats | `Stats` |
| GET | `/config` | GetConfig | `config.Settings` |
| PUT | `/config` | UpdateConfig | body full `config.Settings` → updated `config.Settings` (400 `InvalidSettings` with message on validation failure) |

```jsonc
// ServerInfo = Info (see docs/discovery.md) plus:
{ …Info,
  "backend": "fs", "dataPath": "/data",
  "goVersion": "go1.25.0", "os": "linux", "arch": "amd64", "numCPU": 8,
  "uptimeSeconds": 1234 }

// Stats
{
  "metrics": metrics.Snapshot,          // see below
  "storage": { "buckets": 3, "objects": 1200, "bytes": 123456789,
               "storedBytes": 61234567,  // bytes stored for "bytes" (less when compressed)
               "versions": 1300, "versionBytes": 133456789,
               "storedVersionBytes": 71234567, "compressedVersions": 800,
               "deleteMarkers": 4, "pendingUploads": 1,
               "computedAt": "…" },     // cached up to 30s
  "runtime": { "goroutines": 42, "heapAllocBytes": 1234, "sysBytes": 5678, "gcPauseTotalNs": 1 },
  "disk": { "path": "/data", "totalBytes": 1, "freeBytes": 1, "usedBytes": 1 } // fs backend only, else null
}

// metrics.Snapshot (internal/metrics)
{
  "startedAt": "…", "uptimeSeconds": 1234,
  "requests": 5000, "errors4xx": 12, "errors5xx": 0, "aborted": 1,
  "bytesIn": 1, "bytesOut": 1, "inFlight": 2,
  "ops": [                              // sorted by op
    { "op": "GetObject", "requests": 4000, "errors4xx": 10, "errors5xx": 0, "aborted": 0,
      "bytesIn": 0, "bytesOut": 1, "avgLatencyMs": 1.2, "p50LatencyMs": 0.9, "p99LatencyMs": 8.5 }
  ],
  "recent": [                           // per minute, last 60 minutes, oldest first
    { "minute": "…", "requests": 80, "errors": 1, "aborted": 0, "bytesIn": 1, "bytesOut": 1 }
  ]
}
```

`aborted` counts requests the client abandoned (status 499, for example an
upload whose connection dropped). They are not included in `errors4xx`, and
a `recent[].errors` value counts only 4xx and 5xx responses, leaving aborted
requests out.

`config.Settings` (see `internal/config`):

```jsonc
{
  "name": "gos3-1", "region": "us-east-1", "publicUrl": "http://10.0.0.5:9000",
  "domains": ["s3.local"], "publicInfo": true, "uiRedirect": true,
  "cors": { "allowedOrigins": ["*"], "allowedHeaders": ["*"],
            "exposeHeaders": ["*", "ETag", "Content-Length", "x-amz-version-id", "…"], // default: "*" + the S3 response headers
            "maxAgeSeconds": 3600 },
  "discovery": { "enabled": true, "advertise": true, "mdns": true, "beacon": true,
                 "beaconGroup": "239.255.73.83:7373", "beaconInterval": 10000000000,
                 "seeds": [], "dnsSrv": [], "peerTtl": 60000000000,
                 "allowScan": true, "maxScanHosts": 1024 },
  "logging": { "level": "info", "format": "text", "accessLog": true },
  "lifecycle": { "enabled": true, "intervalSeconds": 3600 },
  "metricsPublic": false,
  "compression": { "mode": "optional",   // optional | enforced | disabled
                   "level": 1, "minSize": 4096,
                   "skipContentTypes": ["image/jpeg", "video/*", "…"] }
}
```

`PUT /config` validates the whole document (`config.ValidateSettings`); the
400 `InvalidSettings` message names the field, e.g.
`discovery.seeds[1]: "ftp://x" must be an absolute http:// or https:// URL`.
Rules: `region` lowercase letters/digits/`-`; `name` ≤ 128 printable
characters (empty allowed); `publicUrl` empty or an absolute http(s) URL
without a path (a trailing `/` is allowed; no credentials, query or
fragment); `domains` DNS names
without scheme or port; CORS origins `*`, `scheme://host[:port]` or
`scheme://*.domain[:port]` (no path); `exposeHeaders` header names or a lone
`*` (no partial patterns like `x-amz-*`); `maxAgeSeconds` 0–604800;
`beaconGroup` an **IPv4** multicast `ip:port`; `beaconInterval` 1 s–1 h;
`peerTtl` 30 s–7 d; `seeds` http(s) base URLs without a path, or a bare
`host[:port]` (meaning `http://`), at most 256; `dnsSrv` domain names, at
most 64; `maxScanHosts` 1–65536; `logging.level` `debug|info|warn|error`;
`logging.format` `text|json`; `lifecycle.intervalSeconds` 60–31536000;
`compression.mode` `optional|enforced|disabled`; `compression.level` 1–9;
`compression.minSize` 0–5 TiB; `compression.skipContentTypes` entries
`type/subtype`, `type/*` or a prefix ending in `*` (no parameters); other
lists at most 256 entries. A document without `compression` (from a client
that predates it) keeps the current compression settings, and fields missing
from `compression` keep their current values. Changes apply immediately and are persisted to
`<data>/.gos3/settings.json`, which from then on overrides the YAML file,
environment and flags for these fields.

### Users & access keys
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/users` | ListUsers | `{"users": iam.User[]}` |
| POST | `/users` | CreateUser | body `iam.UserSpec` (+ optional `"createKey": true`) → 201 `{"user": User, "key"?: AccessKey}`; the names `.` and `..` and 12-digit names are 400 `InvalidName`, and a name equal to an access key ID is 409 `Conflict` like an existing user. A non-empty `"policies"` also requires `admin:UpdateUser`, and `"createKey": true` also requires `admin:CreateAccessKey`; all three are evaluated on `user/<new name>` (with `admin:PolicyName`) before anything is created (otherwise 403 `AccessDenied`) |
| GET | `/users/{name}` | GetUser | `iam.User` |
| PATCH | `/users/{name}` | UpdateUser | body `iam.UserUpdate` → `iam.User` |
| DELETE | `/users/{name}` | DeleteUser | 204 |
| POST | `/users/{name}/keys` | CreateAccessKey | body `{"description"?, "accessKey"?, "secretKey"?}` → 201 `iam.AccessKey` (incl. secret). Omitted fields are generated (20-character ID, 40-character secret). A chosen `accessKey` (3–128 of `[A-Za-z0-9._-]`) also needs `admin:CreateAccessKey` at service level (`*` or `arn:gos3:admin:::*`; otherwise 403 `AccessDenied`) and must not equal a user name, `root`, `anonymous` or a 12-digit account ID (400 `InvalidName`; 409 if the key exists). A chosen `secretKey` must be 16–128 printable ASCII characters without spaces |
| PATCH | `/keys/{accessKey}` | UpdateAccessKey | body `{"status": "Active"\|"Inactive"}` → 204. Applied only if the key still belongs to the owner it was authorized against, otherwise 404 |
| DELETE | `/keys/{accessKey}` | DeleteAccessKey | 204 (404 if the key no longer belongs to the owner it was authorized against) |

### Policies
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/policies` | ListPolicies | `{"policies": iam.NamedPolicy[]}` |
| GET | `/policies/{name}` | GetPolicy | `iam.NamedPolicy` |
| PUT | `/policies/{name}` | PutPolicy | body `{"description"?, "document": PolicyDocument}` → `iam.NamedPolicy` (creating a policy named `.` or `..` is 400 `InvalidName`) |
| DELETE | `/policies/{name}` | DeletePolicy | 204 (409 if attached, 400 if builtin) |
| POST | `/authz/simulate` | SimulatePolicy | body `{"user": "alice" \| "root" \| "" (anonymous), "action": "s3:GetObject", "bucket": "b", "key": "k", "resource"?: "arn:gos3:admin:::user/bob", "conditions"?: {"aws:SourceIp": "10.0.0.1" \| ["…"]}}` → `{"allowed": bool, "explicitDeny": bool, "reason": "…"}` (bucket policy and bucket ACL of `bucket` are included; 404 for an unknown user or bucket). `bucket`/`key` are for s3 actions, `resource` (empty = service level, or `arn:gos3:admin:::{user,policy,bucket}/<name>`) for admin actions; mixing them is 400 |

### Buckets (management helpers; regular bucket ops use the S3 API)
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/buckets` | ListBuckets | `{"buckets": BucketSummary[]}`; `?usage=true` includes usage (cached 30s) |
| GET | `/buckets/{name}` | GetBucket | `BucketSummary` with usage |
| GET | `/buckets/{name}/compression` | GetBucketCompression | `BucketCompression` |
| PUT | `/buckets/{name}/compression` | PutBucketCompression | body `{"enabled": bool}` → `BucketCompression`. 409 `Conflict` when it contradicts the server's `compression.mode` (`true` while `disabled`, `false` while `enforced`); 400 for any other body |
| DELETE | `/buckets/{name}/compression` | PutBucketCompression | removes the bucket's setting (back to the server default) → 204; idempotent |
| DELETE | `/buckets/{name}?force=true` | ForceDeleteBucket | deletes all versions, delete markers, uploads and configs, then the bucket → 204. Without `force` behaves like S3 DeleteBucket (409 if not empty; in-progress uploads alone do not make a bucket non-empty, they are aborted with it). Object lock is honoured: any version under legal hold or unexpired COMPLIANCE retention → 409 before anything is deleted; unexpired GOVERNANCE retention → 409 unless `bypassGovernanceRetention=true` is added (only with `force`, else 400), which also requires `s3:BypassGovernanceRetention` on each such object (identity policies; 403 otherwise). |

```jsonc
// BucketSummary
{ "name": "photos", "createdAt": "…", "region": "us-east-1",
  "versioning": "Enabled" | "Suspended" | "", "objectLock": false, "owner": "alice",
  "configs": { "policy": true, "cors": false, "lifecycle": true, "tagging": false,
               "encryption": false, "website": false, "objectLock": false, "acl": "private" },
  "public": false,          // bucket policy grants anonymous access or ACL public-*
  "compression": { "enabled": false, "effective": false }, // see BucketCompression
  "usage": storage.BucketUsage | null }  // {"objects","bytes","storedBytes","versions","versionBytes",
                                         //  "storedVersionBytes","compressedVersions","deleteMarkers","pendingUploads"}

// BucketCompression
{ "enabled": true,          // the bucket's setting (false when none is stored)
  "effective": true,        // new objects are stored compressed (subject to minSize and skipContentTypes)
  "serverMode": "optional", // the administrator's compression.mode: optional (enabled decides), enforced, disabled
  "algorithm": "deflate", "level": 1, "minSize": 4096 }
```

Compression is transparent to S3 clients (same content, ETags, checksums
and sizes); `storedBytes` and `storedVersionBytes` count what the backend
stores. See [operations.md](operations.md#compression).

### Storage backings and bucket placement

A server can store buckets on several **backings** (storage backends such as
an fs directory per disk; see
[configuration.md](configuration.md#storage-backings)). Every bucket has a
**placement**: the backing that holds its primary copy and, optionally,
replicas on other backings. See [operations.md](operations.md#multiple-backings-and-replication)
for the semantics.

| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/backings` | ListBackings | `{"defaultBacking": "default", "autoFailover": false, "backings": Backing[]}` |
| GET | `/buckets/{name}/placement` | GetBucketPlacement | `Placement` |
| PUT | `/buckets/{name}/placement` | UpdateBucketPlacement | body `{"mode": "single" \| "active-passive" \| "active-active", "replicas": ["fast", …]}` → `Placement`. Sets the replication mode and the replica backings (the primary stays). `single` needs an empty `replicas`, the other modes at least one. New replicas start a full copy (`state` `syncing`); removed replicas lose their copy (deleted in the background). 400 `InvalidRequest` for an unknown backing, the primary listed as a replica, or a duplicate; 409 `Conflict` while a move is in progress or while the bucket's copy on a backing being added is still being deleted |
| POST | `/buckets/{name}/placement/move` | UpdateBucketPlacement | body `{"to": "archive", "from"?: "<backing>"}` → 202 `Placement`. Moves the copy on `from` (default: the primary) to `to`, online: `to` is filled in the background while the bucket stays readable and writable, then a short cutover (writes to the bucket wait, at most 60 s, for in-flight writes to finish) makes `to` take `from`'s place and `from`'s copy is deleted. In-progress multipart uploads are not moved: after a move of the primary they fail with `NoSuchUpload`. 400 for an unknown backing or a `from` that is not a member; 409 when a move is already in progress or `to` already holds a copy |
| DELETE | `/buckets/{name}/placement/move` | UpdateBucketPlacement | cancels the move in progress → `Placement` (the partial copy on `to` is deleted); 409 when there is none or its cutover already started |
| POST | `/buckets/{name}/placement/promote` | UpdateBucketPlacement | body `{"backing": "fast", "force"?: false}` → `Placement`. Makes a replica the primary (the old primary becomes a replica). Without `force` the replica must have finished its initial copy, and pending changes are copied to it during a cutover (as for a move) before the switch, so nothing is lost. `force: true` switches immediately, e.g. when the primary's backing is down: changes not yet copied to the replica are lost (the old primary is re-copied from the new one). 400 when `backing` is not a replica; 409 when it has not finished its initial copy (without `force`) or a move is in progress |
| POST | `/buckets/{name}/placement/resync` | UpdateBucketPlacement | body `{"backing"?: "fast"}` (default: every replica) → 202 `Placement`. Compares the replica(s) with the primary and copies every difference (versions, metadata, bucket configuration) |

`UpdateBucketPlacement` and `GetBucketPlacement` are evaluated against
`arn:gos3:admin:::bucket/<name>`, `ListBackings` at service level. The
built-in `diagnostics` policy allows `ListBackings` and `GetBucketPlacement`.
404 `NotFound` for an unknown bucket.

```jsonc
// Backing
{ "name": "fast",              // "default" is the backing configured by storage.backend / storage.path
  "kind": "fs" | "memory",
  "path": "/mnt/nvme/gos3",    // "" for memory
  "default": false,            // new buckets are created here
  "healthy": true,             // last health check or operation succeeded
  "lastError": "",             // last error that marked it unhealthy ("" when healthy)
  "checkedAt": "…",            // last health check (RFC3339; omitted before the first)
  "buckets": 3,                // buckets whose primary copy is here
  "replicas": 1,               // replica copies (and move targets) of other buckets here
  "disk": { "path": "/mnt/nvme/gos3", "totalBytes": 1, "freeBytes": 1, "usedBytes": 1 } | null } // fs only

// Placement
{ "bucket": "photos",
  "mode": "single" | "active-passive" | "active-active",
  "primary": "default",
  "members": [                 // primary first, then replicas, then a move target
    { "backing": "default",
      "role": "primary" | "replica" | "move-target",
      "state": "in-sync" | "syncing" | "lagging" | "degraded" | "offline",
      "healthy": true,         // the backing's health
      "pending": 0,            // changed keys not yet copied to this member
      "progress": { "keys": 120, "versions": 130, "bytes": 1048576 } | null, // running full copy
      "lastSyncAt": "…",       // last completed full copy (omitted when none)
      "lastError": "" } ],
  "move": { "from": "default", "to": "archive", "startedAt": "…",
            "phase": "copying" | "cutover" } | null }
```

Member states: `in-sync` (an active-active replica receiving every write
synchronously, a replica with no pending changes, or the primary); `syncing`
(initial or requested full copy running); `lagging` (changes are pending,
copied in the background); `degraded` (copying failed; retried with
back-off, see `lastError`); `offline` (the backing is unhealthy).

`BucketSummary` (`GET /buckets`, `GET /buckets/{name}`) also has
`"placement": {"mode": "single", "primary": "default", "replicas": [], "moving": false}`.

### Discovery
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/discovery` | GetDiscovery | `{"enabled": bool, "config": discovery.Config, "self": Info, "peers": discovery.Peer[]}` |
| POST | `/discovery/probe` | ProbeNetwork | body `{"url": "http://host:9000"}` → `discovery.Peer` (400 `InvalidRequest` ending in `address not allowed` when the target is, or resolves to, a cloud metadata, unspecified, multicast or broadcast address; 502 `ProbeFailed` on failure, with only a coarse reason: `unreachable`, `timed out`, `HTTP <status>` or `not a gos3 server`; 429 `TooManyRequests` while 4 probes are already running on the server; each probe is bounded by 10 s) |
| POST | `/discovery/scan` | ScanNetwork | body `discovery.ScanRequest` → `discovery.ScanResult` (403 when `allowScan` is false; 400 when the targets expand to more than `maxScanHosts` hosts, or for more than 256 targets, more than 64 ports, a port outside 1–65535, a scheme other than http/https, a `timeout` outside 0–60 s or `concurrency` outside 0–1024; 429 `TooManyRequests` while 2 scans are already running on the server; the whole scan is bounded by 5 minutes) |

## Public endpoints (no `/admin/v1` prefix; served by `internal/server`)

| Method | Path | Auth | Response |
|---|---|---|---|
| GET | `/_gos3/info` | none if `publicInfo`, else any | `discovery.Info`, `Access-Control-Allow-Origin: *` (also on 401; `OPTIONS` answered with 204) |
| GET | `/_gos3/health` | none | `{"status":"ok"}` |
| GET | `/_gos3/ready` | none | `{"status":"ok"}` or 503 `{"status":"unavailable","error":"…"}` (also while shutting down) |
| GET | `/_gos3/metrics` | none if `metricsPublic`, else admin:GetStats | Prometheus text; 401/403 JSON errors; 404 when `metrics.enabled` is false |

Any other `/_gos3/*` path is a JSON 404 `{"error":{"code":"NotFound",…}}`. See
[operations.md](operations.md#metrics) for the metric names.
