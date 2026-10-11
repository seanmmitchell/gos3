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

Creating a key for oneself needs an explicit Allow of
`admin:CreateOwnAccessKey`, which only the built-in `admin` policy grants;
give it in a custom policy to users who should manage their own keys. Without
it, a stolen key cannot mint fresh keys that would survive revoking the
stolen one. An explicit Deny still wins over the Allow. Every self-service
key creation is logged at Info with the user, the new access key ID and the
access key that signed the request. Listing and deleting one's own keys
needs no grant (deleting is refused only by an explicit Deny of
`admin:DeleteOwnAccessKey` or `admin:*`). When a key leaks, check the log
for keys it created; disabling the user (`PATCH /users/{name}` with
`"disabled": true`) revokes all of the user's keys at once.

Creating a user through the admin API needs `admin:CreateUser`; creating it
with policies attached also needs `admin:UpdateUser`, and with an initial
access key (`"createKey": true`) also `admin:CreateAccessKey`, the same
actions that gate those steps on their own routes. A role with only
`admin:CreateUser` can therefore create users without policies or keys, but
cannot create a user with the `admin` policy and take its key.

Built-in policies: `admin` (everything, including the admin API), `readwrite`
(all S3 actions), `readonly`, `writeonly`, and `diagnostics` (server info,
stats and discovery state in the admin API only; no probes or scans). Custom
policies use the IAM policy language; the admin API's `/authz/simulate`
endpoint and the UI's policy simulator show which statement decides a
request.

### Scoping admin permissions

Several admin actions are each **equivalent to full admin** when granted on
every resource (`"Resource": "*"` or `arn:gos3:admin:::*`), because they let
the holder give itself any permission:

- `admin:UpdateUser`: attach the `admin` policy to yourself.
- `admin:CreateUser` together with `admin:UpdateUser` (or with
  `admin:CreateAccessKey`): create a user with the `admin` policy and use it.
- `admin:PutPolicy`: rewrite a policy attached to yourself to allow `*`.
- `admin:CreateAccessKey`: create a key (with a secret you choose) for an
  administrator and sign as them.
- `admin:UpdateAccessKey`: reactivate an administrator key that was
  deactivated, for example after it leaked.
- `admin:UpdateConfig`: change CORS, discovery and the public URL; treat it
  like root access.

