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
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"go.etcd.io/bbolt"
)

const objectRecordSchema = 1
const maxObjectWriteRetries = 128

type objectRecord struct {
	Schema      int                `json:"schema"`
	Info        storage.ObjectInfo `json:"info"`
	OperationID string             `json:"operationId"`
	Prior       string             `json:"prior,omitempty"`
	Digest      string             `json:"digest"`
}

func latestObjectKey(bucket, key string) string {
	return "latest\x00" + bucket + "\x00" + key
}

func versionObjectKey(bucket, key, version string) string {
	return "version\x00" + bucket + "\x00" + key + "\x00" + version
}

func deletedVersionKey(bucket, key, version string) string {
	return "deleted\x00" + bucket + "\x00" + key + "\x00" + version
}

func objectOperationKey(operationID string) string { return "operation\x00" + operationID }

func (c *ObjectCatalog) committedOperation(intent ObjectPutIntent) (storage.ObjectInfo, bool, error) {
	id := hex.EncodeToString(intent.OperationID[:])
	raw, found, err := c.node.metadata.Get(MetadataVersions, objectOperationKey(id))
	if err != nil || !found {
		return storage.ObjectInfo{}, found, err
	}
	var record objectRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return storage.ObjectInfo{}, false, err
	}
	record, err = decodeObjectRecord(intent.Bucket, intent.Key, record.Info.VersionID, raw)
	if err != nil || record.OperationID != id {
		return storage.ObjectInfo{}, false, errors.New("cluster: corrupt object operation record")
	}
	digest := sha256.Sum256(intent.Payload)
	meta := intent.Meta
	if intent.DeleteMarker {
		meta = storage.ObjectMeta{ReplicationOrigin: intent.Meta.ReplicationOrigin}
	}
	if record.Digest != hex.EncodeToString(digest[:]) || !record.Info.LastModified.Equal(intent.CreatedAt) || record.Info.DeleteMarker != intent.DeleteMarker || (intent.VersionID != "" && record.Info.VersionID != intent.VersionID) || !sameObjectMeta(record.Info.ObjectMeta, meta) {
		return storage.ObjectInfo{}, false, errors.New("cluster: object operation ID reused with different content or metadata")
	}
	return record.Info, true, nil
}

func decodeObjectRecord(bucket, key, version string, data []byte) (objectRecord, error) {
	var record objectRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	operationID, idErr := hex.DecodeString(record.OperationID)
	digest, digestErr := hex.DecodeString(record.Digest)
	if record.Schema != objectRecordSchema || record.Info.Bucket != bucket || record.Info.Key != key || record.Info.VersionID != version ||
		record.Info.LastModified.IsZero() || len(operationID) != 16 || idErr != nil || len(digest) != sha256.Size || digestErr != nil ||
		record.Info.Size < 0 || (record.Info.DeleteMarker && record.Info.Size != 0) {
		return objectRecord{}, errors.New("cluster: corrupt object version record")
	}
	return record, nil
}

// ObjectPutIntent holds the exact bytes, metadata, identity and timestamp of
// an internal write across retries. The caller computes S3 ETag and checksum
// metadata before constructing the intent.
type ObjectPutIntent struct {
	OperationID [16]byte
	CreatedAt   time.Time
	Bucket      string
	Key         string
	// VersionID is set only when importing a version from another bucket.
	// Ordinary S3 writes leave it empty and follow bucket versioning.
	VersionID    string
	Payload      []byte
	Meta         storage.ObjectMeta
	DeleteMarker bool
	IfMatch      string
	IfNoneMatch  bool
	// CheckLatest is reevaluated against the latest version whenever a
	// concurrent commit changes it before this write's Raft transaction.
	CheckLatest func(storage.ObjectInfo, bool) error
	// These fields bind multipart completion to the upload and selected parts.
	Conditions     []MetadataCondition
	PrefixDigests  []MetadataPrefixDigest
	DeletePrefixes []MetadataPrefix
	ExtraChanges   []MetadataCommand
	BeforePublish  func(*storage.ObjectMeta, *storage.ObjectMeta, time.Time, func(string) []byte) error
}

