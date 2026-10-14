package main

// sampleConfig is printed by "gos3 config init". Every value equals the
// built-in default (TestSampleConfig checks this), so the file is a safe
// starting point: uncomment and edit what you need. Every key can also be
// set with a GOS3_* environment variable ("gos3 config env").
const sampleConfig = `# gos3 configuration file.
#
# Precedence: built-in defaults < this file < GOS3_* environment variables <
# command line flags. "gos3 config env" lists the variables, "gos3 config
# print" shows the effective configuration. Settings marked (runtime) can also
# be changed while running through the admin API / web UI; those changes are
# stored in <data>/.gos3/settings.json and win over this file.

server:
  # Listen address, [host]:port.
  address: ":9000"
  # Server name shown in the UI and discovery (runtime). Default: host name.
  # name: my-gos3
  # Region reported to clients (runtime).
  region: us-east-1
  # Externally reachable base URL, advertised in /_gos3/info and discovery
  # (runtime). Default: derived from the listener and the host's addresses.
  public_url: ""
  # Virtual-hosted-style addressing: Host "<bucket>.<domain>" (runtime).
  domains: []
  # HTTPS: PEM certificate and key (both or neither).
  tls_cert: ""
  tls_key: ""
  # Single PUT size limit in bytes (5 GiB; multipart uploads are not limited).
  max_object_size: 5368709120
  # Serve GET /_gos3/info without authentication (runtime).
  public_info: true
  # Time allowed for in-flight requests on shutdown.
  shutdown_timeout: 30s
  # Trust X-Forwarded-For / X-Forwarded-Proto (only behind a reverse proxy).
  # The client is the rightmost X-Forwarded-For entry that is not one of
  # trusted_proxies; the last X-Forwarded-Proto value decides aws:SecureTransport.
  trust_proxy_headers: false
  # Addresses (CIDRs or IPs) of the reverse proxies in front of gos3. With
  # trust_proxy_headers, requests from other addresses are taken at face
  # value. Empty: any peer is trusted and the rightmost entry is the client
  # (fine for a single proxy that gos3 alone can be reached through). List
  # only the proxies, never subnets that also hold clients: a listed client
  # could choose its own aws:SourceIp.
  # trusted_proxies: ["10.0.0.5", "192.0.2.10"]
  trusted_proxies: []

storage:
  # fs (files under path) or memory (lost on exit).
  backend: fs
  # Data directory. System state lives in <path>/.gos3 (iam.json with secret
  # keys, settings.json, server-id, root-credentials.json): back it up and
  # keep it private. With the memory backend leave it empty for a fully
  # ephemeral server.
  path: ./data
  # fsync object data before acknowledging writes (durable across power loss).
  fsync_data: true
  # Remote S3 services individual buckets can be stored on, chosen per
  # bucket at creation (x-gos3-backing header or the web UI). mode: blob
  # (opaque blobs, metadata stays here, every feature) or gateway (native
  # remote objects, no versioning/object lock). See docs/backings.md.
  # backings:
  #   - name: cold
  #     mode: blob
  #     endpoint: https://s3.eu-central-1.amazonaws.com
  #     region: eu-central-1
  #     bucket: acme-gos3-blobs
  #     prefix: prod/
  #     # credentials: access_key / secret_key, or GOS3_BACKING_COLD_ACCESS_KEY
  #     # and GOS3_BACKING_COLD_SECRET_KEY
  backings: []
  # Backing of buckets created without a choice: local or a backing name.
  default_backing: ""

auth:
  # Root credentials. Prefer the environment (GOS3_ROOT_ACCESS_KEY /
  # GOS3_ROOT_SECRET_KEY). When unset, a pair is generated on first start,
  # logged once and stored in <path>/.gos3/root-credentials.json.
  # root_access_key: ""
  # root_secret_key: ""
  # Reject signatures whose credential scope names another region.
  strict_region: false

ui:
  # Serve the embedded web UI at /_gos3/ui/.
  enabled: true
  # Redirect browsers opening / to the UI (runtime: uiRedirect).
  redirect_root: true

cors:
  # Global CORS policy for the S3 API (buckets without their own CORS
  # configuration) and the admin API (runtime). A standalone web UI needs its
  # origin allowed here, e.g. ["http://localhost:3000"].
  allowed_origins: ["*"]
  allowed_headers: ["*"]
  # Default: "*" plus ETag, Content-*, x-amz-version-id, x-amz-checksum-* and
  # the other S3 response headers browsers need to read.
  # expose_headers: ["*", "ETag", "x-amz-version-id"]
  max_age_seconds: 3600

discovery:
  # Find other gos3 servers and let them (and the UI) find this one
  # (runtime). See docs/discovery.md.
  enabled: true
  # Announce this server (mDNS + beacon).
  advertise: true
  mdns: true
  beacon: true
  beacon_group: 239.255.73.83:7373
  beacon_interval: 10s
  # Base URLs of known servers, e.g. ["http://nas:9000"].
  seeds: []
  # Domains whose _gos3._tcp SRV records list servers.
  dns_srv: []
  peer_ttl: 60s
  # Allow network scans through the admin API (admin:ScanNetwork).
  allow_scan: true
  max_scan_hosts: 1024

logging:
  # debug, info, warn or error (runtime).
  level: info
  # text or json (runtime).
  format: text
  # Log every request at info level (otherwise debug) (runtime).
  access_log: true

lifecycle:
  # Apply bucket lifecycle rules (expiration, multipart abort) (runtime).
  enabled: true
  interval: 1h

metrics:
  # Serve Prometheus metrics at /_gos3/metrics.
  enabled: true
  # Without authentication (otherwise admin:GetStats is required) (runtime).
  public: false
`
