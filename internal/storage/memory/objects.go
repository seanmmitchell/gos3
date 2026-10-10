package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// errWriterDone is returned when writing to or committing a writer that was
// already committed or aborted.
var errWriterDone = errors.New("memory: writer already committed or aborted")

// errReaderClosed is returned by reads on a closed object reader.
var errReaderClosed = errors.New("memory: read on closed object reader")

// validKey reports whether the backend can store key. S3 keys are non-empty
// UTF-8; length limits are enforced by the S3 layer.
func validKey(key string) error {
	if key == "" || !utf8.ValidString(key) {
		return fmt.Errorf("%w: object key %q", storage.ErrInvalidName, key)
	}
	return nil
}

// --- writer ---

type objectWriter struct {
	b    *Backend
	bk   *bucket
	key  string
	mu   sync.Mutex
	buf  []byte
	done bool
}

// NewObjectWriter starts writing a new version of bucket/key.
func (b *Backend) NewObjectWriter(ctx context.Context, bucketName, key string) (storage.ObjectWriter, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return nil, err
	}
	bk.mu.RUnlock()
	return &objectWriter{b: b, bk: bk, key: key}, nil
}

func (w *objectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return storage.ObjectInfo{}, errWriterDone
	}
	w.done = true
	data := slices.Clip(w.buf)
	w.buf = nil
	w.mu.Unlock()

	if err := w.b.relock(ctx, w.bk); err != nil {
		return storage.ObjectInfo{}, err
	}
	defer w.bk.mu.Unlock()
	return w.b.commitLocked(w.bk, w.key, data, meta, opts)
}

func (w *objectWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	w.buf = nil
	return nil
}

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

// checkPreconditions evaluates opts against the current latest version of
// obj (nil when the key does not exist).
func checkPreconditions(obj *object, opts storage.CommitOptions) error {
	var cur *version
	if obj != nil {
		if l := obj.latest(); !l.deleteMarker {
			cur = l
		}
	}
	if opts.IfNoneMatch && cur != nil {
		return fmt.Errorf("%w: object exists (If-None-Match)", storage.ErrPreconditionFailed)
	}
	if opts.IfMatch != "" && (cur == nil || !etagMatches(opts.IfMatch, cur.meta.ETag)) {
		return fmt.Errorf("%w: ETag mismatch (If-Match)", storage.ErrPreconditionFailed)
	}
	return nil
}

// commitLocked publishes data as a new version of key in bk, applying
// conditional checks and versioning semantics. bk must be write-locked; data
// is taken over by the backend and must not be modified afterwards.
func (b *Backend) commitLocked(bk *bucket, key string, data []byte, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	obj, _ := bk.objects.Get(key)
	if err := checkPreconditions(obj, opts); err != nil {
		return storage.ObjectInfo{}, err
	}
	if data == nil {
		data = []byte{}
	}
	v := &version{meta: cloneMeta(meta), data: data}
	b.addVersionLocked(bk, key, obj, v)
	return v.info(bk.info.Name, key, true), nil
}

// addVersionLocked stamps v, assigns its version ID per the bucket's
// versioning status (replacing an existing null version when the new one is
// null) and makes it the latest version of key. obj may be nil.
func (b *Backend) addVersionLocked(bk *bucket, key string, obj *object, v *version) {
	v.stamp, v.lastModified = b.nextStamp()
	if bk.info.Versioning == storage.VersioningEnabled {
		v.id = newVersionID(v.stamp)
	} else {
		v.id = storage.NullVersionID
	}
	if obj == nil {
		obj = &object{}
		bk.objects.Set(key, obj)
	} else if v.id == storage.NullVersionID {
		if i := obj.indexByID(storage.NullVersionID); i >= 0 {
			obj.removeAt(i)
		}
	}
	obj.versions = append(obj.versions, v)
}

// findVersion locates a version of key. versionID "" selects the latest.
func findVersion(bk *bucket, key, versionID string) (*object, int, error) {
	obj, ok := bk.objects.Get(key)
	if versionID == "" {
		if !ok {
			return nil, -1, fmt.Errorf("%w: %s/%s", storage.ErrObjectNotFound, bk.info.Name, key)
		}
		return obj, len(obj.versions) - 1, nil
	}
	if ok {
		if i := obj.indexByID(versionID); i >= 0 {
			return obj, i, nil
		}
	}
	return nil, -1, fmt.Errorf("%w: %s/%s version %q", storage.ErrVersionNotFound, bk.info.Name, key, versionID)
}

