package gateway

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// errWriterDone is returned when using a writer after Commit or Abort.
var errWriterDone = errors.New("gateway: writer already committed or aborted")

// maxMetaRewrites bounds the read-modify-write retries of UpdateObjectMeta
// when the object changes concurrently.
const maxMetaRewrites = 5

// singlePutLimit returns the largest object s3client.Client.Upload sends
// with one PUT for an object of size bytes (mirrors its part sizing).
func (s *Store) singlePutLimit(size int64) int64 {
	ps := s.partSize
	if ps <= 0 {
		ps = s3client.DefaultPartSize
	}
	ps = max(ps, s3client.MinPartSize)
	if need := (size + s3client.MaxParts - 1) / s3client.MaxParts; need > ps {
		ps = (need + 1<<20 - 1) &^ (1<<20 - 1)
	}
	return ps
}

// etagMatches compares an If-Match value with an (unquoted) ETag; "*"
// matches any existing object.
func etagMatches(cond, etag string) bool {
	cond = strings.TrimSpace(cond)
	if cond == "*" {
		return true
	}
	return strings.Trim(cond, `"`) == etag
}

// head returns the gos3 view of bucket/key and its remote attributes.
// A missing object yields notFound(versionID).
func (s *Store) head(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, s3client.ObjectAttrs, error) {
	if s.checkKey(bucket, key) != nil {
		return storage.ObjectInfo{}, s3client.ObjectAttrs{}, notFound(bucket, key, versionID)
	}
	rb, rk := s.remoteKey(bucket, key)
	a, err := s.c.HeadObject(ctx, rb, rk)
	if err != nil {
		// A HEAD answer has no body, so a vanished remote bucket looks like
		// a missing object here.
		if s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchBucket") {
			return storage.ObjectInfo{}, a, notFound(bucket, key, versionID)
		}
		return storage.ObjectInfo{}, a, s.remoteErr("HeadObject", bucket, key, err)
	}
	return s.objectInfo(bucket, key, a), a, nil
}

// lastModifiedAfterWrite returns the LastModified of the object just
// written with remote ETag etag ("" = any), from a HEAD; the local clock
// when the object changed again in between or the HEAD fails.
func (s *Store) lastModifiedAfterWrite(ctx context.Context, bucket, key, etag string) time.Time {
	info, a, err := s.head(ctx, bucket, key, "")
	if err == nil && (etag == "" || a.ETag == etag) {
		return info.LastModified
	}
	return truncTime(time.Now())
}

// --- writer ---

type objectWriter struct {
	s           *Store
	bucket, key string
	gen         uint64

	mu    sync.Mutex
	spool *s3client.Spool
	md5   hash.Hash
	done  bool
}

// NewObjectWriter starts writing bucket/key; the data is buffered in a
// spool and uploaded at Commit.
func (s *Store) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	if err := s.checkKey(bucket, key); err != nil {
		return nil, err
	}
	return &objectWriter{
		s: s, bucket: bucket, key: key, gen: s.gen(bucket),
		spool: s3client.NewSpool(s.spoolDir, 0), md5: md5.New(),
	}, nil
}

func (w *objectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	n, err := w.spool.Write(p)
	w.md5.Write(p[:n])
	return n, err
}