To delegate part of user or policy management, scope these actions with the
per-target resources (see
[admin-api.md](admin-api.md#resources-and-condition-keys)):
`arn:gos3:admin:::user/<name>` for user and key actions (a key is evaluated
as its owner), `arn:gos3:admin:::policy/<name>` for policy actions and
`arn:gos3:admin:::bucket/<name>` for `admin:GetBucket` and
`admin:ForceDeleteBucket`, with wildcards, and limit which policies may be
attached with the multi-valued condition key `admin:PolicyName`. For
example, this help-desk policy manages only `team-*` users, which cannot
include the help-desk user itself, and may attach only `readonly`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": "admin:ListUsers", "Resource": "arn:gos3:admin:::*" },
    { "Effect": "Allow",
      "Action": ["admin:GetUser", "admin:UpdateUser", "admin:CreateUser",
                 "admin:CreateAccessKey", "admin:UpdateAccessKey", "admin:DeleteAccessKey"],
      "Resource": "arn:gos3:admin:::user/team-*",
      "Condition": { "ForAllValues:StringEquals": { "admin:PolicyName": ["readonly"] } } }
  ]
}
```

Name the scoped users and policies so that the delegated user and the
policies attached to it fall outside the pattern, and do not let it attach
policies that grant admin actions. A Deny with
`"ForAnyValue:StringEquals": {"admin:PolicyName": "admin"}` on
`admin:UpdateUser` and `admin:CreateUser` blocks attaching `admin` whatever
else is allowed. `*` and `arn:gos3:admin:::*` keep matching every admin
resource, and s3 ARNs such as `arn:aws:s3:::*` never grant admin actions. The
UI's capability flags (`GET /whoami`) reflect only unscoped grants.

Setting an ACL or tags while writing an object needs more than
`s3:PutObject`, as on AWS: PutObject, CopyObject (with the `REPLACE` tagging
directive), CreateMultipartUpload and PostObject also require
`s3:PutObjectAcl` when an ACL is given (`x-amz-acl` or the `acl` form field)
and `s3:PutObjectTagging` when tags are given (`x-amz-tagging` or the
`tagging` form field). CopyObject with the `COPY` tagging directive (the
default) from a tagged source copies the tags, so it also needs
`s3:GetObjectTagging` on the source (`s3:GetObjectVersionTagging` when a
`versionId` is given) and `s3:PutObjectTagging` on the destination,
evaluated with the copied tags as `s3:RequestObjectTag/<key>` keys. Copying
an untagged source, or `REPLACE` without tags, needs only `s3:GetObject` and
`s3:PutObject`. A principal limited to `s3:PutObject` (such as the built-in
`writeonly` policy) can therefore neither publish an object through an ACL
nor plant tags that `s3:ExistingObjectTag` conditions rely on. The
`s3:RequestObjectTag/<key>` and `s3:RequestObjectTagKeys` condition keys are
evaluated for these uploads, for PutObjectTagging (from the XML body) and for
PostObject (from the `tagging` field).

Bucket policies see exactly the attribute values that get stored. The values
of `x-amz-acl`, `x-amz-storage-class`, `x-amz-server-side-encryption(-aws-kms-key-id)`,
`x-amz-object-lock-*` and the copy directives are trimmed of spaces and tabs
only; a value padded with other whitespace (for example U+00A0) is rejected
(`400 InvalidArgument` or `InvalidStorageClass`) instead of being normalised
after authorization, where it could slip past a `StringEquals` condition.
SigV4 canonical header values are likewise trimmed of spaces and tabs only,
so other whitespace appended to a signed header breaks the signature. Query
strings are decoded exactly as the signature covers them (`;` and invalid
`%`-escapes stay literal), so a presigned parameter cannot be rewritten into
one the server ignores.

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
secret too. On Windows the modes above do not apply: files inherit the ACLs of
the data directory, so put it somewhere only the gos3 account (and
administrators) can read, not in a shared location such as `C:\ProgramData`
with default permissions. **gos3 does not encrypt objects.** Bucket default encryption and
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

- gos3 serves plain HTTP unless TLS is configured. A signature covers the
  method, path, query and signed headers, and the body only when the client
  signs its SHA-256: S3 requests sent with `UNSIGNED-PAYLOAD` or a presigned
  URL can have their body replaced by anyone on the path, and a captured
  request can be replayed within the 15-minute clock skew (there is no
  replay cache). The admin API therefore refuses unsigned bodies (see
  [admin-api.md](admin-api.md#authentication--authorization)). Without TLS the
  data and the responses (including secret keys returned by the admin API
  when keys are created) also travel in clear text.
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
for PDFs, `Content-Security-Policy: sandbox`. HTML and SVG
objects opened in a browser render without scripts, forms or same-origin
access, so HTML applications cannot be hosted in a bucket and opened directly
(images, media and downloads are unaffected).

Only a `Content-Type` that parses as exactly `application/pdf`, sent as a
single header value without a comma, is exempt from the sandbox; such
responses are served as the bare `application/pdf` (parameters are dropped).
Browsers use the last type of a comma-separated list, so a list such as
`application/pdf, text/html` is always sandboxed.

## CORS

The default global CORS policy allows **any origin** (`*`), but only for
**signed requests**: requests with an `Authorization` header, or presigned
URLs (`X-Amz-Credential` / `X-Amz-Signature` in the query). Browsers send no
credentials of their own to S3, so a page can only make signed requests if it
already has a secret key. Under the global `*` rule:

- Unsigned (anonymous) responses get no `Access-Control-Allow-Origin` (they
  still carry `Vary: Origin`), so the calling script cannot read them.
- Preflights are approved only when `Access-Control-Request-Headers`
  includes `authorization` or the URL is presigned; otherwise the answer is
  `403 AccessForbidden`.
- Browser POST-policy uploads (PostObject) never count as signed, even when
  they come with an `Authorization` header or `X-Amz-*` query parameters:
  PostObject authenticates from its form fields and accepts forms without a
  policy signature anonymously.

Websites therefore cannot use a visitor's browser to read or write
anonymously accessible data, such as buckets restricted by `aws:SourceIp`
or reachable only on the LAN, and see the response. The embedded and
standalone web UIs are unaffected, because they sign every request.

Explicitly listed global origins (`cors.allowed_origins`, `--cors-origin`, or
**Settings → CORS**) and bucket CORS configurations, including a bucket rule
with `*`, still apply to anonymous requests. Bucket CORS configurations take
precedence for their bucket. To serve a public bucket to scripts on other
sites, set a bucket CORS configuration. Browser POST-policy uploads
(PostObject) under the global `*` rule still store the object, but their
response is not readable cross-origin; use a bucket CORS rule for them too.
In production, list the origins of the web UIs you use rather than relying
on `*`. `/_gos3/info` always answers with `Access-Control-Allow-Origin: *`.

## Public access evaluation

Block Public Access (`BlockPublicPolicy`, `RestrictPublicBuckets`),
`GetBucketPolicyStatus` and the admin API's `public` flags treat a bucket
policy statement that allows anonymous access as **non-public** only when a
condition restricts it to a bounded set of sources. Negated, `...IfExists`
and `ForAllValues:` conditions never count as restricting; `ForAllValues:`
conditions hold when the key is missing, as it is for anonymous callers.

Allow statements for everyone that are restricted only by `aws:SourceIp`
are judged together: the union of their ranges across the whole policy may
cover at most four IPv4 /8 networks and four IPv6 /32 networks, so splitting
a wide range across statements does not hide it. Overlapping or repeated
ranges are merged, not counted twice, and `::ffff:a.b.c.d/96+n` counts as
the IPv4 /n. Wider allow-lists, such as `fd00::/8` or many large ranges,
count as public. Statements that also carry another restricting key
(`aws:username`, `aws:SourceVpce`, ...) are not counted towards that union.

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
- **Probes** (`POST /_gos3/admin/v1/discovery/probe`) need
  `admin:ProbeNetwork`, which the `diagnostics` policy does not grant (only
  `admin`, or a custom policy). They make the server fetch
  `<url>/_gos3/info` from any HTTP(S) host it can reach, which is a limited
  server-side request vector. The response must be a valid gos3 `Info`
  document (at most 64 KiB), probes never follow redirects or use proxies,
  each is bounded by 10 s, and at most 4 run at once per server (`429
  TooManyRequests`). A failed probe reports only `unreachable`, `timed out`,
  `HTTP <status>` or `not a gos3 server`, never the underlying error, so
  probes do not reveal banners of other services or why a connection failed
  (refused, reset, TLS or protocol errors all read `unreachable`).
- The standalone web UI's Node server performs local discovery (mDNS, beacon,
  scans, probes) for **anyone who can reach it**, without authentication.
  Restrict access to it, or set `GOS3_UI_LOCAL_DISCOVERY=false`. Against DNS
  rebinding and other websites, its `/api/discovery/*` routes answer 403
  unless the `Host` is an IP address, `localhost`/`*.localhost` or a name in
  `GOS3_UI_ALLOWED_HOSTS`, and refuse a foreign `Origin` (one that matches
  neither the `Host` nor a name listed in `GOS3_UI_ALLOWED_HOSTS`) or
  `Sec-Fetch-Site: cross-site`/`same-site`. At most 2 scans and 4 local
  discoveries run at once per UI server (`429 TooManyRequests`). Its probes
  read at most 64 KiB of `/_gos3/info` (with or without `Content-Length`);
  local discovery keeps at most 2048 mDNS records and 1024 beacon candidates
  and verifies at most 256 URLs.

## Admin API notes

- `DELETE /_gos3/admin/v1/buckets/{name}?force=true` deletes every version,
  including versions under object lock retention or legal hold. It needs
  `admin:ForceDeleteBucket`, which only `admin` grants by default.
- `PUT /_gos3/admin/v1/config` (`admin:UpdateConfig`) can change CORS,
  discovery and the public URL. Treat it like root access (see
  [Scoping admin permissions](#scoping-admin-permissions) for the other
  admin-equivalent actions).
- Admin requests are recorded in metrics (op `Admin`) and in the access log.
  The log contains paths only, never query strings, so presigned URL signatures
  are not logged.

## Dependencies

The shipped `gos3` binary links six third-party Go modules: `go.etcd.io/bbolt`,
`github.com/hashicorp/mdns`, `github.com/miekg/dns`, `golang.org/x/net`,
`golang.org/x/sys` and `gopkg.in/yaml.v3` (`go version -m bin/gos3` lists
them). Everything else in `go.mod` is used by tests only.

Supply-chain rules:

- **Release cooldown.** New versions are adopted only once they are at least
  14 days old, which gives the ecosystem time to catch malicious or broken
  releases. The web UI enforces this with pnpm's `minimumReleaseAge` in
  `web/pnpm-workspace.yaml`, and Dependabot applies the same cooldown to
  update pull requests for npm, Go modules, GitHub Actions and container
  images. Go dependencies are pinned to releases that were at least 14 days
  old when chosen. The only exceptions are security fixes younger than the
  cooldown: they are listed in `minimumReleaseAgeExclude` and pinned to the
  oldest fixed release in `overrides` (currently `shell-quote` 1.11.0 and
  `source-map-js` 1.2.2).
- **Small module graph.** `github.com/minio/minio-go/v7` (a test-only client)
  stays on v7.1.x: v7.2 and later declare their linter toolchain as module
  requirements, which would add about 200 modules to gos3's module graph.
- **Patched transitive packages.** `overrides` in `web/pnpm-workspace.yaml`
  forces patched versions where a dependency lags behind (for example
  `simple-git`, pulled in by Nuxt devtools, which gos3 disables).
- **No unused fallbacks.** The same `overrides` remove the WebAssembly
  fallbacks of Tailwind's and Parcel's native binaries
  (`@tailwindcss/oxide-wasm32-wasi`, `@parcel/watcher-wasm`). They load only
  on platforms without a prebuilt native package, which gos3 does not
  support.
- **Install scripts.** Only the packages listed in `onlyBuiltDependencies`
  may run install scripts.
- **Scanning.** CI runs `govulncheck`; `pnpm audit` covers the web UI.

Known open advisories, both without an upstream fix and both limited to Nuxt's
build and development tooling: `node-forge` (Nitro's dev server, `listhen`)
and `braces` (file globbing during builds, via `globby` → `micromatch`).
Neither is part of the embedded UI, which is static files, nor of the
standalone UI server bundle (`web/.output/server`).

## Reporting vulnerabilities

Please report security issues privately to the maintainers, for example via
GitHub's private vulnerability reporting on the repository, and not in public
issues.
