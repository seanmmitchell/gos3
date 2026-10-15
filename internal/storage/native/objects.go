package native

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// errWriterDone is returned when using a writer after Commit or Abort.
var errWriterDone = errors.New("native: writer already committed or aborted")

// --- versions ---

// version is an object version or a delete marker as found on the remote.
type version struct {
	// info is the gos3 view (overlay applied). For delete markers only
	// Bucket, Key, VersionID, DeleteMarker, IsLatest and (when looked up
	// with statFull) LastModified are set.
	info storage.ObjectInfo
	// attrs is the remote answer of an object (zero for delete markers):
	// its remote ETag, LastModified and version ID ("" from an unversioned
	// remote) identify the remote version.
	attrs s3client.ObjectAttrs
	// ext and ov are the gos3-meta header and the overlay applied to info
	// (nil when there were none).
	ext *metaPatch
	ov  *overlay
}

// statMode says what stat must find out besides the version itself.
type statMode int

const (
	// statFull completes delete markers (LastModified and IsLatest from
	// a listing; a latest marker the remote reported as a plain 404 is
	// found there too) and sets IsLatest of versions addressed by ID
	// (an extra HEAD of the latest version, sent in parallel).
	statFull statMode = iota
	// statQuick skips all that: a latest delete marker may be reported as
	// a missing object, IsLatest of versions addressed by ID is false.
	statQuick
)

// stat returns the version of key addressed by vid ("" = the latest,
// which may be a delete marker). A missing key or version yields
// notFound(bucket, key, vid).
func (s *Store) stat(ctx context.Context, t *target, key, vid string, mode statMode) (version, error) {
	if s.checkKey(key) != nil {
		return version{}, notFound(t.bucket, key, vid)
	}
	switch {
	case vid == "":
		return s.statLatest(ctx, t, key, mode)
	case vid == storage.NullVersionID && !t.versioned():
		// Unversioned buckets hold only null versions: address the
		// current object, which works on services without versioning.
		v, err := s.statLatest(ctx, t, key, mode)
		switch {
		case errors.Is(err, errGone):
			return version{}, goneErr(t.bucket, key, vid)
		case errors.Is(err, storage.ErrObjectNotFound):
			return version{}, notFound(t.bucket, key, vid)
		case err != nil:
			return version{}, err
		case v.info.VersionID == storage.NullVersionID:
			return v, nil
		}
		s.drift(t.bucket, "the remote bucket is versioned")
	}
	return s.statVersion(ctx, t, key, vid, mode)
}

// statLatest returns the latest version of key.
func (s *Store) statLatest(ctx context.Context, t *target, key string, mode statMode) (version, error) {
	var a s3client.ObjectAttrs
	err := s.call(ctx, t, func(c *s3client.Client) error {
		var err error
		a, err = c.HeadObjectWith(ctx, t.b.Remote, key, s3client.HeadOptions{})
		return err
	})
	if err == nil {
		v, err := s.describe(ctx, t, key, a)
		v.info.IsLatest = true
		return v, err
	}
	if mvid, ok := s3client.IsDeleteMarker(err); ok {
		return s.marker(ctx, t, key, mvid, true, mode)
	}
	if !missingObject(err) {
		return version{}, s.remoteErr("HeadObject", t, key, objectOp, err)
	}
	if mode == statFull && t.versioned() {
		// Some services answer a plain 404 for a delete marker.
		e, ok, err := s.listVersion(ctx, t, key, "")
		if err != nil {
			return version{}, err
		}
		if ok && e.DeleteMarker {
			return version{info: markerInfo(t.bucket, key, e)}, nil
		}
	}
	return version{}, goneErr(t.bucket, key, "")
}

