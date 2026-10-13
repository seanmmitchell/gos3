# Operations

Running gos3 day to day: health checks, metrics, logs, backups, the lifecycle
worker and upgrades.

## Health and readiness

| Endpoint | Meaning | Response |
|---|---|---|
| `GET /_gos3/health` | Liveness: the process serves HTTP. | `200 {"status":"ok"}` |
| `GET /_gos3/ready` | Readiness: storage answers (bucket listing within 5 s), and the server is not shutting down. | `200 {"status":"ok"}`, or `503 {"status":"unavailable","error":"storage unavailable"}` (`"shutting down"` during shutdown). The result is reused for 2 s; the storage error is logged when readiness changes, not returned. |

Both are unauthenticated. `gos3 healthcheck [--ready] [--url URL]` wraps them
and exits 0 when healthy; the container image uses it as its `HEALTHCHECK`.
Kubernetes: liveness on `/_gos3/health`, readiness on `/_gos3/ready` (see
`deploy/kubernetes/statefulset.yaml`).

## Metrics

`GET /_gos3/metrics` serves the Prometheus text format (0.0.4). By default it
needs a signed request with `admin:GetStats` (the built-in `diagnostics` policy
grants it; anonymous requests get 401, insufficient permissions 403). Because
Prometheus cannot sign SigV4 requests, usually you set `metrics.public: true`
and restrict network access to the endpoint. `metrics.enabled: false` turns the
endpoint off (404).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `gos3_requests_total` | counter | `op`, `code_class` (`1xx`…`5xx`, `other`, `aborted`) | Requests by operation and status class. `aborted` is status 499: the client closed the request (for example an upload whose connection dropped); these are not counted as `4xx`. |
| `gos3_request_duration_seconds` | histogram | `op` | Latency; buckets 5 ms … 60 s. |
| `gos3_bytes_in_total` | counter | `op` | Request body bytes received. |
| `gos3_bytes_out_total` | counter | `op` | Response body bytes sent. |
| `gos3_inflight_requests` | gauge | | Requests being served. |
| `gos3_uptime_seconds` | gauge | | Seconds since start. |
| `gos3_start_time_seconds` | gauge | | Start time (Unix seconds). |
| `gos3_buckets` | gauge | | Number of buckets. |
| `gos3_discovery_peers` | gauge | | Discovered peers (only while discovery is enabled). |

`op` is the S3 operation name (`GetObject`, `PutObject`,
`CompleteMultipartUpload`, …), `Admin` for admin API requests,
`PreflightRequest` for CORS preflights and `Unknown` for requests rejected
before an operation was identified. At most 256 distinct ops are tracked; any
beyond that are counted as `Other`.

Example scrape configuration:

```yaml
scrape_configs:
  - job_name: gos3
    metrics_path: /_gos3/metrics
    static_configs:
      - targets: ["nas.lan:9000"]
```

Example alerts: `rate(gos3_requests_total{code_class="5xx"}[5m]) > 0`,
`rate(gos3_requests_total{code_class="aborted"}[5m])` for clients dropping
uploads, and
`histogram_quantile(0.99, sum by (le, op) (rate(gos3_request_duration_seconds_bucket[5m])))`
for latency.

The admin API's `GET /_gos3/admin/v1/stats` (and the UI's server overview) add
storage totals (objects, bytes, bytes stored on disk after compression,
versions, delete markers, pending uploads; cached for 30 s), Go runtime numbers and disk usage of the data directory
(fs backend on Linux and macOS).

## Logs

gos3 logs to stderr with Go's `log/slog`, as `text` (default) or `json`
(`logging.format`). The level and format can be changed at runtime.

- Startup prints a banner with the endpoints, UI and admin URLs, the region,
  the storage backend, and where the root credentials came from. A generated
  secret is shown only when it was just generated **and** standard error is a
  terminal; otherwise the banner names the file holding it
  (`<data>/.gos3/root-credentials.json`, or a temporary file deleted on exit
  for a server without a data directory). The secret is never written to the
  log. With JSON logs the banner is one structured line (with
  `credentialsFile`).
