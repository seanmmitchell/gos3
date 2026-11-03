# Cluster deployment and offline import

## Deploying a new cluster

Give every voter a stable node ID, its own persistent data directory, and a
distinct HTTPS peer origin. The cluster ID, complete peer list, root
credentials, replica count and acknowledgment policy must agree across all
voters. For example, the first voter of a two-node filesystem cluster uses:

```yaml
storage:
  backend: fs
  path: /var/lib/gos3-a
  fsync_data: true
cluster:
  enabled: true
  id: production
  node_id: a
  peers:
    - id: a
      url: https://a.example.net:9443
    - id: b
      url: https://b.example.net:9443
  tls_ca: /etc/gos3/peer-ca.pem
  tls_cert: /etc/gos3/a.pem
  tls_key: /etc/gos3/a.key
  replica_count: 2
  min_memory_acks: 2
  min_durable_acks: 2
  stage_memory_bytes: 268435456
```

Set `GOS3_ROOT_ACCESS_KEY` and `GOS3_ROOT_SECRET_KEY` identically on every
voter. The second voter uses `node_id: b`, its own data path and certificate,
and the same peer list. Each peer certificate must be signed by the configured
CA, cover its peer host in its TLS names, and contain exactly one URI identity:
`spiffe://gos3/production/a` or `spiffe://gos3/production/b`. The peer port
is separate from the public S3 and admin listener. Protect the peer port with
network controls as well as mutual TLS. Start both processes with `gos3 serve
--config node-a.yaml` and the corresponding configuration on the second node.
Point clients or a load balancer at the public endpoints only after both
report a healthy cluster at `GET /_gos3/admin/v1/cluster`.

The initial voter set is fixed. Do not change peer IDs or URLs in a running
cluster; use the offline membership migration below. Filesystem voters must
keep `fsync_data: true`. A node using an S3 backing needs a distinct home
prefix; its acknowledged conditional PUT is the backing's durability promise.
`stage_memory_bytes` bounds the complete payload bytes that a node can hold
before committing them. It defaults to 256 MiB and must be at least 1 MiB;
increase it on every eligible placement before accepting larger single objects
or multipart parts. An exhausted node returns a retryable service-unavailable
response.

## Importing a standalone server

Cluster import is an offline migration. Stop the standalone source before
exporting. The export command takes its exclusive data directory lock; it
refuses a running source. Keep the source stopped until every new voter has
been seeded from the **same** bundle and the cutover is complete.

```sh
gos3 cluster import export --source-config standalone.yaml --output /secure/import-bundle
gos3 cluster import seed --cluster-config node-a.yaml --bundle /secure/import-bundle
gos3 cluster import seed --cluster-config node-b.yaml --bundle /secure/import-bundle
```

Run `seed` on each voter with its own node configuration and an empty data
directory. Copy the bundle over a private channel. Its manifest includes IAM
keys and its payload directory contains all object bytes. Protect both as
server credentials. The command refuses an existing cluster or standalone
state and takes the server's target directory lock. A node initialized from
a different bundle is rejected by peer traffic, even when its cluster ID and
peer list match.

The importer checks every bucket, configuration document, object version,
delete marker, IAM document, and runtime setting. It reads each object twice:
once to inventory its metadata and digest, then while writing the bundle.
Seed verifies the manifest and every payload before creating the node's
metadata and Raft snapshot. If verification fails, correct the source or
bundle and start again with fresh empty targets. Do not merge independently
exported bundles.

The current importer refuses unfinished multipart uploads, encrypted object
versions, and individual versions larger than 256 MiB. Complete or abort
uploads before export. It also refuses persisted background jobs and restic
snapshot scheduler state, which cannot yet be represented in the cluster.
These are explicit limits of the current import path.

## Changing the voter set offline

Quiesce clients, finish or abort pending multipart uploads, then stop **every**
old voter. Keep them stopped throughout export,
seeding, and cutover. Supply one configuration file per old voter; each must
point to its original data directory and backing. The command takes each
directory's exclusive lock and rejects a running voter, an incomplete or
disagreeing Raft state, and any object without the old policy's minimum number
of verified durable copies.

```sh
gos3 cluster migrate export \
  --source-config old-a.yaml --source-config old-b.yaml \
  --output /secure/migration-bundle
gos3 cluster import seed --cluster-config new-c.yaml --bundle /secure/migration-bundle
gos3 cluster import seed --cluster-config new-d.yaml --bundle /secure/migration-bundle
```

Use a new cluster ID, new empty node directories, and the complete new peer
list in every new configuration. Keep the root credentials identical to the
old cluster. Seed every new voter from the same bundle, start the new cluster,
and verify buckets, versions, IAM, and settings through each entry node before
routing clients to it. Do not start the old cluster after cutover. Keep the old
data and bundle until the new cluster has been verified and backed up. If
export fails, repair the old cluster before retrying; do not seed from a
partial bundle. The same 256 MiB per-version and encrypted-object limits as
standalone import apply. Pending multipart records must be removed before
export. Shared Raft background job records are verified and carried into the
new bundle; unsupported local jobs and restic scheduler files still cause
export to fail. Imported job records are visible through the authenticated
admin jobs API on every voter. Pause, cancel, and delete changes require the
cluster acknowledgment policy. Cluster copy jobs can copy into an existing
versioned bucket or create a new cluster bucket while preserving encryption
and compression. Supported bucket configurations are copied before objects;
replication rules, their activation state, and restic snapshot schedules are
excluded. Compression preferences are skipped, and selected source encryption
settings are rejected
because cluster storage cannot apply them. Jobs run on the leader and resume
from committed key checkpoints after a leader change. Cluster mirror and
restic snapshot runners are not yet available; unsupported job requests are
rejected during validation.
All voters must use the source's root access and secret key; the bundle binds
them by a digest. Cluster node IDs, peer HTTPS origins, mutual TLS materials,
and local storage backings remain node specific.

With two voters and the default policy of two memory and two durable
acknowledgments, either voter failing stops writes. Reads also need Raft
quorum. An S3 replica's durable acknowledgment means its backing accepted a
conditional PUT, or an uncertain result was resolved by reading back the
exact bytes. It does not claim that the backing has completed a hardware
fsync. Filesystem replicas use an fsync enabled local commit. A read returns
`503 ServiceUnavailable` if no selected replica can provide bytes matching
the committed digest. Recover a failed voter from its retained
state when possible; the remaining node repairs missing committed copies
after it returns. Do not discard a node's Raft state or change the static
peer set while the cluster is running. Membership changes require a
controlled offline migration.

Cluster mode is opt-in. An unconfigured standalone server continues to serve
its own namespace. Keep the migration bundle until every voter has started
and the imported versions have been checked through each public endpoint.
