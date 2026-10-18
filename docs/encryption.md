# Object encryption and restic

Bucket settings → **Encryption** selects native **SSE-S3 (AES256)** or **Restic**.
Both encrypt new object data. S3 clients continue to use `AES256`; restic is a
gos3 implementation of SSE-S3. An explicit AES256 upload uses the bucket's
provider even when default encryption is disabled.

Restic also serves as a backup. A bucket can archive **every** write in
its restic repository, not only the writes that ask for encryption (the
[archive toggle](#archiving-every-write)), and any bucket, native buckets
included, can be snapshotted as a whole on a schedule
([whole-bucket snapshots](#whole-bucket-snapshots)). Both can be read
with the plain `restic` tool, without a running gos3
([recovery without gos3](#recovery-without-gos3)). The web UI gathers this
in Bucket settings → **Backup & archive**.

Storage follows placement. The bucket stores only its encryption provider.
Each backing holding a copy of the bucket keeps that copy's keys and restic
repository itself: an fs backing in its data directory, an s3 backing in the
container of the (wrapped) bucket. A replica or move target uses its own backing's keys and
repository; nothing secret replicates with the bucket configuration.

Native encryption uses gos3's authenticated AES-256-GCM stream format
(`internal/storage/crypt`): 64 KiB chunks, each sealed with a nonce made of
its index and a final-chunk flag, so reordered, truncated or extended data
fails authentication. HKDF-SHA256 derives a separate key for each immutable
data file from a random bucket master key, so every stream has its own key
and swapping objects' ciphertext fails authentication. Compression happens
before encryption; range reads decrypt only the chunks they need. Upload
staging and multipart parts are encrypted too, on fs and on
[s3 backings](backings.md). The memory backend records AES256 for SSE
responses and replication but keeps its in-process data as is.

AWS KMS (`aws:kms` / `aws:kms:dsse`) and SSE-C requests return
`501 NotImplemented`. A KMS bucket default stored by an older build is applied
as SSE-S3 (AES256), with a logged warning, as are multipart uploads that
requested KMS before the upgrade; save the bucket's default encryption again
to replace it.

Encrypted versions use a newer metadata record format that gos3 versions
without object encryption cannot read; see
[operations.md](operations.md#upgrades) before rolling back.

## Existing objects

Earlier gos3 versions recorded SSE settings without encrypting data. Those
objects remain readable and no longer advertise encryption. A setting change
does not rewrite existing versions. Upload or copy with AES256 to create an
encrypted version; older versions remain subject to normal deletion and Object
Lock rules. Disabling encryption or switching provider preserves encrypted reads.

## Where keys and repositories live

What a bucket's encryption needs depends on the backend of each backing
that holds a copy of it (primary, replicas and a move target):

| Backing | Native AES256 | Restic repository |
|---|---|---|
| fs | master key in `<path>/.gos3/keys/<bucket-id>` | local, `<path>/.gos3/restic/<bucket-id>` |
| s3, wrapped bucket | master key in the bucket's container at `<prefix>keys/<bucket-id>.key`, wrapped with the backing's `encryption_key`; **unavailable without one** | restic's s3 backend in the container's bucket, `<prefix>restic/<bucket-id>`, with the backing's current endpoint and credentials and the container's region; needs the `encryption_key` and the credentials; recovery record `<prefix>keys/<bucket-id>.restic.json` without the password, which derives from the wrapped master key |
| s3, native bucket | not offered (objects are stored as they are) | not offered |
| memory | recorded, data kept in memory as is | not offered |

Buckets without a repository of their own (native and memory buckets)
can still have [whole-bucket snapshots](#whole-bucket-snapshots), in an
**aux repository** that a backing keeps for them: an fs backing in
`<path>/.gos3/restic-aux/<name>`, an s3 backing with a home and an
encryption key in the home at `<prefix>restic-aux/<name>` (see
[Snapshot repositories](#snapshot-repositories)).

Restic needs the [restic](https://restic.readthedocs.io/en/stable/020_installation.html)
executable, **0.17.1 or newer**, on the gos3 server's PATH. The gos3
container images include a pinned, checksum-verified restic at
`/usr/local/bin/restic` ([deployment.md](deployment.md#docker)); for the
release archives and the systemd unit, install it yourself. Native AES256
needs no extra executable.

A provider is offered for a bucket only when every backing holding a copy
can store it: the web UI's Encryption tab marks the others unavailable, and
the admin API's `available` list leaves them out. AES256 writes (and
preparing AES256 for the bucket) are refused with `501 NotImplemented`
while any copy of the bucket, a replica or move target included, is on a
backing that cannot store AES256, such as an s3 backing without an
encryption key; on a native bucket AES256 writes are always refused. For the same reason a bucket that uses a provider (as its
provider, through its default encryption, or because it holds versions
encrypted with it) cannot be replicated or moved to a backing that cannot
store it, for example restic data to a memory backing, or AES256 or restic
data to an s3 backing without an encryption key.

Each physical bucket copy has its own ID and key, including replicas.
Restic passwords are derived from that key and managed by the server.
Bucket encryption has no repository URL, password, or credential fields: the
repository location follows the backing, and is fixed for as long as the
backing exists. To keep a bucket's archive elsewhere, move the bucket to
another backing; existing archives stay where they were created.

An s3 backing may be a bucket on another gos3 server, but not one whose
provider is restic: gos3 rejects restic's own uploads (recognized by its
`gos3-restic` user agent) to a bucket that would archive them through restic
again. An s3 backing can never point at the server itself. HTTPS protects
transport; HTTP suits trusted local endpoints.

Bucket provider selection uses scoped `admin:GetBucketEncryption` /
`admin:PutBucketEncryption`; standard S3 permissions separately govern
enabling or disabling bucket-default encryption. Backing settings, the
encryption key included, are read with `admin:GetConfig` and changed with
`admin:UpdateConfig` (see [admin-api.md](admin-api.md#storage-backings-and-bucket-placement)).

### The encryption key of s3 backings

An s3 backing stores each wrapped bucket's random master key in the bucket's
container, and the key of each aux repository in its home, encrypted
(AES-256-GCM) with the backing's **key-encryption key** (one per backing,
used by all its containers):
`encryption_key` in its `s3` settings, `GOS3_S3_ENCRYPTION_KEY` for the
default backing or `GOS3_BACKING_<NAME>_ENCRYPTION_KEY` for others, or the
**Encryption key** field (with **Generate**) under **Advanced** in the
Storage page's backing form. It is standard base64 of exactly 32 bytes:

```sh
openssl rand -base64 32
```

- The S3 service never sees a usable key; reading the remote buckets alone
  does not decrypt the data.
- A backing without the key offers neither AES256 nor restic. The key can
  be added later (also live on the Storage page, where it is checked
  against the keys the backing already holds), but not changed: a
  different key is refused, because the existing master keys could no
  longer be opened.
- **Losing the key loses every encrypted object and restic archive of the
  backing.** gos3 never regenerates a missing key. Keep it outside the
  remote buckets (a password manager or secret store), and back it up with
  the rest of the server's secrets.

See [backings.md](backings.md#encryption-on-s3-backings).

## Archive behavior

Each non-empty object becomes a restic snapshot containing `/object`,
published only after backup succeeds; empty objects need no snapshot.
Uploads stream into `restic backup` while they are received, and the
snapshot is saved only after the write's `If-None-Match`/`If-Match`
conditions (and, for multipart completion, its parts) are checked again, so
aborted or refused writes leave no snapshot. A write refused later in the
commit, or racing another write, can still leave an unreferenced snapshot.
Each snapshot has the host `gos3` and the tags `gos3-object`,
`gos3-bucket:<base64url of the bucket name>`,
`gos3-key:<base64url of the object key>` and `gos3-bucket-id:<bucket ID>`
(unpadded URL-safe base64; snapshots written by earlier versions lack the
bucket ID tag). The tags are stored encrypted in the repository like
everything else. Restic handles
compression and deduplication; gos3 compression is not applied to restic
objects. Restic 0.17.1 or newer is checked on first use.

GET restores the whole object into an encrypted temporary file on the first
read, then serves it, including range requests; requests that send no data
(HEAD, `304`, `412`, `416`) restore nothing, and concurrent readers of the
same version share one restore. The file is removed with its last reader
and crash leftovers are cleared at startup. This requires disk space for the
whole object and adds repository latency. Errors fail the request without
plaintext fallback; they concern the bucket only and do not mark its backing
unhealthy. Restic's diagnostics appear (redacted) in the server log.

Restic keeps a cache, on fs backings in `.gos3/restic-cache/` and on s3
backings in the backing's temporary directory under the operating system's
temporary directory (removed at shutdown); it is managed by restic and safe
to delete. Every object adds a snapshot
and an index file, so run `restic prune` on each repository periodically to
consolidate its index (it forgets nothing; never run `restic forget` on
`gos3-object` snapshots, see [below](#never-forget-gos3-object-snapshots)),
or let gos3 prune it when the bucket's
[whole-bucket snapshots](#whole-bucket-snapshots) go to the same
repository (target `bucket`): `POST /buckets/{name}/restic/prune`, or the
schedule's prune setting. Prune
takes an exclusive lock: writes wait up to 30 seconds for it, and reads,
which do not lock, can fail while it repacks. Overwrites and deletes retain
snapshots: the repository keeps every version ever archived, also after
the version was deleted through S3 or by a lifecycle rule. gos3 never
forgets per-version snapshots.

Stored-byte usage metrics count restic versions at their content size, because
repository packs are deduplicated, compressed, and shared between snapshots
and cannot be attributed to single versions. Use restic/provider statistics to
measure actual archive usage.

## Archiving every write

The restic provider has a **scope**:

| Scope | Archived in the bucket's repository |
|---|---|
| `sse` (default) | the writes that ask for SSE-S3: through the bucket's default encryption or an `x-amz-server-side-encryption: AES256` header |
| `all` | **every** write: each new version of every object becomes a restic snapshot, whatever the request asks for |

`all` is the per-version archive toggle: Bucket settings → **Backup &
archive** → Archive in the web UI, or
`PUT /buckets/{name}/encryption-provider` with
`{"provider": "restic", "scope": "all"}`
([admin-api.md](admin-api.md#object-encryption-providers)). It needs what
the restic provider needs: the restic executable, and every copy of the
bucket on an fs backing or in a wrapped bucket of an s3 backing with an
encryption key. Native and memory buckets cannot archive their versions;
use [whole-bucket snapshots](#whole-bucket-snapshots) for them.

- It applies to new writes (PutObject, CopyObject, PostObject; a multipart
  upload created without SSE is decided when it completes). Existing
  versions stay as they are.
- Versions archived only because of the scope are reported to clients
  without server-side encryption (`x-amz-server-side-encryption` is
  absent, as the client did not ask for it), but they are stored like any
  restic version: encrypted in the repository, restored on read.
- **Cost**: every write runs a `restic backup` (a process, a snapshot and
  an index file per version), so writes take noticeably longer and the
  repository grows by one snapshot per version; every first read of a
  version restores it into a temporary file (see
  [Archive behavior](#archive-behavior)). Deleted and overwritten versions
  stay in the repository. The scope suits buckets whose history matters
  more than their write rate (documents, configuration, exports), not
  buckets with many small writes per second. Prune the repository from
  time to time; prune locks writes while it runs.
- Builds before the scope existed ignore it: after a rollback, writes
  without SSE are stored unencrypted again, while the versions archived so
  far stay readable (see [operations.md](operations.md#upgrades)).

## Keys and recovery

Back up metadata, keys and data/repositories together
([operations.md](operations.md#backups)):

- **fs backings**: a file-level backup of the backing's data directory
  includes everything. Native keys are `.gos3/keys/<bucket-id>`, restic
  recovery records (repository location and password)
  `.gos3/keys/<bucket-id>.restic.json`, local repositories
  `.gos3/restic/<bucket-id>`; aux repositories are
  `.gos3/restic-aux/<name>`, with their keys and records in
  `.gos3/keys/aux/`.
- **s3 backings**: the wrapped keys (`<prefix>keys/`), recovery records
  (without passwords) and repositories (`<prefix>restic/`) of wrapped
  buckets are in their container with the data, and the aux repositories
  (`<prefix>restic-aux/`) with their wrapped keys (`<prefix>keys/aux/`) in
  the home; back up the home and container prefixes and, separately, the
  backing's **encryption key** (see [backings.md](backings.md#backups)).

Keys, recovery records and repositories remain after the bucket is
deleted. Per-version archive snapshots retain object identity (bucket and
key) but do not replace gos3 metadata for versions, tags, permissions, and
retention; [whole-bucket snapshots](#whole-bucket-snapshots) carry that
metadata in their manifest.

Missing native keys fail closed and are not silently regenerated. On fs
backings the key directory gets private filesystem permissions and a protected Windows ACL
before a key is created (failing that fails key creation); at startup existing
key and recovery files are made private again, best effort with a logged
warning, e.g. after a restore that did not keep permissions.
Server-managed keys allow unattended access; a party holding keys and ciphertext
(for an s3 backing: its encryption key and the remote bucket) can decrypt
data. This feature does not protect against a compromised gos3 process or a
full copy of its keys and data. S3 metadata itself is not encrypted: on an
s3 backing, bucket names, object keys, user metadata and tags of wrapped
buckets are readable in their container's journal and snapshots by anyone
who can read the container, and native buckets are not encrypted by gos3 at
all.

## Recovery without gos3

Every repository gos3 writes is an ordinary restic repository. With its
location and password, the `restic` command (0.17.1 or newer) reads it on
any machine, whether gos3 runs or not.

### 1. Find the repository and its password

| Way | Needs | Gives |
|---|---|---|
| Web UI: Bucket settings → **Backup & archive** → Recovery | `admin:GetBucketEncryption`; **Reveal credentials** also `admin:GetResticCredentials` | the repository of each copy of the bucket, its recovery record and where the password comes from; on reveal the password and environment, as bash and PowerShell snippets |
| `GET /buckets/{name}/restic` | `admin:GetBucketEncryption` | the same information without secrets (`ResticInfo`, [admin-api.md](admin-api.md#object-encryption-providers)), plus the snapshot repository and the tags gos3 uses |
| `POST /buckets/{name}/restic/credentials` | `admin:GetResticCredentials` | the repository, password, environment (for s3 backings the backing's `AWS_*` credentials) and `-o` options of one copy (`backing`), for the archive or the snapshots (`purpose`). Every disclosure is logged |
| `gos3 restic env --server <url> --bucket <name>` | a running server and an access key allowed `admin:GetResticCredentials` | shell statements setting `RESTIC_REPOSITORY`, `RESTIC_PASSWORD` and the rest, through the endpoint above; `--shell bash\|powershell\|json`, `--snapshots` for the snapshot repository |
| `gos3 restic env --config <file> --bucket <name>` | the server **stopped** (or gone), its configuration (a file, `--data <dir>`, or `--offline` for the `GOS3_*` variables alone) and access to its backings | the same, read from the backings directly: on fs backings from the recovery records (the metadata database is opened read-only to find the bucket's ID); on s3 backings by listing the recovery records of the home (or, without a home, of the container its local records name) and unwrapping the bucket's key with the configured `encryption_key` (see [below](#the-gos3-restic-command)) |
| By hand | the files | fs backings: the recovery record `.gos3/keys/<bucket-id>.restic.json` holds `repository`, `password` and the bucket's name (`bucket`). s3 backings: the password is the hex encoding of the bucket's master key `<prefix>keys/<bucket-id>.key` once unwrapped with the backing's encryption key, which `gos3 restic env` does for you |

An s3 repository also needs the backing's credentials (`AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`) and options such as `-o s3.bucket-lookup=path`
on every restic command line; restic reads no options from the
environment (`gos3 restic env` prints them in a comment).
`gos3 restic exec --bucket <name> ... -- <restic arguments>` runs restic
with everything set. Without the backing's **encryption key** the archives
of an s3 backing cannot be decrypted. See
[The `gos3 restic` command](#the-gos3-restic-command) for the options that
select the server and the credentials.

### 2. List the archived versions

```sh
eval "$(gos3 restic env --server https://s3.example.com --bucket photos)"   # bash; or set the variables by hand
restic snapshots --tag gos3-object,gos3-bucket:cGhvdG9z
```

One `--tag` with commas selects snapshots that have all of the tags.
`cGhvdG9z` is the bucket name in unpadded base64url:
`printf %s photos | base64 | tr '+/' '-_' | tr -d '='`. Each snapshot is
one archived version; its time is when it was archived and its
`gos3-key:` tag names the object's key. `gos3 restic objects --bucket
photos ...` lists them with the keys decoded. To decode a tag by hand:

```sh
python3 -c 'import base64,sys; s=sys.argv[1]; print(base64.urlsafe_b64decode(s+"="*(-len(s)%4)).decode())' cGhvdG9zL2NhdC5qcGc
# photos/cat.jpg
```

`--tag gos3-object,gos3-key:<base64url of the key>` lists the versions of
one key.

### 3. Restore a version

```sh
restic dump <snapshot-id> /object > cat.jpg
gos3 restic recover --config /etc/gos3/gos3.yaml --bucket photos --key photos/cat.jpg --at 2026-10-01T12:00:00Z -o cat.jpg
```

`gos3 restic recover` picks the newest version of the key archived at or
before `--at` (default: the newest). What comes back is the content: the
version ID, content type, user metadata, tags and whether the version was
later deleted are gos3 metadata, which the per-version archive does not
hold. Empty objects are never archived, so they cannot be recovered.

### The `gos3 restic` command

```
gos3 restic env     --bucket NAME [--snapshots] [--shell bash|powershell|json] SOURCE
gos3 restic exec    --bucket NAME [--snapshots] SOURCE -- RESTIC-ARGUMENTS...
gos3 restic objects --bucket NAME [--key KEY | --prefix PREFIX] [--json] SOURCE
gos3 restic recover --bucket NAME --key KEY [--at TIME] [-o FILE] SOURCE
```

| Subcommand | Does |
|---|---|
| `env` | prints statements setting `RESTIC_REPOSITORY`, `RESTIC_PASSWORD` and, for s3 repositories, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_DEFAULT_REGION`: `export NAME='value'` for bash (the default: `eval "$(gos3 restic env ...)"`), `$env:NAME = 'value'` for PowerShell (`gos3 restic env ... --shell powershell \| Out-String \| Invoke-Expression`), or with `--shell json` an object `{"bucket", "backing", "purpose", "env": {...}, "options": [...]}`. The `-o` options restic needs are listed in a comment |
| `exec` | runs `restic` (from `PATH`, 0.17.1 or newer) with that environment, `--repo` and the `-o` options; its exit code is restic's. The secrets reach restic's environment only |
| `objects` | lists the per-version archive: one line per archived version with its time, snapshot, size and decoded key (`--json`: a list of `{"key", "archived", "snapshot", "shortId", "size"}`) |
| `recover` | writes the newest version of `--key` archived at or before `--at` (RFC 3339; default: the newest) to standard output, or to `-o FILE` (written to a temporary file next to it and renamed once complete) |

The repository is the bucket's per-version archive on its primary backing;
`--backing NAME` picks another copy's (a replica's), and `--snapshots`
(`env` and `exec`) the repository of the bucket's
[whole-bucket snapshots](#whole-bucket-snapshots). `--no-cache` runs restic
without a cache, `--cache-dir DIR` with one in `DIR` (default: restic's
own).

SOURCE says where the repository and its password come from:

- **Online**, the server running: `--server URL` with an access key allowed
  `admin:GetResticCredentials` on the bucket, given as `--access-key` and
  `--secret-key` or in `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`.
  The command calls `POST /buckets/{name}/restic/credentials`, which the
  server logs. `--region` sets the signing region (default: the server's,
  from `/_gos3/info`), `--insecure` skips TLS certificate verification,
  `--allow-http` accepts a plain `http://` URL of another machine (one on
  this machine needs no flag) and `--timeout` bounds the request (default
  30 s). The answer may name only an `s3:` or local repository, `s3.*`
  options and `AWS_*` credential variables; anything else is refused (see
  [security.md](security.md)). `gos3 restic env` (and
  revealed credentials) set `AWS_ACCESS_KEY_ID` and
  `AWS_SECRET_ACCESS_KEY` to the **repository's** keys, those of the
  backing, so after `eval "$(gos3 restic env …)"` a later online command
  in the same shell would sign its request to gos3 with them: pass
  `--access-key` and `--secret-key` explicitly in online mode, or use a
  separate shell.
- **Offline**, the server stopped: `--config FILE` (the server's
  configuration file; the `GOS3_*` variables apply as they do to the
  server, for example `GOS3_BACKING_<NAME>_ENCRYPTION_KEY`), `--data DIR`
  (its data directory, for a server configured by flags) or `--offline`
  (`GOS3_CONFIG` and the `GOS3_*` variables only). Backings added through
  the admin API are read from `backings.json` in the system directory and
  the bucket's primary from `placements.json`. Nothing is written.
  - fs backings: the bucket's ID comes from the backing's metadata
    database, opened read-only. A running server holds it locked: the
    command then fails and suggests `--server`. A bucket that no longer
    exists is found by the name in its recovery record; when several
    former buckets of that name left records, `--bucket-id ID` (the
    record's file name) chooses one.
  - s3 backings: the backing's `encryption_key` is required. The command
    reads the recovery records of the backing's home or, for a backing
    without a home, of the container that its local records
    (`.gos3/s3/<backing>/state.db`) name for the bucket. A backing with a
    home keeps its bucket registry in the home's journal, which only the
    server reads: offline, only wrapped buckets in the home itself (the
    default container) are found, and buckets in other containers need
    `--server`. `--bucket-id` chooses between records of former buckets
    of the same name, as on fs backings.
  - `--snapshots`: the repository the bucket's schedule names, read from
    the snapshot scheduler's state (`.gos3/restic-snapshots.json`): the
    bucket's own repository, or an aux repository of a backing (on an s3
    backing, in its home).

### Never forget `gos3-object` snapshots

A restic-archived version is stored as nothing but the ID of its snapshot:
gos3 reads the snapshot every time the version is read. A snapshot
forgotten (and pruned) by hand makes its version unreadable for good.

- Never run `restic forget` with a policy (`--keep-last`, `--keep-daily`,
  ...) on a repository gos3 writes: it would forget `gos3-object`
  snapshots. Never forget a `gos3-object` snapshot by ID while its version
  exists.
- `restic snapshots`, `ls`, `dump`, `restore`, `check` and `prune` are safe
  (prune forgets nothing; it takes the exclusive lock, see
  [Archive behavior](#archive-behavior)).
- Whole-bucket snapshots are forgotten by gos3's
  [retention](#retention), by **Forget** in the web UI or by
  `DELETE /buckets/{name}/restic/snapshots/{id}`, all of which refuse
  `gos3-object` snapshots.

## Whole-bucket snapshots

A snapshot schedule backs a bucket up as a whole, at points in time, into
a restic repository: each object as a file at its key, plus the bucket's
metadata, so that `restic restore` alone gives the bucket's content back,
on any machine, and gos3 can restore it into a bucket with its metadata.
Unlike the [per-version archive](#archiving-every-write) it works for
every bucket, native and memory buckets included, and costs nothing per
write.

### Schedule

Bucket settings → **Backup & archive** → Snapshots, or
`PUT /buckets/{name}/restic/snapshots/config`
([admin-api.md](admin-api.md#whole-bucket-restic-snapshots)), with
`admin:PutBucketSnapshots` on the bucket:

```json
{ "enabled": true, "interval": "24h", "at": "03:00", "consistent": true, "prefix": "",
  "retention": { "keepLast": 7, "keepDaily": 7, "keepWeekly": 4, "keepMonthly": 12 },
  "target": { "kind": "bucket" },
  "prune": { "enabled": true, "interval": "168h" } }
```

| Field | Default | Meaning |
|---|---|---|
| `enabled` | `true` | schedule snapshots (**Snapshot now** works either way) |
| `interval` | `24h` | time between snapshots, 1 hour to 30 days, counted from the last attempt |
| `at` | none | for intervals of 24 hours or more, the UTC time of day (`HH:MM`) the snapshots start at |
| `consistent` | `true` | versioned buckets: each key's newest version at the run's start, so writes during the run do not end up in the snapshot (a key whose version then was a delete marker is recorded as one). Otherwise, and in unversioned buckets, each object as it is when read |
| `prefix` | `""` | only keys with this prefix |
| `retention` | `keepLast` 7, `keepDaily` 7, `keepWeekly` 4, `keepMonthly` 12 | which snapshots to keep (see [Retention](#retention)); at least one rule must keep something |
| `target` | see below | the repository (see [Snapshot repositories](#snapshot-repositories)) |
| `prune` | enabled every `168h`, but off for a bucket that archives every write | whether and how often the repository is pruned (1 hour to 366 days; see [Prune](#prune)) |

Fields left out take their defaults; the PUT returns the stored document.
The schedule is the bucket configuration document `restic-snapshots`: it
replicates and moves with the bucket, and copy jobs never copy it.
`DELETE …/restic/snapshots/config` stops the schedule and keeps the
snapshots.

gos3 checks the schedules every minute (the first check up to 30 seconds
after the start; the schedules are read again every 10 minutes, and a
bucket's when its configuration changes) and starts a `restic-snapshot` [job](jobs.md) when one is
due and none is active for the bucket. A new schedule without `at` takes
its first snapshot at the next check, one with `at` at the next such time;
a run missed while the server was down starts at the first check after
the next start. **Snapshot now** (`POST /buckets/{name}/restic/snapshots`,
`admin:RunBucketSnapshot`) starts one at once. The last attempt, last
success, last snapshot, next run and last error are in
`GET /buckets/{name}/restic/snapshots` and in the web UI; the scheduler
keeps them in `restic-snapshots.json` in the system directory.

### Snapshot repositories

| `target` | Repository | Used for |
|---|---|---|
| `{"kind": "bucket"}` | the bucket's own restic repository on its primary's backing: `<path>/.gos3/restic/<bucket-id>` (fs), `<container prefix>restic/<bucket-id>` (wrapped buckets) | the default when the bucket's backing keeps a repository for it (fs and wrapped buckets). Shares its data with the per-version archive, so content archived already is not stored again |
| `{"kind": "backing", "backing": "<name>"}` | an **aux repository** `snap-<16 hex digits>` of that backing: `<path>/.gos3/restic-aux/snap-…` on an fs backing, `<home prefix>restic-aux/snap-…` on an s3 backing with a home and an encryption key | the default for other buckets (native and memory buckets), on the default backing when it can hold one; any bucket can choose it, for example to keep its snapshots on another disk or service |

- Memory backings and s3 backings without a home or without an encryption
  key cannot hold aux repositories; a schedule must then name a backing
  that can.
- The aux repository's name is generated once, when the schedule is first
  saved, and kept in it (`repo`, read-only; also remembered after the
  schedule is deleted), so the snapshot chain goes on after the bucket
  moves or a new schedule is saved. A `bucket` target follows the bucket's primary:
  after a move, new snapshots go to the repository on the new backing,
  while the older ones stay where they were written.
- An aux repository's password is the hex encoding of a random key: on an
  fs backing in `.gos3/keys/aux/<name>` (its recovery record
  `.gos3/keys/aux/<name>.restic.json` holds the repository and the
  password), on an s3 backing in `<prefix>keys/aux/<name>.key`, wrapped
  with the backing's encryption key (the record
  `<prefix>keys/aux/<name>.restic.json` holds no password). Keys are never
  regenerated; back them up like bucket keys.
- `GET /buckets/{name}/restic/snapshots` reports the repository's location
  and password source; the credentials come from
  `POST /buckets/{name}/restic/credentials` with `"purpose": "snapshots"`
  or `gos3 restic env --snapshots`.

### What a snapshot holds

```
/
├── gos3-bucket.json       the bucket: its settings (versioning, object lock, ...) and configuration documents
├── gos3-manifest.jsonl    a header line, then one JSON line per key (see below)
└── objects/
    └── <encoded key>      the content of each object, at its key
```

Snapshots have the host `gos3-snapshot` and the tags `gos3-snapshot` and
`gos3-bucket:<base64url of the bucket name>`, plus
`gos3-bucket-id:<bucket ID>` in the bucket's own repository; partial
snapshots (below) also `gos3-partial`. A snapshot's time is the time its
run started (in whole seconds, always after its parent's). A versioned
bucket's snapshot holds the current version of each key, not its older
versions.

The manifest's first line is a header (`{"gos3": "gos3-snapshot-manifest",
"version": 1, "bucket": …, "asOf": …, "keys": …, "bytes": …}`). Each
further line describes one key, in key order:

| Field | Content |
|---|---|
| `k` | the key; `kb` instead holds it in standard base64 when it is not valid UTF-8 |
| `p` | the file's path in the snapshot (`objects/…`); absent for a delete marker |
| `v`, `lm` | the version ID and its LastModified |
| `s`, `e` | the size and ETag |
| `dm` | `true` when the key's version is a delete marker (no file) |
| `mt` | the file's modification time (Unix seconds) |
| `m` | the metadata: `contentType`, `userMeta`, `tags`, `acl`, `checksums`, `retentionMode`, `retainUntil`, `legalHold`, ... |

Keys become paths below `objects/` segment by segment (at each `/`), so
that every key can be a file name on Linux, macOS and Windows:

| Key segment | Path segment |
|---|---|
| ordinary names | unchanged |
| `%`, control characters, `\ : * ? " < > \|`, bytes of invalid UTF-8 | percent-encoded (`%25`, `%3A`, ...) |
| empty (as in `a//b` or a trailing `/`) | `%` |
| `.` and `..` | `%2E` and `%2E%2E` |
| a leading space, a trailing dot or space | percent-encoded (`%20`, `%2E`) |
| Windows device names (`CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9`, ..., with or without an extension) | the first character percent-encoded |
| longer than 240 bytes once encoded | cut to at most 200 bytes, followed by `%~` and 16 hex digits of the SHA-256 of the segment |
| a name already taken in its directory, ignoring case, or taken by a file where a directory is needed or the reverse (an object `a/b` next to keys `a/b/…`) | followed by `%%` and 8 hex digits of the SHA-256 of the key (or of the directory's key prefix) |

Names are claimed in key order (byte-wise), so the earlier key keeps the
plain name: with an object `a/b` and keys `a/b/c`, the file `objects/a/b`
is the object and the directory gets the suffix. **The manifest is
authoritative**: it maps every path to its exact key and metadata, and
restores read keys from it, never from paths. Ordinary keys appear
unchanged, so the restored files of most buckets are directly usable.

### How a run works

A run is a `restic-snapshot` job; its phase shows the step. It waits
(`waiting`) for one of the `restic.snapshots.workers`, then:

1. **preparing**: gos3 resolves the repository and finds the newest
   complete snapshot of the bucket there (the parent). Partial snapshots
   left by an interrupted run are forgotten.
2. **scanning**, **planning**: gos3 lists the bucket and compares it with
   the parent's manifest.
3. **staging**: in the bucket's **stage directory**
   (`restic.snapshots.stage_dir`, by default `restic-stage/` in the system
   directory, one subdirectory per repository and bucket, kept between
   runs), objects unchanged since the parent are **placeholders**: sparse
   files of the object's size and the parent's file time, which take no
   disk space. New and changed objects are read from the bucket and
   written to the stage; files of deleted keys are removed.
4. **backing up**: `restic backup` runs on the stage with the parent and
   `--ignore-inode --ignore-ctime`: restic takes the content of files whose
   size and time match the parent's from the parent without reading them,
   so it reads only what gos3 wrote.
5. **verifying**: every backup is first saved tagged `gos3-partial`
   (unverified). restic must report at most as many new and changed files
   as gos3 wrote since the previous backup, plus the non-object files
   written with it (the manifest and the bucket description, for the final
   backup only). If it reports more (it read a placeholder and would have
   stored zeros), gos3 compares the snapshot with its parent
   (`restic diff`), forgets it by its ID, writes the real content of those
   files and backs up again, at most twice; then the job fails with a hint
   to set `restic.snapshots.incremental: false`. A verified final backup
   is **promoted**: the unchanged stage is backed up once more on top of
   it without the `gos3-partial` tag (restic must read nothing; this costs
   one more scan of the stage), and that snapshot carries the verified
   backup's summary in a `gos3-summary:` tag. An unverified snapshot is
   never used as a parent or offered for restore, even when forgetting it
   fails (its ID is then remembered in `restic-snapshots.json` and
   forgotten by the next run).
6. When the data written to the stage reaches
   `restic.snapshots.spool_bytes` (8 GiB by default, at least 64 MiB; a
   larger single object is staged whole), gos3 saves a **partial
   snapshot** (tagged `gos3-partial`) of what is staged so far, verifies
   it, turns its files into placeholders and continues with the partial as
   the parent. The stage thus holds at most about that much real data.
   Partial snapshots are incomplete and cannot be restored; gos3 forgets
   them once the run's complete snapshot is saved, or when the run fails.
7. After the backup the staged files become placeholders again, and the
   stage remembers which snapshot it matches, so that the next run only
   touches what changed. A stage that does not match the parent (deleted,
   or left by a failed run) is rebuilt from the parent's manifest.
8. **retention**, **pruning**: gos3 applies the retention and prunes when
   it is due; failures of either are warnings of the job, not errors.

While a run copies objects into the stage, their content is there in
**plain text** until restic has stored it; between runs the stage holds
placeholders only. A failed or canceled run removes its staged files
(the next run rebuilds the stage from the parent's manifest), and a
bucket's stage is removed when its schedule or the bucket is deleted.
Without a system directory each server process uses a private stage in
the temporary directory (mode 0700, removed at shutdown).

With `restic.snapshots.incremental: false` every object is read and
written to the stage at every run (restic still stores only new data);
use it when the stage's file system does not keep sparse files or file
times exactly. Placeholders need no space, but every key is a file:
a bucket with millions of keys needs as many inodes in the stage
([operations.md](operations.md#restic-snapshot-stage)).

### Retention

After each run gos3 applies the retention itself, in UTC, to this
bucket's complete whole-bucket snapshots in the target repository:
`keepLast`, `keepHourly`, `keepDaily`, `keepWeekly`, `keepMonthly`,
`keepYearly` (the newest snapshot of each of the last *n* hours, days,
ISO weeks, ...) and `keepWithin` (every snapshot taken within this
duration of the newest one); a snapshot is kept when any rule keeps it, as
with `restic forget --keep-*`, and the newest snapshot is always kept. The
others are forgotten by ID, and only after gos3 has listed them again and found
each to be a whole-bucket snapshot of this bucket (tag and host
`gos3-snapshot`, the bucket's tag, no `gos3-object` tag). Per-version
archive snapshots and snapshots other tools made in the same repository
are never forgotten. Forgetting frees no space until the repository is
pruned.

### Prune

Prune removes the data that no snapshot references any more. A schedule
can prune its repository (`prune.enabled`, `prune.interval`): a run prunes
once the interval has passed since the last prune (the first run only
starts the clock). **Prune** in the web UI or
`POST /buckets/{name}/restic/prune` starts a `restic-prune` job at once.
Prune waits up to 10 minutes for other restic commands' locks, then
holds restic's exclusive lock on the repository: per-version archive
writes into it wait (up to 30 seconds, then fail) and reads can fail while
it repacks. `prune.enabled` therefore defaults to off for a bucket that
archives every write (scope `all`) when the schedule is saved, and to
weekly otherwise; schedule it for a quiet time.

### Restoring a snapshot

**With gos3**: **Restore to bucket…** next to a snapshot in the web UI, or
`POST /buckets/{name}/restic/snapshots/{id}/restore` with a target bucket
(an existing empty one, an existing one with `onExisting: "merge"`, or one
to create with `target.create` as for a copy job), starts a
`restic-restore` job. The snapshot must be a complete whole-bucket
snapshot of the bucket (not a partial one). A created target gets object
lock when asked, or else when the snapshotted bucket had it, and the
snapshotted bucket's versioning. The job reads the manifest and streams
the content (`restic dump --archive tar`), writing every object with its
metadata: into fs, memory and wrapped buckets with its version ID and
LastModified (and delete markers as delete markers), into native buckets
as new writes. A file the manifest names that is missing from the
snapshot fails the job; a retried run skips the keys the target already
holds. `configs` (`safe`, the default, or `none`) restores the bucket
configuration from `gos3-bucket.json` as a one-shot
[copy job](jobs.md#bucket-configuration) would, and `prefix` restores
only some keys. As for a copy job, the snapshotted bucket's ACL guards
(public access block with `IgnorePublicAcls`, ownership controls
`BucketOwnerEnforced`) are applied to the target whatever `configs` says,
since restored objects keep their ACLs; a target that cannot keep them
fails the restore. A snapshot without a readable `gos3-bucket.json`
restores every object without its ACL (private), with a warning. It needs `admin:RestoreBucketSnapshot` on the bucket and
`admin:CreateCopyJob` on the target; the job's progress and warnings
(configuration it skipped) are in its status.

**Without gos3**:

```sh
eval "$(gos3 restic env --snapshots --config /etc/gos3/gos3.yaml --bucket photos)"   # or set the variables by hand
restic snapshots --tag gos3-snapshot,gos3-bucket:cGhvdG9z
restic restore <snapshot-id> --target /restore/photos
```

`/restore/photos/objects/` then holds the objects as files and
`/restore/photos/gos3-manifest.jsonl` names the key and metadata of each
([fields](#what-a-snapshot-holds)). To upload them into any S3 bucket, for
example with the AWS CLI and `jq`:

```sh
cd /restore/photos
jq -r 'select(.gos3 == null and .k != null and (.dm | not)) | [.p, .k] | @tsv' gos3-manifest.jsonl |
while IFS=$'\t' read -r path key; do
  aws s3 cp -- "$path" "s3://photos-restored/$key"
done
```

The filter skips the header line and delete markers; `p` is relative to
the restore target. A key that is not valid UTF-8 has no `k` but `kb`
(its bytes in standard base64); the filter skips such keys, and keys
containing tabs or line breaks need a script of their own. This restores content
and keys only; gos3's restore job also restores the metadata, tags, ACLs
and retention from the manifest. `restic restore --include /objects/<path>`
or `restic dump <snapshot-id> /objects/<path>` fetch single objects.
