package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

const bucketRecordSchema = 1

type bucketRecord struct {
	Schema     int                `json:"schema"`
	InstanceID string             `json:"instanceId"`
	Info       storage.BucketInfo `json:"info"`
}

func bucketKey(name string) string          { return "bucket/" + name }
func bucketConfigPrefix(name string) string { return "config\x00" + name + "\x00" }

func encodeBucketRecord(info storage.BucketInfo) ([]byte, error) {
	return json.Marshal(bucketRecord{Schema: bucketRecordSchema, InstanceID: info.InstanceID, Info: info})
}

func decodeBucketRecord(name string, data []byte) (storage.BucketInfo, error) {
	var record bucketRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return storage.BucketInfo{}, err
	}
	id, err := hex.DecodeString(record.InstanceID)
	if err != nil || len(id) != 16 || record.Schema != bucketRecordSchema || record.Info.Name != name || record.Info.CreatedAt.IsZero() {
		return storage.BucketInfo{}, errors.New("cluster: corrupt bucket record")
	}
	record.Info.InstanceID = record.InstanceID
	return record.Info, nil
}

// BucketCatalog is the logical bucket namespace shared by all voters. Its
// reads use Raft read barriers and its creates use atomic absent-key checks.
// The storage adapter must still materialize bucket data and route its other
// mutations through the cluster before the server can enable cluster mode.
type BucketCatalog struct{ node *Node }

func NewBucketCatalog(node *Node) *BucketCatalog { return &BucketCatalog{node: node} }

// BucketCreateIntent carries the generated identity and timestamp across
// internal retries. Recreating an intent with the same operation ID but a
// different timestamp is rejected by Raft's operation deduplication.
type BucketCreateIntent struct {
	OperationID [16]byte
	CreatedAt   time.Time
	Name        string
	Options     storage.CreateBucketOptions
}

func NewBucketCreateIntent(name string, opts storage.CreateBucketOptions) (BucketCreateIntent, error) {
	intent := BucketCreateIntent{Name: name, Options: opts, CreatedAt: time.Now().UTC()}
	_, err := rand.Read(intent.OperationID[:])
	return intent, err
}

func (c *BucketCatalog) CreateBucket(ctx context.Context, intent BucketCreateIntent) (storage.BucketInfo, error) {
	if c == nil || c.node == nil || intent.Name == "" || strings.Contains(intent.Name, "/") || intent.CreatedAt.IsZero() {
		return storage.BucketInfo{}, errors.New("cluster: invalid bucket catalog create")
	}
	info := storage.BucketInfo{Name: intent.Name, InstanceID: hex.EncodeToString(intent.OperationID[:]), CreatedAt: intent.CreatedAt.UTC(), Region: intent.Options.Region, Owner: intent.Options.Owner, ObjectLockEnabled: intent.Options.ObjectLockEnabled}
	if intent.Options.ObjectLockEnabled {
		info.Versioning = storage.VersioningEnabled
	}
	data, err := encodeBucketRecord(info)
	if err != nil {
		return storage.BucketInfo{}, err
	}
	transaction := MetadataTransaction{OperationID: hex.EncodeToString(intent.OperationID[:]),
		Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: bucketKey(intent.Name), Exists: false}},
		Changes:    []MetadataCommand{{Namespace: MetadataBuckets, Key: bucketKey(intent.Name), Value: data}},
	}
	if err := c.node.AdmitTransactionWrite(ctx, intent.OperationID, intent.Name, "", data, transaction); err != nil {
		if errors.Is(err, ErrMetadataCondition) {
			return storage.BucketInfo{}, storage.ErrBucketExists
		}
		return storage.BucketInfo{}, err
	}
	return info, nil
}

// SetBucketVersioning changes the shared bucket record by comparing the
// complete prior record in the Raft state machine. A concurrent update is
// reread and retried with a fresh operation ID under the caller's context.
func (c *BucketCatalog) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	if c == nil || c.node == nil || name == "" {
		return errors.New("cluster: bucket catalog unavailable")
	}
	if status != storage.VersioningEnabled && status != storage.VersioningSuspended {
		return storage.ErrPreconditionFailed
	}
	var instanceID string
	for {
		current, found, err := c.node.GetMetadata(ctx, MetadataBuckets, bucketKey(name))
		if err != nil {
			return err
		}
		if !found {
			return storage.ErrBucketNotFound
		}
		info, err := decodeBucketRecord(name, current)
		if err != nil {
			return err
		}
		if instanceID != "" && info.InstanceID != instanceID {
			return storage.ErrPreconditionFailed
		}
		instanceID = info.InstanceID
		if info.ObjectLockEnabled && status != storage.VersioningEnabled {
			return storage.ErrPreconditionFailed
		}
		if info.Versioning == status {
			return nil
		}
		info.Versioning = status
		updated, err := encodeBucketRecord(info)
		if err != nil {
			return err
		}
		var operationID [16]byte
		if _, err := rand.Read(operationID[:]); err != nil {
			return err
		}
		transaction := MetadataTransaction{OperationID: hex.EncodeToString(operationID[:]),
			Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: bucketKey(name), Exists: true, Value: current}},
			Changes:    []MetadataCommand{{Namespace: MetadataBuckets, Key: bucketKey(name), Value: updated}},
		}
		if err := c.node.AdmitTransactionWrite(ctx, operationID, name, "", updated, transaction); !errors.Is(err, ErrMetadataCondition) {
			return err
		}
	}
}

