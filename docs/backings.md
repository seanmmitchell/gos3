# Remote S3 backings

A server spreads its buckets over named **backings** (see
[configuration.md](configuration.md#storage-backings)): local fs
directories, memory, and **s3 backings** that keep bucket data on a remote
S3 service (AWS S3, MinIO, Cloudflare R2, Backblaze B2, another gos3, ...).
A bucket's backing is chosen when it is created and can later be changed
with a move, except that gateway backings cannot receive copies.

| s3 mode | Where the data is | Metadata | Features |
|---|---|---|---|
| `blob` | opaque blobs in one remote bucket | the backing's local `path` (fs `meta.db`) | everything (versioning, object lock, conditional writes, multipart, compression, encryption, replication and moves) |
| `gateway` | native objects in the remote service, readable by any S3 client | bucket settings in the backing's `path`, object metadata remote | no versioning, no object lock, not a replica or move target (see [Gateway limits](#gateway-limits)) |

Use **blob** mode to put gos3's storage on cheap or durable object storage
while keeping all gos3 features; the remote bucket is only meaningful
together with the backing's `meta.db`. Use **gateway** mode to put gos3 (its
IAM, policies, web UI, discovery) in front of data that other tools must
also read and write directly, or to expose an existing remote bucket.

## Configuration

```yaml
storage:
  path: /srv/gos3                 # the "default" backing + system state
  backings:
    - name: cold                  # 1-32 of [a-z0-9_-]; "default" is reserved
      backend: s3
      path: /srv/gos3-cold        # local metadata of this backing
      s3:
        mode: blob
        endpoint: https://s3.eu-central-1.amazonaws.com
        region: eu-central-1
        bucket: acme-gos3-blobs   # required in blob mode; must exist
        prefix: prod/             # optional key prefix
        # access_key / secret_key, or GOS3_BACKING_COLD_ACCESS_KEY / _SECRET_KEY
    - name: shared
      backend: s3
      path: /srv/gos3-shared
      s3:
        mode: gateway
        endpoint: http://minio.lan:9000
        addressing: path          # auto (default) | path | virtual
        # bucket empty: gos3 bucket "x" is remote bucket "x" (created if missing)
  default_backing: default        # backing of buckets created without a choice
```

| `s3` key | Description |
|---|---|
| `mode` | `blob` or `gateway`. |
| `endpoint` | `http(s)://host[:port]` of the service, no path. |
| `region` | Signing region (default `us-east-1`); also the region of remote buckets a gateway creates. |
| `bucket` | Blob mode: the remote bucket holding the blobs (required, must exist). Gateway mode: empty = every gos3 bucket maps to the remote bucket of the same name; set = all gos3 buckets share this remote bucket, bucket `b` under the key prefix `<prefix>b/`. |
| `prefix` | Prepended to every remote key. |
| `access_key`, `secret_key` | Credentials. When empty they come from `GOS3_BACKING_<NAME>_ACCESS_KEY` and `GOS3_BACKING_<NAME>_SECRET_KEY` (name upper-cased, `-` as `_`). `gos3 config print` redacts the secret. |
| `addressing` | `path` (`<endpoint>/<bucket>/<key>`), `virtual` (`<bucket>.<host>/<key>`) or `auto` (virtual for `*.amazonaws.com`, path otherwise). |
| `part_size` | Part size of uploads to the service and the largest object sent with one PUT (default 16 MiB, 5 MiB to 5 GiB). |

s3 backings are startup configuration: the live backing editor of the web
UI keeps them as configured but cannot add or change them, and
`restic_s3` does not apply to them. Remote services are not contacted at
startup: an unreachable service only makes the backing's own buckets fail,
with `503 ServiceUnavailable`, and marks the backing unhealthy.

## Choosing the backing of a bucket

- **S3 API**: send the gos3 extension header `x-gos3-backing: <name>` with
  CreateBucket. Without it the bucket goes to `storage.default_backing`. An
  unknown name is `400 InvalidArgument`; object lock on a gateway backing
  is `501 NotImplemented`. HeadBucket answers with `x-gos3-backing` (the
  backing holding the primary copy). The default replicas
  (`storage.replication.default_replicas`) are added as usual.

  The AWS CLI cannot add request headers; use curl, an SDK middleware or
  the web UI:

  ```sh
  curl --aws-sigv4 "aws:amz:us-east-1:s3" --user "$AK:$SK"     -X PUT -H 'x-gos3-backing: cold' http://gos3:9000/photos
  ```

- **Web UI**: the *Create bucket* dialog offers the backings when there is
  more than one.
- **Admin API**: `GET /_gos3/admin/v1/whoami` lists the choices (name, kind
  and s3 mode) for every caller; `GET /_gos3/admin/v1/backings` and the
  placement endpoints describe and change placements (see
  [admin-api.md](admin-api.md)).

Access control is unchanged: creating a bucket needs `s3:CreateBucket`
whatever the backing.

## Blob mode

Bucket records, object versions, delete markers, tags, ACLs, checksums,
multipart upload records: everything except the bytes stays in the fs
backend's `meta.db`. The bytes are stored under

```
<prefix>data/<bucket-id>/<data-id>       object data (immutable, as stored: compressed and/or encrypted when the bucket is)
<prefix>uploads/<upload-id>/<part-file>  multipart parts
```

- Writes are buffered (in memory up to 8 MiB, then in `<path>/.gos3/tmp/`)
  and uploaded when the client's upload completes, before the metadata
  commit, so a crash never publishes a partial object. Large objects are
  uploaded in parallel parts.
- CompleteMultipartUpload assembles the parts server-side (UploadPartCopy)
  when they are at least 5 MiB, so no data flows back through gos3.
- Readers fetch ranges on demand. Superseded or deleted blobs are removed
  after their metadata change commits, but only once no reader has them
  open.
- At startup a background pass deletes remote blobs that `meta.db` does not
  reference and that are older than the orphan retention (7 days), the
  remote equivalent of the local quarantine.
- **Back up `meta.db`** (see [operations.md](operations.md)): without it the
  blobs cannot be mapped back to buckets and keys. gos3 refuses to start
  with an empty `meta.db` while a blob backing still holds blobs.

## Gateway mode

Objects are stored as themselves: gos3 key `k` of bucket `b` is remote key
`<prefix>k` in remote bucket `b` (or `<prefix>b/k` in the shared
`bucket`). Content headers and `x-amz-meta-*` user metadata are native;
gos3-specific metadata (tags, ACL, checksums, retention, ...) travels in
the reserved user-metadata entry `x-amz-meta-gos3-meta`. Objects written by
other clients appear in gos3 listings and can be read through gos3.

Creating a gos3 bucket on a gateway without a shared `bucket` creates the
remote bucket when it does not exist and adopts it, with its objects, when
it does. Deleting the gos3 bucket requires it to be empty (as in S3) and
never deletes the remote bucket. Bucket configuration (policy, CORS,
lifecycle, tags, ...) is kept by gos3, not set on the remote bucket.

gos3 keeps the records of in-progress multipart uploads in
`<path>/.gos3/gateway/`.

### Gateway limits

- Versioning and object lock are not supported (`501 NotImplemented`);
  every object is the `null` version.
- A reader fails, rather than mixing versions, when another client replaces
  the object while it is being read.
- Conditional writes are atomic against other gos3 clients and, where the
  remote supports `If-None-Match`/`If-Match`, against everyone.
- Listings show the remote ETag; for objects that gos3 had to upload in
  parts (larger than `part_size`) HEAD/GET report the original single-PUT
  ETag while listings show the remote multipart ETag.
- Metadata changes (tagging, ACL, retention, legal hold) rewrite the object
  in place with a server-side copy; listings then show the remote
  LastModified of the rewrite while HEAD/GET keep the original.
- Listings carry no tags or user metadata, so lifecycle rules filtering on
  tags never match gateway objects.
- Conditional deletes (`If-Match` on DeleteObject, object lock checks) are
  atomic only within one gos3 process.
- The prefix (and, with a shared `bucket`, the gos3 bucket name) count
  against S3's 1024-byte key limit, so the longest gos3 key is shorter.
- Multipart uploads: parts of 5 MiB or more go straight to a remote
  multipart upload; smaller parts are staged in
  `<path>/.gos3/gateway/parts/` until completion (an upload made only
  of small parts is sent as one object).

## Removing or renaming a backing

A backing that holds buckets cannot be removed while the server runs; move
its buckets first (blob backings only: gateway buckets cannot be moved).
Removing an s3 backing from the configuration makes gos3 forget its buckets
at the next start; gateway data on the remote is never touched, and blob
data is useless without the backing's `meta.db`.
