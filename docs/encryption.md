# Object encryption and restic

Bucket settings → **Encryption** selects native **SSE-S3 (AES256)** or **Restic**.
Both encrypt new object data. S3 clients continue to use `AES256`; restic is a
gos3 implementation of SSE-S3. An explicit AES256 upload uses the bucket's
provider even when default encryption is disabled.

Storage follows placement. The bucket stores only its encryption provider.
Each assigned backing owns its repository location and S3 credentials. A
replica or move target uses its own backing's settings, keys, and repository;
source backing credentials never replicate with bucket configuration.

Native encryption uses authenticated AES-256-GCM streams (MinIO `sio` v2),
with HKDF-SHA256 deriving a separate key for each immutable data file from a
random bucket master key. Swapping objects' ciphertext fails authentication.
Compression happens before encryption; range reads decrypt the necessary
blocks. Filesystem upload staging and multipart parts are encrypted too.

AWS KMS (`aws:kms` / `aws:kms:dsse`) and SSE-C return
`501 NotImplemented`. Change legacy KMS defaults before new uploads.

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

Once repository preparation begins, its location is fixed, including after
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

Each object becomes a restic snapshot containing `/object`, published only
after backup succeeds. Encrypted snapshot tags `gos3-bucket:<base64url>` and
`gos3-key:<base64url>` retain the source identity using unpadded URL-safe
base64. Restic handles compression and deduplication; gos3 compression is not
applied to restic objects. Failed commits can leave unreferenced snapshots.

GET retrieves and verifies the whole object into an encrypted temporary cache
before serving it, including range requests. The cache is removed on close and
crash leftovers are cleared at startup. This requires disk space for the whole
object and adds repository latency. Errors fail the request without plaintext
fallback. Overwrites and deletes retain snapshots; automatic pruning is not
implemented. Do not forget snapshots still referenced by live gos3 metadata.

The per-object stored-byte metric counts the 64-byte local reference and excludes
shared repository packs. Use restic/provider statistics to measure archive usage.

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

Missing native keys fail closed and are not silently regenerated. The system
directory has private filesystem permissions and a protected Windows ACL.
Server-managed keys allow unattended access; a party holding keys and ciphertext
can decrypt data. This feature does not protect against a compromised gos3
process or a full copy of its keys and data. S3 metadata itself is not encrypted.
