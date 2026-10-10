# Operations

Running gos3 day to day: health checks, metrics, logs, backups, the lifecycle
worker and upgrades.

## Health and readiness

| Endpoint | Meaning | Response |
|---|---|---|
| `GET /_gos3/health` | Liveness: the process serves HTTP. | `200 {"status":"ok"}` |
| `GET /_gos3/ready` | Readiness: storage answers (bucket listing within 5 s), and the server is not shutting down. | `200 {"status":"ok"}` or `503 {"status":"unavailable","error":"…"}` |

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
| `gos3_requests_total` | counter | `op`, `code_class` (`1xx`…`5xx`, `other`) | Requests by operation and status class. |
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

Example alerts: `rate(gos3_requests_total{code_class="5xx"}[5m]) > 0`, and
`histogram_quantile(0.99, sum by (le, op) (rate(gos3_request_duration_seconds_bucket[5m])))`
for latency.

The admin API's `GET /_gos3/admin/v1/stats` (and the UI's server overview) add
storage totals (objects, bytes, versions, delete markers, pending uploads;
cached for 30 s), Go runtime numbers and disk usage of the data directory
(fs backend on Linux and macOS).

## Logs

gos3 logs to stderr with Go's `log/slog`, as `text` (default) or `json`
(`logging.format`). The level and format can be changed at runtime.

- Startup prints a banner with the endpoints, UI and admin URLs, the region,
  the storage backend, and where the root credentials came from. A generated
  secret is shown only when it was just generated. With JSON logs the banner is
  one structured line.
- **Access log**: one line per request (S3 and admin) with `request_id`, `op`,
  `method`, `path`, `status`, `bytes_in`, `bytes_out`, `duration_ms`,
  `remote`, `bucket`, `key`, `auth` (signing method), `user` and `access_key`,
  and `error` (the S3 error code).
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
    ├── data/<bucket-id>/<xx>/…  object data files, immutable (fs backend)
    ├── uploads/<upload-id>/…    multipart parts in progress (fs backend)
    ├── tmp/                     files being written (cleaned at startup)
    ├── iam.json                 IAM users, keys, policies        (secret)
    ├── root-credentials.json    generated root keys, if any      (secret)
    ├── settings.json            runtime settings, after the first change
    └── server-id                this server's discovery ID
```

With the memory backend and a data path, only the system files (`iam.json`,
`settings.json`, `server-id`, `root-credentials.json`) are kept; objects live in
memory. `meta.db` is locked while gos3 runs, so a second process cannot open
the same directory. At startup the fs backend removes leftover temporary files
and data files that no metadata references (left behind by a crash).

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
`fsync_data: true`), and orphaned files are cleaned up at the next start. Do
not copy a live directory with `cp` or `rsync` without a snapshot: `meta.db`
can end up inconsistent with the data files.

Restore by stopping gos3, replacing the directory contents (keep the owner and
mode 0700) and starting it again. The restored server keeps its server ID,
IAM users and settings.

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

Expiration times follow AWS: an object becomes eligible at the next 00:00 UTC
after `LastModified + Days`. A pass starts at startup and then repeats
`lifecycle.interval` (default 1 h) after the previous pass ends. Turn the
worker off or change the interval at runtime in Settings
(`lifecycle.enabled`, `lifecycle.intervalSeconds`); the change applies
immediately. Each action and a summary per pass are logged.

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
3. Replace the binary or image, and restart.

The fs metadata database records a format version. A gos3 version that does
not understand it refuses to start rather than risk the data, so rolling back
after an upgrade that changed the format means restoring the backup. Runtime
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
| Startup: `metadata database … is locked by another process` | Another gos3 uses the data directory. |
| Startup: `settings.json: …` | The runtime settings file is corrupt or invalid; fix or remove it. |
| Peers show `unverified` with a TLS error | The peer uses a certificate that this server does not trust. |
| Configuration changes have no effect | `settings.json` overrides them for runtime keys; see [configuration.md](configuration.md#runtime-settings-settingsjson). |
