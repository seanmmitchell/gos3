# gos3 architecture

gos3 is a single-binary, S3-compatible object storage server written in Go,
with an optional web UI (Nuxt 4 + TypeScript) that can be embedded in the
binary or run standalone to manage one or more S3 servers, and dynamic
discovery of gos3 servers on the network.

## Goals

- **S3 compatibility first**: works unmodified with the AWS SDKs (Go v2, JS v3,
  Python boto3), AWS CLI, `mc`, rclone, Terraform. Correct XML, headers,
  error codes, SigV4 (header, presigned, aws-chunked streaming incl. trailers).
- **Full featured**: versioning, multipart (incl. copy), conditional
  reads/writes, ranges, checksums (CRC32/CRC32C/CRC64NVME/SHA1/SHA256, full
  object + composite), tagging, bucket policies + IAM users/policies, canned
  ACLs, CORS, lifecycle expiration, object lock (retention/legal hold),
  presigned URLs, browser POST uploads, virtual-hosted-style addressing.
- **Simple ops**: one binary, `gos3 serve --data ./data`, YAML/env/flags,
  Prometheus metrics, JSON admin API, embedded UI, graceful shutdown.
- **Discovery**: servers advertise via mDNS + UDP beacon, follow static seeds
  and DNS SRV, and can scan networks; the UI aggregates all of it.

## Repository layout & ownership

| Path | Purpose | Owner work item |
|---|---|---|
| `cmd/gos3/` | CLI entrypoint (`serve`, `version`, `config print\|init\|env`, `discover`, `keygen` (alias `keys gen`), `healthcheck`) | integration |
| `internal/server/` | wires everything, top-level mux, public endpoints, TLS, shutdown | integration |
| `internal/config/` | config types (contract) + loading (file/env/flags) + settings persistence | integration |
| `internal/webui/` | `embed` of the built UI (`dist/`), SPA fallback handler | integration |
| `internal/s3err/` | S3 error codes (contract; append-only) | — |
| `internal/storage/` | Backend contract + generic list helpers (`list.go`) | contract / storage-memory |
| `internal/storage/memory/` | in-memory backend | storage-memory |
| `internal/storage/fs/` | filesystem backend (bbolt index + data files) | storage-fs |
| `internal/storage/storagetest/` | backend conformance suite | storage-conformance |
| `internal/auth/` | SigV4 verification, aws-chunked decoding, POST policy, client signer | auth |
| `internal/iam/` | users, keys, policies, bucket policy parsing, authorization engine | iam |
| `internal/s3/` | S3 HTTP layer (see file ownership below) | s3-core + s3-* |
| `internal/lifecycle/` | lifecycle config parsing + expiration worker | s3-bucketconfig |
| `internal/metrics/` | request metrics + Prometheus exposition | s3-core |
| `internal/admin/` | admin JSON API | admin |
| `internal/discovery/` | mDNS, beacon, seeds, DNS SRV, scan, peer table | discovery |
| `web/` | Nuxt 4 web UI | ui-* |
| `test/e2e/` | end-to-end tests with aws-sdk-go-v2 + minio-go against a real server | integration |
| `test/compat/` | boto3 / AWS CLI compatibility suite against a real binary (`run.sh`) | compat |
| `docs/` | architecture, admin API, discovery protocol, user docs | all |
| `Dockerfile`, `docker-compose.yml`, `deploy/`, `.github/` | container image, compose demo, systemd/Kubernetes, CI and release workflows | devops-docs |

Exported names in contract files (files whose package comment says
CONTRACT) are fixed: implement them, don't rename/re-shape them. Additive
exported helpers in your own files are fine.

## Top-level routing (`internal/server`)

One listener (default `:9000`, optional TLS) serves everything:

1. Virtual-hosted-style request (Host is `<bucket>.<domain>` for a configured
   domain) → **S3 handler**, always (even if the path starts with `/_gos3/`;
   that is then an object key).
