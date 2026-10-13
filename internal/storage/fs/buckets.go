package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"
	"unicode/utf8"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// ListBuckets returns all buckets sorted by name.
func (b *Backend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	if err := b.begin(ctx); err != nil {
		return nil, err
	}
	var out []storage.BucketInfo
	err := b.view(func(tx *bbolt.Tx) error {
		out = out[:0]
		return tx.Bucket(topBuckets).ForEach(func(k, v []byte) error {
			var rec bucketRecord
			if err := decodeRecord(v, &rec); err != nil {
				return fmt.Errorf("bucket %q: %w", k, err)
			}
			out = append(out, rec.info(string(k)))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []storage.BucketInfo{}
	}
	return out, nil
}

// CreateBucket creates a bucket. Any non-empty, valid UTF-8 name of at most
// 1024 bytes is accepted (validation is the S3 layer's job); other names
// yield ErrInvalidName.
func (b *Backend) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	if err := b.begin(ctx); err != nil {
		return err
	}
	if name == "" || len(name) > maxBucketNameLen || !utf8.ValidString(name) {
		return fmt.Errorf("%w: bucket %.64q", storage.ErrInvalidName, name)
	}
	return b.update(func(tx *bbolt.Tx) error {
		buckets := tx.Bucket(topBuckets)
		if buckets.Get([]byte(name)) != nil {
			return fmt.Errorf("%w: %q", storage.ErrBucketExists, name)
		}
		var id string
		for {
			id = randomHex(bucketIDLen / 2)
			if tx.Bucket(bucketDataName(id)) == nil {
				break
			}
		}
		bb, err := tx.CreateBucket(bucketDataName(id))
		if err != nil {
			return err
		}
		for _, sub := range perBucketSubs {
			if _, err := bb.CreateBucket(sub); err != nil {
				return err
			}
		}
		rec := bucketRecord{
			ID:                id,
			CreatedAt:         time.Now().UTC(),
			Region:            opts.Region,
			ObjectLockEnabled: opts.ObjectLockEnabled,
			Owner:             opts.Owner,
		}
		if opts.ObjectLockEnabled {
			rec.Versioning = storage.VersioningEnabled
		}
		return putRecord(buckets, []byte(name), &rec)
	})
}

// GetBucket returns the bucket's info.
func (b *Backend) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	if err := b.begin(ctx); err != nil {
		return storage.BucketInfo{}, err
	}
	var info storage.BucketInfo
	err := b.view(func(tx *bbolt.Tx) error {
		rec, _, err := loadBucket(tx, name)
		if err != nil {
			return err
		}
		info = rec.info(name)
		return nil
	})
	return info, err
}

// DeleteBucket removes an empty bucket, its configuration documents and its
// data directory. In-progress multipart uploads do not count: they are
// aborted along with the bucket and their part directories removed.
func (b *Backend) DeleteBucket(ctx context.Context, name string) error {
	if err := b.begin(ctx); err != nil {
		return err
	}
	var (
		id      string
		uploads []string
	)
	err := b.update(func(tx *bbolt.Tx) error {
		uploads = nil
		rec, bb, err := loadBucket(tx, name)
		if err != nil {
			return err
		}
		if k, _ := bb.Bucket(subVersions).Cursor().First(); k != nil {
			return fmt.Errorf("%w: %q", storage.ErrBucketNotEmpty, name)
		}
		// The upload and part records go with the per-bucket data bucket;
		// only the global upload index needs explicit cleanup.
		top := tx.Bucket(topUploads)
		if err := bb.Bucket(subUploads).ForEach(func(k, _ []byte) error {
			uploads = append(uploads, string(k))
			return top.Delete(k)
		}); err != nil {
			return err
		}
		id = rec.ID
		if err := tx.DeleteBucket(bucketDataName(rec.ID)); err != nil {
			return err
		}
		return tx.Bucket(topBuckets).Delete([]byte(name))
	})
	if err != nil {
		return err
	}
	for _, u := range uploads {
		b.removeUploadDir(u)
	}
	if err := os.RemoveAll(b.bucketDataDir(id)); err != nil {
		b.log.Warn("fs: remove bucket data directory", "bucket", name, "bucket_id", id, "err", err)
	}
	return nil
}

// modifyBucket applies fn to the bucket record of name in a write transaction.
func (b *Backend) modifyBucket(ctx context.Context, name string, fn func(*bucketRecord) error) error {
	if err := b.begin(ctx); err != nil {
		return err
	}
	return b.update(func(tx *bbolt.Tx) error {
		rec, _, err := loadBucket(tx, name)
		if err != nil {
			return err
		}
		if err := fn(rec); err != nil {
			return err
		}
		return putRecord(tx.Bucket(topBuckets), []byte(name), rec)
	})
}

// SetBucketVersioning changes the versioning state. Reverting a bucket that
// has been versioned to VersioningUnversioned is rejected.
func (b *Backend) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	switch status {
	case storage.VersioningUnversioned, storage.VersioningEnabled, storage.VersioningSuspended:
	default:
		return fmt.Errorf("fs: invalid versioning status %q", status)
	}
	return b.modifyBucket(ctx, name, func(rec *bucketRecord) error {
		if status == storage.VersioningUnversioned && rec.Versioning != storage.VersioningUnversioned {
			return errors.New("fs: a versioned bucket cannot become unversioned")
		}
		rec.Versioning = status
		return nil
	})
}

