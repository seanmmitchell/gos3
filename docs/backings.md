# Storage backings

A server spreads its buckets over named **backings**. Every backing has
one of three backend kinds:

| Backend | Where buckets live | Needs | Notes |
|---|---|---|---|
| `fs` | files in a local directory (`<path>/.gos3/`) | a `path` | the default |
| `memory` | the server's memory | nothing | lost on restart; no restic |
| `s3` | an account of a remote S3 service (AWS S3, MinIO, Cloudflare R2, another gos3, ...): **native** buckets in remote buckets of their own, **wrapped** buckets in gos3's format inside a container | an endpoint and credentials; a home bucket or `storage.path` (for its records); **no local data directory** | features depend on the [bucket kind](#bucket-kinds); one gos3 server per home and per container |

An s3 backing is a connection to one account of an S3 service. Its buckets
are of two kinds, chosen per bucket (see [Bucket kinds](#bucket-kinds)):

- **Native buckets** are remote buckets used as they are: gos3 key `k` is
  key `k` of the remote bucket, so other S3 tools keep reading and writing
  the objects. Existing remote buckets can be adopted (by an
  administrator), new ones created. Versioning and object lock are the
  remote bucket's, passed through.
- **Wrapped buckets** are stored in gos3's own format inside a
  **container** (a remote bucket and prefix), with every gos3 feature:
  versioning and object lock on any service, conditional writes, multipart
  uploads, compression, native AES256 and restic encryption, replication
  and moves. Many wrapped buckets share one container.

gos3 keeps a few **records** of its own for each s3 backing (which gos3
bucket is of which kind and lives where, the configuration of native
buckets), in the backing's **home** or, without one, in the server's system
directory (see [Home and records](#home-and-records)). The home is also
the backing's shared container.

A bucket's backing is chosen when it is created and can later be changed
with a move (see [operations.md](operations.md#multiple-backings-and-replication)),
except that native buckets can neither be moved nor receive copies.

## The default backing and additional backings

The backing configured by `storage.backend` is named `default`. Each entry
of `storage.backings` adds one more (see
[configuration.md](configuration.md#storage-backings)). An s3 backing can
be either:

```yaml
# All buckets on S3: the default backing is an s3 backing.
storage:
  backend: s3
  path: /var/lib/gos3             # optional with a home: system state only (see below)
  s3:
    endpoint: https://s3.eu-central-1.amazonaws.com
    region: eu-central-1
    home_bucket: acme-gos3        # must exist: records + shared container
    home_prefix: prod/            # optional
    # default_kind: wrapped       # the default with a home
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
        home_bucket: acme-gos3-cold
        home_prefix: gos3/
        # credentials and encryption key: here, or GOS3_BACKING_COLD_*
    - name: shared                # no home: records in /srv/gos3/.gos3/s3/shared/
      backend: s3
      s3:
        endpoint: http://minio.lan:9000
        # default_kind: native    # the default without a home: new buckets are
        #                         # remote buckets other tools can use
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
                "home_bucket": "acme-gos3-cold", "home_prefix": "gos3/", "part_size": 33554432 } }
    ]
  }
}
```

### `s3` settings

| `s3` key | Env (default backing) | Description |
|---|---|---|
| `endpoint` | `GOS3_S3_ENDPOINT` | `http(s)://host[:port]` of the service, without a path. Required. |
| `region` | `GOS3_S3_REGION` | Signing region (default `us-east-1`). Remote buckets in other regions are reached in their own region. |
| `home_bucket` | `GOS3_S3_HOME_BUCKET` | The remote bucket of the backing's [home](#home-and-records): gos3's records for the backing and its shared container of wrapped buckets. Optional; when set it must exist (gos3 does not create it). Without it the records are kept in the system directory, which then needs `storage.path`. |
| `home_prefix` | `GOS3_S3_HOME_PREFIX` | Optional key prefix of the home inside `home_bucket` (a trailing `/` is added); requires `home_bucket`. Several backings, of one or more gos3 servers, can share a bucket with separate prefixes; two backings of one server whose homes are on the same service and bucket with equal or nested prefixes are refused. |
| `default_kind` | `GOS3_S3_DEFAULT_KIND` | `wrapped` or `native`: the [kind](#bucket-kinds) of buckets created without one. Default: `wrapped` with a home, `native` without. `wrapped` requires a home. |
| `addressing` | `GOS3_S3_ADDRESSING` | `path` (`<endpoint>/<bucket>/<key>`), `virtual` (`<bucket>.<host>/<key>`) or `auto` (default: virtual for `*.amazonaws.com`, path otherwise). |
| `part_size` | `GOS3_S3_PART_SIZE` | Part size of uploads to the service and the largest object sent with one PUT: 5 MiB to 5 GiB (default 16 MiB). |
| `access_key`, `secret_key` | `GOS3_S3_ACCESS_KEY`, `GOS3_S3_SECRET_KEY` | Credentials. Required (from the file or the environment). See [Permissions](#permissions-of-the-backings-credentials). |
| `encryption_key` | `GOS3_S3_ENCRYPTION_KEY` | Key-encryption key (KEK): standard base64 of exactly 32 bytes, for example from `openssl rand -base64 32`. Needed for native AES256 and restic on the backing's wrapped buckets (see [Encryption](#encryption-on-s3-backings)). |
| `bucket`, `prefix`, `gateway` | `GOS3_S3_BUCKET`, `GOS3_S3_PREFIX`, `GOS3_S3_GATEWAY` | **Deprecated** keys of earlier development builds, still accepted: `bucket` and `prefix` mean `home_bucket` and `home_prefix`, `gateway: true` means `default_kind: native`. The server logs a deprecation warning for each at startup; a deprecated key and its replacement with different values are refused. See [Migration](#migration-from-earlier-development-builds). |

`gos3 config print` shows `secret_key` and `encryption_key` redacted, and
the deprecated keys under their new names.

### Environment variables

For the **default** backing, the `GOS3_S3_*` variables in the table above
are ordinary configuration variables: they override the file, as every
`GOS3_*` variable does. A complete s3 server can be configured from the
environment alone:

```sh
GOS3_STORAGE_BACKEND=s3
GOS3_S3_ENDPOINT=http://minio.lan:9000
GOS3_S3_HOME_BUCKET=gos3
GOS3_S3_ACCESS_KEY=...
GOS3_S3_SECRET_KEY=...
GOS3_S3_ENCRYPTION_KEY=...        # a fixed key: never generate a new one per start
GOS3_DATA=/var/lib/gos3           # optional with a home: keeps IAM and settings across restarts
```

For **additional** backings, `GOS3_STORAGE_BACKINGS` accepts `name=s3`
entries next to `name=/path` and `name=memory`, and each s3 backing reads
the `GOS3_BACKING_<NAME>_<FIELD>` family: `<NAME>` is the backing name
upper-cased with `-` written as `_`, and `<FIELD>` is one of `ENDPOINT`,
`REGION`, `HOME_BUCKET`, `HOME_PREFIX`, `DEFAULT_KIND`, `ADDRESSING`,
`PART_SIZE`, `ACCESS_KEY`, `SECRET_KEY` and `ENCRYPTION_KEY` (`BUCKET`,
`PREFIX` and `GATEWAY=true` are deprecated aliases of `HOME_BUCKET`,
`HOME_PREFIX` and `DEFAULT_KIND=native`):

```sh
GOS3_STORAGE_BACKINGS=nvme=/mnt/nvme/gos3,cold=s3
GOS3_BACKING_COLD_ENDPOINT=https://s3.eu-central-1.amazonaws.com
GOS3_BACKING_COLD_REGION=eu-central-1
GOS3_BACKING_COLD_HOME_BUCKET=acme-gos3-cold
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
versus `_` are refused, because they would share their variables. A
deprecated variable and its replacement set to different values are an
error.

### System state when the default backing is s3

IAM users and policies, runtime settings, bucket placements,
`backings.json`, the server ID and generated root credentials are not
stored remotely. As with the memory backend, they live in
`<storage.path>/.gos3/` when `storage.path` is set, and are **ephemeral**
otherwise: the server logs a warning at startup and loses them on restart
(new root credentials are generated on every start unless
`GOS3_ROOT_ACCESS_KEY` / `GOS3_ROOT_SECRET_KEY` are set). With a home, the
buckets themselves survive either way: they are found in the home at the
next start. For anything but a test, set `storage.path` (or `GOS3_DATA`)
to a small persistent directory. An s3 backing **without a home** keeps
its records in that directory and cannot be configured without it.

## Configuring backings live

**Storage → Configure backings** in the web UI, and
`PUT /_gos3/admin/v1/backings/config` in the admin API (see
[admin-api.md](admin-api.md#storage-backings-and-bucket-placement)), add,
change and remove additional backings, s3 backings included, without a
restart. Choose **S3 service** as the backend; the form asks for the
endpoint, region, home bucket, home prefix, default bucket kind, access key
and secret key and, under **Advanced**, the addressing style, the part size
and the encryption key (with a **Generate** button). Changes are saved in
`<storage.path>/.gos3/backings.json` and override `storage.backings` and
`storage.default_backing` at the next start (see
[configuration.md](configuration.md#storage-backings)). The `default`
backing is startup configuration. Remote buckets and containers of an s3
backing are managed on its own page (see
[Remote bucket browser](#remote-bucket-browser)).

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
  environment; the API lists them in the backing's `envFields`. Only the
  current keys are written: a `backings.json` with the deprecated `bucket`,
  `prefix` or `gateway` keys is read as before and rewritten with the new
  keys at the next change.
- **A new backing must load and be empty.** A live-added s3 backing must
  load within 30 seconds, so that wrong settings are reported right away
  instead of leaving a backing that never comes up, and its home must hold
  no gos3 buckets yet. To bring a home that already holds gos3 buckets into
  a server, configure it at startup, where its buckets are adopted.
- **Connection settings and the default kind can change while a backing is
  in use**: the endpoint, region, credentials, addressing, part size, the
  encryption key (adding one, removing it, or re-entering the same one) and
  `default_kind`. gos3 first connects with the new settings and checks the
  `gos3.json` marker and encryption key of the home and of every loaded
  container (and, for a backing without a home, that the service accepts
  the credentials and that up to 20 of its native buckets' remote buckets
  can be reached), and only then switches; on failure the running backing
  is unchanged. This is how credentials are rotated.
- **The home** (bucket and prefix, including adding or removing one) and
  the backend kind define which records the backing holds. Changing them
  replaces the backing, which needs it unused and empty: move its buckets,
  release or delete its native buckets and remove its replicas first
  (`409 Conflict` otherwise). The form locks those fields while the backing
  is in use. An s3 backing that holds no placed bucket can be removed or
  replaced even while its service is unreachable; remote data is left
  untouched.
- **Errors** are reported with the reason: `409 Conflict` when the home is
  in use by another gos3 server; `400` for settings that are wrong rather
  than a passing outage: an encryption key that does not match the
  backing, a `gos3.json` marker that belongs to another store, a home
  prefix holding gos3 data without a marker, a service without conditional
  writes, credentials the service refuses, a missing bucket or a wrong
  region. An endpoint that is this server itself is refused too (`400`):
  its data would be stored back into the same server. A service that does
  not answer at all fails the change with `503` once the 30 seconds are up.

## Choosing the backing of a bucket

- **S3 API**: send the gos3 extension header `x-gos3-backing: <name>` with
  CreateBucket. Without it the bucket goes to `storage.default_backing`. An
  unknown name is `400 InvalidArgument`. On an s3 backing,
  `x-gos3-bucket-kind: native|wrapped` chooses the [kind](#bucket-kinds)
  (default: the backing's `default_kind`), `x-gos3-container: <id>` the
  container of a wrapped bucket and `x-gos3-remote-bucket: <name>` the
  remote bucket a native bucket **creates** (default: the gos3 bucket's
  name; see [s3-compatibility.md](s3-compatibility.md#service-and-bucket-operations)).
  The S3 API never adopts an existing remote bucket. HeadBucket answers
  with `x-gos3-backing` (the backing holding the primary copy) and, on s3
  backings, `x-gos3-bucket-kind`. The default replicas
  (`storage.replication.default_replicas`) are added as usual.

  The AWS CLI cannot add request headers; use curl, an SDK middleware or
  the web UI:

  ```sh
  curl --aws-sigv4 "aws:amz:us-east-1:s3" --user "$AK:$SK" \
    -X PUT -H 'x-gos3-backing: cold' -H 'x-gos3-bucket-kind: wrapped' \
    http://gos3:9000/photos
  ```

- **Web UI**: the *Create bucket* dialog offers the backings when there is
  more than one and, on an s3 backing, the bucket kind with its container
  or remote bucket.
- **Admin API**: `GET /_gos3/admin/v1/whoami` lists the choices for every
  caller (name, kind and, for s3 backings, the bucket kinds offered, the
  default kind and the containers);
  `POST /backings/{name}/buckets` creates a bucket on a chosen kind and
  location, adopts remote buckets and creates containers on the way;
  `GET /_gos3/admin/v1/backings` and the placement endpoints describe and
  change placements (see [admin-api.md](admin-api.md)).

Access control: creating a bucket needs `s3:CreateBucket` whatever the
backing (a native bucket created this way also creates its remote bucket).
Adopting remote buckets and managing containers are admin actions on the
backing (see [Remote bucket browser](#remote-bucket-browser) and
[security.md](security.md#s3-backings)).

## Home and records

An s3 backing keeps a few **records** of its own:

- the registry of its buckets: each gos3 bucket's kind and its container or
  remote bucket (with its region, and whether gos3 created it);
- its containers and which one is the default;
- the bucket configuration of native buckets (policy, CORS, lifecycle,
  tags, ACL, ...), which gos3 keeps instead of setting it on the remote
  bucket;
- the state of multipart uploads to native buckets (small parts are staged
  there until completion) and the overlay records of native objects (see
  [Where gos3 metadata lives](#where-gos3-metadata-lives)).

With a **home** (`home_bucket`, `home_prefix`), the records live in the home,
a gos3 store at that location (see
[How a container stores wrapped buckets](#how-a-container-stores-wrapped-buckets)),
written through its journal. The home is also the backing's **shared
container** (container ID `home`), where wrapped buckets go unless another
container is chosen. Everything about the backing then lives in the S3 account: a new
server configured with the same home finds every bucket again.

Without a home, the records are kept in the server's system directory, in
`<storage.path>/.gos3/s3/<backing>/` (`state.db`, plus `staged/` for small
multipart parts of native buckets; mode 0700, locked while the server
runs, so a second process cannot use them). Such a backing:

- needs `storage.path`;
- creates native buckets by default, and has no shared container: wrapped
  buckets need a container added first (see [Containers](#containers)),
  and `default_kind: wrapped` and default replicas on it are refused;
- is ready as soon as the server starts, without waiting for the service.

Back up that directory with the rest of the system state
([operations.md](operations.md#backups)): it is the only copy of the native
buckets' configuration and of the list of containers. If it is lost,
containers can be attached again and remote buckets adopted again, but the
configuration of native buckets and the metadata kept in overlay records
are gone.

The home bucket must exist and the credentials must be able to read and
write it. The home may share its bucket with other data under other
prefixes, but give it a prefix of its own: a home prefix that holds gos3
data (`meta/`, `data/`, `parts/`, `keys/`, `restic/` or `gateway/` objects)
without a `gos3.json` marker is refused rather than mixed with.

## Bucket kinds

| | Native bucket | Wrapped bucket |
|---|---|---|
| Where the objects live | one remote bucket of its own (the whole bucket); the gos3 name may differ from the remote name | a container (the home, or another remote bucket and prefix), shared with other wrapped buckets |
| How they are stored | as themselves: content headers and user metadata are native; what the remote cannot hold travels in `x-amz-meta-gos3-meta` or gos3's records | gos3's format: a metadata journal and immutable data objects |
| What other S3 tools see | the objects, readable and writable; gos3 sees their changes | opaque data; never change it |
| Versioning, object lock | the remote bucket's, passed through: remote version IDs and delete markers, retention enforced by the remote ([details](#native-buckets-versioning-and-object-lock)); not offered where the service lacks them | gos3's own, on any S3 service |
| Bucket configuration (policy, CORS, lifecycle, tags, ACL, ...) | kept by gos3 in the backing's records; only versioning and object lock are set on the remote bucket | in the container |
| Replicas and moves | no: a native bucket can neither be replicated nor moved, and replicas and move targets are never native | yes, like fs buckets; replicas and move targets on s3 backings are always wrapped buckets |
| Compression | no: data is stored as sent (the bucket's compression setting has no effect) | yes |
| Native AES256, restic | no: AES256 uploads answer `501 NotImplemented` | yes, with the backing's encryption key |
| Deleting the gos3 bucket | must be empty (versions and delete markers count); deletes the remote bucket too only if gos3 created it | must be empty |
| Releasing (gos3 forgets the bucket, the data stays) | yes (admin) | no: move or delete it |
| How it comes to be | CreateBucket creates a new remote bucket; an administrator can adopt an existing one | CreateBucket in a container |

Choose **native** for data that other tools must keep reading and writing,
or to bring existing remote buckets under gos3's IAM, policies, web UI and
discovery. Choose **wrapped** for gos3's full feature set, replication and
moves, encryption, or to keep many small buckets in one remote bucket. A
bucket's kind cannot change; copy its objects into a bucket of the other
kind instead.

The kind of a new bucket is the backing's `default_kind` unless the
request names one: CreateBucket's `x-gos3-bucket-kind` (or a container or
remote bucket, which imply the kind), the web UI's *Create bucket* dialog,
or the admin API. A backing without any container offers native buckets
only.

## Containers

A **container** is a gos3 store (a `gos3.json` marker, a metadata journal
and data objects) at a remote bucket and prefix, holding any number of
wrapped buckets. A backing has three kinds of containers:

| Role | Location | Added |
|---|---|---|
| `home` | the backing's home (container ID `home`) | with `home_bucket` |
| `dedicated` | a new remote bucket that gos3 creates for it | `POST /backings/{name}/containers` with `createBucket`, or **Add container** in the web UI |
| `existing` | a prefix of an existing remote bucket (or the whole bucket) | the same, without `createBucket` |

Containers are listed with their location, state (`loading`, `ready`,
`failed`), bucket count and label in `GET /backings/{name}/containers`,
`GET /backings` and the backing page of the web UI. Labels are chosen by
administrators and shown to every user who can create buckets; do not put
secrets in them.

- **Empty location.** A new container's location must be empty: no key
  under its prefix but a zero-byte "folder" object named like the prefix.
  Emptiness is checked again right before the marker is written, so
  another client writing there meanwhile makes the add fail instead of
  mixing data.
- **No nesting.** The location must not lie inside another gos3 store
  (a `gos3.json` at the bucket's root or at any parent prefix), and must
  not overlap another use of this server: the home or a container (of any
  backing) in the same remote bucket with an equal or nested prefix, or
  the remote bucket of a native bucket, which nothing else may use. Remote
  buckets are compared case-insensitively, per service (endpoint host and
  port): equal names on different services do not conflict.
- **Attaching.** With `attach`, the location must already hold a gos3
  store, for example a container detached from another server; its buckets
  become wrapped buckets of the backing. The store must accept the
  backing's encryption key; a store this backing already uses and the
  metadata prefix of a legacy gateway backing are refused.
- **One server per container.** Every container, the home included, is
  owned by one gos3 server at a time through its lease (see
  [Requirements and limits](#requirements-and-limits)).
- **Loading.** A container loads in the background; adding one waits up to
  30 seconds for it. Until it is loaded its buckets answer
  `503 ServiceUnavailable` and are listed from the backing's records. A
  container that cannot be used (in use by another server, a wrong
  encryption key, its remote bucket deleted or forbidden) only makes its
  own buckets answer `503`: the backing stays healthy.
- **The default container** receives new wrapped buckets, replicas and
  move targets that name none: the home, unless another one was made the
  default (`PUT /backings/{name}/containers/default`, or **Make default**).
- **Removing** a container forgets it: nothing is deleted remotely, and it
  can be attached again later. It must hold no bucket; the home and the
  default container cannot be removed.
- **At most 64 containers** per backing, the home included. Each one is a
  loaded store with its own lease, journal, memory and background work:
  prefer a few containers holding many buckets.

## Remote bucket browser

`GET /_gos3/admin/v1/backings/{name}/remote-buckets` and the **Remote
buckets** tab of the backing's page in the web UI list the buckets of the
account (page by page, with a name prefix filter) and what each is to gos3:

| Status | Meaning | What can be done |
|---|---|---|
| `native` | the remote bucket of a native bucket of this backing | open the gos3 bucket; release it |
| `home` | the backing's home | |
| `container` | holds a container of this backing | |
| `claimed` | used by another backing of this server | |
| `gos3` | holds a gos3 store that no backing of this server uses (found only with `probe=true`, one request per unmanaged bucket) | attach it as a container |
| `inaccessible` | the backing's credentials cannot read it (found with `probe=true`) | |
| `unmanaged` | not used by gos3 | adopt it as a native bucket; add a container in it; create a wrapped bucket in a new container there |

When the credentials may not list the account's buckets, the list is
marked incomplete and shows only the remote buckets the backing knows;
other buckets can still be adopted by name.

- **Adopting** makes an existing remote bucket a native bucket, under its
  own name or another gos3 bucket name (`POST /backings/{name}/buckets`
  with `adopt`, or **Adopt** in the web UI). It is an administrator action
  (`admin:AdoptRemoteBucket` on the backing, plus `admin:CreateBucket` on
  the new bucket), never possible through the S3 API, so that holders of
  `s3:CreateBucket` cannot take over remote buckets. gos3 refuses a remote
  bucket that another use of this server claims, that the credentials
  cannot reach, or that holds a gos3 store: a `gos3.json` at its root or in
  one of its first 100 top-level prefixes (a store nested deeper is not
  found, so do not adopt a bucket holding another server's containers). The
  objects stay as they are; versioning and object lock are read from the
  remote bucket. gos3 never deletes an adopted remote bucket.
- **Creating** a remote bucket: `POST /backings/{name}/remote-buckets`
  (`admin:CreateRemoteBucket`, or **Create remote bucket** in the web UI)
  creates an unmanaged bucket to adopt or put a container in later.
  Creating a native bucket creates its remote bucket directly; an existing
  remote bucket is refused (`409`, `BucketAlreadyExists` on the S3 API:
  an administrator can adopt it). Deleting a native bucket that gos3
  created deletes the then-empty remote bucket too; a failure to delete it
  is logged and leaves it as an unmanaged bucket.
- **Releasing** a native bucket (`POST /buckets/{name}/release`,
  `admin:ReleaseBucket`, or **Release** in the bucket's Danger zone) makes
  gos3 forget it without touching the remote bucket or its objects, even
  when gos3 created it. gos3's multipart uploads in progress on it are
  aborted, and its configuration and overlay records are dropped:
  metadata changed through gos3 after an object was written (an ACL, for
  example) is lost, and adopting the remote bucket again shows each object
  with the metadata it was written with. Wrapped buckets cannot be
  released (`400`): their data would be orphaned.
- **Force delete** (`DELETE /buckets/{name}?force=true`, or the Danger
  zone) of a native bucket deletes the objects, versions and delete
  markers **of its remote bucket**, adopted or not. To stop using a native
  bucket and keep its data, release it instead.

## Native buckets: versioning and object lock

A native bucket's versioning and object lock are those of its remote
bucket:

- **Version IDs and delete markers** are the remote's, passed through
  verbatim (`null` in an unversioned bucket). IDs of other services may be
  longer than gos3's own; the S3 API accepts version IDs of up to 1024
  characters.
- **Versioning**: PutBucketVersioning changes the remote bucket's
  versioning. A versioned bucket cannot become unversioned (as in S3).
- **Object lock**: enabling it on the gos3 bucket enables it on the remote
  bucket, and a default retention (PutObjectLockConfiguration) is mirrored
  to it. Retention and legal hold are written with the remote's object lock
  headers and APIs, so the remote enforces them against every client;
  retain-until dates are rounded up to the millisecond there, and gos3
  keeps reporting the exact date it was given.
- **GOVERNANCE bypass**: gos3 decides first, as for every bucket
  (`x-amz-bypass-governance-retention: true` and
  `s3:BypassGovernanceRetention` for the caller). Only an approved bypass
  is passed on to the remote (`x-amz-bypass-governance-retention` on the
  remote request); the backing's credentials then need
  `s3:BypassGovernanceRetention` too. A refusal of the remote is
  `403 AccessDenied`.
- **Changes by other tools** are seen: gos3 asks the remote on every
  request, and when an answer contradicts what gos3 recorded (a version ID
  from a bucket recorded as unversioned, refused object lock headers), it
  re-reads the remote bucket's versioning and object lock state in the
  background.
- A service that does not support versioning or object lock (a `501`
  answer) or refuses the change (for example enabling object lock on an
  existing bucket where the service allows it only at creation) makes the
  request answer `501 NotImplemented`. gos3 remembers which features the
  remote bucket offers.

### Provider differences

| Service | Versioning | Object lock | Notes |
|---|---|---|---|
| AWS S3 | ✅ | ✅, at creation or later on a versioned bucket | AWS answers 204 to deleting an unknown version ID; gos3 looks a version up before deleting it, so it can tell. |
| MinIO | ✅ | ✅ only when the remote bucket is created with it (`x-amz-bucket-object-lock-enabled` on CreateBucket, or **Object lock** when creating it in the web UI); enabling it later answers `501` | |
| Cloudflare R2 | ❌: native buckets stay unversioned, PutBucketVersioning answers `501` | ❌ | |
| Backblaze B2 | buckets always keep versions | as the service allows | version IDs are much longer than gos3's own |
| gos3 | ✅ | ✅ | |

### Where gos3 metadata lives

A native object carries as much of its gos3 metadata as the remote can
hold natively, so other S3 clients see it:

- content headers (`Content-Type`, `-Encoding`, `-Disposition`,
  `-Language`, `Cache-Control`, `Expires`) and user metadata, when they
  survive any S3 service unchanged (printable ASCII);
- tags (`x-amz-tagging`), when they are valid AWS tags and the remote
  supports tagging;
- retention and legal hold, on buckets with object lock.

Other fields the remote cannot hold (checksums, part sizes, an ETag that
differs from the remote one, SSE and storage class settings; values that
cannot be native) travel in one user metadata entry,
`x-amz-meta-gos3-meta`, which is left out when there is nothing to put in
it. What does not fit into S3's 2 KiB of user metadata, and every change
made after the object was written (tags, metadata edits), is kept in an
**overlay record** in the backing's records, never by copying the object
onto itself: gos3 creates no remote version for a metadata change, and
LastModified never moves. An overlay applies only to the remote version it
was made for (the version ID and remote ETag, and for `null` versions the
LastModified second): when another tool replaces the object, the new
object does not inherit it. Overlays of versions removed outside gos3 are
dropped when gos3 next looks the version up or lists it, and by a daily
sweep.

An object's **ACL and website redirect** are kept in overlay records only,
written once the remote has acknowledged the object, and never in the
object's own headers: an `x-amz-meta-gos3-meta` entry written, copied or
restored by another tool never makes an object public or redirects it,
and an ACL revoked through gos3 stays revoked when another tool restores an
older version (see [security.md](security.md#s3-backings)). Losing the
backing's records therefore makes native objects private and drops their
redirects.

Writes send `Content-MD5`. `If-None-Match: *` is passed to the remote;
`If-Match` is checked under a per-object lock and pinned to the remote
ETag. Conditional writes and multipart completions are sent once: when
their outcome is unknown (a `5xx` or transport error), gos3 reads the key
back and compares its ETag before answering. LastModified is reported in
whole seconds, so HEAD and listings agree, and listings report the same
ETag as HEAD.

### Permissions of the backing's credentials

What the credentials must be allowed to do on the remote service, in AWS
IAM terms (other services have their own equivalents):

| Use | Actions |
|---|---|
| Home and containers (their bucket and prefix; restic uses the same) | `s3:ListBucket`, `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` |
| Every native bucket | `s3:ListBucket`, `s3:ListBucketVersions`, `s3:GetBucketLocation`, `s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration`, `s3:GetObject`, `s3:GetObjectVersion`, `s3:PutObject`, `s3:DeleteObject`, `s3:DeleteObjectVersion`, `s3:GetObjectTagging`, `s3:GetObjectVersionTagging`, `s3:PutObjectTagging`, `s3:PutObjectVersionTagging`, `s3:DeleteObjectTagging`, `s3:DeleteObjectVersionTagging`, `s3:AbortMultipartUpload` |
| Native buckets with object lock | `s3:GetObjectRetention`, `s3:GetObjectLegalHold` (AWS returns lock headers on HEAD and GET only with them), `s3:PutObjectRetention`, `s3:PutObjectLegalHold`, `s3:BypassGovernanceRetention` |
| Changing versioning or object lock through gos3 | `s3:PutBucketVersioning`, `s3:PutBucketObjectLockConfiguration` |
| Creating native buckets, dedicated containers and remote buckets; deleting native buckets gos3 created | `s3:CreateBucket` (with object lock also `s3:PutBucketVersioning` and `s3:PutBucketObjectLockConfiguration`), `s3:DeleteBucket` |
| Remote bucket browser | `s3:ListAllMyBuckets` (optional: without it the list is incomplete and buckets are adopted by name) |

Grant them on exactly the remote buckets the backing uses. Bucket-level
refusals (`403` on the bucket, a deleted bucket) make that bucket answer
`503`; object-level refusals answer `403 AccessDenied` (see
[Behaviour when the remote is unreachable](#behaviour-when-the-remote-is-unreachable)).

### Limits of native buckets

- Listings carry no tags or user metadata (gos3 does not send a HEAD per
  listed object), so lifecycle rules filtering on tags never match native
  objects.
- HEAD and GET of a tagged object cost one more request
  (GetObjectTagging).
- Multipart uploads: parts of 5 MiB or more go straight to a remote
  multipart upload; smaller parts are staged in the backing's records
  until completion (an upload made only of small parts is written as one
  object). After a remote completion, what the remote could not take (the
  gos3 ETag, checksums, part sizes) is kept in an overlay record only.
- An overlay on a `null` version is inherited by a replacement written by
  another tool within the same second with identical content (gos3 cannot
  tell them apart).
- Conditional writes are atomic against other users of this server and,
  where the remote honours `If-None-Match` / `If-Match`, against everyone.
  Conditional deletes (`If-Match` on DeleteObject, object lock checks) are
  atomic only within one gos3 process.
- A reader fails, rather than mixing versions, when another client
  replaces an unversioned object while it is being read.
- Several gos3 servers can adopt the same remote bucket. To each, the
  other is just another S3 client: overlay records and multipart uploads
  are per server.
- No compression, AES256 or restic, no replicas and no moves (see
  [Bucket kinds](#bucket-kinds)).

## How a container stores wrapped buckets

**Metadata** is kept in an in-memory index (the same one the memory
backend uses) and made durable in the container as a **journal**: every
change (a new version, a delete, a tag update, a bucket configuration) is
acknowledged only after the journal batch holding it was written.
Concurrent changes share batches, so a write costs its data upload plus,
typically, one small journal PUT. At startup the server loads the newest
**snapshot** of the index and replays the journal written after it.
Snapshots are written in the background when enough journal has
accumulated (64 MiB or 10,000 batches) or every 6 hours while there are
changes, and at a clean shutdown; the two newest are kept and older
journal batches are deleted. The home's journal also holds the backing's
records.

**Object data** is stored in immutable objects under `data/`, as stored
by gos3 (compressed and/or encrypted when the bucket is). Uploads are
spooled to a local temporary file, then uploaded (in parallel parts when
larger than `part_size`) before the metadata change is journaled, so a
crash never publishes a partial object. CompleteMultipartUpload assembles
the parts server-side (UploadPartCopy) when the result is their plain
concatenation (no compression or encryption, parts of at least 5 MiB),
so no data flows back through gos3. Readers fetch ranges on demand;
superseded or deleted data is removed after the change is journaled, but
only once no reader has it open.

Every data object, part and staged part carries the user metadata
`x-amz-meta-gos3-store: <store ID>`. A background pass (hourly, the first
within 5 minutes of loading) deletes objects that no metadata references
(left by a crash) once they are older than 24 hours, and only when their
name is one gos3 generates **and** they carry this store's ID. Untagged
objects (another tool's data, or data written by builds before the tag
existed) and objects of another store are never deleted, only counted in
the debug log.

### What is stored where

Every key is under `<prefix>` in the container's bucket:

| Key | Content |
|---|---|
| `gos3.json` | marker: identifies the prefix as a gos3 store (format, store ID, role `home` or `container`, whether data objects are tagged, a check value of the encryption key; on the home of an earlier gateway backing, a gateway flag until it is migrated) |
| `meta/lease.json` | the owner lease of the running server |
| `meta/journal/<epoch>/<seq>` | journal batches (metadata changes; in the home, also the backing's records) |
| `meta/snapshots/<lsn>-<epoch>` | snapshots of the metadata index |
| `data/<bucket-id>/<data-id>` | object data |
| `parts/<upload-id>/...` | parts of multipart uploads in progress |
| `keys/<bucket-id>.key` | the bucket's AES master key, wrapped (encrypted) with the encryption key |
| `keys/<bucket-id>.restic.json` | the bucket's restic recovery record (repository location; no password) |
| `restic/<bucket-id>/` | the bucket's restic repository |
| `gateway/parts/<namespace>/...` | home only: small parts of multipart uploads to native buckets, staged until completion |

Do not change or delete these objects with other tools, and do not put
other data under the prefix. gos3 refuses to open a prefix that holds
`meta/`, `data/`, `parts/`, `keys/`, `restic/` or `gateway/` objects but no
`gos3.json`, rather than mixing data.

### Requirements and limits

- **Conditional writes.** The journal, the lease and the keys rely on
  `If-None-Match: *` and `If-Match` on PUT. AWS S3, MinIO, Cloudflare R2
  and gos3 support them. Before creating a store gos3 probes the service
  and refuses one that ignores them ("the S3 service does not support
  conditional writes"), because it could not keep two servers from
  overwriting each other's metadata. Native buckets need no conditional
  writes, but without them conditional requests are only atomic within
  gos3.
- **One server per home and per container.** The running server holds a
  lease (`meta/lease.json`), renewed every 10 seconds. A second server
  opening the same prefix is refused with "prefix in use by another
  server". After a crash, the next start waits until the old lease is 30
  seconds old (by the S3 service's clock) and then takes over; a clean
  shutdown releases the lease at once. If an old server still runs after
  its lease was taken over (for example after a long network partition),
  its next journal write fails and it stops serving the store, so it
  cannot overwrite the new owner's changes. A server that cannot renew its
  lease stops serving the store before the lease can expire, and reloads
  it when the service answers again.
- **Memory and startup time.** The whole metadata index of a container is
  held in memory and loaded at startup, like the memory backend's. The
  intended envelope is up to a few million object versions per container;
  beyond that, use several containers (or backings), or fs. Startup time
  grows with the snapshot size and the journal written since.
- **Latency.** Every write waits for its data upload and its journal batch,
  and reads of data go to the service, so latency follows the service.

## Behaviour when the remote is unreachable

At startup the server waits up to 15 seconds for its s3 backings to load
(they load concurrently), so a reachable service is healthy from the first
request. A backing with a home is loaded once its home is; a backing
without a home is ready at once, since its records are local. A backing
that has not loaded by then does not stop the server: it starts unhealthy
and keeps loading in the background. Only that backing's buckets are
affected: they answer `503 ServiceUnavailable` until the backing is
loaded, the backing is marked unhealthy (with the last error on the
Storage page and in `GET /_gos3/admin/v1/backings`), and every other bucket
is listed and served normally. When the service answers, the backing
loads, becomes healthy at the next health check, and buckets found on it
that have no placement yet are adopted. The same applies while a running
backing loses its service.

A backing with a home is healthy while its home is; one without a home
while its service answers. Failures that concern **one bucket** do not
mark the backing unhealthy:

- a native bucket whose remote bucket was deleted, is forbidden to the
  credentials (`403` on the bucket), lives in another region than
  recorded, or keeps throttling;
- a wrapped bucket whose container is not loaded yet or cannot be used (in
  use by another server, a wrong encryption key, its bucket deleted or
  forbidden).

Such a bucket answers `503 ServiceUnavailable`; the backing's other buckets
are served, and a replicated bucket reads from another copy. Transport
errors and `5xx` answers of the service count against the whole backing.
An object-level refusal of the remote (`403` on an object, for example
object lock) answers `403 AccessDenied`, and any other refusal
`500 InternalError` naming at most the remote's error code; the full
remote error is logged with the request ID, never sent to clients.

While the home is unreachable, the home's own wrapped buckets answer
`503`, and so does every change to the backing's records: creating and
deleting buckets, the configuration of native buckets and multipart
uploads to native buckets. Native buckets and other containers keep their
data elsewhere.

Until a backing that could not be listed at startup has been listed, gos3
cannot know which buckets it holds. Meanwhile requests for a bucket name
the server does not know answer `503 ServiceUnavailable` instead of
`404 NoSuchBucket`, and CreateBucket of a new name is refused with `503`,
so that a bucket on the missing backing is neither reported absent nor
created a second time. Admin API requests that reach an unavailable
backing also answer `503`.

`GET /_gos3/ready` reports ready while the bucket listing answers and the
**default** backing is healthy: an s3 default backing that is unreachable
makes the server not ready, while an unavailable additional backing does
not (its buckets are the only ones affected).

Some errors are not cured by waiting and need a configuration change: a
home or container in use by another server, an encryption key that does
not match, a service without conditional writes, a prefix with gos3 data
but no marker, or a damaged journal. The home's errors keep the whole
backing unavailable; a container's only its buckets. The error is
reported on the Storage page and the backing's page.

## Encryption on s3 backings

Native AES256 and restic work on the **wrapped** buckets of s3 backings,
and both need the backing's **encryption key** (`encryption_key`, the KEK),
which every container of the backing uses. Each bucket's random AES master
key is stored in its container wrapped with the KEK (AES-256-GCM), so the S3
service never holds a usable key; restic repository passwords are derived
from the master key. **Native buckets** store objects as they are: neither
AES256 nor restic is offered for them, and AES256 uploads (explicit, or
through the bucket's default encryption) answer `501 NotImplemented`.

- Without an encryption key the backing offers neither AES256 nor restic:
  the web UI marks them unavailable, and AES256 uploads to a bucket with
  any copy on it (primary, replica or move target) are refused with
  `501 NotImplemented`. Adding a replica or moving a bucket to it is
  refused when the bucket uses AES256: as its provider, through its
  default encryption, or because it already holds AES256 versions.
- The KEK can be added later, also live on the Storage page. It cannot be
  changed: the marker of the home and of every container and every wrapped
  key record a check value of the KEK, and a different key is refused
  ("encryption key does not match this backing") so that existing keys are
  not orphaned. A KEK added live is checked against the keys the backing
  already holds, and its check value is written to `gos3.json` when the
  store is next loaded (for example at the next start). Attaching a
  container whose store records another key is refused.
- **Losing the KEK loses every encrypted object of the backing.** gos3
  never regenerates a missing key. Keep the KEK somewhere safe, apart from
  the remote buckets (a password manager, a secret store).
- Restic repositories of a wrapped bucket live in its container at
  `<prefix>restic/<bucket-id>`, using restic's s3 backend with the
  backing's current endpoint, credentials and addressing, and the
  container's region. The repository password derives from the bucket's
  master key, so the recovery record `<prefix>keys/<bucket-id>.restic.json`
  holds the repository location only. Restic needs the KEK, the backing's
  credentials, and the restic executable on the gos3 server (see
  [encryption.md](encryption.md)).
- Local files: each s3 backing gets its own temporary directory under the
  operating system's temporary directory (`gos3-<backing>-*`), removed at
  shutdown. It holds upload spool files, restic's cache and restores of
  restic objects being read, so it needs room for the largest object being
  uploaded or read. It is never the data directory.

## Migration from earlier development builds

Development builds before account connections configured an s3 backing
with `bucket`, `prefix` and `gateway`. These keys keep working (see
[`s3` settings](#s3-settings)), and the backing's data is taken over in
place the first time this version loads it; nothing is copied:

- **A plain s3 backing** (`bucket` and `prefix`) becomes the backing's
  **home**: its buckets become **wrapped buckets** in the shared container,
  with their data, versions, keys and restic repositories where they were.
  The default kind stays `wrapped`.
- **A gateway backing** (`gateway: true`) becomes a backing with a home
  (the former metadata bucket and prefix) and the default kind `native`.
  Each of its buckets becomes a **native bucket** bound to the remote bucket
  of its name, as adopted, so gos3 never deletes that remote bucket. Bucket
  configuration and in-progress multipart uploads are kept, and objects
  written by the gateway (including its `x-amz-meta-gos3-meta` entries) read
  as before, except for two fields: an object ACL or website redirect kept
  in that header is ignored, because gos3 now keeps these only in its own
  records (see [security.md](security.md#s3-backings)); such objects read
  as private without a redirect until their ACL is set again through gos3. Once every
  bucket is migrated, the gateway flag of the home's
  marker is cleared. A bucket that cannot be migrated (its name is taken by
  another bucket of the backing) stays where it was and is logged as an
  error; the migration is retried at every load.

Each step is recorded as it completes, so an interrupted migration is
finished at the next start. Two behaviours change for former gateway
backings: creating a bucket whose remote bucket already exists is refused
(`409 BucketAlreadyExists`; an administrator adopts it instead), and
versioning and object lock are offered where the remote service has them.

Going back is limited. An earlier build can still open a migrated plain
backing's home and serve its buckets, but it knows nothing about native
buckets, other containers or the records of this version, so buckets
created since the upgrade outside the home are invisible to it. A migrated
gateway backing cannot be used by an earlier build at all (its marker no
longer says gateway). Back up the home's prefix (see [Backups](#backups))
before upgrading if you may need to downgrade.

## Backups

The data of an s3 backing is in its S3 account: the home and every
container hold their wrapped buckets completely (metadata, data, wrapped
keys and restic repositories; the home also holds the backing's records),
and native buckets are ordinary remote buckets. Protect them with the
service's own means: bucket versioning (so an accidental delete or
overwrite of a journal, snapshot or data object can be undone), replication
to another region or service, or a periodic copy of a container's whole
prefix taken while gos3 is stopped. A copy of a running container can be
inconsistent (a snapshot without the data it refers to). Native buckets
hold ordinary objects that any S3 tool can back up; gos3 keeps their
configuration, overlay records and in-progress multipart uploads in the
backing's records.

Also back up, separately:

- the **encryption key** of every s3 backing that has one: without it the
  encrypted objects and restic archives cannot be read;
- the system state in `<storage.path>/.gos3/` (`iam.json`,
  `settings.json`, `placements.json`, `backings.json`, ...), as described
  in [operations.md](operations.md#backups). `backings.json` contains the
  s3 credentials and encryption keys entered in the web UI; treat its
  backup as secret. For a backing without a home this includes its records
  in `.gos3/s3/<backing>/`.

## Removing or renaming a backing

A backing that holds buckets cannot be removed while the server runs; move
its wrapped buckets and release or delete its native buckets first.
Removing an s3 backing from the configuration makes gos3 forget its
buckets at the next start (it refuses to start while a bucket's only copy
was there; see
[operations.md](operations.md#multiple-backings-and-replication)). Remote
data is never deleted: configuring the same home again, under any backing
name, brings its buckets back, because the home is identified by its
`gos3.json` marker and holds the records. Containers can be attached again
and native remote buckets adopted again by any backing. A home or container
can only be used by one backing at a time.