// statVersion returns version vid of key.
func (s *Store) statVersion(ctx context.Context, t *target, key, vid string, mode statMode) (version, error) {
	var (
		a, la   s3client.ObjectAttrs
		lerr    error
		latestc = make(chan struct{})
	)
	if mode == statFull {
		lt := *t // the calls may learn a region: no sharing between goroutines
		go func() {
			defer close(latestc)
			lerr = s.call(ctx, &lt, func(c *s3client.Client) error {
				var err error
				la, err = c.HeadObjectWith(ctx, lt.b.Remote, key, s3client.HeadOptions{})
				return err
			})
		}()
	} else {
		close(latestc)
	}
	err := s.call(ctx, t, func(c *s3client.Client) error {
		var err error
		a, err = c.HeadObjectWith(ctx, t.b.Remote, key, s3client.HeadOptions{VersionID: vid})
		return err
	})
	<-latestc
	if err != nil {
		st, _ := statusOf(err)
		if _, ok := s3client.IsDeleteMarker(err); ok || st == http.StatusMethodNotAllowed {
			return s.marker(ctx, t, key, vid, false, mode)
		}
		if missingObject(err) {
			return version{}, goneErr(t.bucket, key, vid)
		}
		if missingVersion(err) {
			return version{}, notFound(t.bucket, key, vid)
		}
		return version{}, s.remoteErr("HeadObject", t, key, objectOp, err)
	}
	if versionOf(a.VersionID) != vid {
		// The remote ignored the version ID (a service without
		// versioning answering with the current object).
		return version{}, notFound(t.bucket, key, vid)
	}
	v, err := s.describe(ctx, t, key, a)
	if err != nil {
		return version{}, err
	}
	if mode == statFull {
		_, latestMarker := s3client.IsDeleteMarker(lerr)
		switch {
		case lerr == nil:
			v.info.IsLatest = versionOf(la.VersionID) == vid
		case latestMarker || missingObject(lerr):
		default:
			return version{}, s.remoteErr("HeadObject", t, key, objectOp, lerr)
		}
	}
	return v, nil
}

// marker returns the delete marker mvid of key (mvid "" = not reported:
// the latest one). latest says it was found as the latest version.
func (s *Store) marker(ctx context.Context, t *target, key, mvid string, latest bool, mode statMode) (version, error) {
	info := storage.ObjectInfo{Bucket: t.bucket, Key: key, VersionID: versionOf(mvid), DeleteMarker: true, IsLatest: latest}
	if mode != statFull {
		return version{info: info}, nil
	}
	want := mvid
	if latest {
		want = ""
	}
	e, ok, err := s.listVersion(ctx, t, key, want)
	switch {
	case err != nil:
		return version{}, err
	case ok && e.DeleteMarker && (mvid == "" || versionOf(e.VersionID) == versionOf(mvid)):
		return version{info: markerInfo(t.bucket, key, e)}, nil
	case !latest:
		// The marker is gone since the HEAD.
		return version{}, goneErr(t.bucket, key, mvid)
	}
	// The key changed since the HEAD: report the marker as seen then.
	info.LastModified = truncTime(time.Now())
	return version{info: info}, nil
}

// markerInfo returns the gos3 view of a delete marker listing entry.
func markerInfo(bucket, key string, e s3client.ObjectAttrs) storage.ObjectInfo {
	return storage.ObjectInfo{
		Bucket: bucket, Key: key, VersionID: versionOf(e.VersionID), DeleteMarker: true,
		IsLatest: e.IsLatest, LastModified: truncTime(e.LastModified),
	}
}

// listVersion looks up a version of key (vid "" = the newest entry) in
// the remote's version listing. ok is false when the key has no such
// version (or delete marker).
func (s *Store) listVersion(ctx context.Context, t *target, key, vid string) (e s3client.ObjectAttrs, ok bool, err error) {
	in := s3client.VersionListInput{Prefix: key}
	if vid == "" {
		in.MaxKeys = 1
	}
	for {
		var page s3client.VersionListOutput
		err := s.call(ctx, t, func(c *s3client.Client) error {
			var err error
			page, err = c.ListObjectVersions(ctx, t.b.Remote, in)
			return err
		})
		if err != nil {
			return e, false, s.remoteErr("ListObjectVersions", t, key, bucketOp, err)
		}
		for _, e := range page.Entries {
			if e.Key != key {
				return e, false, nil // past the key's versions
			}
			if vid == "" || versionOf(e.VersionID) == vid {
				return e, true, nil
			}
		}
		if !page.IsTruncated || page.NextKeyMarker == "" {
			return e, false, nil
		}
		in.KeyMarker, in.VersionIDMarker = page.NextKeyMarker, page.NextVersionIDMarker
	}
}

