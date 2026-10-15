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
# stored in <data>/.gos3/settings.json and win over this file when admin is enabled.

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
  # Addresses (CIDRs or IPs) of the reverse proxies in front of gos3;
  # required with trust_proxy_headers. Requests from other addresses are
  # taken at face value. List only the proxies, never subnets that also hold
  # clients: a listed client could choose its own aws:SourceIp.
  # trusted_proxies: ["10.0.0.5", "192.0.2.10"]
  trusted_proxies: []
  # Extra host names clients use to reach this server (Host header, no
  # port). IP addresses, localhost, the OS host name (and <name>.local) and
  # the hosts of address, public_url and domains are always known. Empty:
  # requests for other host names are served only when signed (no anonymous
  # access, web UI or public metrics: protects against DNS rebinding).
  # Non-empty: requests for other host names are refused. "*.example.com"
  # allows subdomains; ["*"] turns the check off.
  allowed_hosts: []
  # Maximum simultaneously open client connections (0 = no limit). At the
  # limit a new connection closes the longest-idle keep-alive connection, or
  # is refused when none is idle.
  max_connections: 0

storage:
  # fs (files under path), memory (lost on exit) or s3 (a bucket of a
  # remote S3 service, configured by the s3 block below).
  backend: fs
  # Data directory. System state lives in <path>/.gos3 (iam.json with secret
  # keys, settings.json, server-id, root-credentials.json): back it up and
  # keep it private. With the memory and s3 backends it holds system state
  # only: leave it empty for ephemeral system state.
  path: ./data
  # With backend: s3, the remote service holding all data and metadata
  # (one gos3 server per bucket and prefix). Credentials and the
  # encryption key can come from GOS3_S3_ACCESS_KEY, GOS3_S3_SECRET_KEY and
  # GOS3_S3_ENCRYPTION_KEY instead.
  # s3:
  #   endpoint: https://s3.eu-central-1.amazonaws.com
  #   region: eu-central-1
  #   bucket: acme-gos3
  #   prefix: gos3/
  #   # base64 of 32 random bytes (openssl rand -base64 32): enables AES256
  #   # and restic encryption. Back it up: without it encrypted data is lost.
  #   encryption_key: ""
  # fsync object data before acknowledging writes (durable across power loss).
  fsync_data: true
  # Additional backings (this one is named "default"): fs directories,
  # memory, or buckets of remote S3 services (backend: s3, no path; with
  # gateway: true objects are stored natively in remote buckets named like
  # the gos3 buckets, without versioning or object lock). New buckets go to
  # default_backing unless CreateBucket names another with the
  # x-gos3-backing header. See docs/configuration.md and docs/backings.md.
  # backings:
  #   - name: hdd
  #     path: /mnt/hdd/gos3
  #   - name: cold
  #     backend: s3
  #     s3:
  #       endpoint: https://s3.eu-central-1.amazonaws.com
  #       region: eu-central-1
  #       bucket: acme-gos3-cold
  #       prefix: gos3/
  #       # access_key / secret_key / encryption_key, or GOS3_BACKING_COLD_ACCESS_KEY,
  #       # GOS3_BACKING_COLD_SECRET_KEY and GOS3_BACKING_COLD_ENCRYPTION_KEY
  backings: []
  default_backing: ""

auth:
  # Root credentials. Prefer the environment (GOS3_ROOT_ACCESS_KEY /
  # GOS3_ROOT_SECRET_KEY). When unset, a pair is generated on first start
  # and stored in <path>/.gos3/root-credentials.json (the secret is never
  # logged; the startup banner shows it on a terminal).
  # root_access_key: ""
  # root_secret_key: ""
  # Reject signatures whose credential scope names another region.
  strict_region: false
  # Allow a published example root secret key (README / docker-compose
  # demo keys). Anyone can sign requests with those, including websites
  # opened in a browser on this machine: throwaway local demos only.
  insecure_demo_credentials: false

admin:
  # Enable the management API and saved UI settings. False also disables UI.
  enabled: true

ui:
  # Serve the embedded web UI at /_gos3/ui/.
  # Defaults to true, or false in builds with the noui tag.
  # enabled: true
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
  enabled: false
  # Announce this server (mDNS + beacon).
  advertise: false
  mdns: false
  beacon: false
  beacon_group: 239.255.73.83:7373
  beacon_interval: 10s
  # Base URLs of known servers, e.g. ["http://nas:9000"].
  seeds: []
  # Domains whose _gos3._tcp SRV records list servers.
  dns_srv: []
  peer_ttl: 60s
  # Allow network scans through the admin API (admin:ScanNetwork).
  allow_scan: false
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

compression:
  # Transparent compression of object data (runtime). optional: each
  # bucket decides (admin API / web UI, off by default); enforced: every
  # bucket compresses new objects; disabled: nothing new is compressed.
  mode: optional
  # DEFLATE level, 1 (fastest) to 9 (smallest).
  level: 1
  # Objects smaller than this (when their size is known) stay uncompressed.
  min_size: 4096
  # Media types stored uncompressed because they are compressed already
  # ("video/*" and "prefix*" wildcards).
  skip_content_types:
    - image/jpeg
    - image/png
    - image/gif
    - image/webp
    - image/avif
    - image/heic
    - image/heif
    - image/jxl
    - video/*
    - audio/*
    - font/woff
    - font/woff2
    - application/zip
    - application/gzip
    - application/x-gzip
    - application/zstd
    - application/x-bzip2
    - application/x-xz
    - application/x-7z-compressed
    - application/x-rar-compressed
    - application/vnd.rar
    - application/x-lz4
    - application/java-archive
    - application/epub+zip
    - application/vnd.openxmlformats-officedocument.*
    - application/vnd.oasis.opendocument.*
`
