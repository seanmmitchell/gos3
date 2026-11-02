package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"go.etcd.io/raft/v3"
)

// S3Backend adapts the committed cluster catalogs to the storage contract.
type S3Backend struct {
	node             *Node
	buckets          *BucketCatalog
	objects          *ObjectCatalog
	operationTimeout time.Duration
}

var _ storage.Backend = (*S3Backend)(nil)
var _ storage.ConditionalMetaUpdater = (*S3Backend)(nil)
var _ storage.ConditionalDeleter = (*S3Backend)(nil)
var _ storage.NewerVersionsDeleter = (*S3Backend)(nil)
var _ storage.VersionImporter = (*S3Backend)(nil)

func NewS3Backend(node *Node) *S3Backend {
	return &S3Backend{node: node, buckets: NewBucketCatalog(node), objects: NewObjectCatalog(node), operationTimeout: 10 * time.Second}
}

// IsClusterLeader lets background reconciliations run once per cluster.
// The consensus admission path still fences a leader that loses its term
// while a scan is in progress.
func (b *S3Backend) IsClusterLeader() bool {
	return b.node.consensus.Status().RaftState == raft.StateLeader
}

func replicationProgressID(bucket string) string {
	digest := sha256.Sum256([]byte(bucket))
	return "replication_" + hex.EncodeToString(digest[:16])
}

// LoadReplicationProgress reads the leader's durable scan checkpoint through
// a Raft read barrier. The worker treats destination versions as the final
// per-version commit record and uses this checkpoint for backfill and retries.
func (b *S3Backend) LoadReplicationProgress(ctx context.Context, bucket string) ([]byte, bool, error) {
	if bucket == "" {
		return nil, false, storage.ErrInvalidName
	}
	return NewJobCatalog(b.node).Get(ctx, replicationProgressID(bucket))
}

func (b *S3Backend) SaveReplicationProgress(ctx context.Context, bucket string, expected []byte, exists bool, next []byte) error {
	if bucket == "" {
		return storage.ErrInvalidName
	}
	mutation, err := NewJobMutation(replicationProgressID(bucket), expected, exists, next)
	if err != nil {
		return err
	}
	return NewJobCatalog(b.node).CompareAndSwap(ctx, mutation)
}

func (b *S3Backend) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, b.operationTimeout)
}

func clusterBackendError(err error) error {
	if err != nil && (errors.Is(err, ErrInsufficientAcks) || errors.Is(err, ErrStageFull) ||
		errors.Is(err, ErrConsensusUnavailable) || errors.Is(err, ErrVerifiedReplicaUnavailable) ||
		errors.Is(err, raft.ErrProposalDropped) || errors.Is(err, raft.ErrStopped) ||
		errors.Is(err, context.DeadlineExceeded)) {
		return fmt.Errorf("%w: %w", storage.ErrBackingUnavailable, err)
	}
	return err
}

func (b *S3Backend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	list, err := b.buckets.ListBuckets(ctx)
	return list, clusterBackendError(err)
}

func (b *S3Backend) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	intent, err := NewBucketCreateIntent(name, opts)
	if err != nil {
		return err
	}
	_, err = b.buckets.CreateBucket(ctx, intent)
	return clusterBackendError(err)
}

// CreateBucketForJob uses a stable Raft operation ID derived from a persisted
// job ID. A copy job can retry after a crash between bucket creation and its
// next checkpoint without adopting a bucket another request created.
func (b *S3Backend) CreateBucketForJob(ctx context.Context, jobID string, createdAt time.Time, name string, opts storage.CreateBucketOptions) (storage.BucketInfo, error) {
	if b == nil || b.node == nil || jobID == "" || createdAt.IsZero() {
		return storage.BucketInfo{}, storage.ErrInvalidName
	}
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	digest := sha256.Sum256([]byte("cluster-copy-target\x00" + jobID))
	var operationID [16]byte
	copy(operationID[:], digest[:16])
	intent := BucketCreateIntent{OperationID: operationID, CreatedAt: createdAt.UTC(), Name: name, Options: opts}
	info, err := b.buckets.CreateBucket(ctx, intent)
	return info, clusterBackendError(err)
}

