package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/seanmmitchell/gos3/internal/storage"
	"go.etcd.io/bbolt"
)

func newMutationID() ([16]byte, error) {
	var id [16]byte
	_, err := rand.Read(id[:])
	return id, err
}

// objectMutationSnapshot is read from a single applied state-machine view.
// Conditions compare these exact bytes again when the Raft command applies.
type objectMutationSnapshot struct {
	bucketRaw  []byte
	latestRaw  []byte
	versionRaw []byte
	record     objectRecord
	versionID  string
	latest     bool
}

func (c *ObjectCatalog) newerRecord(bucket, key string, latestRaw []byte, targetVersion string) (objectRecord, []byte, error) {
	var newer objectRecord
	var newerRaw []byte
	err := c.node.metadata.db.View(func(tx *bbolt.Tx) error {
		state := tx.Bucket(stateBucket(MetadataVersions))
		seen := map[string]bool{}
		for v := string(latestRaw); v != ""; {
			if seen[v] {
				return errors.New("cluster: cycle in object version chain")
			}
			seen[v] = true
			raw := state.Get([]byte(versionObjectKey(bucket, key, v)))
			if raw == nil {
				return errors.New("cluster: broken object version chain")
			}
			record, err := decodeObjectRecord(bucket, key, v, raw)
			if err != nil {
				return err
			}
			if record.Prior == targetVersion {
				newer, newerRaw = record, bytes.Clone(raw)
				return nil
			}
			v = record.Prior
		}
		return errors.New("cluster: object version is not in the latest chain")
	})
	return newer, newerRaw, err
}

// newerChain returns versions nearest to the target first and a digest of
// every version record of this key. The digest binds the precondition's
// chain view to the eventual Raft transaction without a condition for each
// version in an arbitrarily long history.
func (c *ObjectCatalog) newerChain(bucket, key string, latestRaw []byte, targetVersion string, limit int) (objectRecord, []storage.ObjectInfo, string, error) {
	var immediate objectRecord
	var newer []storage.ObjectInfo
	var digest string
	err := c.node.metadata.db.View(func(tx *bbolt.Tx) error {
		state := tx.Bucket(stateBucket(MetadataVersions))
		prefix := []byte("version\x00" + bucket + "\x00" + key + "\x00")
		h := sha256.New()
		cursor := state.Cursor()
		for k, v := cursor.Seek(prefix); bytes.HasPrefix(k, prefix); k, v = cursor.Next() {
			digestPrefixRecord(h, k, v)
		}
		digest = hex.EncodeToString(h.Sum(nil))
		seen := map[string]bool{}
		latest := string(latestRaw)
		for version := latest; version != ""; {
			if seen[version] {
				return errors.New("cluster: cycle in object version chain")
			}
			seen[version] = true
			raw := state.Get([]byte(versionObjectKey(bucket, key, version)))
			if raw == nil {
				return errors.New("cluster: broken object version chain")
			}
			record, err := decodeObjectRecord(bucket, key, version, raw)
			if err != nil {
				return err
			}
			if version == targetVersion {
				slices.Reverse(newer)
				if len(newer) > limit {
					newer = newer[:limit]
				}
				return nil
			}
			info := record.Info
			info.IsLatest = version == latest
			newer = append(newer, info)
			if record.Prior == targetVersion {
				immediate = record
			}
			version = record.Prior
		}
		return errors.New("cluster: object version is not in the latest chain")
	})
	return immediate, newer, digest, err
}

