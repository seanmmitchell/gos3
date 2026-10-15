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
- [ ] List the host names clients use in `server.allowed_hosts` (see
      [DNS rebinding](#dns-rebinding-and-the-host-header)), so requests
      for any other name are refused, signed ones included.
- [ ] Decide on discovery: `discovery.advertise`, `discovery.allow_scan`, and
      `server.public_info`.
- [ ] Keep the data directory private (mode 0700) and back it up; it contains
      secrets.
- [ ] For s3 backings: scope their credentials to their bucket, keep the
      remote bucket private, and store each backing's encryption key
      somewhere safe (see [s3 backings](#s3-backings)).
- [ ] If metrics are public (`metrics.public`), restrict who can reach
      `/_gos3/metrics`.
- [ ] Enable a standalone web UI's local discovery
      (`GOS3_UI_LOCAL_DISCOVERY=true`) only when its Node server is not
      reachable from untrusted networks.

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
  are sandboxed (see [Object responses](#object-responses)) and the
  embedded UI is served with a Content-Security-Policy whose `script-src`
  allows only the UI's own files and the hashes of its build's inline
  scripts (no `unsafe-inline`/`unsafe-eval`), so injected markup cannot run
  script. `connect-src` stays open to any http(s) origin because the UI
  talks to every S3 endpoint the user adds.

### Root credentials

The root identity is always allowed, even where a bucket policy denies access.
Use it to administer the server, and give applications IAM users with narrow
policies.

| Configuration | Behaviour |
|---|---|
| `GOS3_ROOT_ACCESS_KEY` + `GOS3_ROOT_SECRET_KEY` (or `auth.root_*` in YAML) | Used as given. The secret is never logged. A secret shorter than 16 characters is accepted with a warning. A published example secret (`change-me-now`, `gos3admin-secret`, `minioadmin`, the AWS documentation key, ...) is refused unless `auth.insecure_demo_credentials` / `GOS3_INSECURE_DEMO_CREDENTIALS=true` is set. |
| Not set, with a data directory | Generated on first start with a random 20-character access key and 40-character secret, saved to `<data>/.gos3/root-credentials.json` (mode 0600) and reused on later starts. The secret is **never logged**: the startup banner shows it only when standard error is a terminal, otherwise it names the file. The log names the access key and the file. |
| Not set, memory or s3 backend without a data path | New keys on every start. On a terminal the banner shows the secret; otherwise it is written to a private temporary file (named in the banner and the log) that is deleted on exit. |

Logs end up in systems (journald, `docker logs`, Kubernetes log stacks) that
many more people can read than the data directory, so no secret is written
there. Published example keys are refused because the default CORS policy
admits signed requests from any origin: any website a user opens could sign
requests with a published key and drive the server through the user's
browser (even one bound to `127.0.0.1`). A short secret can be guessed
offline from a single captured signed request, especially over plain HTTP.

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

Revocation also stops requests already in flight. Authentication and
authorization happen before a request body is read, and a body may arrive
slowly, so every write with a body (PutObject, PostObject, UploadPart,
CompleteMultipartUpload, PutObjectAcl, PutObjectTagging,
PutObjectRetention, PutObjectLegalHold, DeleteObjects, CreateBucket and
every bucket configuration write: policy, ACL, public access block,
ownership controls, CORS, lifecycle, versioning, encryption, object lock,
tagging and the rest) checks again right
before it commits that its access key is still active and belongs to the
same enabled user, and fails with `AccessDenied` otherwise. A write that has
been in flight for more than 5 seconds also repeats its authorization
decisions against the current policies, ACLs, ownership controls and public
access block, so a policy change or a newly enabled `BlockPublicAcls`
applies to it as well. That repeated decision evaluates the clock-derived
condition keys (`aws:CurrentTime`, `aws:EpochTime`, `s3:signatureAge`,
`s3:object-lock-remaining-retention-days`) as of the request's start, so a
long upload that was allowed when it started is not refused merely for
taking long. Reads already streaming a response are not cut off.

Generated keys have a 20-character ID and a 40-character random secret. A
secret chosen through the admin API must be at least 16 characters: any
SigV4 signature (a shared presigned URL, a logged request) lets an attacker
test guesses of the secret offline, so prefer generated secrets. Keys stored
by earlier versions with shorter secrets keep working. gos3 does not
throttle failed signature checks, and, as on AWS, `InvalidAccessKeyId`
versus `SignatureDoesNotMatch` tells a caller whether an access key ID
exists; rate-limit authentication failures at a reverse proxy if the
endpoint is exposed.

Bucket-policy principals name users by name and keys by ID, so the two
namespaces are kept apart: a key ID cannot equal a user name, `root`,
`anonymous` or a 12-digit account ID, a user name cannot equal a key ID or
a 12-digit account ID, and a bare 12-digit principal only ever names the
account. Keys in an older `iam.json` that break this rule are refused at
authentication and logged at startup and on first use; replace them.

Bucket policies must name local users by bare name, not by an AWS IAM user
ARN: gos3 cannot verify an ARN's account or path. Before upgrading, replace
user ARNs in `Principal` and `NotPrincipal` with the intended local names.
Existing policies that still contain user ARNs fail closed for non-root
requests until root updates them.

Choosing a key's ID (`"accessKey"` in `POST /users/{name}/keys`) needs
`admin:CreateAccessKey` at service level, so a delegated administrator can
only create keys with generated IDs. Principals, `arn:gos3:admin:::user/…`
resources and `aws:username` / `aws:userid` are bound to the user **name**,
which can be reused: a user created with the name of a deleted user (or a
key re-created with a deleted key's ID) inherits every bucket-policy
statement and delegation that named it. Remove a deleted user's name from
bucket policies and admin policies, or do not reuse names.

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
  administrator and sign as them. Choosing the key's ID always needs this
  action at service level, even when it is otherwise scoped.
- `admin:UpdateAccessKey`: reactivate an administrator key that was
  deactivated, for example after it leaked.
- `admin:UpdateConfig`: change CORS, discovery and the public URL, and the
  storage backings (including s3 endpoints, credentials and encryption
  keys); treat it like root access.

To delegate part of user or policy management, scope these actions with the
per-target resources (see
[admin-api.md](admin-api.md#resources-and-condition-keys)):
`arn:gos3:admin:::user/<name>` for user and key actions (a key is evaluated
as its owner), `arn:gos3:admin:::policy/<name>` for policy actions and
`arn:gos3:admin:::bucket/<name>` for `admin:GetBucket`,
`admin:ForceDeleteBucket`, `admin:GetBucketCompression` and
`admin:PutBucketCompression`, with wildcards, and limit which policies may be
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
resource, and s3 ARNs such as `arn:aws:s3:::*` never grant admin actions,
neither as `Resource` nor as `NotResource`: an Allow of `"Action": "*"` with
`"NotResource": ["arn:aws:s3:::secret/*"]` grants every s3 action outside
`secret` but no admin action (an Allow's `NotResource` covers only the
namespaces it lists; a Deny's applies to everything not listed). The
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
nor plant tags that `s3:ExistingObjectTag` conditions rely on.

When a decision depends on the object itself (`s3:ExistingObjectTag/*`
conditions, or an object ACL for reads), the operation acts on exactly the
version that was evaluated: reads open that version, and deletes and ACL,
tagging, retention and legal hold updates are made conditional on it,
atomically in the storage backend. If another object took its place in the
meantime (in unversioned and versioning-suspended buckets an overwrite
reuses the version ID `null`), the request fails with `AccessDenied`
instead of acting on an object the decision never saw. Each entry of a
DeleteObjects request is decided on a fresh lookup of its object.
CompleteMultipartUpload checks the upload's canned ACL against the bucket's
current `BlockPublicAcls` (refusing a public ACL) and ownership controls
(dropping the ACL when ACLs are disabled), not only when the upload was
created. The
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
| `.gos3/backings.json` | backings configured in the web UI (names, backends, paths, s3 settings **including s3 secret keys and encryption keys**, except values taken from the environment) | **secret** (0600) |
| `.gos3/meta.db` | bucket and object metadata, bucket policies and configurations (fs backend) | private (0600) |
| `.gos3/data/`, `.gos3/uploads/` | object and part data | your data (files 0600) |
| `.gos3/keys/` | bucket encryption keys and restic repository passwords (fs backend) | **secret** (0600) |
| `.gos3/restic/`, `.gos3/restic-cache/` | restic repositories and cache (fs backend) | encrypted by restic |
| `.gos3/server-id` | random server ID | public |

Keep the directory owned by the gos3 user with mode 0700, and treat backups as
secret too. With the fs backend, gos3 also removes group and other access
from an existing `.gos3`, its `data/`, `tmp/`, `uploads/` and `orphans/`
directories and `meta.db` at startup (logging a warning), so a directory
pre-created by a volume mount or restored from a backup with looser modes
does not stay readable. For every backend the same is done for `.gos3`
itself, `root-credentials.json`, `iam.json` and `settings.json`. On Windows
the modes above do not apply; instead gos3 gives `.gos3` a protected ACL
(no inherited entries) that grants access only to the account running gos3,
SYSTEM and Administrators, inherited by everything inside, and writes the
secret files with the same ACL. If that fails (for example on a file system
without ACLs), gos3 logs a warning and starts anyway. The fs backend's
`.gos3/keys/` directory (encryption keys and restic recovery records) is
handled the same way at startup, and is also made private before a new key
is written there; if that fails, creating the key (and so enabling
encryption for the bucket) fails. Object data outside
`.gos3` keeps the data directory's ACL. SSE-S3 (`AES256`) encrypts new object
data using native AES-256-GCM or the bucket’s restic provider. Existing legacy
objects require rewriting; AWS KMS and SSE-C return `NotImplemented`.
Server-managed keys allow unattended access and must be backed up with
metadata. They do not protect against a compromised server or a full copy of
its data and keys. See [encryption.md](encryption.md).

### s3 backings

An [s3 backing](backings.md) keeps everything of its buckets in a remote
bucket: the metadata journal and snapshots (bucket names, object keys, user
metadata, tags, policies and other bucket configuration, unencrypted),
object data, wrapped encryption keys and restic repositories. Anyone who
can read that bucket can read the metadata and every unencrypted object;
anyone who can write it can change or destroy the backing. Give the
backing's credentials access to its bucket (and, for a gateway, to creating
and using the remote buckets named like the gos3 buckets) and nothing
else, and protect the bucket like the data directory.

- **Encryption key (KEK).** The per-bucket master keys of an s3 backing
  are stored in the remote bucket wrapped (AES-256-GCM) with the backing's
  `encryption_key`, which is never stored there (only a check value, which
  does not reveal it). The remote bucket alone therefore does not decrypt
  AES256 or restic data. The KEK lives in the configuration file, the
  environment (`GOS3_S3_ENCRYPTION_KEY`,
  `GOS3_BACKING_<NAME>_ENCRYPTION_KEY`) or `backings.json`; keep a copy
  elsewhere, because losing it loses the encrypted data.
- **Where the secrets are.** The s3 `secret_key` and `encryption_key` come
  from the configuration file, the environment, or the web UI (then saved
  in `backings.json`, mode 0600, never with values that came from the
  environment). Prefer the environment or a secret store for them, and
  keep a configuration file that contains them at mode 0600.
- **Redaction.** `gos3 config print` shows `secret_key` and
  `encryption_key` redacted. The admin API never returns them: they are
  write-only (`GET /backings/config` reports only `secretKeyConfigured`,
  `encryptionKeyConfigured` and whether a value comes from the
  environment), and the Storage page shows them as "Saved – leave blank to
  keep". Backing status shows the location (`s3://bucket/prefix/`), not
  credentials. Restic runs with the credentials in its environment, never
  on its command line, and its diagnostics are redacted before they are
  logged.
- **Who can change them.** Reading the backing configuration needs
  `admin:GetConfig` and changing it `admin:UpdateConfig` at service level;
  an administrator who can change backings can point a backing at any
  S3 service, so treat it like root. An endpoint that is this server
  itself is refused.

Compressed objects (see [operations.md](operations.md#compression)) are
not protected by compression alone: without object encryption, anyone who
can read the data files can inflate them. The stored size of a compressed object depends on its content, so it
can leak how well data that an attacker partly controls compresses together
with a secret in the same object (the idea behind CRIME/BREACH). Stored sizes
are only reported by the admin API's bucket usage and stats
(`admin:GetBucket`, `admin:ListBuckets`, `admin:GetStats`), as totals per
bucket; if your buckets mix untrusted input with secrets in single objects,
keep compression off for them or do not grant those actions to the parties
supplying the input.

Only one gos3 process can use a data directory at a time: every backend,
including the memory and s3 backends with a data path, takes an exclusive lock on
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
- Behind a proxy, enable `server.trust_proxy_headers` and list the proxies
  in `server.trusted_proxies`. gos3 then honours `X-Forwarded-For` /
  `X-Forwarded-Proto` only from those peers, takes the rightmost
  `X-Forwarded-For` entry that is not a trusted proxy as the client and the
  last `X-Forwarded-Proto` value as the transport, so a client cannot choose
  its `aws:SourceIp` by sending its own header, even through a proxy that
  appends to it. List only the proxies themselves, never a subnet that also
  holds clients: a client whose own address is in `trusted_proxies` is
  skipped like a proxy hop, so it can choose its `aws:SourceIp` again.
  `trust_proxy_headers` with an empty `trusted_proxies` is a configuration
  error (it used to trust every peer, so a client that could reach gos3
  without the proxy could fake `aws:SourceIp` and `aws:SecureTransport`).
  The proxy must *set* `X-Forwarded-Proto` (not append to a client-sent
  one). The setting applies to the S3 API, the admin API and the
  `/_gos3/*` endpoints (conditions and access logs alike).
- Where TLS terminates at gos3, `aws:SecureTransport` and `s3:TlsVersion`
  come from the connection, not from the request: an HTTP/2 client that
  sends the `:scheme http` pseudo-header over TLS cannot drop
  `s3:TlsVersion` from the condition context.
- gos3 logs a warning at startup when it serves plain HTTP on a
  non-loopback address without `trust_proxy_headers`.

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
- Bodies read before the caller is authorized must arrive completely within
  60 s, however steadily they trickle in: PostObject form fields before the
  file, and the bodies of DeleteObjects (at most about 6.2 MiB; the keys to
  authorize are in it), PutObjectTagging, PutObjectRetention and
  PutObjectLegalHold (at most 64 KiB; their condition keys come from the
  body). Every other S3 request is authorized before its body is read, and
  re-validated before it commits (see [IAM users and keys](#iam-users-and-keys)).
- Slow CopyObject, UploadPartCopy and CompleteMultipartUpload calls keep the
  connection alive with whitespace (see
  [s3-compatibility.md](s3-compatibility.md#other-differences-from-aws)).
- Every write of a response (in pieces of at most 64 KiB) must make progress
  within 2 minutes; otherwise the request is aborted (HTTP/1.1: the
  connection is closed; HTTP/2: the stream is reset). Downloads of any size
  keep working as long as the client reads at least about 550 bytes/s. A
  client that stops reading, or never opens its HTTP/2 flow-control window,
  cannot hold handlers, buffers and open files forever.
- HTTP/2 connections allow 100 concurrent streams (net/http's default is
  250), are pinged after 30 s of silence and closed when a write to them
  makes no progress for 2 minutes.
- `server.max_connections` (off by default) caps the open client
  connections: at the cap a new connection replaces the longest-idle
  keep-alive connection, or is closed when all are busy. Without it the
  process's file descriptor limit applies. Internet-facing servers should
  sit behind a proxy with connection and rate limits.

### Request size limits

XML request bodies are bounded by the largest valid request: about 6.2 MiB
for DeleteObjects (1000 keys), about 2.4 MiB for CompleteMultipartUpload
(10000 parts), at most 2 MiB for bucket configuration documents and 64 KiB
for object subresources; a larger body is rejected with `MaxMessageLengthExceeded`
after reading at most one byte more. DeleteObjects and
CompleteMultipartUpload documents are parsed as they arrive: the 1001st
`<Object>` or the 10001st `<Part>` ends the request at once, and nesting
depth, element count and the size of a single tag are bounded, so the work
and memory a request causes stay proportional to a valid one, also for an
anonymous DeleteObjects that is rejected per key afterwards. A DeleteObjects
`<Key>`, `<VersionId>` or `<ETag>` longer than 1024 bytes ends the request
too, so per-key errors never echo more than a valid key, and the
DeleteResult is streamed as the keys are processed (each write with a 60 s
deadline) rather than built in memory. DeleteObjects also requires
`Content-MD5` or an `x-amz-checksum-*` header (or trailer), verified against
the body, as AWS does; a web page cannot send such a header cross-origin
without a CORS preflight, so it cannot make a visitor's browser delete
objects in a bucket that its network position (an `aws:SourceIp` policy) or
anonymous access lets it write to.

Object metadata is bounded as on AWS: user metadata (`x-amz-meta-*`) to
2 KiB, and all metadata returned as response headers (the content headers
`Content-Type`, `Content-Encoding`, `Content-Disposition`,
`Content-Language`, `Cache-Control`, `Expires`, the website redirect and the
user metadata) to 8 KiB together (`MetadataTooLarge`), on PutObject,
CopyObject (including metadata copied from the source), PostObject and
CreateMultipartUpload. Names and values must be valid HTTP header names and
values (no control characters); this matters for PostObject, whose form
fields are not restricted by HTTP. Listings keep only the fixed-size fields
of each entry in memory, so a page of 1000 entries costs the same whatever
metadata the objects carry. Lifecycle rule prefixes are limited to 1024
bytes, and parsed lifecycle configurations (looked up on object reads to
report `x-amz-expiration`) are cached within a 64 MiB budget and recognised
by a hash of the stored document, without copying it per request.

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
responses are served as the bare `application/pdf` (parameters are dropped)
and always with `Content-Disposition: attachment` (keeping a stored file
name), whatever the stored disposition or a `response-content-disposition`
override says, so a PDF is downloaded rather than rendered inline on the UI's
origin.
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
- A preflight's `Access-Control-Request-Headers` is limited to 8 KiB and 64
  requested headers before matching bucket CORS rules; larger lists get
  `400 BadRequest`.
- Browser POST-policy uploads (PostObject) never count as signed, even when
  they come with an `Authorization` header or `X-Amz-*` query parameters:
  PostObject authenticates from its form fields and accepts forms without a
  policy signature anonymously.

Websites therefore cannot use a visitor's browser to read anonymously
accessible data, such as buckets restricted by `aws:SourceIp` or reachable
only on the LAN, and see the response. Hiding the response does not stop a
write, though: browsers send HTML form posts (including PostObject uploads)
and `no-cors` requests with a `text/plain` body without a preflight. gos3
therefore refuses (`403 AccessDenied`) anonymous requests that change data
(any method but GET, HEAD and OPTIONS) when a browser sends them for another
origin (`Sec-Fetch-Site: same-site` or `cross-site`, or an `Origin` that is
not the request's host, `null` included), unless the bucket's CORS
configuration or an explicitly listed global origin allows that origin and
method. Clients that are not browsers send neither header and are not
affected. Anonymous reads (GET, HEAD) carry
`Cross-Origin-Resource-Policy: same-site`, so other sites cannot load
LAN-only objects as `<script>`, stylesheets or images (reading what a
script defines, measuring images, or probing which keys exist); a bucket
CORS rule that allows GET, or an explicitly listed global origin, lifts it.
Signed requests, presigned URLs included, never get it. The embedded and
standalone web UIs are unaffected, because they sign every request. The
global `*` default stays for that reason: browser clients connect
cross-origin with signed requests, and a page can only sign if it already
has a secret key (which is why published example keys are refused, see
[Root credentials](#root-credentials)).

Explicitly listed global origins (`cors.allowed_origins`, `--cors-origin`, or
**Settings → CORS**) and bucket CORS configurations, including a bucket rule
with `*`, still apply to anonymous requests. Bucket CORS configurations take
precedence for their bucket. To serve a public bucket to scripts on other
sites, set a bucket CORS configuration. Browser POST-policy uploads
(PostObject) from another origin are refused under the global `*` rule; a
bucket CORS rule that allows the origin and `POST` admits them.
Bucket CORS values must be valid in the headers they are sent in
(`AllowedHeader`/`ExposeHeader` header names, no spaces or control
characters in `AllowedOrigin`); a stored configuration that no longer
validates (written by an older version) disables CORS for its bucket, with
a warning in the log, until it is replaced or deleted. Requests to the
bucket are otherwise unaffected.
In production, list the origins of the web UIs you use rather than relying
on `*`. `/_gos3/info` always answers with `Access-Control-Allow-Origin: *`.

### DNS rebinding and the Host header

A website can make its own host name resolve to a gos3 server (DNS
rebinding). The visitor's browser then talks to gos3 *same-origin* with the
attacker's page, so CORS never applies. gos3 therefore checks the `Host`
header. Known names are IP addresses, `localhost` and `*.localhost`, the OS
host name (its first label and `<label>.local`), the hosts of
`server.address`, `server.public_url` and `server.domains` (with bucket
subdomains), and the entries of `server.allowed_hosts`.

- With `server.allowed_hosts` empty (the default), requests for any other
  name are served only when signed: the signature covers the `Host`, and a
  rebinding page has no key. Anonymous S3 requests get `403 AccessDenied`;
  the web UI, the root redirect and public metrics answer `421 Misdirected
  Request`; `/_gos3/info`, `/_gos3/health`, `/_gos3/ready` and the admin
  API (always signed) are served. The first request for each unknown name
  is logged as a warning.
- With `server.allowed_hosts` set, every request for another name is
  refused with `421`, signed ones included. A request that the web UI
  signed for a wrong or malicious endpoint (`Host: evil.example`) then
  cannot be relayed to the real server.
- `server.allowed_hosts: ["*"]` turns the check off.

Behind a reverse proxy that passes the client's `Host` through, set
`server.public_url` or list the name in `server.allowed_hosts`.

## Public access evaluation

Block Public Access (`BlockPublicPolicy`, `RestrictPublicBuckets`),
`GetBucketPolicyStatus` and the admin API's `public` flags treat a bucket
policy statement that allows anonymous access as **non-public** only when a
condition restricts it to a bounded set of sources. Negated, `...IfExists`
and `ForAllValues:` conditions never count as restricting; `ForAllValues:`
conditions hold when the key is missing, as it is for anonymous callers.

Allow statements for everyone that are restricted only by `aws:SourceIp`
are judged together: each IPv4 range must be /8 or narrower and each IPv6
range /32 or narrower. The union across the whole policy may cover at most
four IPv4 /8 networks and four IPv6 /32 networks, so splitting a wide range
across statements does not hide it. Overlapping or repeated
ranges are merged, not counted twice, and `::ffff:a.b.c.d/96+n` counts as
the IPv4 /n. Wider allow-lists, such as `fd00::/8` or many large ranges,
count as public. Statements that also carry another restricting key
(`aws:username`, `aws:SourceVpce`, ...) are not counted towards that union.

## Public endpoints

| Endpoint | Default access | Reveals |
|---|---|---|
| `/_gos3/health`, `/_gos3/ready` | anonymous | `{"status":"ok"}`; `/ready` answers `503` with `"storage unavailable"` (the storage error itself is only logged, once per state change) and reuses its result for 2 s |
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
  subnet (never from a broadcast or subnet network address), are
  rate-limited per sender and in total, and can only make a server probe the
  sender's own IP (announced host names are never resolved). Queries are
  rate-limited too, so a server cannot be used to flood the link with
  announces. If the network interfaces cannot be listed, only datagrams from
  this host are accepted. See
  [discovery.md](discovery.md#3-udp-multicast-beacon).
- The mDNS responder (UDP 5353) answers only senders on this host or a
  directly connected subnet, and only questions about the server's own
  records. It collapses duplicate questions (at most 8 per query),
  rate-limits each sender like the beacon (50 responses per second in
  total), multicasts answers to the group with TTL 255 and keeps every
  response to one packet of at most 1400 bytes, so it cannot be used to
  reflect or amplify traffic towards other networks. Queries from other
  hosts must arrive with TTL 255 (RFC 6762 §11), which drops datagrams
  forwarded from off the link with a spoofed on-link source; this check
  also runs on Windows. On a platform where the TTL cannot be read, a
  warning is logged and other hosts are answered by link-local multicast
  only, never by unicast. It runs whatever the
  listen address and `server.public_info` are: on hosts with a routable
  address, also firewall UDP 5353 from outside the local network, or set
  `discovery.mdns: false`. mDNS answers are accepted only from on-link
  senders, and only the responder's own address is probed (a loopback
  address only when the responder is this host). See
  [discovery.md](discovery.md#2-mdns--dns-sd).
- Announcements are unauthenticated hints. A candidate becomes a peer only after
  its `/_gos3/info` was fetched, and adding a server to the UI still needs
  credentials. A malicious host on the network can still make itself appear in
  peer lists, but it cannot take over a verified peer's entry (name,
  endpoints, addresses) while that peer still answers at its known
  addresses; such a claim is shown in the peer's `error` and logged. See
  [discovery.md](discovery.md#peer-identity).
- The peer table is bounded in memory: at most 1024 peers with a stored
  identity document of at most 4 KiB each, so `GET
  /_gos3/admin/v1/discovery` stays a few MiB at most. Peers known only from
  announcements are limited to 512, and to 16 per sender address (IPv6: per
  /64); unverified sightings to 256 and 16 per sender. A flood of fake
  servers from one host therefore cannot fill the table, and never
  displaces verified peers or peers found through seeds, DNS SRV, the admin
  probe or scans. Announcements never keep a verified peer alive: only a
  successful probe does.
- An announcement that names this host's own loopback or local addresses
  is accepted only from a sender on this host. If the operating system
  delivers packets from the network whose source is one of the host's own
  addresses (Linux drops them by default), an on-link attacker could make
  the server fetch `/_gos3/info` from local ports; a failed check of such an
  address is recorded only as `could not be verified`, so the peer table
  does not reveal which local ports are open.
- Discovery probes (admin API probe, scans, seeds, DNS SRV, verification of
  announcements) never connect to cloud metadata endpoints
  (`169.254.169.254`, `169.254.170.2`, `169.254.170.23`, `169.254.0.23`,
  `168.63.129.16`, `192.0.0.192`, `fd00:ec2::254`, `fd00:ec2::23`,
  `fd20:ce::254`, `100.100.100.200`, also in IPv4-mapped, IPv4-compatible or NAT64 `64:ff9b::/96` and
  `64:ff9b:1::/48` form) or to unspecified, multicast or broadcast
  addresses, checked after DNS resolution. Peer errors hold a fixed category, never
  text the probed service sent.
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
  TooManyRequests`). Targets that are, or resolve to, cloud metadata,
  unspecified, multicast or broadcast addresses are refused (`400`). A failed probe reports only `unreachable`, `timed out`,
  `HTTP <status>` or `not a gos3 server`, never the underlying error, so
  probes do not reveal banners of other services or why a connection failed
  (refused, reset, TLS or protocol errors all read `unreachable`).
- The standalone web UI's Node server can perform local discovery (mDNS,
  beacon, scans, probes) from the UI host. It has no authentication, so
  this is **off unless `GOS3_UI_LOCAL_DISCOVERY=true`** (the container image
  and the compose demo leave it off). When on, anyone who can reach the server can make it
  probe and scan, so enable it only on a trusted interface. Against DNS
  rebinding and other websites, its `/api/discovery/*` routes answer 403
  unless the `Host` (and any `X-Forwarded-Host` / `Forwarded: host=`) is an
  IP address, `localhost`/`*.localhost` or a name in
  `GOS3_UI_ALLOWED_HOSTS`, and refuse a foreign `Origin` (one that matches
  neither the `Host` nor a name listed in `GOS3_UI_ALLOWED_HOSTS`),
  `Sec-Fetch-Site: cross-site`/`same-site`, and a browser
  `GET /api/discovery/local` without `Sec-Fetch-Site` (older browsers)
  that lacks the `X-Gos3-Discovery` header. Like gos3, its mDNS and beacon discovery accept datagrams only
  from on-link senders (only loopback when the interfaces cannot be listed)
  and only ever probe the sender's own address, and a
  host claiming the ID of a server already verified in the same result
  cannot take over its entry (the claim is reported in `error`; two
  addresses count as one server only when they were observed together or
  each document advertises the other's address). As for gos3, a host on
  the link that spoofs the UI host's own address as the source of a beacon
  or mDNS answer can still make it probe its loopback and local addresses
  (a fixed `GET /_gos3/info`, never returned to that host). Identity
  documents are validated and bounded (16 KiB, cleaned and clipped fields,
  at most 64 features and 64 `endpoints.all` entries). It never connects to
  cloud metadata addresses (`169.254.169.254`, `169.254.170.2`,
  `169.254.170.23`, `169.254.0.23`, `168.63.129.16`, `192.0.0.192`,
  `100.100.100.200`, `fd00:ec2::254`, `fd00:ec2::23`, `fd20:ce::254`),
  unspecified, multicast or broadcast addresses, including their
  IPv4-mapped, IPv4-compatible and NAT64 (`64:ff9b::/96`, `64:ff9b:1::/48`)
  forms: literals are refused up front, and host names are checked
  against the addresses they resolve to when the connection is made (DNS
  rebinding cannot bypass it). Probe failures report only a coarse reason
  (unreachable, timed out, `HTTP <status>`, authentication required, not a
  gos3 server). At most 2 scans, 4 local discoveries and 8 probes run at
  once per UI server (`429 TooManyRequests`), each taking its slot before
  the request body is read; bodies are capped at 8 KiB (probe) and 64 KiB
  (scan) with `413`. Its probes read at most 16 KiB of `/_gos3/info` (with
  or without `Content-Length`); local discovery keeps at most 2048 mDNS
  records and 1024 beacon candidates (16 per sender) and verifies at most
  256 URLs. Socket errors are reported as fixed texts (details go to the
  server log).
- The standalone UI's Node server sends the embedded UI's security headers
  (the same Content-Security-Policy with per-page inline-script hashes,
  `frame-ancestors 'none'`, `X-Frame-Options: DENY`,
  `Referrer-Policy: no-referrer`, `nosniff`) plus a restrictive
  `Permissions-Policy`. Its `servers.json` is served by a route behind the
  Host check, not as a static file. When presets are configured
  (`NUXT_PUBLIC_SERVERS`), and always for `servers.json`, it answers `421`
  to a `Host` a DNS-rebinding
  site could use (anything but IP addresses, `localhost`, names that cannot
  exist in public DNS such as single labels, `.local`, `.lan`, `.internal`,
  `.home.arpa`, and names in `GOS3_UI_ALLOWED_HOSTS`). Builds refuse presets
  that contain secret keys (`GOS3_UI_ALLOW_PRESET_SECRETS=true` overrides).

## Admin API notes

- `DELETE /_gos3/admin/v1/buckets/{name}?force=true` deletes every version.
  It needs `admin:ForceDeleteBucket` on the bucket, which only `admin` grants
  by default, and it honours object lock like S3 DeleteObject: a bucket with
  any version under a legal hold or unexpired COMPLIANCE retention is refused
  (`409`) before anything is deleted, whoever asks, root included. Unexpired
  GOVERNANCE retention is refused too unless the request adds
  `bypassGovernanceRetention=true` and the caller's identity policies allow
  `s3:BypassGovernanceRetention` on each locked object (`403` otherwise), so a
  key with only `admin:ForceDeleteBucket` cannot destroy locked data. Remove
  legal holds through the S3 API first; COMPLIANCE data stays until its
  retention expires.
- `PUT /_gos3/admin/v1/config` (`admin:UpdateConfig`) can change CORS,
  discovery and the public URL. Treat it like root access (see
  [Scoping admin permissions](#scoping-admin-permissions) for the other
  admin-equivalent actions).
- Admin requests are authorized before their body is read. Once the body
  has arrived, the request is refused (403 `AccessDenied`, nothing changed)
  if the signing key was meanwhile deleted or deactivated or its user
  disabled, and a request in flight for more than 5 seconds has its route
  action authorized again against the current policies (time condition keys
  as of the request's start), as S3 writes are. There is no absolute
  deadline for the body beyond the server's idle read timeout.
- Admin requests are recorded in metrics (op `Admin`) and in the access log.
  The log contains paths only, never query strings, so presigned URL signatures
  are not logged.

## Dependencies

The shipped `gos3` binary links five third-party Go modules: `go.etcd.io/bbolt`,
`github.com/miekg/dns`, `golang.org/x/net`, `golang.org/x/sys` and
`gopkg.in/yaml.v3` (`go version -m bin/gos3` lists them). Everything else in `go.mod` is used by tests only.

Supply-chain rules:

- **Release cooldown.** New versions are adopted only once they are at least
  14 days old, which gives the ecosystem time to catch malicious or broken
  releases. The web UI enforces this with pnpm's `minimumReleaseAge` in
  `web/pnpm-workspace.yaml`, and Dependabot applies the same cooldown to
  update pull requests for npm, Go modules, Python packages, GitHub Actions
  and container images (except the golang base image: Go point releases
  carry standard library security fixes). Go dependencies are pinned to releases that were at
  least 14 days old when chosen. The compatibility suite's Python packages,
  transitive ones included, are compiled with a 14-day cut-off into the
  hash-locked `test/compat/requirements.txt`. The only exceptions are
  security fixes younger than the cooldown: the npm ones are listed in
  `minimumReleaseAgeExclude` and pinned to the oldest fixed release in
  `overrides` (currently `shell-quote` 1.11.0 and `source-map-js` 1.2.2),
  and the Go ones are the oldest fixed release in `go.mod` (currently
  `golang.org/x/net` v0.60.0).
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
- **Release builds.** Releases are built without GitHub Actions caches (which
  any job on `dev` can write), the shipped UI is built without the test-only
  npm packages, and archives and images carry signed build provenance
  attestations (`gh attestation verify`). See
  [releasing.md](releasing.md#build-integrity).
- **License notices.** `THIRD_PARTY_NOTICES` reproduces the licenses of
  everything gos3 ships: the Go standard library, the Go modules linked into
  the binary on every release platform, and the npm packages that the web UI
  builds bundle (recorded during the build by
  `web/modules/third-party-notices.ts`). `make notices` regenerates it. CI
  fails when it is out of date, for example when a dependency update adds a
  package or changes a license text, so run `make notices` on such a pull
  request. A shipped package without a license file needs its upstream text in
  `internal/tools/notices/fallback/`.

Known open advisories, both without an upstream fix and both limited to Nuxt's
build and development tooling: `node-forge` (Nitro's dev server, `listhen`)
and `braces` (file globbing during builds, via `globby` → `micromatch`).
Neither is part of the embedded UI, which is static files, nor of the
standalone UI server bundle (`web/.output/server`).

## Reporting vulnerabilities

Please report security issues privately to the maintainers, for example via
GitHub's private vulnerability reporting on the repository, and not in public
issues.
