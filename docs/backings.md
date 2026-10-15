# Storage backings

A server spreads its buckets over named **backings**. Every backing has
one of three backend kinds:

| Backend | Where buckets live | Needs | Notes |
|---|---|---|---|
| `fs` | files in a local directory (`<path>/.gos3/`) | a `path` | the default |
| `memory` | the server's memory | nothing | lost on restart; no restic |
| `s3` | a bucket (and key prefix) of a remote S3 service: AWS S3, MinIO, Cloudflare R2, another gos3, ... | an endpoint, a bucket and credentials; **no local path** | every gos3 feature; one gos3 server per bucket and prefix |

An s3 backing is a complete storage backend, like fs: bucket records,
configuration documents, object versions, delete markers, multipart
uploads, object data, encryption keys and restic repositories all live in
the remote bucket. It supports versioning, object lock, conditional
writes, multipart uploads, compression, native AES256 and restic
encryption, replication and moves. The **gateway** option (see
[below](#the-gateway-option)) instead stores objects natively, so other
S3 tools can use them too, at the price of versioning and object lock.

A bucket's backing is chosen when it is created and can later be changed
with a move (see [operations.md](operations.md#multiple-backings-and-replication)),
except that gateway backings cannot receive copies.

## The default backing and additional backings

The backing configured by `storage.backend` is named `default`. Each entry
of `storage.backings` adds one more (see
[configuration.md](configuration.md#storage-backings)). An s3 backing can
be either:

```yaml
# All buckets on S3: the default backing is an s3 backing.
storage:
  backend: s3
  path: /var/lib/gos3             # optional: system state only (see below)
  s3:
    endpoint: https://s3.eu-central-1.amazonaws.com
    region: eu-central-1
    bucket: acme-gos3             # must exist
    prefix: prod/                 # optional
    # access_key, secret_key and encryption_key: here, or GOS3_S3_ACCESS_KEY,
    # GOS3_S3_SECRET_KEY and GOS3_S3_ENCRYPTION_KEY
```

```yaml
# Local disks plus S3 backings for some buckets.
storage:
  path: /srv/gos3                 # the "default" fs backing + system state
  backings:
    - name: cold                  # 1-32 of [a-z0-9_-]; "default" is reserved
      backend: s3                 # an s3 backing has no path
      s3:
        endpoint: https://s3.eu-central-1.amazonaws.com
        region: eu-central-1
        bucket: acme-gos3-cold
        prefix: gos3/
        # credentials and encryption key: here, or GOS3_BACKING_COLD_*
    - name: shared
      backend: s3
      s3:
        endpoint: http://minio.lan:9000
        bucket: gos3-meta         # gos3's own state of the gateway backing
        gateway: true             # objects in remote buckets named like the gos3 buckets
  default_backing: default        # backing of buckets created without a choice
```

The same in JSON (snake_case keys, integer byte sizes, as for every JSON
configuration):

```json
{
  "storage": {
    "path": "/srv/gos3",
    "backings": [
      { "name": "cold", "backend": "s3",
        "s3": { "endpoint": "https://s3.eu-central-1.amazonaws.com", "region": "eu-central-1",
                "bucket": "acme-gos3-cold", "prefix": "gos3/", "part_size": 33554432 } }
    ]
  }
}
```

### `s3` settings

| `s3` key | Env (default backing) | Description |
|---|---|---|
| `endpoint` | `GOS3_S3_ENDPOINT` | `http(s)://host[:port]` of the service, without a path. Required. |
| `region` | `GOS3_S3_REGION` | Signing region (default `us-east-1`). |
| `bucket` | `GOS3_S3_BUCKET` | The remote bucket holding the backing. Required; it must exist (gos3 does not create it). |
| `prefix` | `GOS3_S3_PREFIX` | Optional key prefix inside the bucket (a trailing `/` is added). Several backings, of one or more gos3 servers, can share a bucket with separate prefixes; two backings of one server whose prefixes in the same bucket are equal or nested are refused. |
| `addressing` | `GOS3_S3_ADDRESSING` | `path` (`<endpoint>/<bucket>/<key>`), `virtual` (`<bucket>.<host>/<key>`) or `auto` (default: virtual for `*.amazonaws.com`, path otherwise). |
| `part_size` | `GOS3_S3_PART_SIZE` | Part size of uploads to the service and the largest object sent with one PUT: 5 MiB to 5 GiB (default 16 MiB). |
| `access_key`, `secret_key` | `GOS3_S3_ACCESS_KEY`, `GOS3_S3_SECRET_KEY` | Credentials. Required (from the file or the environment). |
| `encryption_key` | `GOS3_S3_ENCRYPTION_KEY` | Key-encryption key (KEK): standard base64 of exactly 32 bytes, for example from `openssl rand -base64 32`. Needed for native AES256 and restic on the backing (see [Encryption](#encryption-on-s3-backings)). |
| `gateway` | `GOS3_S3_GATEWAY` | `true` stores objects natively (see [The gateway option](#the-gateway-option)). Default `false`. |

`gos3 config print` shows `secret_key` and `encryption_key` redacted.

### Environment variables

For the **default** backing, the `GOS3_S3_*` variables in the table above
are ordinary configuration variables: they override the file, as every
`GOS3_*` variable does. A complete s3 server can be configured from the
environment alone:

```sh
GOS3_STORAGE_BACKEND=s3
GOS3_S3_ENDPOINT=http://minio.lan:9000
GOS3_S3_BUCKET=gos3
GOS3_S3_ACCESS_KEY=...
GOS3_S3_SECRET_KEY=...
GOS3_S3_ENCRYPTION_KEY=...        # a fixed key: never generate a new one per start
GOS3_DATA=/var/lib/gos3           # optional: keeps IAM and settings across restarts
```

For **additional** backings, `GOS3_STORAGE_BACKINGS` accepts `name=s3`
entries next to `name=/path` and `name=memory`, and each s3 backing reads
the `GOS3_BACKING_<NAME>_<FIELD>` family: `<NAME>` is the backing name
upper-cased with `-` written as `_`, and `<FIELD>` is one of `ENDPOINT`,
`REGION`, `BUCKET`, `PREFIX`, `ADDRESSING`, `PART_SIZE`, `ACCESS_KEY`,
`SECRET_KEY`, `ENCRYPTION_KEY` and `GATEWAY`:

```sh
GOS3_STORAGE_BACKINGS=nvme=/mnt/nvme/gos3,cold=s3
GOS3_BACKING_COLD_ENDPOINT=https://s3.eu-central-1.amazonaws.com
GOS3_BACKING_COLD_REGION=eu-central-1
GOS3_BACKING_COLD_BUCKET=acme-gos3-cold
GOS3_BACKING_COLD_ACCESS_KEY=...
GOS3_BACKING_COLD_SECRET_KEY=...
GOS3_BACKING_COLD_ENCRYPTION_KEY=...
```

These variables **fill only the fields that are empty**: a value in the
configuration file (or in `backings.json`, see below) wins. That makes
them the place for secrets: keep the location in the file and the
`secret_key` and `encryption_key` in the environment (or a secret store
feeding it). They also apply to s3 backings added live: a backing named
`cold` created on the Storage page without a secret key takes
`GOS3_BACKING_COLD_SECRET_KEY`. Two backing names that differ only in `-`
versus `_` are refused, because they would share their variables.

### System state when the default backing is s3

IAM users and policies, runtime settings, bucket placements,
`backings.json`, the server ID and generated root credentials are not
stored in the remote bucket. As with the memory backend, they live in
`<storage.path>/.gos3/` when `storage.path` is set, and are **ephemeral**
otherwise: the server logs a warning at startup and loses them on restart
(new root credentials are generated on every start unless
`GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY` are set). The buckets
themselves survive either way: they are found in the remote bucket at the
next start. For anything but a test, set `storage.path` (or `GOS3_DATA`)
to a small persistent directory.

## Configuring backings live

**Storage → Configure backings** in the web UI, and
`PUT /_gos3/admin/v1/backings/config` in the admin API (see
[admin-api.md](admin-api.md#storage-backings-and-bucket-placement)), add,
change and remove additional backings, s3 backings included, without a
restart. Choose **S3 service** as the backend; the form asks for the
endpoint, region, bucket, prefix, access key and secret key, has a
**Gateway** switch and, under **Advanced**, the addressing style, the
part size and the encryption key (with a **Generate** button). Changes are
saved in `<storage.path>/.gos3/backings.json` and override
`storage.backings` and `storage.default_backing` at the next start (see
[configuration.md](configuration.md#storage-backings)). The `default`
backing is startup configuration.

- **Secrets are write-only.** The secret key and the encryption key are
  never returned: the API reports `secretKeyConfigured` and
  `encryptionKeyConfigured` instead, and the form shows "Saved – leave
  blank to keep". Leaving the field blank keeps the stored value; typing
  one replaces it. A value taken from a `GOS3_BACKING_<NAME>_*` variable
  is reported with the source `env` (the form names the variable).
- **`backings.json` holds the secrets** of backings configured this way
  and is written with mode 0600 (an owner-only ACL on Windows). Values
  filled from `GOS3_BACKING_<NAME>_*` variables (any field, not only the
  secrets) are never written to it, so they keep coming from the
  environment; the API lists them in the backing's `envFields`.
- **A new backing must load and be empty.** A live-added s3 backing must
  load within 30 seconds, so that wrong settings are reported right away
  instead of leaving a backing that never comes up, and must point at an
  unused bucket or prefix. To bring a prefix that already holds gos3
  buckets into a server, configure it at startup, where its buckets are
  adopted.
- **Connection settings can change while a backing is in use**: the
  endpoint, region, credentials, addressing, part size and the encryption
  key (adding one, removing it, or re-entering the same one). gos3 first
  connects with the new settings and checks the backing's `gos3.json`
  marker and encryption key, and only then switches; on failure the
  running backing is unchanged. This is how credentials are rotated.
  **Gateway backings** are the exception: their connection settings can
  change live only while they hold no buckets (`409 Conflict` otherwise);
  with buckets, change them in the configuration file (or `backings.json`)
  and restart.
- **Identity settings** (bucket, prefix, the gateway switch, or the
  backend kind) define which data the backing holds. Changing them
  replaces the backing, which needs it unused and empty: move its buckets
  and remove its replicas first (`409 Conflict` otherwise). The form locks
  those fields while the backing is in use. An s3 backing that holds no
  placed bucket can be removed or replaced even while its service is
  unreachable; the data in the remote bucket is left untouched.
- **Errors** are reported with the reason: `409 Conflict` when the prefix
  is in use by another gos3 server; `400` for settings that are wrong
  rather than a passing outage: an encryption key that does not match the
  backing, a prefix holding gos3 data without a marker (or a marker of a
  different kind, such as a gateway prefix for a non-gateway backing), a
  service without conditional writes, credentials the service refuses, a
  missing bucket or a wrong region. An endpoint that is this server itself
  is refused too (`400`): its data would be stored back into the same
  server. A service that does not answer at all fails the change with
  `503` once the 30 seconds are up.

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
  curl --aws-sigv4 "aws:amz:us-east-1:s3" --user "$AK:$SK" \
    -X PUT -H 'x-gos3-backing: cold' http://gos3:9000/photos
  ```

- **Web UI**: the *Create bucket* dialog offers the backings when there is
  more than one.
- **Admin API**: `GET /_gos3/admin/v1/whoami` lists the choices (name, kind
  and, for gateway backings, `mode: "gateway"`) for every caller;
  `GET /_gos3/admin/v1/backings` and the placement endpoints describe and
  change placements (see [admin-api.md](admin-api.md)).

Access control is unchanged: creating a bucket needs `s3:CreateBucket`
whatever the backing.

## How an s3 backing works

**Metadata** is kept in an in-memory index (the same one the memory
backend uses) and made durable in the remote bucket as a **journal**:
every change (a new version, a delete, a tag update, a bucket
configuration) is acknowledged only after the journal batch holding it
was written. Concurrent changes share batches, so a write costs its data
upload plus, typically, one small journal PUT. At startup the server loads
the newest **snapshot** of the index and replays the journal written
after it. Snapshots are written in the background when enough journal has
accumulated (64 MiB or 10,000 batches) or every 6 hours while there are
changes, and at a clean shutdown; the two newest are kept and older
journal batches are deleted.

**Object data** is stored in immutable objects under `data/`, as stored
by gos3 (compressed and/or encrypted when the bucket is). Uploads are
spooled to a local temporary file, then uploaded (in parallel parts when
larger than `part_size`) before the metadata change is journaled, so a
crash never publishes a partial object. CompleteMultipartUpload assembles
the parts server-side (UploadPartCopy) when the result is their plain
concatenation (no compression or encryption, parts of at least 5 MiB),
so no data flows back through gos3. Readers fetch ranges on demand;
superseded or deleted data is removed after the change is journaled, but
only once no reader has it open. A background pass deletes data and part
objects that no metadata references (left by a crash) once they are older
than 24 hours.

### What is stored where

Every key is under `<prefix>` in the configured bucket:

| Key | Content |
|---|---|
| `gos3.json` | marker: identifies the prefix as a gos3 backing (format, backing ID, gateway flag, a check value of the encryption key) |
| `meta/lease.json` | the owner lease of the running server |
| `meta/journal/<epoch>/<seq>` | journal batches (metadata changes) |
| `meta/snapshots/<lsn>-<epoch>` | snapshots of the metadata index |
| `data/<bucket-id>/<data-id>` | object data |
| `parts/<upload-id>/...` | parts of multipart uploads in progress |
| `keys/<bucket-id>.key` | the bucket's AES master key, wrapped (encrypted) with the encryption key |
| `keys/<bucket-id>.restic.json` | the bucket's restic recovery record (repository location; no password) |
| `restic/<bucket-id>/` | the bucket's restic repository |
| `gateway/parts/...` | gateway backings only: small multipart parts staged until completion |

Do not change or delete these objects with other tools, and do not put
other data under the prefix. gos3 refuses to open a prefix that holds
`meta/`, `data/` or `keys/` objects but no `gos3.json`, rather than
mixing data.

### Requirements and limits

- **Conditional writes.** The journal, the lease and the keys rely on
  `If-None-Match: *` and `If-Match` on PUT. AWS S3, MinIO, Cloudflare R2
  and gos3 support them. At the first open gos3 probes the service and
  refuses one that ignores them ("the S3 service does not support
  conditional writes"), because it could not keep two servers from
  overwriting each other's metadata.
- **One server per bucket and prefix.** The running server holds a lease
  (`meta/lease.json`), renewed every 10 seconds. A second server opening
  the same prefix is refused with "prefix in use by another server". After
  a crash, the next start waits until the old lease is 30 seconds old (by
  the S3 service's clock) and then takes over; a clean shutdown releases
  the lease at once. If an old server still runs after its lease was
  taken over (for example after a long network partition), its next
  journal write fails and it stops serving the backing, so it cannot
  overwrite the new owner's changes. A server that cannot renew its lease
  stops serving the backing before the lease can expire, and reloads it
  when the service answers again.
- **Memory and startup time.** The whole metadata index of a backing is
  held in memory and loaded at startup, like the memory backend's. The
  intended envelope is up to a few million object versions per backing;
  beyond that, use several backings (or prefixes), or fs. Startup time
  grows with the snapshot size and the journal written since.
- **Latency.** Every write waits for its data upload and its journal batch,
  and reads of data go to the service, so latency follows the service.

## Behaviour when the remote is unreachable

At startup the server waits up to 15 seconds for its s3 backings to load
(they load concurrently), so a reachable service is healthy from the first
request. A backing that has not loaded by then does not stop the server:
it starts unhealthy and keeps loading in the background. Only that
backing's buckets are affected: they answer `503 ServiceUnavailable`
until the backing is loaded, the backing is marked unhealthy (with the
last error on the Storage page and in `GET /_gos3/admin/v1/backings`), and
every other bucket is listed and served normally. When the service
answers, the backing loads, becomes healthy at the next health check, and
buckets found on it that have no placement yet are adopted. The same
applies while a running backing loses its service.

Until a backing that could not be listed at startup has been listed, gos3
cannot know which buckets it holds. Meanwhile requests for a bucket name
the server does not know answer `503 ServiceUnavailable` instead of
`404 NoSuchBucket`, and CreateBucket of a new name is refused with `503`,
so that a bucket on the missing backing is neither reported absent nor
created a second time. Admin API requests that
reach an unavailable backing also answer `503`.

`GET /_gos3/ready` reports ready while the bucket listing answers and the
**default** backing is healthy: an s3 default backing that is unreachable
makes the server not ready, while an unavailable additional backing does
not (its buckets are the only ones affected).

Some errors are not cured by waiting and need a configuration change: a
prefix in use by another server, an encryption key that does not match the
backing, a service without conditional writes, a prefix with gos3 data but
no marker, or a damaged journal. The backing stays unavailable and reports
the error.

## Encryption on s3 backings

Native AES256 and restic both work on s3 backings, and both need the
backing's **encryption key** (`encryption_key`, the KEK). Each bucket's
random AES master key is stored in the remote bucket wrapped with the KEK
(AES-256-GCM), so the S3 service never holds a usable key; restic
repository passwords are derived from the master key.

- Without an encryption key the backing offers neither AES256 nor restic:
  the web UI marks them unavailable, and AES256 uploads to a bucket with
  any copy on it (primary, replica or move target) are refused with
  `501 NotImplemented`. Adding a replica or moving a bucket to it is
  refused when the bucket uses AES256: as its provider, through its
  default encryption, or because it already holds AES256 versions.
- The KEK can be added later, also live on the Storage page. It cannot be
  changed: the marker and every wrapped key record a check value of the
  KEK, and a different key is refused ("encryption key does not match this
  backing") so that existing keys are not orphaned. A KEK added live is
  checked against the keys the backing already holds, and its check value
  is written to `gos3.json` when the backing is next loaded (for example
  at the next start).
- **Losing the KEK loses every encrypted object of the backing.** gos3
  never regenerates a missing key. Keep the KEK somewhere safe, apart from
  the remote bucket (a password manager, a secret store).
- Restic repositories of the backing's buckets live in the same bucket at
  `<prefix>restic/<bucket-id>`, using restic's s3 backend with the
  backing's current endpoint, region, credentials and addressing. The
  repository password derives from the bucket's master key, so the
  recovery record `<prefix>keys/<bucket-id>.restic.json` holds the
  repository location only. Restic needs the KEK, the backing's
  credentials, and the restic executable on the gos3 server (see
  [encryption.md](encryption.md)).
- Local files: each s3 backing gets its own temporary directory under the
  operating system's temporary directory (`gos3-<backing>-*`), removed at
  shutdown. It holds upload spool files, restic's cache
  (`store/restic-cache/<backing-id>`) and restores of restic objects being
  read, so it needs room for the largest object being uploaded or read. It
  is never the data directory.

## The gateway option

With `gateway: true`, objects are stored as themselves: gos3 key `k` of
bucket `b` is key `k` in the remote bucket `b`, in the same service.
Content headers and `x-amz-meta-*` user metadata are native; gos3-specific
metadata (tags, ACL, checksums, ...) travels in the reserved user-metadata
entry `x-amz-meta-gos3-meta`. Objects written by other clients appear in
gos3 listings and can be read through gos3. Use it to put gos3 (its IAM,
policies, web UI, discovery) in front of data that other tools must also
read and write directly, or to expose existing remote buckets.

The configured `bucket` and `prefix` hold gos3's own state of the gateway
backing (bucket records and configuration, multipart upload records,
staged parts), with the same lease and journal as other s3 backings. The
prefix applies only to that state, never to object keys, and the
configured `bucket` cannot also be used as a gos3 bucket name on the
gateway (creating it is refused as an existing bucket). Creating a gos3
bucket creates the remote bucket when it does not exist and adopts it, with its objects, when it does, except a remote bucket that
holds gos3 backing state: the bucket of another s3 backing of this server,
or one with a `gos3.json` marker (`409 BucketAlreadyExists`). Deleting the gos3 bucket
requires it to be empty (as in S3) and never deletes the remote bucket.
Bucket configuration (policy, CORS, lifecycle, tags, ...) is kept by
gos3, not set on the remote bucket.

### Gateway limits

- Versioning and object lock are not supported (`501 NotImplemented`);
  every object is the `null` version.
- A gateway backing cannot be a replica or a move target, and cannot be
  listed in `storage.replication.default_replicas`; nor can
  `default_replicas` be set while the default backing for new buckets
  (`storage.default_backing`) is a gateway. Its buckets can be moved away
  to another backing.
- Objects are stored as they are: native AES256 and restic are not
  offered, whatever the encryption key.
- Connection settings (endpoint, credentials, ...) can change live only
  while the gateway holds no buckets; otherwise change them in the
  configuration and restart.
- A reader fails, rather than mixing versions, when another client replaces
  the object while it is being read.
- Conditional writes are atomic against other gos3 clients and, where the
  remote supports `If-None-Match`/`If-Match`, against everyone.
- Listings show the remote ETag; for objects that gos3 had to upload in
  parts (larger than `part_size`) HEAD/GET report the original single-PUT
  ETag while listings show the remote multipart ETag.
- Metadata changes (tagging, ACL) rewrite the object in place with a
  server-side copy; listings then show the remote LastModified of the
  rewrite while HEAD/GET keep the original.
- Listings carry no tags or user metadata, so lifecycle rules filtering on
  tags never match gateway objects.
- Conditional deletes (`If-Match` on DeleteObject) are atomic only within
  one gos3 process.
- Multipart uploads: parts of 5 MiB or more go straight to a remote
  multipart upload; smaller parts are staged under `<prefix>gateway/parts/`
  until completion (an upload made only of small parts is sent as one
  object).

## Backups

An s3 backing is entirely in its bucket and prefix; nothing local is
needed to read it again. Protect it with the service's own means: bucket
versioning (so an accidental delete or overwrite of a journal, snapshot or
data object can be undone), replication to another region or service, or
a periodic copy of the whole prefix taken while gos3 is stopped. A copy of
a running backing can be inconsistent (a snapshot without the data it
refers to).

Also back up, separately:

- the **encryption key** of every s3 backing that has one: without it the
  encrypted objects and restic archives cannot be read;
- the system state in `<storage.path>/.gos3/` (`iam.json`,
  `settings.json`, `placements.json`, `backings.json`, ...), as described
  in [operations.md](operations.md#backups). `backings.json` contains the
  s3 credentials and encryption keys entered in the web UI; treat its
  backup as secret.

Gateway buckets hold ordinary objects that any S3 tool can back up; gos3
keeps only their bucket configuration and in-progress multipart uploads.

## Removing or renaming a backing

A backing that holds buckets cannot be removed while the server runs; move
its buckets first (gateway buckets can be moved away, but nothing can be
moved onto a gateway). Removing an s3 backing from the configuration makes
gos3 forget its buckets at the next start (it refuses to start while a
bucket's only copy was there; see
[operations.md](operations.md#multiple-backings-and-replication)). The data
in the remote bucket is never deleted: configuring the same bucket and
prefix again, under any backing name, brings its buckets back, because the
backing is identified by its `gos3.json` marker. A prefix can only be used
by one backing at a time.