// GetObjectInfo returns metadata for a version ("" = latest, which may be a
// delete marker). A missing key yields ErrObjectNotFound when versionID is
// "" and ErrVersionNotFound otherwise.
func (b *Backend) GetObjectInfo(ctx context.Context, bucketName, key, versionID string) (storage.ObjectInfo, error) {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	defer bk.mu.RUnlock()
	obj, i, err := findVersion(bk, key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return obj.versions[i].info(bk.info.Name, key, i == len(obj.versions)-1), nil
}

// objectReader is an immutable snapshot reader over one version's data.
type objectReader struct {
	mu     sync.Mutex
	r      bytes.Reader
	closed bool
}

func newObjectReader(data []byte) *objectReader {
	o := &objectReader{}
	o.r.Reset(data)
	return o
}

func (o *objectReader) Read(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errReaderClosed
	}
	return o.r.Read(p)
}

func (o *objectReader) Seek(offset int64, whence int) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errReaderClosed
	}
	return o.r.Seek(offset, whence)
}

// ReadAt implements io.ReaderAt.
func (o *objectReader) ReadAt(p []byte, off int64) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errReaderClosed
	}
	return o.r.ReadAt(p, off)
}

// WriteTo implements io.WriterTo for efficient io.Copy.
func (o *objectReader) WriteTo(w io.Writer) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errReaderClosed
	}
	return o.r.WriteTo(w)
}

func (o *objectReader) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	return nil
}

