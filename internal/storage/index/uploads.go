package index

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Upload is an in-progress multipart upload of a bucket.
type Upload[P any] struct {
	// Info describes the upload; it must not be modified once the upload
	// is added.
	Info  storage.UploadInfo
	parts map[int]*Part[P]
}

// Part is one committed part of an upload.
type Part[P any] struct {
	Info storage.PartInfo
	// Data is the caller's reference to the part's bytes.
	Data P
}

// Part returns part number n.
func (u *Upload[P]) Part(n int) (*Part[P], bool) {
	p, ok := u.parts[n]
	return p, ok
}

// Parts returns the committed parts sorted by part number.
func (u *Upload[P]) Parts() []*Part[P] {
	out := make([]*Part[P], 0, len(u.parts))
	for _, p := range u.parts {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, c *Part[P]) int { return a.Info.PartNumber - c.Info.PartNumber })
	return out
}

// PutPart stores p (kept, not copied) as part p.Info.PartNumber, replacing
// and returning a part with the same number.
func (u *Upload[P]) PutPart(p *Part[P]) *Part[P] {
	old := u.parts[p.Info.PartNumber]
	u.parts[p.Info.PartNumber] = p
	return old
}

// SelectParts returns the parts to assemble a completed upload from, in the
// order of numbers. With non-nil etags (one per number, see
// storage.CommitOptions.PartETags) each part's current ETag must match.
// A missing or replaced part yields storage.ErrPartNotFound.
func (u *Upload[P]) SelectParts(numbers []int, etags []string) ([]*Part[P], error) {
	if etags != nil && len(etags) != len(numbers) {
		return nil, fmt.Errorf("index: %d part ETags for %d parts", len(etags), len(numbers))
	}
	parts := make([]*Part[P], 0, len(numbers))
	for i, n := range numbers {
		p, ok := u.parts[n]
		if !ok {
			return nil, fmt.Errorf("%w: part %d of upload %q", storage.ErrPartNotFound, n, u.Info.UploadID)
		}
		if etags != nil && p.Info.ETag != etags[i] {
			return nil, fmt.Errorf("%w: part %d of upload %q was replaced (ETag %q, want %q)",
				storage.ErrPartNotFound, n, u.Info.UploadID, p.Info.ETag, etags[i])
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// NewUploadID returns a random upload ID not used by an upload of b.
func (b *Bucket[P]) NewUploadID() string {
	id := NewUploadID()
	for _, dup := b.uploads[id]; dup; _, dup = b.uploads[id] {
		id = NewUploadID()
	}
	return id
}

// AddUpload registers a new upload described by a copy of info, replacing
// an upload with the same ID, and returns it.
func (b *Bucket[P]) AddUpload(info storage.UploadInfo) *Upload[P] {
	b.RemoveUpload(info.UploadID)
	u := &Upload[P]{Info: CloneUpload(info), parts: make(map[int]*Part[P])}
	b.uploads[u.Info.UploadID] = u
	b.uploadOrder.Set(uploadOrderKey(u.Info.Key, u.Info.Initiated, u.Info.UploadID), u)
	return u
}

// Upload returns the upload with the given ID if it belongs to key, or
// storage.ErrUploadNotFound.
func (b *Bucket[P]) Upload(key, uploadID string) (*Upload[P], error) {
	u, ok := b.uploads[uploadID]
	if !ok || u.Info.Key != key {
		return nil, fmt.Errorf("%w: %q", storage.ErrUploadNotFound, uploadID)
	}
	return u, nil
}

// RemoveUpload unregisters the upload with the given ID and returns it
// (nil if there is none); its parts are the caller's to release.
func (b *Bucket[P]) RemoveUpload(uploadID string) *Upload[P] {
	u, ok := b.uploads[uploadID]
	if !ok {
		return nil
	}
	delete(b.uploads, uploadID)
	b.uploadOrder.Delete(uploadOrderKey(u.Info.Key, u.Info.Initiated, u.Info.UploadID))
	return u
}

// ClearUploads unregisters every upload and returns them in listing order.
func (b *Bucket[P]) ClearUploads() []*Upload[P] {
	out := b.Uploads()
	clear(b.uploads)
	b.uploadOrder = newSkipList[*Upload[P]]()
	return out
}

// Uploads returns the uploads in listing order: by key, then initiation
// time (oldest first), then upload ID.
func (b *Bucket[P]) Uploads() []*Upload[P] {
	out := make([]*Upload[P], 0, len(b.uploads))
	for n := b.uploadOrder.First(); n != nil; n = n.Next() {
		out = append(out, n.val)
	}
	return out
}

// WalkUploads implements storage.UploadWalker.WalkMultipartUploads: it
// calls fn for the uploads with the given key prefix after position after,
// in listing order, reading them in batches as the other walk helpers do.
func WalkUploads[P any](ctx context.Context, acquire func() (*Bucket[P], func(), error), prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	start := string(appendEscapedKey(nil, prefix))
	cursor, inclusive := uploadSeekKey(after)
	if cursor < start {
		cursor, inclusive = start, true
	}
	batch := walkMinBatch
	for {
		bk, unlock, err := acquire()
		if err != nil {
			return err
		}
		var n *slNode[*Upload[P]]
		if inclusive {
			n = bk.uploadOrder.SeekGE(cursor)
		} else {
			n = bk.uploadOrder.SeekGT(cursor)
		}
		items := make([]storage.UploadInfo, 0, batch)
		done := true
		for ; n != nil && strings.HasPrefix(n.key, start); n = n.Next() {
			if len(items) == batch {
				done = false
				break
			}
			cursor, inclusive = n.key, false
			if strings.HasPrefix(n.val.Info.Key, prefix) {
				items = append(items, CloneUpload(n.val.Info))
			}
		}
		unlock()
		if stop, err := callWalk(items, fn); stop || err != nil {
			return err
		}
		if done {
			return nil
		}
		batch = nextBatch(batch)
	}
}

// appendEscapedKey appends key with every 0x00 byte escaped as 0x00 0xFF.
// Followed by a terminator starting with 0x00 and a byte below 0xFF, the
// byte-wise order of escaped keys is the order of the keys and no escaped
// key is a prefix of another key's terminated encoding.
func appendEscapedKey(dst []byte, key string) []byte {
	for i := 0; i < len(key); i++ {
		dst = append(dst, key[i])
		if key[i] == 0 {
			dst = append(dst, 0xff)
		}
	}
	return dst
}

// uploadOrderKey returns the uploadOrder key of an upload: the escaped
// object key, the terminator 0x00 0x01, the initiation time (sign-flipped
// big-endian Unix seconds and nanoseconds) and the upload ID, so that the
// byte-wise order of keys is the listing order (key, Initiated, UploadID).
func uploadOrderKey(key string, initiated time.Time, uploadID string) string {
	b := make([]byte, 0, len(key)+14+len(uploadID))
	b = appendEscapedKey(b, key)
	b = append(b, 0x00, 0x01)
	b = binary.BigEndian.AppendUint64(b, uint64(initiated.Unix())^(1<<63))
	b = binary.BigEndian.AppendUint32(b, uint32(initiated.Nanosecond()))
	return string(append(b, uploadID...))
}

// uploadSeekKey returns where a walk after position p starts: at the
// returned key (inclusive) or strictly after it.
func uploadSeekKey(p storage.UploadPosition) (key string, inclusive bool) {
	switch {
	case p == storage.UploadPosition{}:
		return "", true
	case p.AfterKey:
		// 0x00 0x02 sorts after the terminator 0x00 0x01 of every upload of
		// p.Key and before every escaped greater key.
		return string(append(appendEscapedKey(nil, p.Key), 0x00, 0x02)), true
	default:
		return uploadOrderKey(p.Key, p.Initiated, p.UploadID), false
	}
}
