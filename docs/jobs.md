# Copy and mirror jobs

A **copy job** copies one bucket (or the keys under a prefix) into another
bucket once; a **mirror job** does the same and then keeps the target up
to date. Source and target can be on any backing and of any kind (fs,
memory, wrapped or native buckets of [s3 backings](backings.md)), and
their names can differ. Jobs run in the background on the server, survive
restarts and report their progress. They are managed on the web UI's
**Jobs** page and with the admin API
([admin-api.md](admin-api.md#jobs-copy-and-mirror)).

The same job engine runs restic snapshots, restores and prunes of whole
buckets (job kinds `restic-snapshot`, `restic-restore`, `restic-prune`;
see [encryption.md](encryption.md#whole-bucket-snapshots)). Their states,
retries and persistence are those described here.

## Copy or mirror

| | Copy (`kind: "copy"`) | Mirror (`kind: "mirror"`) |
|---|---|---|
| Runs | once: phases `preparing`, `copying`, `verifying`, then `done` and the job succeeds | an initial full pass (`copying`), then `watching` until it is canceled, with periodic full passes (`verifying`) |
| Follows later changes | no | yes: writes through gos3 within seconds, other tools' writes to a native source at each poll (see [Mirrors](#mirrors-following-the-source)), and changes of the source's configuration |
| Deletes on the target | versions of copied keys that the source no longer has (not with `merge`) | also keys deleted in the source (`propagateDeletes`) |
| Bucket configuration | copied once (`configs`), lifecycle included | copied, and kept in step with the source, never lifecycle |
| Typical use | change a bucket's backing or kind, rename it, take a copy out of a native bucket | a continuously updated second copy: on another backing, another service, or as plain objects other tools can read |

**Preparing** creates the target bucket when asked (`target.create`),
sets its versioning as the copy needs, copies the bucket configuration and
counts the source's keys and bytes for the progress estimate. **Copying**
walks the source and the target side by side in key order and makes each
target key hold what the source key holds. **Verifying** (`finalPass`, on
by default) compares both once more and copies what changed during the
first pass.

## Sources and targets

- **Any bucket to any bucket**, on the same backing or another one, under
  another name. With prefixes, source key `<source prefix>k` becomes
  target key `<target prefix>k`; a key that would end up longer than 1024
  bytes is reported as a conflict and skipped. Source and target must
  differ: the same bucket only with prefixes that do not overlap.
- **Creating the target.** With `target.create` the job creates the target
  bucket before copying, as the job's creator (its owner): on the backing
  named there (default: the default backing), with the bucket kind,
  container or remote bucket given (as in
  [backings.md](backings.md#choosing-the-backing-of-a-bucket)). Without a
  kind it is a bucket that keeps version IDs (a wrapped bucket on an s3
  backing). It gets object lock when `objectLock` says so, by default when
  the source has object lock and the new bucket can. It is created like
  any new bucket, so the server's default replicas apply to it.
  `target.create` is ignored when the target exists.
- **Existing targets** (`onExisting`):

  | Value | Target |
  |---|---|
  | `fail` (default) | the target range (the target prefix of the bucket) must be empty: no versions, delete markers or multipart uploads, else the job is refused with `409`. Two jobs with `fail` cannot target overlapping ranges of one bucket |
  | `merge` | may hold data; the job never deletes anything there. A version with the ID of a source version is made identical to it, a `null` version overwritten. A merging mirror applies no deletes |
  | `adopt` | mirrors only (copy jobs refuse it): may hold data, which the mirror owns from then on: keys the source has are made identical to the source's (their other versions are deleted), and target keys the source lacks are deleted |

- **Versions** (`versions`):

  | Value | Copies |
  |---|---|
  | `all` | every version and delete marker of each key, oldest first, so the target's history of a key becomes the source's |
  | `current` | each key's current version; keys whose current version is a delete marker count as absent |

  The default is `all` when the target can keep versions (it imports them:
  fs and memory buckets, wrapped buckets; or it is an existing bucket with
  versioning enabled), else `current`. `all` into a native target needs
  versioning enabled on it (a new native target gets it).

How the copies are stored depends on the target:

| Target | Version IDs and `LastModified` | Versioning |
|---|---|---|
| fs, memory, wrapped bucket | kept: the copy has the source's version IDs, modification times, metadata and order; `current` stores each version as the target's `null` version | with `all`, follows the source's (never back to unversioned; an existing target the job did not create only leaves unversioned, unless the job is a mirror) |
| native bucket | the remote service's own: each copy is a new write (oldest first with `all`); the job's database maps source to target versions, so later passes copy only what is new | enabled with `all` |

Version IDs that gos3 cannot import (a native source's remote IDs need
not fit gos3's rules) are imported as `x` followed by 43 characters of the
base64url SHA-256 of the original ID, so every copy maps an ID the same
way.

## How changes are detected

Each job copies its keys in one of two modes, shown in its status as
`mode`:

- **Identity mode**: both buckets are stored by gos3 itself (fs, memory
  or wrapped buckets), so both import versions. Both version lists are
  compared directly, as a replica is compared with its primary: versions
  missing on the target are copied, versions the source no longer has are
  deleted, and versions whose metadata differs (tags, ACL, retention,
  legal hold, content headers) are updated in place.
- **Fingerprint mode**: a native source or a native target. Listings of
  remote buckets may report other data than reads (an object uploaded in
  parts by another tool lists with a multipart ETag), so the two sides are
  never compared with each other. Instead, the job records, per key, a
  fingerprint of the source's listing (version IDs, ETags, sizes,
  modification times, delete markers) and of the target's after it copied
  the key, and copies the key again when either listing changes: the
  source was written, or someone changed the copy behind the job's back.
  The fingerprints, the version map and which keys the job wrote live in
  the job's database ([Persistence](#persistence-and-restarts)).

Limits of fingerprint mode:

- **Metadata-only changes are not detected**: tags, user metadata, an ACL
  or a retention changed on the source without rewriting the object (by
  another tool, or through gos3) leave its listing as it was, so the copy
  keeps the old metadata. Rewrite the object, or copy into a new target.
  Identity mode has no such limit.
- Listings of native buckets carry no metadata, so every copied version is
  read with its metadata from the remote service (see
  [backings.md](backings.md#limits-of-native-buckets)).

In both modes a key that changes while it is copied is compared again,
up to three more times, and then left for the next pass.

## What is copied with each version

| | Behaviour | Option |
|---|---|---|
| Content, content headers, user metadata, tags, object ACL, checksums | copied | |
| Object lock retention and legal hold | copied; the target bucket needs object lock, else the job is refused unless `dropRetention` copies the versions without them | `dropRetention` |
| Encryption | `preserve` (default): each version is stored encrypted as its source version is (AES256 or restic), with the target's own keys and repository; a target that cannot store it is refused when the job is created. `target`: stored as a client's write to the target bucket would be (its default encryption and provider, restic for every version when the target archives every write); copies are compared ignoring encryption, so changing the target's settings does not copy everything again | `encryption`, `allowPlaintext` |
| Unencrypted copies of encrypted data | refused unless `allowPlaintext`: needed for native targets, memory backings and backings without an encryption key ([encryption.md](encryption.md#where-keys-and-repositories-live)) | `allowPlaintext` |
| Compression | `preserve` (default): compressed where the source version is; `target`: as the target bucket's compression setting and the server's policy decide for a new object. Native buckets store data uncompressed either way | `compression` |

Whether the source holds encrypted data is judged from its encryption
settings and a sample of its first 1000 versions; a version found later
that the target cannot store fails the job.

## Bucket configuration

`configs` chooses which configuration documents the job copies:

| Value | Copied |
|---|---|
| `safe` (default) | CORS, bucket tags, default encryption, website, ownership controls, public access block, bucket ACL and the compression setting as they are; the encryption provider and the object lock configuration only where the target supports them (else skipped with a warning); lifecycle rules for copies only; the bucket policy rewritten for the target (below) |
| `all` | every configuration document except those never copied (below), the policy rewritten as with `safe` |
| `none` | nothing, except the settings that keep object ACLs from making objects public (below) |

- **Never copied**: the replication configuration, the
  [whole-bucket snapshot](encryption.md#whole-bucket-snapshots) schedule,
  and lifecycle rules into a mirror (expiring objects in the target would
  fight the mirror). Versioning is set as the copy needs it whatever
  `configs` says.
- **Bucket policy**: resources naming the source bucket are rewritten to
  the target (`arn:aws:s3:::<source>` becomes `arn:aws:s3:::<target>`).
  With prefixes, object ARNs inside the source prefix are mapped to the
  target prefix and the others are dropped, and a policy using `s3:prefix`
  conditions is skipped with a warning. A policy that is not valid for the
  target after rewriting is skipped with a warning too (in the dry run and
  in the job's `warnings`).
- **Object ACLs travel with their versions.** When the source neutralises
  them (public access block with `IgnorePublicAcls`, or ownership controls
  `BucketOwnerEnforced`), those settings are copied to the target whatever
  `configs` says, so the copies do not become public. Bucket policies and
  bucket ACLs grant on the target what they granted on the source, public
  access included: review them, or use `configs: "none"`.
- **Mirrors keep the configuration in step**: when the source's
  configuration or versioning changes, the mirror copies the change at
  its next run. It deletes a target document only when the source no
  longer has it and the mirror copied it before; documents the job did not
  copy are never deleted. When a changed source policy can no longer be
  rewritten for the target, the mirror removes the policy it copied
  before and warns.

## Safety rules

- **Empty targets by default**: an existing target range must be empty
  unless `onExisting` says otherwise.
- **No cycles**: a mirror whose target is, directly or through other
  mirrors, its own source is refused (`409`). Chains (A to B, B to C) are
  allowed.
- **One writer per mirror target**: no job may copy into the target range
  of an active mirror, and a mirror may not target the range of another
  active job (`409`). These checks run again when a job starts, against
  the jobs created before it, so two jobs submitted at once cannot both
  pass.
- **Locked versions are never deleted or replaced.** A target version
  under unexpired retention or a legal hold that the copy would delete or
  replace is left alone and reported as a **conflict** (`conflicts`,
  `conflictSamples`); the job goes on.
- **Deletes are conditional**: a target version is deleted only if it is
  still the one the job compared, so a client's write to the target is
  never lost to a decision made before it.
- **Only the job's own keys disappear**: a target key that the source does
  not have is deleted only when the job wrote it, when an `adopt` mirror
  owns the target, or with `deleteExtraneous` (mirrors). `merge` never
  deletes.
- **Encryption and retention are never dropped silently**:
  `allowPlaintext` and `dropRetention` must be set for that.

Clients can still write to a target bucket. A mirror overwrites changes
to the keys it copies at its next pass over them; use bucket policies to
keep writers off a mirror's target.

## Mirrors: following the source

| Source | How changes are found |
|---|---|
| any, writes through gos3 | every write, delete and metadata change made through gos3 (S3 API, lifecycle, other jobs) marks its key; after about a second, which batches bursts, the mirror runs and copies the marked keys. More than 100,000 marked keys turn into a full pass. Keys that fail are tried again after 30 seconds |
| native, writes by other tools | a full pass compares the source's listing with the job's records every `pollInterval` (default `5m`, at least `30s`): one full listing of the source per poll |
| any | a full **verify pass** compares everything every `verifyInterval` (default `24h`, at least `1m`) and after every restart, since marked keys are kept in memory only |

A mirror shows its lag in its status: `pending` (keys changed and not yet
copied, as of its last run), `lagSeconds` (the age of the oldest of
them), `lastPassAt`, `nextPassAt` (the next full pass) and `passes`.
Between runs it is `waiting` and holds no worker; **Run now**
(`POST /jobs/{id}/run`) starts a run at once, and on a running mirror
makes it run again right after.

Deleting the source or the target bucket fails the mirror.

## Progress

| Field | Meaning |
|---|---|
| `mode` | `identity` or `fingerprint` (see [above](#how-changes-are-detected)) |
| `keysScanned` | keys compared in the current (or last) full pass |
| `bytesDone` | the source bytes those keys hold, against `totalBytes` |
| `keysCopied`, `versionsCopied`, `bytesCopied` | keys written to (copied versions or updated metadata), versions and delete markers written, and their content bytes, over the job's life |
| `keysDeleted` | target keys removed because the source no longer has them |
| `totalKeys`, `totalBytes` | the source's keys and the bytes of the versions copied, counted when the job prepared |
| `bytesPerSecond`, `etaSeconds` | the copy rate (averaged over about a minute) and the estimated time left in the current pass |
| `conflicts`, `conflictSamples` | what the job will not do (locked versions, unmappable keys) in the current pass; the last 20 |
| `errors` | the last 20 keys that could not be copied, with the error and time |
| `warnings` | what the job skipped on purpose, such as a bucket policy that cannot be rewritten for the target |

The totals are counted once, so the estimate drifts when the source
changes a lot during the copy.

## Controlling jobs

| State | Meaning |
|---|---|
| `queued` | waiting for a free worker, first come first served |
| `running` | a pass is in progress |
| `waiting` | between mirror runs, or waiting for a retry after a failure (`lastError`, `failures`, `nextRunAt`) |
| `paused` | stopped until resumed |
| `succeeded`, `failed`, `canceled` | finished |

| Action | Effect |
|---|---|
| Pause | stops the job; a running pass is interrupted and the job resumes later from its last checkpoint |
| Resume | queues a paused job again; also retries a failed job from its checkpoint |
| Cancel | ends the job. What it copied stays in the target |
| Run now | starts a waiting job at once: a mirror run or a pending retry (409 for paused and finished jobs) |
| Delete | removes a finished job from the history, with its database |

- **Retries.** A run that fails because something is unavailable (a
  backing or remote service, a bucket's encryption keys or restic
  repository) or for an unexpected reason is retried after 1 s, then after
  twice as long each time, at most every 5 minutes, until it succeeds or
  the job is paused or canceled. Errors that running the job again would
  meet again fail it: a bucket that no longer exists, a target that cannot
  store the data as the options say (encryption, retention, versions), a
  conflict with another job. Fix the cause and resume it. A key that fails
  in a pass is retried in the next pass; in a copy's last pass it makes
  the pass fail at its end, and the retry continues from that key.
- **Concurrency.** At most `jobs.workers` jobs (default 2) run at once,
  restic snapshot jobs included; the others wait in `queued`. Within a
  job, `concurrency` keys (default 4, 1-32) are copied in parallel, and
  all copy and mirror jobs together copy at most 32 keys at once. See
  [configuration.md](configuration.md#jobs-and-restic-snapshots).
- **Dry run.** `"dryRun": true` validates and normalizes a spec (every
  option filled in) and returns it with warnings, without creating
  anything: an existing target with a lifecycle configuration, configuration
  that will replace the target's or will be skipped, a mode in which
  version IDs change, a native source polled for changes, versions or
  retention that will not be copied, data that may be stored unencrypted,
  deletes that will or will not happen, renamed keys. The web UI's wizard
  shows this summary before it starts a job.

## Persistence and restarts

Jobs are kept in the system directory (`<storage.path>/.gos3/`):

| Path | Content |
|---|---|
| `jobs/jobs.json` | every job: spec, state, progress and checkpoint (written atomically on every state change; progress and checkpoints at most every 10 seconds) |
| `jobs/<id>/state.db` | a copy or mirror job's own state: the fingerprints, version maps and the keys it wrote; deleted with the job |

- After a restart, jobs that were running are queued again and continue
  from their last checkpoint (keys after it are compared again, which
  copies nothing twice); paused jobs stay paused; waiting jobs keep their
  next run time. Mirrors start with a full pass.
- At shutdown the running passes are stopped before the storage closes,
  and continue at the next start.
- Finished jobs are kept for 30 days, at most 200 of them; older ones are
  deleted with their state directories.
- Without a system directory (a memory or s3 default backing without
  `storage.path`), jobs are kept in memory and lost at restart; the server
  logs a warning.
- A `jobs.json` written by a later gos3 version in another format stops
  the server at startup rather than being overwritten.
- The admin API never returns a job's checkpoint.

Back up `jobs/` with the rest of the system state
([operations.md](operations.md#backups)). Without it, mirrors stop and
must be created again (with `onExisting: "adopt"` or `"merge"` for their
targets).

## Permissions

| Request | Admin action |
|---|---|
| Create a job (`POST /jobs`) | `admin:CreateCopyJob` on **both** `arn:gos3:admin:::bucket/<source>` and `…/bucket/<target>`, evaluated before the spec is validated; it also covers creating the target |
| Read one job (`GET /jobs/{id}`) | `admin:GetJob` on every bucket of the job |
| A bucket's jobs (`GET /buckets/{name}/jobs`) | `admin:GetJob` on the bucket; only the jobs whose other bucket the caller may also read are listed |
| List every job (`GET /jobs`) | `admin:ListJobs` (service level) |
| Pause, resume, cancel, run, delete | `admin:UpdateJob` on every bucket of the job |

A job's buckets are its `source` and `target` labels (also for restic
restore jobs) or its `bucket` label (restic snapshot and prune jobs).

A job reads and writes as the server, not as its creator: S3 permissions,
bucket policies and object ACLs do not limit it, and it keeps running
after its creator loses the permissions it was created with.
`admin:CreateCopyJob` on a source therefore amounts to reading every
version, its metadata and the bucket's configuration, and on a target to
writing and deleting there (within the [safety rules](#safety-rules)). See
[security.md](security.md#copy-jobs-snapshots-and-restic-credentials).
The built-in `diagnostics` policy allows `ListJobs` and `GetJob`. The web
UI shows the Jobs page with the `viewJobs` capability (`admin:ListJobs`),
the copy actions with `manageJobs` (`admin:CreateCopyJob`) and the job
actions with `updateJobs` (`admin:UpdateJob`).

## Copy jobs, replicas and moves

| | Replica | Move | Copy job | Mirror job |
|---|---|---|---|---|
| Result | more copies of the **same** bucket | the same bucket on another backing | a second, independent bucket | a second bucket, kept up to date |
| Name | same | same | any | any |
| Clients notice | nothing; failover when a backing fails | nothing | a new bucket | a new bucket, behind by the mirror's lag |
| Native buckets | no | no | yes, as source and as target | yes, as source and as target |
| Version IDs | identical | identical | identical between fs, memory and wrapped buckets; the service's own into native buckets | as for copies |

Use **replicas and moves** for buckets gos3 stores itself, to protect them
or to move them between backings without clients noticing (see
[operations.md](operations.md#multiple-backings-and-replication)). Use
**copy and mirror jobs** for everything else:

- **Native buckets** can be neither replicated nor moved. To bring one
  onto another backing, copy it into a new bucket and switch the clients
  over; to keep a second copy of it, mirror it.
- **Changing a bucket's kind** (native to wrapped or back) or **renaming**
  it: copy it into a new bucket, then delete the old one.
- **Plain objects for other tools**: mirror a wrapped or fs bucket into a
  native bucket of an s3 backing, where every S3 tool reads the objects
  as they are.
- **A copy on another server**: point an s3 backing at the other gos3
  server (or any S3 service) and copy or mirror into it.

## Examples

The admin API needs signed requests whose body is signed too
([admin-api.md](admin-api.md#authentication--authorization)); curl's
`--aws-sigv4` signs a `--data` body. With `$AK`/`$SK` holding an access key
allowed the actions above:

```sh
API=http://gos3:9000/_gos3/admin/v1
SIGN=(--aws-sigv4 "aws:amz:us-east-1:s3" --user "$AK:$SK" -H 'Content-Type: application/json')

# Copy native bucket "photos" into a new wrapped bucket on the "cold" backing,
# keeping every version.
curl "${SIGN[@]}" "$API/jobs" --data '{
  "kind": "copy",
  "source": {"bucket": "photos"},
  "target": {"bucket": "photos-archive",
             "create": {"backing": "cold", "kind": "wrapped"}},
  "options": {"versions": "all"}}'

# Check a mirror first: validate it and show the warnings, create nothing.
curl "${SIGN[@]}" "$API/jobs" --data '{
  "kind": "mirror",
  "source": {"bucket": "invoices"},
  "target": {"bucket": "invoices-plain",
             "create": {"backing": "aws", "kind": "native", "remoteBucket": "acme-invoices"}},
  "options": {"versions": "current", "allowPlaintext": true, "configs": "none"},
  "dryRun": true}'

# Follow, pause and resume a job; list the failed ones.
curl "${SIGN[@]}" "$API/jobs/<id>"
curl "${SIGN[@]}" -X POST "$API/jobs/<id>/pause"
curl "${SIGN[@]}" -X POST "$API/jobs/<id>/resume"
curl "${SIGN[@]}" "$API/jobs?state=failed"
```

Sent again without `dryRun`, the second example keeps the current version
of every object of `invoices` as a plain object of the new remote bucket
`acme-invoices`, where other tools can read it; `allowPlaintext` is needed
when `invoices` holds encrypted versions, because native buckets store
objects as they are.

In the web UI, **Copy / Mirror…** in a bucket's settings header or in the
object browser opens a wizard: the target (an existing bucket, or a new
one with its backing, kind and container or remote bucket), the options,
and a review of the dry run before the job is created. The **Jobs** page
lists every job with its progress, estimate or lag and its last error,
filters them by kind, state and bucket, and pauses, resumes, cancels, runs
and deletes them (see [web-ui.md](web-ui.md#jobs)).
