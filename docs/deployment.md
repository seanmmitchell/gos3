# Deployment

gos3 is one static binary, available with or without the embedded web UI.
It needs a data directory and one TCP port (9000 by default). Discovery uses UDP multicast
(`239.255.73.83:7373`, mDNS `224.0.0.251:5353`) when it is enabled.

Every gos3 server is **independent**: there is no replication or clustering.
Running several servers gives you several sets of buckets, which discovery and
the web UI help you find and manage. Do not put several gos3 servers behind one
load balancer.

- [Binary](#binary)
- [Docker](#docker) and [Docker Compose](#docker-compose)
- [systemd](#systemd)
- [Kubernetes](#kubernetes)
- [Reverse proxy](#reverse-proxy), [virtual-hosted-style domains](#virtual-hosted-style-domains) and [TLS](#tls)

## Binary

Release archives for Linux, macOS and Windows (amd64 and arm64) come in two
variants: `gos3_X.Y.Z_<os>_<arch>.tar.gz` with the UI embedded, and
`gos3_X.Y.Z_<os>_<arch>-headless.tar.gz` with no UI assets compiled in
(`.zip` on Windows). Both contain a `gos3` executable (`gos3.exe` on Windows).
The release also includes `checksums.txt` covering both variants. From the first release
after 0.1.0, every archive and image has a signed build provenance
attestation; check it before installing (see
[releasing.md](releasing.md#verifying-a-release)):

```sh
gh attestation verify gos3_X.Y.Z_linux_amd64.tar.gz \
  --repo seanmmitchell/gos3 --source-ref refs/heads/main
```

To build from source you need Go 1.26.9 or newer (`go.mod` requires it, for
the standard library's security fixes), Node.js 22 and pnpm (via corepack):

```sh
make build                    # web UI + bin/gos3 with the UI embedded
bin/gos3 serve --data /srv/gos3
```

`make build-go` (or `go build ./cmd/gos3`) builds without rebuilding the UI;
it embeds the current contents of `internal/webui/dist` (a placeholder in a
fresh checkout). For a binary with no UI assets, only Go is needed:

```sh
make build-headless           # go build -tags noui, no Node or pnpm
bin/gos3 serve --data /srv/gos3             # S3 + management API
bin/gos3 serve --data /srv/gos3 --no-admin  # S3 only
```

Headless builds cannot enable the embedded UI at runtime. An ordinary build
started with `--no-ui` still contains its compiled UI assets.

On first start without configured root keys, gos3 generates an access key
and secret key and stores them in `<data>/.gos3/root-credentials.json`
(mode 0600). The banner shows them when gos3 runs in a terminal; logs never
contain the secret. `gos3 keygen` prints a random pair as
`GOS3_ROOT_ACCESS_KEY=…` / `GOS3_ROOT_SECRET_KEY=…` lines for an env file.
See [configuration.md](configuration.md) for all settings and
[security.md](security.md) before exposing the server.

## Docker

The default image (`Dockerfile`, target `embedded`) builds the web UI with pnpm, a
static Go binary with the UI embedded (cross-compiled for the target
architecture), and a `gcr.io/distroless/static-debian12:nonroot` runtime. That
runtime has no shell, runs as uid/gid 65532, and is about 25 MB.

The `headless` target builds with `-tags noui` and skips the Node/pnpm stage
entirely. Both targets use the same runtime and support both architectures.
Published headless tags append `-headless` to the regular tags:
`X.Y.Z-headless`, `X.Y-headless`, `latest-headless` and `dev-headless`
(also `X-headless` from 1.0.0 and `dev-<commit>-headless`). For a local build:

```sh
docker build --target headless -t gos3:headless .
docker run -d --name gos3 -p 127.0.0.1:9000:9000 -v gos3-data:/data \
  --env-file gos3.env gos3:headless serve --no-admin
```

Omit `--no-admin` to keep the management API enabled for a separate web UI.

```sh
docker build -t gos3 .              # or: make docker (adds version build args)
docker run --rm gos3 keygen > gos3.env   # random root keys (keep the file private)
docker run -d --name gos3 -p 127.0.0.1:9000:9000 -v gos3-data:/data \
  --env-file gos3.env gos3
docker logs gos3                    # banner: endpoints, UI URL, credentials source
```

`-p 127.0.0.1:9000:9000` publishes the port on the host's loopback only;
drop the address (`-p 9000:9000`) to serve the network. The published images
are `ghcr.io/seanmmitchell/gos3:X.Y.Z` (also `X.Y` and `latest`); deploy a
version, not `latest`, and pin its digest after verifying it:

```sh
gh attestation verify oci://ghcr.io/seanmmitchell/gos3:X.Y.Z \
  --repo seanmmitchell/gos3 --source-ref refs/heads/main
```

Image defaults:

| | |
|---|---|
| Entrypoint / command | `gos3` / `serve` (append flags: `docker run gos3 serve --log-format json`; but see the health check note below) |
| Environment | `GOS3_ADDRESS=:9000`, `GOS3_DATA=/data` |
| Volume | `/data` (objects, metadata, IAM, settings, generated credentials) |
| Ports | `9000/tcp` (everything); `7373/udp`, `5353/udp` (discovery multicast) |
| User | `65532:65532` |
| Health check | `gos3 healthcheck` (GET `/_gos3/health` every 30 s) |

Notes:

- **Set the listen address and TLS through `GOS3_*` variables** (or a config
  file via `GOS3_CONFIG`), not as `serve` flags. The health check runs
  `gos3 healthcheck` as a separate process, which derives the URL from the
  config file and the environment only. With `serve --address :8443` or
  `serve --tls-cert … --tls-key …` it would keep probing
  `http://127.0.0.1:9000` and mark the container unhealthy (which also blocks
  `depends_on: condition: service_healthy`). Use `-e GOS3_ADDRESS=:8443`,
  `-e GOS3_TLS_CERT=… -e GOS3_TLS_KEY=…` instead. Other flags, such as
  `--log-format`, are fine.
- **Bind mounts** must be writable by uid 65532:
  `install -d -o 65532 -g 65532 -m 0700 /srv/gos3`, then
  `-v /srv/gos3:/data`. Named volumes get the right owner automatically.
- Without `GOS3_ROOT_*`, the generated keys are only in the volume, at
  `/data/.gos3/root-credentials.json` (shown in the banner only with
  `docker run -it`). The image has no shell; copy the file out with
  `docker cp gos3:/data/.gos3/root-credentials.json .` and delete the copy
  once you have stored the keys.
- **Discovery across hosts** needs multicast, which Docker's default bridge
  network keeps local to the host. Use `--network host`, or seeds
  (`GOS3_DISCOVERY_SEEDS`) and DNS SRV. Set `GOS3_PUBLIC_URL` to the URL other
  machines should use, because the container only knows its internal address.
- The image is built for `linux/amd64` and `linux/arm64`:
  `docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/gos3 --push .`.
- Build arguments: `VERSION`, `COMMIT` and `DATE` (shown by `gos3 version`).
- The base images (`node:22-bookworm-slim`, `golang:1.26-bookworm`,
  `gcr.io/distroless/static-debian12:nonroot`) and the Dockerfile frontend
  are pinned by digest, and the GitHub Actions in `.github/workflows` by
  commit SHA, so a retagged upstream image or action cannot change a release.
  Dependabot proposes updates weekly (Go images without the 14-day
  cooldown, since Go point releases carry standard library security fixes).
  To build on another Go or Node version, edit the tag and the digest of the
  `FROM` line together. The golang image builds with its own Go
  (`GOTOOLCHAIN=local`), so the build fails when that Go is older than
  `go.mod`'s `go` line (the minimum with current standard library fixes);
  raise both together when the minimum goes up.

The standalone web UI has its own image, `deploy/docker/web.Dockerfile`
(`make docker-ui`); see [web-ui.md](web-ui.md#standalone-mode). Its local
discovery routes are off unless you set `GOS3_UI_LOCAL_DISCOVERY=true`; do
that only where every client that can reach the container is trusted (for
example `-p 127.0.0.1:3000:3000`).

## Docker Compose

`docker-compose.yml` is a two-node demo:

```sh
make demo                                     # random root keys in .env, then the next line
docker compose --profile ui up -d --build     # gos3-1 on :9001, gos3-2 on :9002, standalone UI on :3000
docker compose up -d --build                  # the two nodes only
docker compose --profile ui down -v           # stop and delete the data volumes
```

- Both nodes use the root keys `GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY`
  from your shell or the `.env` file next to `docker-compose.yml`. There are
  no default keys: compose refuses to start without them. `make demo` appends
  a random pair to `.env` when none is set (or run
  `docker run --rm gos3 keygen >> .env`).
- Ports 9001, 9002 and 3000 are published on **127.0.0.1 only**. To reach
  the demo from other machines, set `GOS3_DEMO_BIND=0.0.0.0` (or one host
  address). Docker's published ports usually bypass the host firewall.
- The nodes find each other through the **UDP beacon** on the shared compose
  network, and each lists the other as a **seed** as a fallback. Open
  `http://localhost:9001/_gos3/ui/`, sign in and go to **Discovery**: the other
  node appears with sources `beacon` and `static`.
- Peers are listed with their container addresses (for example
  `http://172.18.0.3:9000`), which a browser on the host may not reach (Docker
  Desktop). Add the second node through its published port,
  `http://localhost:9002`.
- The standalone UI is preset with both nodes (`NUXT_PUBLIC_SERVERS`). Its
  local discovery (finding the nodes through the beacon inside the network)
  is off, as in the image, because its routes have no authentication. Turn
  it on with `GOS3_UI_LOCAL_DISCOVERY=true` only while the UI is published
  on `127.0.0.1` (the default `GOS3_DEMO_BIND`). Local discovery answers
  only when the UI is opened via an IP address or `localhost`, and with
  presets configured the console itself answers `421` for public DNS names
  (DNS-rebinding protection); if you open it through such a name, set
  `GOS3_UI_ALLOWED_HOSTS=<name>` (passed through by the compose file).

## systemd

`deploy/systemd/gos3.service` is a hardened unit. It uses a dynamic service
user, keeps its state in `/var/lib/gos3`, makes the file system read-only, has
no capabilities, and restricts system calls, namespaces and address families.

```sh
install -m 0755 bin/gos3 /usr/local/bin/gos3
install -m 0644 deploy/systemd/gos3.service /etc/systemd/system/
install -d -m 0755 /etc/gos3
install -m 0600 deploy/systemd/gos3.env.example /etc/gos3/gos3.env   # set the root keys
systemctl daemon-reload
systemctl enable --now gos3
journalctl -u gos3 -f
```

- `/etc/gos3/gos3.env` is read by systemd as root, so it can hold
  `GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY` at mode 0600
  (`gos3 keygen | sudo tee -a /etc/gos3/gos3.env`). Without them, gos3
  generates a pair on first start and stores it in
  `/var/lib/gos3/.gos3/root-credentials.json`; the journal names the access
  key and the file, never the secret.
- For a YAML configuration, set `GOS3_CONFIG=/etc/gos3/config.yaml` in the
  env file. The service user must be able to read the YAML file, so make it
  0644 and keep secrets out of it.
- To listen on port 80 or 443 directly, add
  `AmbientCapabilities=CAP_NET_BIND_SERVICE` and
  `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` in a drop-in
  (`systemctl edit gos3`).
- TLS certificate and key files must be readable by the dynamic service
  user: put the certificate in `/etc/gos3` with mode 0644 and set
  `GOS3_TLS_CERT` in the env file. A private key with the usual mode 0600 is
  not readable by that user, so pass it as a systemd credential in a drop-in
  (`systemctl edit gos3`); `%d` does not work in the env file:

  ```ini
  [Service]
  LoadCredential=tls.key:/etc/ssl/private/gos3.key
  Environment=GOS3_TLS_KEY=%d/tls.key
  ```
- `systemctl stop gos3` sends SIGTERM: in-flight requests get
  `server.shutdown_timeout` (30 s) to finish.

## Kubernetes

`deploy/kubernetes/` contains a ConfigMap (the YAML configuration), an example
Secret (root keys), a headless Service, a client Service, a StatefulSet and
an optional NetworkPolicy example, tied together by `kustomization.yaml`:

```sh
kubectl create namespace storage
kubectl -n storage create secret generic gos3-root-credentials \
  --from-literal=access-key="$(openssl rand -hex 10)" \
  --from-literal=secret-key="$(openssl rand -hex 20)"
kubectl apply -k deploy/kubernetes -n storage
kubectl -n storage port-forward svc/gos3 9000:9000
```

The kustomization does **not** include `secret.example.yaml`, so its
placeholder keys are never deployed. The Secret is required: until it
exists, the pods wait in `CreateContainerConfigError`. (Generated keys would
only be stored on the PVC, and the image has no shell to read them with.)
Changing the Secret takes effect when the pods restart.

The image is pinned to a release with `newTag` in `kustomization.yaml`
(and in `statefulset.yaml`); raise it when you upgrade. Do not use `latest`,
which Kubernetes pulls again on every pod start, so whatever was pushed last
runs. For an immutable reference, verify the release
([releasing.md](releasing.md#verifying-a-release)) and add its `digest:`
next to `newTag`.

How it fits together:

- **StatefulSet `gos3`**: each pod gets its own PVC (`data`, 20 Gi) mounted at
  `/data`, and runs as uid 65532 with a read-only root file system, all
  capabilities dropped, the RuntimeDefault seccomp profile and no service
  account token (gos3 does not use the Kubernetes API). Probes:
  startup and liveness on `/_gos3/health`, readiness on `/_gos3/ready`. The pod
  name is the server name.
- **Headless Service `gos3-headless`** has its port named **`gos3`**, so
  Kubernetes DNS publishes the SRV records
  `_gos3._tcp.gos3-headless.<namespace>.svc.cluster.local`. Each pod sets
  `GOS3_DISCOVERY_DNS_SRV=gos3-headless.<namespace>.svc.cluster.local`, so
  replicas find each other through DNS SRV (multicast is disabled in the
  ConfigMap because pod networks rarely carry it). Each pod advertises
  `GOS3_PUBLIC_URL=http://<pod>.gos3-headless.<namespace>.svc.cluster.local:9000`.
- **Service `gos3`** is for clients and selects **only `gos3-0`**. Replicas
  do not share data, so load-balancing across them would scatter buckets. For
  more servers, raise `replicas` and add one Service (or Ingress backend) per
  pod, selecting `statefulset.kubernetes.io/pod-name: gos3-N`.
- Runtime changes made in the UI are stored on the PVC
  (`/data/.gos3/settings.json`) and override the ConfigMap from then on (see
  [configuration.md](configuration.md#runtime-settings-settingsjson)).
- **NetworkPolicy** (`networkpolicy.yaml`, not applied by default): only
  the Ingress controller's namespace, a `monitoring` namespace and the gos3
  pods themselves may connect to port 9000. Without a policy, every pod in
  the cluster can reach the S3 and admin APIs. Adjust the namespace labels,
  then uncomment it in `kustomization.yaml`; it needs a CNI that enforces
  NetworkPolicy.
- Metrics: Prometheus cannot sign requests. Either set `metrics.public: true`
  and restrict `/_gos3/metrics` with a NetworkPolicy, or scrape through a
  proxy that adds authentication.
- An Ingress in front of the Service must allow large bodies and must not
  buffer them (see the nginx settings below). The host it serves must be the
  host clients sign; set `trust_proxy_headers: true` in the ConfigMap, and
  `trusted_proxies` to the Ingress controller pods' addresses so requests
  that bypass the Ingress cannot forge their address. Listing the whole pod
  CIDR is simpler but lets every pod in it choose its own `aws:SourceIp`;
  do that only when a NetworkPolicy allows nothing but the Ingress
  controller to reach gos3, or when no policy relies on in-cluster source
  addresses.
- **Host names.** gos3 knows its pod's headless DNS name
  (`GOS3_PUBLIC_URL`), IP addresses and `localhost`; for any other `Host`
  the web UI and anonymous reads answer `421` / `403`. List the Ingress host
  in `server.allowed_hosts` (ConfigMap) or `GOS3_ALLOWED_HOSTS`
  (StatefulSet; both have a commented example). A non-empty list refuses
  every other name, signed requests included, so also list the Service
  names in-cluster clients use (`gos3`, `gos3.<namespace>.svc.cluster.local`).
  See [configuration.md](configuration.md) (`server.allowed_hosts`).
- A standalone web UI deployed next to gos3 keeps its local discovery
  routes off unless `GOS3_UI_LOCAL_DISCOVERY=true`; leave them off when the
  UI is reachable through an Ingress, since they have no authentication.
  If you do enable them, a UI reached through an Ingress host name also
  needs `GOS3_UI_ALLOWED_HOSTS=<that host>` (see
  [web-ui.md](web-ui.md#local-discovery-routes)).

## Reverse proxy

gos3 can sit behind nginx, Caddy, Traefik or a cloud load balancer. The rules:

1. **Keep the `Host` header and the request URI unchanged.** SigV4 signs the
   host and the exact path. gos3 verifies against the `Host` it receives and
   ignores `X-Forwarded-Host`. Serve gos3 at the root of a (sub)domain; a path
   prefix such as `https://example.com/s3/` does not work.
2. **No body size limit, no request buffering, long timeouts**, for multipart
   parts (up to 5 GiB) and large downloads. Slow CopyObject, UploadPartCopy and
   CompleteMultipartUpload calls are kept alive by gos3 with whitespace every
   5 s. gos3 itself fails a request body that sends no data for 60 s
   (`RequestTimeout`); this limit is fixed. Bodies read before the caller is
   authorized (DeleteObjects, up to about 6.2 MiB, and the small
   PutObjectTagging / PutObjectRetention / PutObjectLegalHold bodies) must
   arrive completely within 60 s, so a maximal DeleteObjects needs at least
   about 110 KB/s end to end (see [security.md](security.md#timeouts)).
3. Set `server.trust_proxy_headers: true` and list the proxies in
   `server.trusted_proxies`. Then `aws:SourceIp`, `aws:SecureTransport` and
   the access logs see the real client:
   - Forwarding headers are honoured only from a peer in `trusted_proxies`
     (required: an empty list is a configuration error). Requests that
     reach gos3 directly are taken at face value. The proxy must set (not
     append to) `X-Forwarded-Proto`.
   - Set `server.public_url` to the public URL, or list the host name in
     `server.allowed_hosts`, when the proxy passes the client's `Host`
     through: anonymous requests and the web UI are refused for host names
     gos3 does not know (see
     [security.md](security.md#dns-rebinding-and-the-host-header)).
   - The client is the **rightmost** `X-Forwarded-For` entry that is not in
     `trusted_proxies`. Each proxy appends the address it received the
     request from, so the entries to the right of that one were written by
     your proxies and anything further left may have been forged by the
     client. With a chain (CDN, load balancer, Ingress) list every hop.
     List only proxies: a client whose own address is in the list is
     skipped like a proxy, so it could choose its `aws:SourceIp`.
   - With an empty `trusted_proxies` the rightmost entry is the client. That
     is right for a single proxy that appends to or overwrites the header,
     as long as clients cannot reach gos3 without going through it.
   - `aws:SecureTransport` follows the **last** `X-Forwarded-Proto` value, so
     the proxy must set (overwrite) or append it.
4. Set `server.public_url` to the public URL (for example
   `https://s3.example.com`), so `/_gos3/info`, discovery and the UI advertise
   it instead of the internal address.

nginx:

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name s3.example.com *.s3.example.com;   # wildcard only for virtual-host style
    ssl_certificate     /etc/ssl/s3.example.com/fullchain.pem;
    ssl_certificate_key /etc/ssl/s3.example.com/privkey.pem;

    client_max_body_size 0;          # no limit
    proxy_request_buffering off;     # stream uploads
    proxy_buffering off;             # stream downloads
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;

    location / {
        proxy_pass http://127.0.0.1:9000;           # no URI part: the request URI is passed unchanged
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;            # exactly what the client signed (incl. a non-default port)
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;   # appending is fine: gos3 reads it from the right
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";
    }
}
```

Caddy:

```caddyfile
s3.example.com, *.s3.example.com {
    reverse_proxy 127.0.0.1:9000 {
        flush_interval -1
    }
}
```

Caddy passes `Host` through unchanged. Unless you configure trusted proxies,
it replaces any `X-Forwarded-For` / `X-Forwarded-Proto` sent by the client.
A wildcard certificate needs the DNS challenge.

With either proxy on the same host as gos3, the gos3 side is:

```yaml
server:
  trust_proxy_headers: true
  trusted_proxies: ["127.0.0.1", "::1"]
```

Requests from any other address (clients that reach gos3's port without the
proxy) then keep their own address whatever headers they send.

## Virtual-hosted-style domains

Path-style (`https://s3.example.com/bucket/key`) always works and is what most
self-hosted setups use. For virtual-hosted style
(`https://bucket.s3.example.com/key`):

1. Add the domain: `server.domains: ["s3.example.com"]` (`--domain`,
   `GOS3_DOMAINS`, or at runtime under Settings).
2. Point wildcard DNS `*.s3.example.com` (and `s3.example.com`) at the server
   or proxy.
3. Use a certificate that covers both `s3.example.com` and `*.s3.example.com`.
   Bucket names containing dots do not match a wildcard certificate; use
   path-style for those.

Requests to the bare domain stay path-style, and `/_gos3/*` on the bare domain
reaches the admin API and UI. This holds for every configured domain, even
one that is a subdomain of another (with `example.com` and `s3.example.com`
both configured, `s3.example.com` is path-style, not bucket `s3`). On a bucket
host, every path is an object key.

## TLS

gos3 can terminate TLS itself:

```sh
gos3 serve --tls-cert /etc/gos3/fullchain.pem --tls-key /etc/gos3/privkey.pem
# or: GOS3_TLS_CERT=/etc/gos3/fullchain.pem GOS3_TLS_KEY=/etc/gos3/privkey.pem gos3 serve
```

- TLS 1.2 or later, with HTTP/2. The certificate is loaded at startup, so
  restart gos3 after renewing it (or terminate TLS at a reverse proxy that
  reloads certificates).
- `gos3 healthcheck` follows the configuration (file and `GOS3_*`
  environment, not the flags of the running `serve`) and uses `https`
  (without certificate verification against its own loopback address). In
  containers, configure TLS through the environment so the image's health
  check sees it.
- Discovery probes of other servers verify their certificates. A peer with a
  self-signed certificate shows up as unverified, with a TLS error.
- For self-signed certificates, clients need the CA (for example
  `AWS_CA_BUNDLE` for the AWS CLI and SDKs, `--ca-bundle`, `mc --insecure` for
  tests only).
