// Package index is the in-memory object index shared by the backends that
// keep all their metadata in memory: memory (object data as byte slices)
// and s3store (object data as remote blobs, the index itself journaled to
// S3). One implementation keeps their versioning semantics identical.
//
// Design notes:
//   - The index is generic over the data payload P of versions and parts.
//     It never inspects P; the caller stores and reads it.
//   - A Bucket holds everything of one bucket: its info, configuration
//     documents, versions and multipart uploads. Info and Configs are plain
//     fields the caller maintains; the version operations read
//     Info.Versioning and Info.Name, and BeforePublish hooks read Configs.
//   - Nothing here locks. The caller guards each Bucket with its own lock
//     (shared for reads, exclusive for every mutation). The walk helpers
//     take an acquire function, so that they hold the lock only while
//     collecting a batch and call the walk function with no lock held. A
//     Clock is the exception: it is shared by all buckets of a backend and
//     locks itself.
//   - Keys of a bucket are kept in a skip list, so walks seek to
//     prefix/marker in O(log n) and then step in O(1) per key. The versions
//     of a key are ordered oldest first by stamp.
//   - Every version (including null versions and delete markers) is stamped
//     with a strictly increasing 64-bit sequence from a Clock (derived from
//     the wall clock in nanoseconds) that orders versions newest-first.
//     Generated version IDs embed this stamp: 16 lowercase hex digits of the
//     stamp followed by 8 random hex digits (24 characters, URL-safe,
//     time-ordered, never "null").
//   - Imported versions (Bucket.Import) get a fresh stamp but keep the
//     version ID they had on their source backend, which need not embed the
//     stamp; lookups by such IDs fall back to scanning the key's versions.
//   - Every operation that changes versions computes its exact effect as a
//     Change (versions removed, version added or metadata replaced), applies
//     it with Bucket.Apply and returns it. Replaying the returned changes in
//     order on another Bucket (after Put of a snapshot's versions) rebuilds
//     the same state, and the removed versions tell the caller which data
//     to release. Multipart operations (AddUpload, RemoveUpload,
//     Upload.PutPart) are deterministic as they are.
//   - Versions, uploads and parts reachable from the index are owned by it
//     once added: callers read them under their lock and never modify them.
//     Metadata handed out in ObjectInfo/UploadInfo values is deep-copied.
package index

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Version is one stored object version or delete marker.
type Version[P any] struct {
	// Stamp orders the versions of a key (see Clock); unique per backend.
	Stamp uint64
	// ID is the version ID: storage.NullVersionID, generated from Stamp
	// (NewVersionID), or imported.
	ID           string
	DeleteMarker bool
	// Imported is set for versions stored with a non-null version ID taken
	// from another backend (see Bucket.Import).
	Imported     bool
	Meta         storage.ObjectMeta
	LastModified time.Time
	// Size is the size of the content.
	Size int64
	// Compression is the algorithm Data is compressed with ("" = none);
	// StoredSize is then the number of stored bytes.
	Compression string
	StoredSize  int64
	// Data is the caller's reference to the stored bytes.
	Data P
}

// Content describes the stored data of a new version.
type Content[P any] struct {
	Data P
	// Size is the size of the content.
	Size int64
	// Compression is the algorithm Data is compressed with ("" = none);
	// StoredSize is then the number of stored bytes.
	Compression string
	StoredSize  int64
}

// Info builds a deep-copied ObjectInfo for version v of key in bucket.
func (v *Version[P]) Info(bucket, key string, isLatest bool) storage.ObjectInfo {
	oi := storage.ObjectInfo{
		Bucket:       bucket,
		Key:          key,
		VersionID:    v.ID,
		IsLatest:     isLatest,
		DeleteMarker: v.DeleteMarker,
		Size:         v.Size,
		LastModified: v.LastModified,
	}
	if !v.DeleteMarker {
		oi.ObjectMeta = CloneMeta(v.Meta)
		if v.Compression != "" {
			oi.Compression, oi.StoredSize = v.Compression, v.StoredSize
		}
	}
	return oi
}

// Object is the version history of one key. An object with no versions is
// removed from its bucket.
type Object[P any] struct {
	// versions is ordered oldest first (ascending stamp), so the latest
	// version is the last element.
	versions []*Version[P]
	// imported counts the versions with Imported set, whose IDs do not
	// embed their stamp.
	imported int
}

// Versions returns the versions of the key, oldest first. The slice and
// the versions belong to the index and must not be modified.
func (o *Object[P]) Versions() []*Version[P] { return o.versions }

// Latest returns the newest version of the key (possibly a delete marker).
func (o *Object[P]) Latest() *Version[P] { return o.versions[len(o.versions)-1] }

// indexByStamp returns the index of the version with the given stamp, or -1.
func (o *Object[P]) indexByStamp(stamp uint64) int {
	i := sort.Search(len(o.versions), func(i int) bool { return o.versions[i].Stamp >= stamp })
	if i < len(o.versions) && o.versions[i].Stamp == stamp {
		return i
	}
	return -1
}

