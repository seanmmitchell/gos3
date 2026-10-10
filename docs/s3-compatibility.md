# S3 compatibility

gos3 implements the S3 REST API (`2006-03-01`) for a single account. It is
tested with aws-sdk-go-v2 and minio-go (`test/e2e`), boto3 and AWS CLI v1
(`test/compat`), and the web UI's browser client. This page lists what is
supported, operation by operation, and where gos3 differs from AWS.

**Legend:** ✅ supported · 🟡 partial (see notes) · ❌ not supported
(`501 NotImplemented`)

## Addressing, protocol and authentication

| Feature | Status | Notes |
|---|---|---|
| Path-style URLs (`http://host:9000/bucket/key`) | ✅ | Always available. Use this with clients that need it (`forcePathStyle`, `addressing_style = path`). |
| Virtual-hosted-style URLs (`bucket.s3.example.com`) | ✅ | Needs `server.domains` and wildcard DNS (and a wildcard certificate for TLS). The longest matching domain wins; the port is ignored. |
| Signature Version 4, `Authorization` header | ✅ | Payload as hex SHA-256 or `UNSIGNED-PAYLOAD`. Allowed clock skew is 15 minutes. Signature comparison is constant-time. `SignedHeaders` listing a header twice is rejected (`AuthorizationHeaderMalformed`, or `AuthorizationQueryParametersError` for presigned URLs). `SignatureDoesNotMatch` echoes at most 16 KiB of `StringToSign` and `CanonicalRequest`. A signed `Expect: 100-continue` header also verifies over HTTP/2 (Go's HTTP/2 server removes that header before gos3 sees it). |
| SigV4 streaming (`aws-chunked`) | ✅ | `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `…-PAYLOAD-TRAILER` and `STREAMING-UNSIGNED-PAYLOAD-TRAILER` (the default of current AWS SDKs). Trailing checksums are verified. AWS's 8 KiB minimum chunk size is not enforced. |
| Presigned URLs (SigV4 query) | ✅ | `X-Amz-Expires` up to 7 days. Other `x-amz-*` query parameters are covered by the signature and treated as the corresponding request headers, as the AWS SDK for JavaScript v3 presigner produces them (for example `x-amz-copy-source`, `x-amz-meta-*`, `x-amz-tagging`, `x-amz-acl`, `x-amz-storage-class` and `x-amz-object-lock-*`); a header actually sent takes precedence. Unlike AWS, the checksum value parameters (`x-amz-checksum-crc32`, `-crc32c`, `-crc64nvme`, `-sha1`, `-sha256`) and `x-amz-sdk-checksum-algorithm` are ignored, because presigners compute them over an empty body when the URL is created; `x-amz-checksum-algorithm`, `x-amz-checksum-type` and `x-amz-checksum-mode` are honoured like the other parameters. Parameter values containing control characters get `400 InvalidArgument`. |
| Browser POST uploads (POST policy) | ✅ | V4 form fields, every policy condition including `content-length-range`, `${filename}`, `success_action_redirect` / `success_action_status`. |
| Anonymous access | ✅ | Allowed only by a bucket policy with `"Principal": "*"` or by a public canned ACL. |
| Region in the credential scope | ✅ | Any region is accepted unless `auth.strict_region` is set. Use `us-east-1` (or `server.region`). |
| Signature Version 2 | ❌ | Rejected with `AccessDenied` and a message saying so. AWS CLI **v1** presigns with SigV2 unless configured otherwise; see the [README](../README.md#aws-cli). |
| SigV4A | ❌ | |
| Session tokens (`X-Amz-Security-Token`, STS) | ❌ | Rejected with `InvalidToken`. There is no STS; use IAM users and access keys. |

Every response carries `x-amz-request-id`, `x-amz-id-2`, `Date` and
`Server: gos3/<version>`. Errors are AWS-style XML (`<Error><Code/>…`), and
HEAD errors have no body. Unknown query parameters are ignored, as on AWS.
Query strings are decoded exactly as the SigV4 signature covers them: `;` and
invalid `%`-escapes are literal characters of a parameter (not dropped, as
some HTTP libraries do), and a request with more than 1000 query parameters
gets `400 InvalidArgument`, whether it is signed or not.

## Service and bucket operations

| Operation | Status | Notes |
|---|---|---|
| ListBuckets | ✅ | `max-buckets`, `continuation-token`, `prefix`, `bucket-region`. Every bucket on the server is listed (one account); `BucketRegion` is always included. |
| CreateBucket | ✅ | Name rules as AWS. `LocationConstraint` is stored as the bucket region (`EU` → `eu-west-1`). Supports `x-amz-acl` (canned), `x-amz-object-ownership` and `x-amz-bucket-object-lock-enabled`. Directory buckets ❌. New buckets do **not** get AWS's 2023 defaults (BucketOwnerEnforced, Block Public Access): canned ACLs work as in classic S3. |
| DeleteBucket | ✅ | `BucketNotEmpty` also counts noncurrent versions and delete markers. In-progress multipart uploads do not keep a bucket alive; they are aborted with it, as on AWS. |
| HeadBucket | ✅ | `x-amz-bucket-region`. |
| GetBucketLocation | ✅ | Empty `LocationConstraint` for `us-east-1`. |
| ListObjects (v1) | ✅ | `prefix`, `delimiter`, `marker`, `max-keys` (≤ 1000), `encoding-type=url`. |
| ListObjectsV2 | ✅ | Opaque continuation tokens, `start-after`, `fetch-owner`, `ChecksumAlgorithm` / `ChecksumType` per object. |
| ListObjectVersions | ✅ | `key-marker` / `version-id-marker`; versions and delete markers interleaved in order. |
| ListMultipartUploads | ✅ | `prefix`, `delimiter`, `key-marker`, `upload-id-marker`, `max-uploads`, `encoding-type`. |

## Bucket configuration

| Subresource | Get / Put / Delete | Notes |
|---|---|---|
| `?versioning` | ✅ | Enabled / Suspended. MFA Delete ❌. Suspending is refused while object lock or replication is configured. |
| `?tagging` | ✅ | Up to 50 tags. |
| `?cors` | ✅ | Bucket rules override the server-wide `cors` settings. |
| `?policy`, `?policyStatus` | ✅ | IAM policy language with conditions (see [Authorization](#authorization)). Policies are stored byte for byte. `IsPublic` reflects anonymous access. |
| `?lifecycle` | 🟡 | Every rule form is validated and stored. Executed by the lifecycle worker: Expiration (Days, Date, ExpiredObjectDeleteMarker), NoncurrentVersionExpiration (NoncurrentDays, NewerNoncurrentVersions) and AbortIncompleteMultipartUpload. Filters: prefix, tags, object size. **Transitions are stored but never executed** (there are no storage tiers). `x-amz-expiration` and `x-amz-abort-date` headers are reported. |
| `?encryption` | 🟡 | Configuration stored and reported (AES256, aws:kms, aws:kms:dsse); objects get the matching `x-amz-server-side-encryption` headers. **gos3 does not encrypt data at rest**; use disk encryption. |
| `?acl` | 🟡 | Canned ACLs (`private`, `public-read`, `public-read-write`, `authenticated-read`, `log-delivery-write`), as `x-amz-acl` or as an equivalent `AccessControlPolicy`; what each grants is listed under [Authorization](#authorization). Grants to individual users or e-mail addresses and `x-amz-grant-*` headers ❌. |
| `?object-lock` | ✅ | Default retention (GOVERNANCE / COMPLIANCE, Days or Years). Lock can be enabled at creation or later on a versioned bucket. |
| `?ownershipControls` | ✅ | `BucketOwnerEnforced` disables ACLs. |
| `?publicAccessBlock` | ✅ | Enforced: `BlockPublicAcls`, `IgnorePublicAcls`, `BlockPublicPolicy`, `RestrictPublicBuckets`. |
| `?website` | 🟡 | Stored and returned only; there is no website endpoint. |
| `?notification` | 🟡 | Stored and returned only; no events are delivered. |
| `?logging` | 🟡 | Stored and returned only; no access logs are written to buckets (use the server access log). |
| `?replication` | 🟡 | Stored and returned only; nothing is replicated. |
| `?accelerate` | 🟡 | Stored and returned only. |
| `?requestPayment` | 🟡 | `BucketOwner` only; `Requester` ❌. |
| `?analytics`, `?inventory`, `?metrics`, `?intelligent-tiering`, `?torrent`, metadata tables, `?session` | ❌ | |

## Object operations

| Operation | Status | Notes |
|---|---|---|
| PutObject | ✅ | `Content-MD5`, `x-amz-checksum-*` (header or trailer), user metadata (≤ 2 KiB), `x-amz-tagging`, canned `x-amz-acl`, storage class (recorded only), object lock headers, `x-amz-website-redirect-location`, standard content headers. Conditional writes: `If-None-Match: *` and `If-Match: <etag>`, checked atomically. Size limit `server.max_object_size` (5 GiB). An ACL needs `s3:PutObjectAcl` and tags need `s3:PutObjectTagging` in addition to `s3:PutObject` (see [Authorization](#authorization)). |
| GetObject | ✅ | `Range` (single range), `partNumber`, conditional headers (`If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since`, `If-Range` with a strong ETag or the exact `Last-Modified`; a mismatch serves the whole object with 200), `response-*` overrides (signed requests only), `versionId`, `x-amz-checksum-mode: ENABLED`. `partNumber=1` on a single-part object returns 206 with `Content-Range`; an empty part returns an empty 200. Responses carry `X-Content-Type-Options: nosniff` and, except for PDFs, `Content-Security-Policy: sandbox` (see [security.md](security.md#object-responses)). |
| HeadObject | ✅ | Same as GetObject without a body. |
| DeleteObject | ✅ | Versioning semantics (delete markers), `x-amz-bypass-governance-retention`, `If-Match`. The `If-Match` and object lock checks are atomic with the delete. |
| DeleteObjects | ✅ | Up to 1000 keys, `Quiet` mode, per-key `ETag` condition, object lock checks. |
| CopyObject | ✅ | Versioned sources, `x-amz-copy-source-if-*`, `x-amz-metadata-directive` / `x-amz-tagging-directive` (COPY / REPLACE), checksum recomputation. Like UploadPartCopy and CompleteMultipartUpload, a copy that takes longer than about 5 s sends `200` early with whitespace keep-alives and may then report an `<Error>` inside the 200 body, as AWS does; the copy stops when the client disconnects. An ACL or `REPLACE` tags need `s3:PutObjectAcl` / `s3:PutObjectTagging`; copying the tags of a tagged source (`COPY`, the default) needs tagging permissions too (see [Authorization](#authorization)). |
| GetObjectAttributes | ✅ | ETag, Checksum, ObjectParts (paged), StorageClass, ObjectSize. |
| Get/Put/DeleteObjectTagging | ✅ | Up to 10 tags; version-aware. PutObjectTagging evaluates `s3:RequestObjectTag/<key>` and `s3:RequestObjectTagKeys` from the XML body. |
| Get/PutObjectAcl | 🟡 | Canned ACLs only (see bucket `?acl`). |
| Get/PutObjectRetention | ✅ | GOVERNANCE (with bypass) and COMPLIANCE (can only be extended). |
| Get/PutObjectLegalHold | ✅ | |
| PostObject (browser form upload) | ✅ | See [Addressing, protocol and authentication](#addressing-protocol-and-authentication). The `acl` and `tagging` fields need `s3:PutObjectAcl` / `s3:PutObjectTagging` (with the request tag condition keys from `tagging`). Form fields before the file must arrive within 60 s. |
| RestoreObject | ❌ | There are no archive tiers. |
| SelectObjectContent | ❌ | |
| Server-side encryption with customer keys (SSE-C) | ❌ | Requests with SSE-C headers get `NotImplemented`. |

## Multipart uploads

| Operation | Status | Notes |
|---|---|---|
| CreateMultipartUpload | ✅ | Metadata, tags, ACL, storage class, object lock and SSE headers carry over to the object. `x-amz-checksum-algorithm` and `x-amz-checksum-type` (COMPOSITE / FULL_OBJECT). An ACL or tags need `s3:PutObjectAcl` / `s3:PutObjectTagging`. |
| UploadPart | ✅ | Part numbers 1–10000, parts up to 5 GiB, checksum validation. |
| UploadPartCopy | ✅ | `x-amz-copy-source-range`, versioned sources, copy-source conditions. |
| CompleteMultipartUpload | ✅ | Every part except the last must be at least 5 MiB. Multipart ETag `md5(part md5s)-N`. Composite or full-object checksums. `If-None-Match` / `If-Match`. Total size up to 5 TiB. Slow completions send whitespace keep-alives, as AWS does. |
| AbortMultipartUpload | ✅ | |
| ListParts | ✅ | `max-parts`, `part-number-marker`, per-part checksums. |

## Checksums

| Algorithm | PutObject / UploadPart | Multipart object | Notes |
|---|---|---|---|
| MD5 (`Content-MD5`, ETag) | ✅ | ETag `…-N` | Always computed. |
| CRC32 | ✅ | COMPOSITE or FULL_OBJECT | |
| CRC32C | ✅ | COMPOSITE or FULL_OBJECT | |
| CRC64NVME | ✅ | FULL_OBJECT | Computed and stored by default when the client sends no checksum, as current AWS does. |
| SHA1 | ✅ | COMPOSITE | |
| SHA256 | ✅ | COMPOSITE | |

Checksums can be sent as headers or as aws-chunked trailers, and are verified
before the object is committed (`BadDigest` on mismatch). They are returned on
GET / HEAD with `x-amz-checksum-mode: ENABLED` (for the whole object), in
ListObjectsV2, ListParts and GetObjectAttributes.

## Authorization

- **Identities**: one root user (always allowed) and IAM users with access keys
  (up to 10 per user), managed through the admin API or the web UI.
- **Policies**: built-in `admin`, `readwrite`, `readonly`, `writeonly` and
  `diagnostics`, plus custom policies in the IAM policy language: Allow/Deny,
  `Action`/`NotAction` and `Resource`/`NotResource` with wildcards,
  `${aws:username}` variables, and condition operators `String*`, `Numeric*`,
  `Date*`, `Bool`, `IpAddress`/`NotIpAddress`, `Null`, `ArnLike`, `…IfExists`,
  `ForAnyValue:` / `ForAllValues:`.
- **Bucket policies** are evaluated together with identity policies, and an
  explicit Deny wins (except for root). Supported condition keys include
  `aws:SourceIp`, `aws:SecureTransport`, `aws:username`, `aws:userid`,
  `aws:PrincipalType`, `aws:CurrentTime`, `aws:EpochTime`, `aws:UserAgent`,
  `aws:Referer`, `s3:prefix`, `s3:delimiter`, `s3:max-keys`, `s3:VersionId`,
  `s3:ExistingObjectTag/<key>`, `s3:x-amz-copy-source`,
  `s3:x-amz-metadata-directive`, and the object-attribute keys `s3:x-amz-acl`,
  `s3:x-amz-server-side-encryption`,
  `s3:x-amz-server-side-encryption-aws-kms-key-id`, `s3:x-amz-storage-class`,
  `s3:x-amz-website-redirect-location`, `s3:object-lock-mode`,
  `s3:object-lock-retain-until-date`,
  `s3:object-lock-remaining-retention-days`, `s3:object-lock-legal-hold`,
  `s3:RequestObjectTag/<key>` and `s3:RequestObjectTagKeys`. The
  authentication keys are:
  - `s3:authType`: `REST-HEADER`, `REST-QUERY-STRING` (presigned URL) or
    `POST` (browser form upload); absent for anonymous requests.
  - `s3:signatureversion`: `AWS4-HMAC-SHA256` for every signed request.
  - `s3:signatureAge`: milliseconds since the request was signed
    (`X-Amz-Date`, or `Date` when a header-signed request has no
    `X-Amz-Date`; for PostObject the form's `x-amz-date`), so AWS's "deny
    presigned URLs older than N ms" policy works.
  - `s3:x-amz-content-sha256`: the signed payload mode (a hex digest,
    `UNSIGNED-PAYLOAD` or a `STREAMING-…` value); absent for presigned URLs
    with an unsigned payload and for anonymous requests.
  - `s3:TlsVersion`: `1.2` or `1.3`, only when TLS terminates at gos3. Behind
    a TLS-terminating proxy the key is absent.

  `s3:x-amz-grant-read`, `-write`, `-read-acp`, `-write-acp` and
  `-full-control` are set only from AccessControlPolicy bodies (see below),
  since `x-amz-grant-*` headers are not supported. Keys gos3 never sets (for
  example `aws:SourceVpce`, `aws:SourceVpc`, `aws:PrincipalOrgID`) are
  absent: positive operators on them evaluate to false, so `Allow`
  statements using them never grant and `Deny` statements using them never
  apply (use `…IfExists` or `Null` deliberately).
- **Where the object-attribute keys come from**, per operation:
  - PutObject, CreateMultipartUpload and CopyObject: the request headers.
  - PostObject: the normalised form fields; the POST's own headers are
    ignored.
  - PutObjectAcl and PutBucketAcl: the `x-amz-acl` header, or the
    AccessControlPolicy body. A body is evaluated with `s3:x-amz-acl` set to
    the equivalent canned ACL (`private` when nothing is granted) and
    `s3:x-amz-grant-read`, `-write`, `-read-acp`, `-write-acp` and
    `-full-control` set to its grantees as `id=<canonical id>` or
    `uri=<group URI>`. AWS sets neither for a body; gos3 does, so that
    "deny public ACL" policies cover both forms.
  - PutObjectTagging, PutObjectRetention and PutObjectLegalHold: the XML
    body. These operations are authorized after the body is read, so
    `MalformedXML` and invalid dates are reported before `AccessDenied`,
    `InvalidRequest` (no object lock) or `NoSuchKey`.
  - UploadPart, UploadPartCopy and CompleteMultipartUpload: the attributes
    recorded at CreateMultipartUpload (including bucket default encryption
    and retention applied then). The AWS "require SSE" bucket policy
    therefore works for multipart uploads.
  - `s3:RequestObjectTag/<key>` and `s3:RequestObjectTagKeys` come from
    `x-amz-tagging` only where that header is stored: PutObject,
    CreateMultipartUpload, and CopyObject with `REPLACE` (PostObject uses its
    `tagging` field, PutObjectTagging its body, and a `COPY` CopyObject the
    copied tags, described below).
  - `s3:object-lock-remaining-retention-days` is the number of days until
    the retain-until date, rounded up. On PutObject, CreateMultipartUpload
    and CopyObject it is not set for bucket default retention.
  - On the header path, `s3:object-lock-retain-until-date` is reported in
    RFC 3339 form (`2030-01-02T03:04:05Z`) whichever accepted ISO 8601 form
    was sent, and `s3:x-amz-metadata-directive` is reported upper-cased
    (`COPY` or `REPLACE`).
- Writing an object with an ACL (`x-amz-acl`, or the `acl` form field of
  PostObject) also requires `s3:PutObjectAcl`, and writing it with tags
  (`x-amz-tagging`, CopyObject's `REPLACE` tagging directive, or the
  `tagging` form field) also requires `s3:PutObjectTagging`, as on AWS. The
  built-in `writeonly` policy therefore cannot upload with an ACL or tags.
- CopyObject with the `COPY` tagging directive (the default) from a tagged
  source needs `s3:GetObjectTagging` on the source
  (`s3:GetObjectVersionTagging` when a `versionId` is given) and
  `s3:PutObjectTagging` on the destination, evaluated with the copied tags
  as `s3:RequestObjectTag/<key>` keys. Untagged sources, and `REPLACE`
  without tags, need only `s3:GetObject` plus `s3:PutObject`.
- **Principals** in bucket policies: `"*"`, `AWS` (user names, access key
  IDs, `arn:<partition>:iam::<account>:user/[path/]<name>`, the account
  principal) and `CanonicalUser` (matched like `AWS`). `Service` and
  `Federated` principals are accepted and stored but match no gos3
  identity: a statement with only them never applies, and a `NotPrincipal`
  naming only them applies to everyone. Other principal types (for example
  OIDC) are rejected with `MalformedPolicy`.
- The **account principal** (`arn:<partition>:iam::<account>:root` or a
  12-digit account ID; the account number is not checked, because gos3 has
  one account) is read in whichever way grants less:
  - `Deny` with `Principal` account: every authenticated IAM user (root is
    never denied).
  - `Allow` with `Principal` account: root only. It delegates to identity
    policies and grants IAM users nothing by itself.
  - `Allow` with `NotPrincipal` account: only anonymous callers get the
    grant (the policy counts as public).
  - `Deny` with `NotPrincipal` account: does not exempt IAM users; list the
    user's ARN or name as well.
- IAM actions follow AWS (for example `s3:GetObjectVersion` when `versionId` is
  given, and `s3:ListBucket` for HeadBucket). Admin API actions are
  `admin:<Action>` on resource `arn:gos3:admin:::*` (see
  [admin-api.md](admin-api.md)).
- **Canned ACLs** grant exactly:
  - `public-read` / `authenticated-read` (to everyone / to every
    authenticated identity): on a bucket, `s3:ListBucket`,
    `s3:ListBucketVersions` and `s3:GetBucketLocation`; on an object,
    `s3:GetObject` and `s3:GetObjectVersion`, through the object's own ACL.
  - `public-read-write`: the read set plus `s3:PutObject`, `s3:DeleteObject`
    (a delete marker in versioned buckets) and
    `s3:ListBucketMultipartUploads`. It does not grant permanent version
    deletes (`s3:DeleteObjectVersion`), aborting uploads
    (`s3:AbortMultipartUpload`) or listing parts
    (`s3:ListMultipartUploadParts`); these need a policy.
- A bucket ACL grants bucket-level access and object writes, but never object
  reads, as on AWS. To publish a bucket, use a bucket policy (the web UI has
  templates).

## Limits

| Item | Limit |
|---|---|
| Bucket name | 3–63 characters, lowercase letters, digits, `-` and `.`; starts and ends with a letter or digit; not an IP address; no `..`, no `xn--` prefix, no `-s3alias` / `--ol-s3` suffix |
| Object key | 1–1024 bytes of UTF-8 |
| User metadata | 2 KiB |
| Object tags / bucket tags | 10 / 50 |
| Single PUT | `server.max_object_size` (default 5 GiB, at most 5 TiB) |
| Multipart | 10000 parts of 5 MiB – 5 GiB (the last may be smaller), 5 TiB in total |
| `max-keys`, DeleteObjects keys | 1000 |
| Presigned URL lifetime | 7 days |
| Bucket policy size | 20 KiB |

## Other differences from AWS

- **One account.** All IAM users share the account; ListBuckets lists every
  bucket, and `BucketAlreadyOwnedByYou` is returned to the bucket's creator
  and to root, while other users get `BucketAlreadyExists`.
- **Storage classes** are accepted and reported but have no effect.
- **No replication between gos3 servers.** Every server stores its own data.
  Discovery only finds servers; it does not cluster them.
- With slow CopyObject and CompleteMultipartUpload calls (over about 5 s),
  `x-amz-version-id` and `x-amz-expiration` are sent as HTTP trailers because
  the status line has already gone out. SDKs then do not report them.
- Object keys containing characters that are invalid in XML 1.0 (C0 control
  characters other than tab, LF and CR) appear as U+FFFD in XML responses:
  listings without `encoding-type=url`, DeleteObjects results and the `Key`
  element of errors. Use `encoding-type=url` for such keys (the web UI always
  does for ListObjectsV2, ListObjectVersions and ListMultipartUploads).
- Request bodies that send no data for 60 s fail with `400 RequestTimeout`.
  On HTTP/1.1 the unread body of a rejected request is drained for at most
  10 s and 256 KiB; otherwise the response carries `Connection: close`. These
  limits are fixed.
- An upload aborted by the client is recorded in the access log with status
  `499` and `client_aborted=true`.
- **Root bypasses bucket policies.** The root credentials are never subject to
  a bucket policy, so an explicit `Deny` does not lock root out (on AWS it
  applies to the account root too). Use IAM users for everyday access.
- **Bucket regions.** CreateBucket accepts any `LocationConstraint`, even one
  other than `server.region`, and records it as the bucket's region; an AWS
  regional endpoint answers `IllegalLocationConstraintException` instead.
- `x-amz-expected-bucket-owner` is ignored (gos3 has no account IDs).
- A missing key read by a caller without `s3:ListBucket` on the bucket gives
  `403 AccessDenied` instead of `404 NoSuchKey`, as on AWS, so that key
  existence does not leak. Root and callers that may list still get 404.
- DeleteObject/DeleteObjects with a `versionId` that does not exist report
  success (idempotent); AWS may reject version IDs it cannot parse.
- More than 10 object tags give `BadRequest`, as on AWS; more than 50 bucket
  tags give `InvalidTag` (not verified against AWS).
- `PutBucketLifecycleConfiguration` accepts
  `x-amz-transition-default-minimum-object-size` but does not echo it, and
  rejects a rule with neither `Filter` nor `Prefix` (`MalformedXML`).
- Multipart checksums: UploadPart without a checksum is accepted on an upload
  created with a `ChecksumAlgorithm` (the server computes it). An upload
  created without an algorithm gets a CRC64NVME full-object checksum at
  completion, whatever per-part checksums the client sent.
- XML responses escape quotes in `ETag` elements as `&#34;` rather than
  `&quot;`; both are equivalent XML and every tested client accepts them.
- Object lock and `If-Match` / `ETag` checks on DeleteObject and
  DeleteObjects run in the same storage transaction (or under the same lock)
  as the delete, so a version written or locked concurrently is never
  removed by a check made against an older one. This relies on the optional
  `storage.ConditionalDeleter` extension, which both built-in backends
  implement; a backend without it gets a check made just before the delete.
- Header values of `x-amz-acl`, `x-amz-storage-class`,
  `x-amz-server-side-encryption`,
  `x-amz-server-side-encryption-aws-kms-key-id`, `x-amz-object-lock-*` and
  the copy directives (`x-amz-metadata-directive`,
  `x-amz-tagging-directive`) are trimmed of spaces and tabs only. A value
  padded with other whitespace (for example U+00A0) gets `400
  InvalidArgument` (or `InvalidStorageClass`) rather than being normalised
  after authorization, so the value a bucket policy sees is the value that
  is stored. SigV4 canonical header values are likewise trimmed of spaces
  and tabs only.
- The admin API's force delete (`DELETE /_gos3/admin/v1/buckets/{name}?force=true`)
  removes locked object versions too. It requires `admin:ForceDeleteBucket`.
