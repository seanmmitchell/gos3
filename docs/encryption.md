# Object encryption and restic

Bucket settings → **Encryption** offers native **SSE-S3 (AES256)** and
**Restic** on gos3 connections. Both encrypt new object data before it is
persisted. S3 clients keep using `AES256`; restic is a gos3 provider of that
encryption, not a new S3 algorithm name. An explicit `AES256` upload uses the
bucket's selected provider even if default bucket encryption is disabled.

Native encryption uses authenticated AES-256-GCM streams (MinIO's `sio`
format v2). A random bucket master key derives a separate key for each immutable
data file using HKDF-SHA256. The file identity is part of the derivation, so
swapping two objects' ciphertext fails authentication. Compression, when enabled,
happens before encryption. Range reads decrypt only the necessary blocks.
Multipart parts and upload staging files are encrypted too. Replicas and online
moves re-encrypt content using the destination backing's own keys.

AWS KMS (`aws:kms`, `aws:kms:dsse`) and SSE-C are not implemented and return
`501 NotImplemented`. Existing KMS bucket defaults must be changed before new
uploads can succeed. A KMS identifier is never treated as a local encryption key.

## Existing objects

Earlier gos3 versions accepted encryption settings but did not encrypt data.
Those objects remain readable, but no longer return an encryption response
header. Changing a bucket setting does not rewrite existing versions. Upload or
copy an object with `AES256` to create an encrypted version; in a versioned bucket,
older unencrypted versions remain until explicitly removed under normal retention
and Object Lock rules. Existing encrypted versions remain readable after changing
or disabling the bucket default.

## Restic setup

Install **restic 0.17.1 or newer** on the server's `PATH` (on every host serving a
filesystem backing). The native AES option does not need another executable.
The memory backend supports native AES but not restic.
See restic's [installation instructions](https://restic.readthedocs.io/en/stable/020_installation.html)
and [S3 repository documentation](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html#amazon-s3).

Choose Restic in bucket settings:

- Leave **Repository** empty for a local repository at
  `<backing>/.gos3/restic/<bucket-id>`.
- For a remote repository, enter
  `s3:https://s3.example.com/backup-bucket/source-bucket-prefix`, its S3 access key,
  secret key, optional session token, and region. Use a distinct repository/prefix
  for each source bucket. Standard AWS S3 and S3-compatible endpoints use the same
  form. HTTP is supported for trusted local endpoints; HTTPS protects transport.
- For an existing remote repository, supply its password. For a new repository,
  leave the password empty and gos3 generates and stores one.

The repository destination must not be the source bucket or another bucket with
restic encryption enabled: storing a repository inside itself would recurse.
Repository creation is attempted only when restic reports that the repository
does not exist. Wrong passwords, access failures, and network failures stop the
operation. The destination is fixed after setup so existing snapshot references
remain valid. Credential updates are checked before replacing the saved values;
blank secret fields retain their prior values.

Provider configuration needs `admin:GetBucketEncryption` and
`admin:PutBucketEncryption`, scoped to `arn:gos3:admin:::bucket/<name>`.
Enabling/disabling the default additionally uses the standard
`s3:GetEncryptionConfiguration` / `s3:PutEncryptionConfiguration` permissions.

## Restic behavior

Each completed object becomes a restic snapshot containing `/object`. Snapshot
tags `gos3-bucket:<base64url>` and `gos3-key:<base64url>` identify the original
bucket and key using unpadded URL-safe base64, including names with commas or
unusual characters. These tags are encrypted with the snapshot and visible via
`restic snapshots --json` when unlocked. gos3
publishes the S3 version only after restic completes successfully. Temporary
upload data uses native encryption; no plaintext staging file is written.
Restic handles compression and deduplication, so gos3's separate compression
setting is not applied to restic objects.

Downloads retrieve and verify the snapshot's object into an encrypted temporary
cache before serving it. This bounds memory, supports ranges and seeking, and
removes the cache on close; startup clears caches left by crashes. It adds
repository latency and requires temporary disk space for the entire object,
including for small range requests. Repository failures fail the request rather
than falling back to unencrypted storage.

S3 overwrites and deletes leave snapshots in the repository as an archive.
Automatic snapshot retention/pruning is not implemented. Do not run `forget`
against snapshots still referenced by gos3: those are live S3 data. Failed commits
can also leave unreferenced snapshots. A repository is not a standalone backup of
the S3 service: keep gos3 metadata to recover version IDs, S3 tags,
permissions, and retention settings.

The per-object stored-byte metric counts the 64-byte local snapshot reference;
it excludes repository packs, which are shared across objects and snapshots.
Use restic's repository statistics and filesystem/provider usage to measure the
archive, including deleted objects' retained snapshots.

## Keys and recovery

Native master keys live in `<backing>/.gos3/keys/<bucket-id>`. Restic credentials
are protected bucket configuration in `meta.db`; a recovery copy is retained in
`.gos3/keys/<bucket-id>.restic.json`, including after bucket deletion. Local
repositories and native keys are independent per backing. Remote repository
settings, including credentials, replicate only to the bucket's trusted backings.
Secrets never appear in encryption-provider API responses or subprocess arguments.

Back up keys, metadata, and object data/repositories together. A missing native
key fails closed and is not silently regenerated for an existing encrypted bucket.
Files use private permissions; on Windows the system directory has a protected
ACL for the service account, SYSTEM, and Administrators.

Server-managed keys allow unattended reads and writes. A party with both the
protected keys and ciphertext can decrypt the objects; this does not protect
against a compromised gos3 process or a complete copy of its data directory.
Filesystem/full-disk encryption or externally managed secrets are still needed
for that threat model. Bucket names and S3 metadata in `meta.db` are not encrypted
by this object encryption feature.
