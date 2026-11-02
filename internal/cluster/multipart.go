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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

const multipartRecordSchema = 1

type multipartPart struct {
	Schema      int              `json:"schema"`
	Info        storage.PartInfo `json:"info"`
	OperationID string           `json:"operationId"`
	Digest      string           `json:"digest"`
}

func uploadKey(bucket, id string) string  { return "upload\x00" + bucket + "\x00" + id }
func uploadPrefix(bucket string) string   { return "upload\x00" + bucket + "\x00" }
func partPrefix(bucket, id string) string { return "part\x00" + bucket + "\x00" + id + "\x00" }
func partKey(bucket, id string, number int) string {
	return fmt.Sprintf("%s%05d", partPrefix(bucket, id), number)
}

func (b *S3Backend) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	if key == "" || strings.ContainsRune(key, 0) {
		return storage.UploadInfo{}, storage.ErrInvalidName
	}
	_, err := b.buckets.GetBucket(ctx, bucket)
	if err != nil {
		return storage.UploadInfo{}, clusterBackendError(err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return storage.UploadInfo{}, err
	}
	u := storage.UploadInfo{UploadID: hex.EncodeToString(id[:]), Bucket: bucket, Key: key, Initiated: time.Now().UTC(), Meta: meta, Initiator: initiator}
	raw, err := json.Marshal(u)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	bucketRaw, found, err := b.node.GetMetadata(ctx, MetadataBuckets, bucketKey(bucket))
	if err != nil {
		return storage.UploadInfo{}, clusterBackendError(err)
	}
	if !found {
		return storage.UploadInfo{}, storage.ErrBucketNotFound
	}
	tx := MetadataTransaction{OperationID: u.UploadID, Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: bucketKey(bucket), Exists: true, Value: bucketRaw}, {Namespace: MetadataJobs, Key: uploadKey(bucket, u.UploadID), Exists: false}}, Changes: []MetadataCommand{{Namespace: MetadataJobs, Key: uploadKey(bucket, u.UploadID), Value: raw}}}
	return u, clusterBackendError(b.node.AdmitTransactionWrite(ctx, id, bucket, key, nil, tx))
}

func (b *S3Backend) upload(ctx context.Context, bucket, key, id string) (storage.UploadInfo, []byte, error) {
	if _, err := b.buckets.GetBucket(ctx, bucket); err != nil {
		return storage.UploadInfo{}, nil, err
	}
	raw, found, err := b.node.GetMetadata(ctx, MetadataJobs, uploadKey(bucket, id))
	if err != nil {
		return storage.UploadInfo{}, nil, err
	}
	if !found {
		return storage.UploadInfo{}, nil, storage.ErrUploadNotFound
	}
	var u storage.UploadInfo
	if err := json.Unmarshal(raw, &u); err != nil || u.UploadID != id || u.Bucket != bucket || u.Initiated.IsZero() {
		return storage.UploadInfo{}, nil, errors.New("cluster: corrupt multipart upload")
	}
	if u.Key != key {
		return storage.UploadInfo{}, nil, storage.ErrUploadNotFound
	}
	return u, raw, nil
}

func (b *S3Backend) GetMultipartUpload(ctx context.Context, bucket, key, id string) (storage.UploadInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	u, _, err := b.upload(ctx, bucket, key, id)
	return u, clusterBackendError(err)
}

type clusterPartWriter struct {
	b                     *S3Backend
	bucket, key, uploadID string
	number                int
	mu                    sync.Mutex
	buf                   bytes.Buffer
	done                  bool
}

func (b *S3Backend) NewPartWriter(ctx context.Context, bucket, key, uploadID string, number int) (storage.PartWriter, error) {
	if number < 1 || number > 10000 {
		return nil, storage.ErrInvalidName
	}
	if _, err := b.GetMultipartUpload(ctx, bucket, key, uploadID); err != nil {
		return nil, err
	}
	return &clusterPartWriter{b: b, bucket: bucket, key: key, uploadID: uploadID, number: number}, nil
}

func (w *clusterPartWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errors.New("cluster: part writer already completed")
	}
	if int64(len(p)) > w.b.node.stageMax-int64(w.buf.Len()) {
		return 0, clusterBackendError(ErrStageFull)
	}
	return w.buf.Write(p)
}

func (w *clusterPartWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	w.buf.Reset()
	return nil
}