2. Path starts with `/_gos3/`:
   - `/_gos3/admin/v1/...` → admin API
   - `/_gos3/ui/...` → embedded web UI (SPA, `index.html`/`200.html` fallback)
   - `/_gos3/info` → public `discovery.Info` JSON (CORS `*`; requires auth when `publicInfo` is false)
   - `/_gos3/health` (liveness) and `/_gos3/ready` (backend reachable within 5 s; 503 while shutting down) → `{"status":"ok"}`
   - `/_gos3/metrics` → Prometheus text (admin:GetStats unless `metrics.public`; 404 when `metrics.enabled` is false)
   - anything else → 404 JSON
3. Anonymous browser `GET /` with `Accept: text/html` and UI redirect enabled → 302 to `/_gos3/ui/`.
4. Everything else → **S3 handler**.

`_gos3` can never be a bucket name (underscore is invalid), so there is no clash.

## S3 layer (`internal/s3`)

### File ownership inside the package

| File(s) | Owner | Content |
|---|---|---|
| `server.go`, `router.go`, `context.go`, `middleware*.go`, `response.go`, `xml.go`, `cors.go`, `checksum.go`, `validate.go`, `authz.go`, `ops.go` | s3-core | handler, routing, op detection, auth/authz, CORS, XML/error writing, checksum + hashing readers, common validators, metrics/access-log middleware |
| `handlers_bucket.go` (+ `_test`) | s3-bucket | ListBuckets, CreateBucket, DeleteBucket, HeadBucket, GetBucketLocation, ListObjects (v1), ListObjectsV2, ListObjectVersions |
| `handlers_object.go`, `handlers_object_*.go` (+ `_test`) | s3-object | PutObject, CopyObject, GetObject, HeadObject, DeleteObject, DeleteObjects, Get/Put/DeleteObjectTagging, Get/PutObjectAcl, Get/PutObjectRetention, Get/PutObjectLegalHold, GetObjectAttributes, PostObject (browser form upload), RestoreObject/SelectObjectContent (NotImplemented) |
| `handlers_multipart.go` (+ `_test`) | s3-multipart | CreateMultipartUpload, UploadPart, UploadPartCopy, CompleteMultipartUpload, AbortMultipartUpload, ListParts, ListMultipartUploads |
| `handlers_bucketconfig.go`, `handlers_bucketconfig_*.go` (+ `_test`) | s3-bucketconfig | versioning, bucket tagging, CORS config, policy + policyStatus, lifecycle, encryption, bucket ACL, object-lock config, ownership controls, website, notification, logging, public access block, replication, accelerate, request payment |

s3-core creates **every** handler method as a stub returning
`s3err.ErrNotImplemented` in the owning file, so the package always compiles;
area owners replace the stubs in their own files only.

### Operation detection

| Op | Method | Target | Discriminator |
|---|---|---|---|
| ListBuckets | GET | `/` | |
| CreateBucket / DeleteBucket / HeadBucket | PUT / DELETE / HEAD | bucket | no subresource |
| ListObjects / ListObjectsV2 | GET | bucket | `list-type=2` → V2 |
| ListObjectVersions | GET | bucket | `?versions` |
| ListMultipartUploads | GET | bucket | `?uploads` |
| GetBucketLocation | GET | bucket | `?location` |
| Bucket subresources | GET/PUT/DELETE | bucket | `?versioning ?tagging ?cors ?policy ?policyStatus ?lifecycle ?encryption ?acl ?object-lock ?ownershipControls ?website ?notification ?logging ?publicAccessBlock ?replication ?accelerate ?requestPayment` |
| DeleteObjects | POST | bucket | `?delete` |
| PostObject | POST | bucket | `Content-Type: multipart/form-data` |
| PutObject / CopyObject | PUT | object | `x-amz-copy-source` → Copy |
| UploadPart / UploadPartCopy | PUT | object | `?partNumber&uploadId` (+ copy source) |
| GetObject / HeadObject / DeleteObject | GET / HEAD / DELETE | object | |
| Object subresources | GET/PUT/DELETE | object | `?tagging ?acl ?retention ?legal-hold ?attributes` |
| CreateMultipartUpload / CompleteMultipartUpload | POST | object | `?uploads` / `?uploadId` |
| AbortMultipartUpload / ListParts | DELETE / GET | object | `?uploadId` |
| RestoreObject / SelectObjectContent | POST | object | `?restore` / `?select` → NotImplemented |
| Preflight | OPTIONS | any | CORS |

