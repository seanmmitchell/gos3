package index

import (
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Bucket is the index of one bucket. It is not safe for concurrent use (see
// the package comment); the zero value is not usable, use NewBucket.
type Bucket[P any] struct {
	// Info is the bucket's info; Name and Versioning drive the version
	// operations.
	Info storage.BucketInfo
	// Configs holds the bucket's configuration documents by key. Commit
	// passes it to CommitOptions.BeforePublish.
	Configs map[string][]byte

	stamp   func() (uint64, time.Time)
	objects *skipList[*Object[P]]
	uploads map[string]*Upload[P]
	// uploadOrder indexes uploads by uploadOrderKey (listing order).
	uploadOrder *skipList[*Upload[P]]
}

// NewBucket returns an empty bucket index. stamp issues the stamp and
// creation time of new versions (normally Clock.Next of the current time);
// it is only called by the operations that create versions, not by Apply
// or Put.
func NewBucket[P any](info storage.BucketInfo, stamp func() (uint64, time.Time)) *Bucket[P] {
	return &Bucket[P]{
		Info:        info,
		Configs:     make(map[string][]byte),
		stamp:       stamp,
		objects:     newSkipList[*Object[P]](),
		uploads:     make(map[string]*Upload[P]),
		uploadOrder: newSkipList[*Upload[P]](),
	}
}

// Len returns the number of keys with at least one version (or delete
// marker).
func (b *Bucket[P]) Len() int { return b.objects.Len() }

// Object returns the version history of key.
func (b *Bucket[P]) Object(key string) (*Object[P], bool) { return b.objects.Get(key) }

// Objects iterates over the keys and their version histories in ascending
// key order. The bucket must not be modified during the iteration.
func (b *Bucket[P]) Objects() iter.Seq2[string, *Object[P]] {
	return func(yield func(string, *Object[P]) bool) {
		for n := b.objects.First(); n != nil; n = n.Next() {
			if !yield(n.key, n.val) {
				return
			}
		}
	}
}

// Find locates a version of key; versionID "" selects the latest, which
// may be a delete marker. latest reports whether it is the newest version.
// A missing key yields storage.ErrObjectNotFound when versionID is "" and
// storage.ErrVersionNotFound otherwise.
func (b *Bucket[P]) Find(key, versionID string) (v *Version[P], latest bool, err error) {
	obj, ok := b.objects.Get(key)
	if versionID == "" {
		if !ok {
			return nil, false, fmt.Errorf("%w: %s/%s", storage.ErrObjectNotFound, b.Info.Name, key)
		}
		return obj.Latest(), true, nil
	}
	if ok {
		if i := obj.indexByID(versionID); i >= 0 {
			return obj.versions[i], i == len(obj.versions)-1, nil
		}
	}
	return nil, false, fmt.Errorf("%w: %s/%s version %q", storage.ErrVersionNotFound, b.Info.Name, key, versionID)
}

// --- changes ---

// Change is the exact effect of one operation on the versions of Key:
// first the versions Removed (identified by their stamps), then Added
// inserted at the position of its stamp, or the metadata of the version
// with Updated's stamp replaced by Updated.Meta. Added and Removed are
// versions of the index and must not be modified; Updated is a copy.
type Change[P any] struct {
	Key     string
	Removed []*Version[P]
	Added   *Version[P]
	Updated *Version[P]
}

// Empty reports whether the change has no effect.
func (c Change[P]) Empty() bool {
	return len(c.Removed) == 0 && c.Added == nil && c.Updated == nil
}

// Apply applies c: it removes the versions with the stamps of c.Removed,
// inserts c.Added (kept, not copied) and replaces the metadata of the
// version with the stamp of c.Updated by a copy of c.Updated.Meta. Stamps
// that are not present are ignored. Apply is how every operation changes
// the bucket, so applying the changes returned by a sequence of operations
// to a copy of the bucket's earlier state yields its current state.
func (b *Bucket[P]) Apply(c Change[P]) {
	obj, ok := b.objects.Get(c.Key)
	if !ok {
		if c.Added == nil {
			return
		}
		obj = &Object[P]{}
		b.objects.Set(c.Key, obj)
	}
	for _, r := range c.Removed {
		if i := obj.indexByStamp(r.Stamp); i >= 0 {
			obj.removeAt(i)
		}
	}
	if c.Added != nil {
		obj.put(c.Added)
	}
	if c.Updated != nil {
		if i := obj.indexByStamp(c.Updated.Stamp); i >= 0 {
			// Copy on write: versions handed out earlier stay unchanged.
			nv := *obj.versions[i]
			nv.Meta = CloneMeta(c.Updated.Meta)
			obj.versions[i] = &nv
		}
	}
	if len(obj.versions) == 0 {
		b.objects.Delete(c.Key)
	}
}

// Put inserts v (kept, not copied) as a version of key at the position of
// its stamp, replacing and returning a version with the same stamp. It is
// meant for loading a snapshot of the index; it does not apply versioning
// rules.
func (b *Bucket[P]) Put(key string, v *Version[P]) *Version[P] {
	obj, ok := b.objects.Get(key)
	if !ok {
		obj = &Object[P]{}
		b.objects.Set(key, obj)
	}
	return obj.put(v)
}

// --- operations ---

// etagMatches compares an If-Match value with a stored (unquoted) ETag. "*"
// matches any current object; surrounding quotes are tolerated.
func etagMatches(cond, etag string) bool {
	cond = strings.TrimSpace(cond)
	if cond == "*" {
		return true
	}
	if len(cond) >= 2 && cond[0] == '"' && cond[len(cond)-1] == '"' {
		cond = cond[1 : len(cond)-1]
	}
	return cond == etag
}

// CheckPreconditions evaluates the IfNoneMatch and IfMatch conditions of
// opts against the current latest version of key, failing with
// storage.ErrPreconditionFailed. Commit checks them itself; callers use
// CheckPreconditions to fail early, e.g. before assembling the parts of a
// multipart upload.
func (b *Bucket[P]) CheckPreconditions(key string, opts storage.CommitOptions) error {
	obj, _ := b.objects.Get(key)
	return checkPreconditions(obj, opts)
}

func checkPreconditions[P any](obj *Object[P], opts storage.CommitOptions) error {
	var cur *Version[P]
	if obj != nil {
		if l := obj.Latest(); !l.DeleteMarker {
			cur = l
		}
	}
	if opts.IfNoneMatch && cur != nil {
		return fmt.Errorf("%w: object exists (If-None-Match)", storage.ErrPreconditionFailed)
	}
	if opts.IfMatch != "" && (cur == nil || !etagMatches(opts.IfMatch, cur.Meta.ETag)) {
		return fmt.Errorf("%w: ETag mismatch (If-Match)", storage.ErrPreconditionFailed)
	}
	return nil
}

// Commit publishes c as the new latest version of key with metadata meta,
// per storage.ObjectWriter.Commit: it checks the preconditions of opts,
// takes a stamp, runs opts.BeforePublish (meta.Encryption is restored
// afterwards: the encoding is the backend's choice, never the hook's) and
// adds the version with an ID per the bucket's versioning status,
// replacing an existing null version when the new one is null. c.Data is
// kept by the index.
func (b *Bucket[P]) Commit(key string, c Content[P], meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, Change[P], error) {
	obj, _ := b.objects.Get(key)
	if err := checkPreconditions(obj, opts); err != nil {
		return storage.ObjectInfo{}, Change[P]{}, err
	}
	stamp, created := b.stamp()
	if opts.BeforePublish != nil {
		encryption := meta.Encryption
		var current *storage.ObjectMeta
		if obj != nil && len(obj.versions) > 0 {
			if latest := obj.Latest(); !latest.DeleteMarker {
				m := CloneMeta(latest.Meta)
				current = &m
			}
		}
		if err := opts.BeforePublish(&meta, current, created, func(key string) []byte {
			return b.Configs[key]
		}); err != nil {
			return storage.ObjectInfo{}, Change[P]{}, err
		}
		meta.Encryption = encryption
	}
	v := &Version[P]{
		Stamp:        stamp,
		Meta:         CloneMeta(meta),
		LastModified: created,
		Size:         c.Size,
		Compression:  c.Compression,
		StoredSize:   c.StoredSize,
		Data:         c.Data,
	}
	ch := b.add(key, obj, v)
	return v.Info(b.Info.Name, key, true), ch, nil
}

// add assigns v (stamped) its version ID per the bucket's versioning
// status and makes it the latest version of key, replacing an existing
// null version when the new one is null. obj is the key's history or nil.
func (b *Bucket[P]) add(key string, obj *Object[P], v *Version[P]) Change[P] {
	if b.Info.Versioning == storage.VersioningEnabled {
		v.ID = NewVersionID(v.Stamp)
	} else {
		v.ID = storage.NullVersionID
	}
	ch := Change[P]{Key: key, Added: v}
	if obj != nil && v.ID == storage.NullVersionID {
		if i := obj.indexByID(storage.NullVersionID); i >= 0 {
			ch.Removed = []*Version[P]{obj.versions[i]}
		}
	}
	b.Apply(ch)
	return ch
}

// Delete deletes a version or creates a delete marker per S3 semantics
// (see storage.Backend.DeleteObject). In an Unversioned bucket the result
// is always storage.DeleteResult{}.
func (b *Bucket[P]) Delete(key, versionID string) (storage.DeleteResult, Change[P], error) {
	if versionID != "" {
		v, _, err := b.Find(key, versionID)
		if err != nil {
			return storage.DeleteResult{}, Change[P]{}, err
		}
		ch := Change[P]{Key: key, Removed: []*Version[P]{v}}
		b.Apply(ch)
		return storage.DeleteResult{VersionID: v.ID, DeleteMarker: v.DeleteMarker}, ch, nil
	}

	obj, _ := b.objects.Get(key)
	switch b.Info.Versioning {
	case storage.VersioningUnversioned:
		ch := Change[P]{Key: key}
		if obj != nil {
			if i := obj.indexByID(storage.NullVersionID); i >= 0 {
				ch.Removed = []*Version[P]{obj.versions[i]}
				b.Apply(ch)
			}
		}
		return storage.DeleteResult{}, ch, nil
	default: // Enabled or Suspended: add a delete marker.
		if err := ValidKey(key); err != nil {
			return storage.DeleteResult{}, Change[P]{}, err
		}
		dm := &Version[P]{DeleteMarker: true}
		dm.Stamp, dm.LastModified = b.stamp()
		ch := b.add(key, obj, dm)
		return storage.DeleteResult{VersionID: dm.ID, DeleteMarker: true}, ch, nil
	}
}

// DeleteIf is Delete made conditional on check, which is called with the
// addressed version (and true); a refusal is returned as is. With absentOK
// and versionID "", check is also called when the key has no versions,
// with a zero ObjectInfo and false (see storage.CheckedDeleter).
func (b *Bucket[P]) DeleteIf(key, versionID string, check func(storage.ObjectInfo, bool) error, absentOK bool) (storage.DeleteResult, Change[P], error) {
	v, latest, err := b.Find(key, versionID)
	switch {
	case err == nil:
		err = check(v.Info(b.Info.Name, key, latest), true)
	case absentOK && versionID == "" && errors.Is(err, storage.ErrObjectNotFound):
		err = check(storage.ObjectInfo{}, false)
	}
	if err != nil {
		return storage.DeleteResult{}, Change[P]{}, err
	}
	return b.Delete(key, versionID)
}

// DeleteVersionIf permanently deletes version versionID (not "") of key
// when check accepts it and the at most limit versions newer than it,
// nearest first (see storage.NewerVersionsDeleter); a refusal is returned
// as is.
func (b *Bucket[P]) DeleteVersionIf(key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, Change[P], error) {
	if versionID == "" {
		return storage.DeleteResult{}, Change[P]{}, fmt.Errorf("%w: %s/%s: no version ID", storage.ErrVersionNotFound, b.Info.Name, key)
	}
	v, latest, err := b.Find(key, versionID)
	if err != nil {
		return storage.DeleteResult{}, Change[P]{}, err
	}
	obj, _ := b.objects.Get(key)
	i := obj.indexByStamp(v.Stamp)
	limit = max(limit, 1)
	newer := make([]storage.ObjectInfo, 0, min(limit, len(obj.versions)-1-i))
	for j := i + 1; j < len(obj.versions) && len(newer) < limit; j++ {
		newer = append(newer, obj.versions[j].Info(b.Info.Name, key, j == len(obj.versions)-1))
	}
	if err := check(v.Info(b.Info.Name, key, latest), newer); err != nil {
		return storage.DeleteResult{}, Change[P]{}, err
	}
	ch := Change[P]{Key: key, Removed: []*Version[P]{v}}
	b.Apply(ch)
	return storage.DeleteResult{VersionID: v.ID, DeleteMarker: v.DeleteMarker}, ch, nil
}

// UpdateMeta mutates the metadata of an existing version (versionID "" =
// latest) per storage.Backend.UpdateObjectMeta: fn receives a private copy
// and nothing changes if it fails; ObjectMeta.Encryption cannot be changed.
// A non-nil check is called first with the addressed version (see
// storage.ConditionalMetaUpdater). Addressing a delete marker yields
// storage.ErrObjectNotFound.
func (b *Bucket[P]) UpdateMeta(key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, Change[P], error) {
	v, latest, err := b.Find(key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, Change[P]{}, err
	}
	if v.DeleteMarker {
		return storage.ObjectInfo{}, Change[P]{}, fmt.Errorf("%w: %s/%s version %q is a delete marker", storage.ErrObjectNotFound, b.Info.Name, key, v.ID)
	}
	if check != nil {
		if err := check(v.Info(b.Info.Name, key, latest)); err != nil {
			return storage.ObjectInfo{}, Change[P]{}, err
		}
	}
	m := CloneMeta(v.Meta)
	if err := fn(&m); err != nil {
		return storage.ObjectInfo{}, Change[P]{}, err
	}
	m.Encryption = v.Meta.Encryption
	nv := *v
	nv.Meta = m
	ch := Change[P]{Key: key, Updated: &nv}
	b.Apply(ch)
	return nv.Info(b.Info.Name, key, latest), ch, nil
}

// CheckImport validates the identity of a version to import (see
// storage.VersionWriter.Commit): a valid imported version ID, and no
// content (size) for a delete marker. Import checks it itself; callers use
// it to fail before taking locks.
func CheckImport(bucket, key string, info storage.ObjectInfo, size int64) error {
	if !storage.ValidImportedVersionID(info.VersionID) {
		return fmt.Errorf("%w: version ID %.80q", storage.ErrInvalidName, info.VersionID)
	}
	if info.DeleteMarker && size > 0 {
		return fmt.Errorf("%w: delete marker %s/%q with data", storage.ErrInvalidName, bucket, key)
	}
	return nil
}

// Import stores c as the newest version of key with the identity given by
// info (see storage.VersionWriter.Commit): the version gets a fresh stamp,
// so it sorts newest, but keeps info's version ID, delete-marker flag,
// LastModified and (except for delete markers) metadata, including
// Encryption. A version with the same ID is replaced wherever it is in the
// history.
func (b *Bucket[P]) Import(key string, info storage.ObjectInfo, c Content[P]) (storage.ObjectInfo, Change[P], error) {
	if err := CheckImport(b.Info.Name, key, info, c.Size); err != nil {
		return storage.ObjectInfo{}, Change[P]{}, err
	}
	v := &Version[P]{
		ID:           info.VersionID,
		DeleteMarker: info.DeleteMarker,
		Imported:     info.VersionID != storage.NullVersionID,
		LastModified: info.LastModified.UTC(),
		Size:         c.Size,
		Compression:  c.Compression,
		StoredSize:   c.StoredSize,
		Data:         c.Data,
	}
	if !info.DeleteMarker {
		v.Meta = CloneMeta(info.ObjectMeta)
	}
	v.Stamp, _ = b.stamp()
	ch := Change[P]{Key: key, Added: v}
	if obj, ok := b.objects.Get(key); ok {
		if i := obj.indexByID(v.ID); i >= 0 {
			ch.Removed = []*Version[P]{obj.versions[i]}
		}
	}
	b.Apply(ch)
	return v.Info(b.Info.Name, key, true), ch, nil
}
