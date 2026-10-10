package fs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// The "upk" per-bucket index orders a bucket's in-progress multipart
// uploads for listing: uploadOrderKey(key, initiated, uploadID) -> upload
// ID. It lets ListMultipartUploads pages, the lifecycle abort pass and
// usage counts seek and stream instead of decoding and sorting every
// upload record of the bucket.

// uploadOrderKey returns the "upk" key of an upload: the escaped object key,
// the terminator 0x00 0x01, the initiation time (sign-flipped big-endian
// Unix seconds, then nanoseconds) and the upload ID, so that the byte-wise
// order of keys is the listing order (key, Initiated, UploadID).
func uploadOrderKey(key string, initiated time.Time, uploadID string) []byte {
	b := keyVersionsPrefix(key)
	b = binary.BigEndian.AppendUint64(b, uint64(initiated.Unix())^(1<<63))
	b = binary.BigEndian.AppendUint32(b, uint32(initiated.Nanosecond()))
	return append(b, uploadID...)
}

// uploadSeekKey returns where a walk after position p starts: at the
// returned key (inclusive) or strictly after it.
func uploadSeekKey(p storage.UploadPosition) (key []byte, inclusive bool) {
	switch {
	case p == storage.UploadPosition{}:
		return nil, true
	case p.AfterKey:
		return keyVersionsEnd(p.Key), true
	default:
		return uploadOrderKey(p.Key, p.Initiated, p.UploadID), false
	}
}

// putUploadIndex adds the "upk" entry of an upload.
func putUploadIndex(bb *bbolt.Bucket, uploadID string, rec *uploadRecord) error {
	return bb.Bucket(subUploadOrder).Put(uploadOrderKey(rec.Key, rec.Initiated, uploadID), []byte(uploadID))
}

// deleteUploadIndex removes the "upk" entry of an upload.
func deleteUploadIndex(bb *bbolt.Bucket, uploadID string, rec *uploadRecord) error {
	return bb.Bucket(subUploadOrder).Delete(uploadOrderKey(rec.Key, rec.Initiated, uploadID))
}

// ensureUploadIndexes creates the "upk" index of every bucket that lacks
// one (databases written before it existed) and rebuilds indexes whose
// entry count differs from the number of upload records (e.g. after an
// older gos3 version ran on the database). Runs once at Open.
func ensureUploadIndexes(tx *bbolt.Tx) error {
	return tx.Bucket(topBuckets).ForEach(func(name, v []byte) error {
		var br bucketRecord
		if err := decodeRecord(v, &br); err != nil {
			return fmt.Errorf("bucket %q: %w", name, err)
		}
		bb := tx.Bucket(bucketDataName(br.ID))
		if bb == nil {
			return nil
		}
		if idx := bb.Bucket(subUploadOrder); idx != nil {
			if idx.Stats().KeyN == bb.Bucket(subUploads).Stats().KeyN {
				return nil
			}
			if err := bb.DeleteBucket(subUploadOrder); err != nil {
				return err
			}
		}
		if _, err := bb.CreateBucket(subUploadOrder); err != nil {
			return err
		}
		return bb.Bucket(subUploads).ForEach(func(id, v []byte) error {
			var rec uploadRecord
			if err := decodeRecord(v, &rec); err != nil {
				return fmt.Errorf("bucket %q upload %q: %w", name, id, err)
			}
			return putUploadIndex(bb, string(id), &rec)
		})
	})
}

// WalkMultipartUploads reads uploads in batches of uploadWalkMinBatch,
// doubling up to uploadWalkMaxBatch, so that walks stopped early (a page of
// a listing) decode few records while long walks amortise transactions.
const (
	uploadWalkMinBatch = 16
	uploadWalkMaxBatch = 1024
)

// WalkMultipartUploads calls fn for the uploads with the given key prefix
// after position after, in listing order (see storage.UploadWalker). The
// index is read in batches, each in its own read transaction; fn runs
// outside any transaction.
func (b *Backend) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	if fn == nil {
		return errors.New("fs: nil walk function")
	}
	start := escapeKey(nil, prefix)
	cursor, inclusive := uploadSeekKey(after)
	if bytes.Compare(cursor, start) < 0 {
		cursor, inclusive = start, true
	}
	bucketID := ""
	batch := uploadWalkMinBatch
	for {
		if err := b.begin(ctx); err != nil {
			return err
		}
		items := make([]storage.UploadInfo, 0, batch)
		done := true
		err := b.view(func(tx *bbolt.Tx) error {
			items = items[:0]
			done = true
			var (
				br  *bucketRecord
				bb  *bbolt.Bucket
				err error
			)
			if bucketID == "" {
				br, bb, err = loadBucket(tx, bucket)
			} else {
				br, bb, err = loadBucketID(tx, bucket, bucketID)
			}
			if err != nil {
				return err
			}
			bucketID = br.ID
			upl := bb.Bucket(subUploads)
			c := bb.Bucket(subUploadOrder).Cursor()
			k, v := c.Seek(cursor)
			if !inclusive && bytes.Equal(k, cursor) {
				k, v = c.Next()
			}
			for ; k != nil && bytes.HasPrefix(k, start); k, v = c.Next() {
				if len(items) == batch {
					done = false
					break
				}
				cursor, inclusive = bytes.Clone(k), false
				raw := upl.Get(v)
				if raw == nil {
					continue // stale entry; cannot happen within one version
				}
				var rec uploadRecord
				if err := decodeRecord(raw, &rec); err != nil {
					return err
				}
				if !strings.HasPrefix(rec.Key, prefix) {
					continue
				}
				info := rec.info(bucket, string(v))
				info.Meta = cloneMeta(info.Meta)
				items = append(items, info)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := fn(it); err != nil {
				if errors.Is(err, storage.ErrStopWalk) {
					return nil
				}
				return err
			}
		}
		if done {
			return nil
		}
		batch = min(2*batch, uploadWalkMaxBatch)
	}
}