// SetBucketObjectLock publishes the bucket-level flag through the same
// compare-and-swap state machine as versioning and bucket configuration.
func (c *BucketCatalog) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	if c == nil || c.node == nil || name == "" {
		return errors.New("cluster: bucket catalog unavailable")
	}
	var instanceID string
	for {
		current, found, err := c.node.GetMetadata(ctx, MetadataBuckets, bucketKey(name))
		if err != nil {
			return err
		}
		if !found {
			return storage.ErrBucketNotFound
		}
		info, err := decodeBucketRecord(name, current)
		if err != nil {
			return err
		}
		if instanceID != "" && info.InstanceID != instanceID {
			return storage.ErrPreconditionFailed
		}
		instanceID = info.InstanceID
		if info.ObjectLockEnabled == enabled {
			return nil
		}
		info.ObjectLockEnabled = enabled
		if enabled {
			info.Versioning = storage.VersioningEnabled
		}
		updated, err := encodeBucketRecord(info)
		if err != nil {
			return err
		}
		id, err := newMutationID()
		if err != nil {
			return err
		}
		transaction := MetadataTransaction{OperationID: hex.EncodeToString(id[:]), Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: bucketKey(name), Exists: true, Value: current}}, Changes: []MetadataCommand{{Namespace: MetadataBuckets, Key: bucketKey(name), Value: updated}}}
		if err := c.node.AdmitTransactionWrite(ctx, id, name, "", updated, transaction); !errors.Is(err, ErrMetadataCondition) {
			return err
		}
	}
}

func (c *BucketCatalog) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	if c == nil || c.node == nil || name == "" {
		return storage.BucketInfo{}, errors.New("cluster: invalid bucket catalog read")
	}
	data, found, err := c.node.GetMetadata(ctx, MetadataBuckets, bucketKey(name))
	if err != nil {
		return storage.BucketInfo{}, err
	}
	if !found {
		return storage.BucketInfo{}, storage.ErrBucketNotFound
	}
	return decodeBucketRecord(name, data)
}

func (c *BucketCatalog) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	if c == nil || c.node == nil {
		return nil, errors.New("cluster: bucket catalog unavailable")
	}
	records, err := c.node.ListMetadata(ctx, MetadataBuckets, "bucket/")
	if err != nil {
		return nil, err
	}
	buckets := make([]storage.BucketInfo, 0, len(records))
	for _, record := range records {
		name := strings.TrimPrefix(record.Key, "bucket/")
		info, err := decodeBucketRecord(name, record.Value)
		if err != nil {
			return nil, fmt.Errorf("cluster: listing bucket %q: %w", name, err)
		}
		buckets = append(buckets, info)
	}
	return buckets, nil
}

// DeleteBucket atomically requires an empty version namespace and removes
// every configuration document in the same Raft-applied transaction. A
// concurrent object commit cannot pass its bucket-record condition after
// this deletion, while a preceding commit fails the prefix absence check.
func (c *BucketCatalog) DeleteBucket(ctx context.Context, name string) error {
	if c == nil || c.node == nil || name == "" {
		return errors.New("cluster: invalid bucket catalog delete")
	}
	var instanceID string
	for {
		current, found, err := c.node.GetMetadata(ctx, MetadataBuckets, bucketKey(name))
		if err != nil {
			return err
		}
		if !found {
			return storage.ErrBucketNotFound
		}
		info, err := decodeBucketRecord(name, current)
		if err != nil {
			return err
		}
		if instanceID != "" && info.InstanceID != instanceID {
			return storage.ErrBucketNotFound
		}
		instanceID = info.InstanceID
		latestPrefix := latestObjectKey(name, "")
		versionPrefix := "version\x00" + name + "\x00"
		for _, prefix := range []string{latestPrefix, versionPrefix} {
			hasVersions, err := c.node.HasMetadataPrefix(ctx, MetadataVersions, prefix)
			if err != nil {
				return err
			}
			if hasVersions {
				return storage.ErrBucketNotEmpty
			}
		}
		id, err := newMutationID()
		if err != nil {
			return err
		}
		transaction := MetadataTransaction{OperationID: hex.EncodeToString(id[:]),
			Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: bucketKey(name), Exists: true, Value: current}},
			AbsentPrefixes: []MetadataPrefix{
				{Namespace: MetadataVersions, Prefix: latestPrefix},
				{Namespace: MetadataVersions, Prefix: versionPrefix},
			},
			Changes:        []MetadataCommand{{Namespace: MetadataBuckets, Key: bucketKey(name), Delete: true}},
			DeletePrefixes: []MetadataPrefix{{Namespace: MetadataBuckets, Prefix: bucketConfigPrefix(name)}, {Namespace: MetadataJobs, Prefix: uploadPrefix(name)}, {Namespace: MetadataJobs, Prefix: "part\x00" + name + "\x00"}},
		}
		transaction.DeletePrefixes = append(transaction.DeletePrefixes, MetadataPrefix{Namespace: MetadataVersions, Prefix: "deleted\x00" + name + "\x00"})
		if err := c.node.AdmitTransactionWrite(ctx, id, name, "", nil, transaction); !errors.Is(err, ErrMetadataCondition) {
			return err
		}
	}
}