Unknown subresources → `NotImplemented`. Unsupported method for a resource → `MethodNotAllowed`.

### Conventions

- XML namespace `http://s3.amazonaws.com/doc/2006-03-01/`; responses start with `<?xml version="1.0" encoding="UTF-8"?>`.
- Every response carries `x-amz-request-id` (16 uppercase hex), `x-amz-id-2`,
  `Server: gos3`, `Date`. Errors render `<Error><Code/><Message/><Resource/><RequestId/><HostId/>…</Error>`;
  HEAD errors have no body.
- ETags are stored unquoted and always sent quoted (`"…"`). Multipart ETag =
  `md5(concat(binary part md5s))-N`.
- Times: `LastModified` in XML is RFC3339 with milliseconds (`2006-01-02T15:04:05.000Z`);
  HTTP `Last-Modified` is RFC1123 GMT.
- `x-amz-version-id` header is sent only when the bucket's versioning status is
  not Unversioned (then `null` versions are reported as `null`).
- Limits: key ≤ 1024 bytes UTF-8; user metadata ≤ 2 KiB; ≤ 10 object tags
  (key ≤ 128, value ≤ 256 chars); ≤ 50 bucket tags; single PUT ≤ 5 GiB
  (configurable); parts 1..10000, each ≥ 5 MiB except the last; max-keys ≤ 1000;
  DeleteObjects ≤ 1000 keys.
- Bucket names: 3–63 chars, lowercase letters/digits/`-`/`.`, start+end alnum,
  not an IP address, no `..`, no `xn--` prefix, no `-s3alias` / `--ol-s3` suffix.
- Storage errors map to S3 errors in one place (s3-core `mapStorageError`).

### Request pipeline (s3-core)

1. Request ID, panic recovery, metrics (`metrics.Registry.Begin`) and access log.
2. Parse bucket/key (path-style, or virtual-hosted-style using `Settings.Domains`),
   detect operation (table above).
3. CORS: answer `OPTIONS` preflights from the bucket CORS config (fallback to
   global `Settings.CORS`); add CORS response headers to actual requests.
4. Authenticate with `auth.Verifier.Authenticate` (→ `auth.Result`, whose
   `Body` replaces `r.Body`). PostObject authenticates from the form instead.
5. Authorize with `iam.Manager.Authorize` using the op's IAM action
   (e.g. `s3:GetObject`, `s3:GetObjectVersion` when `versionId` is present,
   `s3:ListBucket`, `s3:PutBucketPolicy` …), bucket policy (parsed &
   cached), canned ACL (bucket ACL; for object reads denied at first, retry with
   the object's ACL) and condition context (`aws:SourceIp`,
   `aws:SecureTransport`, `s3:prefix`, `s3:delimiter`, `s3:max-keys`,
   `s3:x-amz-acl`, `s3:VersionId`, `aws:username`, `aws:CurrentTime`, …).
   Handlers that touch a second resource (CopyObject/UploadPartCopy source,
   DeleteObjects per key) call the core's authorize helper themselves.
   `aws:SourceIp` / `aws:SecureTransport` come from `RemoteAddr` / TLS, or
   from the first `X-Forwarded-For` entry and `X-Forwarded-Proto` when
   `server.trust_proxy_headers` is set. PublicAccessBlock and
   ObjectOwnership (BucketOwnerEnforced disables ACLs) are applied here too.
6. Dispatch to the handler with a per-request context struct (bucket, key, op,
   auth result, identity, request id, versioning status cache, …).

### Checksums (s3-core `checksum.go`)

A hashing reader computes MD5 (always) + the requested checksum algorithm(s)
while streaming into storage, enforces declared `Content-Length` /
`x-amz-decoded-content-length`, and at EOF validates `Content-MD5`,
`x-amz-checksum-<algo>` headers and trailer checksums (`auth.Result.Trailer`).
Mismatch → `BadDigest` / `InvalidDigest`/ `XAmzContentSHA256Mismatch`; the
storage writer is then aborted. Supported algorithms: CRC32, CRC32C,
CRC64NVME (poly 0xad93d23594c93659 reflected = `crc64.MakeTable(0x9a6c9329ac4bc9b5)`;
check value of "123456789" = 0xae8b14860a799888), SHA1, SHA256. Multipart:
COMPOSITE checksum = checksum of concatenated binary part checksums with
`-N` suffix; FULL_OBJECT for CRC algorithms combines part CRCs (or recomputes
on completion). `x-amz-checksum-mode: ENABLED` on GET/HEAD returns stored checksums.

