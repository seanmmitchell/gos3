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
- **Simple ops**: one binary, `gos3 serve --data ./data`, JSON/YAML/env/flags,
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
| `internal/storage/` | Backend contract (+ per-bucket capabilities `capabilities.go`, approved GOVERNANCE bypass `lockctx.go`) + generic list helpers (`list.go`) | contract / storage-memory |
| `internal/storage/index/` | in-memory object index (skip list, version stamps, versioning semantics, walks, uploads), generic over the data payload; shared by memory and s3store | storage-memory |
| `internal/storage/memory/` | in-memory backend (on `index`) | storage-memory |
| `internal/storage/fs/` | filesystem backend (bbolt index + data files) | storage-fs |
| `internal/storage/s3account/` | `s3` backend: one `storage.Backend` per S3 account connection; registry of native and wrapped buckets, home and containers, server-wide claims on remote locations, remote bucket browser, migration of earlier layouts | backings |
| `internal/storage/s3store/` | a gos3 store in a remote S3 bucket/prefix, used as an s3 backing's home or container of wrapped buckets (`index` in memory, journaled to S3; tagged data blobs; wrapped keys; restic repositories; aux namespaces for other components' records) | backings |
| `internal/storage/native/` | native buckets of s3 backings: objects stored as-is in remote buckets, versioning and object lock passed through, gos3 metadata in `x-amz-meta-gos3-meta` and overlay records, multipart staging | backings |
| `internal/storage/localstate/` | local records of s3 backings without a home (bbolt `state.db` + staged files under the system directory) | backings |
| `internal/storage/restic/` | restic runner shared by fs and s3store: prepare/init/validate, stdin archiver, lazy shared restores (+ `restictest` fake restic) | storage-encryption |
| `internal/storage/multi/` | multi-backing router: bucket placement, replication (active-passive / active-active), online moves, release, per-bucket capability checks, backing health, tolerant open, live backing configuration | storage-multi |
| `internal/storage/storagetest/` | backend conformance suite | storage-conformance |
| `internal/s3client/` | minimal SigV4 S3 client for s3 backings: objects, versions, multipart, buckets, regions, object lock, tagging (+ `s3clienttest` in-process remote with fault injection and provider quirks) | backings |
| `internal/storage/compress/` | seekable block-compressed data format used by the fs, memory and s3 backends | storage-compression |
| `internal/storage/crypt/` | authenticated AES-256-GCM chunked stream format for natively encrypted (SSE-S3) data | storage-encryption |
| `internal/compression/` | compression policy: bucket setting document, server mode, per-object decision | storage-compression |
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

