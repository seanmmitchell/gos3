# Configuration

gos3 is configured from four layers. Each layer overrides the one before it:

1. built-in defaults
2. a JSON or YAML file (`--config FILE`, or `GOS3_CONFIG=FILE`)
3. `GOS3_*` environment variables
4. command-line flags of `gos3 serve`

There is also a fifth, **runtime** layer. The settings marked *runtime* below can
be changed while the server runs, through the web UI (**Settings**) or the admin
API (`PUT /_gos3/admin/v1/config`, see [admin-api.md](admin-api.md)). gos3
writes them to `<data>/.gos3/settings.json`, and from then on that file wins
over the configuration file, the environment and the flags for those keys (see
[Runtime settings](#runtime-settings-settingsjson)).

This fifth layer applies only when `admin.enabled` is true. S3-only servers
(`admin.enabled: false` or `--no-admin`) use the four static layers and
neither read nor overwrite an existing `settings.json`. IAM and object state
continue to load normally.

Useful commands:

```sh
gos3 config init -o gos3.yaml   # commented sample file; its values are the defaults
gos3 config init -o gos3.json   # complete JSON configuration with default values
gos3 config print --json        # effective JSON, secrets redacted
gos3 config print [serve flags] # effective configuration (secrets redacted); exit 1 if invalid
gos3 config env                 # table of every GOS3_* variable
gos3 serve -h                   # flags
```

`gos3 config print` takes the configuration flags of `gos3 serve`, so you can check what
a command line would produce. When management is enabled and `<data>/.gos3/settings.json` exists, it
lays that file over the result (defaults < file < env < flags <
settings.json), as the server does, and lists the configured values it
overrides. Unknown YAML keys are an error, reported with
their line number (for example `line 3: unknown key "adress" in section "server"`).
Validation errors name the YAML key, for example
`cors.allowed_origins[1]: must not contain a path...`.

## Value syntax

JSON uses exactly the same snake_case keys and nested objects as the reference
below, including `storage.backings` and `storage.replication`. Use native
booleans, integer byte sizes, arrays, and string durations such as `"30s"`.
A `.json` file must contain one JSON object; comments, trailing commas, nulls,
duplicate keys, unknown fields and incorrect types are rejected. Omitted
fields retain defaults. Static configuration JSON differs from the runtime
admin API's camelCase fields and nanosecond durations.

`gos3 config init --json` emits JSON to stdout; `-o file.json` also selects
JSON automatically. `config print --json` prints a plain JSON object with the
same effective settings as YAML output and exits unsuccessfully if invalid.

## Server modes

| Mode | Configuration | CLI |
|---|---|---|
| Embedded UI (default) | `admin.enabled: true`, `ui.enabled: true` | `gos3 serve` |
| Separate UI / management API | `admin.enabled: true`, `ui.enabled: false` | `gos3 serve --no-ui` |
| S3 only | `admin.enabled: false` (also disables UI) | `gos3 serve --no-admin` |

Disabled UI and management endpoints return 404 and are not advertised.
Mode switches require a restart and cannot be changed through the admin API.
Health/readiness, optional metrics and the identity endpoint remain available.
Discovery and all of its switches default to off in every mode.

Examples: [S3 only](../deploy/config/s3-only.json),
[separate UI](../deploy/config/managed.json), and
[embedded UI](../deploy/config/embedded.json). Run
`gos3 serve --config deploy/config/s3-only.json`; supply root keys through
the environment or let gos3 generate them.

`make build-headless` or `go build -tags noui ./cmd/gos3` excludes all UI
assets and requires no Node/pnpm. These binaries default to UI disabled and
management enabled; add `--no-admin` for S3 only. Explicitly enabling UI in a
headless binary fails with an actionable error. For containers use
`docker build --target headless -t gos3:headless .`.

### MinIO startup compatibility

`gos3 server /data --address :9000 --no-admin` accepts the MinIO-style
`server` command, one local data directory, and flags before or after it.
Do not combine the positional directory with `--data`.

- `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` alias the root credential variables.
- `MINIO_ADDRESS`, `MINIO_SITE_REGION`, and `MINIO_BROWSER` alias
  `GOS3_ADDRESS`, `GOS3_REGION`, and `GOS3_UI_ENABLED`.
- Native `GOS3_*` variables win over their aliases, even when explicitly empty.
  Aliases override the file; flags override both environment forms.
- `--json` selects JSON logs; `--quiet` suppresses the startup banner;
  `--certs-dir DIR` uses `DIR/public.crt` and `DIR/private.key`.
- Anonymous GET/HEAD `/minio/health/live` and `/minio/health/ready` are
  probe aliases. Signed, presigned and virtual-host requests still route to
  S3 objects, including a bucket named `minio`. The two anonymous path-style
  URLs are reserved for probes.

The UI shares the S3 listener at `/_gos3/ui/`; `--console-address` reports
an error explaining how to use that URL or disable UI. Distributed MinIO
volume syntax and multiple positional directories are rejected. Use gos3's
storage configuration for additional backings. MinIO admin APIs and on-disk
data layouts are not compatible; migrate objects through S3 into a new gos3
data directory. JSON configures server settings; provision buckets and IAM
resources through their APIs.

### Environment and flag values

These rules apply to environment variables and flags. YAML uses its native
types for the same keys.

| Type | Syntax |
|---|---|
| bool | `true/false`, `1/0`, `t/f`, `yes/no`, `y/n`, `on/off` (any case) |
| list | comma-separated: `GOS3_DOMAINS=s3.example.com,s3.local`. List flags can be repeated and also accept comma lists. Spaces are trimmed and empty entries dropped. |
| duration | Go syntax: `500ms`, `30s`, `10m`, `1h` |
| size | plain bytes, or a number with a `K`, `M`, `G` or `T` suffix in powers of 1024 (`KB`/`KiB` forms are also accepted): `5368709120`, `5GiB`, `1.5G` |

## Reference

**Flag** lists the `gos3 serve` flag for the key, where one exists. **Runtime**
shows the field name in the admin API `Settings` document for keys that can be
changed while running.

### `server`

| YAML key | Env | Flag | Default | Runtime | Description |
|---|---|---|---|---|---|
| `server.address` | `GOS3_ADDRESS` | `--address` | `:9000` | | Listen address, `[host]:port`. Port `0` picks a free port; the banner shows the real URL. |
| `server.name` | `GOS3_NAME` | `--name` | host name | `name` | Server name shown in the UI and announced by discovery (at most 128 printable characters). |
| `server.region` | `GOS3_REGION` | `--region` | `us-east-1` | `region` | Region reported to clients (lowercase letters, digits and `-`). |
| `server.public_url` | `GOS3_PUBLIC_URL` | `--public-url` | derived | `publicUrl` | Externally reachable base URL, advertised first in `/_gos3/info` and discovery. No path (a trailing `/` is allowed), credentials, query or fragment: gos3 must be at the root of the URL. When empty, gos3 advertises the listener port on every non-loopback address of the host. |
| `server.domains` | `GOS3_DOMAINS` | `--domain` | `[]` | `domains` | Domains for virtual-hosted-style requests: Host `<bucket>.<domain>` addresses `<bucket>`. DNS names only, no scheme or port. Path-style requests always work. |
| `server.tls_cert` | `GOS3_TLS_CERT` | `--tls-cert` | | | PEM certificate (chain) file. Set it together with `tls_key` to serve HTTPS (HTTP/2 enabled). |
| `server.tls_key` | `GOS3_TLS_KEY` | `--tls-key` | | | PEM private key file. |
| `server.max_object_size` | `GOS3_MAX_OBJECT_SIZE` | | `5368709120` (5 GiB) | | Largest single PUT / POST / copy, in bytes, at most 5 TiB. `0` means the default. Multipart uploads are limited by part count and size instead. |
| `server.public_info` | `GOS3_PUBLIC_INFO` | | `true` | `publicInfo` | Serve `GET /_gos3/info` without authentication. When false, any valid signature is required. |
| `server.shutdown_timeout` | `GOS3_SHUTDOWN_TIMEOUT` | | `30s` | | How long in-flight requests may run after SIGINT/SIGTERM before connections are closed. |
| `server.trust_proxy_headers` | `GOS3_TRUST_PROXY_HEADERS` | | `false` | | Use `X-Forwarded-For` for the client address (`aws:SourceIp`, access logs) and `X-Forwarded-Proto` for `aws:SecureTransport`, on the S3 API, the admin API and the `/_gos3/*` system endpoints. The client is the rightmost `X-Forwarded-For` entry (all header lines) that is not in `server.trusted_proxies`; the last `X-Forwarded-Proto` value counts. Enable it only behind a reverse proxy; see [deployment.md](deployment.md#reverse-proxy). |
| `server.trusted_proxies` | `GOS3_TRUSTED_PROXIES` | | `[]` | | Reverse proxies (single addresses such as `192.0.2.10`, or CIDR prefixes such as `10.0.5.0/28`, IPv4 or IPv6) whose forwarding headers are honoured when `trust_proxy_headers` is on. List only proxies, never a subnet that also holds clients: a client whose address is listed is skipped like a proxy hop and can choose its `aws:SourceIp`. Requests whose peer address is not in the list are taken at face value. `X-Forwarded-For` is walked from the right past these addresses: the first other entry is the client; an invalid entry stops the walk at the last trusted hop (or the peer address). **Required** when `trust_proxy_headers` is on: an empty list is a configuration error (before v0.2 it trusted every peer, which let any client that reaches gos3 directly choose its `aws:SourceIp` and `aws:SecureTransport`). An invalid entry is a configuration error. The variable takes a comma-separated list. Your proxy must *set* (not append to) `X-Forwarded-Proto`. |
| `server.allowed_hosts` | `GOS3_ALLOWED_HOSTS` | `--allowed-host` (repeatable) | `[]` | | Extra host names (the `Host` header, without port) that clients use to reach the server, besides the ones it knows itself: IP addresses, `localhost` and `*.localhost`, the OS host name, its first label and `<label>.local`, the host of `server.address`, `server.public_url` and `server.domains` (with their bucket subdomains). `*.example.com` allows every subdomain; `["*"]` turns the check off. **Empty** (default): requests for any other host name are served only when they are signed; anonymous S3 requests, the web UI and public metrics answer `403` / `421 Misdirected Request`, and the first request for each such name is logged. This stops DNS-rebinding websites from using a visitor's browser against LAN-only data. **Non-empty**: every request for another host name is refused with `421`, signed ones included, so a signature captured by a foreign endpoint cannot be relayed to this server. `/_gos3/info`, `/_gos3/health` and `/_gos3/ready` and the admin API are served for any host only while the list is empty. See [security.md](security.md#dns-rebinding-and-the-host-header). |
| `server.max_connections` | `GOS3_MAX_CONNECTIONS` | | `0` (no limit) | | Opt-in cap on simultaneously open client connections. At the cap a new connection makes room by closing the longest-idle keep-alive connection or, when none is idle, a connection that has been open for at least 1 s without starting its first request (it sent nothing, only part of the request headers, or has not finished its TLS handshake), the oldest of the client address (for IPv6, its /64) with the most pending connections; it is closed itself only when every other connection has a request in flight or is younger than 1 s. Accepting never blocks, so idle, silent or half-open connections cannot lock out health probes. There is no per-address limit (reverse proxies funnel many clients through one address). |

### `storage`

| YAML key | Env | Flag | Default | Description |
|---|---|---|---|---|
| `storage.backend` | `GOS3_STORAGE_BACKEND` | `--backend` | `fs` | `fs` (files under `path`), `memory` (everything is lost on exit) or `s3` (an account of a remote S3 service, configured by `storage.s3`; see [backings.md](backings.md)). |
| `storage.path` | `GOS3_DATA` | `--data` | `./data` for `fs`, empty for `memory` and `s3` | Data directory. With `fs` it holds the objects and the metadata database. With every backend it holds the system state in `<path>/.gos3/` (see [operations.md](operations.md#the-data-directory)), including the records of s3 backings without a home, which therefore require it. With `memory` or `s3` and no path the system state is ephemeral, including generated root credentials and IAM users (a warning is logged); with `memory` the whole server is. |
| `storage.s3.endpoint` | `GOS3_S3_ENDPOINT` | | | `s3` backend: service URL, `http(s)://host[:port]` without a path. Required. |
| `storage.s3.region` | `GOS3_S3_REGION` | | `us-east-1` | `s3` backend: signing region. |
| `storage.s3.home_bucket` | `GOS3_S3_HOME_BUCKET` | | | `s3` backend: the remote bucket of the backing's [home](backings.md#home-and-records), which holds gos3's records for the backing and its shared container of wrapped buckets. Optional; must exist when set. Empty: the records are kept in the system directory (requires `storage.path`). |
| `storage.s3.home_prefix` | `GOS3_S3_HOME_PREFIX` | | | `s3` backend: key prefix of the home inside `home_bucket`; requires `home_bucket`. |
| `storage.s3.default_kind` | `GOS3_S3_DEFAULT_KIND` | | `wrapped` with a home, else `native` | `s3` backend: [kind](backings.md#bucket-kinds) of buckets created without one, `wrapped` (requires a home) or `native`. |
| `storage.s3.addressing` | `GOS3_S3_ADDRESSING` | | `auto` | `s3` backend: `auto`, `path` or `virtual`. |
| `storage.s3.part_size` | `GOS3_S3_PART_SIZE` | | `16MiB` | `s3` backend: upload part size and largest single PUT to the service, 5 MiB-5 GiB. |
| `storage.s3.access_key`, `storage.s3.secret_key` | `GOS3_S3_ACCESS_KEY`, `GOS3_S3_SECRET_KEY` | | | `s3` backend: credentials. Required. The secret is redacted by `gos3 config print`. |
| `storage.s3.encryption_key` | `GOS3_S3_ENCRYPTION_KEY` | | | `s3` backend: key-encryption key, base64 of exactly 32 bytes (`openssl rand -base64 32`). Enables native AES256 and restic on wrapped buckets, and restic repositories for [whole-bucket snapshots](encryption.md#whole-bucket-snapshots) in the home; losing it loses the encrypted objects and those snapshots. Redacted by `gos3 config print`. |
| `storage.s3.bucket`, `storage.s3.prefix`, `storage.s3.gateway` | `GOS3_S3_BUCKET`, `GOS3_S3_PREFIX`, `GOS3_S3_GATEWAY` | | | **Deprecated** aliases: `bucket` and `prefix` mean `home_bucket` and `home_prefix`, `gateway: true` means `default_kind: native`. Accepted with a warning at startup; an alias and its replacement with different values are an error. `gos3 config print` shows the new keys. |
| `storage.fsync_data` | `GOS3_FSYNC` | `--fsync` | `true` | fsync object data before acknowledging a write (every fs backing). `--fsync=false` is faster but can lose recent writes on power loss. |
| `storage.backings` | `GOS3_STORAGE_BACKINGS` | | none | Additional storage backings, see [below](#storage-backings). Env syntax: comma-separated `name=/path` (fs), `name=fs:/path`, `name=memory` or `name=s3` (configured by `GOS3_BACKING_<NAME>_*`). |
| `storage.backings[].s3.*` | `GOS3_BACKING_<NAME>_<FIELD>` | | | Settings of an s3 backing, the same keys as `storage.s3`. The variables (`<NAME>` upper-cased with `-` as `_`; `<FIELD>` `ENDPOINT`, `REGION`, `HOME_BUCKET`, `HOME_PREFIX`, `DEFAULT_KIND`, `ADDRESSING`, `PART_SIZE`, `ACCESS_KEY`, `SECRET_KEY` or `ENCRYPTION_KEY`; `BUCKET`, `PREFIX` and `GATEWAY=true` are deprecated aliases of `HOME_BUCKET`, `HOME_PREFIX` and `DEFAULT_KIND=native`) only fill fields left empty by the file or `backings.json`. |
| `storage.default_backing` | `GOS3_DEFAULT_BACKING` | | `default` | Backing new buckets are created on. |
| `storage.replication.default_mode` | `GOS3_REPLICATION_MODE` | | `single` | Replication of new buckets: `single`, `active-passive` or `active-active` (`active-passive` when only `default_replicas` is set). |
| `storage.replication.default_replicas` | `GOS3_REPLICATION_REPLICAS` | | none | Backings new buckets are replicated to (comma-separated in env). Must not include the default backing. |
| `storage.replication.auto_failover` | `GOS3_AUTO_FAILOVER` | | `false` | Promote a synced replica of an **active-passive** bucket when its primary backing fails. Changes not yet copied to the replica are lost. Active-active buckets always fail over. |
| `storage.replication.workers` | `GOS3_REPLICATION_WORKERS` | | `2` | Bucket copies (to replicas and move targets) running concurrently, 1-64. |
| `storage.replication.health_interval` | `GOS3_HEALTH_INTERVAL` | | `10s` | How often every backing is checked (1s-1h). |
| `storage.replication.cutover_timeout` | `GOS3_CUTOVER_TIMEOUT` | | `60s` | How long the cutover of a move or promotion waits for writes in flight on the bucket (1s-1h); new writes to the bucket wait meanwhile. |

#### Storage backings

A server can spread its buckets over several **backings**, for example one
fs directory per disk. The backing configured by `storage.backend`,
`storage.path` and `storage.s3` is named `default`; `storage.path` also
holds the system state. Each entry of `storage.backings` adds one more:

```yaml
storage:
  path: /var/lib/gos3              # the "default" backing + system state
  backings:
    - name: nvme                   # 1-32 of [a-z0-9_-]
      path: /mnt/nvme/gos3         # backend: fs (default) needs its own directory
    - name: archive
      path: /mnt/hdd/gos3
    - name: scratch
      backend: memory              # lost on restart
    - name: cold
      backend: s3                  # an account of a remote S3 service; no path
      s3:
        endpoint: https://s3.eu-central-1.amazonaws.com
        region: eu-central-1
        home_bucket: acme-gos3-cold  # records + shared container: wrapped buckets by default
        home_prefix: gos3/
        # access_key, secret_key, encryption_key: or GOS3_BACKING_COLD_*
    - name: shared
      backend: s3                  # no home: records in /var/lib/gos3/.gos3/s3/shared/
      s3:
        endpoint: http://minio.lan:9000
        # default_kind: native     # the default without a home
  default_backing: nvme            # new buckets go here
  replication:
    default_mode: active-passive   # new buckets get a replica...
    default_replicas: [archive]    # ...on archive
```

An s3 backing is a connection to an account of an S3 service and needs no
local data directory; `path` is not allowed on it. Its buckets are
**wrapped** (gos3's format inside a container, every feature) or **native**
(objects as they are in a remote bucket of their own, which other tools
can use); see [backings.md](backings.md#bucket-kinds). Its `s3` block takes
the keys of `storage.s3` above, and every field can come from
`GOS3_BACKING_<NAME>_<FIELD>` instead (filling only fields left empty).
With the environment alone:

```sh
GOS3_STORAGE_BACKINGS=nvme=/mnt/nvme/gos3,cold=s3
GOS3_BACKING_COLD_ENDPOINT=https://s3.eu-central-1.amazonaws.com
GOS3_BACKING_COLD_HOME_BUCKET=acme-gos3-cold
GOS3_BACKING_COLD_ACCESS_KEY=...
GOS3_BACKING_COLD_SECRET_KEY=...
```

Validation of s3 backings:

- `home_prefix` requires `home_bucket`; a backing without `home_bucket`
  requires `storage.path` (its records live in the system directory).
- `default_kind` is `wrapped` or `native`; empty means `wrapped` with a
  home and `native` without. `wrapped` requires a home.
- Two s3 backings of one server cannot have their homes on the same
  service and bucket with equal or nested prefixes. Backings without a home
  are not compared; overlapping containers and native buckets are refused
  when they are added (see [backings.md](backings.md#containers)).
- `storage.replication.default_replicas` cannot name an s3 backing without
  a home (replicas are wrapped buckets, which need a container), and cannot
  be set while new buckets on `storage.default_backing` are native (its
  `default_kind`), because native buckets cannot be replicated.
- A deprecated key (`bucket`, `prefix`, `gateway`) and its replacement with
  different values are an error.

See [backings.md](backings.md) for the s3 backend, its requirements (one
server per home and per container, conditional writes) and the migration of
earlier development builds. New buckets can be put on any backing with
CreateBucket's `x-gos3-backing` header (and, on s3 backings,
`x-gos3-bucket-kind`) or the web UI.

Backings can also be configured live in **Storage → Configure backings** in
the web UI. Administrators can add filesystem, memory or S3 backings, edit or
remove empty backings, and select the default for new buckets. Changes apply
immediately and are saved in `<storage.path>/.gos3/backings.json` (mode
0600: it includes the s3 credentials and encryption keys entered there,
except values taken from the environment), which overrides
`storage.backings` and `storage.default_backing` on subsequent starts while
management is enabled. Without a storage path, changes last only until
restart. The system backing named `default` and the replication defaults
remain startup configuration.

New backings added live must contain no existing buckets. Backings holding
buckets, replicas, move targets, or pending cleanup cannot be replaced or
removed; move their buckets and remove replicas first. The connection
settings of an s3 backing (endpoint, region, credentials, encryption key,
addressing, part size) and its `default_kind` can be changed while it is in
use; its home (`home_bucket`, `home_prefix`, including adding or removing
one) cannot. Changing the default does not move
existing buckets. The selected default must not be one of the configured
default replicas. See [backings.md](backings.md#configuring-backings-live).

Every bucket has a primary backing and optionally replicas; buckets can be
moved between backings online. Placements are changed with the admin API or
the web UI and saved in `<data>/.gos3/placements.json`. Existing buckets stay
where they are when backings are added. See
[operations.md](operations.md#multiple-backings-and-replication).

### `auth`

| YAML key | Env | Default | Description |
|---|---|---|---|
| `auth.root_access_key` | `GOS3_ROOT_ACCESS_KEY` | | Root access key: 3–128 characters of `[A-Za-z0-9._-]`. |
| `auth.root_secret_key` | `GOS3_ROOT_SECRET_KEY` | | Root secret key: 8–128 printable ASCII characters without spaces. |
| `auth.strict_region` | `GOS3_STRICT_REGION` | `false` | Reject signatures whose credential scope names a region other than `server.region`. By default any region is accepted. |
| `auth.insecure_demo_credentials` | `GOS3_INSECURE_DEMO_CREDENTIALS` | `false` | Allow a root secret key that is published as an example: `change-me-now`, `gos3admin-secret`, `minioadmin`, the AWS documentation example key and a few generic ones (`changeme`, `password`, ...). Without it gos3 refuses to start with such a key, because anyone can sign requests with it, including any website opened in a browser that can reach the server. For throwaway local demos only. |

The root keys have no flags, so they never show up in process listings. Set
both or neither. A configured secret shorter than 16 characters is accepted
with a warning: one captured signed request is enough to guess a short
secret offline. When neither key is set, gos3 generates a pair on first
start and stores it in `<data>/.gos3/root-credentials.json` (mode 0600; an
owner-only ACL on Windows) for later starts. The secret key is never
written to the log: the startup banner shows it only when standard error is
a terminal, and otherwise names the file to read it from. Without a data
directory (memory or s3 backend, no path) new keys are generated on every start;
without a terminal they are written to a private temporary file (named in
the banner and the log) that is deleted on exit. `gos3 keygen` prints a
random pair you can use instead. See
[security.md](security.md#root-credentials).

### `admin`

| Key | Env | Flag | Default | Description |
|---|---|---|---|---|
| `admin.enabled` | `GOS3_ADMIN_ENABLED` | `--no-admin` (sets false) | `true` | Enable the management API and persisted runtime settings. False also disables embedded UI. Restart required. |

### `ui`

| YAML key | Env | Flag | Default | Runtime | Description |
|---|---|---|---|---|---|
| `ui.enabled` | `GOS3_UI_ENABLED` | `--no-ui` (sets false) | `true` | | Serve the embedded web UI at `/_gos3/ui/`. |
| `ui.redirect_root` | `GOS3_UI_REDIRECT` | | `true` | `uiRedirect` | Redirect an anonymous browser `GET /` (`Accept: text/html`) to `/_gos3/ui/`. Signed requests and virtual-host requests are never redirected. |

### `cors`

The global CORS policy. It applies to the admin API and to S3 requests on
buckets that have no CORS configuration of their own (and to `/` and missing
buckets). A bucket's own CORS configuration (`PutBucketCors`) takes precedence.

| YAML key | Env | Flag | Default | Runtime | Description |
|---|---|---|---|---|---|
| `cors.allowed_origins` | `GOS3_CORS_ORIGINS` | `--cors-origin` | `["*"]` | `cors.allowedOrigins` | `*`, `scheme://host[:port]`, `scheme://[ipv6][:port]` or `scheme://*.domain[:port]`; no path or trailing slash. |
| `cors.allowed_headers` | `GOS3_CORS_ALLOWED_HEADERS` | | `["*"]` | `cors.allowedHeaders` | Request headers allowed in preflights (header names or `*` patterns). |
| `cors.expose_headers` | `GOS3_CORS_EXPOSE_HEADERS` | | `*` plus an explicit list (below) | `cors.exposeHeaders` | Response headers readable by browser scripts. A lone `*` is allowed; partial patterns like `x-amz-*` are not. |
| `cors.max_age_seconds` | `GOS3_CORS_MAX_AGE` | | `3600` | `cors.maxAgeSeconds` | Preflight cache time, 0–604800. |

The default `expose_headers` is `*`, `ETag`, `Content-Length`, `Content-Type`,
`Content-Range`, `Content-Disposition`, `Content-Encoding`, `Content-Language`,
`Cache-Control`, `Expires`, `Last-Modified`, `Accept-Ranges`,
`x-amz-request-id`, `x-amz-id-2`, `x-amz-version-id`, `x-amz-delete-marker`,
`x-amz-checksum-crc32`, `x-amz-checksum-crc32c`, `x-amz-checksum-crc64nvme`,
`x-amz-checksum-sha1`, `x-amz-checksum-sha256`, `x-amz-checksum-type`,
`x-amz-mp-parts-count`, `x-amz-tagging-count`, `x-amz-storage-class`,
`x-amz-server-side-encryption`, `x-amz-object-lock-mode`,
`x-amz-object-lock-retain-until-date`, `x-amz-object-lock-legal-hold`,
`x-amz-expiration`, `x-amz-restore`, `x-amz-website-redirect-location`,
`x-amz-abort-date`, `x-amz-abort-rule-id`, `x-amz-bucket-region` and the
gos3 extension headers `x-gos3-backing` and `x-gos3-bucket-kind`. The `*` makes `x-amz-meta-*`
readable, which the web UI's metadata editor needs. The explicit names cover
browsers that do not honour `*`. A standalone web UI only needs its origin in
`allowed_origins`; see [web-ui.md](web-ui.md#cors).

For S3 requests, the `*` entry admits only signed requests (an
`Authorization` header or a presigned URL); explicitly listed origins also
apply to anonymous requests. Anonymous requests that change data and come
from another origin are refused unless a bucket CORS rule (or an explicitly
listed global origin) allows the origin and method. The `*` default stays
because the standalone web UI and other browser clients connect
cross-origin with signed requests. See [security.md](security.md#cors).

### `discovery`

All discovery keys are runtime settings (`discovery.<camelCase>` in the admin
API). [discovery.md](discovery.md) describes the protocol.

| YAML key | Env | Flag | Default | Description |
|---|---|---|---|---|
| `discovery.enabled` | `GOS3_DISCOVERY_ENABLED` | `--no-discovery` (sets false) | `false` | Run discovery (mDNS, beacon, seeds, DNS SRV). |
| `discovery.advertise` | `GOS3_DISCOVERY_ADVERTISE` | | `false` | Announce this server via mDNS and the beacon. |
| `discovery.mdns` | `GOS3_DISCOVERY_MDNS` | | `false` | Use mDNS / DNS-SD (`_gos3._tcp.local.`). |
| `discovery.beacon` | `GOS3_DISCOVERY_BEACON` | | `false` | Use the UDP multicast beacon. |
| `discovery.beacon_group` | `GOS3_DISCOVERY_BEACON_GROUP` | | `239.255.73.83:7373` | IPv4 multicast group and port. |
| `discovery.beacon_interval` | `GOS3_DISCOVERY_BEACON_INTERVAL` | | `10s` | Announce interval (±10 % jitter), 1s–1h. |
| `discovery.seeds` | `GOS3_DISCOVERY_SEEDS` | `--seed` | `[]` | Base URLs (or bare `host[:port]`, meaning `http://`) of servers to probe on start and every 30 s. At most 256. |
| `discovery.dns_srv` | `GOS3_DISCOVERY_DNS_SRV` | | `[]` | Domains whose `_gos3._tcp.<domain>` SRV records list servers (queried every 60 s). At most 64. |
| `discovery.peer_ttl` | `GOS3_DISCOVERY_PEER_TTL` | | `60s` | Peers not seen for this long are dropped, 30s–7d. |
| `discovery.allow_scan` | `GOS3_DISCOVERY_ALLOW_SCAN` | | `false` | Allow network scans through the admin API (users also need `admin:ScanNetwork`). |
| `discovery.max_scan_hosts` | `GOS3_DISCOVERY_MAX_SCAN_HOSTS` | | `1024` | Host limit per scan, 1–65536. |

### `logging`

| YAML key | Env | Flag | Default | Runtime | Description |
|---|---|---|---|---|---|
| `logging.level` | `GOS3_LOG_LEVEL` | `--log-level` | `info` | `logging.level` | `debug`, `info`, `warn` or `error`. |
| `logging.format` | `GOS3_LOG_FORMAT` | `--log-format` | `text` | `logging.format` | `text` or `json` (slog). With `json`, the startup banner becomes one structured log line. |
| `logging.access_log` | `GOS3_ACCESS_LOG` | | `true` | `logging.accessLog` | Log every request at info level. When false, requests are logged at debug level. The log contains the path only, never the query string, so presigned signatures are not logged. |

### `lifecycle`

| YAML key | Env | Default | Runtime | Description |
|---|---|---|---|---|
| `lifecycle.enabled` | `GOS3_LIFECYCLE_ENABLED` | `true` | `lifecycle.enabled` | Run the lifecycle worker that applies bucket lifecycle rules. |
| `lifecycle.interval` | `GOS3_LIFECYCLE_INTERVAL` | `1h` | `lifecycle.intervalSeconds` | Pause between passes: a whole number of seconds between 1 minute and 365 days. The first pass runs at startup. |

### `metrics`

| YAML key | Env | Default | Runtime | Description |
|---|---|---|---|---|
| `metrics.enabled` | `GOS3_METRICS_ENABLED` | `true` | | Serve Prometheus metrics at `/_gos3/metrics` (404 when disabled). |
| `metrics.public` | `GOS3_METRICS_PUBLIC` | `false` | `metricsPublic` | Serve metrics without authentication. Otherwise a signed request with `admin:GetStats` is required. |

### `compression`

The server-wide compression policy. Compression is transparent to clients;
see [operations.md](operations.md#compression).

| YAML key | Env | Default | Runtime | Description |
|---|---|---|---|---|
| `compression.mode` | `GOS3_COMPRESSION_MODE` | `optional` | `compression.mode` | `optional`: each bucket's setting decides (off unless enabled through the admin API or the web UI). `enforced`: every bucket compresses new objects and cannot opt out. `disabled`: nothing new is compressed and buckets cannot enable it. Stored objects stay readable whatever the mode. |
| `compression.level` | `GOS3_COMPRESSION_LEVEL` | `1` | `compression.level` | DEFLATE level, 1 (fastest) to 9 (smallest). |
| `compression.min_size` | `GOS3_COMPRESSION_MIN_SIZE` | `4096` | `compression.minSize` | Objects smaller than this (when the size is known before the upload, i.e. not for chunked uploads without a length) are stored uncompressed. Bytes or `4KiB`-style. |
| `compression.skip_content_types` | `GOS3_COMPRESSION_SKIP_CONTENT_TYPES` | compressed images, `video/*`, `audio/*`, `font/woff*`, archives, zip-based document formats (see `gos3 config print`) | `compression.skipContentTypes` | Media types stored uncompressed because they are compressed already: `type/subtype`, `type/*` or a prefix ending in `*` (case-insensitive, parameters ignored), at most 256. `[]` compresses every type. Objects with a compressing `Content-Encoding` (`gzip`, `br`, `zstd`, …) are never compressed again. |

### Jobs and restic snapshots

Background [jobs](jobs.md) (copy, mirror, and the snapshots, restores and
prunes of [whole-bucket restic snapshots](encryption.md#whole-bucket-snapshots))
and the snapshot runs themselves. The jobs and the snapshot schedules are
managed through the admin API and the web UI; these keys only size the
machinery. They are static: a change takes effect at the next start. The
jobs are kept in `<data>/.gos3/jobs/` (in memory, with a warning, without a
system directory). See [operations.md](operations.md#jobs-and-restic-snapshots).

| YAML key | Env | Default | Description |
|---|---|---|---|
| `jobs.workers` | `GOS3_JOBS_WORKERS` | `2` | Jobs running at once, of every kind, 1–64; further jobs wait in the `queued` state. A mirror between passes does not count. The keys a single copy job copies in parallel are its own `concurrency` option (default 4). |
| `restic.snapshots.workers` | `GOS3_RESTIC_SNAPSHOTS_WORKERS` | `1` | Whole-bucket snapshot, restore and prune jobs running restic at once, 1–64. Each also takes one of the `jobs.workers`, so a value above `jobs.workers` has no effect. |
| `restic.restore_spool_bytes` | `GOS3_RESTIC_RESTORE_SPOOL_BYTES` | `1GiB` | Maximum encrypted temporary data reserved by concurrent per-object restic restores across all backings, at least 64 MiB. A single restore reserves at most half of it: a read of an object larger than half the limit restores it in windows of that size, one at a time per reader, so one slow download can never hold the whole limit and objects larger than the limit stay readable (each further window re-reads the object from the start, so very large objects read slower). A read that finds the limit reserved by other reads returns ServiceUnavailable, while a download already under way waits up to 30 s for room for its next window; a restore waiting for a `max_restores` slot reserves nothing until it starts; ranged reads reserve only the requested range. Bytes or `1GiB`-style. |
| `restic.max_restores` | `GOS3_RESTIC_MAX_RESTORES` | `4` | Per-object restic restores (`restic dump` processes) running at once across all backings, 1–64. Further restores wait for one to finish for as long as their request lasts and at most 30 s, then return ServiceUnavailable. Readers of a part of an object already being restored share that restore and need no slot. |
| `restic.snapshots.stage_dir` | `GOS3_RESTIC_SNAPSHOTS_STAGE_DIR` | `restic-stage/` in the system directory | Where snapshot runs build their trees, one subdirectory per repository and bucket, kept between runs. Needs about `spool_bytes` free, an inode per key of the largest bucket, and a file system that keeps sparse files and file times (see [operations.md](operations.md#restic-snapshot-stage)). Without a system directory (and without this setting) each run of the server uses a private temporary directory, created on first use (mode 0700) and removed at shutdown. |
| `restic.snapshots.spool_bytes` | `GOS3_RESTIC_SNAPSHOTS_SPOOL_BYTES` | `8GiB` | Data a run writes to the stage before it saves a partial snapshot and continues from it, at least 64 MiB (a single larger object is staged whole). Bytes or `8GiB`-style. |
| `restic.snapshots.incremental` | `GOS3_RESTIC_SNAPSHOTS_INCREMENTAL` | `true` | Stage unchanged objects as sparse placeholders, so a run reads only new and changed objects. `false` reads and stages every object at every run (restic still stores only new data). |

### Other

| Env | Flag | Description |
|---|---|---|
| `GOS3_CONFIG` | `--config` | Path of the YAML file. Without it, no file is read. |

## Example file

```yaml
server:
  address: ":9000"
  name: nas
  public_url: https://s3.example.com
  domains: [s3.example.com]
  trust_proxy_headers: true      # behind a reverse proxy ...
  trusted_proxies: [10.0.0.5]    # ... at this address
storage:
  backend: fs
  path: /srv/gos3
  backings:
    - name: offsite                # buckets that should live on S3
      backend: s3
      s3:
        endpoint: https://s3.eu-central-1.amazonaws.com
        region: eu-central-1
        home_bucket: nas-gos3      # wrapped buckets by default
        # credentials and encryption key from GOS3_BACKING_OFFSITE_ACCESS_KEY,
        # GOS3_BACKING_OFFSITE_SECRET_KEY and GOS3_BACKING_OFFSITE_ENCRYPTION_KEY
cors:
  allowed_origins: ["https://console.example.com"]
discovery:
  mdns: false
  seeds: ["http://backup.lan:9000"]
logging:
  format: json
metrics:
  public: true                   # scraped by Prometheus on a private network
```

Keep the root keys, and the `secret_key` and `encryption_key` of s3
backings, out of files that are readable by others. Pass them as
`GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY`, `GOS3_S3_*` and
`GOS3_BACKING_<NAME>_*`, or keep the YAML file at mode 0600.

## Runtime settings (`settings.json`)

The runtime subset is the `Settings` document of the admin API:

| Settings field | Static key |
|---|---|
| `name`, `region`, `publicUrl`, `domains`, `publicInfo` | `server.name`, `server.region`, `server.public_url`, `server.domains`, `server.public_info` |
| `uiRedirect` | `ui.redirect_root` |
| `cors.*` | `cors.*` |
| `discovery.*` | `discovery.*` |
| `logging.*` | `logging.*` |
| `lifecycle.enabled`, `lifecycle.intervalSeconds` | `lifecycle.enabled`, `lifecycle.interval` |
| `metricsPublic` | `metrics.public` |
| `compression.*` | `compression.*` (`minSize` = `min_size`, `skipContentTypes` = `skip_content_types`) |

How it behaves:

- At startup the static configuration (defaults, file, environment, flags)
  produces the initial settings. If `<data>/.gos3/settings.json` exists, it is
  laid over them and **wins** for every runtime field.
- The file is written atomically (mode 0600) the first time settings are saved
  through the UI or `PUT /config`, not at first start. Until then, changes to
  JSON, YAML, environment or flags take effect on the next restart as usual.
- After the first save, changing one of these keys in YAML, the environment or
  a flag has no effect. Change it in the UI or the admin API, or stop the
  server and delete `settings.json` to go back to the static configuration.
  At startup the server logs a warning for every explicitly configured value
  (file, environment or flag) that `settings.json` overrides, and
  `gos3 config print` lists them.
- Changes apply immediately and need no restart: log level and format, the
  access log, CORS, domains, region, discovery (only the mechanisms whose
  settings changed are restarted), the lifecycle worker and the compression
  policy (from the next write on). With
  `auth.strict_region`, signature checks follow a region change immediately.
- A corrupt or invalid `settings.json` stops the server at startup with an
  error that names the file.

## Command-line reference

```
gos3 [serve] [flags]          run the server (default command)
gos3 version [--json]         print the version
gos3 config print [flags]     print the effective configuration (secrets redacted)
gos3 config init [-o FILE] [--force]   write a commented sample configuration
gos3 config env               list the GOS3_* environment variables
gos3 discover [flags]         find gos3 servers on the local network
gos3 keygen [--json]          generate a random access/secret key pair (alias: gos3 keys gen)
gos3 healthcheck [flags]      check a server's health (exit 0 = healthy)
gos3 restic env|exec|objects|recover [flags]   a bucket's restic repositories: credentials, restic, archived objects
```

`gos3 restic` is described in
[encryption.md](encryption.md#the-gos3-restic-command) (`gos3 restic help`
lists its flags).

Flags can be written with one or two dashes (`-address` or `--address`). Exit
codes are 0 (success), 1 (error) and 2 (usage error).

### `gos3 serve`

| Flag | Config key |
|---|---|
| `--config FILE` | (`GOS3_CONFIG`) |
| `--address [host]:port` | `server.address` |
| `--data DIR` | `storage.path` |
| `--backend fs\|memory\|s3` | `storage.backend` (s3 is configured by `storage.s3` / `GOS3_S3_*`) |
| `--name NAME` | `server.name` |
| `--region REGION` | `server.region` |
| `--public-url URL` | `server.public_url` |
| `--domain DOMAIN` (repeatable) | `server.domains` |
| `--allowed-host HOST` (repeatable) | `server.allowed_hosts` |
| `--tls-cert FILE`, `--tls-key FILE` | `server.tls_cert`, `server.tls_key` |
| `--cors-origin ORIGIN` (repeatable) | `cors.allowed_origins` |
| `--seed URL` (repeatable) | `discovery.seeds` |
| `--no-ui` | `ui.enabled: false` |
| `--no-discovery` | `discovery.enabled: false` |
| `--log-level LEVEL`, `--log-format FORMAT` | `logging.level`, `logging.format` |
| `--fsync=false` | `storage.fsync_data: false` |

Only flags that are given override the lower layers. SIGINT or SIGTERM starts
a graceful shutdown, and a second signal exits immediately.

### `gos3 discover`

Looks for gos3 servers from the machine it runs on (mDNS query plus beacon
query), probes extra seeds and DNS SRV domains, and prints every server that
answers `/_gos3/info`. It never advertises anything.

| Flag | Default | Description |
|---|---|---|
| `--wait DURATION` | `3s` | How long to listen for mDNS and beacon answers. |
| `--json` | | Print the peers as JSON. |
| `--seed URL` | | Also probe this base URL (repeatable). |
| `--dns-srv DOMAIN` | | Also resolve `_gos3._tcp.<domain>` (repeatable). |
| `--no-mdns`, `--no-beacon` | | Skip a mechanism. |
| `--beacon-group ip:port` | `239.255.73.83:7373` | Beacon multicast group. |

### `gos3 healthcheck`

Requests `/_gos3/health` (or `/_gos3/ready` with `--ready`) and exits 0 when
the server answers `{"status":"ok"}`. Without `--url`, it derives
`http(s)://127.0.0.1:<port>` from `server.address` (configuration file,
`GOS3_ADDRESS`). When TLS is configured it uses `https` and skips certificate
verification, because it is checking its own server. This is the container
`HEALTHCHECK`.

| Flag | Default | Description |
|---|---|---|
| `--url URL` | derived | Server base URL. |
| `--ready` | | Check `/_gos3/ready` (storage reachable) instead of `/_gos3/health`. |
| `--timeout DURATION` | `5s` | Request timeout. |
| `--insecure` | | Skip TLS certificate verification. |
| `--config FILE` | `GOS3_CONFIG` | Configuration used to derive the URL. |
