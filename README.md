# gos3

**gos3** is an S3-compatible object storage server in a single Go binary. It
comes with a built-in web console, IAM, versioning, object lock, lifecycle
rules, and opt-in discovery of other gos3 servers on your network.

Run `gos3 serve`, point the AWS CLI, an SDK, `mc` or rclone at it, and open
`http://localhost:9000/_gos3/ui/`.

**v0.2.0 is the first full gos3 release.** It brings together local and remote
S3 storage, bucket replication, compression, encryption, copy/mirror jobs and
restic backups. Start with the [0.2.0 release overview](docs/releases/0.2.0.md)
for downloads, what's new since 0.1.0, upgrade steps and limitations.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/dashboard-dark.png">
  <img alt="gos3 web UI: dashboard of connected servers" src="docs/screenshots/dashboard.png">
</picture>

## Features

- **S3 API**: buckets, objects, multipart uploads (including UploadPartCopy),
  ranges, conditional reads and writes, copy, tagging, presigned URLs, browser
  POST uploads, path-style and virtual-hosted-style addressing. SigV4 with
  header auth, presigned URLs and `aws-chunked` streaming with trailers (the
  default of current AWS SDKs). See [docs/s3-compatibility.md](docs/s3-compatibility.md).
- **IAM and policies**: a root user, IAM users with access keys, built-in
  and custom identity policies, bucket policies with conditions, canned ACLs,
  Block Public Access, and a policy simulator.
- **Data protection**: versioning with delete markers, object lock
  (governance and compliance retention, legal hold), and lifecycle expiration
  of current and noncurrent versions, delete markers and stale multipart uploads.
- **Checksums**: CRC32, CRC32C, CRC64NVME, SHA-1 and SHA-256, as headers or
  trailers, full-object or composite for multipart uploads, plus MD5/ETag.