func (c *ObjectCatalog) mutationSnapshot(ctx context.Context, bucket, key, versionID string) (objectMutationSnapshot, error) {
	if err := c.node.ReadBarrier(ctx); err != nil {
		return objectMutationSnapshot{}, err
	}
	var snap objectMutationSnapshot
	err := c.node.metadata.db.View(func(tx *bbolt.Tx) error {
		bucketState := tx.Bucket(stateBucket(MetadataBuckets))
		snap.bucketRaw = bytes.Clone(bucketState.Get([]byte(bucketKey(bucket))))
		if snap.bucketRaw == nil {
			return storage.ErrBucketNotFound
		}
		versionState := tx.Bucket(stateBucket(MetadataVersions))
		snap.latestRaw = bytes.Clone(versionState.Get([]byte(latestObjectKey(bucket, key))))
		if versionID == "" {
			if snap.latestRaw == nil {
				return storage.ErrObjectNotFound
			}
			versionID = string(snap.latestRaw)
		}
		snap.versionID = versionID
		snap.latest = snap.latestRaw != nil && string(snap.latestRaw) == versionID
		snap.versionRaw = bytes.Clone(versionState.Get([]byte(versionObjectKey(bucket, key, versionID))))
		if snap.versionRaw == nil {
			return storage.ErrVersionNotFound
		}
		record, err := decodeObjectRecord(bucket, key, versionID, snap.versionRaw)
		if err != nil {
			return err
		}
		snap.record = record
		snap.record.Info.IsLatest = snap.latest
		return nil
	})
	return snap, err
}

// UpdateMeta preserves the version identity and payload. The function runs
// outside bbolt, and a concurrent edit retries against fresh metadata.
func (c *ObjectCatalog) UpdateMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	return c.UpdateMetaIf(ctx, bucket, key, versionID, nil, fn)
}

func (c *ObjectCatalog) UpdateMetaIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if c == nil || c.node == nil || bucket == "" || key == "" || fn == nil {
		return storage.ObjectInfo{}, errors.New("cluster: invalid metadata update")
	}
	for attempts := 0; attempts < 16; attempts++ {
		snap, err := c.mutationSnapshot(ctx, bucket, key, versionID)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if snap.record.Info.DeleteMarker {
			return storage.ObjectInfo{}, fmt.Errorf("delete marker: %w", storage.ErrObjectNotFound)
		}
		info := snap.record.Info
		if check != nil {
			if err := check(info); err != nil {
				return storage.ObjectInfo{}, err
			}
		}
		if err := fn(&info.ObjectMeta); err != nil {
			return storage.ObjectInfo{}, err
		}
		updated := snap.record
		updated.Info = info
		data, err := json.Marshal(updated)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		id, err := newMutationID()
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		transaction := MetadataTransaction{OperationID: hex.EncodeToString(id[:]),
			Conditions: []MetadataCondition{
				{Namespace: MetadataBuckets, Key: bucketKey(bucket), Exists: true, Value: snap.bucketRaw},
				{Namespace: MetadataVersions, Key: versionObjectKey(bucket, key, snap.versionID), Exists: true, Value: snap.versionRaw},
			},
			Changes: []MetadataCommand{{Namespace: MetadataVersions, Key: versionObjectKey(bucket, key, snap.versionID), Value: data}},
		}
		if err := c.node.AdmitTransactionWrite(ctx, id, bucket, key, nil, transaction); err == nil {
			return info, nil
		} else if !errors.Is(err, ErrMetadataCondition) {
			return storage.ObjectInfo{}, err
		}
	}
	return storage.ObjectInfo{}, storage.ErrConflict
}

// DeleteVersion removes one exact version and relinks its newer neighbour in
// the same consensus commit. The payload becomes unreachable but remains on
// replicas until a separate garbage collector removes it.
func (c *ObjectCatalog) DeleteVersion(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	return c.deleteVersion(ctx, bucket, key, versionID, 1, nil)
}

// DeleteVersionChecked applies the caller's precondition to the same version
// bytes that the ordered Raft transaction compares before removing it.
func (c *ObjectCatalog) DeleteVersionChecked(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	return c.deleteVersion(ctx, bucket, key, versionID, 1, func(info storage.ObjectInfo, _ []storage.ObjectInfo) error { return check(info) })
}

// DeleteVersionWithNewer gives the precondition a bounded view of versions
// newer than its target. All traversed chain records are compared again in
// the same Raft transaction that relinks and deletes the target.
func (c *ObjectCatalog) DeleteVersionWithNewer(ctx context.Context, bucket, key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	return c.deleteVersion(ctx, bucket, key, versionID, limit, check)
}

