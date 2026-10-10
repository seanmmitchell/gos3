# Deployment

gos3 is one static binary with the web UI built in. It needs a data directory
and one TCP port (9000 by default). Discovery also uses UDP multicast
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

Release archives for Linux, macOS and Windows (amd64 and arm64) contain the
binary with the UI embedded, plus `checksums.txt`. To build from source you
need Go ≥ 1.25, Node.js 22 and pnpm (via corepack):

```sh
make build                    # web UI + bin/gos3 with the UI embedded
bin/gos3 serve --data /srv/gos3
```

`make build-go` (or `go build ./cmd/gos3`) builds without rebuilding the UI;
the binary then serves a placeholder at `/_gos3/ui/`.

On first start without configured root keys, gos3 prints a generated access
key and secret key in the banner and stores them in
`<data>/.gos3/root-credentials.json`. See [configuration.md](configuration.md)
for all settings and [security.md](security.md) before exposing the server.

## Docker

The image (`Dockerfile`) is built in three stages: the web UI with pnpm, a
static Go binary with the UI embedded (cross-compiled for the target
architecture), and a `gcr.io/distroless/static-debian12:nonroot` runtime. That
runtime has no shell, runs as uid/gid 65532, and is about 25 MB.

```sh
docker build -t gos3 .              # or: make docker (adds version build args)
docker run -d --name gos3 -p 9000:9000 -v gos3-data:/data \
  -e GOS3_ROOT_ACCESS_KEY=admin -e GOS3_ROOT_SECRET_KEY=change-me-now \
  gos3
docker logs gos3                    # banner: endpoints, UI URL, credentials source
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
- Without `GOS3_ROOT_*`, the generated keys are in `docker logs` (once) and in
  the volume at `/data/.gos3/root-credentials.json`.
- **Discovery across hosts** needs multicast, which Docker's default bridge
  network keeps local to the host. Use `--network host`, or seeds
  (`GOS3_DISCOVERY_SEEDS`) and DNS SRV. Set `GOS3_PUBLIC_URL` to the URL other
  machines should use, because the container only knows its internal address.
- The image is built for `linux/amd64` and `linux/arm64`:
  `docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/gos3 --push .`.
- Build arguments: `VERSION`, `COMMIT` and `DATE` (shown by `gos3 version`),
  `GO_VERSION` (default 1.26) and `NODE_VERSION` (default 22).

The standalone web UI has its own image, `deploy/docker/web.Dockerfile`
(`make docker-ui`); see [web-ui.md](web-ui.md#standalone-mode).

## Docker Compose

`docker-compose.yml` is a two-node demo:

```sh
docker compose up -d --build                  # gos3-1 on :9001, gos3-2 on :9002
docker compose --profile ui up -d --build     # + standalone UI on :3000 (or: make demo)
docker compose down -v                        # stop and delete the data volumes
```

- Both nodes use the root keys `gos3admin` / `gos3admin-secret`. Override them
  with `GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY` in your shell or an
  `.env` file.
- Because those default keys are public, ports 9001, 9002 and 3000 are
  published on **127.0.0.1 only**. To reach the demo from other machines, set
  your own root keys and `GOS3_DEMO_BIND=0.0.0.0` (or one host address).
  Docker's published ports usually bypass the host firewall.
- The nodes find each other through the **UDP beacon** on the shared compose
  network, and each lists the other as a **seed** as a fallback. Open
  `http://localhost:9001/_gos3/ui/`, sign in and go to **Discovery**: the other
  node appears with sources `beacon` and `static`.
- Peers are listed with their container addresses (for example
  `http://172.18.0.3:9000`), which a browser on the host may not reach (Docker
  Desktop). Add the second node through its published port,
  `http://localhost:9002`.
- The standalone UI is preset with both nodes (`NUXT_PUBLIC_SERVERS`), and its
  local discovery finds them through the beacon inside the network. Local
  discovery answers only when the UI is opened via an IP address or
  `localhost`; if you open it through a DNS name, set
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
  `GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY` at mode 0600.
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
Secret (root keys), a headless Service, a client Service and a StatefulSet,
tied together by `kustomization.yaml`:

```sh
kubectl create namespace storage
kubectl -n storage create secret generic gos3-root-credentials \
  --from-literal=access-key="$(openssl rand -hex 10)" \
  --from-literal=secret-key="$(openssl rand -hex 20)"
kubectl apply -k deploy/kubernetes -n storage
kubectl -n storage port-forward svc/gos3 9000:9000
```

The kustomization does **not** include `secret.example.yaml`, so its
placeholder keys are never deployed. Without the Secret (its references are
optional), each server generates its own root keys on first start and logs
them once (`kubectl -n storage logs gos3-0`); creating the Secret later takes
effect when the pods restart. The image tag is `latest`, which Kubernetes
pulls on every pod start; pin a release with `newTag` in
`kustomization.yaml` for reproducible deployments.

How it fits together:

- **StatefulSet `gos3`**: each pod gets its own PVC (`data`, 20 Gi) mounted at
  `/data`, and runs as uid 65532 with a read-only root file system, all
  capabilities dropped and the RuntimeDefault seccomp profile. Probes:
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
- Metrics: Prometheus cannot sign requests. Either set `metrics.public: true`
  and restrict `/_gos3/metrics` with a NetworkPolicy, or scrape through a
  proxy that adds authentication.
- An Ingress in front of the Service must allow large bodies and must not
  buffer them (see the nginx settings below). The host it serves must be the
  host clients sign; set `trust_proxy_headers: true` in the ConfigMap.
- A standalone web UI deployed next to gos3 and reached through an Ingress
  host name needs `GOS3_UI_ALLOWED_HOSTS=<that host>` for its local discovery
  routes (see [web-ui.md](web-ui.md#local-discovery-routes)).

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
   (`RequestTimeout`); this limit is fixed.
3. Set `server.trust_proxy_headers: true` only when the proxy **overwrites**
   `X-Forwarded-For` and `X-Forwarded-Proto`. Then `aws:SourceIp`,
   `aws:SecureTransport` and the access log see the real client. gos3 uses the
   *first* `X-Forwarded-For` entry, so a proxy that appends to a header sent by
   the client would let the client choose its address.
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
        proxy_set_header X-Forwarded-For $remote_addr;
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
reaches the admin API and UI. On a bucket host, every path is an object key.

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
