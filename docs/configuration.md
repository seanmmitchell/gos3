# Configuration

gos3 is configured from four layers. Each layer overrides the one before it:

1. built-in defaults
2. a YAML file (`--config FILE`, or `GOS3_CONFIG=FILE`)
3. `GOS3_*` environment variables
4. command-line flags of `gos3 serve`

There is also a fifth, **runtime** layer. The settings marked *runtime* below can
be changed while the server runs, through the web UI (**Settings**) or the admin
API (`PUT /_gos3/admin/v1/config`, see [admin-api.md](admin-api.md)). gos3
writes them to `<data>/.gos3/settings.json`, and from then on that file wins
over the YAML file, the environment and the flags for those keys (see
[Runtime settings](#runtime-settings-settingsjson)).

Useful commands:

```sh
gos3 config init -o gos3.yaml   # commented sample file; its values are the defaults
gos3 config print [serve flags] # effective configuration (secrets redacted); exit 1 if invalid
gos3 config env                 # table of every GOS3_* variable
gos3 serve -h                   # flags
```

`gos3 config print` takes the same flags as `gos3 serve`, so you can check what
a command line would produce. When `<data>/.gos3/settings.json` exists, it
lays that file over the result (defaults < file < env < flags <
settings.json), as the server does, and lists the configured values it
overrides. Unknown YAML keys are an error, reported with
their line number (for example `line 3: unknown key "adress" in section "server"`).
Validation errors name the YAML key, for example
`cors.allowed_origins[1]: must not contain a path...`.

## Value syntax

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
| `server.trust_proxy_headers` | `GOS3_TRUST_PROXY_HEADERS` | | `false` | | Use `X-Forwarded-For` for the client address (`aws:SourceIp`, access log) and `X-Forwarded-Proto` for `aws:SecureTransport`. Applies to the S3 API and to the admin API (its `aws:SourceIp` / `aws:SecureTransport` conditions and the remote address in its access log). Enable it only behind a reverse proxy that overwrites these headers. |

### `storage`

| YAML key | Env | Flag | Default | Description |
|---|---|---|---|---|
| `storage.backend` | `GOS3_STORAGE_BACKEND` | `--backend` | `fs` | `fs` (files under `path`) or `memory` (everything is lost on exit). |
| `storage.path` | `GOS3_DATA` | `--data` | `./data` for `fs`, empty for `memory` | Data directory. With `fs` it holds the objects and the metadata database. With either backend it holds the system state in `<path>/.gos3/` (see [operations.md](operations.md#the-data-directory)). With `memory` and no path the server is fully ephemeral, including generated root credentials and IAM users. |
| `storage.fsync_data` | `GOS3_FSYNC` | `--fsync` | `true` | fsync object data before acknowledging a write. `--fsync=false` is faster but can lose recent writes on power loss. |

### `auth`

| YAML key | Env | Default | Description |
|---|---|---|---|
| `auth.root_access_key` | `GOS3_ROOT_ACCESS_KEY` | | Root access key: 3–128 characters of `[A-Za-z0-9._-]`. |
| `auth.root_secret_key` | `GOS3_ROOT_SECRET_KEY` | | Root secret key: 8–128 printable ASCII characters without spaces. |
| `auth.strict_region` | `GOS3_STRICT_REGION` | `false` | Reject signatures whose credential scope names a region other than `server.region`. By default any region is accepted. |

The root keys have no flags, so they never show up in process listings. Set
both or neither. When neither is set, gos3 generates a pair on first start,
logs it **once** as a warning (and in the startup banner), and stores it in
`<data>/.gos3/root-credentials.json` (mode 0600) for later starts. Without a
data directory (memory backend, no path) new keys are generated on every
start. `gos3 keygen` prints a random pair you can use instead. See
[security.md](security.md#root-credentials).

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
`x-amz-abort-date` and `x-amz-abort-rule-id`. The `*` makes `x-amz-meta-*`
readable, which the web UI's metadata editor needs. The explicit names cover
browsers that do not honour `*`. A standalone web UI only needs its origin in
`allowed_origins`; see [web-ui.md](web-ui.md#cors).

For S3 requests, the `*` entry admits only signed requests (an
`Authorization` header or a presigned URL); explicitly listed origins also
apply to anonymous requests. See [security.md](security.md#cors).

### `discovery`

All discovery keys are runtime settings (`discovery.<camelCase>` in the admin
API). [discovery.md](discovery.md) describes the protocol.

| YAML key | Env | Flag | Default | Description |
|---|---|---|---|---|
| `discovery.enabled` | `GOS3_DISCOVERY_ENABLED` | `--no-discovery` (sets false) | `true` | Run discovery (mDNS, beacon, seeds, DNS SRV). |
| `discovery.advertise` | `GOS3_DISCOVERY_ADVERTISE` | | `true` | Announce this server via mDNS and the beacon. |
| `discovery.mdns` | `GOS3_DISCOVERY_MDNS` | | `true` | Use mDNS / DNS-SD (`_gos3._tcp.local.`). |
| `discovery.beacon` | `GOS3_DISCOVERY_BEACON` | | `true` | Use the UDP multicast beacon. |
| `discovery.beacon_group` | `GOS3_DISCOVERY_BEACON_GROUP` | | `239.255.73.83:7373` | IPv4 multicast group and port. |
| `discovery.beacon_interval` | `GOS3_DISCOVERY_BEACON_INTERVAL` | | `10s` | Announce interval (±10 % jitter), 1s–1h. |
| `discovery.seeds` | `GOS3_DISCOVERY_SEEDS` | `--seed` | `[]` | Base URLs (or bare `host[:port]`, meaning `http://`) of servers to probe on start and every 30 s. At most 256. |
| `discovery.dns_srv` | `GOS3_DISCOVERY_DNS_SRV` | | `[]` | Domains whose `_gos3._tcp.<domain>` SRV records list servers (queried every 60 s). At most 64. |
| `discovery.peer_ttl` | `GOS3_DISCOVERY_PEER_TTL` | | `60s` | Peers not seen for this long are dropped, 30s–7d. |
| `discovery.allow_scan` | `GOS3_DISCOVERY_ALLOW_SCAN` | | `true` | Allow network scans through the admin API (users also need `admin:ScanNetwork`). |
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
  trust_proxy_headers: true      # behind a reverse proxy
storage:
  backend: fs
  path: /srv/gos3
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

Keep the root keys out of files that are readable by others. Pass them as
`GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY`, or keep the YAML file at mode
0600.

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

How it behaves:

- At startup the static configuration (defaults, file, environment, flags)
  produces the initial settings. If `<data>/.gos3/settings.json` exists, it is
  laid over them and **wins** for every runtime field.
- The file is written atomically (mode 0600) the first time settings are saved
  through the UI or `PUT /config`, not at first start. Until then, changes to
  YAML, environment or flags take effect on the next restart as usual.
- After the first save, changing one of these keys in YAML, the environment or
  a flag has no effect. Change it in the UI or the admin API, or stop the
  server and delete `settings.json` to go back to the static configuration.
  At startup the server logs a warning for every explicitly configured value
  (file, environment or flag) that `settings.json` overrides, and
  `gos3 config print` lists them.
- Changes apply immediately and need no restart: log level and format, the
  access log, CORS, domains, region, discovery (only the mechanisms whose
  settings changed are restarted) and the lifecycle worker. With
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
```

Flags can be written with one or two dashes (`-address` or `--address`). Exit
codes are 0 (success), 1 (error) and 2 (usage error).

### `gos3 serve`

| Flag | Config key |
|---|---|
| `--config FILE` | (`GOS3_CONFIG`) |
| `--address [host]:port` | `server.address` |
| `--data DIR` | `storage.path` |
| `--backend fs\|memory` | `storage.backend` |
| `--name NAME` | `server.name` |
| `--region REGION` | `server.region` |
| `--public-url URL` | `server.public_url` |
| `--domain DOMAIN` (repeatable) | `server.domains` |
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