func NewObjectPutIntent(bucket, key string, payload []byte, meta storage.ObjectMeta) (ObjectPutIntent, error) {
	intent := ObjectPutIntent{Bucket: bucket, Key: key, Payload: bytes.Clone(payload), Meta: meta, CreatedAt: time.Now().UTC()}
	_, err := rand.Read(intent.OperationID[:])
	return intent, err
}

// ObjectCatalog keeps versions in Raft commit order using explicit predecessor
// links. Null-version replacement relinks the old position atomically.
type ObjectCatalog struct {
	node *Node
}

func NewObjectCatalog(node *Node) *ObjectCatalog {
	return &ObjectCatalog{node: node}
}

func (c *ObjectCatalog) PutVersion(ctx context.Context, intent ObjectPutIntent) (storage.ObjectInfo, error) {
	return c.putVersion(ctx, intent, 0)
}

func (c *ObjectCatalog) putVersion(ctx context.Context, intent ObjectPutIntent, retries int) (storage.ObjectInfo, error) {
	if c == nil || c.node == nil || intent.Bucket == "" || intent.Key == "" || strings.ContainsRune(intent.Bucket, 0) || strings.ContainsRune(intent.Key, 0) || intent.CreatedAt.IsZero() || (intent.DeleteMarker && len(intent.Payload) != 0) || (intent.VersionID != "" && !storage.ValidImportedVersionID(intent.VersionID)) {
		return storage.ObjectInfo{}, errors.New("cluster: invalid object put intent")
	}
	operationID := hex.EncodeToString(intent.OperationID[:])
	digest := sha256.Sum256(intent.Payload)
	// The operation record survives replacement of a null version or
	// permanent deletion of a generated version.
	if prior, found, err := c.committedOperation(intent); err != nil {
		return storage.ObjectInfo{}, err
	} else if found {
		return prior, nil
	}
	// Older catalog records predate operation records; keep their retry path.
	if prior, found, err := c.node.metadata.Get(MetadataVersions, versionObjectKey(intent.Bucket, intent.Key, operationID)); err != nil {
		return storage.ObjectInfo{}, err
	} else if found {
		record, err := decodeObjectRecord(intent.Bucket, intent.Key, operationID, prior)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if record.OperationID != operationID || record.Digest != hex.EncodeToString(digest[:]) || !record.Info.LastModified.Equal(intent.CreatedAt) || !sameObjectMeta(record.Info.ObjectMeta, intent.Meta) {
			return storage.ObjectInfo{}, errors.New("cluster: object operation ID reused with different content or metadata")
		}
		return record.Info, nil
	}
	bucketRaw, found, err := c.node.GetMetadata(ctx, MetadataBuckets, bucketKey(intent.Bucket))
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if !found {
		return storage.ObjectInfo{}, storage.ErrBucketNotFound
	}
	bucket, err := decodeBucketRecord(intent.Bucket, bucketRaw)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if bucket.Versioning != storage.VersioningEnabled && bucket.Versioning != storage.VersioningSuspended && bucket.Versioning != storage.VersioningUnversioned {
		return storage.ObjectInfo{}, storage.ErrNotSupported
	}
	version := intent.VersionID
	if version == "" && bucket.Versioning == storage.VersioningEnabled {
		version = operationID
	} else if version == "" {
		version = storage.NullVersionID
	}
	latestRaw, latestFound, err := c.node.GetMetadata(ctx, MetadataVersions, latestObjectKey(intent.Bucket, intent.Key))
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	priorVersion := string(latestRaw)
	var current storage.ObjectInfo
	var currentRaw []byte
	if latestFound {
		priorRecord, found, err := c.node.metadata.Get(MetadataVersions, versionObjectKey(intent.Bucket, intent.Key, priorVersion))
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if !found {
			return storage.ObjectInfo{}, errors.New("cluster: latest object points to a missing version")
		}
		currentRaw = priorRecord
		decoded, err := decodeObjectRecord(intent.Bucket, intent.Key, priorVersion, priorRecord)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		current = decoded.Info
	}
	if intent.IfNoneMatch && latestFound && !current.DeleteMarker {
		return storage.ObjectInfo{}, storage.ErrPreconditionFailed
	}
	if intent.IfMatch != "" && (!latestFound || current.DeleteMarker || !objectETagMatches(intent.IfMatch, current.ETag)) {
		return storage.ObjectInfo{}, storage.ErrPreconditionFailed
	}
	if intent.CheckLatest != nil {
		if err := intent.CheckLatest(current, latestFound); err != nil {
			return storage.ObjectInfo{}, err
		}
	}
	originalMeta := intent.Meta
	var configConditions []MetadataCondition
	if intent.BeforePublish != nil {
		var currentMeta *storage.ObjectMeta
		if latestFound && !current.DeleteMarker {
			copy := current.ObjectMeta
			currentMeta = &copy
		}
		var configErr error
		seen := map[string]bool{}
		config := func(key string) []byte {
			if configErr != nil {
				return nil
			}
			value, found, err := c.node.GetMetadata(ctx, MetadataBuckets, bucketConfigKey(intent.Bucket, key))
			if err != nil {
				configErr = err
				return nil
			}
			if !seen[key] {
				configConditions = append(configConditions, MetadataCondition{Namespace: MetadataBuckets, Key: bucketConfigKey(intent.Bucket, key), Exists: found, Value: value})
				seen[key] = true
			}
			return value
		}
		if err := intent.BeforePublish(&intent.Meta, currentMeta, intent.CreatedAt, config); err != nil {
			return storage.ObjectInfo{}, err
		}
		if configErr != nil {
			return storage.ObjectInfo{}, configErr
		}
	}
	if bucket.Versioning == storage.VersioningUnversioned && intent.DeleteMarker && intent.VersionID == "" {
		return storage.ObjectInfo{}, storage.ErrNotSupported
	}
	var replacedRaw []byte
	var replaced objectRecord
	var newerRaw []byte
	var newer objectRecord
	if version == storage.NullVersionID || intent.VersionID != "" {
		if latestFound && priorVersion == version {
			// The latest version may be overwritten in place (notably the null
			// version in suspended mode). Compare the exact bytes used by
			// CheckLatest, even if another writer replaces them meanwhile.
			replacedRaw, found = currentRaw, true
		} else {
			replacedRaw, found, err = c.node.GetMetadata(ctx, MetadataVersions, versionObjectKey(intent.Bucket, intent.Key, version))
			if err != nil {
				return storage.ObjectInfo{}, err
			}
		}
		if found {
			replaced, err = decodeObjectRecord(intent.Bucket, intent.Key, version, replacedRaw)
			if err != nil {
				return storage.ObjectInfo{}, err
			}
			if protectedObjectVersion(ctx, replaced.Info) {
				return storage.ObjectInfo{}, storage.ErrPreconditionFailed
			}
			if latestFound && priorVersion == version {
				priorVersion = replaced.Prior
			} else {
				newer, newerRaw, err = c.newerRecord(intent.Bucket, intent.Key, latestRaw, version)
				if err != nil {
					if retries < maxObjectWriteRetries {
						return c.putVersion(ctx, intent, retries+1)
					}
					return storage.ObjectInfo{}, err
				}
			}
		}
	}
	// A follower may have applied this operation during the read barriers.
	if prior, found, err := c.committedOperation(intent); err != nil {
		return storage.ObjectInfo{}, err
	} else if found {
		return prior, nil
	}
	info := storage.ObjectInfo{ObjectMeta: intent.Meta, Bucket: intent.Bucket, Key: intent.Key, VersionID: version,
		DeleteMarker: intent.DeleteMarker, Size: int64(len(intent.Payload)), LastModified: intent.CreatedAt.UTC(), IsLatest: true}
	if intent.DeleteMarker {
		info.ObjectMeta = storage.ObjectMeta{ReplicationOrigin: intent.Meta.ReplicationOrigin}
	}
	record := objectRecord{Schema: objectRecordSchema, Info: info, OperationID: operationID, Prior: priorVersion, Digest: hex.EncodeToString(digest[:])}
	data, err := json.Marshal(record)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	transaction := MetadataTransaction{OperationID: operationID,
		Conditions: []MetadataCondition{
			{Namespace: MetadataBuckets, Key: bucketKey(intent.Bucket), Exists: true, Value: bucketRaw},
			{Namespace: MetadataVersions, Key: latestObjectKey(intent.Bucket, intent.Key), Exists: latestFound, Value: latestRaw},
			{Namespace: MetadataVersions, Key: versionObjectKey(intent.Bucket, intent.Key, version), Exists: replacedRaw != nil, Value: replacedRaw},
			{Namespace: MetadataVersions, Key: objectOperationKey(operationID), Exists: false},
		},
		Changes: []MetadataCommand{
			{Namespace: MetadataVersions, Key: versionObjectKey(intent.Bucket, intent.Key, version), Value: data},
			{Namespace: MetadataVersions, Key: latestObjectKey(intent.Bucket, intent.Key), Value: []byte(version)},
			{Namespace: MetadataVersions, Key: objectOperationKey(operationID), Value: data},
			{Namespace: MetadataVersions, Key: deletedVersionKey(intent.Bucket, intent.Key, version), Delete: true},
		},
	}
	if newerRaw != nil {
		newer.Prior = replaced.Prior
		updated, err := json.Marshal(newer)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		expectedNewer := newerRaw
		if intent.CheckLatest != nil && latestFound && newer.Info.VersionID == current.VersionID {
			expectedNewer = currentRaw
		}
		transaction.Conditions = append(transaction.Conditions, MetadataCondition{Namespace: MetadataVersions, Key: versionObjectKey(intent.Bucket, intent.Key, newer.Info.VersionID), Exists: true, Value: expectedNewer})
		transaction.Changes = append(transaction.Changes, MetadataCommand{Namespace: MetadataVersions, Key: versionObjectKey(intent.Bucket, intent.Key, newer.Info.VersionID), Value: updated})
	}
	transaction.Conditions = append(transaction.Conditions, intent.Conditions...)
	if intent.CheckLatest != nil && latestFound && current.VersionID != version && (newerRaw == nil || newer.Info.VersionID != current.VersionID) {
		transaction.Conditions = append(transaction.Conditions, MetadataCondition{Namespace: MetadataVersions, Key: versionObjectKey(intent.Bucket, intent.Key, current.VersionID), Exists: true, Value: currentRaw})
	}
	transaction.PrefixDigests = append(transaction.PrefixDigests, intent.PrefixDigests...)
	transaction.Conditions = append(transaction.Conditions, configConditions...)
	transaction.DeletePrefixes = append(transaction.DeletePrefixes, intent.DeletePrefixes...)
	transaction.Changes = append(transaction.Changes, intent.ExtraChanges...)
	if err := c.node.AdmitTransactionWrite(ctx, intent.OperationID, intent.Bucket, intent.Key, intent.Payload, transaction); err != nil {
		if errors.Is(err, ErrMetadataCondition) {
			if intent.IfNoneMatch || intent.IfMatch != "" {
				latest, getErr := c.GetInfo(ctx, intent.Bucket, intent.Key, "")
				if intent.IfNoneMatch && getErr == nil && !latest.DeleteMarker {
					return storage.ObjectInfo{}, storage.ErrPreconditionFailed
				}
				if intent.IfMatch != "" && (getErr != nil || latest.DeleteMarker || !objectETagMatches(intent.IfMatch, latest.ETag)) {
					return storage.ObjectInfo{}, storage.ErrPreconditionFailed
				}
			}
			if len(intent.Conditions) != 0 || len(intent.DeletePrefixes) != 0 {
				return storage.ObjectInfo{}, storage.ErrConflict
			}
			// A concurrent unconditional writer can change the latest pointer
			// after our read. Its rejected transaction is durable, so reuse the
			// payload with a fresh operation/version ID and reread the chain.
			if !intent.IfNoneMatch && intent.IfMatch == "" && retries < maxObjectWriteRetries {
				if _, err := rand.Read(intent.OperationID[:]); err != nil {
					return storage.ObjectInfo{}, err
				}
				intent.Meta = originalMeta
				return c.putVersion(ctx, intent, retries+1)
			}
			if !intent.IfNoneMatch && intent.IfMatch == "" {
				return storage.ObjectInfo{}, storage.ErrConflict
			}
			return storage.ObjectInfo{}, storage.ErrPreconditionFailed
		}
		return storage.ObjectInfo{}, err
	}
	return info, nil
}