// describe builds the version of a HEAD/GET answer for an object: its
// gos3 metadata (see "Where metadata lives" in meta.go), reading native
// tags when the answer reports some. IsLatest is left to the caller.
func (s *Store) describe(ctx context.Context, t *target, key string, a s3client.ObjectAttrs) (version, error) {
	hm, ok := decodeHeaders(a)
	if !ok {
		s.log.Warn("native: ignoring a malformed gos3-meta header", "backing", s.name, "bucket", t.bucket, "key", key)
	}
	vid := versionOf(a.VersionID)
	v := version{
		info: storage.ObjectInfo{
			Bucket: t.bucket, Key: key, VersionID: vid, Size: a.Size, LastModified: truncTime(a.LastModified),
		},
		attrs: a,
		ext:   hm.ext,
		ov:    s.overlayFor(t.bucket, key, vid, a.ETag, a.LastModified),
	}
	if x := hm.ext; x != nil && (x.ACL != nil || x.Redirect != nil) {
		// Never from the object's headers, which anybody who can write
		// the remote bucket controls (see "Where metadata lives").
		s.log.Debug("native: ignoring the ACL and redirect of a gos3-meta header", "backing", s.name,
			"bucket", t.bucket, "key", key, "version", vid)
		x.ACL, x.Redirect = nil, nil
	}
	m := hm.meta
	hm.ext.apply(&m)
	var ovMeta *metaPatch
	if v.ov != nil {
		ovMeta = &v.ov.Meta
		ovMeta.apply(&m)
	}
	if hm.tagCount > 0 && (ovMeta == nil || ovMeta.Tags == nil) {
		var tags map[string]string
		err := s.call(ctx, t, func(c *s3client.Client) error {
			var err error
			tags, err = c.GetObjectTagging(ctx, t.b.Remote, key, a.VersionID)
			return err
		})
		switch {
		case err == nil:
			m.Tags = tags
		case missingVersion(err):
			// Replaced or deleted since the HEAD.
			return version{}, notFound(t.bucket, key, a.VersionID)
		default:
			return version{}, s.remoteErr("GetObjectTagging", t, key, objectOp, err)
		}
	}
	if t.b.ObjectLock || hm.hasRetention || hm.hasLegalHold {
		// The remote enforces object lock: report its settings only.
		m.RetentionMode, m.RetainUntil = "", time.Time{}
		if hm.hasRetention {
			m.RetentionMode, m.RetainUntil = hm.lockMode, hm.lockUntil
			for _, p := range []*metaPatch{ovMeta, hm.ext} {
				if p != nil && p.RetainPrecise != nil && ceilMillis(*p.RetainPrecise).Equal(hm.lockUntil) {
					m.RetainUntil = *p.RetainPrecise
					break
				}
			}
		}
		m.LegalHold = hm.legalHold
	}
	if m.PartChecksums != nil && len(m.PartChecksums) != len(m.PartSizes) {
		m.PartChecksums = nil
	}
	if hm.ext != nil && hm.ext.LastModified != 0 {
		v.info.LastModified = time.Unix(hm.ext.LastModified, 0).UTC()
	}
	v.info.ObjectMeta = m
	return v, nil
}

