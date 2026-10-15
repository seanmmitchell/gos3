package s3store

import (
	"context"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/index"
)

// versionWriter is the storage.VersionWriter of the backend.
type versionWriter struct {
	*contentWriter
}

// NewVersionWriter starts writing an imported version of bucket/key (see
// storage.VersionImporter).
func (b *Backend) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	return b.NewVersionWriterWith(ctx, bucket, key, storage.WriteOptions{})
}

// NewVersionWriterWith is NewVersionWriter with write options (see
// storage.CompressingImporter): imported versions are stored compressed
// and encrypted (AES256 or restic) as asked, like new versions.
func (b *Backend) NewVersionWriterWith(ctx context.Context, bucket, key string, opts storage.WriteOptions) (storage.VersionWriter, error) {
	w, err := b.newContentWriter(ctx, bucket, key, opts)
	if err != nil {
		return nil, err
	}
	return &versionWriter{w}, nil
}

// Commit stores the written data as the newest version of the key with the
// identity given by info (see storage.VersionWriter).
func (w *versionWriter) Commit(ctx context.Context, info storage.ObjectInfo) (storage.ObjectInfo, error) {
	b, bk := w.b, w.bk
	c, key, err := w.finish(ctx, func(size int64) error { return index.CheckImport(w.bucket, w.key, info, size) })
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if key != "" {
		defer b.pending.done(key)
	}
	if err := b.relock(ctx, bk); err != nil {
		b.deleteLater(key)
		return storage.ObjectInfo{}, err
	}
	info.Encryption = w.encryption
	if info.DeleteMarker {
		info.Encryption = ""
	}
	out, ch, err := bk.ix.Import(w.key, info, c)
	if err != nil {
		bk.mu.Unlock()
		b.deleteLater(key)
		return storage.ObjectInfo{}, err
	}
	done := b.appendChange(bk, ch, "")
	bk.mu.Unlock()
	if err := await(ctx, done); err != nil {
		return storage.ObjectInfo{}, err
	}
	return out, nil
}

func (w *versionWriter) Abort() error {
	w.abort()
	return nil
}