func sameObjectMeta(a, b storage.ObjectMeta) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func objectETagMatches(condition, etag string) bool {
	condition = strings.TrimSpace(condition)
	if condition == "*" {
		return true
	}
	return strings.Trim(condition, `"`) == etag
}

func protectedObjectVersion(ctx context.Context, info storage.ObjectInfo) bool {
	if info.LegalHold {
		return true
	}
	if info.RetentionMode == "" || !info.RetainUntil.After(time.Now()) {
		return false
	}
	return info.RetentionMode != "GOVERNANCE" || !storage.GovernanceBypass(ctx)
}

func (c *ObjectCatalog) lookupRecord(ctx context.Context, bucket, key, versionID string) (objectRecord, error) {
	if c == nil || c.node == nil || bucket == "" || key == "" {
		return objectRecord{}, errors.New("cluster: invalid object read")
	}
	if err := c.node.ReadBarrier(ctx); err != nil {
		return objectRecord{}, err
	}
	var raw []byte
	var isLatest bool
	err := c.node.metadata.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(stateBucket(MetadataBuckets)).Get([]byte(bucketKey(bucket))) == nil {
			return storage.ErrBucketNotFound
		}
		versions := tx.Bucket(stateBucket(MetadataVersions))
		latest := versions.Get([]byte(latestObjectKey(bucket, key)))
		if versionID == "" {
			if latest == nil {
				return storage.ErrObjectNotFound
			}
			versionID = string(latest)
		}
		isLatest = latest != nil && string(latest) == versionID
		if value := versions.Get([]byte(versionObjectKey(bucket, key, versionID))); value != nil {
			raw = bytes.Clone(value)
			return nil
		}
		return storage.ErrVersionNotFound
	})
	if err != nil {
		return objectRecord{}, err
	}
	record, err := decodeObjectRecord(bucket, key, versionID, raw)
	if err != nil {
		return objectRecord{}, err
	}
	record.Info.IsLatest = isLatest
	return record, nil
}