// SetBucketObjectLock sets the bucket's object-lock flag.
func (b *Backend) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	return b.modifyBucket(ctx, name, func(rec *bucketRecord) error {
		rec.ObjectLockEnabled = enabled
		return nil
	})
}

func validConfigKey(key string) bool { return key != "" && len(key) <= maxConfigKeyLen }

// GetBucketConfig returns a copy of a configuration document.
func (b *Backend) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	if err := b.begin(ctx); err != nil {
		return nil, err
	}
	var data []byte
	err := b.view(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		var v []byte
		if validConfigKey(key) {
			v = bb.Bucket(subConfig).Get([]byte(key))
		}
		if v == nil {
			return fmt.Errorf("%w: %s/%.64s", storage.ErrConfigNotFound, bucket, key)
		}
		data = bytes.Clone(v)
		if data == nil {
			data = []byte{}
		}
		return nil
	})
	return data, err
}

// ViewBucketConfig calls fn with a stored configuration document inside a
// read transaction, without copying it (see storage.ConfigViewer): data
// points into the database's memory map and is only valid during the call.
func (b *Backend) ViewBucketConfig(ctx context.Context, bucket, key string, fn func([]byte) error) error {
	if err := b.begin(ctx); err != nil {
		return err
	}
	return b.view(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		var v []byte
		if validConfigKey(key) {
			v = bb.Bucket(subConfig).Get([]byte(key))
		}
		if v == nil {
			return fmt.Errorf("%w: %s/%.64s", storage.ErrConfigNotFound, bucket, key)
		}
		return fn(v)
	})
}

// PutBucketConfig stores a copy of a configuration document.
func (b *Backend) PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error {
	if !validConfigKey(key) {
		return fmt.Errorf("%w: config key %.64q", storage.ErrInvalidName, key)
	}
	if err := b.begin(ctx); err != nil {
		return err
	}
	return b.update(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		v := data
		if v == nil {
			v = []byte{}
		}
		return bb.Bucket(subConfig).Put([]byte(key), v)
	})
}

// DeleteBucketConfig removes a configuration document; it is idempotent.
func (b *Backend) DeleteBucketConfig(ctx context.Context, bucket, key string) error {
	if err := b.begin(ctx); err != nil {
		return err
	}
	return b.update(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		if !validConfigKey(key) {
			return nil
		}
		return bb.Bucket(subConfig).Delete([]byte(key))
	})
}
