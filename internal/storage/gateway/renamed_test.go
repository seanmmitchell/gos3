package gateway_test

import (
	"context"
	"io"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// renamed adapts a routed backend to the conformance suite:
//
//   - In per-bucket mode every gos3 bucket is a remote bucket of the same
//     name, and the suite's names ("b", "u", ...) are not valid S3 bucket
//     names, so every bucket name gets a prefix on the way in (and loses
//     it on the way out).
//   - BucketInfo.Backing is cleared (the suite expects the "" it passes).
type renamed struct {
	b      storage.Backend
	prefix string
}

var (
	_ storage.Backend            = renamed{}
	_ storage.ConditionalDeleter = renamed{}
	_ storage.UploadWalker       = renamed{}
)

func (r renamed) in(name string) string { return r.prefix + name }

func (r renamed) bucketOut(bi storage.BucketInfo) storage.BucketInfo {
	bi.Name = strings.TrimPrefix(bi.Name, r.prefix)
	bi.Backing = ""
	return bi
}

func (r renamed) objOut(o storage.ObjectInfo) storage.ObjectInfo {
	o.Bucket = strings.TrimPrefix(o.Bucket, r.prefix)
	return o
}

func (r renamed) upOut(u storage.UploadInfo) storage.UploadInfo {
	u.Bucket = strings.TrimPrefix(u.Bucket, r.prefix)
	return u
}

func (r renamed) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	list, err := r.b.ListBuckets(ctx)
	for i := range list {
		list[i] = r.bucketOut(list[i])
	}
	return list, err
}

func (r renamed) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	return r.b.CreateBucket(ctx, r.in(name), opts)
}

func (r renamed) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	bi, err := r.b.GetBucket(ctx, r.in(name))
	return r.bucketOut(bi), err
}

func (r renamed) DeleteBucket(ctx context.Context, name string) error {
	return r.b.DeleteBucket(ctx, r.in(name))
}

func (r renamed) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	return r.b.SetBucketVersioning(ctx, r.in(name), status)
}

func (r renamed) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	return r.b.SetBucketObjectLock(ctx, r.in(name), enabled)
}

func (r renamed) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	return r.b.GetBucketConfig(ctx, r.in(bucket), key)
}

func (r renamed) PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error {
	return r.b.PutBucketConfig(ctx, r.in(bucket), key, data)
}

func (r renamed) DeleteBucketConfig(ctx context.Context, bucket, key string) error {
	return r.b.DeleteBucketConfig(ctx, r.in(bucket), key)
}

type renamedWriter struct {
	storage.ObjectWriter
	r renamed
}

func (w renamedWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	o, err := w.ObjectWriter.Commit(ctx, meta, opts)
	return w.r.objOut(o), err
}

func (r renamed) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	w, err := r.b.NewObjectWriter(ctx, r.in(bucket), key)
	if err != nil {
		return nil, err
	}
	return renamedWriter{w, r}, nil
}

func (r renamed) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	o, err := r.b.GetObjectInfo(ctx, r.in(bucket), key, versionID)
	return r.objOut(o), err
}

func (r renamed) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	o, rd, err := r.b.OpenObject(ctx, r.in(bucket), key, versionID)
	return r.objOut(o), rd, err
}

func (r renamed) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	o, err := r.b.UpdateObjectMeta(ctx, r.in(bucket), key, versionID, fn)
	return r.objOut(o), err
}

func (r renamed) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	return r.b.DeleteObject(ctx, r.in(bucket), key, versionID)
}

func (r renamed) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	return r.b.(storage.ConditionalDeleter).DeleteObjectIf(ctx, r.in(bucket), key, versionID, func(o storage.ObjectInfo) error {
		return check(r.objOut(o))
	})
}

func (r renamed) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	return r.b.WalkObjects(ctx, r.in(bucket), prefix, after, func(o storage.ObjectInfo) error { return fn(r.objOut(o)) })
}

func (r renamed) WalkVersions(ctx context.Context, bucket, prefix, km, vm string, fn func(storage.ObjectInfo) error) error {
	return r.b.WalkVersions(ctx, r.in(bucket), prefix, km, vm, func(o storage.ObjectInfo) error { return fn(r.objOut(o)) })
}

func (r renamed) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	u, err := r.b.CreateMultipartUpload(ctx, r.in(bucket), key, meta, initiator)
	return r.upOut(u), err
}

func (r renamed) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	u, err := r.b.GetMultipartUpload(ctx, r.in(bucket), key, uploadID)
	return r.upOut(u), err
}

func (r renamed) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	return r.b.NewPartWriter(ctx, r.in(bucket), key, uploadID, partNumber)
}

func (r renamed) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	return r.b.ListParts(ctx, r.in(bucket), key, uploadID)
}

func (r renamed) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	list, err := r.b.ListMultipartUploads(ctx, r.in(bucket))
	for i := range list {
		list[i] = r.upOut(list[i])
	}
	return list, err
}

func (r renamed) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	return r.b.(storage.UploadWalker).WalkMultipartUploads(ctx, r.in(bucket), prefix, after, func(u storage.UploadInfo) error { return fn(r.upOut(u)) })
}

func (r renamed) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	o, err := r.b.CompleteMultipartUpload(ctx, r.in(bucket), key, uploadID, partNumbers, meta, opts)
	return r.objOut(o), err
}

func (r renamed) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	return r.b.AbortMultipartUpload(ctx, r.in(bucket), key, uploadID)
}

func (r renamed) Close() error { return r.b.Close() }