// Commit uploads the buffered data: one PUT up to the part size, a remote
// multipart upload above it. Preconditions: IfNoneMatch is sent as
// "If-None-Match: *"; IfMatch is checked against the gos3 ETag (which may
// differ from the remote one) with a HEAD under the object's lock, and the
// upload is pinned to the remote ETag seen there with "If-Match", so it
// is atomic against other gos3 writers and, where the remote supports
// conditional writes, against everybody.
func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return storage.ObjectInfo{}, errWriterDone
	}
	w.done = true
	sum := hex.EncodeToString(w.md5.Sum(nil))
	w.mu.Unlock()
	defer w.spool.Close()
	if err := w.spool.Finish(); err != nil {
		return storage.ObjectInfo{}, err
	}
	s := w.s
	if s.gen(w.bucket) != w.gen {
		return storage.ObjectInfo{}, fmt.Errorf("%w: %q (deleted while writing)", storage.ErrBucketNotFound, w.bucket)
	}
	size := w.spool.Size()
	multipart := size > s.singlePutLimit(size)
	// The remote ETag is the MD5 of the content for single PUTs, so the gos3
	// ETag only needs to be kept when it differs.
	h := encodeHeaders(meta, headerOpts{etag: multipart || meta.ETag != sum})

	unlock := s.lockObject(w.bucket, w.key)
	defer unlock()
	up := s3client.UploadOptions{PutOptions: s3client.PutOptions{Header: h, IfNoneMatch: opts.IfNoneMatch}, PartSize: s.partSize}
	if opts.IfMatch != "" {
		cur, a, err := s.head(ctx, w.bucket, w.key, "")
		if errors.Is(err, storage.ErrObjectNotFound) {
			return storage.ObjectInfo{}, fmt.Errorf("%w: %s/%s does not exist (If-Match)", storage.ErrPreconditionFailed, w.bucket, w.key)
		}
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if !etagMatches(opts.IfMatch, cur.ETag) {
			return storage.ObjectInfo{}, fmt.Errorf("%w: ETag mismatch (If-Match)", storage.ErrPreconditionFailed)
		}
		up.IfMatch = a.ETag
	}
	rb, rk := s.remoteKey(w.bucket, w.key)
	etag, _, err := s.c.Upload(ctx, rb, rk, w.spool, size, up)
	if err != nil {
		return storage.ObjectInfo{}, s.remoteErr("PutObject", w.bucket, w.key, err)
	}
	return storage.ObjectInfo{
		ObjectMeta:   cloneMeta(meta),
		Bucket:       w.bucket,
		Key:          w.key,
		VersionID:    storage.NullVersionID,
		IsLatest:     true,
		Size:         size,
		LastModified: s.lastModifiedAfterWrite(ctx, w.bucket, w.key, etag),
	}, nil
}

func (w *objectWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		w.done = true
		return w.spool.Close()
	}
	return nil
}

// --- reads ---

// GetObjectInfo returns the object's metadata (HEAD).
func (s *Store) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	if err := checkVersion(bucket, key, versionID); err != nil {
		return storage.ObjectInfo{}, err
	}
	info, _, err := s.head(ctx, bucket, key, versionID)
	return info, err
}

// OpenObject GETs the whole object; info and the first bytes come from the
// same response, so they always describe one version. The reader keeps
// reading that response; after a Seek (or a broken connection) it issues
// ranged GETs pinned to the remote ETag, which fail with an error wrapping
// storage.ErrPreconditionFailed if the object was replaced meanwhile:
// without remote versioning, the old content is gone.
func (s *Store) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if err := checkVersion(bucket, key, versionID); err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	if s.checkKey(bucket, key) != nil {
		return storage.ObjectInfo{}, nil, notFound(bucket, key, versionID)
	}
	rb, rk := s.remoteKey(bucket, key)
	body, a, err := s.c.GetObject(ctx, rb, rk, s3client.GetOptions{})
	if err != nil {
		if s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchBucket") {
			return storage.ObjectInfo{}, nil, notFound(bucket, key, versionID)
		}
		return storage.ObjectInfo{}, nil, s.remoteErr("GetObject", bucket, key, err)
	}
	if a.Size < 0 {
		// No Content-Length: learn the size from a HEAD pinned by ETag.
		_ = body.Close()
		body = nil
		ha, err := s.c.HeadObject(ctx, rb, rk)
		if err != nil || ha.ETag != a.ETag {
			return storage.ObjectInfo{}, nil, s.remoteErr("HeadObject", bucket, key, errors.Join(err, errors.New("object changed while being opened")))
		}
		a = ha
	}
	info := s.objectInfo(bucket, key, a)
	r := &reader{s: s, ctx: ctx, bucket: bucket, key: key, rbucket: rb, rkey: rk, size: a.Size, etag: a.ETag, body: body}
	return info, r, nil
}

// drainLimit is the largest unread response remainder a reader drains
// (instead of dropping the connection) when it abandons a response.
const drainLimit = 64 << 10

// reader reads one remote object version (see OpenObject).
type reader struct {
	s             *Store
	ctx           context.Context
	bucket, key   string
	rbucket, rkey string
	size          int64
	etag          string // remote ETag every request is pinned to

	mu     sync.Mutex
	off    int64
	body   io.ReadCloser // response positioned at off, or nil
	closed bool
}

var errReaderClosed = errors.New("gateway: read from a closed object reader")