func (w *clusterPartWriter) Commit(ctx context.Context, meta storage.PartMeta) (storage.PartInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return storage.PartInfo{}, errors.New("cluster: part writer already completed")
	}
	w.done = true
	ctx, cancel := w.b.callContext(ctx)
	defer cancel()
	key := partKey(w.bucket, w.uploadID, w.number)
	body := w.buf.Bytes()
	digest := sha256.Sum256(body)
	info := storage.PartInfo{PartMeta: meta, PartNumber: w.number, Size: int64(len(body)), LastModified: time.Now().UTC()}
	for retries := 0; retries < 16; retries++ {
		_, uploadRaw, err := w.b.upload(ctx, w.bucket, w.key, w.uploadID)
		if err != nil {
			return storage.PartInfo{}, clusterBackendError(err)
		}
		bucketRaw, found, err := w.b.node.GetMetadata(ctx, MetadataBuckets, bucketKey(w.bucket))
		if err != nil {
			return storage.PartInfo{}, clusterBackendError(err)
		}
		if !found {
			return storage.PartInfo{}, storage.ErrBucketNotFound
		}
		prior, exists, err := w.b.node.GetMetadata(ctx, MetadataJobs, key)
		if err != nil {
			return storage.PartInfo{}, clusterBackendError(err)
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return storage.PartInfo{}, err
		}
		record, err := json.Marshal(multipartPart{Schema: multipartRecordSchema, Info: info, OperationID: hex.EncodeToString(id[:]), Digest: hex.EncodeToString(digest[:])})
		if err != nil {
			return storage.PartInfo{}, err
		}
		tx := MetadataTransaction{OperationID: hex.EncodeToString(id[:]), Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: bucketKey(w.bucket), Exists: true, Value: bucketRaw}, {Namespace: MetadataJobs, Key: uploadKey(w.bucket, w.uploadID), Exists: true, Value: uploadRaw}, {Namespace: MetadataJobs, Key: key, Exists: exists, Value: prior}}, Changes: []MetadataCommand{{Namespace: MetadataJobs, Key: key, Value: record}}}
		if err := w.b.node.AdmitTransactionWrite(ctx, id, w.bucket, w.key, body, tx); err != nil {
			if errors.Is(err, ErrMetadataCondition) {
				continue
			}
			return storage.PartInfo{}, clusterBackendError(err)
		}
		return info, nil
	}
	return storage.PartInfo{}, storage.ErrConflict
}

func decodeMultipartPart(raw []byte, number int) (multipartPart, error) {
	var p multipartPart
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	id, idErr := hex.DecodeString(p.OperationID)
	digest, digestErr := hex.DecodeString(p.Digest)
	if p.Schema != multipartRecordSchema || p.Info.PartNumber != number || p.Info.Size < 0 || p.Info.LastModified.IsZero() || idErr != nil || len(id) != 16 || digestErr != nil || len(digest) != sha256.Size {
		return p, errors.New("cluster: corrupt multipart part")
	}
	return p, nil
}

func (b *S3Backend) ListParts(ctx context.Context, bucket, key, id string) ([]storage.PartInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	if _, _, err := b.upload(ctx, bucket, key, id); err != nil {
		return nil, clusterBackendError(err)
	}
	records, err := b.node.ListMetadata(ctx, MetadataJobs, partPrefix(bucket, id))
	if err != nil {
		return nil, clusterBackendError(err)
	}
	out := make([]storage.PartInfo, 0, len(records))
	for _, r := range records {
		var number int
		if _, err := fmt.Sscanf(strings.TrimPrefix(r.Key, partPrefix(bucket, id)), "%05d", &number); err != nil {
			return nil, err
		}
		p, err := decodeMultipartPart(r.Value, number)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Info)
	}
	return out, nil
}

func (b *S3Backend) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	if _, err := b.buckets.GetBucket(ctx, bucket); err != nil {
		return nil, clusterBackendError(err)
	}
	records, err := b.node.ListMetadata(ctx, MetadataJobs, uploadPrefix(bucket))
	if err != nil {
		return nil, clusterBackendError(err)
	}
	out := make([]storage.UploadInfo, 0, len(records))
	for _, r := range records {
		var u storage.UploadInfo
		if err := json.Unmarshal(r.Value, &u); err != nil || u.Bucket != bucket || r.Key != uploadKey(bucket, u.UploadID) {
			return nil, errors.New("cluster: corrupt multipart upload")
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		if !out[i].Initiated.Equal(out[j].Initiated) {
			return out[i].Initiated.Before(out[j].Initiated)
		}
		return out[i].UploadID < out[j].UploadID
	})
	return out, nil
}