func (c *ObjectCatalog) GetInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	record, err := c.lookupRecord(ctx, bucket, key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return record.Info, nil
}

// GetVersion returns an exact committed version or the latest version when
// versionID is empty. A delete marker has no body.
func (c *ObjectCatalog) GetVersion(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, []byte, error) {
	record, err := c.lookupRecord(ctx, bucket, key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	if record.Info.DeleteMarker {
		return record.Info, nil, nil
	}
	var id [16]byte
	decoded, _ := hex.DecodeString(record.OperationID)
	copy(id[:], decoded)
	var expected [32]byte
	digestBytes, _ := hex.DecodeString(record.Digest)
	copy(expected[:], digestBytes)
	body, err := c.node.ReadVerifiedPayload(ctx, id, bucket, key, expected, record.Info.Size)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	digest := sha256.Sum256(body)
	if int64(len(body)) != record.Info.Size || hex.EncodeToString(digest[:]) != record.Digest {
		return storage.ObjectInfo{}, nil, errors.New("cluster: committed payload differs from version record")
	}
	return record.Info, body, nil
}

// ListVersions follows the predecessor chain established by ordered Raft
// commits. It returns newest first and rejects a missing link or cycle.
func (c *ObjectCatalog) ListVersions(ctx context.Context, bucket, key string) ([]storage.ObjectInfo, error) {
	if c == nil || c.node == nil || bucket == "" || key == "" {
		return nil, errors.New("cluster: invalid version list")
	}
	if err := c.node.ReadBarrier(ctx); err != nil {
		return nil, err
	}
	var versions []storage.ObjectInfo
	err := c.node.metadata.db.View(func(tx *bbolt.Tx) error {
		bucketState := tx.Bucket(stateBucket(MetadataVersions))
		version := string(bucketState.Get([]byte(latestObjectKey(bucket, key))))
		seen := make(map[string]bool)
		for version != "" {
			if seen[version] {
				return errors.New("cluster: cycle in object version chain")
			}
			seen[version] = true
			raw := bucketState.Get([]byte(versionObjectKey(bucket, key, version)))
			if raw == nil {
				return fmt.Errorf("cluster: missing object version %s in chain", version)
			}
			record, err := decodeObjectRecord(bucket, key, version, raw)
			if err != nil {
				return err
			}
			info := record.Info
			info.IsLatest = len(versions) == 0
			versions = append(versions, info)
			version = record.Prior
		}
		return nil
	})
	return versions, err
}

// walkSnapshot reads the latest pointer and its version chain for every key
// in one applied state-machine snapshot. Callbacks run after releasing bbolt.
func (c *ObjectCatalog) walkSnapshot(ctx context.Context, bucket, prefix string, allVersions bool) ([]storage.ObjectInfo, error) {
	if err := c.node.ReadBarrier(ctx); err != nil {
		return nil, err
	}
	var infos []storage.ObjectInfo
	root := []byte("latest\x00" + bucket + "\x00")
	err := c.node.metadata.db.View(func(tx *bbolt.Tx) error {
		state := tx.Bucket(stateBucket(MetadataVersions))
		cursor := state.Cursor()
		for k, v := cursor.Seek(root); k != nil && bytes.HasPrefix(k, root); k, v = cursor.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := string(k[len(root):])
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			version := string(v)
			seen := map[string]bool{}
			for version != "" {
				if seen[version] {
					return errors.New("cluster: cycle in object version chain")
				}
				seen[version] = true
				raw := state.Get([]byte(versionObjectKey(bucket, key, version)))
				if raw == nil {
					return fmt.Errorf("cluster: missing object version %s in chain", version)
				}
				record, err := decodeObjectRecord(bucket, key, version, raw)
				if err != nil {
					return err
				}
				info := record.Info
				info.IsLatest = len(seen) == 1
				if allVersions || !info.DeleteMarker {
					infos = append(infos, info)
				}
				if !allVersions {
					break
				}
				version = record.Prior
			}
		}
		return nil
	})
	return infos, err
}