## Storage (`internal/storage`)

See the doc comments in `storage.go`. Key points:

- Backends are dumb and atomic: the S3 layer computes ETag/checksums and passes
  them in `ObjectMeta` at `Commit`; the backend owns versioning bookkeeping
  (null versions, delete markers, latest pointer), conditional commit checks
  and listing order.
- Version IDs for versioned writes must be unique, URL-safe, ≤ 64 chars and
  never `null`.
- `fs` layout: `<root>/.gos3/meta.db` (bbolt: buckets, configs, object
  versions, uploads) + `<root>/.gos3/data/<bucket-id>/<2-hex>/<random-id>` data
  files (written to `<root>/.gos3/tmp/` then renamed; fsync configurable) +
  multipart parts under `<root>/.gos3/uploads/<upload-id>/<part>`. Data files
  are immutable; replaced/deleted versions are unlinked after the metadata
  transaction commits (open readers keep working on POSIX). If the commit
  reports an error after the transaction itself succeeded (e.g. an fsync
  error), the new data or part file is kept rather than deleted, because the
  commit may have persisted; the client still gets an error.
- `fs` recovery (startup): cleans `tmp/`; unreferenced data files, part
  files, bucket directories and upload directories are **moved**, not
  deleted, to `<root>/.gos3/orphans/<UTC yyyymmddThhmmssZ>-<8 hex>/`
  (keeping their path relative to `.gos3`), and a quarantine directory is
  deleted at a later start once older than `fs.Options.OrphanRetention`
  (default `fs.DefaultOrphanRetention`, 7 days; not exposed in the config
  file). A failed move leaves the entry in place. Entries with names gos3
  never generates (e.g. `lost+found`) are left alone. The pass logs a Warn
  `recovery moved unreferenced files to the quarantine` with the path and the
  counts (`unrecognized_entries`, `purged_quarantine_dirs`, ...). `Open`
  refuses to start when `meta.db` is missing or empty while `data/` or
  `uploads/` still hold object or part files.
- `WalkVersions` sets `IsLatest` only on the first entry delivered for a key,
  never on a later entry of the same key, even if the key changes during the
  walk (both backends). The lifecycle worker skips a key whose walk reports
  more than one latest version until the next pass.
- `memory` keeps everything in maps guarded by RW mutexes.
- `list.go` turns `WalkObjects`/`WalkVersions` into S3 listings (prefix,
  delimiter grouping, markers, truncation). When a common prefix `p` is found
  it restarts the walk after `p + "\xff"` (UTF-8 never contains 0xFF) so huge
  "directories" are skipped in O(log n) per prefix.

System state lives in `<data>/.gos3/`: `lock` (an exclusive lock held by the
running server, whatever the backend; a no-op on platforms without
`flock`/Windows sharing), `iam.json`, `settings.json` (written on
the first runtime settings update, then it wins over file/env/flags for the
Settings fields), `server-id`, and `root-credentials.json` (only when root
keys were generated). With the memory backend this needs `storage.path`;
without it the server is fully ephemeral. Bucket names cannot start with `.`,
so no clash.

## Auth & IAM

- `auth.Verifier` supports: Authorization header SigV4; presigned query SigV4
  (`X-Amz-Expires` ≤ 7 days); payloads: hex SHA-256, `UNSIGNED-PAYLOAD`,
  `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`(-TRAILER) with chunk signatures,
  `STREAMING-UNSIGNED-PAYLOAD-TRAILER` (default of recent AWS SDKs with
  checksums). Clock skew ≤ 15 min. Any credential-scope region is accepted
  unless `StrictRegion`. Constant-time signature compare.