- **Multiple backings**: spread buckets over several disks or directories,
  move buckets between them online, and replicate a bucket to other backings
  in active-passive or active-active mode with automatic failover. See
  [docs/operations.md](docs/operations.md#multiple-backings-and-replication).
- **Compression**: transparent, seekable DEFLATE compression of object data,
  enabled per bucket, or enforced or disabled server-wide by the
  administrator. Already-compressed media types are skipped. See
  [docs/operations.md](docs/operations.md#compression).
- **S3 as a storage backend**: connect an account of AWS S3, MinIO, R2 or
  another S3 service and keep a whole server, or individual buckets, there
  with no local data directory. Wrapped buckets get every gos3 feature
  (many of them share one remote bucket, on services that support
  conditional writes); native buckets are remote buckets used as they
  are, so other tools keep working with them, with their versioning and
  object lock passed through. Browse the account's buckets, adopt existing
  ones, and add and edit S3 backings live from the web UI. See
  [docs/backings.md](docs/backings.md).
- **Copy and mirror jobs**: copy a bucket into another once, or keep it
  mirrored, across backings and bucket kinds (native buckets included)
  and under another name, with versions, metadata, object lock and bucket
  configuration. Jobs report progress and lag, can be paused and resumed,
  and survive restarts. See [docs/jobs.md](docs/jobs.md).
- **Restic backups**: archive every version written to a bucket in an
  encrypted restic repository, or snapshot whole buckets on a schedule
  with retention, and restore them with gos3 or with plain `restic`,
  without gos3. See [docs/encryption.md](docs/encryption.md#whole-bucket-snapshots).
- **Web UI**: embedded in the binary or run standalone to manage many gos3
  and other S3 servers. It has an object browser with uploads, versions,
  metadata and share links, bucket settings, users and policies, server
  settings, metrics, and discovery.
- **Discovery**: servers announce themselves through mDNS and a UDP multicast
  beacon, and find each other through static seeds, DNS SRV records and
  on-demand network scans. The UI merges what every server sees.
- **Simple to run**: one static binary or a distroless image. JSON, YAML,
  environment variables or flags, runtime settings without restarts,
  Prometheus metrics, health and readiness endpoints, and graceful shutdown.

## Quick start

### Binary

Download an archive from [v0.2.0](https://github.com/seanmmitchell/gos3/releases/tag/v0.2.0)
(Linux, macOS, Windows; amd64 and arm64). For public-repository builds from
v0.2.0 onward,
`gh attestation verify <archive> --repo seanmmitchell/gos3 --source-ref refs/heads/main`
checks that it was built by the Release workflow on `main` (see
[verifying a release](docs/releasing.md#verifying-a-release)). Or build
it with Go 1.26.9 or newer, Node.js 22 and pnpm:

```sh
make build                      # web UI + bin/gos3 with the UI embedded
bin/gos3 serve --data ./data
```

The startup banner shows the S3 endpoint, the web UI URL and where the root
credentials come from. Without `GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY`,
a key pair is generated on first start and stored in
`./data/.gos3/root-credentials.json` (the banner shows it when it runs in a
terminal; the secret is never logged). To choose your own:

```sh
export $(bin/gos3 keygen)       # random GOS3_ROOT_ACCESS_KEY / GOS3_ROOT_SECRET_KEY
bin/gos3 serve --data ./data
```

### Docker

```sh
(umask 077; docker run --rm ghcr.io/seanmmitchell/gos3:0.2.0 keygen > gos3.env)  # private root keys
docker run -d --name gos3 -p 127.0.0.1:9000:9000 -v gos3-data:/data --env-file gos3.env \
  ghcr.io/seanmmitchell/gos3:0.2.0
```

This publishes the port on this machine only; use `-p 9000:9000` to serve
the network. `X.Y.Z` and `X.Y` are releases, `latest` is the newest release,
and `dev` follows the development branch. Public-repository builds from v0.2.0
onward carry signed build provenance:
`gh attestation verify oci://ghcr.io/seanmmitchell/gos3:0.2.0 --repo seanmmitchell/gos3 --source-ref refs/heads/main`.
Builds published while the repository is private have no GitHub attestations;
see [verification and its limits](docs/releasing.md#verifying-a-release).

To build the image yourself, run `docker build -t gos3 .` or `make docker`.
The image runs as a non-root user (uid 65532), keeps its data in `/data`, has
a health check, and includes the restic executable that restic-encrypted
buckets use.

### Docker Compose: two nodes with discovery

```sh
make demo                                   # node-1 on :9001, node-2 on :9002, standalone UI on :3000
```

`make demo` writes random root keys to `.env` on first use (or put your own
`GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY` there, then run
`docker compose --profile ui up -d --build`). Sign in at
`http://localhost:9001/_gos3/ui/` with the keys from `.env` and open
**Discovery**: the nodes find each other through the UDP beacon, with seeds
as a fallback. Remove everything with `docker compose --profile ui down -v`.
The ports are published on 127.0.0.1 only; see
[docs/deployment.md](docs/deployment.md#docker-compose) to expose the demo.

### Standalone web UI

```sh
cd web && pnpm install && pnpm build
NUXT_PUBLIC_SERVERS='[{"name":"NAS","endpoint":"http://10.0.0.5:9000"}]' node .output/server/index.mjs
```

It also runs as a container (`deploy/docker/web.Dockerfile`). The browser talks
to each server directly, so every server must allow the UI origin via CORS
(gos3 allows any origin by default). Its local discovery routes (mDNS,
beacon, scans) are off unless you set `GOS3_UI_LOCAL_DISCOVERY=true`; even
then they only answer when the UI is opened via an IP address or `localhost`
(add any DNS name you use to `GOS3_UI_ALLOWED_HOSTS`). See
[docs/web-ui.md](docs/web-ui.md).

## Configuration

Choose how the server is managed:

```sh
gos3 serve --no-admin                 # S3 only, static configuration
gos3 serve --no-ui                    # S3 + management API for a separate UI
gos3 serve                            # S3 + management API + embedded UI (default)
gos3 serve --config gos3.json          # JSON or YAML configuration
make build-headless                   # no UI assets, Node or pnpm required
```

The release and dev workflows also publish binaries and images without UI
assets compiled in. Choose `gos3_X.Y.Z_linux_amd64-headless.tar.gz` (or the
matching OS/architecture archive, `.zip` on Windows), and image tags
`ghcr.io/seanmmitchell/gos3:X.Y.Z-headless`, `latest-headless` or `dev-headless`.
These builds retain the management API; use `serve --no-admin` for S3 only.
Build the image locally with `docker build --target headless -t gos3:headless .`.
The ordinary archives and image tags continue to include the UI.

Discovery, advertising, mDNS, beacons and scans all default to off.
Set the discovery switches explicitly to enable them. To replace a basic
MinIO startup command, use `gos3 server /data --address :9000 --no-admin`;
`MINIO_ROOT_USER` and `MINIO_ROOT_PASSWORD` are accepted as credential
aliases. See [server modes and MinIO compatibility](docs/configuration.md#server-modes)
for JSON examples, precedence, health probes and migration limits.

Settings come from defaults, then a JSON or YAML file (`--config` or `GOS3_CONFIG`), then
`GOS3_*` environment variables, then flags. Each layer overrides the one before.

```sh
gos3 config init -o gos3.yaml   # commented sample configuration
gos3 config init -o gos3.json   # all settings as JSON
gos3 config print --json        # effective JSON, secrets redacted
gos3 config env                 # every GOS3_* variable
gos3 config print               # effective configuration, secrets redacted
```

Frequently used settings:

| Setting | Flag / env | Default |
|---|---|---|
| Listen address | `--address` / `GOS3_ADDRESS` | `:9000` |
| Data directory | `--data` / `GOS3_DATA` | `./data` |
| Backend | `--backend fs\|memory\|s3` / `GOS3_STORAGE_BACKEND` (s3: `GOS3_S3_*`) | `fs` |
| Root keys | `GOS3_ROOT_ACCESS_KEY`, `GOS3_ROOT_SECRET_KEY` | generated |
| Region | `--region` / `GOS3_REGION` | `us-east-1` |
| Public URL | `--public-url` / `GOS3_PUBLIC_URL` | derived |
| Virtual-host domains | `--domain` / `GOS3_DOMAINS` | none (path-style) |
| TLS | `--tls-cert`, `--tls-key` / `GOS3_TLS_CERT`, `GOS3_TLS_KEY` | off |
| CORS origins | `--cors-origin` / `GOS3_CORS_ORIGINS` | `*` |
| Discovery | `--no-discovery`, `--seed` / `GOS3_DISCOVERY_*` | off (opt-in) |

When management is enabled, runtime settings (name, region, CORS, domains, discovery, logging, lifecycle,
public metrics) can also be changed from the UI or the admin API without a
restart. They are then stored in `<data>/.gos3/settings.json` and take
precedence over the file, environment and flags. Every key is listed in
[docs/configuration.md](docs/configuration.md).

With `--no-admin`, the server ignores saved UI settings and uses only
defaults, the config file, environment and flags. Existing saved settings
are preserved for a later managed startup.

## Using gos3 with S3 clients

Use the server URL as the endpoint, **path-style** addressing (unless you set
up [virtual-host domains](docs/deployment.md#virtual-hosted-style-domains)),
and region `us-east-1` (or whatever `server.region` is). The examples use
`http://localhost:9000` and the stand-in keys `admin` / `change-me-now`:
use your server's keys instead.

### AWS CLI

```sh
export AWS_ACCESS_KEY_ID=admin AWS_SECRET_ACCESS_KEY=change-me-now AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://localhost:9000      # or --endpoint-url on every command

aws s3 mb s3://photos
aws s3 cp ./cat.jpg s3://photos/
aws s3 ls s3://photos
aws s3api put-bucket-versioning --bucket photos --versioning-configuration Status=Enabled
aws s3 presign s3://photos/cat.jpg --expires-in 3600
```

AWS CLI **v1** presigns with Signature Version 2, which gos3 rejects. Configure
SigV4 (and path-style) in `~/.aws/config`:

```ini
[default]
s3 =
    signature_version = s3v4
    addressing_style = path
```

### Python (boto3)

```python
import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url="http://localhost:9000",
    aws_access_key_id="admin",
    aws_secret_access_key="change-me-now",
    region_name="us-east-1",
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
)
s3.create_bucket(Bucket="photos")
s3.upload_file("cat.jpg", "photos", "cat.jpg")
print(s3.generate_presigned_url("get_object", Params={"Bucket": "photos", "Key": "cat.jpg"}))
```

### Go (aws-sdk-go-v2)

```go
client := s3.New(s3.Options{
	BaseEndpoint: aws.String("http://localhost:9000"),
	Region:       "us-east-1",
	Credentials:  credentials.NewStaticCredentialsProvider("admin", "change-me-now", ""),
	UsePathStyle: true,
})
_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("photos")})
```

### JavaScript (AWS SDK v3)

```js
import { S3Client, ListBucketsCommand } from "@aws-sdk/client-s3";

const s3 = new S3Client({
  endpoint: "http://localhost:9000",
  region: "us-east-1",
  forcePathStyle: true,
  credentials: { accessKeyId: "admin", secretAccessKey: "change-me-now" },
});
console.log(await s3.send(new ListBucketsCommand({})));
```

### MinIO client (mc)

```sh
mc alias set gos3 http://localhost:9000 admin change-me-now
mc mb gos3/photos
mc cp ./cat.jpg gos3/photos/
mc ls gos3/photos
```

### rclone

```ini
# ~/.config/rclone/rclone.conf
[gos3]
type = s3
provider = Other
endpoint = http://localhost:9000
access_key_id = admin
secret_access_key = change-me-now
region = us-east-1
force_path_style = true
```

```sh
rclone mkdir gos3:photos
rclone copy ./pictures gos3:photos
rclone ls gos3:photos
```

For applications, create IAM users with narrow policies in the web UI
(**Users**, **Policies**) instead of using the root keys.

## Web UI

The UI is embedded at **`/_gos3/ui/`**. Browsers opening `/` are redirected
there. It detects the server it is served from, so you only enter your keys. It
can also manage other gos3 servers and generic S3 services, and it runs as a
standalone Node app with extra local discovery. Requests are signed with SigV4
in the browser; there are no server-side sessions.

| | |
|---|---|
| ![Object browser](docs/screenshots/browser.png) | ![Object details](docs/screenshots/object-details.png) |
| Object browser: folders, uploads, bulk actions | Object details: metadata, tags, versions, share links |
| ![Bucket settings](docs/screenshots/bucket-settings.png) | ![Users](docs/screenshots/users.png) |
| Bucket settings: versioning, policy, CORS, lifecycle, lock | IAM users, access keys and policies |
| ![Server overview](docs/screenshots/overview.png) | ![Discovery](docs/screenshots/discovery.png) |
| Server overview: traffic, operations, storage | Discovery: peers, scans, one-click add |

<details>
<summary>Dark mode</summary>

![Object browser, dark](docs/screenshots/browser-dark.png)

</details>

Details on the embedded and standalone modes, presets (`servers.json`,
`NUXT_PUBLIC_SERVERS`) and CORS are in [docs/web-ui.md](docs/web-ui.md).

## Discovery

Each gos3 server publishes an identity document at `/_gos3/info`. Discovery
defaults to off; enable `discovery.enabled` and the mechanisms you want:

| Mechanism | Default | Notes |
|---|---|---|
| mDNS / DNS-SD `_gos3._tcp.local.` | off | Enable `discovery.mdns`; `discovery.advertise` enables announcements |
| UDP multicast beacon `239.255.73.83:7373` | off | Enable `discovery.beacon`; `discovery.advertise` enables announcements |
| Static seeds | none | `--seed http://nas:9000` |
| DNS SRV `_gos3._tcp.<domain>` | none | For example a Kubernetes headless Service |
| Network scan | off | Enable `discovery.allow_scan`; needs `admin:ScanNetwork` |

Every candidate is verified through `/_gos3/info` before it is listed as a
peer. `gos3 discover` explicitly runs a one-shot search from the command line,
including mDNS and beacons unless disabled for that command. The protocol is
specified in [docs/discovery.md](docs/discovery.md).

## Security notes

- **Root credentials**: set `GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY`, or
  use the generated pair stored in `<data>/.gos3/root-credentials.json` (the
  secret is shown in the startup banner on a terminal, never in the log).
  Published example secrets are refused. Use IAM users for applications.
- **Sensitive files**: `<data>/.gos3/iam.json` holds every IAM user's secret
  key, and `root-credentials.json` holds generated root keys. Both have mode
  0600. Keep the data directory private, and treat backups as secret.
- **TLS**: gos3 speaks plain HTTP unless you pass `--tls-cert`/`--tls-key` or
  put it behind a TLS reverse proxy. Do not send keys over the internet in
  clear text.
- **CORS**: the default (`*`) allows any origin for signed requests only, so
  other websites cannot read anonymously accessible data through a visitor's
  browser. Narrow it to your UI origins in production, and use a bucket CORS
  configuration to serve a public bucket to scripts on other sites.
- **Discovery and scans**: all switches default to off. Explicitly enabled
  advertising announces the server's name, version and URLs on the LAN.
  Network scans require `discovery.allow_scan: true` and `admin:ScanNetwork`.
  The standalone UI's Node server performs
  local discovery only when `GOS3_UI_LOCAL_DISCOVERY=true`, and then for
  anyone who can reach it.
- **Object encryption**: SSE-S3 encrypts new object data using server-managed
  keys. Per-bucket restic repositories provide encrypted archival on the
  bucket's backing, a local directory or the container of a wrapped bucket
  on an S3 backing (restic is bundled in the container images; install it
  next to a release binary). On S3 backings the keys are wrapped with a
  key-encryption key you keep; native buckets are stored as they are. Existing objects require rewriting to
  encrypt them; back up the protected keys and metadata. See [encryption](docs/encryption.md).

More in [docs/security.md](docs/security.md).

## Documentation

| | |
|---|---|
| [docs/releases/0.2.0.md](docs/releases/0.2.0.md) | first full release: capabilities, downloads, upgrade notes and limitations |
| [docs/configuration.md](docs/configuration.md) | every YAML key, environment variable and flag |
| [docs/deployment.md](docs/deployment.md) | Docker, Compose, systemd, Kubernetes, reverse proxies, TLS |
| [docs/backings.md](docs/backings.md) | storage backings: S3 accounts as a backend (whole server or per bucket), native and wrapped buckets, containers, the remote bucket browser, live configuration |
| [docs/jobs.md](docs/jobs.md) | copy and mirror jobs between buckets, backings and bucket kinds |
| [docs/encryption.md](docs/encryption.md) | native AES256 and restic encryption, the restic archive, whole-bucket snapshots, keys and recovery |
| [docs/operations.md](docs/operations.md) | health checks, metrics, logs, backups, lifecycle, upgrades |
| [docs/security.md](docs/security.md) | authentication, stored secrets, exposure checklist |
| [docs/s3-compatibility.md](docs/s3-compatibility.md) | supported S3 operations and differences from AWS |
| [docs/web-ui.md](docs/web-ui.md) | embedded and standalone UI, presets, CORS |
| [docs/admin-api.md](docs/admin-api.md) | admin JSON API |
| [docs/discovery.md](docs/discovery.md) | discovery protocol |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | internals |
| [docs/releasing.md](docs/releasing.md) | branches, branch rules, versioning, releases and dev builds |

## Development

Requirements: Go 1.26.9 or newer, Node.js 22 with corepack (pnpm), and
optionally Docker and Python 3 (for the compatibility suite). `go.mod`
requires Go 1.26.9 (`go 1.26.9`, a supported release with current security
fixes for the standard library gos3 links); with the default
`GOTOOLCHAIN=auto` an older go command downloads it, and with
`GOTOOLCHAIN=local` it refuses to build.

```sh
make dev          # in-memory server on 127.0.0.1:9000 (discovery off, random secret printed, CORS for :3000)
cd web && pnpm dev   # UI with hot reload on http://localhost:3000
```

| Target | |
|---|---|
| `make build` | Web UI + `bin/gos3` with the UI embedded (`make build-go`: Go only) |
| `make test` | Go tests with `-race` plus the web unit tests (Vitest) |
| `make e2e` | `test/e2e`: real servers in-process, driven by aws-sdk-go-v2 and minio-go |
| `make e2e-web` | Playwright: mocked UI, UI embedded in a real gos3, standalone UI cross-origin |
| `make compat` | `test/compat`: boto3 / pytest and AWS CLI against a real binary |
| `make lint`, `make fmt` | gofmt and go vet |
| `make docker`, `make docker-ui` | container images |
| `make demo`, `make demo-down` | the Docker Compose demo |
| `make ui-clean` | Restore the UI placeholder in `internal/webui/dist` (run before committing after `make ui`) |

`make help` lists all targets.

Repository layout:

```
cmd/gos3/            CLI (serve, config, discover, keygen, healthcheck, version)
internal/server/     wiring, top-level routing, TLS, graceful shutdown
internal/s3/         S3 HTTP layer          internal/auth/       SigV4, chunked uploads, POST policy
internal/storage/    backends, replication, encryption, copying
internal/iam/        users, keys, policies
internal/jobs/       persistent job engine  internal/copyjob/   copy and mirror jobs
internal/resticsnap/ whole-bucket snapshots and restores
internal/admin/      admin JSON API         internal/discovery/  mDNS, beacon, seeds, SRV, scan
internal/lifecycle/  lifecycle rules        internal/config/     configuration and runtime settings
internal/metrics/    Prometheus metrics     internal/webui/      embedded UI
web/                 Nuxt 4 web UI (see web/README.md)
test/e2e/            Go end-to-end tests    test/compat/         Python / AWS CLI compatibility suite
deploy/              systemd unit, Kubernetes manifests, standalone UI Dockerfile
docs/                documentation
```

CI (`.github/workflows/ci.yml`) runs gofmt, vet, govulncheck, the Go unit and
e2e tests with `-race`, the web typecheck, tests and builds, the Playwright suites, the
compatibility suite, and the Docker builds. The Go tests run on Linux, macOS and
Windows with a pinned restic installed (`.github/actions/setup-restic`), so the
restic repository tests run instead of skipping.

Pull requests target `dev`, the default branch; `main` only receives release
merges from `dev`. Every push to `dev` publishes `ghcr.io/seanmmitchell/gos3:dev`,
and the Release workflow tags `main` with the next version and publishes the
archives and image. Branch rules, versioning and the release steps are in
[docs/releasing.md](docs/releasing.md).

## Limitations

- **Independent servers.** Bucket replicas belong to the backings of one
  gos3 server; there is no erasure coding or distributed cluster. Copy/mirror
  jobs can reach another server through an S3 backing, but discovery does
  not combine servers or their bucket namespaces. Use backups
  (see [docs/operations.md](docs/operations.md#backups)) for durability.
- One account: IAM users share the bucket namespace, and there is no STS and
  no session tokens.
- Not implemented: SigV2, SigV4A, SSE-C, ACL grants beyond canned ACLs,
  RestoreObject, SelectObjectContent, analytics, inventory and metrics
  configurations. Lifecycle transitions, website, notification, logging and
  replication configurations are stored but not acted on. Details are in
  [docs/s3-compatibility.md](docs/s3-compatibility.md).
- AWS KMS encryption is not implemented. Native AES256 and restic encrypt
  new objects when requested or enabled as the bucket default.
- TLS certificates are loaded at startup; restart after renewal.

## License

MIT; see [LICENSE](LICENSE). The licenses of the third-party code that gos3
and the standalone web UI include are reproduced in
[THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES). The file ships in the release
archives and in both container images (`/usr/share/doc/gos3/` and
`/usr/share/doc/gos3-ui/`). Regenerate it with `make notices` after changing
dependencies; CI fails when it is out of date.
