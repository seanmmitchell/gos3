# Object encryption and restic

Bucket settings → **Encryption** selects native **SSE-S3 (AES256)** or **Restic**.
Both encrypt new object data. S3 clients continue to use `AES256`; restic is a
gos3 implementation of SSE-S3. An explicit AES256 upload uses the bucket's
provider even when default encryption is disabled.

Storage follows placement. The bucket stores only its encryption provider.
Each backing holding a copy of the bucket keeps that copy's keys and restic
repository itself: an fs backing in its data directory, an s3 backing in its
remote bucket. A replica or move target uses its own backing's keys and
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
| s3 | master key in the remote bucket at `<prefix>keys/<bucket-id>.key`, wrapped with the backing's `encryption_key`; **unavailable without one** | restic's s3 backend in the backing's own bucket, `<prefix>restic/<bucket-id>`, with the backing's current endpoint, region and credentials; needs the `encryption_key` and the credentials; recovery record `<prefix>keys/<bucket-id>.restic.json` without the password, which derives from the wrapped master key |
| memory | recorded, data kept in memory as is | not offered |
| s3 gateway | not offered (objects are stored as they are) | not offered |

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
encryption key. For the same reason a bucket that uses a provider (as its
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

An s3 backing stores each bucket's random master key in its remote bucket,
encrypted (AES-256-GCM) with the backing's **key-encryption key**:
`encryption_key` in its `s3` settings, `GOS3_S3_ENCRYPTION_KEY` for the
default backing or `GOS3_BACKING_<NAME>_ENCRYPTION_KEY` for others, or the
**Encryption key** field (with **Generate**) under **Advanced** in the
Storage page's backing form. It is standard base64 of exactly 32 bytes:

```sh
openssl rand -base64 32
```

- The S3 service never sees a usable key; reading the remote bucket alone
  does not decrypt the data.
- A backing without the key offers neither AES256 nor restic. The key can
  be added later (also live on the Storage page, where it is checked
  against the keys the backing already holds), but not changed: a
  different key is refused, because the existing master keys could no
  longer be opened.
- **Losing the key loses every encrypted object and restic archive of the
  backing.** gos3 never regenerates a missing key. Keep it outside the
  remote bucket (a password manager or secret store), and back it up with
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
Encrypted snapshot tags `gos3-bucket:<base64url>` and `gos3-key:<base64url>`
retain the source identity using unpadded URL-safe base64. Restic handles
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
snapshots still referenced by gos3). Prune takes an exclusive lock: writes
wait up to 30 seconds for it, and reads, which do not lock, can fail while
it repacks. Overwrites and deletes retain snapshots; automatic pruning is not
implemented.

Stored-byte usage metrics count restic versions at their content size, because
repository packs are deduplicated, compressed, and shared between snapshots
and cannot be attributed to single versions. Use restic/provider statistics to
measure actual archive usage.

## Keys and recovery

Back up metadata, keys and data/repositories together
([operations.md](operations.md#backups)):

- **fs backings**: a file-level backup of the backing's data directory
  includes everything. Native keys are `.gos3/keys/<bucket-id>`, restic
  recovery records (repository location and password)
  `.gos3/keys/<bucket-id>.restic.json`, local repositories
  `.gos3/restic/<bucket-id>`.
- **s3 backings**: the wrapped keys (`<prefix>keys/`), recovery records
  (without passwords) and repositories (`<prefix>restic/`) are in the
  remote bucket with the data; back up the backing's prefix and, separately,
  its **encryption key** (see [backings.md](backings.md#backups)).

Keys, recovery records and repositories remain after the bucket is
deleted. Repository snapshots retain object identity but do not replace
gos3 metadata for versions, tags, permissions, and retention.

Missing native keys fail closed and are not silently regenerated. On fs
backings the key directory gets private filesystem permissions and a protected Windows ACL
before a key is created (failing that fails key creation); at startup existing
key and recovery files are made private again, best effort with a logged
warning, e.g. after a restore that did not keep permissions.
Server-managed keys allow unattended access; a party holding keys and ciphertext
(for an s3 backing: its encryption key and the remote bucket) can decrypt
data. This feature does not protect against a compromised gos3 process or a
full copy of its keys and data. S3 metadata itself is not encrypted: on an
s3 backing, bucket names, object keys, user metadata and tags are readable
in the journal and snapshots by anyone who can read the remote bucket.
