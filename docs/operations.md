# Operations

Running gos3 day to day: health checks, metrics, logs, backups, the lifecycle
worker, background jobs and restic snapshots, and upgrades.

## Health and readiness

| Endpoint | Meaning | Response |
|---|---|---|
| `GET /_gos3/health` | Liveness: the process serves HTTP. | `200 {"status":"ok"}` |
| `GET /_gos3/ready` | Readiness: storage answers (bucket listing within 5 s), the default backing is healthy, and the server is not shutting down. An unavailable additional backing (for example an unreachable [s3 backing](backings.md)) leaves the server ready; an unavailable s3 default backing does not. | `200 {"status":"ok"}`, or `503 {"status":"unavailable","error":"storage unavailable"}` (`"shutting down"` during shutdown). The result is reused for 2 s; the storage error is logged when readiness changes, not returned. |

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
    ├── keys/                    per-bucket encryption keys and restic recovery records (fs backend) (secret)
    │   ├── <bucket-id>              a bucket's encryption master key
    │   ├── <bucket-id>.restic.json  its restic repository location and password
    │   └── aux/<name>[.restic.json] key and recovery record of an aux restic repository
    ├── restic/<bucket-id>/      local restic repositories (fs backend)
    ├── restic-aux/<name>/       aux restic repositories: whole-bucket snapshots of other buckets (fs backend)
    ├── restic-cache/            restic's local repository cache (fs backend)
    ├── restic-stage/            stage of whole-bucket snapshots (default restic.snapshots.stage_dir)
    ├── restic-snapshots.json    state of the snapshot scheduler: last and next runs per bucket
    ├── jobs/jobs.json           copy, mirror and restic jobs: specs, states, progress, checkpoints
    ├── jobs/<id>/               a job's own state (state.db), deleted with the job
    ├── lock                     held by the running server
    ├── iam.json                 IAM users, keys, policies        (secret)
    ├── root-credentials.json    generated root keys, if any      (secret)
    ├── settings.json            runtime settings, after the first change
    ├── placements.json          which backings hold which buckets
    ├── backings.json            backings configured in the web UI, after the first change (secret: s3 credentials)
    ├── s3/<backing>/            records of an s3 backing without a home (state.db, staged/)
    └── server-id                this server's discovery ID