- SigV2 is not supported (AccessDenied with an explanatory message).
- Root credentials come from config (`GOS3_ROOT_ACCESS_KEY` /
  `GOS3_ROOT_SECRET_KEY`) or are generated on first start and stored in
  `<data>/.gos3/root-credentials.json`; IAM users/keys/policies are stored in
  `iam.json`. Built-in policies: `admin`, `readwrite`, `readonly`,
  `writeonly`, `diagnostics`. Custom policies use the IAM policy language
  (Allow/Deny, Action/NotAction with wildcards, Resource/NotResource ARNs with
  wildcards and `${aws:username}`, Condition operators String*, Numeric*,
  Date*, Bool, IpAddress/NotIpAddress, Null, ArnLike, `…IfExists`,
  `ForAnyValue:`/`ForAllValues:`).
- Bucket policies (`?policy`) are evaluated together with identity policies;
  explicit Deny wins; anonymous requests are allowed only by bucket policy
  (`Principal: "*"`) or public canned ACLs.
- Admin API actions are `admin:<Action>` (see `docs/admin-api.md`).

## Admin API

JSON REST API at `/_gos3/admin/v1`, SigV4-authenticated with the same
credentials as S3. Specified in [`docs/admin-api.md`](admin-api.md).

## Discovery

mDNS (`_gos3._tcp`), UDP multicast beacon (`239.255.73.83:7373`), static
seeds, DNS SRV and active scans; every candidate is verified by fetching
`/_gos3/info`. Specified in [`docs/discovery.md`](discovery.md).

## Web UI (`web/`)

- Nuxt 4 (`app/` srcDir), TypeScript strict, `ssr: false` (SPA), Nuxt UI v4
  (Tailwind v4) + Pinia, icons bundled locally (no runtime CDN / remote fonts:
  the embedded UI must work offline).
- Two deployment modes from one codebase:
  1. **Embedded** — `NUXT_APP_BASE_URL=/_gos3/ui/ nuxt generate`, copied into
     `internal/webui/dist` and served by gos3 at `/_gos3/ui/`. The UI detects
     the hosting gos3 (same-origin `GET /_gos3/info`) and offers it as the
     default connection; same origin ⇒ no CORS needed.
  2. **Standalone** — `nuxt build` → Node server (`node .output/server/index.mjs`).
     Manages any number of servers (gos3 or generic S3). Extra Nitro server
     routes provide *local* discovery from the UI host (mDNS browse + beacon
     query + network scan). Browser → server requests need the server's CORS
     to allow the UI origin (`cors.allowed_origins`).
- Server connections (`{id, name, endpoint, region, accessKey, secretKey,
  kind: 'gos3'|'s3', forcePathStyle, remember}`) live in a Pinia store persisted
  to localStorage (or sessionStorage when "remember" is off), can be
  imported/exported as JSON, and can be preconfigured via `public/servers.json`
  or runtime config `NUXT_PUBLIC_SERVERS` (JSON array).
- All requests are signed in the browser with SigV4 (`aws4fetch`), for both the
  S3 API and the admin API. Downloads use presigned URLs; uploads use
  PutObject or multipart (parallel parts) with progress.
- Features: multi-server dashboard; bucket list/create/delete; object browser
  (folders via delimiter, upload/drag&drop, download, delete, rename/copy,
  metadata, tags, versions, presigned share links); bucket settings
  (versioning, policy editor with templates, CORS, lifecycle, tags, object
  lock, encryption, ACL); IAM users/keys/policies; server settings; metrics
  dashboard; discovery (per-server peers, scans, local discovery, one-click
  "add server").

## Build, test & conventions

- Go ≥ 1.25 (toolchain auto). `gofmt`, `go vet` clean. Std `log/slog`.
  Stdlib `net/http` (Go 1.22+ mux patterns OK for the admin API).
- Allowed third-party Go deps (already in go.mod): `go.etcd.io/bbolt`,
  `github.com/hashicorp/mdns`, `gopkg.in/yaml.v3`, and for tests only
  `github.com/aws/aws-sdk-go-v2/...` and `github.com/minio/minio-go/v7`.
- Tests: `go test ./internal/<pkg>/...` for your package; `-race` must pass.
- `make build` (UI + binary), `make ui`, `make test`, `make e2e`, `make e2e-web`,
  `make compat`, `make dev`, `make docker`. User documentation: `README.md` and
  `docs/{configuration,deployment,operations,security,s3-compatibility,web-ui}.md`.
