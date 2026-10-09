# gos3 admin API (v1)

Base path: `/_gos3/admin/v1` on the S3 listener. JSON in/out
(`Content-Type: application/json`). Timestamps are RFC3339. Go
`time.Duration` fields (e.g. discovery `beaconInterval`, `peerTtl`, scan
`timeout`) are **integers in nanoseconds**.

## Authentication & authorization

- Requests are signed with AWS SigV4 (Authorization header **or** presigned
  query) using the same access keys as the S3 API. Service name `s3`, any
  region. Body hash: hex SHA-256 or `UNSIGNED-PAYLOAD`.
- Each endpoint requires an IAM action `admin:<Action>` (listed below),
  checked with `iam.Manager.Authorize` (resource `*`). Root is always allowed;
  the built-in `admin` policy allows all; `diagnostics` allows `ServerInfo`,
  `GetStats`, `GetDiscovery`.
- Endpoints marked *any* only require a valid (non-anonymous) identity.
- CORS: preflight `OPTIONS` is answered without auth using the global
  `cors` settings; actual responses carry the matching CORS headers.

## Errors

Non-2xx responses: `{"error": {"code": "<Code>", "message": "<text>"}}`.

| HTTP | code |
|---|---|
| 400 | `InvalidRequest`, `InvalidName`, `InvalidPolicy`, `InvalidSettings` |
| 401 | `Unauthenticated` (anonymous or bad signature — message carries the S3 error code, e.g. `SignatureDoesNotMatch`) |
| 403 | `AccessDenied` |
| 404 | `NotFound` |
| 405 | `MethodNotAllowed` |
| 409 | `Conflict` (exists / in use / bucket not empty) |
| 500 | `InternalError` |

## Endpoints