func (r *reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errReaderClosed
	}
	if r.off >= r.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > r.size-r.off {
		p = p[:r.size-r.off]
	}
	for attempt := 0; ; attempt++ {
		if r.body == nil {
			body, _, err := r.s.c.GetObject(r.ctx, r.rbucket, r.rkey, s3client.GetOptions{Offset: r.off, IfMatch: r.etag})
			if err != nil {
				if s3client.IsPreconditionFailed(err) || (s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchBucket")) {
					return 0, fmt.Errorf("gateway %q: %s/%s was replaced or deleted on the remote while being read: %w",
						r.s.name, r.bucket, r.key, storage.ErrPreconditionFailed)
				}
				return 0, r.s.remoteErr("GetObject", r.bucket, r.key, err)
			}
			r.body = body
		}
		n, err := r.body.Read(p)
		r.off += int64(n)
		switch {
		case err == nil:
			return n, nil
		case n > 0:
			// Return the data; at a premature end or a broken response the
			// next Read issues a new request at the current offset.
			_ = r.closeBody(false)
			return n, nil
		}
		_ = r.closeBody(false)
		if errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("gateway %q: %s/%s ended at %d of %d bytes: %w", r.s.name, r.bucket, r.key, r.off, r.size, io.ErrUnexpectedEOF)
		}
		if attempt > 0 || r.ctx.Err() != nil {
			return 0, r.s.remoteErr("GetObject", r.bucket, r.key, err)
		}
	}
}

// closeBody abandons the current response. With drain, a small unread
// remainder is read so the connection can be reused.
func (r *reader) closeBody(drain bool) error {
	if r.body == nil {
		return nil
	}
	if drain && r.size-r.off <= drainLimit {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.body, drainLimit))
	}
	err := r.body.Close()
	r.body = nil
	return err
}

// Seek implements io.Seeker. Moving away from the current offset drops the
// open response; the next Read issues a ranged GET pinned to the ETag.
func (r *reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errReaderClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, errors.New("gateway: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("gateway: negative position")
	}
	if abs != r.off {
		_ = r.closeBody(true)
		r.off = abs
	}
	return abs, nil
}

// Close releases the open response. It is idempotent.
func (r *reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	_ = r.closeBody(true)
	return nil
}

// --- metadata updates ---

// UpdateObjectMeta runs fn on a copy of the object's metadata and rewrites
// the remote object onto itself (server-side copy with all headers
// replaced, pinned to the remote ETag read), retrying when the object
// changes concurrently. The gos3 ETag and the original LastModified are
// kept in gos3-meta, so neither changes for gos3 clients.
func (s *Store) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if fn == nil {
		return storage.ObjectInfo{}, errors.New("gateway: nil update function")
	}
	if err := checkVersion(bucket, key, versionID); err != nil {
		return storage.ObjectInfo{}, err
	}
	unlock := s.lockObject(bucket, key)
	defer unlock()
	rb, rk := s.remoteKey(bucket, key)
	for attempt := 1; ; attempt++ {
		info, a, err := s.head(ctx, bucket, key, versionID)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		m := cloneMeta(info.ObjectMeta)
		if err := fn(&m); err != nil {
			return storage.ObjectInfo{}, err
		}
		m = cloneMeta(m)
		h := encodeHeaders(m, headerOpts{etag: true, lastModified: info.LastModified})
		_, err = s.c.Copy(ctx, rb, rk, rk, a.Size, s3client.CopyOptions{ReplaceMetadata: true, Header: h, IfMatch: a.ETag})
		if err == nil {
			info.ObjectMeta = m
			return info, nil
		}
		if !s3client.IsPreconditionFailed(err) || attempt == maxMetaRewrites {
			return storage.ObjectInfo{}, s.remoteErr("CopyObject (metadata update)", bucket, key, err)
		}
	}
}

// --- deletes ---

// DeleteObject removes the object. versionID "" never fails for a
// missing key (DeleteResult{}, as for an unversioned bucket); "null"
// reports ErrVersionNotFound for a missing key.
func (s *Store) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	if err := checkVersion(bucket, key, versionID); err != nil {
		return storage.DeleteResult{}, err
	}
	if versionID == "" {
		if s.checkKey(bucket, key) != nil {
			return storage.DeleteResult{}, nil
		}
		unlock := s.lockObject(bucket, key)
		defer unlock()
		return storage.DeleteResult{}, s.deleteRemote(ctx, bucket, key)
	}
	return s.DeleteObjectIf(ctx, bucket, key, versionID, func(storage.ObjectInfo) error { return nil })
}