// indexByID returns the index of the version with the given ID, or -1.
// Generated IDs are found by binary search on the stamp they embed; "null"
// and imported IDs (which may have any shape, including that of an ID
// generated by another backend, whose embedded stamp means nothing here)
// by a linear scan, which only runs when the key has such versions.
func (o *Object[P]) indexByID(id string) int {
	if stamp, ok := StampFromVersionID(id); ok {
		if i := o.indexByStamp(stamp); i >= 0 && o.versions[i].ID == id {
			return i
		}
	}
	if id != storage.NullVersionID && o.imported == 0 {
		return -1
	}
	for i := len(o.versions) - 1; i >= 0; i-- {
		if o.versions[i].ID == id {
			return i
		}
	}
	return -1
}

// put inserts v at the position of its stamp, replacing (and returning) a
// version with the same stamp.
func (o *Object[P]) put(v *Version[P]) *Version[P] {
	i := sort.Search(len(o.versions), func(i int) bool { return o.versions[i].Stamp >= v.Stamp })
	var old *Version[P]
	if i < len(o.versions) && o.versions[i].Stamp == v.Stamp {
		old = o.versions[i]
		if old.Imported {
			o.imported--
		}
		o.versions[i] = v
	} else {
		o.versions = slices.Insert(o.versions, i, v)
	}
	if v.Imported {
		o.imported++
	}
	return old
}

func (o *Object[P]) removeAt(i int) *Version[P] {
	v := o.versions[i]
	o.versions = slices.Delete(o.versions, i, i+1)
	if v.Imported {
		o.imported--
	}
	return v
}

// --- stamps and IDs ---

// Clock issues version stamps: strictly increasing values derived from the
// wall clock in nanoseconds. It is safe for concurrent use; a backend uses
// one Clock for all its buckets, so stamps are unique per backend. The zero
// value is ready to use.
type Clock struct {
	mu   sync.Mutex
	last uint64
}

// Next returns a stamp for a version created at now (normally the current
// time), and now in UTC. The stamp is now in Unix nanoseconds unless that
// is not greater than every stamp issued (or observed) before, in which
// case it is the last stamp plus one.
func (c *Clock) Next(now time.Time) (uint64, time.Time) {
	now = now.UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	s := uint64(now.UnixNano())
	if now.UnixNano() < 0 || s <= c.last {
		s = c.last + 1
	}
	c.last = s
	return s, now
}

// Last returns the greatest stamp issued or observed.
func (c *Clock) Last() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Observe records a stamp issued earlier (e.g. by a previous run whose
// index was reloaded), so that later stamps are greater.
func (c *Clock) Observe(stamp uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stamp > c.last {
		c.last = stamp
	}
}

const (
	stampHexLen = 16
	// VersionIDLength is the length of a generated version ID.
	VersionIDLength = stampHexLen + 8
)

// NewVersionID formats a version ID embedding stamp.
func NewVersionID(stamp uint64) string {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:]) // crypto/rand.Read never fails (Go >= 1.24)
	buf := make([]byte, 0, VersionIDLength)
	s := strconv.AppendUint(nil, stamp, 16)
	for i := len(s); i < stampHexLen; i++ {
		buf = append(buf, '0')
	}
	buf = append(buf, s...)
	buf = hex.AppendEncode(buf, rnd[:])
	return string(buf)
}

// StampFromVersionID extracts the stamp from an ID generated by
// NewVersionID.
func StampFromVersionID(id string) (uint64, bool) {
	if len(id) != VersionIDLength {
		return 0, false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return 0, false
		}
	}
	s, err := strconv.ParseUint(id[:stampHexLen], 16, 64)
	if err != nil {
		return 0, false
	}
	return s, true
}

// NewUploadID returns a random, URL-safe upload ID.
func NewUploadID() string {
	var rnd [24]byte
	_, _ = rand.Read(rnd[:])
	return hex.EncodeToString(rnd[:])
}

// ValidKey reports whether key can be stored. S3 keys are non-empty UTF-8;
// length limits are enforced by the S3 layer.
func ValidKey(key string) error {
	if key == "" || !utf8.ValidString(key) {
		return fmt.Errorf("%w: object key %q", storage.ErrInvalidName, key)
	}
	return nil
}

// --- clone helpers ---

// CloneMeta returns a deep copy of m.
func CloneMeta(m storage.ObjectMeta) storage.ObjectMeta {
	m.UserMeta = maps.Clone(m.UserMeta)
	m.Tags = maps.Clone(m.Tags)
	m.Checksums = maps.Clone(m.Checksums)
	m.PartSizes = slices.Clone(m.PartSizes)
	if m.PartChecksums != nil {
		pc := make([]map[string]string, len(m.PartChecksums))
		for i, c := range m.PartChecksums {
			pc[i] = maps.Clone(c)
		}
		m.PartChecksums = pc
	}
	return m
}

// ClonePartMeta returns a deep copy of m.
func ClonePartMeta(m storage.PartMeta) storage.PartMeta {
	m.Checksums = maps.Clone(m.Checksums)
	return m
}

// CloneUpload returns a deep copy of u.
func CloneUpload(u storage.UploadInfo) storage.UploadInfo {
	u.Meta = CloneMeta(u.Meta)
	return u
}

// checkCtx is ctx.Err() tolerating a nil ctx.
func checkCtx(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