func (c *ObjectCatalog) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("cluster: nil walk function")
	}
	infos, err := c.walkSnapshot(ctx, bucket, prefix, false)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if info.Key <= after {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(info); err != nil {
			if errors.Is(err, storage.ErrStopWalk) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (c *ObjectCatalog) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("cluster: nil walk function")
	}
	infos, err := c.walkSnapshot(ctx, bucket, prefix, true)
	if err != nil {
		return err
	}
	if keyMarker != "" {
		markerPosition := -1
		for i, info := range infos {
			if info.Key == keyMarker && info.VersionID == versionIDMarker {
				markerPosition = i
				break
			}
		}
		if versionIDMarker != "" && markerPosition >= 0 {
			infos = infos[markerPosition+1:]
		} else {
			// A page marker can have been deleted since the prior page. Keep
			// its predecessor link as a tombstone so pagination resumes at
			// the first surviving older version, even after several deletes.
			if versionIDMarker != "" && versionIDMarker != storage.NullVersionID {
				seen := map[string]bool{}
				for version := versionIDMarker; version != "" && !seen[version]; {
					seen[version] = true
					prior, found, err := c.node.metadata.Get(MetadataVersions, deletedVersionKey(bucket, keyMarker, version))
					if err != nil {
						return err
					}
					if !found {
						break
					}
					if len(prior) == 0 {
						for len(infos) > 0 && infos[0].Key <= keyMarker {
							infos = infos[1:]
						}
						break
					}
					version = string(prior)
					for i, info := range infos {
						if info.Key == keyMarker && info.VersionID == version {
							infos = infos[i:]
							markerPosition = i
							break
						}
					}
					if markerPosition >= 0 {
						break
					}
				}
			}
			if markerPosition >= 0 {
				goto walk
			}
			start := 0
			for start < len(infos) && (infos[start].Key < keyMarker || (infos[start].Key == keyMarker && versionIDMarker != storage.NullVersionID)) {
				start++
			}
			infos = infos[start:]
		}
	}

walk:
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(info); err != nil {
			if errors.Is(err, storage.ErrStopWalk) {
				return nil
			}
			return err
		}
	}
	return nil
}