// DeleteObjectIf implements the storage.ConditionalDeleter semantics: the
// object is read (HEAD) and checked, then deleted, under the object's
// lock, so it is atomic against other users of this Store. (s3client has
// no conditional DELETE, so a client writing the object directly on the
// remote between the check and the delete is not detected.)
func (s *Store) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("gateway: nil delete check")
	}
	if err := checkVersion(bucket, key, versionID); err != nil {
		return storage.DeleteResult{}, err
	}
	unlock := s.lockObject(bucket, key)
	defer unlock()
	info, _, err := s.head(ctx, bucket, key, versionID)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if err := check(info); err != nil {
		return storage.DeleteResult{}, err
	}
	if err := s.deleteRemote(ctx, bucket, key); err != nil {
		return storage.DeleteResult{}, err
	}
	if versionID == "" {
		return storage.DeleteResult{}, nil
	}
	return storage.DeleteResult{VersionID: storage.NullVersionID}, nil
}

func (s *Store) deleteRemote(ctx context.Context, bucket, key string) error {
	rb, rk := s.remoteKey(bucket, key)
	return s.remoteErr("DeleteObject", bucket, key, s.c.DeleteObject(ctx, rb, rk))
}

// --- walks ---

// maxCodePoint is the largest Unicode code point; no valid UTF-8 string
// that starts with base+maxCodePoint+... sorts below base+maxCodePoint
// except base+maxCodePoint itself.
const maxCodePoint = "\U0010FFFF"

// validPrefixLen returns the length of the longest valid UTF-8 prefix of s.
func validPrefixLen(s string) int {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n <= 1 {
			return i
		}
		i += n
	}
	return len(s)
}

// startAfter returns a valid UTF-8 lower bound for keys > after: every
// key > after is > the bound. Remotes reject invalid UTF-8 in start-after,
// but internal/storage/list.go restarts walks after p+"\xff" to skip the
// keys under a common prefix p. When after has an invalid byte at
// position i that can never start a UTF-8 sequence (0xF5..0xFF), every
// valid key with prefix after[:i] that sorts before after has a code point
// <= U+10FFFF at i, so after[:i]+U+10FFFF is a tight bound; for other
// invalid bytes the bound is after[:i]. Keys between the bound and after
// are skipped by the caller.
func startAfter(after string) string {
	i := validPrefixLen(after)
	if i == len(after) {
		return after
	}
	if after[i] >= 0xF5 {
		return after[:i] + maxCodePoint
	}
	return after[:i]
}

// walk calls fn with every remote object of bucket whose gos3 key has the
// given prefix and sorts after after, in ascending order.
func (s *Store) walk(ctx context.Context, bucket, prefix, after string, fn func(key string, a s3client.ObjectAttrs) error) error {
	rb, rp := s.remote(bucket)
	in := s3client.ListInput{Prefix: rp + prefix[:validPrefixLen(prefix)]}
	if sa := startAfter(after); sa != "" {
		in.StartAfter = rp + sa
	}
	for {
		page, err := s.c.ListObjectsV2(ctx, rb, in)
		if err != nil {
			return s.remoteErr("ListObjectsV2", bucket, "", err)
		}
		for _, o := range page.Objects {
			key, ok := strings.CutPrefix(o.Key, rp)
			if !ok || key == "" || !utf8.ValidString(key) || !strings.HasPrefix(key, prefix) || key <= after {
				continue
			}
			if err := fn(key, o); err != nil {
				if errors.Is(err, storage.ErrStopWalk) {
					return nil
				}
				return err
			}
		}
		if !page.IsTruncated {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		in.ContinuationToken = page.NextContinuationToken
	}
}

// WalkObjects lists the bucket's objects (ListObjectsV2); entries carry the
// listing's metadata only (see listedInfo).
func (s *Store) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("gateway: nil walk function")
	}
	return s.walk(ctx, bucket, prefix, after, func(key string, a s3client.ObjectAttrs) error {
		return fn(listedInfo(bucket, key, a))
	})
}

// WalkVersions lists every object as its only (null, latest) version.
// Every versionIDMarker positions after keyMarker's null version, i.e.
// after keyMarker: there is nothing else to resume within a key.
func (s *Store) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	return s.WalkObjects(ctx, bucket, prefix, keyMarker, fn)
}