One listener (default `:9000`, optional TLS) serves everything. Before
routing, response writes get a progress deadline, the TLS state is taken from
the connection, and the `Host` header is checked against the server's names
(`server.allowed_hosts`; unknown hosts get no anonymous access, UI or public
metrics, or are refused outright when the list is set; see
docs/security.md#dns-rebinding-and-the-host-header):

1. Virtual-hosted-style request (Host is `<bucket>.<domain>` for a configured
   domain, and is not itself a configured domain) → **S3 handler**, always
   (even if the path starts with `/_gos3/`; that is then an object key).
2. Path starts with `/_gos3/`:
   - `/_gos3/admin/v1/...` → admin API (404 when `admin.enabled` is false)
   - `/_gos3/ui/...` → embedded web UI (SPA, `index.html`/`200.html` fallback)
   - `/_gos3/info` → public `discovery.Info` JSON (CORS `*`; requires auth when `publicInfo` is false)
   - `/_gos3/health` (liveness) and `/_gos3/ready` (backend `ListBuckets` within 5 s and the default backing healthy; 503 otherwise and while shutting down) → `{"status":"ok"}`
   - `/_gos3/metrics` → Prometheus text (admin:GetStats unless `metrics.public`; 404 when `metrics.enabled` is false)
   - anything else → 404 JSON
3. Anonymous browser `GET /` with `Accept: text/html` and UI redirect enabled → 302 to `/_gos3/ui/`.
4. Everything else → **S3 handler**.

`_gos3` can never be a bucket name (underscore is invalid), so there is no clash.

Anonymous path-style `/minio/health/live` and `/minio/health/ready` without
a query string are also probes; signed and virtual-host requests still go
to S3. Management can be disabled independently of S3; doing so also disables
UI and the saved settings overlay. The `noui` build tag excludes embedded
assets and defaults UI off. Every discovery switch defaults off.

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
- Limits: key ≤ 1024 bytes UTF-8 (keys objects are created at: XML 1.0
  characters only, see `validateNewObjectKey`); user metadata ≤ 2 KiB; ≤ 10 object tags
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
   from `X-Forwarded-For` (rightmost entry that is not a trusted proxy) and
   the last `X-Forwarded-Proto` value when `server.trust_proxy_headers` is
   set and the peer is in `server.trusted_proxies` (package `clientip`,
   shared with the admin API). PublicAccessBlock and
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
- Optional Backend extensions (`optional.go`, `encryption.go`), discovered
  by type assertion; the built-in backends (fs, memory, s3store) implement all of them except
  where noted:
  - `ConditionalDeleter.DeleteObjectIf` looks up the addressed version and
    deletes it only if a check on it passes, atomically (same transaction or
    lock). The S3 DeleteObject/DeleteObjects `If-Match`, object lock and
    authorized-version (`s3:ExistingObjectTag`) paths and the lifecycle
    worker use it; without it the S3 layer checks just before the delete and
    the lifecycle worker skips permanent deletes (it still adds delete
    markers).
  - `CheckedDeleter.DeleteObjectChecked` is `DeleteObjectIf` whose check
    is also consulted when the key has no versions (and the delete then
    adds the delete marker of a versioned bucket), so a delete authorized
    on "no object here" still creates its marker but not over an object
    that appeared meanwhile.
  - `ConditionalMetaUpdater.UpdateObjectMetaIf` is `UpdateObjectMeta` with
    such a check, so ACL, tagging, retention and legal hold updates apply
    to exactly the version that was authorized; without it the S3 layer
    compares the metadata it can see in the update function.
  - `ConfigViewer.ViewBucketConfig` lends a stored configuration document
    to a callback without copying it (the S3 layer hashes lifecycle
    documents per request to recognise the cached parse).
  - `UploadWalker.WalkMultipartUploads` walks in-progress uploads in listing
    order (key, Initiated, UploadID) from an `UploadPosition`, without loading
    all of a bucket's uploads.
  - `CompressingBackend.NewObjectWriterWith` / `CompleteMultipartUploadWith`
    store a version's data compressed (`WriteOptions.Compression`), see
    [Compression](#compression), and encrypted (`WriteOptions.Encryption`:
    `AES256` or `restic`; an unsupported provider fails, never falls back to
    plaintext). `ObjectMeta.Encryption` of the result records how the data
    is actually stored.
  - In `encryption.go`: `EncryptionBackend.PrepareEncryption` prepares a
    bucket for encrypted writes (the fs backend creates its key and, for
    restic, initializes its repository; `errors.ErrUnsupported` for a
    provider the backend cannot store), and
    `ValidateEncryptionSettings` checks a provider before the admin API
    saves it. The memory backend implements `EncryptionBackend` for
    `AES256` only. See [encryption.md](encryption.md).
  - In `encryption_providers.go`: `EncryptionProvidersBackend` reports which
    SSE-S3 providers a bucket can use, so the admin API and web UI offer
    only those: fs reports `restic` only while the restic executable is on
    `PATH`, memory reports `AES256` only, s3store reports nothing without a
    KEK and `AES256` (plus `restic` when the executable is on `PATH`) with
    one, the s3 backend reports its container's for a wrapped bucket and
    nothing for a native one, and the multi router reports the intersection
    over the bucket's backings.
  - In `capabilities.go`: `BucketCapabilities` reports what one bucket
    supports (`BucketCaps`: kind, `Import` of copied versions, a copyable
    `History`, versioning, object lock, compression), and with
    `NewBucketCaps` what a bucket about to be created with given options
    would support, without I/O. `CapsOf` / `NewCapsOf` fall back to full
    capabilities for backends implementing `VersionImporter` (fs, memory).
    `BucketReleaser` forgets a bucket without deleting its data,
    `HealthProber` replaces the `ListBuckets` health probe, and
    `KindReporter` reports the bucket kinds a backing offers and its
    default. The s3 backend implements all four.
  - In `lockctx.go`: `WithGovernanceBypass` marks a context whose
    GOVERNANCE bypass the S3 layer has approved; backends that enforce
    retention themselves (native buckets, through their remote service)
    pass it on, the others ignore it.
  - Remote-location errors: `ErrBucketUnavailable` (one bucket's remote
    location fails; wraps `ErrBackingUnavailable`, so 503, but never marks
    the backing down), `ErrRemoteDenied` (403), `ErrRemoteRejected` (500
    naming at most the remote's error code; the S3 layer logs the full
    error and sends clients fixed messages), `ErrInvalidTarget` (a
    `BucketTarget` the backing cannot honour, 400, without detail on the
    S3 API) and `ErrRemoteBucketExists` (wraps
    `ErrBucketExists`; 409 `BucketAlreadyExists`).
  `storage.ListObjects`, `ListObjectVersions` and `ListUploads` return
  entries trimmed to the fields a listing reports (`storage.ListingInfo`).
  `uploads.go` builds on them: `storage.ListUploads` (S3
  ListMultipartUploads paging: prefix, delimiter, key/upload-ID markers),
  `storage.WalkMultipartUploads` and `storage.CountMultipartUploads`, each
  falling back to `ListMultipartUploads` for a backend without
  `UploadWalker`.
- `fs` layout: `<root>/.gos3/meta.db` (bbolt: buckets, configs, object
  versions, uploads, and a per-bucket `upk` index of multipart uploads in
  listing order: key, initiation time, upload ID. The index is created, and
  checked against the upload records, at `Open`, so existing data
  directories are indexed on first start with no schema version change.
  Paged upload listings, the lifecycle abort pass and usage counts seek it
  instead of decoding every upload; and a per-bucket `imp` index, imported
  version ID -> version position, for versions stored through
  `storage.VersionImporter` with an ID from another backend, which does not
  embed the position the way generated IDs do. It is created by a bucket's
  first import, so existing databases need no migration and the schema
  version is unchanged) +
  `<root>/.gos3/data/<bucket-id>/<2-hex>/<random-id>` data
  files (written to `<root>/.gos3/tmp/` then renamed; fsync configurable) +
  multipart parts under `<root>/.gos3/uploads/<upload-id>/<part>`. Data files
  are immutable; replaced/deleted versions are unlinked after the metadata
  transaction commits (open readers keep working on POSIX). If the commit
  reports an error after the transaction itself succeeded (e.g. an fsync
  error), the new data or part file is kept rather than deleted, because the
  commit may have persisted; the client still gets an error. Encryption
  (see [encryption.md](encryption.md)) adds `<root>/.gos3/keys/<bucket-id>`
  (the bucket's master key; data, staging and part files are encrypted
  with keys derived from it or fresh ones) and
  `<root>/.gos3/keys/<bucket-id>.restic.json` (restic repository location
  and password), local restic repositories in
  `<root>/.gos3/restic/<bucket-id>/` (always local; a recovery record
  naming an `s3:` repository fails closed) and restic's cache in
  `<root>/.gos3/restic-cache/`. Keys, recovery records and repositories
  stay after a bucket is deleted. The data file of a restic-archived
  version holds only its snapshot ID. The restic plumbing itself lives in
  `internal/storage/restic`, parameterised by a `Location` (repository,
  password, environment, options, cache directory) and a `RecoveryStore`.
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
  walk (every built-in backend). The lifecycle worker skips a key whose walk reports
  more than one latest version until the next pass.
- `memory` keeps each bucket in an `internal/storage/index` index (guarded
  by an RW mutex per bucket), with object data as byte slices. Imported
  versions get a fresh internal stamp (so they sort newest) but keep their
  foreign ID; lookups by such IDs fall back to scanning the key's versions.
  Every mutation computes its exact effect as an `index.Change`, which is
  what makes the index journalable for s3store.
- Compression: see [Compression](#compression). A compressed fs version
  record uses record format 4 (format 3 plus the algorithm and the stored
  size); uncompressed records stay format 3, so databases without
  compressed objects remain readable by older builds. An encrypted version
  record uses record format 5 (format 4 plus `ObjectMeta.Encryption`,
  compressed or not), which builds before object encryption cannot read
  (rollback: [operations.md](operations.md#upgrades)).
- `s3store` (a gos3 store in a remote bucket, [`docs/backings.md`](backings.md)):
  a full `storage.Backend` implementing every optional extension fs and
  memory implement, with everything under `P = <bucket>/<prefix>` of one
  remote bucket and no local state (upload spool and restic temporaries
  aside). The s3 backend (`s3account`, below) opens one per container:
  its home and each further container of wrapped buckets.
  - **Layout**: `gos3.json` (marker: format, store ID, role `home` or
    `container`, `blobTags`, KEK check value; the legacy gateway flag stays
    readable for the migration), `meta/lease.json`,
    `meta/journal/<epoch>/<seq>`, `meta/snapshots/<lsn>-<epoch>`,
    `data/<bucket-id>/<data-id>`, `parts/<upload-id>/<n>-<id>`,
    `keys/<bucket-id>.key` (+ `.restic.json`, without password),
    `restic/<bucket-id>/`, `gateway/parts/<ns>/<name>` (staged blobs of aux
    namespaces).
  - **Create modes** (`Options.Create`): `CreateIfNoData` (default, homes:
    a prefix holding `meta/`, `data/`, `parts/`, `keys/`, `restic/` or
    `gateway/` objects without a marker is `ErrUnmanagedPrefix`; other keys
    are tolerated), `CreateIfEmpty` (new containers: any key but a zero-byte
    folder placeholder and probe leftovers is `ErrPrefixNotEmpty`, checked
    again right before the marker is written) and `MustExist` (registered
    or attached containers: `ErrMarkerMissing`). `Options.ExpectBackingID`
    refuses a marker of another store (`ErrMarkerMismatch`), so a swapped
    location is never used.
  - **Aux namespaces** (`AuxState(ns)`): journaled key/value records plus
    staged blobs for other components, with the method set of
    `AuxStore`: the s3 backend's registry (`account`) and the native
    store's state (`gateway`, the namespace legacy gateway backings used).
  - **Metadata engine** (`engine.go`, `journal.go`, `lease.go`,
    `marker.go`, `replay.go`, `snapshot.go`): metadata is an `index` held in
    memory; every change is a journal record (its exact `index.Change`, so
    replay is deterministic), and a write is acknowledged only after the
    batch holding it was PUT. A group-commit sequencer writes batches one
    after the other into slots `meta/journal/<epoch>/<seq>` with
    `If-None-Match`; an ambiguous result (412, 5xx, transport error) is
    resolved by reading the slot back and comparing owner and SHA-256.
    Point reads wait for in-flight LSNs; listings may show in-flight
    writes. A stalled journal or a fencing fail-stops the engine, followed
    by a background reload.
  - **Fencing**: one process owns a prefix. The lease is renewed with
    `If-Match` and its age is judged by the service's clock only. A new
    owner (new epoch) writes a **seal** into the next free slot of every
    older epoch; seals are never deleted, so a zombie's next batch gets
    412, it reads the seal and fences itself. At the first open a probe
    refuses services that ignore conditional writes.
  - **Snapshots**: chunked gzip NDJSON (each chunk carrying its own LSN,
    checksummed trailer), written in the background per `SnapshotPolicy`
    (journal bytes, batch count, age; and at Close). The two newest are
    kept, and journal batches older than the older one are deleted, except
    seals and the last slot of each epoch. Documented envelope: a few
    million versions per store (container).
  - **Data path**: compress, encrypt, spool, upload, then the journal
    record. Readers are lazy ranged GETs; open readers are refcounted per
    blob and the delete of a superseded blob is deferred until the last
    one closes. CompleteMultipartUpload composes server-side
    (UploadPartCopy) only for plain data. Every blob carries
    `x-amz-meta-gos3-store: <store ID>`. An age-based recovery pass (24 h
    default, hourly) deletes an unreferenced blob only when its name is
    one the store generates and a HEAD shows its own store ID: untagged
    and foreign objects are never deleted (counted at debug level), which
    is what makes containers in shared buckets and existing prefixes safe.
  - **Keys**: per-bucket master keys are wrapped (AES-256-GCM with AAD) by
    the KEK (`Options.KEK`) and created with `If-None-Match`; a bucket
    marked encrypted whose key is missing is `ErrEncryptionUnavailable`
    and the key is never regenerated; a KEK whose check value differs from
    the marker's refuses Open (`ErrWrongKEK`).
  - **Restic**: repositories are `s3:<endpoint>/<bucket>/<P>restic/<id>`
    through restic's own s3 backend (`-o s3.bucket-lookup=path|dns`,
    credentials in `AWS_*`), with the password derived from the master
    key; the snapshot ID is kept in the index.
  - **Startup**: `s3store.Open` never fails on remote I/O. The backend
    loads in the background and answers `storage.ErrBackingUnavailable`
    (503) until loaded or while the service is unreachable. Invalid
    options, a lease held by another live server (`ErrInUse`), a wrong KEK,
    an unmanaged prefix, a service without conditional writes and a
    damaged journal are hard errors reported through the backing's health.
    `PrepareConnection` verifies new connection settings (marker, KEK)
    before a live reconfiguration swaps them in.
- `s3account` (the `s3` backend, [`docs/backings.md`](backings.md)): one
  `storage.Backend` per s3 backing, i.e. per connection to an S3 account
  (endpoint + credentials), holding buckets of two kinds
  (`storage.BucketKind`):
  - **Registry** (`registry.go`): records in the aux namespace `account` of
    the home's s3store, or, for a backing without a home, of a local
    `internal/storage/localstate` database (`<sys>/s3/<backing>/state.db`
    plus staged files, mode 0700, locked while open): `k/<container>/`
    (containers other than the home), `b/<bucket>/` (every bucket: kind,
    container or remote bucket and region, whether gos3 created it, cached
    `BucketInfo`), `c/<bucket>/<config>/` (native buckets' configuration
    documents), `d/default-container`. Loaded into memory, written through
    under `adminMu`; a record of a newer schema is refused.
  - **Wrapped** buckets live in a **container**, an s3store: the home
    (implicit ID `home`, opened `CreateIfNoData`) or a registered one
    (`dedicated`: a remote bucket gos3 created; `existing`: a prefix of an
    existing bucket; opened `MustExist` with the recorded store ID, or
    `CreateIfEmpty` when added). Containers load in the background; at most
    64 per backing. Errors of a container other than the home are
    classified (`containers.go`): transport errors and 5xx stay
    backing-level, anything else (not loaded, `ErrInUse`, wrong KEK, a
    deleted or forbidden bucket) becomes `storage.ErrBucketUnavailable`.
  - **Native** buckets are served by one `internal/storage/native` store
    whose state (multipart records, staged small parts, overlay records)
    is the aux namespace `gateway`. The store keeps no bucket records: it
    resolves each bucket's binding (remote bucket, region, owned) and
    recorded versioning/lock on every operation, reports contradicting
    remote answers (`OnDrift`), and the backing then refreshes the record.
    Remote errors map as in `native/errors.go` (bucket-level failures →
    `ErrBucketUnavailable`, object-level 403 → `ErrRemoteDenied`, other 4xx
    → `ErrRemoteRejected`, 5xx/transport → `ErrBackingUnavailable`).
  - **Dispatch** (`dispatch.go`): wrapped buckets forward everything to
    their container; native buckets go to the native store, with bucket
    configuration from the registry (object lock configuration and
    versioning are mirrored to the remote first). Native buckets refuse
    imports (`errors.ErrUnsupported`) and encryption, and store compressed
    writes uncompressed. `ReleaseBucket` detaches a native bucket without a
    remote DELETE; `DeleteBucket` deletes the remote bucket only when gos3
    created it.
  - **Claims** (`claims.go`, `NewClaims`): a server-wide in-memory registry
    of remote locations, keyed by service (the endpoint's host and port,
    normalised like config's overlap check) and lower-cased bucket: a
    native claim conflicts with any claim on its bucket of that service,
    home and container claims with each other when their prefixes nest.
    Conflicts found while loading make the container or bucket unavailable
    (logged); new ones are `ErrInvalidTarget`.
  - **Remote manager** (`remote.go`, CONTRACT `RemoteManager`, used by the
    admin API): `RemoteBuckets` (the account's buckets with their uses and
    status; incomplete when ListBuckets is denied), `CheckAdoptable` (no
    claim, no `gos3.json` at the root or in the first 100 top-level
    prefixes), `CreateRemoteBucket`, `AddContainer` / `RemoveContainer` /
    `SetDefaultContainer`, `Info`. Adoption is only reachable through the
    admin API (`BucketTarget.Adopt`).
  - **Reconcile and migration** (`reconcile.go`, `migrate.go`): records are
    written after (creation) or deleted after (deletion) the container
    change, and each loaded container is reconciled: a bucket without a
    record is recorded as wrapped (which is also how the home of a plain
    s3 backing of an earlier build migrates), a record whose bucket is gone
    is dropped. A legacy gateway home (`Marker.Gateway`) is migrated bucket
    by bucket into native records bound to the same-named remote bucket
    (as adopted), then its marker flag is cleared (`ClearLegacyGateway`).
    Every step is idempotent.
  - **Connection** (`connection.go`): `PrepareConnection` verifies the
    home's and every loaded container's marker and KEK (and, without a
    home, the credentials and up to 20 native remote buckets); its commit
    swaps the client pool (per-region clones), every store and the native
    store. `SetDefaultKind` changes the default kind in place.

  The server (`internal/server/s3backing.go`) opens one `s3account` per s3
  backing with the server-wide claims, waits up to 15 s at startup for
  them to be ready (a backing without a home is ready at once) and starts
  the rest unhealthy; a backing added live must load within 30 s, and its
  errors are classified for the admin API (`s3store.ErrInUse`,
  `ErrFenced`, `s3account.ErrClaimed` and `localstate.ErrLocked` → 409;
  wrong KEK, marker mismatch or missing marker, unmanaged or non-empty
  prefix, no conditional writes, other invalid targets and remote
  auth/bucket/region errors → 400; `storage.ErrBackingUnavailable` → 503). Each s3 backing gets a temporary
  directory under `os.TempDir()` (spool, restic cache and restores),
  removed at Close.
- CreateBucket's `x-gos3-backing` header picks the primary backing of a new
  bucket (`storage.ErrInvalidBacking` -> 400 for unknown names);
  `BucketInfo.Backing` reports it. On s3 backings `x-gos3-bucket-kind`,
  `x-gos3-container` and `x-gos3-remote-bucket` build a
  `storage.BucketTarget` (`CreateBucketOptions.Target`; `Adopt` is never
  set by the S3 layer), and `BucketInfo.Kind` / `Location` report the
  result (HeadBucket: `x-gos3-bucket-kind`).
- `list.go` turns `WalkObjects`/`WalkVersions` into S3 listings (prefix,
  delimiter grouping, markers, truncation). When a common prefix `p` is found
  it restarts the walk after `p + "\xff"` (UTF-8 never contains 0xFF) so huge
  "directories" are skipped in O(log n) per prefix.

### Multiple backings (`internal/storage/multi`)

The server's backend is always a `multi.Router` over one or more named
backings (`default` = `storage.backend` with `storage.path` or
`storage.s3`, plus `storage.backings`; each is an fs, memory or s3
(`s3account`) backend). It implements `storage.Backend` and every optional
extension by routing each bucket to its placement: a primary backing and
optional replicas, in mode `single`, `active-passive` (changed keys copied
in the background, read fallback, promotion by hand or with
`auto_failover`) or `active-active` (writes teed to every in-sync replica
before they are acknowledged, reads spread, automatic failover). Placements
are persisted in `<data>/.gos3/placements.json`; buckets without one are
adopted where they are found at startup.

- Copies use the optional `storage.VersionImporter` extension, which stores
  a version under the ID, `LastModified` and position it has on the source.
  A key is copied by comparing both version lists (`syncKey`): versions
  missing on the source are deleted, the common oldest-first prefix is kept
  (metadata updated where it differs) and the rest is replaced. Copying is
  therefore idempotent, and every repair is just another comparison: changed
  keys are queued per replica (in memory; a full comparison runs after a
  restart, after a write to a replica fails and when a queue overflows).
- **Per-bucket capabilities** (`caps.go`): a bucket can receive copies
  (be a replica or move target) when the target backing reports `Import`
  for it (`storage.NewCapsOf` with `Import` set and the move's
  `BucketTarget`, no I/O), and be the primary of replicas or a move source
  when its own copy reports `History` (`storage.CapsOf`). A native bucket
  has neither, so it can be neither replicated nor moved, and replicas and
  move targets on s3 backings are created with `CreateBucketOptions.Import`
  set, which makes them wrapped buckets. `checkNewMembers`, the default
  replicas of new buckets and live backing configuration use these checks.
  A move may name the target's container (`Router.Move` with a
  `BucketTarget`), persisted with the move in `placements.json`.
- **Release** (`Router.ReleaseBucket`): a bucket with a single copy and no
  move in progress, on a backing implementing `storage.BucketReleaser`, is
  forgotten without the background deletion that follows DeleteBucket (a
  native bucket keeps its remote bucket and objects).
- A per-bucket gate counts writes in flight; cutovers (end of a move,
  graceful promotion, an active-active replica switching to synchronous
  writes, failover) close it, wait for in-flight writes, copy the last
  queued keys and switch. Per-key locks keep an active-active replica's
  versions in the primary's order.
- A backing is unhealthy after an I/O error (any error that is not a
  storage sentinel, a context error or the caller's callback error) or a
  failed periodic probe (`storage.HealthProber` when the backend has one,
  else `ListBuckets`). `storage.ErrBucketUnavailable`, `ErrRemoteDenied`,
  `ErrRemoteRejected` and `ErrInvalidTarget` concern one bucket and never
  mark the backing down: reads fall back to another copy on
  `ErrBucketUnavailable` and an active-active follower failing with it is
  demoted to background copying. Reads try unhealthy backings last; walks
  fall back only before the first entry was delivered.
- **Tolerant open**: at `Open`, `reconcile()` lists each backing with a
  timeout. A backing that fails (an s3 backing whose service is down or
  still loading) is marked down and its placements are kept (it is assumed
  to still hold its buckets); adopting its unplaced buckets is deferred
  until it comes back up (`adoptLocked`, from `onBackingUp`). `ListBuckets`
  falls back to the cached `BucketInfo` of a bucket whose backing does not
  answer instead of failing as a whole. `ErrNotSupported` and
  `ErrInvalidBacking` are not backing failures.
- **Live configuration** (`ConfigureBackings` with a `BackingOpener`):
  backings are compared with their current definition by identity
  (`config.BackingDefinition.Identity`: fs path; s3 `home:<bucket>/<prefix>`,
  or `local:<name>` for a backing without a home). A different identity
  replaces the backing (it must be unused and empty, and a new backing must
  hold no buckets); a change of an s3 backing's connection (endpoint,
  region, credentials, KEK, addressing, part size) or default kind goes
  through `BackingOpener.Reconfigure` (`s3account.PrepareConnection`
  verifies the home's and containers' markers and KEK and returns a commit
  that swaps the connection). The revision is a hash of the redacted
  settings plus a generation counter. `Backing` and `BackingStatus` carry
  `Location` (an s3 backing's home, `s3://bucket/prefix/`); `BackingStatus`
  also carries the `Kinds` and `DefaultKind` an s3 backing reports
  (`storage.KindReporter`), read at status time; `Router.BackingBackend`
  gives the admin API the backend itself (`s3account.RemoteManager`).
- Multipart uploads live on the primary only (not copied).
- Compression: the router implements `storage.CompressingBackend` by
  forwarding to the primary; active-active replicas receive the same write
  options, and background copies of a compressed version are stored
  compressed too (`storage.CompressingImporter`, at the default level).
- Encryption: `PrepareEncryption` / `ValidateEncryptionSettings` go to the
  bucket's primary; the providers a bucket can use are the intersection of
  `providersOf()` (`EncryptionProvidersBackend`) over its backings, and
  `checkEncryptionTarget` refuses a replica or move target that cannot
  store a provider the bucket uses (restic to memory, AES256 or restic to
  a KEK-less s3 backing). Copies of an encrypted version are
  encrypted on the receiving backing with that backing's own key and
  repository (`WriteOptions.Encryption`); a backing that cannot store the
  provider fails the copy instead of storing plaintext.

System state lives in `<data>/.gos3/`: `lock` (an exclusive lock held by the
running server, whatever the backend; a no-op on platforms without
`flock`/Windows sharing), `iam.json`, `placements.json` (bucket placements),
`backings.json` (backings configured live through the admin API; it
overrides `storage.backings` and `storage.default_backing` at the next
start while management is enabled), `settings.json` (written on
the first runtime settings update, then it wins over file/env/flags for the
Settings fields), `server-id`, `root-credentials.json` (only when root
keys were generated) and `s3/<backing>/` (the records of s3 backings
without a home). With a memory or s3 default backing this needs
`storage.path`; without it the system state is ephemeral (a warning is
logged), and s3 backings without a home are refused. Bucket names cannot start with `.`,
so no clash.

### Compression

Object data can be stored compressed, transparently: the S3 layer computes
ETags and checksums over the uncompressed stream as always, and
`ObjectInfo.Size`, `OpenObject` and seeking are in uncompressed terms;
`ObjectInfo.Compression` / `StoredSize` (and `PhysicalSize()`) say how a
version is stored.

- **Policy** (`internal/compression`): the server's `Settings.Compression`
  (`mode` optional | enforced | disabled, `level`, `minSize`,
  `skipContentTypes`) and the bucket's setting, a JSON document
  `{"enabled": bool}` under the bucket config key `compression`
  (`storage.ConfigCompression`), managed by the admin API. `Effective`
  combines them; `Decide` refines per object (empty or smaller than
  `minSize` when the size is known, skipped media type, compressing
  `Content-Encoding` → uncompressed).
- **S3 layer** (`internal/s3/compression.go`): `storeObject` (PutObject,
  CopyObject, PostObject) and CompleteMultipartUpload ask the policy and
  use the `CompressingBackend` methods; backends without them store
  everything uncompressed.
- **Format** (`internal/storage/compress`): 256 KiB blocks, each raw
  DEFLATE (or stored as is when it does not shrink by 1/32, with a
  backoff that skips attempts on incompressible data), then an index of
  12-byte entries (cumulative end offset with a raw flag, CRC-32C of the
  uncompressed block) and a 24-byte footer (magic `gos3blk1`, block size,
  block count, uncompressed size). Readers locate any block with one
  `ReadAt` of the index, verify every block's CRC and keep one decoded
  block. Empty content is never compressed.
- **fs**: the compressor sits between the object writer and the temporary
  file; multipart completion compresses while concatenating the parts
  (parts themselves are stored uncompressed). The data file's size is the
  record's `StoredSize`, checked at open like `Size` is for plain files.
- **memory**: keeps the compressed bytes in the version.

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
- Admin API actions are `admin:<Action>`, evaluated against
  `arn:gos3:admin:::user/<name>`, `…:policy/<name>`, `…:bucket/<name>`,
  `…:backing/<name>` (the remote buckets and containers of an s3 backing:
  `ListRemoteBuckets`, `CreateRemoteBucket`, `AdoptRemoteBucket`,
  `ManageContainers`) or service level (see `docs/admin-api.md`); policy
  documents may use these types with wildcards; s3 and admin ARNs never
  cross-match. Admin request bodies must be signed (hex SHA-256 or signed
  streaming).

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
  lock, encryption, ACL, placement); IAM users/keys/policies; server
  settings; storage backings (live configuration; for s3 backings the
  remote bucket browser and containers); metrics dashboard; discovery
  (per-server peers, scans, local discovery, one-click "add server").

## Build, test & conventions

- Go ≥ 1.26.9 (`go.mod`'s `go` line; toolchain auto). `gofmt`, `go vet` clean. Std `log/slog`.
  Stdlib `net/http` (Go 1.22+ mux patterns OK for the admin API).
- Allowed third-party Go deps (already in go.mod): `go.etcd.io/bbolt`,
  `github.com/miekg/dns`, `gopkg.in/yaml.v3`, and for tests only
  `github.com/aws/aws-sdk-go-v2/...` and `github.com/minio/minio-go/v7`.
- Tests: `go test ./internal/<pkg>/...` for your package; `-race` must pass.
- `make build` (UI + binary), `make ui`, `make test`, `make e2e`, `make e2e-web`,
  `make compat`, `make dev`, `make docker`. User documentation: `README.md` and
  `docs/{configuration,backings,encryption,deployment,operations,security,s3-compatibility,web-ui}.md`.
