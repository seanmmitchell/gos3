# gos3 admin API (v1)

The API is enabled by default. Set `admin.enabled: false`,
`GOS3_ADMIN_ENABLED=false`, or `--no-admin` to disable it and the embedded
UI. Disabled API routes return 404, including signed requests and preflights.
Enabling it requires a restart. `--no-ui` alone keeps the API available for
a separately hosted UI.

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
  `GetBucketPlacement`, `ListJobs`, `GetJob`, `GetBucketSnapshots` (not
  `ProbeNetwork`, `ScanNetwork` or `GetResticCredentials`).
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
| `arn:gos3:admin:::bucket/<name>` | GetBucket, CreateBucket, ForceDeleteBucket, ReleaseBucket, GetBucketCompression, PutBucketCompression, GetBucketEncryption, PutBucketEncryption, GetBucketPlacement, UpdateBucketPlacement, GetResticCredentials, GetBucketSnapshots, PutBucketSnapshots, RunBucketSnapshot, RestoreBucketSnapshot; CreateCopyJob, GetJob, UpdateJob (the job's source and target buckets) |
| `arn:gos3:admin:::backing/<name>` | ListRemoteBuckets, CreateRemoteBucket, AdoptRemoteBucket, ManageContainers (s3 backings, see [Remote buckets and containers](#remote-buckets-and-containers-s3-backings)); also, besides the bucket, UpdateBucketPlacement (a backing that receives a new replica or a move), CreateCopyJob (a copy, mirror or restore with `target.create`), PutBucketSnapshots (a snapshot target's backing) and GetResticCredentials (the backing whose S3 keys the credentials carry) |
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
`arn:gos3:admin:::<type>/<name>` with type `user`, `policy`, `bucket` or `backing` (or
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
| 503 | `InternalError` (request canceled or timed out, or a storage backing that cannot be reached, e.g. an s3 backing whose service does not answer or that is still loading; also an unknown bucket name while a backing that could not be listed at startup is pending adoption; a bucket's encryption keys or restic installation or repository that cannot serve the request) |

Endpoints of a feature this server does not run (background jobs, copy
job dry runs, whole-bucket snapshots) answer 404 `NotFound` with a message
saying so, after authorization.

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
                              // policies, buckets or backings do not show here)
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
    "manageCompression": false, // admin:PutBucketCompression
    "browseRemote": false,    // admin:ListRemoteBuckets
    "createRemote": false,    // admin:CreateRemoteBucket
    "adoptRemote": false,     // admin:AdoptRemoteBucket
    "manageContainers": false, // admin:ManageContainers
    "viewJobs": false,        // admin:ListJobs
    "manageJobs": false,      // admin:CreateCopyJob
    "manageSnapshots": false, // admin:PutBucketSnapshots
    "revealResticCredentials": false, // admin:GetResticCredentials
    "runSnapshots": false,    // admin:RunBucketSnapshot
    "restoreSnapshots": false, // admin:RestoreBucketSnapshot
    "updateJobs": false       // admin:UpdateJob
  },
  "server": Info,             // same as GET /_gos3/info
  "backings": {               // storage backings a new bucket can be put on (x-gos3-backing),
                              // shown to every caller; details: GET /backings
    "default": "default",
    "available": [
      { "name": "default", "kind": "fs" },
      { "name": "cloud", "kind": "s3",
        "kinds": ["native", "wrapped"],   // bucket kinds offered (s3 backings only)
        "defaultKind": "wrapped",         // kind of buckets created without x-gos3-bucket-kind
        "containers": [                   // containers of wrapped buckets (x-gos3-container)
          { "id": "home", "label": "shared", "default": true },
          { "id": "c-7f3a", "label": "photos-archive" } ] } ]
  }
}
```

Container labels are set by administrators and shown to every caller (as
are backing names); don't put secrets in them.

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
  "backend": "fs",             // the default backing: "fs" | "memory" | "s3"
  "dataPath": "/data",         // storage.path ("" when none, e.g. memory or s3 without a path)
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
| POST | `/authz/simulate` | SimulatePolicy | body `{"user": "alice" \| "root" \| "" (anonymous), "action": "s3:GetObject", "bucket": "b", "key": "k", "resource"?: "arn:gos3:admin:::user/bob", "conditions"?: {"aws:SourceIp": "10.0.0.1" \| ["…"]}}` → `{"allowed": bool, "explicitDeny": bool, "reason": "…"}` (bucket policy and bucket ACL of `bucket` are included; 404 for an unknown user or bucket). `bucket`/`key` are for s3 actions, `resource` (empty = service level, or `arn:gos3:admin:::{user,policy,bucket,backing}/<name>`) for admin actions; mixing them is 400 |

### Buckets (management helpers; regular bucket ops use the S3 API)
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/buckets` | ListBuckets | `{"buckets": BucketSummary[]}`; `?usage=true` includes usage (cached 30s) |
| GET | `/buckets/{name}` | GetBucket | `BucketSummary` with usage |
| GET | `/buckets/{name}/compression` | GetBucketCompression | `BucketCompression` |
| PUT | `/buckets/{name}/compression` | PutBucketCompression | body `{"enabled": bool}` → `BucketCompression`. 409 `Conflict` when it contradicts the server's `compression.mode` (`true` while `disabled`, `false` while `enforced`); 400 for any other body |
| DELETE | `/buckets/{name}/compression` | PutBucketCompression | removes the bucket's setting (back to the server default) → 204; idempotent |
| POST | `/buckets/{name}/release` | ReleaseBucket | forgets a **native** bucket of an s3 backing without touching the remote bucket or its objects → 204. 409 `Conflict` while the bucket has replicas or a move in progress ("remove replicas first"); 400 `InvalidRequest` for buckets that are not native (their data would be orphaned: delete them instead) |
| DELETE | `/buckets/{name}?force=true` | ForceDeleteBucket | deletes all versions, delete markers, uploads and configs, then the bucket → 204. Without `force` behaves like S3 DeleteBucket (409 if not empty; in-progress uploads alone do not make a bucket non-empty, they are aborted with it). Object lock is honoured: any version under legal hold or unexpired COMPLIANCE retention → 409 before anything is deleted; unexpired GOVERNANCE retention → 409 unless `bypassGovernanceRetention=true` is added (only with `force`, else 400), which also requires `s3:BypassGovernanceRetention` on each such object (identity policies; 403 otherwise). |

```jsonc
// BucketSummary
{ "name": "photos", "createdAt": "…", "region": "us-east-1",
  "versioning": "Enabled" | "Suspended" | "", "objectLock": false, "owner": "alice",
  "kind": "native" | "wrapped",   // s3 backings only (omitted otherwise)
  "location": "s3://acme-photos/", // s3 backings: remote bucket (native) or container (wrapped)
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

### Object encryption providers

| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/buckets/{name}/encryption-provider` | GetBucketEncryption | `{"provider":"AES256"\|"restic", "scope":"sse"\|"all", "available":["AES256","restic"], "backings":["default",…]}` |
| PUT | `/buckets/{name}/encryption-provider` | PutBucketEncryption | body `{"provider":"AES256"\|"restic", "scope"?:"sse"\|"all"}`; prepares the provider, then saves it and returns the GET document. `scope: "all"` (restic only) archives **every** write in the bucket's restic repository, not only writes that request SSE (the per-version restic archive toggle); default `sse` |
| GET | `/buckets/{name}/restic` | GetBucketEncryption | `ResticInfo`: where the bucket's restic data lives and how to recover it without gos3 (no secrets) |
| POST | `/buckets/{name}/restic/credentials` | GetResticCredentials; when the environment carries a backing's S3 keys (a repository on an s3 backing), also on `arn:gos3:admin:::backing/<name>` of that backing (service level when unknown) | body (optional) `{"backing"?: "<name>", "purpose"?: "archive"\|"snapshots"}` → `ResticCredentials` (the repository password and environment for the `restic` CLI). `purpose` defaults to `archive`, the archive of the copy on `backing` (default: the primary); `backing` is refused with `snapshots` (400: the bucket has one snapshot repository). Every disclosure is logged at Warn with the caller, access key, client address, bucket, backing and purpose (never the secrets). 404 when the bucket has no repository of that purpose on that backing (none yet, none at all, or a backing holding no copy of the bucket); 503 when the repository cannot be reached; other failures are 500 without their cause (which is logged) |

The provider is the bucket's implementation of SSE-S3: it applies to bucket
default encryption and to uploads that request `AES256`. Provider actions use
`arn:gos3:admin:::bucket/<name>`. The body is decoded strictly: storage and
secret fields are rejected (naming the field), since restic derives its
repositories from each assigned backing.

`available` is always sent: `[]` when the bucket can use no provider, and
`null` only when the storage backend cannot tell which providers it can
store. It lists the providers the bucket can use now, the ones every
backing holding a copy of the bucket (primary, replicas and a move target)
can store: `AES256` needs every copy on an s3 backing to be a wrapped bucket
of a backing with an encryption key; `restic` needs the restic executable on
the server and every copy on a filesystem backing or in a wrapped bucket of
an s3 backing with an encryption key (not memory). Native buckets of s3
backings (stored as-is in their remote bucket) offer no provider: their
`available` list is empty and PUT answers 400. See
[encryption.md](encryption.md#where-keys-and-repositories-live).
PUT refuses an unavailable provider with 400. Otherwise it validates the provider and prepares the
bucket's keys or restic repository **before** saving the selection; if that
fails it returns 400 and the previous provider stays in effect. A missing
bucket is 404; a stored provider document that is not valid makes GET fail
with 500 until a PUT replaces it. Changes are serialized per bucket.
GET reports the saved selection and the assigned backings, not repository
health. Provider selection alone does not enable S3 bucket-default encryption;
use PutBucketEncryption with AES256 (also for restic), or an AES256 upload
header. Force-deleting a bucket removes its encryption documents only together
with the bucket.

Restic repositories follow the bucket's backings and have no settings of
their own: an fs backing keeps them in its data directory, an s3 backing in
the container holding the (wrapped) bucket.

`scope` is always returned (`sse` when the stored document has none); the
stored document leaves out the default `sse`, so builds that predate scopes
read it unchanged. `scope: "all"` with `AES256`, or an unknown scope, is
400 and changes nothing.

`GET /buckets/{name}/restic` describes repositories as they are and never
creates one. A copy whose repository cannot be described has `"info":
null` and an `error`: `not created yet: …` (the bucket's first restic
write creates it), `no repository: …` (a backing that keeps no restic
repositories, e.g. memory, or a native bucket), or the backing's error.
`tags.bucketId` is the primary's bucket ID tag, omitted until its
repository exists (each backing has its own bucket ID). `snapshots` and
`snapshotRepository` are null when the bucket has no snapshot schedule or
the server has no whole-bucket snapshots; `snapshotsError` (omitted when
empty) says why they could not be read. A stored provider document that is
not valid fails the request with 500, as for `GET …/encryption-provider`.

```jsonc
// ResticInfo (GET /buckets/{name}/restic)
{ "settings": { "provider": "restic", "scope": "all" },   // the bucket's encryption provider document
  "archive": [                    // per copy of the bucket: its per-version archive repository
    { "backing": "default", "role": "primary" | "replica" | "move-target",
      "info": { "bucketId": "…", "repository": "/data/.gos3/restic/<id>" | "s3:https://…/bucket/prefix/restic/<id>",
                "recordLocation": "…", "passwordSource": "…", "initialized": true,
                "envNames": ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"], "options": ["s3.bucket-lookup=path"] },
      "error": "" } ],            // e.g. "no repository: …" for native buckets (info is then null)
  "snapshots": SnapshotConfig | null,          // whole-bucket snapshot schedule (see below)
  "snapshotRepository": RepositoryInfo | null, // where whole-bucket snapshots go
  "snapshotsError"?: "…",                      // why the two above could not be read
  "tags": { "object": "gos3-object", "bucket": "gos3-bucket:<b64url name>", "bucketId": "gos3-bucket-id:<id>",
            "snapshot": "gos3-snapshot" } }   // restic tags gos3 uses (for `restic snapshots --tag …`)

// ResticCredentials (POST /buckets/{name}/restic/credentials)
{ "backing": "default",           // snapshots: the backing the schedule's target names (omitted when unknown)
  "purpose": "archive", "repository": "…", "password": "…",
  "env": { "AWS_ACCESS_KEY_ID": "…", "AWS_SECRET_ACCESS_KEY": "…" }, // {} when none
  "options": ["s3.region=eu-central-1"] }
```

Per-version archive snapshots (tag `gos3-object`) are read by gos3 while
the objects exist: never forget or prune them by hand (see
[encryption.md](encryption.md)).

### Storage backings and bucket placement

A server can store buckets on several **backings** (storage backends such as
an fs directory per disk or a bucket of a remote S3 service; see
[configuration.md](configuration.md#storage-backings) and
[backings.md](backings.md)). Every bucket has a
**placement**: the backing that holds its primary copy and, optionally,
replicas on other backings. See [operations.md](operations.md#multiple-backings-and-replication)
for the semantics.

| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/backings` | ListBackings | `{"defaultBacking": "default", "autoFailover": false, "backings": Backing[]}` |
| GET | `/backings/config` | GetConfig | `BackingConfiguration` (404 when live configuration is unavailable) |
| PUT | `/backings/config` | UpdateConfig | full `BackingConfiguration` → updated configuration, applied live and persisted before success |
| GET | `/buckets/{name}/placement` | GetBucketPlacement | `Placement` |
| PUT | `/buckets/{name}/placement` | UpdateBucketPlacement | body `{"mode": "single" \| "active-passive" \| "active-active", "replicas": ["fast", …]}` → `Placement`. Sets the replication mode and the replica backings (the primary stays). `single` needs an empty `replicas`, the other modes at least one. New replicas start a full copy (`state` `syncing`); removed replicas lose their copy (deleted in the background). 400 `InvalidRequest` for an unknown backing, the primary listed as a replica, or a duplicate; 409 `Conflict` while a move is in progress or while the bucket's copy on a backing being added is still being deleted |
| POST | `/buckets/{name}/placement/move` | UpdateBucketPlacement | body `{"to": "archive", "from"?: "<backing>", "container"?: "<container id>"}` → 202 `Placement`. `container` chooses the container of the copy on an s3 `to` backing (default: the backing's default container; copies on s3 backings are always wrapped buckets). Moves the copy on `from` (default: the primary) to `to`, online: `to` is filled in the background while the bucket stays readable and writable, then a short cutover (writes to the bucket wait, at most 60 s, for in-flight writes to finish) makes `to` take `from`'s place and `from`'s copy is deleted. In-progress multipart uploads are not moved: after a move of the primary they fail with `NoSuchUpload`. 400 for an unknown backing or a `from` that is not a member; 409 when a move is already in progress or `to` already holds a copy |
| DELETE | `/buckets/{name}/placement/move` | UpdateBucketPlacement | cancels the move in progress → `Placement` (the partial copy on `to` is deleted); 409 when there is none or its cutover already started |
| POST | `/buckets/{name}/placement/promote` | UpdateBucketPlacement | body `{"backing": "fast", "force"?: false}` → `Placement`. Makes a replica the primary (the old primary becomes a replica). Without `force` the replica must have finished its initial copy, and pending changes are copied to it during a cutover (as for a move) before the switch, so nothing is lost. `force: true` switches immediately, e.g. when the primary's backing is down: changes not yet copied to the replica are lost (the old primary is re-copied from the new one), except versions under object lock (a legal hold or unexpired retention), which are copied back to the new primary instead of being deleted; so are those of a copy being removed. 400 when `backing` is not a replica; 409 when it has not finished its initial copy (without `force`) or a move is in progress |
| POST | `/buckets/{name}/placement/resync` | UpdateBucketPlacement | body `{"backing"?: "fast"}` (default: every replica) → 202 `Placement`. Compares the replica(s) with the primary and copies every difference (versions, metadata, bucket configuration) |

`UpdateBucketPlacement` and `GetBucketPlacement` are evaluated against
`arn:gos3:admin:::bucket/<name>`, `ListBackings` at service level. Putting a
copy of the bucket on a backing (a replica that `PUT …/placement` adds, the
`to` of a move) also needs `UpdateBucketPlacement` on
`arn:gos3:admin:::backing/<name>` of that backing (403 otherwise, before
anything changes); replicas the bucket already has need nothing more. The
built-in `diagnostics` policy allows `ListBackings` and `GetBucketPlacement`.
404 `NotFound` for an unknown bucket.

`BackingConfiguration` is `{"backings": BackingDefinition[], "defaultBacking": "default", "revision": "<opaque token>", "persistent": true}`.
`backings` contains only additional backings; the system backing named
`default` cannot be replaced or removed. Send the revision from the last GET to
prevent overwriting concurrent changes; stale revisions return 409
`Conflict`. `persistent` is informational and ignored on PUT. Without a
system data directory, changes apply live but are lost at restart.

```jsonc
// BackingDefinition
{ "name": "archive",
  "backend": "fs" | "memory" | "s3",
  "path": "/mnt/archive",      // fs: directory (required); "" for memory and s3
  "s3": {                      // s3 only (omitted otherwise); see backings.md
    "endpoint": "https://s3.eu-central-1.amazonaws.com",  // required, no path
    "region": "eu-central-1",  // "" = us-east-1
    "homeBucket": "acme-gos3", // optional: where gos3 keeps the backing's records and its shared
                               // container; "" = records in the server's system directory
    "homePrefix": "gos3/",     // optional, needs homeBucket
    "defaultKind": "wrapped",  // "wrapped" (needs a home) | "native"; "" = wrapped with a home, else native
    "addressing": "",          // "" (auto) | "path" | "virtual"
    "partSize": 16777216,      // bytes, 5 MiB-5 GiB; omitted/0 = default
    "accessKey": "AKIA…",
    "secretKey": "…",          // write-only: never returned
    "encryptionKey": "…",      // write-only: base64 of 32 bytes, never returned
    "secretKeyConfigured": true,      // read-only
    "encryptionKeyConfigured": true,  // read-only
    "accessKeySource": "env",         // read-only: "env" when the value comes from
    "secretKeySource": "env",         //   GOS3_BACKING_<NAME>_* (omitted otherwise)
    "encryptionKeySource": "env",
    "envFields": ["endpoint", "secret_key"] } } // read-only: fields filled from GOS3_BACKING_<NAME>_*
```

The keys `bucket`, `prefix` and `gateway` of earlier versions are accepted
on PUT (mapped to `homeBucket`, `homePrefix` and `defaultKind: "native"`)
and never returned.

The secret key and the encryption key are **write-only**. GET returns them
blank, with `secretKeyConfigured` / `encryptionKeyConfigured` set; on PUT a
blank value keeps the value stored for the backing of the same name, and a
non-blank one replaces it. Fields still empty are then filled from the
`GOS3_BACKING_<NAME>_*` environment variables; such values (any field) are
listed in `envFields` (the keys also report source `env`) and never written
to `backings.json`. The read-only fields
are ignored on PUT. `backings.json` (mode 0600) stores the other secrets.

A backing is matched to its current definition by name. Unchanged
definitions are left alone. A change of an s3 backing's connection settings
(endpoint, region, credentials, encryption key, addressing, part size) or
of its `defaultKind` is applied in place, also while it holds buckets: gos3
connects with the new settings and verifies the home's and every
container's marker and encryption key (and, for a backing without a home,
access to its native buckets) before switching; on failure the backing
keeps running unchanged. Any other change (an fs path; an s3 home bucket or
prefix, including adding or removing the home; the backend) replaces the
backing, which needs it unused and empty.
An s3 backing that holds no placed bucket can be removed or replaced even
while its service is unreachable (the remote data is left untouched).
A new or replaced s3 backing must load within 30 seconds.

Opening or reconfiguring an s3 backing fails with 409 `Conflict` when its
prefix is in use by another gos3 server, and with 400 `InvalidRequest` for
settings that are wrong: an encryption key that does not match the backing,
a `gos3.json` marker that does not match (another backing), a home prefix
holding gos3 data without a marker, a service
without conditional writes, and credentials, bucket or region the service
refuses (`AccessDenied`, `InvalidAccessKeyId`, `SignatureDoesNotMatch`,
`NoSuchBucket`, `AuthorizationHeaderMalformed`, `PermanentRedirect`,
`InvalidBucketName`). A service that does not answer within the 30 seconds
gives 503.

Invalid names, duplicate directories, overlapping s3 bucket prefixes,
invalid s3 settings (errors name the field) and invalid defaults return 400
`InvalidRequest`, as does an s3 endpoint that is this server itself (one of
its advertised URLs, or a URL whose `/_gos3/info` reports this server's ID).
Replacing or removing a backing holding a bucket, replica, move target,
pending cleanup, or untracked bucket returns 409 `Conflict`. New backings
must have no existing buckets (409). A failed open or save leaves the
running configuration unchanged. `GetConfig` and `UpdateConfig` are
evaluated at service level. The saved `backings.json` overrides static
backing definitions on restart, like the runtime settings overlay.

```jsonc
// Backing
{ "name": "fast",              // "default" is the backing configured by storage.backend / storage.path
  "kind": "fs" | "memory" | "s3",
  "location": "s3://acme-gos3/gos3/", // s3 backings: the home (no endpoint, no secrets), or "local:"
                               // when the records are kept in the system directory
  "kinds": ["native", "wrapped"], // s3 backings: bucket kinds offered (omitted otherwise)
  "defaultKind": "wrapped",    // s3 backings: kind of new buckets without an explicit kind
  "account": AccountInfo,      // s3 backings only (omitted otherwise), see below
  "path": "/mnt/nvme/gos3",    // fs directory; "" for memory and s3
  "default": false,            // new buckets are created here
  "healthy": true,             // last health check or operation succeeded (false while an
                               // s3 backing's service is unreachable or it is still loading)
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
            "phase": "copying" | "cutover",
            "target": { "kind": "wrapped", "container": "c-7f3a" } } | null } // target: when the move chose a container
```

Member states: `in-sync` (an active-active replica receiving every write
synchronously, a replica with no pending changes, or the primary); `syncing`
(initial or requested full copy running); `lagging` (changes are pending,
copied in the background); `degraded` (copying failed; retried with
back-off, see `lastError`); `offline` (the backing is unhealthy).

`BucketSummary` (`GET /buckets`, `GET /buckets/{name}`) also has
`"placement": {"mode": "single", "primary": "default", "replicas": [], "moving": false}`.

### Remote buckets and containers (s3 backings)

An s3 backing is a connection to an S3 service. Besides the buckets gos3
manages there, the account may hold other buckets; these endpoints browse
them, create remote buckets, adopt existing ones as **native** gos3 buckets
and manage the **containers** that hold **wrapped** buckets (see
[backings.md](backings.md)). They answer 404 `NotFound` for an unknown
backing and 400 `InvalidRequest` for a backing that is not an s3 backing.
Actions are evaluated against `arn:gos3:admin:::backing/<name>` (and
`arn:gos3:admin:::bucket/<bucket>` where noted).

| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/backings/{name}/remote-buckets` | ListRemoteBuckets | query `prefix`, `continuationToken`, `maxBuckets` (1-1000), `probe=true` (check unmanaged buckets for gos3 data, one request each) → `RemoteBucketList` |
| POST | `/backings/{name}/remote-buckets` | CreateRemoteBucket | body `{"name": "acme-new", "region"?: "eu-central-1", "objectLock"?: false}` → 201 `RemoteBucket` (an unmanaged remote bucket). 409 `Conflict` when it exists |
| GET | `/backings/{name}/containers` | ListRemoteBuckets | `{"containers": ContainerInfo[]}` (the home first) |
| POST | `/backings/{name}/containers` | ManageContainers, plus CreateRemoteBucket with `createBucket` or AdoptRemoteBucket without | body `ContainerSpec` → 201 `ContainerInfo`, once loaded (at most 30 s, else 503). Without `attach` the location must be empty (400 otherwise; 409 when it holds a gos3 store); it must not be claimed by another use or lie inside another gos3 store (409) |
| PUT | `/backings/{name}/containers/default` | ManageContainers | body `{"id": "c-7f3a"}` → `{"containers": ContainerInfo[]}` (400 for an unknown container) |
| DELETE | `/backings/{name}/containers/{id}` | ManageContainers | forgets an empty container (its remote data is left alone) → 204. 409 while it holds buckets; 400 for the home and for the default container (make another one the default first); 404 for an unknown container |
| POST | `/backings/{name}/buckets` | CreateBucket on the bucket, plus on the backing AdoptRemoteBucket (`adopt`) or CreateRemoteBucket (a new native remote bucket), and with `newContainer` ManageContainers plus CreateRemoteBucket (`newContainer.createBucket`) or AdoptRemoteBucket (an existing remote bucket), as for `POST …/containers` | body `CreateOnBacking` → 201 `BucketSummary` (without usage). The bucket's owner is the caller |

```jsonc
// RemoteBucketList
{ "buckets": RemoteBucket[],
  "complete": true,              // false when the credentials may not list buckets: then only the
  "listError": "AccessDenied …", //   locations the backing knows are listed (adopt others by name)
  "continuationToken": "…" }     // omitted on the last page

// RemoteBucket
{ "name": "acme-photos", "region": "eu-central-1", "createdAt": "…",
  "status": "native" | "home" | "container" | "claimed" | "gos3" | "inaccessible" | "unmanaged",
  "uses": [ { "role": "native", "bucket": "photos", "created": false },          // gos3 bucket "photos"
            { "role": "container", "containerId": "c-7f3a", "prefix": "gos3/" },
            { "role": "claimed", "backing": "other" } ],                        // used by another backing
  "note": "…" }

// ContainerInfo
{ "id": "home" | "c-7f3a", "label": "shared", "location": { "bucket": "acme-gos3", "prefix": "gos3/" },
  "region": "eu-central-1", "role": "home" | "dedicated" | "existing", "default": true,
  "buckets": 4, "state": "loading" | "ready" | "failed", "error": "", "added": "…" }

// ContainerSpec
{ "bucket": "acme-archive", "prefix"?: "gos3/", "region"?: "",
  "createBucket"?: false,   // create the remote bucket (a dedicated container); else it must exist
  "attach"?: false,         // use a location that already holds a gos3 store (e.g. moved from another server)
  "label"?: "archive" }

// CreateOnBacking
{ "bucket": "photos",                 // gos3 bucket name
  "kind": "native" | "wrapped",       // default: the kind the other fields imply (remoteBucket, region,
                                      //   adopt: native; container, newContainer: wrapped), else the
                                      //   backing's defaultKind
  "objectLock"?: false,               // object lock (native: the remote bucket is created with it; not with adopt)
  // native:
  "remoteBucket"?: "acme-photos",     // default: the gos3 name
  "region"?: "",                      // remote region (default: discovered / the backing's)
  "adopt"?: false,                    // use the existing remote bucket (objects as they are) instead of
                                      //   creating one; refused for buckets holding gos3 data or in use
  // wrapped:
  "container"?: "c-7f3a",             // default: the backing's default container
  "newContainer"?: ContainerSpec }    // or create/register a container first (not with container)

// AccountInfo (in Backing.account)
{ "home": { "bucket": "acme-gos3", "prefix": "gos3/" } | null,
  "records": "home" | "local", "defaultKind": "wrapped", "defaultContainer": "home",
  "kinds": ["native", "wrapped"], "nativeBuckets": 2, "wrappedBuckets": 4,
  "containers": ContainerInfo[] }
```

Errors: 409 `Conflict` when the gos3 bucket name is taken, the remote bucket
to create exists (`adopt` it instead), the location is used by another
bucket, container or backing (or by another gos3 server), or it holds a gos3
store: a remote bucket to `adopt` with a `gos3.json` at its root or in one of
its first 100 top-level prefixes, a container inside another gos3 store, or
one holding a store without `attach`; 400 `InvalidRequest` for invalid
names, contradicting fields, a remote bucket that does not exist (`adopt`,
or a container in an existing bucket), a container location that is not
empty, an unknown container, a wrapped bucket on a backing without
containers, and when the remote service refuses the request
made with the backing's credentials (e.g. `AccessDenied`; the message
carries the remote error); 503 when the remote service cannot be reached or
a container is not loaded. A taken gos3 bucket name is refused before
anything is done remotely; when the bucket cannot be created after
`newContainer` was added, the container stays registered and the message
names it: retry with `container`.

### Jobs: copy and mirror

Background **jobs** copy one bucket into another (any backings, any kinds,
different names) once (`copy`) or continuously (`mirror`), and run restic
snapshots and restores (see below). Jobs survive restarts (`jobs.json` in
the system directory; in memory without one) and report progress. See
[jobs.md](jobs.md).

| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/jobs?kind=&state=&bucket=` | ListJobs | `{"jobs": Job[]}`, newest first; `bucket` matches the source or target bucket |
| POST | `/jobs` | CreateCopyJob on **both** `bucket/<source>` and `bucket/<target>` | body `CopyJobSpec` → 201 `Job`; with `"dryRun": true` → 200 `{"spec": CopyJobSpec, "warnings": ["…"]}` (validated and normalized, nothing created) |
| GET | `/jobs/{id}` | GetJob on `bucket/<source>` and `bucket/<target>` | `Job` |
| POST | `/jobs/{id}/pause` \| `/resume` \| `/cancel` \| `/run` | UpdateJob on `bucket/<source>` and `bucket/<target>` | `Job`. `run` starts a waiting job (a mirror pass, a retry) now; on a queued or running job it does nothing (a running mirror makes another pass right after this one), on a paused or finished job it is 409; `resume` also retries a failed job from its checkpoint |
| DELETE | `/jobs/{id}` | UpdateJob (as above) | 204; 409 unless the job is finished (succeeded, failed or canceled) |
| GET | `/buckets/{name}/jobs` | GetJob on `bucket/<name>` | `{"jobs": Job[]}`: jobs whose source or target is the bucket (also after it was deleted), newest first, `kind` and `state` filters as for `/jobs`; only the jobs the caller may also get by ID (GetJob on their other bucket) |

A job's buckets are its `labels`: `source` and `target` (copy, mirror,
restore jobs) or `bucket`. They are what `bucket=` filters on and what
GetJob and UpdateJob are evaluated against (every one of them; a job
without bucket labels at service level). An unknown job ID is evaluated at
service level too, so a caller whose grants name buckets gets 403 rather
than telling unknown IDs from other buckets' jobs. `POST /jobs` evaluates
CreateCopyJob on both buckets before it validates anything else. When
`target.create` is present, it also requires CreateCopyJob on
`arn:gos3:admin:::backing/<backing>` (the effective default when omitted),
including dry runs. The authorized backing is saved in the job spec so
changing the default later cannot redirect the job. Only
kinds `copy` and `mirror` can be submitted (other kinds 400: restic jobs
come from the snapshot endpoints). `dryRun` (matched like any member,
case-insensitively) is removed from the spec the job keeps. Responses
never include a job's checkpoint. `state=` must be a job state (400
otherwise).

Errors: 400 `InvalidRequest` for an invalid spec (unknown fields, a bucket
copied onto itself, an unknown backing or container, `onExisting: "adopt"`
on a copy, `versions: "all"` to a
target that cannot keep versions, encrypted data to a target that cannot
store it without `allowPlaintext`, locked data to a target without object
lock without `dropRetention`); 409 `Conflict` for a non-empty target with
`onExisting: "fail"` (versions, delete markers and multipart uploads
count), a target range overlapping that of another active job with
`onExisting: "fail"`, a mirror cycle, a target of an active mirror, a
mirror into the target of another active job, or a
state change the job's state does not allow; 404 for unknown jobs.

```jsonc
// CopyJobSpec
{ "kind": "copy" | "mirror",
  "source": { "bucket": "photos", "prefix"?: "" },
  "target": { "bucket": "photos-bak", "prefix"?: "",
              "create"?: { "backing"?: "cold", "kind"?: "native" | "wrapped", "container"?: "c-7f3a",
                           "remoteBucket"?: "acme-photos-bak", "objectLock"?: false } }, // create the target bucket
  "options"?: {
    "versions": "all" | "current",              // default: all when the target keeps versions, else current
    "configs": "safe" | "all" | "none",         // bucket configuration to copy (default safe)
    "onExisting": "fail" | "merge" | "adopt",   // existing target bucket: refuse unless empty (default),
                                                //   copy without deleting, or (mirror only) take it over:
                                                //   target keys the source lacks are deleted
    "encryption": "preserve" | "target",        // keep each version's encryption, or use the target's settings
    "allowPlaintext": false,                    // allow copying encrypted data to an unencrypted target
    "dropRetention": false,                     // allow copying locked versions to a target without object lock
    "compression": "preserve" | "target",
    "propagateDeletes": true,                   // mirror: deletes in the source are applied to the target
    "deleteExtraneous": false,                  // mirror: delete target keys the source does not have
    "pollInterval": "5m",                       // mirror of a native source: how often to compare listings (>= 30s)
    "verifyInterval": "24h",                    // mirror: full comparison besides change notifications (>= 1m)
    "concurrency": 4,                           // keys copied in parallel (1-32)
    "finalPass": true },                        // copy: compare again after the first pass
  "dryRun"?: false }

// Job
{ "id": "…", "kind": "copy" | "mirror" | "restic-snapshot" | "restic-restore" | "restic-prune",
  "state": "queued" | "running" | "waiting" | "paused" | "succeeded" | "failed" | "canceled",
  "phase": "preparing" | "copying" | "verifying" | "watching" | "done"    // copy and mirror jobs
         | "waiting" | "scanning" | "planning" | "staging" | "backing up" | "retention" | "pruning" | "…", // restic jobs
  "createdBy": "alice", "createdAt": "…", "startedAt": "…", "updatedAt": "…", "finishedAt": "…", "nextRunAt": "…",
  "lastError": "", "failures": 0,
  "labels": { "source": "photos", "target": "photos-bak" },
  "spec": { … },          // the normalized spec
  "status": CopyStatus | SnapshotJobStatus }

// CopyStatus
{ "keysScanned": 0, "keysCopied": 0, "versionsCopied": 0, "bytesCopied": 0, "bytesDone": 0, "keysDeleted": 0,
  "conflicts": 0, "conflictSamples": [ { "key": "k", "reason": "locked version" } ],
  "totalKeys": 0, "totalBytes": 0, "bytesPerSecond": 0, "etaSeconds": 0,
  "pending": 0, "lagSeconds": 0, "lastPassAt": "…", "nextPassAt": "…", "passes": 0,   // mirror
  "errors": [ { "key": "k", "error": "…", "at": "…" } ],                               // last 20
  "mode": "identity" | "fingerprint",          // how keys are compared (see jobs.md)
  "warnings": [ "…" ] }                         // what the job skipped on purpose (e.g. a policy it cannot rewrite)
```

### Whole-bucket restic snapshots

A bucket can be snapshotted on a schedule into a restic repository: each
snapshot holds the bucket's objects as files plus a manifest
(`gos3-manifest.jsonl`) and the bucket's configuration
(`gos3-bucket.json`), so `restic restore` alone recovers the bucket's
content (see [encryption.md](encryption.md#whole-bucket-snapshots)).

| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/buckets/{name}/restic/snapshots` | GetBucketSnapshots | `{"config": SnapshotConfig \| null, "status": SnapshotStatus, "repository": RepositoryInfo \| null, "snapshots": Snapshot[], "error"?: "…"}` (snapshots cached 30 s; `error` says why the repository or its snapshots could not be read, the rest of the document is still valid) |
| PUT | `/buckets/{name}/restic/snapshots/config` | PutBucketSnapshots, also on `arn:gos3:admin:::backing/<name>` when `target.backing` names one | body `SnapshotConfig` → `SnapshotConfig` (validated; the repository is prepared) |
| DELETE | `/buckets/{name}/restic/snapshots/config` | PutBucketSnapshots | 204 (stops scheduling; existing snapshots stay; idempotent) |
| POST | `/buckets/{name}/restic/snapshots` | RunBucketSnapshot | 202 `Job` (`restic-snapshot`); 409 while one runs for the bucket; 400 without a schedule |
| DELETE | `/buckets/{name}/restic/snapshots/{id}` | PutBucketSnapshots | forgets one whole-bucket snapshot of this bucket → 204 (never a per-version archive snapshot: 400; `{id}` is the full snapshot ID, 64 lower-case hex digits; 404 for an unknown snapshot) |
| POST | `/buckets/{name}/restic/snapshots/{id}/restore` | RestoreBucketSnapshot on the bucket, plus CreateCopyJob on the target | body `{"target": {"bucket": "photos-restored", "create"?: {…as CopyJobSpec}}, "prefix"?: "", "onExisting"?: "fail" \| "merge", "configs"?: "safe" \| "none"}` → 202 `Job` (`restic-restore`). A merge never replaces a target version under object lock (a legal hold or unexpired retention): that key is skipped and named in the job's `warnings` |
| POST | `/buckets/{name}/restic/prune` | PutBucketSnapshots | 202 `Job` (`restic-prune`): removes data no snapshot references (takes restic's exclusive lock: archive writes wait) |

```jsonc
// SnapshotConfig (bucket configuration document "restic-snapshots"; replicates with the bucket)
{ "enabled": true,
  "interval": "24h",          // between snapshots (1h-30d)
  "at"?: "03:00",             // UTC time of day for intervals of 24h or more
  "consistent": true,         // versioned buckets: the versions current at the snapshot's start
  "prefix"?: "",              // only keys with this prefix
  "retention": { "keepLast": 7, "keepHourly": 0, "keepDaily": 7, "keepWeekly": 4, "keepMonthly": 12,
                 "keepYearly": 0, "keepWithin": "0s" },
  "target": { "kind": "bucket" }                     // the bucket's own restic repository (fs and wrapped buckets)
          | { "kind": "backing", "backing": "cold",  // an aux repository on that backing (any bucket;
              "repo": "snap-…" },                    //   repo is generated on first PUT and read-only)
  "prune": { "enabled": true, "interval": "168h", // 1h-366d
             "default": true } }  // read-only on GET, echoed back on PUT: enabled was not chosen and
                                  //   follows the bucket's archive (off while the encryption scope is "all");
                                  //   a PUT without "default" (or false) makes enabled an explicit choice
// Absent fields take the defaults shown (retention: keepLast 7, keepDaily 7, keepWeekly 4,
// keepMonthly 12); the retention must keep something besides the newest snapshot, which is always kept.

// SnapshotStatus
{ "lastAttemptAt": "…", "lastSuccessAt": "…", "lastSnapshot": "<id>", "lastError": "", "nextRunAt": "…",
  "running": "<job id>" }

// Snapshot
{ "id": "<64 hex>", "shortId": "…", "time": "…", "host": "gos3-snapshot", "tags": ["gos3-snapshot", "…"],
  "paths": ["…"], "parent"?: "<id>",
  "partial": false,           // always sent; true for a partial snapshot (tag gos3-partial)
  "summary"?: { "filesNew": 0, "filesChanged": 0, "filesUnmodified": 0, "dirsNew": 0, "dirsChanged": 0,
                "dirsUnmodified": 0, "dataAdded": 0, "totalFilesProcessed": 0, "totalBytesProcessed": 0 } }

// SnapshotJobStatus (status of restic-snapshot / restic-restore / restic-prune jobs)
{ "keys": 0, "bytes": 0, "materialized": 0, "partials": 0, "snapshot": "<id>", "forgotten": 0, "repository": "…",
  "warnings": [ "…" ] }       // problems that did not fail the job (retention, prune, configuration a restore skipped)
```

The default target is `bucket` for buckets on fs backings and wrapped
buckets of s3 backings, and otherwise an aux repository on the default
backing when it can hold one (an fs backing with a system directory, an s3
backing with a home and an encryption key); else the PUT must name a
backing. Retention is applied by gos3 to this bucket's whole-bucket
snapshots only (tags `gos3-snapshot` and the bucket's tag), never to
per-version archive snapshots.

The restore's `target.bucket` is read like the snapshot service reads the
body, and CreateCopyJob on it is evaluated before the service validates the
request. With `target.create`, CreateCopyJob is also required on the
effective target backing, which is pinned in the submitted spec as for
copy jobs. A restore limited by either the request's prefix or the
snapshot's own prefix skips the bucket policy with a warning; it cannot
install permissions for unrelated keys. An unknown bucket is 404 on every
snapshot endpoint.

Identity capabilities (WhoAmI): `viewJobs` (admin:ListJobs), `manageJobs`
(admin:CreateCopyJob), `updateJobs` (admin:UpdateJob), `manageSnapshots`
(admin:PutBucketSnapshots), `runSnapshots` (admin:RunBucketSnapshot),
`restoreSnapshots` (admin:RestoreBucketSnapshot), `revealResticCredentials`
(admin:GetResticCredentials), evaluated at service level like the others. The built-in `diagnostics` policy allows
ListJobs, GetJob and GetBucketSnapshots.

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