func (b *S3Backend) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	info, err := b.buckets.GetBucket(ctx, name)
	return info, clusterBackendError(err)
}

func (b *S3Backend) DeleteBucket(ctx context.Context, name string) error {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	return clusterBackendError(b.buckets.DeleteBucket(ctx, name))
}

func (b *S3Backend) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	return clusterBackendError(b.buckets.SetBucketVersioning(ctx, name, status))
}

func (b *S3Backend) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	return clusterBackendError(b.buckets.SetBucketObjectLock(ctx, name, enabled))
}

func bucketConfigKey(bucket, key string) string { return bucketConfigPrefix(bucket) + key }

func (b *S3Backend) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	if _, err := b.GetBucket(ctx, bucket); err != nil {
		return nil, err
	}
	value, found, err := b.node.GetMetadata(ctx, MetadataBuckets, bucketConfigKey(bucket, key))
	if err != nil {
		return nil, clusterBackendError(err)
	}
	if !found {
		return nil, storage.ErrConfigNotFound
	}
	return value, nil
}

func (b *S3Backend) PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error {
	if key == "" || strings.ContainsRune(bucket, 0) || strings.ContainsRune(key, 0) {
		return storage.ErrInvalidName
	}
	return b.changeBucketConfig(ctx, bucket, key, data, false)
}

func (b *S3Backend) DeleteBucketConfig(ctx context.Context, bucket, key string) error {
	if key == "" || strings.ContainsRune(bucket, 0) || strings.ContainsRune(key, 0) {
		return storage.ErrInvalidName
	}
	return b.changeBucketConfig(ctx, bucket, key, nil, true)
}

func (b *S3Backend) changeBucketConfig(ctx context.Context, bucket, key string, value []byte, remove bool) error {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	for {
		current, found, err := b.node.GetMetadata(ctx, MetadataBuckets, bucketKey(bucket))
		if err != nil {
			return clusterBackendError(err)
		}
		if !found {
			return storage.ErrBucketNotFound
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		transaction := MetadataTransaction{OperationID: hex.EncodeToString(id[:]),
			Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: bucketKey(bucket), Exists: true, Value: current}},
			Changes:    []MetadataCommand{{Namespace: MetadataBuckets, Key: bucketConfigKey(bucket, key), Value: value, Delete: remove}},
		}
		if err := b.node.AdmitTransactionWrite(ctx, id, bucket, key, value, transaction); !errors.Is(err, ErrMetadataCondition) {
			return clusterBackendError(err)
		}
	}
}

// PutReplicationConfiguration publishes rules and their activation state in
// one Raft transaction. A failed or uncertain proposal cannot leave a new
// document paired with an old activation time (or the inverse).
func (b *S3Backend) PutReplicationConfiguration(ctx context.Context, bucket string, rules, state []byte) error {
	if bucket == "" || strings.ContainsRune(bucket, 0) || len(rules) == 0 || len(state) == 0 {
		return storage.ErrInvalidName
	}
	return b.changeReplicationConfiguration(ctx, bucket, rules, state, false)
}

func (b *S3Backend) DeleteReplicationConfiguration(ctx context.Context, bucket string) error {
	if bucket == "" || strings.ContainsRune(bucket, 0) {
		return storage.ErrInvalidName
	}
	return b.changeReplicationConfiguration(ctx, bucket, nil, nil, true)
}

