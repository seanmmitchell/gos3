# Object encryption and restic

Bucket settings → **Encryption** selects native **SSE-S3 (AES256)** or **Restic**.
Both encrypt new object data. S3 clients continue to use `AES256`; restic is a
gos3 implementation of SSE-S3. An explicit AES256 upload uses the bucket's
provider even when default encryption is disabled.

Storage follows placement. The bucket stores only its encryption provider.
Each assigned backing owns its repository location and S3 credentials. A
replica or move target uses its own backing's settings, keys, and repository;
source backing credentials never replicate with bucket configuration.

Native encryption uses gos3's authenticated AES-256-GCM stream format
(`internal/storage/crypt`): 64 KiB chunks, each sealed with a nonce made of
its index and a final-chunk flag, so reordered, truncated or extended data
fails authentication. HKDF-SHA256 derives a separate key for each immutable
data file from a random bucket master key, so every stream has its own key
and swapping objects' ciphertext fails authentication. Compression happens
before encryption; range reads decrypt only the chunks they need. Filesystem
upload staging and multipart parts are encrypted too. The memory backend
records AES256 for SSE responses and replication but keeps its in-process
data as is.

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

## Backing storage for restic

Restic needs the [restic](https://restic.readthedocs.io/en/stable/020_installation.html)
executable, **0.17.1 or newer**, on the gos3 server's PATH. The gos3
container images include a pinned, checksum-verified restic at
`/usr/local/bin/restic` ([deployment.md](deployment.md#docker)); for the
release archives and the systemd unit, install it yourself. Native AES256
needs no extra executable; memory backings do not support restic.

Restic cannot be selected while a memory backing holds a copy of the bucket,
and a bucket that uses restic or holds restic versions cannot be replicated
or moved to one.

On server **Storage**, open **Restic storage** for the assigned backing:

- **Local backing storage** (default): each bucket copy uses
  `<backing>/.gos3/restic/<bucket-id>`.
- **S3 storage**: configure the endpoint (`https://s3.example.com`), destination
  S3 bucket, repository prefix, region, access key, secret key, and optional
  session token. Each bucket copy derives its repository as
  `s3:<endpoint>/<destination-bucket>/<prefix>/<bucket-id>`. Saving the
  settings runs restic to check access to the destination (and to every
  repository the backing already has), so restic must be installed first.

Each physical bucket copy has its own ID and key, including replicas. Passwords
are derived from that key and managed by the server. Bucket encryption has no
repository URL, password, or credential fields. S3 settings may also be supplied
in startup JSON/YAML as `storage.restic_s3` for the default backing and
`storage.backings[].restic_s3` for additional filesystem backings:

```yaml
storage:
  backend: fs
  path: /srv/gos3
  backings:
    - name: archive
      backend: fs
      path: /srv/gos3-archive
      restic_s3:
        endpoint: https://s3.example.com
        bucket: archives
        prefix: gos3-archive
        region: us-east-1
        access_key: ARCHIVE_ACCESS_KEY
        secret_key: ARCHIVE_SECRET_KEY
```

The filesystem path still holds metadata, keys, references, and encrypted
staging. The backing's S3 configuration controls restic payload storage;
unencrypted/native-AES objects on an fs backing continue to use its filesystem.
The admin API does not expose S3 credentials. Blank credential inputs retain
saved values; an expired session token can be explicitly cleared. Invalid
credential updates leave working settings intact. Persisted settings override
startup settings for that backing.

Once a repository is initialized, its location is fixed, including after
objects or buckets are deleted. Configure another backing and use **Placement**
to move the bucket when changing storage. Existing archives stay with their
original backing. Credentials and region can be updated at the same location.
The destination may be a bucket on another gos3 server, but not one whose
provider is restic: gos3 rejects restic's own uploads (recognized by its
`gos3-restic` user agent) to a bucket that would archive them through restic
again. HTTPS protects transport; HTTP supports trusted local endpoints.

Reading backing settings uses `admin:ListBackings`; updates require
`admin:UpdateConfig` at service level. Bucket provider selection uses scoped
`admin:GetBucketEncryption` / `admin:PutBucketEncryption`. Standard S3
permissions separately govern enabling or disabling bucket-default encryption.

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

Restic keeps a cache in `.gos3/restic-cache/` (one subdirectory per
repository, managed by restic; safe to delete). Every object adds a snapshot
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

Back up metadata, keys, backing settings, and data/repositories together:
a file-level backup of a backing's data directory includes all but remote S3
repositories ([operations.md](operations.md#backups)). Native keys are
`.gos3/keys/<bucket-id>`. Restic repository/password recovery records are
`.gos3/keys/<bucket-id>.restic.json`. Keys, recovery records and local
repositories (`.gos3/restic/<bucket-id>`) remain after the bucket is deleted.
S3 settings and credentials live in the backing's protected
`.gos3/restic-backing.json`, or its protected startup configuration; they are
never stored in bucket configuration. Recovery records do not duplicate S3
credentials. Repository snapshots retain object identity but do not replace
gos3 metadata for versions, tags, permissions, and retention.

Missing native keys fail closed and are not silently regenerated. The key
directory gets private filesystem permissions and a protected Windows ACL
before a key is created (failing that fails key creation); at startup existing
key and recovery files are made private again, best effort with a logged
warning, e.g. after a restore that did not keep permissions.
Server-managed keys allow unattended access; a party holding keys and ciphertext
can decrypt data. This feature does not protect against a compromised gos3
process or a full copy of its keys and data. S3 metadata itself is not encrypted.