- A warning names each unknown `Host` the server is asked for (the first
  request per name; see `server.allowed_hosts` in
  [configuration.md](configuration.md#server)).
- **Access log**: one line per request (S3 and admin) with `request_id`, `op`,
  `method`, `path`, `status`, `bytes_in`, `bytes_out`, `duration_ms`,
  `remote`, `bucket`, `key`, `auth` (signing method), `user` and `access_key`,
  and `error` (the S3 error code). An upload the client aborted is logged with
  status `499` (nginx's "client closed request") and `client_aborted=true`;
  the metrics currently count it in the `4xx` class.
  It is logged at info level when `logging.access_log` is true (the default),
  otherwise at debug level. Query strings are never logged.
- The lifecycle worker logs every action at info level, and failures at warn
  level.
- `x-amz-request-id` in a response matches `request_id` in the log.

## The data directory

```
<data>/
└── .gos3/
    ├── meta.db                  bbolt metadata: buckets, configs, object versions, uploads (fs backend)
    ├── data/<bucket-id>/<xx>/…  object data files, immutable, possibly compressed (fs backend)
    ├── uploads/<upload-id>/…    multipart parts in progress (fs backend)
    ├── tmp/                     files being written (cleaned at startup)
    ├── orphans/<time>-<id>/     unreferenced files moved aside at startup (fs backend)
    ├── lock                     held by the running server
    ├── iam.json                 IAM users, keys, policies        (secret)
    ├── root-credentials.json    generated root keys, if any      (secret)
    ├── settings.json            runtime settings, after the first change
    ├── placements.json          which backings hold which buckets
    └── server-id                this server's discovery ID
```

With the memory backend and a data path, only the system files (`iam.json`,
`settings.json`, `server-id`, `root-credentials.json`) are kept; objects live in
memory. Every backend takes an exclusive lock on `.gos3/lock` while it runs
(and the fs backend also locks `meta.db`), so a second process using the same
directory fails at startup with "is another gos3 using this data
directory?". On platforms without `flock` or Windows file sharing the lock is
a no-op.

At startup the fs backend removes leftover temporary files from `tmp/`. Data
files, part files, bucket directories and upload directories that no
metadata references (left behind by a crash, or written after the backup a
restored `meta.db` came from) are **not** deleted: they are moved to
`.gos3/orphans/<UTC yyyymmddThhmmssZ>-<8 hex>/`, keeping their path relative
to `.gos3`, and a warning `recovery moved unreferenced files to the
quarantine` names the directory and the counts. A quarantine directory is
deleted at a later start once it is older than 7 days. If a move fails (for
example when `data/` is on another file system), the entry is left where it
is. Entries whose names gos3 never generates (such as `lost+found` when
`.gos3/data` is a mount point) are left alone and counted as
`unrecognized_entries`.

The fs backend refuses to start when `meta.db` is missing or empty while
`data/` or `uploads/` still contain object or part files: starting would
discard the index of every stored object. Restore `meta.db` from a backup, or
move `data/` and `uploads/` aside to start with an empty store.

## Multiple backings and replication

A server can store buckets on several backings (see
[configuration.md](configuration.md#storage-backings)), typically one fs
directory per disk. Each backing directory has its own `.gos3/` with its own
`meta.db`, data files and lock; the system state (`iam.json`,
`settings.json`, `placements.json`, ...) stays in the default backing's data
directory. Every bucket has a **placement**, managed in the web UI (bucket
settings, Placement tab, and the server's Storage page) or the admin API
([admin-api.md](admin-api.md#storage-backings-and-bucket-placement)):

| Mode | Writes | Reads | When the primary's backing fails |
|---|---|---|---|
| `single` | primary | primary | the bucket is unavailable |
| `active-passive` | primary; changed keys are copied to the replicas in the background (usually within milliseconds) | primary | reads fall back to a synced replica at once; writes fail until a replica is promoted (by hand, or automatically with `storage.replication.auto_failover`); changes not yet copied are lost on promotion |
| `active-active` | primary and every in-sync replica before the request is acknowledged | spread over the in-sync members | an in-sync replica is promoted automatically; nothing acknowledged is lost |

Copies are exact: version IDs, `LastModified` times, metadata, tags, ACLs,
object lock settings, bucket configuration (policy, CORS, lifecycle, ...) and
versioning state are the same on every member, so clients notice nothing
when the primary changes.

**Adding a replica** (or switching a bucket to a replicated mode) starts a
full copy in the background; the member shows `syncing` with a progress
count, then `in-sync`. An active-active replica receives synchronous writes
only once it is in sync. A replica whose backing fails, or that misses a
write, drops to background copying (`lagging`, `degraded` or `offline`) and
returns to synchronous writes by itself once it caught up. Removing a replica
deletes its copy in the background.

**Moving a bucket** (`POST /buckets/{name}/placement/move`, or Move in the UI)
copies it to the target backing while it stays readable and writable, then
performs a short cutover: new writes to the bucket wait while the writes in
flight finish (at most `storage.replication.cutover_timeout`) and the last
changes are copied; then the target takes the source's place and the source
copy is deleted. A move can be canceled until the cutover starts. If the
writes in flight do not finish in time, the cutover is retried later.

**Promoting a replica** makes it the primary. A normal promotion copies the
pending changes during a cutover first, so nothing is lost; a forced one
(`"force": true`, for a failed primary) switches at once.

Things to know:

- In-progress multipart uploads live on the primary only. After the primary
  changes (move, promotion, failover) they fail with `NoSuchUpload` and must
  be restarted.
- Changes waiting to be copied are kept in memory. After a restart every
  replica is compared with its primary in full, in the background (cost:
  reading the metadata of every object on both sides); active-active replicas
  serve reads and receive synchronous writes again when that finishes.
- Backing health: a backing is marked unhealthy when an operation on it fails
  with an I/O error, or when it does not list its buckets within 5 s at the
  periodic check (`storage.replication.health_interval`); it is healthy again
  after the next successful check. `GET /_gos3/admin/v1/backings` and the
  Storage page show the state and the last error.
- Removing a backing from the configuration: replicas on it are forgotten (a
  replica is promoted where it was the primary). gos3 refuses to start when a
  bucket's only copy was on it; configure the backing again, or remove the
  bucket from `placements.json` to forget it. A bucket found on a backing
  without an entry in `placements.json` is adopted where it is (that is how
  existing data directories keep working); a copy of a bucket that is placed
  elsewhere is ignored with a warning and never deleted automatically.
- Backups: with several backings, back up every backing directory together
  with the default data directory (one snapshot per file system).

## Backups

The data directory is the complete state of a server. Two approaches:

**1. File-level copy (complete, including IAM and settings).** `meta.db` and
the data files must be copied as one consistent snapshot:

```sh
systemctl stop gos3                      # or: docker stop gos3
tar -C /var/lib/gos3 -czf gos3-$(date +%F).tar.gz .
systemctl start gos3
```

To avoid downtime, take a crash-consistent snapshot of the file system (ZFS,
LVM or btrfs snapshot, or a cloud volume snapshot) and copy from it. gos3 treats
a snapshot like a power loss: committed objects are intact (with
`fsync_data: true`), and orphaned files are moved to `.gos3/orphans/` at the
next start. Do
not copy a live directory with `cp` or `rsync` without a snapshot: `meta.db`
can end up inconsistent with the data files.

Restore by stopping gos3, replacing the directory contents (keep the owner and
mode 0700) and starting it again. The restored server keeps its server ID,
IAM users and settings. If you restore only an older `meta.db` over newer
data, objects written after that backup are not destroyed: they land in
`.gos3/orphans/` and can be recovered by hand until the retention (7 days)
expires. Never delete `meta.db` to get past a "locked by another process"
error: stop the other process instead.

**2. Object-level copy (portable).** Mirror buckets with any S3 tool, for
example `rclone sync gos3:bucket backup:bucket` or
`mc mirror gos3/bucket backup/bucket`. This copies current objects (and, with
suitable flags, versions) to any S3 service or disk. It does not include IAM
users, policies, bucket configurations or object lock state, so also back up
`.gos3/iam.json` and `.gos3/settings.json` (both secret or private).

## Lifecycle worker

The worker applies bucket lifecycle rules (`PutBucketLifecycleConfiguration`):

- **Expiration**: `Days` and `Date` expire current versions. In versioned
  buckets this adds a delete marker; in unversioned buckets the object is
  deleted. `ExpiredObjectDeleteMarker` removes delete markers that have no
  remaining versions.
- **NoncurrentVersionExpiration**: `NoncurrentDays`, optionally keeping the
  newest `NewerNoncurrentVersions`.
- **AbortIncompleteMultipartUpload**: `DaysAfterInitiation`.
- Filters: prefix, tags (`And`), `ObjectSizeGreaterThan` / `ObjectSizeLessThan`.
- Transition rules are stored but not executed.
- Objects under object lock retention or legal hold are never removed.
- A key that changes during a pass (for example a delete marker removed
  mid-pass, so the walk reports more than one latest version) is skipped
  until the next pass.
- Every lifecycle delete is conditional and atomic in the storage backend
  (both built-in backends support this). A version is removed, or hidden
  behind a delete marker, only if at the moment of deletion it is still the
  version the pass examined (same version ID, `LastModified`, ETag and
  current/noncurrent state), still matches the rule (tags, size) and is not
  under retention or legal hold. Objects written, re-tagged or locked during
  a pass are therefore never removed or hidden by it.
- The abort pass handles multipart uploads in batches, so buckets with very
  many pending uploads are never loaded into memory at once.

Expiration times follow AWS: an object becomes eligible at the next 00:00 UTC
after `LastModified + Days`. A pass starts at startup and then repeats
`lifecycle.interval` (default 1 h) after the previous pass ends. Turn the
worker off or change the interval at runtime in Settings
(`lifecycle.enabled`, `lifecycle.intervalSeconds`); the change applies
immediately. Each action and a summary per pass are logged.

## Compression

gos3 can store object data compressed. Compression is transparent: clients
read back exactly the bytes they wrote, with the same ETag, checksums,
`Content-Length` and `Content-Range`, and listings report the original
sizes. Only the space used on disk (or in memory) changes.

Who decides:

- **The server administrator** sets the policy in Settings
  (`compression.mode`, see [configuration.md](configuration.md#compression)),
  through the web UI's server settings or `PUT /_gos3/admin/v1/config`:
  - `optional` (default): each bucket's own setting decides; buckets are
    uncompressed until enabled.
  - `enforced`: every bucket compresses new objects. Buckets cannot opt out.
  - `disabled`: nothing new is compressed and buckets cannot enable it.
- **Per bucket**, with `admin:PutBucketCompression` on the bucket: the web
  UI's bucket settings, or `PUT /_gos3/admin/v1/buckets/<name>/compression`
  with `{"enabled": true}` (see [admin-api.md](admin-api.md#buckets-management-helpers-regular-bucket-ops-use-the-s3-api)).
  While the administrator enforces or disables compression, a bucket setting
  that contradicts it is refused with `409 Conflict`; settings stored before
  the policy changed stay in place but have no effect until the mode is
  `optional` again. `DELETE` on the same path returns the bucket to the
  server default.

What is compressed: new objects written by PutObject, CopyObject (following
the destination bucket), PostObject and CompleteMultipartUpload, unless

- the object is smaller than `compression.min_size` (4 KiB) and its size is
  known up front,
- its `Content-Type` is in `compression.skip_content_types` (by default
  JPEG, PNG and other compressed images, `video/*`, `audio/*`, archives and
  zip-based office formats), or
- it has a compressing `Content-Encoding` (`gzip`, `br`, `zstd`, …): the
  client compressed it already.

Changing the policy or a bucket's setting applies to the next write; stored
objects stay as they are and remain readable either way, so compressed and
uncompressed versions coexist in a bucket. To compress existing objects,
rewrite them: copy them into a bucket that compresses (CopyObject compresses
by the destination bucket), or copy each object onto itself. S3 only allows
a copy onto itself that changes something, typically
`x-amz-metadata-directive: REPLACE`, which replaces the metadata, so pass the
metadata you want to keep.

How it is stored: DEFLATE (`compress/flate`, level `compression.level`, 1 by
default) in independently compressed blocks of 256 KiB with a CRC-32C per
block, so a range request inflates at most one block per 256 KiB it returns
instead of everything before it, and damaged data is detected and reported as
a read error rather than served. Blocks that do not shrink are stored as is;
data that does not compress (already compressed content that the media type
did not reveal) is detected and mostly skipped, costing little CPU. The
admin API's bucket usage and `GET /stats` report both sizes: `bytes` (the
content) and `storedBytes` (on disk), plus `compressedVersions`; the web UI
shows the saving next to the bucket size. Multipart uploads keep their parts
uncompressed until completion, which compresses them while concatenating.

Compression costs CPU on writes and some on reads. As a guide, on source
code and documentation one core compresses about 280 MB/s at level 1 (to
32 % of the size), 165 MB/s at level 6 (27 %) and 40 MB/s at level 9 (26 %),
and reads back about 330 MB/s; each request uses one core. It pays off for text, logs, CSV/JSON, source code,
uncompressed images and backups of such data; it does nothing for media and
archives, which the default skip list leaves alone.

## Graceful shutdown and restarts

SIGTERM or SIGINT stops accepting connections, lets in-flight requests finish
for up to `server.shutdown_timeout` (30 s), then stops discovery (sending a
beacon "bye"), the lifecycle worker and the storage backend. A second signal
exits immediately. `/_gos3/ready` returns 503 during shutdown. Give the process
manager a longer stop timeout than `shutdown_timeout`; the systemd unit uses
45 s, as does the Kubernetes manifest's `terminationGracePeriodSeconds`.

## Upgrades

1. Read the release notes.
2. Back up the data directory (see above).
3. Verify the new archive or image with `gh attestation verify`
   ([releasing.md](releasing.md#verifying-a-release)).
4. Replace the binary or image (on Kubernetes, raise `newTag`, and `digest`
   if you pin one, in `kustomization.yaml`), and restart.

The fs metadata database records a format version. A gos3 version that does
not understand it refuses to start rather than risk the data, so rolling back
after an upgrade that changed the format means restoring the backup.
Compressed objects are recorded in a newer record format: a gos3 version
without compression support starts, but fails to read those objects (and
reports their metadata as corrupt), so roll back only after rewriting them
uncompressed, or restore the backup. Runtime
settings in `settings.json` and IAM data in `iam.json` are kept across upgrades.
`gos3 version` (or `GET /_gos3/info`) shows the running version.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `SignatureDoesNotMatch` behind a proxy | The proxy changes the `Host` header or re-encodes the path; see [deployment.md](deployment.md#reverse-proxy). |
| `AccessDenied: Signature Version 2 is not supported` | AWS CLI v1 presigning; set `s3.signature_version = s3v4`. |
| `RequestTimeTooSkewed` | The client's clock is off by more than 15 minutes. |
| Web UI: `NetworkError` or "blocked by CORS" | The server's `cors.allowed_origins` does not include the UI origin; see [web-ui.md](web-ui.md#cors). |
| Web UI: multipart upload fails with `MissingETag` | `ETag` is not in the exposed CORS headers (other S3 services). |
| Startup: `… is in use (is another gos3 using this data directory?)` or `metadata database … is locked by another process` | Another gos3 uses the data directory. Stop it; do not delete `meta.db`. |
| Startup: `refusing to start with an empty index` | `meta.db` is missing or empty but object files exist; restore `meta.db` from a backup, or move `data/` and `uploads/` aside. |
| Startup warning: `recovery moved unreferenced files to the quarantine` | Files no metadata references were moved to `.gos3/orphans/`; inspect them if objects are missing. |
| Startup: `settings.json: …` | The runtime settings file is corrupt or invalid; fix or remove it. |
| Peers show `unverified` with a TLS error | The peer uses a certificate that this server does not trust. |
| Configuration changes have no effect | `settings.json` overrides them for runtime keys; see [configuration.md](configuration.md#runtime-settings-settingsjson). |