func (b *S3Backend) changeReplicationConfiguration(ctx context.Context, bucket string, rules, state []byte, remove bool) error {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	for {
		current, found, err := b.node.GetMetadata(ctx, MetadataBuckets, bucketKey(bucket))
		if err != nil {
			return clusterBackendError(err)
		}
		if !found {
			return storage.ErrBucketNotFound
		}
		if !remove {
			info, err := decodeBucketRecord(bucket, current)
			if err != nil {
				return err
			}
			if info.Versioning != storage.VersioningEnabled {
				return storage.ErrPreconditionFailed
			}
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		transaction := MetadataTransaction{OperationID: hex.EncodeToString(id[:]),
			Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: bucketKey(bucket), Exists: true, Value: current}},
			Changes: []MetadataCommand{
				{Namespace: MetadataBuckets, Key: bucketConfigKey(bucket, storage.ConfigReplication), Value: rules, Delete: remove},
				{Namespace: MetadataBuckets, Key: bucketConfigKey(bucket, storage.ConfigReplicationState), Value: state, Delete: remove},
			},
		}
		payload := append(bytes.Clone(rules), state...)
		if err := b.node.AdmitTransactionWrite(ctx, id, bucket, storage.ConfigReplication, payload, transaction); !errors.Is(err, ErrMetadataCondition) {
			return clusterBackendError(err)
		}
	}
}

func (b *S3Backend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	if _, err := b.GetBucket(ctx, bucket); err != nil {
		return nil, err
	}
	return &clusterObjectWriter{backend: b, bucket: bucket, key: key}, nil
}

type clusterObjectWriter struct {
	mu      sync.Mutex
	backend *S3Backend
	bucket  string
	key     string
	buf     bytes.Buffer
	done    bool
}

func (w *clusterObjectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errors.New("cluster: object writer already completed")
	}
	if int64(len(p)) > w.backend.node.stageMax-int64(w.buf.Len()) {
		return 0, clusterBackendError(ErrStageFull)
	}
	return w.buf.Write(p)
}

func (w *clusterObjectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	ctx, cancel := w.backend.callContext(ctx)
	defer cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return storage.ObjectInfo{}, errors.New("cluster: object writer already completed")
	}
	w.done = true
	intent, err := NewObjectPutIntent(w.bucket, w.key, w.buf.Bytes(), meta)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	intent.IfMatch, intent.IfNoneMatch = opts.IfMatch, opts.IfNoneMatch
	intent.BeforePublish = opts.BeforePublish
	info, err := w.backend.objects.PutVersion(ctx, intent)
	return info, clusterBackendError(err)
}

func (w *clusterObjectWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	w.buf.Reset()
	return nil
}

// NewVersionWriter lets replication retain a source version's ID, timestamp,
// metadata and delete-marker identity in the shared cluster namespace.
func (b *S3Backend) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	if _, err := b.GetBucket(ctx, bucket); err != nil {
		return nil, err
	}
	return &clusterVersionWriter{clusterObjectWriter: &clusterObjectWriter{backend: b, bucket: bucket, key: key}}, nil
}

type clusterVersionWriter struct{ *clusterObjectWriter }

func (w *clusterVersionWriter) Commit(ctx context.Context, info storage.ObjectInfo) (storage.ObjectInfo, error) {
	ctx, cancel := w.backend.callContext(ctx)
	defer cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return storage.ObjectInfo{}, errors.New("cluster: imported version writer already completed")
	}
	w.done = true
	if !storage.ValidImportedVersionID(info.VersionID) || info.LastModified.IsZero() || (info.DeleteMarker && w.buf.Len() != 0) {
		return storage.ObjectInfo{}, storage.ErrInvalidName
	}
	meta := info.ObjectMeta
	if info.DeleteMarker {
		meta = storage.ObjectMeta{ReplicationOrigin: info.ReplicationOrigin}
	}
	intent, err := NewObjectPutIntent(w.bucket, w.key, w.buf.Bytes(), meta)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	intent.CreatedAt = info.LastModified.UTC()
	intent.VersionID = info.VersionID
	intent.DeleteMarker = info.DeleteMarker
	stored, err := w.backend.objects.PutVersion(ctx, intent)
	return stored, clusterBackendError(err)
}

func (b *S3Backend) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	info, err := b.objects.GetInfo(ctx, bucket, key, versionID)
	return info, clusterBackendError(err)
}

type byteReadSeekCloser struct{ *bytes.Reader }

func (byteReadSeekCloser) Close() error { return nil }