// OpenObject returns a version's info and a reader over its content. The
// reader is nil for delete markers.
func (b *Backend) OpenObject(ctx context.Context, bucketName, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	defer bk.mu.RUnlock()
	obj, i, err := findVersion(bk, key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	v := obj.versions[i]
	info := v.info(bk.info.Name, key, i == len(obj.versions)-1)
	if v.deleteMarker {
		return info, nil, nil
	}
	return info, newObjectReader(v.data), nil
}

// UpdateObjectMeta atomically mutates the metadata of an existing version.
// fn receives a private copy; if it returns an error nothing changes. fn is
// called with the bucket locked and must not call back into the backend.
// Addressing a delete marker yields ErrObjectNotFound (wrapped with a
// "delete marker" message).
func (b *Backend) UpdateObjectMeta(ctx context.Context, bucketName, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if fn == nil {
		return storage.ObjectInfo{}, errors.New("memory: nil update function")
	}
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	defer bk.mu.Unlock()
	obj, i, err := findVersion(bk, key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	v := obj.versions[i]
	if v.deleteMarker {
		return storage.ObjectInfo{}, fmt.Errorf("%w: %s/%s version %q is a delete marker", storage.ErrObjectNotFound, bk.info.Name, key, v.id)
	}
	m := cloneMeta(v.meta)
	if err := fn(&m); err != nil {
		return storage.ObjectInfo{}, err
	}
	v.meta = cloneMeta(m)
	return v.info(bk.info.Name, key, i == len(obj.versions)-1), nil
}

// DeleteObject deletes a version or creates a delete marker per S3 semantics
// (see storage.Backend). In an Unversioned bucket the result is always
// DeleteResult{} (S3 reports no version information there).
func (b *Backend) DeleteObject(ctx context.Context, bucketName, key, versionID string) (storage.DeleteResult, error) {
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	defer bk.mu.Unlock()

	if versionID != "" {
		obj, i, err := findVersion(bk, key, versionID)
		if err != nil {
			return storage.DeleteResult{}, err
		}
		v := obj.removeAt(i)
		if len(obj.versions) == 0 {
			bk.objects.Delete(key)
		}
		return storage.DeleteResult{VersionID: v.id, DeleteMarker: v.deleteMarker}, nil
	}

	obj, _ := bk.objects.Get(key)
	switch bk.info.Versioning {
	case storage.VersioningUnversioned:
		if obj != nil {
			if i := obj.indexByID(storage.NullVersionID); i >= 0 {
				obj.removeAt(i)
				if len(obj.versions) == 0 {
					bk.objects.Delete(key)
				}
			}
		}
		return storage.DeleteResult{}, nil
	default: // Enabled or Suspended: add a delete marker.
		if err := validKey(key); err != nil {
			return storage.DeleteResult{}, err
		}
		dm := &version{deleteMarker: true}
		b.addVersionLocked(bk, key, obj, dm)
		return storage.DeleteResult{VersionID: dm.id, DeleteMarker: true}, nil
	}
}

// --- walks ---

const (
	walkMinBatch = 16
	walkMaxBatch = 1024
)

// nextBatch grows walk batches geometrically so that walks stopped early
// (the list helpers stop after one item on every common prefix) copy little,
// while long walks amortise locking.
func nextBatch(n int) int {
	if n*2 > walkMaxBatch {
		return walkMaxBatch
	}
	return n * 2
}

// callWalk invokes fn for each item; it reports stop=true when fn asked to
// stop (err nil) or failed (err non-nil).
func callWalk(items []storage.ObjectInfo, fn func(storage.ObjectInfo) error) (bool, error) {
	for _, it := range items {
		if err := fn(it); err != nil {
			if errors.Is(err, storage.ErrStopWalk) {
				return true, nil
			}
			return true, err
		}
	}
	return false, nil
}

// WalkObjects calls fn with the latest version of each key (skipping keys
// whose latest version is a delete marker) in ascending key order, for keys
// with the given prefix that are strictly greater than after. fn runs with
// no lock held and may call into the backend.
func (b *Backend) WalkObjects(ctx context.Context, bucketName, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("memory: nil walk function")
	}
	cursor := after
	inclusive := false // seek > cursor, or >= prefix when cursor < prefix
	if after < prefix {
		cursor, inclusive = prefix, true
	}
	batch := walkMinBatch
	for {
		bk, err := b.rlockBucket(ctx, bucketName)
		if err != nil {
			return err
		}
		var n *slNode[*object]
		if inclusive {
			n = bk.objects.SeekGE(cursor)
		} else {
			n = bk.objects.SeekGT(cursor)
		}
		items := make([]storage.ObjectInfo, 0, batch)
		scanned, done := 0, true
		for ; n != nil; n = n.Next() {
			if !strings.HasPrefix(n.key, prefix) {
				break
			}
			if len(items) == batch || scanned == 4*batch {
				done = false
				break
			}
			scanned++
			cursor, inclusive = n.key, false
			if l := n.val.latest(); !l.deleteMarker {
				items = append(items, l.info(bk.info.Name, n.key, true))
			}
		}
		bk.mu.RUnlock()

		if stop, err := callWalk(items, fn); stop || err != nil {
			return err
		}
		if done {
			return nil
		}
		if err := checkCtx(ctx); err != nil {
			return err
		}
		batch = nextBatch(batch)
	}
}

// WalkVersions calls fn for every version and delete marker of keys with the
// given prefix, keys ascending and newest version first within a key,
// starting after (keyMarker, versionIDMarker) per S3 ListObjectVersions
// semantics. If versionIDMarker is not a version of keyMarker, the walk
// resumes at the position the ID would occupy when it is an ID generated by
// this backend (e.g. a version deleted between pages), and otherwise after
// all versions of keyMarker. fn runs with no lock held.
func (b *Backend) WalkVersions(ctx context.Context, bucketName, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("memory: nil walk function")
	}
	// Position: either strictly after all versions of key (below == 0) or
	// at the versions of key whose stamp is < below. inclusive means "seek
	// to the first key >= key" (initial position at the prefix).
	//
	// midKey is set when a batch ended inside the versions of key: the
	// newest of them was already delivered, so none of the remaining ones
	// is reported as latest even if newer versions were deleted meanwhile
	// (IsLatest marks at most the first entry delivered for a key).
	var (
		key       string
		below     uint64
		inclusive bool
		midKey    bool
	)
	switch {
	case keyMarker == "" || keyMarker < prefix:
		key, inclusive = prefix, true
	case versionIDMarker == "":
		key = keyMarker
	default:
		key = keyMarker
		bk, err := b.rlockBucket(ctx, bucketName)
		if err != nil {
			return err
		}
		if obj, ok := bk.objects.Get(keyMarker); ok {
			if i := obj.indexByID(versionIDMarker); i >= 0 {
				below = obj.versions[i].stamp
			}
		}
		bk.mu.RUnlock()
		if below == 0 {
			if s, ok := stampFromVersionID(versionIDMarker); ok {
				below = s
			}
		}
	}

	batch := walkMinBatch
	for {
		bk, err := b.rlockBucket(ctx, bucketName)
		if err != nil {
			return err
		}
		items := make([]storage.ObjectInfo, 0, batch)
		done := true
		var n *slNode[*object]
		switch {
		case inclusive:
			n = bk.objects.SeekGE(key)
		case below != 0:
			n = bk.objects.SeekGE(key)
			if n != nil && n.key != key {
				// key vanished; continue with the following keys.
				below = 0
			}
		default:
			n = bk.objects.SeekGT(key)
		}
	scan:
		for ; n != nil; n = n.Next() {
			if !strings.HasPrefix(n.key, prefix) {
				break
			}
			if len(items) == batch {
				// Resume at n: the current position (after the previous key,
				// or within the marker key) is still accurate.
				done = false
				break
			}
			vs := n.val.versions
			top := len(vs) - 1
			continued := false
			if below != 0 && n.key == key {
				// Only versions strictly older than the marker.
				top = sort.Search(len(vs), func(i int) bool { return vs[i].stamp >= below }) - 1
				continued = midKey
			}
			for j := top; j >= 0; j-- {
				items = append(items, vs[j].info(bk.info.Name, n.key, !continued && j == len(vs)-1))
				if len(items) == batch && j > 0 {
					key, below, inclusive, midKey = n.key, vs[j].stamp, false, true
					done = false
					break scan
				}
			}
			key, below, inclusive, midKey = n.key, 0, false, false
		}
		bk.mu.RUnlock()

		if stop, err := callWalk(items, fn); stop || err != nil {
			return err
		}
		if done {
			return nil
		}
		if err := checkCtx(ctx); err != nil {
			return err
		}
		batch = nextBatch(batch)
	}
}
