# Security

This page describes what gos3 protects and how, what it stores on disk, and
the settings to review before you expose a server.

## Checklist

- [ ] Set your own root keys (`GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY`),
      or note and protect the generated ones. Use IAM users for applications,
      not root.
- [ ] Serve HTTPS: either `server.tls_cert` / `tls_key`, or a TLS-terminating
      reverse proxy (see [deployment.md](deployment.md#reverse-proxy)).
- [ ] Restrict `cors.allowed_origins` to the web UI origins you use.
- [ ] Decide on discovery: `discovery.advertise`, `discovery.allow_scan`, and
      `server.public_info`.
- [ ] Keep the data directory private (mode 0700) and back it up; it contains
      secrets.
- [ ] If metrics are public (`metrics.public`), restrict who can reach
      `/_gos3/metrics`.
- [ ] Do not expose a standalone web UI's Node server to untrusted networks,
      or set `GOS3_UI_LOCAL_DISCOVERY=false`.

## Authentication

- Every S3 and admin API request is authenticated with **AWS Signature
  Version 4**: an `Authorization` header, a presigned URL (at most 7 days) or a
  signed POST policy. Signatures are compared in constant time, and the allowed
  clock skew is 15 minutes. SigV2 is rejected, and so are session tokens
  (there is no STS).
- Requests without a signature are anonymous. They are allowed only by a bucket
  policy with `"Principal": "*"` or by a public canned ACL, and the admin API
  never allows them.
- The web UI signs requests in the browser. There are no sessions, cookies or
  passwords. A secret key the user saves with "remember" is kept in the
  browser's `localStorage`, so use that only on trusted devices. Any script
  running on the UI's origin could read it, which is why object responses
  are sandboxed (see [Object responses](#object-responses)).

### Root credentials

The root identity is always allowed, even where a bucket policy denies access.
Use it to administer the server, and give applications IAM users with narrow
policies.

| Configuration | Behaviour |
|---|---|
| `GOS3_ROOT_ACCESS_KEY` + `GOS3_ROOT_SECRET_KEY` (or `auth.root_*` in YAML) | Used as given. The secret is never logged. |
| Not set, with a data directory | Generated on first start with a random 20-character access key and 40-character secret. They are logged **once** as a warning and shown in the startup banner, then saved to `<data>/.gos3/root-credentials.json` (mode 0600) and reused on later starts. Only the access key is logged after that. |
| Not set, memory backend without a data path | New keys on every start, logged each time. |

To rotate the root keys, set new ones in the environment and restart. If you
want to switch from generated keys to configured ones, delete
`root-credentials.json` once you no longer need it. `gos3 config print`
redacts the root secret.

### IAM users and keys

IAM users, their access keys (including secret keys) and custom policies are
stored in `<data>/.gos3/iam.json` (mode 0600). A user can have up to 10 keys,
and keys can be deactivated. Users can list, create and delete their own keys
through `/_gos3/admin/v1/whoami/keys`.

Users may create and delete their own keys without any grant, unless a
policy explicitly denies `admin:CreateOwnAccessKey` /
`admin:DeleteOwnAccessKey` (a Deny of `admin:*` also matches). Deny them to
stop a user, or a stolen key, from minting keys that would survive revoking
the current one.

Built-in policies: `admin` (everything, including the admin API), `readwrite`
(all S3 actions), `readonly`, `writeonly`, and `diagnostics` (server info,
stats and discovery in the admin API only). Custom policies use the IAM policy
language; the admin API's `/authz/simulate` endpoint and the UI's policy
simulator show which statement decides a request.

Setting an ACL or tags while writing an object needs more than
`s3:PutObject`, as on AWS: PutObject, CopyObject (with the `REPLACE` tagging
directive), CreateMultipartUpload and PostObject also require
`s3:PutObjectAcl` when an ACL is given (`x-amz-acl` or the `acl` form field)
and `s3:PutObjectTagging` when tags are given (`x-amz-tagging` or the
`tagging` form field). A principal limited to `s3:PutObject` (such as the
built-in `writeonly` policy) can therefore neither publish an object through
an ACL nor plant tags that `s3:ExistingObjectTag` conditions rely on. The
`s3:RequestObjectTag/<key>` and `s3:RequestObjectTagKeys` condition keys are
evaluated for these uploads, for PutObjectTagging (from the XML body) and for
PostObject (from the `tagging` field).

## Data at rest

The data directory (default `./data`, `/data` in the container) holds:

| Path | Content | Sensitivity |
|---|---|---|
| `.gos3/root-credentials.json` | generated root keys | **secret** (0600) |
| `.gos3/iam.json` | IAM users, access **and secret** keys, policies | **secret** (0600) |
| `.gos3/settings.json` | runtime settings | private (0600) |
| `.gos3/meta.db` | bucket and object metadata, bucket policies and configurations (fs backend) | private (0600) |
| `.gos3/data/`, `.gos3/uploads/` | object and part data | your data (files 0600) |
| `.gos3/server-id` | random server ID | public |

Keep the directory owned by the gos3 user with mode 0700, and treat backups as
secret too. **gos3 does not encrypt objects.** Bucket default encryption and
`x-amz-server-side-encryption` headers are accepted and reported for client
compatibility only. Use full-disk or file system encryption if you need
encryption at rest. SSE-C is rejected (`NotImplemented`).

Only one gos3 process can use a data directory at a time: every backend,
including the memory backend with a data path, takes an exclusive lock on
`<data>/.gos3/lock`, and a second process fails at startup with "is another
gos3 using this data directory?". On platforms without `flock` or Windows
file sharing the lock is a no-op.

The fs backend never deletes object or part files that its metadata does not
reference at startup; it moves them to `.gos3/orphans/` (deleted after 7
days), so treat that directory like the rest of the data. It also refuses to
start when `meta.db` is missing or empty while object files exist, instead
of discarding them. See [operations.md](operations.md#the-data-directory).

## Transport

- gos3 serves plain HTTP unless TLS is configured. Signatures protect requests
  from tampering, but without TLS the data and the responses (including secret
  keys returned by the admin API when keys are created) travel in clear text.
- With `server.tls_cert` / `tls_key`, gos3 accepts TLS 1.2 or later and serves
  HTTP/2.
- Behind a proxy, enable `server.trust_proxy_headers` only when the proxy
  overwrites `X-Forwarded-For` and `X-Forwarded-Proto`. Otherwise clients can
  fake `aws:SourceIp` and `aws:SecureTransport` conditions. The setting
  applies to the S3 API and the admin API (conditions and access log alike).

### Timeouts

These limits are fixed (not configurable):

- A request body that sends no data for 60 s fails: S3 requests with `400
  RequestTimeout`, requests to `/_gos3/*` endpoints (admin API, UI, info)
  with an error.
- On HTTP/1.1, when gos3 answers before reading the request body (typically
  an error), it drains at most 256 KiB of the unread body for at most 10 s
  (S3 API); otherwise, and always for `/_gos3/*` endpoints, the response
  carries `Connection: close` and the connection is closed. A client that
  announces a body and never sends it cannot hold a connection open.
- PostObject form fields before the file must arrive within 60 s.
- Slow CopyObject, UploadPartCopy and CompleteMultipartUpload calls keep the
  connection alive with whitespace (see
  [s3-compatibility.md](s3-compatibility.md#other-differences-from-aws)).

## Object responses

Objects are served from the same origin (scheme, host and port) as the
embedded web UI. An uploaded `text/html` or `image/svg+xml` object opened in a
browser could otherwise run script with the UI's origin and read the
connections and remembered secret keys it stores. Therefore every GetObject
and HeadObject response carries `X-Content-Type-Options: nosniff` and, except
for `application/pdf`, `Content-Security-Policy: sandbox`. HTML and SVG
objects opened in a browser render without scripts, forms or same-origin
access, so HTML applications cannot be hosted in a bucket and opened directly
(images, media and downloads are unaffected).

## CORS

The default global CORS policy allows **any origin** (`*`). That is
reasonably safe, because browsers send no credentials automatically: a page can
only make signed requests if it already has a secret key. Still, in production,
list the origins of the web UIs you use (`cors.allowed_origins`, `--cors-origin`,
or **Settings → CORS**). Bucket CORS configurations take precedence for their
bucket. `/_gos3/info` always answers with `Access-Control-Allow-Origin: *`.

**Known issue:** with the `*` default, objects that are anonymous only for
some networks (a bucket policy with an `aws:SourceIp` condition, e.g. an
intranet allow-list) can be read by scripts on *any* website that a user on
those networks visits, because the browser makes the request from the user's
address and the response is shared with every origin. Until this is fixed in
the S3 CORS handling, restrict `cors.allowed_origins` (or set a bucket CORS
configuration) on servers that use IP-based anonymous access.

## Public access evaluation

Block Public Access (`BlockPublicPolicy`, `RestrictPublicBuckets`),
`GetBucketPolicyStatus` and the admin API's `public` flags treat a bucket
policy statement that allows anonymous access as **non-public** only when a
condition restricts it to a bounded set of sources. An `aws:SourceIp`
condition counts as restricting only when its ranges together cover at most
four IPv4 /8 networks and four IPv6 /32 networks (overlapping ranges are
counted twice; `::ffff:a.b.c.d/96+n` counts as the IPv4 /n). Wider
allow-lists, such as `fd00::/8` or many large ranges, count as public.

## Public endpoints

| Endpoint | Default access | Reveals |
|---|---|---|
| `/_gos3/health`, `/_gos3/ready` | anonymous | `{"status":"ok"}`, and for `/ready` a storage error message |
| `/_gos3/info` | anonymous (`server.public_info: true`) | server ID, name, version, region, endpoint URLs, features; no secrets |
| `/_gos3/metrics` | `admin:GetStats` (`metrics.public: false`) | request counts and latencies per operation, bucket count |
| `/_gos3/ui/` | anonymous (static files) | the UI code; no data until you sign in |

Set `server.public_info: false` to require a signature for `/_gos3/info`. This
also hides the server from unauthenticated discovery: other servers and UIs
then cannot verify it.

## Discovery

- mDNS and beacon announcements contain the server ID, name, version, region
  and URLs. Turn them off with `discovery.advertise: false`, or turn off
  discovery with `discovery.enabled: false` (`--no-discovery`).
  `server.public_info: false` does not stop these announcements: also set
  `discovery.advertise: false` to hide them.
- Beacon datagrams are accepted only from this host or a directly connected
  subnet, are rate-limited per sender, and can only make a server probe the
  sender's own IP (announced host names are never resolved). mDNS answers are
  only probed at on-link addresses. See
  [discovery.md](discovery.md#3-udp-multicast-beacon).
- Announcements are unauthenticated hints. A candidate becomes a peer only after
  its `/_gos3/info` was fetched, and adding a server to the UI still needs
  credentials. A malicious host on the network can still make itself appear in
  peer lists.
- **Scans** (`POST /_gos3/admin/v1/discovery/scan`) need `admin:ScanNetwork`
  and `discovery.allow_scan: true` (the default). They are limited to
  `max_scan_hosts` hosts (default 1024) and 5 minutes, and at most 2 scans
  run at once per server (`429 TooManyRequests`). Set `allow_scan: false`
  where port scanning is unwelcome.
- **Probes** (`POST /_gos3/admin/v1/discovery/probe`) need only
  `admin:GetDiscovery`, which the `diagnostics` policy grants. They make the
  server fetch `<url>/_gos3/info` from any HTTP(S) host it can reach, which is
  a limited server-side request vector. The response must be a valid gos3
  `Info` document (at most 64 KiB), and probes never follow redirects or use
  proxies. Grant `diagnostics` only to trusted users.
- The standalone web UI's Node server performs local discovery (mDNS, beacon,
  scans, probes) for **anyone who can reach it**, without authentication.
  Restrict access to it, or set `GOS3_UI_LOCAL_DISCOVERY=false`. Against DNS
  rebinding and other websites, its `/api/discovery/*` routes answer 403
  unless the `Host` is an IP address, `localhost`/`*.localhost` or a name in
  `GOS3_UI_ALLOWED_HOSTS`, and refuse a foreign `Origin` or `Sec-Fetch-Site:
  cross-site`/`same-site`. Its probes read at most 64 KiB of `/_gos3/info`
  (with or without `Content-Length`); local discovery keeps at most 2048
  mDNS records and 1024 beacon candidates and verifies at most 256 URLs.

## Admin API notes

- `DELETE /_gos3/admin/v1/buckets/{name}?force=true` deletes every version,
  including versions under object lock retention or legal hold. It needs
  `admin:ForceDeleteBucket`, which only `admin` grants by default.
- `PUT /_gos3/admin/v1/config` (`admin:UpdateConfig`) can change CORS,
  discovery and the public URL. Treat it like root access.
- Admin requests are recorded in metrics (op `Admin`) and in the access log.
  The log contains paths only, never query strings, so presigned URL signatures
  are not logged.

## Reporting vulnerabilities

Please report security issues privately to the maintainers, for example via
GitHub's private vulnerability reporting on the repository, and not in public
issues.