func (b *S3Backend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	info, body, err := b.objects.GetVersion(ctx, bucket, key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, nil, clusterBackendError(err)
	}
	if info.DeleteMarker {
		return info, nil, nil
	}
	return info, byteReadSeekCloser{bytes.NewReader(body)}, nil
}

func (b *S3Backend) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	info, err := b.objects.UpdateMeta(ctx, bucket, key, versionID, fn)
	return info, clusterBackendError(err)
}

func (b *S3Backend) UpdateObjectMetaIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	if check == nil {
		return storage.ObjectInfo{}, errors.New("cluster: nil metadata update check")
	}
	info, err := b.objects.UpdateMetaIf(ctx, bucket, key, versionID, check, fn)
	return info, clusterBackendError(err)
}

func (b *S3Backend) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	if versionID != "" {
		result, err := b.objects.DeleteVersion(ctx, bucket, key, versionID)
		return result, clusterBackendError(err)
	}
	bucketInfo, err := b.GetBucket(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if bucketInfo.Versioning == storage.VersioningUnversioned {
		_, err := b.objects.DeleteVersion(ctx, bucket, key, storage.NullVersionID)
		if errors.Is(err, storage.ErrVersionNotFound) || errors.Is(err, storage.ErrObjectNotFound) {
			return storage.DeleteResult{}, nil
		}
		return storage.DeleteResult{}, clusterBackendError(err)
	}
	intent, err := NewObjectPutIntent(bucket, key, nil, storage.ObjectMeta{})
	if err != nil {
		return storage.DeleteResult{}, err
	}
	intent.DeleteMarker = true
	info, err := b.objects.PutVersion(ctx, intent)
	if err != nil {
		return storage.DeleteResult{}, clusterBackendError(err)
	}
	return storage.DeleteResult{VersionID: info.VersionID, DeleteMarker: true}, nil
}

// DeleteObjectIf evaluates check against a quorum-confirmed version and
// commits only if that version is still unchanged when the Raft transaction
// applies. A changed latest pointer causes the check to run again.
func (b *S3Backend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("cluster: nil conditional delete check")
	}
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	if versionID != "" {
		result, err := b.objects.DeleteVersionChecked(ctx, bucket, key, versionID, check)
		return result, clusterBackendError(err)
	}
	bucketInfo, err := b.buckets.GetBucket(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, clusterBackendError(err)
	}
	if bucketInfo.Versioning == storage.VersioningUnversioned {
		result, err := b.objects.DeleteVersionChecked(ctx, bucket, key, storage.NullVersionID, check)
		if errors.Is(err, storage.ErrVersionNotFound) {
			return storage.DeleteResult{}, storage.ErrObjectNotFound
		}
		return result, clusterBackendError(err)
	}
	intent, err := NewObjectPutIntent(bucket, key, nil, storage.ObjectMeta{})
	if err != nil {
		return storage.DeleteResult{}, err
	}
	intent.DeleteMarker = true
	intent.CheckLatest = func(info storage.ObjectInfo, found bool) error {
		if !found {
			return storage.ErrObjectNotFound
		}
		return check(info)
	}
	info, err := b.objects.PutVersion(ctx, intent)
	if err != nil {
		return storage.DeleteResult{}, clusterBackendError(err)
	}
	return storage.DeleteResult{VersionID: info.VersionID, DeleteMarker: true}, nil
}

func (b *S3Backend) DeleteVersionIf(ctx context.Context, bucket, key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if versionID == "" || check == nil {
		return storage.DeleteResult{}, errors.New("cluster: invalid conditional version delete")
	}
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	result, err := b.objects.DeleteVersionWithNewer(ctx, bucket, key, versionID, limit, check)
	return result, clusterBackendError(err)
}

func (b *S3Backend) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	if _, err := b.GetBucket(ctx, bucket); err != nil {
		return err
	}
	return clusterBackendError(b.objects.WalkObjects(ctx, bucket, prefix, after, fn))
}

func (b *S3Backend) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	if _, err := b.GetBucket(ctx, bucket); err != nil {
		return err
	}
	return clusterBackendError(b.objects.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn))
}

func (b *S3Backend) Close() error { return nil }