func (c *ObjectCatalog) deleteVersion(ctx context.Context, bucket, key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if c == nil || c.node == nil || bucket == "" || key == "" || versionID == "" {
		return storage.DeleteResult{}, errors.New("cluster: invalid version delete")
	}
	limit = max(limit, 1)
	for attempts := 0; attempts < maxObjectWriteRetries; attempts++ {
		snap, err := c.mutationSnapshot(ctx, bucket, key, versionID)
		if err != nil {
			return storage.DeleteResult{}, err
		}
		var newer objectRecord
		var newerRaw []byte
		var newerVersions []storage.ObjectInfo
		var chainDigest string
		if !snap.latest {
			if check == nil {
				newer, newerRaw, err = c.newerRecord(bucket, key, snap.latestRaw, versionID)
			} else {
				newer, newerVersions, chainDigest, err = c.newerChain(bucket, key, snap.latestRaw, versionID, limit)
			}
			if err != nil {
				// The snapshot and chain walk are separate reads. A concurrent
				// delete can retire the snapshot's latest pointer in between.
				// Retry that ordinary race, but surface a stable broken chain.
				fresh, refreshErr := c.mutationSnapshot(ctx, bucket, key, versionID)
				if refreshErr != nil {
					return storage.DeleteResult{}, refreshErr
				}
				if !bytes.Equal(fresh.latestRaw, snap.latestRaw) || !bytes.Equal(fresh.versionRaw, snap.versionRaw) {
					continue
				}
				return storage.DeleteResult{}, err
			}
		}
		if check != nil {
			if err := check(snap.record.Info, newerVersions); err != nil {
				return storage.DeleteResult{}, err
			}
		}
		if protectedObjectVersion(ctx, snap.record.Info) {
			return storage.DeleteResult{}, storage.ErrPreconditionFailed
		}
		id, err := newMutationID()
		if err != nil {
			return storage.DeleteResult{}, err
		}
		transaction := MetadataTransaction{OperationID: hex.EncodeToString(id[:]),
			Conditions: []MetadataCondition{
				{Namespace: MetadataBuckets, Key: bucketKey(bucket), Exists: true, Value: snap.bucketRaw},
				{Namespace: MetadataVersions, Key: latestObjectKey(bucket, key), Exists: snap.latestRaw != nil, Value: snap.latestRaw},
				{Namespace: MetadataVersions, Key: versionObjectKey(bucket, key, versionID), Exists: true, Value: snap.versionRaw},
			},
			Changes: []MetadataCommand{
				{Namespace: MetadataVersions, Key: versionObjectKey(bucket, key, versionID), Delete: true},
				{Namespace: MetadataVersions, Key: deletedVersionKey(bucket, key, versionID), Value: []byte(snap.record.Prior)},
			},
		}
		if chainDigest != "" {
			transaction.PrefixDigests = append(transaction.PrefixDigests, MetadataPrefixDigest{Namespace: MetadataVersions, Prefix: "version\x00" + bucket + "\x00" + key + "\x00", SHA256: chainDigest})
		}
		if newerRaw != nil {
			transaction.Conditions = append(transaction.Conditions, MetadataCondition{Namespace: MetadataVersions, Key: versionObjectKey(bucket, key, newer.Info.VersionID), Exists: true, Value: newerRaw})
		}
		if snap.latest {
			transaction.Changes = append(transaction.Changes, MetadataCommand{Namespace: MetadataVersions, Key: latestObjectKey(bucket, key), Value: []byte(snap.record.Prior), Delete: snap.record.Prior == ""})
		} else {
			newer.Prior = snap.record.Prior
			data, err := json.Marshal(newer)
			if err != nil {
				return storage.DeleteResult{}, err
			}
			transaction.Changes = append(transaction.Changes, MetadataCommand{Namespace: MetadataVersions, Key: versionObjectKey(bucket, key, newer.Info.VersionID), Value: data})
		}
		if err := c.node.AdmitTransactionWrite(ctx, id, bucket, key, nil, transaction); err == nil {
			return storage.DeleteResult{VersionID: versionID, DeleteMarker: snap.record.Info.DeleteMarker}, nil
		} else if !errors.Is(err, ErrMetadataCondition) {
			return storage.DeleteResult{}, err
		}
	}
	return storage.DeleteResult{}, storage.ErrConflict
}