### Identity
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/whoami` | *any* | `WhoAmI` |
| GET | `/whoami/keys` | *any* | `{"keys": AccessKey[]}` (own keys, no secrets) |
| POST | `/whoami/keys` | *any* | body `{"description"?}` → 201 `AccessKey` (with `secretKey`). Not for root. |
| DELETE | `/whoami/keys/{accessKey}` | *any* | 204 (own keys only; cannot delete the key used to sign this request) |

```jsonc
// WhoAmI
{
  "user": "alice", "accessKey": "AKIA…", "isRoot": false,
  "policies": ["readwrite"],
  "capabilities": {           // derived by evaluating the actions below
    "admin": false,           // any admin:* allowed
    "viewInfo": true,         // admin:ServerInfo
    "viewStats": false,       // admin:GetStats
    "manageConfig": false,    // admin:UpdateConfig
    "manageUsers": false,     // admin:CreateUser
    "managePolicies": false,  // admin:PutPolicy
    "manageBuckets": false,   // admin:ForceDeleteBucket
    "discovery": false,       // admin:GetDiscovery
    "scan": false             // admin:ScanNetwork
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
  "metrics": metrics.Snapshot,          // see internal/metrics
  "storage": { "buckets": 3, "objects": 1200, "bytes": 123456789,
               "versions": 1300, "versionBytes": 133456789,
               "deleteMarkers": 4, "pendingUploads": 1,
               "computedAt": "…" },     // cached up to 30s
  "runtime": { "goroutines": 42, "heapAllocBytes": 1234, "sysBytes": 5678, "gcPauseTotalNs": 1 },
  "disk": { "path": "/data", "totalBytes": 1, "freeBytes": 1, "usedBytes": 1 } // fs backend only, else null
}
```

`config.Settings` (see `internal/config`):

```jsonc
{
  "name": "gos3-1", "region": "us-east-1", "publicUrl": "http://10.0.0.5:9000",
  "domains": ["s3.local"], "publicInfo": true, "uiRedirect": true,
  "cors": { "allowedOrigins": ["*"], "allowedHeaders": ["*"], "exposeHeaders": ["ETag"], "maxAgeSeconds": 3600 },
  "discovery": { "enabled": true, "advertise": true, "mdns": true, "beacon": true,
                 "beaconGroup": "239.255.73.83:7373", "beaconInterval": 10000000000,
                 "seeds": [], "dnsSrv": [], "peerTtl": 60000000000,
                 "allowScan": true, "maxScanHosts": 1024 },
  "logging": { "level": "info", "format": "text", "accessLog": true },
  "lifecycle": { "enabled": true, "intervalSeconds": 3600 },
  "metricsPublic": false
}
```

### Users & access keys
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/users` | ListUsers | `{"users": iam.User[]}` |
| POST | `/users` | CreateUser | body `iam.UserSpec` (+ optional `"createKey": true`) → 201 `{"user": User, "key"?: AccessKey}` |
| GET | `/users/{name}` | GetUser | `iam.User` |
| PATCH | `/users/{name}` | UpdateUser | body `iam.UserUpdate` → `iam.User` |
| DELETE | `/users/{name}` | DeleteUser | 204 |
| POST | `/users/{name}/keys` | CreateAccessKey | body `{"description"?, "accessKey"?, "secretKey"?}` → 201 `iam.AccessKey` (incl. secret) |
| PATCH | `/keys/{accessKey}` | UpdateAccessKey | body `{"status": "Active"\|"Inactive"}` → 204 |
| DELETE | `/keys/{accessKey}` | DeleteAccessKey | 204 |

### Policies
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/policies` | ListPolicies | `{"policies": iam.NamedPolicy[]}` |
| GET | `/policies/{name}` | GetPolicy | `iam.NamedPolicy` |
| PUT | `/policies/{name}` | PutPolicy | body `{"description"?, "document": PolicyDocument}` → `iam.NamedPolicy` |
| DELETE | `/policies/{name}` | DeletePolicy | 204 (409 if attached, 400 if builtin) |
| POST | `/authz/simulate` | SimulatePolicy | body `{"user": "alice" \| "" (anonymous), "action": "s3:GetObject", "bucket": "b", "key": "k", "conditions"?: {…}}` → `iam.Decision` (bucket policy of `bucket` is included) |

### Buckets (management helpers; regular bucket ops use the S3 API)
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/buckets` | ListBuckets | `{"buckets": BucketSummary[]}`; `?usage=true` includes usage (cached 30s) |
| GET | `/buckets/{name}` | GetBucket | `BucketSummary` with usage |
| DELETE | `/buckets/{name}?force=true` | ForceDeleteBucket | deletes all versions, delete markers, uploads and configs, then the bucket → 204. Without `force` behaves like S3 DeleteBucket (409 if not empty). |

```jsonc
// BucketSummary
{ "name": "photos", "createdAt": "…", "region": "us-east-1",
  "versioning": "Enabled" | "Suspended" | "", "objectLock": false, "owner": "alice",
  "configs": { "policy": true, "cors": false, "lifecycle": true, "tagging": false,
               "encryption": false, "website": false, "objectLock": false, "acl": "private" },
  "public": false,          // bucket policy grants anonymous access or ACL public-*
  "usage": storage.BucketUsage | null }  // {"objects","bytes","versions","versionBytes","deleteMarkers","pendingUploads"}
```

### Discovery
| Method | Path | Action | Response |
|---|---|---|---|
| GET | `/discovery` | GetDiscovery | `{"enabled": bool, "config": discovery.Config, "self": Info, "peers": discovery.Peer[]}` |
| POST | `/discovery/probe` | GetDiscovery | body `{"url": "http://host:9000"}` → `discovery.Peer` (502 `ProbeFailed` with message on failure) |
| POST | `/discovery/scan` | ScanNetwork | body `discovery.ScanRequest` → `discovery.ScanResult` (403 when `allowScan` is false; 400 if too many hosts) |

## Public endpoints (no `/admin/v1` prefix; served by `internal/server`)

| Method | Path | Auth | Response |
|---|---|---|---|
| GET | `/_gos3/info` | none if `publicInfo`, else any | `discovery.Info`, `Access-Control-Allow-Origin: *` |
| GET | `/_gos3/health` | none | `{"status":"ok"}` |
| GET | `/_gos3/ready` | none | `{"status":"ok"}` or 503 `{"status":"unavailable","error":"…"}` |
| GET | `/_gos3/metrics` | none if `metricsPublic`, else admin:GetStats | Prometheus text |