func (b *S3Backend) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	if fn == nil {
		return errors.New("cluster: nil upload walk function")
	}
	list, err := b.ListMultipartUploads(ctx, bucket)
	if err != nil {
		return err
	}
	for _, u := range list {
		if strings.HasPrefix(u.Key, prefix) && after.Precedes(u) {
			if err := fn(u); errors.Is(err, storage.ErrStopWalk) {
				return nil
			} else if err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *S3Backend) CompleteMultipartUpload(ctx context.Context, bucket, key, id string, numbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	_, uploadRaw, err := b.upload(ctx, bucket, key, id)
	if err != nil {
		return storage.ObjectInfo{}, clusterBackendError(err)
	}
	if len(numbers) == 0 || (opts.PartETags != nil && len(opts.PartETags) != len(numbers)) {
		return storage.ObjectInfo{}, storage.ErrPartNotFound
	}
	conditions := []MetadataCondition{{Namespace: MetadataJobs, Key: uploadKey(bucket, id), Exists: true, Value: uploadRaw}}
	partRecords, err := b.node.ListMetadata(ctx, MetadataJobs, partPrefix(bucket, id))
	if err != nil {
		return storage.ObjectInfo{}, clusterBackendError(err)
	}
	partHash := sha256.New()
	partsByKey := make(map[string][]byte, len(partRecords))
	for _, record := range partRecords {
		digestPrefixRecord(partHash, []byte(record.Key), record.Value)
		partsByKey[record.Key] = record.Value
	}
	body := make([]byte, 0)
	for i, number := range numbers {
		if number < 1 || number > 10000 {
			return storage.ObjectInfo{}, storage.ErrPartNotFound
		}
		raw, found := partsByKey[partKey(bucket, id, number)]
		if !found {
			return storage.ObjectInfo{}, storage.ErrPartNotFound
		}
		part, err := decodeMultipartPart(raw, number)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if opts.PartETags != nil && opts.PartETags[i] != part.Info.ETag {
			return storage.ObjectInfo{}, storage.ErrPartNotFound
		}
		var partID [16]byte
		decoded, _ := hex.DecodeString(part.OperationID)
		copy(partID[:], decoded)
		var expected [32]byte
		digestBytes, _ := hex.DecodeString(part.Digest)
		copy(expected[:], digestBytes)
		data, err := b.node.ReadVerifiedPayload(ctx, partID, bucket, key, expected, part.Info.Size)
		if err != nil {
			return storage.ObjectInfo{}, clusterBackendError(err)
		}
		digest := sha256.Sum256(data)
		if int64(len(data)) != part.Info.Size || hex.EncodeToString(digest[:]) != part.Digest {
			return storage.ObjectInfo{}, errors.New("cluster: multipart part payload mismatch")
		}
		if int64(len(data)) > b.node.stageMax-int64(len(body)) {
			return storage.ObjectInfo{}, clusterBackendError(ErrStageFull)
		}
		body = append(body, data...)
	}
	intent, err := NewObjectPutIntent(bucket, key, body, meta)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	intent.IfMatch, intent.IfNoneMatch = opts.IfMatch, opts.IfNoneMatch
	intent.BeforePublish = opts.BeforePublish
	intent.Conditions = conditions
	intent.PrefixDigests = []MetadataPrefixDigest{{Namespace: MetadataJobs, Prefix: partPrefix(bucket, id), SHA256: hex.EncodeToString(partHash.Sum(nil))}}
	intent.DeletePrefixes = []MetadataPrefix{{Namespace: MetadataJobs, Prefix: partPrefix(bucket, id)}}
	// Upload removal is in the same consensus commit as object publication.
	intent.ExtraChanges = []MetadataCommand{{Namespace: MetadataJobs, Key: uploadKey(bucket, id), Delete: true}}
	info, err := b.objects.PutVersion(ctx, intent)
	if errors.Is(err, storage.ErrConflict) {
		if _, _, checkErr := b.upload(ctx, bucket, key, id); checkErr != nil {
			return storage.ObjectInfo{}, clusterBackendError(checkErr)
		}
	}
	return info, clusterBackendError(err)
}

func (b *S3Backend) AbortMultipartUpload(ctx context.Context, bucket, key, id string) error {
	ctx, cancel := b.callContext(ctx)
	defer cancel()
	_, raw, err := b.upload(ctx, bucket, key, id)
	if err != nil {
		return clusterBackendError(err)
	}
	var op [16]byte
	if _, err := rand.Read(op[:]); err != nil {
		return err
	}
	tx := MetadataTransaction{OperationID: hex.EncodeToString(op[:]), Conditions: []MetadataCondition{{Namespace: MetadataJobs, Key: uploadKey(bucket, id), Exists: true, Value: raw}}, Changes: []MetadataCommand{{Namespace: MetadataJobs, Key: uploadKey(bucket, id), Delete: true}}, DeletePrefixes: []MetadataPrefix{{Namespace: MetadataJobs, Prefix: partPrefix(bucket, id)}}}
	if err := b.node.AdmitTransactionWrite(ctx, op, bucket, key, nil, tx); errors.Is(err, ErrMetadataCondition) {
		return storage.ErrUploadNotFound
	} else {
		return clusterBackendError(err)
	}
}