// listedInfo builds the gos3 view of a listing entry (ListObjectsV2 or
// ListObjectVersions): key, size, ETag, LastModified, storage class and
// version, with the version's overlay applied. Listings carry no
// headers, so user metadata, tags and what gos3-meta holds are not
// available without a HEAD per object, which listings deliberately avoid.
func (s *Store) listedInfo(t *target, key string, e s3client.ObjectAttrs) storage.ObjectInfo {
	info := storage.ObjectInfo{
		Bucket: t.bucket, Key: key, VersionID: versionOf(e.VersionID), DeleteMarker: e.DeleteMarker,
		IsLatest: e.IsLatest, LastModified: truncTime(e.LastModified),
	}
	if e.DeleteMarker {
		return info
	}
	info.Size = e.Size
	info.ETag = e.ETag
	info.StorageClass = normalizeStorageClass(e.StorageClass)
	if o := s.overlayFor(t.bucket, key, info.VersionID, e.ETag, e.LastModified); o != nil {
		m := info.ObjectMeta
		o.Meta.apply(&m)
		info.ObjectMeta = m
	}
	return info
}

// --- writer ---

// singlePutLimit returns the largest object s3client.Client.UploadObject
// sends with one PUT for an object of size bytes with part size ps
// (mirrors its part sizing).
func singlePutLimit(size, ps int64) int64 {
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
	if _, err := s.target(ctx, bucket); err != nil {
		return nil, err
	}
	if err := s.checkKey(key); err != nil {
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

// Commit uploads the buffered data as a new version: one PUT up to the
// part size, a remote multipart upload above it, with Content-MD5. The
// bucket's settings are those at commit time. Preconditions:
// IfNoneMatch is sent as "If-None-Match: *"; IfMatch is checked against
// the gos3 ETag (which may differ from the remote one) with a HEAD under
// the object's lock, and the upload is pinned to the remote ETag seen
// there with "If-Match", so it is atomic against other gos3 writers and,
// where the remote supports conditional writes, against everybody.
func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return storage.ObjectInfo{}, errWriterDone
	}
	w.done = true
	sum := w.md5.Sum(nil)
	w.mu.Unlock()
	defer w.spool.Close()
	if err := w.spool.Finish(); err != nil {
		return storage.ObjectInfo{}, err
	}
	s := w.s
	t, err := s.target(ctx, w.bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if s.gen(w.bucket) != w.gen {
		return storage.ObjectInfo{}, fmt.Errorf("%w: %q (deleted while writing)", storage.ErrBucketNotFound, w.bucket)
	}
	unlock := s.lockObject(w.bucket, w.key)
	defer unlock()
	meta = cloneMeta(meta)
	ifMatch, err := s.prePublish(ctx, t, w.key, &meta, opts)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return s.putObject(ctx, t, w.key, w.spool, w.spool.Size(), sum, meta, opts.IfNoneMatch, ifMatch)
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

// prePublish evaluates a commit's preconditions and BeforePublish hook
// under the object's lock (held by the caller), just before the object is
// published. The current object is read (HEAD) when either needs it.
// IfMatch is checked against the gos3 ETag; the remote ETag seen is
// returned, to pin the upload to it with "If-Match". The hook may change
// meta (not its encoding); the bucket configuration it is given is
// supplied by the caller of the store (the s3 account backing), so here
// it sees none.
func (s *Store) prePublish(ctx context.Context, t *target, key string, meta *storage.ObjectMeta, opts storage.CommitOptions) (remoteIfMatch string, err error) {
	if opts.IfMatch == "" && opts.BeforePublish == nil {
		return "", nil
	}
	cur, err := s.stat(ctx, t, key, "", statQuick)
	exists := err == nil && !cur.info.DeleteMarker
	if err != nil && !errors.Is(err, storage.ErrObjectNotFound) {
		return "", err
	}
	if opts.IfMatch != "" {
		if !exists {
			return "", fmt.Errorf("%w: %s/%s does not exist (If-Match)", storage.ErrPreconditionFailed, t.bucket, key)
		}
		if !etagMatches(opts.IfMatch, cur.info.ETag) {
			return "", fmt.Errorf("%w: ETag mismatch (If-Match)", storage.ErrPreconditionFailed)
		}
		remoteIfMatch = cur.attrs.ETag
	}
	if opts.BeforePublish != nil {
		var current *storage.ObjectMeta
		if exists {
			m := cloneMeta(cur.info.ObjectMeta)
			current = &m
		}
		encryption := meta.Encryption
		if err := opts.BeforePublish(meta, current, truncTime(time.Now()), func(string) []byte { return nil }); err != nil {
			return "", err
		}
		meta.Encryption = encryption // the encoding is the backend's choice
	}
	return remoteIfMatch, nil
}

// checkCompliance refuses COMPLIANCE retention on the remote when
// Options.ForbidCompliance is set (it can never be removed again).
func (s *Store) checkCompliance(t *target, mode string) error {
	if s.forbidCompliance && t.b.ObjectLock && strings.EqualFold(mode, "COMPLIANCE") {
		return fmt.Errorf("%w: native %q: COMPLIANCE retention is disabled for this backing", storage.ErrNotSupported, s.name)
	}
	return nil
}

// maxWriteAttempts bounds the attempts of a write whose outcome was
// unknown (see putObject).
const maxWriteAttempts = 2

// putObject uploads r[0:size] (MD5 sum; nil = not known) as the new
// version of key with meta. The caller holds the object's lock and ran
// prePublish.
//
// Conditional writes and remote multipart completions are sent once by
// the client (s3client's NoRetry): a retry could fail against the first
// attempt, which the service may have applied before failing. Instead,
// when the outcome is unknown (a 5xx answer or a transport error; a
// vanished internal multipart upload; a 412 after an unknown outcome),
// the key is read back: if it holds this write's content (ETag: the MD5,
// or the multipart ETag of the parts), the write succeeded; otherwise it
// is tried once more (unknown outcomes only) or fails.
func (s *Store) putObject(ctx context.Context, t *target, key string, r io.ReaderAt, size int64, sum []byte,
	meta storage.ObjectMeta, ifNoneMatch bool, ifMatch string) (storage.ObjectInfo, error) {
	if err := s.checkCompliance(t, meta.RetentionMode); err != nil {
		return storage.ObjectInfo{}, err
	}
	configured := s.uploadPartSize()
	partSize := singlePutLimit(size, configured) // also the part size of a multipart upload
	multipart := size > partSize
	keepETag := multipart || meta.ETag != hex.EncodeToString(sum)
	conditional := ifNoneMatch || ifMatch != ""
	nullID := overlayID{t.bucket, key, storage.NullVersionID}
	var hidden *overlay
	if t.nullWrites() {
		// The null version is replaced: readers must not apply its
		// overlay to the new object; it is dropped once the write is
		// acknowledged, restored if it fails.
		hidden = s.hideOverlay(nullID)
	}
	var (
		enc      encoded
		res      s3client.WriteResult
		err      error
		expected string // ETag of this write's content, once computed
		unsure   bool   // an earlier attempt may have been applied
		retries  int
	)
	for attempt := 0; ; attempt++ {
		enc = encodeMeta(meta, encodeOpts{keepETag: keepETag, lock: t.b.ObjectLock, tagging: s.tagging(t)})
		err = s.call(ctx, t, func(c *s3client.Client) error {
			var err error
			res, err = c.UploadObject(ctx, t.b.Remote, key, r, size, s3client.UploadOptions{
				PutOptions: s3client.PutOptions{Header: enc.header, IfNoneMatch: ifNoneMatch, IfMatch: ifMatch, ContentMD5: sum,
					NoRetry: conditional || multipart},
				PartSize: configured,
				PartMD5:  true,
			})
			return err
		})
		if err == nil {
			break
		}
		if enc.tags && attempt == 0 && s3client.IsNotImplemented(err) {
			s.noNativeTags(t, err.Error())
			continue
		}
		if ambiguous(err) || s3client.IsCode(err, "NoSuchUpload") || unsure && s3client.IsPreconditionFailed(err) {
			if expected == "" {
				expected = expectedETag(r, size, sum, partSize)
			}
			if got, ok := s.landed(ctx, t, key, expected); ok {
				res, err = got, nil
				break
			}
			if ambiguous(err) && retries < maxWriteAttempts-1 {
				retries++
				unsure = true
				continue
			}
		}
		break
	}
	if err != nil {
		s.restoreOverlay(nullID, hidden)
		s.checkLockRefusal(t, enc, err)
		if s3client.IsCode(err, "NoSuchUpload") {
			// The multipart upload carrying this PUT was removed on the
			// remote (a lifecycle rule, another client).
			return storage.ObjectInfo{}, fmt.Errorf("native %q: PutObject %s/%s: the remote multipart upload of the write was removed: %w: %w",
				s.name, t.bucket, key, storage.ErrBucketUnavailable, err)
		}
		return storage.ObjectInfo{}, s.remoteErr("PutObject", t, key, objectOp, err)
	}
	s.forgetOverlay(ctx, nullID, hidden)
	return s.written(ctx, t, key, res, meta, size, enc)
}

// checkLockRefusal reports drift when the remote refused a write carrying
// object lock headers with 400 InvalidRequest (AWS and gos3: "Bucket is
// missing Object Lock Configuration").
func (s *Store) checkLockRefusal(t *target, enc encoded, err error) {
	if st, code := statusOf(err); enc.lock && st == http.StatusBadRequest && code == "InvalidRequest" {
		s.drift(t.bucket, "the remote refused object lock settings: "+err.Error())
	}
}

// written completes a write that the remote acknowledged with res: it
// reads the new version back (HEAD) for its LastModified, checks that
// native tags were kept, stores the overlay of what the headers could not
// hold and returns the version's info.
func (s *Store) written(ctx context.Context, t *target, key string, res s3client.WriteResult, meta storage.ObjectMeta,
	size int64, enc encoded) (storage.ObjectInfo, error) {
	vid := versionOf(res.VersionID)
	switch {
	case !t.versioned() && vid != storage.NullVersionID:
		s.drift(t.bucket, "the remote bucket is versioned")
	case t.b.Versioning == storage.VersioningEnabled && vid == storage.NullVersionID:
		s.drift(t.bucket, "versioning is not enabled on the remote bucket")
	}
	info := storage.ObjectInfo{
		ObjectMeta: cloneMeta(meta),
		Bucket:     t.bucket,
		Key:        key,
		VersionID:  vid,
		IsLatest:   true,
		Size:       size,
	}
	var a s3client.ObjectAttrs
	herr := s.call(ctx, t, func(c *s3client.Client) error {
		var err error
		a, err = c.HeadObjectWith(ctx, t.b.Remote, key, s3client.HeadOptions{VersionID: res.VersionID})
		return err
	})
	// The HEAD of a null version may see a later write.
	seen := herr == nil && a.VersionID == res.VersionID && (vid != storage.NullVersionID || a.ETag == res.ETag)
	remoteETag, lastModified := res.ETag, time.Now()
	if seen {
		remoteETag, lastModified = a.ETag, a.LastModified
	}
	info.LastModified = truncTime(lastModified)

	patch := metaPatch{}
	essential := false
	if enc.overflow != nil {
		patch, essential = *enc.overflow, true
	}
	if enc.tags && seen {
		if hm, _ := decodeHeaders(a); hm.tagCount != len(meta.Tags) {
			// The remote accepted x-amz-tagging but did not keep the tags.
			s.noNativeTags(t, "the remote ignored x-amz-tagging")
			patch.Tags, essential = mapPtr(meta.Tags), true
		}
	}
	if meta.ETag != remoteETag {
		// For listings, which see the remote ETag only (ext has it too).
		etag := meta.ETag
		patch.ETag = &etag
	}
	if patch.empty() {
		return info, nil
	}
	var err error
	switch {
	case seen || vid != storage.NullVersionID:
		err = s.putOverlay(ctx, overlayID{t.bucket, key, vid}, remoteETag, lastModified, &patch)
	case herr != nil && !missingObject(herr):
		err = s.remoteErr("HeadObject", t, key, objectOp, herr)
	default:
		// Another client replaced or deleted the object right after the
		// write: the null version this metadata was for is gone.
		err = fmt.Errorf("native %q: %s/%s was replaced or deleted on the remote before its metadata could be stored: %w",
			s.name, t.bucket, key, storage.ErrPreconditionFailed)
	}
	if err != nil {
		if essential {
			return storage.ObjectInfo{}, err
		}
		s.log.Warn("native: store the ETag of a new object for listings", "backing", s.name, "bucket", t.bucket, "key", key, "err", err)
	}
	return info, nil
}

// --- reads ---

// GetObjectInfo returns the metadata of a version (HEAD).
func (s *Store) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	t, err := s.target(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	before := time.Now()
	v, err := s.stat(ctx, t, key, versionID, statFull)
	s.pruneMissing(ctx, t, key, versionID, before, false, err)
	return v.info, err
}

// pruneMissing drops the overlay of the version addressed by vid ("" =
// the null version of an unversioned bucket) when err, the outcome of a
// lookup started at before, proves it gone. locked: the caller holds the
// object's lock.
func (s *Store) pruneMissing(ctx context.Context, t *target, key, vid string, before time.Time, locked bool, err error) {
	if !errors.Is(err, errGone) {
		return
	}
	if vid == "" {
		if t.versioned() {
			return
		}
		vid = storage.NullVersionID
	}
	s.pruneVersion(ctx, t.bucket, key, vid, before, locked)
}

// OpenObject GETs the object; info and the first bytes come from the
// same response, so they always describe one version. The reader keeps
// reading that response; after a Seek (or a broken connection) it issues
// ranged GETs pinned to the version (versionId) and the remote ETag,
// which fail with an error wrapping storage.ErrPreconditionFailed when an
// unversioned object was replaced meanwhile: the old content is gone.
func (s *Store) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	t, err := s.target(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	if s.checkKey(key) != nil {
		return storage.ObjectInfo{}, nil, notFound(bucket, key, versionID)
	}
	byVersion := versionID != "" && (t.versioned() || versionID != storage.NullVersionID)
	before := time.Now()
	for {
		info, r, err := s.open(ctx, t, key, versionID, byVersion)
		s.pruneMissing(ctx, t, key, versionID, before, false, err)
		if errors.Is(err, errVersioned) {
			// An unversioned bucket's null version on a versioned remote.
			s.drift(t.bucket, "the remote bucket is versioned")
			byVersion = true
			continue
		}
		return info, r, err
	}
}

// errVersioned is returned by open when the current object of a bucket
// recorded as unversioned has a version ID.
var errVersioned = errors.New("native: the remote object has a version ID")

// open is OpenObject's work: a GET of the latest version (byVersion
// false; versionID is then "" or "null") or of versionID.
func (s *Store) open(ctx context.Context, t *target, key, versionID string, byVersion bool) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	var (
		body    io.ReadCloser
		a, la   s3client.ObjectAttrs
		lerr    error
		latestc = make(chan struct{})
	)
	if byVersion {
		lt := *t
		go func() {
			defer close(latestc)
			lerr = s.call(ctx, &lt, func(c *s3client.Client) error {
				var err error
				la, err = c.HeadObjectWith(ctx, lt.b.Remote, key, s3client.HeadOptions{})
				return err
			})
		}()
	} else {
		close(latestc)
	}
	opts := s3client.GetOptions{}
	if byVersion {
		opts.VersionID = versionID
	}
	err := s.call(ctx, t, func(c *s3client.Client) error {
		var err error
		body, a, err = c.GetObject(ctx, t.b.Remote, key, opts)
		return err
	})
	<-latestc
	fail := func(err error) (storage.ObjectInfo, io.ReadSeekCloser, error) {
		if body != nil {
			_ = body.Close()
		}
		return storage.ObjectInfo{}, nil, err
	}
	if err != nil {
		st, _ := statusOf(err)
		mvid, isMarker := s3client.IsDeleteMarker(err)
		switch {
		case !byVersion && isMarker:
			v, err := s.marker(ctx, t, key, mvid, true, statFull)
			return v.info, nil, err
		case byVersion && (isMarker || st == http.StatusMethodNotAllowed):
			v, err := s.marker(ctx, t, key, versionID, false, statFull)
			return v.info, nil, err
		case !byVersion && missingObject(err):
			if t.versioned() {
				// Some services answer a plain 404 for a delete marker.
				e, ok, err := s.listVersion(ctx, t, key, "")
				if err != nil {
					return fail(err)
				}
				if ok && e.DeleteMarker {
					return markerInfo(t.bucket, key, e), nil, nil
				}
			}
			if !t.versioned() {
				return fail(goneErr(t.bucket, key, versionID))
			}
			return fail(notFound(t.bucket, key, versionID))
		case byVersion && missingObject(err):
			return fail(goneErr(t.bucket, key, versionID))
		case byVersion && missingVersion(err):
			return fail(notFound(t.bucket, key, versionID))
		}
		return fail(s.remoteErr("GetObject", t, key, objectOp, err))
	}
	switch {
	case !byVersion && versionID == storage.NullVersionID && a.VersionID != "" && a.VersionID != storage.NullVersionID:
		return fail(errVersioned)
	case byVersion && versionOf(a.VersionID) != versionID:
		return fail(notFound(t.bucket, key, versionID))
	}
	if a.Size < 0 {
		// No Content-Length: learn the size from a HEAD of the version.
		_ = body.Close()
		body = nil
		var ha s3client.ObjectAttrs
		err := s.call(ctx, t, func(c *s3client.Client) error {
			var err error
			ha, err = c.HeadObjectWith(ctx, t.b.Remote, key, s3client.HeadOptions{VersionID: a.VersionID})
			return err
		})
		if err != nil || ha.ETag != a.ETag {
			return fail(s.remoteErr("HeadObject", t, key, objectOp, errors.Join(err, errors.New("object changed while being opened"))))
		}
		a = ha
	}
	v, err := s.describe(ctx, t, key, a)
	if err != nil {
		return fail(err)
	}
	v.info.IsLatest = true
	if byVersion {
		_, latestMarker := s3client.IsDeleteMarker(lerr)
		switch {
		case lerr == nil:
			v.info.IsLatest = versionOf(la.VersionID) == v.info.VersionID
		case latestMarker || missingObject(lerr):
			v.info.IsLatest = false
		default:
			return fail(s.remoteErr("HeadObject", t, key, objectOp, lerr))
		}
	}
	r := &reader{s: s, t: t, ctx: ctx, key: key, size: a.Size, vid: a.VersionID, etag: a.ETag, body: body}
	return v.info, r, nil
}

// drainLimit is the largest unread response remainder a reader drains
// (instead of dropping the connection) when it abandons a response.
const drainLimit = 64 << 10

// reader reads one remote object version (see OpenObject).
type reader struct {
	s    *Store
	t    *target
	ctx  context.Context
	key  string
	size int64
	vid  string // remote version ID every request is pinned to ("" = none)
	etag string // remote ETag every request is pinned to

	mu     sync.Mutex
	off    int64
	body   io.ReadCloser // response positioned at off, or nil
	closed bool
}

var errReaderClosed = errors.New("native: read from a closed object reader")

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
			body, _, err := r.t.c.GetObject(r.ctx, r.t.b.Remote, r.key, s3client.GetOptions{Offset: r.off, IfMatch: r.etag, VersionID: r.vid})
			if err != nil {
				if s3client.IsPreconditionFailed(err) || missingVersion(err) {
					return 0, fmt.Errorf("native %q: %s/%s was replaced or deleted on the remote while being read: %w",
						r.s.name, r.t.bucket, r.key, storage.ErrPreconditionFailed)
				}
				return 0, r.s.remoteErr("GetObject", r.t, r.key, objectOp, err)
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
			return 0, fmt.Errorf("native %q: %s/%s ended at %d of %d bytes: %w", r.s.name, r.t.bucket, r.key, r.off, r.size, io.ErrUnexpectedEOF)
		}
		if attempt > 0 || r.ctx.Err() != nil {
			return 0, r.s.remoteErr("GetObject", r.t, r.key, objectOp, err)
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
// open response; the next Read issues a ranged GET pinned to the version.
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
		return 0, errors.New("native: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("native: negative position")
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