```

Encrypted objects cannot be read without `keys/`, so back it up together
with `meta.db` and the data (see
[encryption.md](encryption.md#keys-and-recovery)). Keys, restic recovery
records and local restic repositories stay after their bucket is deleted.
Each additional fs backing has its own `keys/`, `restic/`, `restic-aux/`
and `restic-cache/`. Restic repositories are always local to an fs backing
(`restic/<bucket-id>`, `restic-aux/<name>`). `jobs/`,
`restic-snapshots.json` and `restic-stage/` belong to the server and exist
only in the system directory.

With the memory backend and a data path, only the system files (`iam.json`,
`settings.json`, `placements.json`, `backings.json`, `jobs/`,
`restic-snapshots.json`, `server-id`, `root-credentials.json`) are kept;
objects live in memory. The same holds
when the default backing is an [s3 backing](backings.md) with a home: its
buckets, metadata, keys and restic repositories live in its S3 account, and
`storage.path` is optional and only holds the system files. Without it they
are ephemeral (the server logs a warning): IAM users, settings, placements
and live backing changes are lost on restart, and root credentials are
generated anew unless configured. An s3 backing without a home keeps its
records (its buckets and containers, the configuration of its native
buckets) in `.gos3/s3/<backing>/` and requires `storage.path`; back that
directory up with the rest. s3 backings need no local data directory:
each gets a temporary directory under the operating system's temporary
directory (`gos3-<backing>-*`, removed at shutdown) for upload spool files,
restic's cache and restores. Every backend takes an exclusive lock on `.gos3/lock` while it runs
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

Use **Storage → Configure backings** to add storage (a directory, memory or
a bucket of an S3 service) and choose the default for new buckets without
restarting. You can replace or remove additional backings once their
buckets and replicas have been moved or removed and background cleanup has
finished; the connection settings, credentials and default bucket kind of
an s3 backing can be changed at any time. Backing definitions persist in
`backings.json` in the system directory; the UI indicates when the server
has no persistent settings directory. The system backing named `default`
is fixed at startup.

A server can store buckets on several backings (see
[configuration.md](configuration.md#storage-backings)), typically one fs
directory per disk, or an account of a remote S3 service
([backings.md](backings.md)). Each fs backing directory has its own
`.gos3/` with its own `meta.db`, data files and lock, and each s3 backing
its own home, containers and native remote buckets; the system state (`iam.json`, `settings.json`,
`placements.json`, ...) stays in `storage.path`. Every bucket has a **placement**, managed in the web UI (bucket
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
- s3 backings: [native buckets](backings.md#bucket-kinds) can be neither
  replicated nor moved, and replicas and move targets on an s3 backing are
  always wrapped buckets, in the backing's default container or the
  container a move names (`container` in the move request, or the Container
  field of Move in the web UI). An s3 backing without any container can
  hold no replicas. A native bucket that is no longer wanted in gos3 can be
  released (`POST /buckets/{name}/release`): gos3 forgets it and its remote
  bucket stays as it is. To get a native bucket's data onto another
  backing, or to keep a second copy of it, use a
  [copy or mirror job](jobs.md#copy-jobs-replicas-and-moves).
- Changes waiting to be copied are kept in memory. After a restart every
  replica is compared with its primary in full, in the background (cost:
  reading the metadata of every object on both sides); active-active replicas
  serve reads and receive synchronous writes again when that finishes.
- Backing health: a backing is marked unhealthy when an operation on it fails
  with an I/O error, or when it fails the periodic check
  (`storage.replication.health_interval`): listing its buckets within 5 s,
  or for an s3 backing, its home serving (without a home, its service
  answering); it is healthy again after the next successful check. A
  failure of one bucket's remote location (a native bucket's remote
  bucket, a container) leaves the backing healthy and affects that bucket
  only. `GET /_gos3/admin/v1/backings` and the Storage page show the state
  and the last error.
- Removing a backing from the configuration: replicas on it are forgotten (a
  replica is promoted where it was the primary). gos3 refuses to start when a
  bucket's only copy was on it; configure the backing again, or remove the
  bucket from `placements.json` to forget it. A bucket found on a backing
  without an entry in `placements.json` is adopted where it is (that is how
  existing data directories keep working); a copy of a bucket that is placed
  elsewhere is ignored with a warning and never deleted automatically.
- At startup the server waits up to 15 s for its s3 backings to load. One
  whose service is unreachable does not stop the server: its buckets answer
  `503 ServiceUnavailable` and the backing is unhealthy until the service
  answers, then its buckets are served (and buckets found there without a
  placement are adopted). Until it has been listed, unknown bucket names
  answer `503` instead of `404` and new buckets cannot be created. An s3
  backing that holds no placed bucket can be removed live even while it is
  unreachable. See
  [backings.md](backings.md#behaviour-when-the-remote-is-unreachable).
- Backups: with several backings, back up every backing directory together
  with the default data directory (one snapshot per file system), and every
  s3 backing as described in [backings.md](backings.md#backups).

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

The copy includes the encryption keys (`.gos3/keys/`) and `backings.json`
with the credentials and encryption keys of s3 backings configured in the
web UI, so protect it like the keys themselves. It also includes the jobs
(`.gos3/jobs/`) and the snapshot scheduler's state
(`.gos3/restic-snapshots.json`): without them copy and mirror jobs are
gone (mirrors stop until they are created again) and the snapshot
scheduler forgets when each bucket was last snapshotted (every schedule
then snapshots at its next check, or its next time of day). The snapshot stage
(`.gos3/restic-stage/`) need not be backed up: a run without it rebuilds
it from the newest snapshot's manifest.

Restore by stopping gos3, replacing the directory contents (keep the owner and
mode 0700) and starting it again. The restored server keeps its server ID,
IAM users and settings. If you restore only an older `meta.db` over newer
data, objects written after that backup are not destroyed: they land in
`.gos3/orphans/` and can be recovered by hand until the retention (7 days)
expires. Never delete `meta.db` to get past a "locked by another process"
error: stop the other process instead.

Buckets on [s3 backings](backings.md) live in the backing's S3 account:
wrapped buckets entirely in their container (metadata, data, wrapped keys
and restic repositories), native buckets as ordinary objects of their
remote bucket, which any S3 tool can back up. The data directory holds
none of it, except the records of an s3 backing without a home
(`.gos3/s3/<backing>/`). Protect the remote buckets with the service's own
means (bucket versioning, replication), or copy a container's whole prefix
while gos3 is stopped; see [backings.md](backings.md#backups). **Keep the
encryption key** of each s3 backing (`encryption_key`,
`GOS3_S3_ENCRYPTION_KEY` or `GOS3_BACKING_<NAME>_ENCRYPTION_KEY`) in a safe
place of its own: without it the backing's encrypted objects and restic
archives are lost.

**2. Object-level copy (portable).** Mirror buckets with any S3 tool, for
example `rclone sync gos3:bucket backup:bucket` or
`mc mirror gos3/bucket backup/bucket`. This copies current objects (and, with
suitable flags, versions) to any S3 service or disk. It does not include IAM
users, policies, bucket configurations or object lock state, so also back up
`.gos3/iam.json` and `.gos3/settings.json` (both secret or private).
gos3 can do this itself: a [mirror job](jobs.md) keeps a bucket copied
into another bucket, on another disk, an S3 service or another gos3
server (through an s3 backing), with its versions, metadata, object lock
and bucket configuration.

**3. Restic snapshots.** [Whole-bucket snapshots](encryption.md#whole-bucket-snapshots)
back a bucket up on a schedule into a restic repository, with retention,
as files plus a manifest of keys and metadata, which `restic restore`
reads without gos3. The [per-version archive](encryption.md#archiving-every-write)
keeps every version written to a bucket. Both live on the bucket's own
backing unless a snapshot schedule names another one, so combine them
with an off-site copy of that backing.

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
  (every built-in backend supports this). A version is removed, or hidden
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

## Jobs and restic snapshots

Copy and mirror [jobs](jobs.md), and the snapshots, restores and prunes of
[whole-bucket restic snapshots](encryption.md#whole-bucket-snapshots), run
in the background, at most `jobs.workers` (default 2) at a time (see
[configuration.md](configuration.md#jobs-and-restic-snapshots)). They are
kept in `.gos3/jobs/jobs.json` and survive restarts: a job interrupted by
a shutdown or crash continues from its last checkpoint at the next start.
Finished jobs stay in the history for 30 days (at most 200).

Snapshot, restore and prune jobs also wait for one of the
`restic.snapshots.workers` (default 1) before they run restic, in the phase
`waiting`, and hold their job worker while they wait. With the defaults a
second snapshot job therefore occupies the other job worker until the
first one finishes, and copy jobs queue behind both; raise `jobs.workers`
when many buckets have snapshot schedules.

Monitoring:

- The web UI's **Jobs** page, or `GET /_gos3/admin/v1/jobs?state=failed`
  (and `state=waiting` with a `lastError` for jobs retrying a failure),
  needs `admin:ListJobs` (granted by the built-in `diagnostics` policy).
  A mirror's `lagSeconds` and `pending` show how far its target is behind.
- `GET /_gos3/admin/v1/buckets/{name}/restic/snapshots` reports each
  snapshot schedule's last attempt, last success, last error and next run.
- The log has a line per job event, with `job` and `kind`: `job submitted`
  and `job succeeded` at info level, `job run failed; will retry` and
  `job failed` at warn level with the error; the admin API also logs who
  created, paused, resumed, canceled, ran or deleted a job. A failing run
  is retried with a growing delay (at most every 5 minutes) until it
  succeeds or the job is paused or canceled, so alert on warnings that
  repeat for the same job. Problems that did not fail a job (a skipped
  policy, a failed retention or prune) are in its status's `warnings`.
- There are no Prometheus metrics for jobs yet.

### Restic snapshot stage

Each snapshot run builds its tree in the stage directory
(`restic.snapshots.stage_dir`, default `.gos3/restic-stage/` in the system
directory), one subdirectory per repository and bucket, kept between runs
(see [How a run works](encryption.md#how-a-run-works)). Without a system
directory each server process uses a private stage in the temporary
directory.

- **Plain text**: while a run copies objects into the stage, their content
  is there unencrypted until restic has stored it. Between runs, and after
  a failed run, the stage holds only placeholders; a bucket's stage is
  removed when its snapshot schedule is deleted. Keep the stage on a
  private local file system (mode 0700), like the data directory.
- **Space**: objects that did not change since the previous snapshot are
  sparse placeholders that take no space; new and changed objects take
  their full size until they are in a snapshot. A run needs about
  `restic.snapshots.spool_bytes` (8 GiB by default, at least 64 MiB) plus
  the largest object, however large the bucket; lower the setting on small
  disks (more partial snapshots per run), or raise it to make fewer.
- **Inodes**: every key of the bucket is a file in the stage, plus a
  directory per "folder". A bucket with ten million keys needs ten million
  inodes there; check `df -i` on file systems with a fixed inode count
  (ext4), or put the stage on one without (XFS, btrfs, ZFS).
- **File system**: it must keep sparse files and file times exactly (ext4,
  XFS, btrfs, ZFS and NTFS do; some network file systems do not). If
  verification keeps failing, set `restic.snapshots.incremental: false`,
  which writes every object at every run.
- The stage may be deleted while no snapshot runs; the next run rebuilds
  it from the newest snapshot's manifest. Restic's own cache and temporary
  files come on top.

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
beacon "bye"), the lifecycle worker, the background jobs (a running copy,
mirror pass or snapshot is interrupted and continues from its checkpoint
at the next start) and the storage backend. A second signal
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

Some object versions are also recorded in a newer record format than older
gos3 versions understand. Such a version starts, but reports their metadata
as corrupt (`unknown record format`), and listings, lifecycle passes and
usage counts that reach one of them fail for the whole bucket:

- Compressed versions use record format 4. Roll back to a gos3 version
  without compression support only after rewriting them uncompressed.
- Encrypted versions (SSE-S3 `AES256`, native or restic) use record format
  5. Roll back to a gos3 version without object encryption only after
  removing every encrypted version, current and noncurrent: disable the
  bucket's default encryption, copy each object onto itself with
  `x-amz-metadata-directive: REPLACE` (repeating its metadata) and without
  `x-amz-server-side-encryption`, then permanently delete the encrypted
  versions (Object Lock retention can prevent that until it expires).

Otherwise restore the backup. Runtime
settings in `settings.json` and IAM data in `iam.json` are kept across upgrades.

Copy jobs and restic snapshots add state that earlier versions ignore:

- A bucket that [archives every write](encryption.md#archiving-every-write)
  (restic scope `all`) archives only SSE writes after a rollback to a
  version without scopes; other writes are stored unencrypted again.
  Versions archived so far stay readable.
- `jobs/`, `restic-snapshots.json` and the buckets' `restic-snapshots`
  configuration documents are not read by earlier versions: jobs and
  snapshot schedules stop until the newer version runs again, which
  resumes them. Whole-bucket snapshots and aux repositories stay in their
  repositories.
- `jobs.json` records a format version; a gos3 version with jobs that does
  not know that format (one written by a later version) refuses to start
  rather than overwrite it.

Development builds before the s3 backend kind configured remote storage
differently: a `restic_s3` block on fs backings (restic repositories on an
S3 service), a `.gos3/restic-backing.json` file, and s3 backings with
`mode: blob` or `mode: gateway` and a local `path`. None of these were in a
release, and none are read any more: restic repositories of fs backings are
local, and an s3 backing is configured as described in
[backings.md](backings.md) (a `mode` or `path` on an s3 backing, or a
`restic_s3` key, is a configuration error). Data written by those builds to
a remote bucket is not picked up; copy such buckets through S3 into a
backing of the current version.

Later development builds configured an s3 backing with `bucket`, `prefix`
and `gateway`. These keys are still accepted (a deprecation warning is
logged at startup; `gos3 config print` shows `home_bucket`, `home_prefix`
and `default_kind`), and the backing's data is migrated in place when it
is first loaded: a plain backing's buckets become wrapped buckets in its
home, a gateway backing's buckets native buckets. Builds before the
migration cannot open a migrated backing, so back up its prefix before
upgrading if you may need to roll back. See
[backings.md](backings.md#migration-from-earlier-development-builds).
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
| The buckets of one backing answer `503 ServiceUnavailable` | Its S3 service is unreachable, or the backing is still loading; the Storage page shows the last error. See [backings.md](backings.md#behaviour-when-the-remote-is-unreachable). |
| Only one bucket of an s3 backing answers `503 ServiceUnavailable` | Its own remote location fails: a native bucket's remote bucket was deleted or is forbidden to the backing's credentials, or a wrapped bucket's container is still loading or cannot be used (see its state on the backing's page). See [backings.md](backings.md#behaviour-when-the-remote-is-unreachable). |
| CreateBucket of a native bucket: `BucketAlreadyExists` ("an administrator can adopt it") | The remote bucket to create exists in the backing's account. The S3 API never adopts remote buckets; an administrator can, on the backing's Remote buckets page. |
| s3 backing: `prefix in use by another server` | Another gos3 server runs on the same home or container. After a crash, the next start waits until the old lease is 30 s old. |
| s3 backing: `the S3 service does not support conditional writes` | The service ignores `If-None-Match` / `If-Match` on PUT; gos3 cannot use it for an s3 backing. |
| s3 backing: `encryption key does not match this backing` | The configured `encryption_key` differs from the one the backing's keys are wrapped with; configure the original key. |
| Creating a copy or mirror job is refused because "the source holds … -encrypted data", or the job fails with "the copy would store encrypted data unencrypted" | The source holds AES256 or restic versions and the target cannot store them encrypted (a native bucket, a memory backing, an s3 backing without an encryption key). Choose another target, `encryption: "target"`, or allow plaintext copies (`allowPlaintext`). See [jobs.md](jobs.md#what-is-copied-with-each-version). |
| Creating a job is refused because "the source bucket … has object lock and the target has none" | Create the target with object lock, or copy without retention (`dropRetention`). |
| A job is refused with `409` "the target … is not empty" | The target range holds versions, delete markers or multipart uploads; choose an empty target, or `onExisting: "merge"` (or `"adopt"` for a mirror). |
| A job reports conflicts | Target versions under retention or legal hold that the job would delete or replace, or keys too long for the target prefix; the job leaves them alone. `conflictSamples` names some. |
| A whole-bucket snapshot run fails verification | restic read placeholder files in the stage (a stage file system that does not keep sparse files or file times exactly). Move `restic.snapshots.stage_dir`, or set `restic.snapshots.incremental: false`. See [above](#restic-snapshot-stage). |
| Writes to a restic bucket wait or fail during a prune | Prune holds the repository's exclusive lock; schedule it for a quiet time or turn it off for that bucket ([encryption.md](encryption.md#prune)). |
| Peers show `unverified` with a TLS error | The peer uses a certificate that this server does not trust. |
| Configuration changes have no effect | `settings.json` overrides them for runtime keys; see [configuration.md](configuration.md#runtime-settings-settingsjson). |
