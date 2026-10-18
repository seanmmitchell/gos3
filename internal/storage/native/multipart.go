package native

import (
	"cmp"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Multipart uploads
//
// A gos3 upload has its own ID and a record (bucket, key, metadata,
// initiator, parts, and the remote bucket and region of its remote
// upload), persisted in Options.State. Its parts go to a remote multipart
// upload of the target key, created lazily for the first part that needs
// it (with the creation-time metadata: content headers, user metadata,
// tags, object lock headers and gos3-meta), or are staged in the
// StateStore:
//
//   - A part of at least s3client.MinPartSize (5 MiB) is uploaded to the
//     remote upload right away, under the same part number, with
//     Content-MD5.
//   - A smaller part is staged (StateStore.PutStaged) and only uploaded at
//     completion. S3 rejects completions whose non-last parts are below
//     5 MiB, and parts of a remote upload cannot be read back, so small
//     parts must stay readable until it is known how they are used.
//
// CompleteMultipartUpload then takes one of two paths:
//
//   - Every listed part is staged: the parts are concatenated and written
//     like a new object (single PUT up to the part size, else a fresh
//     remote multipart upload), with the final metadata; the lazily
//     created remote upload, if any, is aborted. This covers small objects
//     uploaded in parts and every upload whose parts are all small.
//   - Every listed part but the last is remote and the part numbers are
//     ascending: a staged last part is uploaded now and the remote upload
//     is completed with the remote parts. This is always the case for
//     uploads the S3 layer accepts (it enforces ascending part numbers and
//     5 MiB non-last parts), so large uploads are never staged. The
//     metadata known only at completion (the ETag, checksums, part sizes,
//     tags or retention changed by the S3 layer's hook) is then applied
//     like a metadata update (applyMeta): native tags and object lock
//     settings through their APIs (retention only ever extended), the
//     rest as an overlay. The object is never copied onto itself.
//
// Any other combination (a small non-last part next to remote parts, or a
// non-ascending list including remote parts) cannot be assembled from
// parts the remote no longer lets anyone read, and fails with an error
// wrapping storage.ErrNotSupported; the S3 layer never asks for it.

// maxPartNumber is the largest part number (S3 allows 1..10000).
const maxPartNumber = s3client.MaxParts

// upload is an in-progress multipart upload.
type upload struct {
	// Immutable after creation.
	id        string
	bucket    string
	key       string
	initiated time.Time
	meta      storage.ObjectMeta
	initiator string

	// op is held shared by part commits and exclusively by Complete,
	// Abort and DetachBucket, so an upload never finishes under a part
	// commit in flight.
	op sync.RWMutex
	// createMu serializes the lazy creation of the remote upload.
	createMu sync.Mutex

	// Guarded by Store.mu.
	remoteID string
	// remoteBucket and region locate the remote upload. Records of the
	// former gateway backing have none: they are filled in from the
	// bucket's binding at first use.
	remoteBucket, region string
	// remoteSaved reports that the upload record holding remoteID was
	// written (remote parts are only uploaded after that).
	remoteSaved bool
	parts       map[int]*part
	gone        bool
}

// part is a committed part: remote (remoteETag set) or staged (file set).
type part struct {
	info       storage.PartInfo
	remoteETag string
	file       string // staged blob name in the StateStore
	md5        string // hex MD5 of a staged part ("" in old records)
}

func (u *upload) info() storage.UploadInfo {
	return storage.UploadInfo{
		UploadID:  u.id,
		Bucket:    u.bucket,
		Key:       u.key,
		Initiated: u.initiated,
		Meta:      cloneMeta(u.meta),
		Initiator: u.initiator,
	}
}

// info0 is info without the metadata copy (for comparisons).
func (u *upload) info0() storage.UploadInfo {
	return storage.UploadInfo{UploadID: u.id, Bucket: u.bucket, Key: u.key, Initiated: u.initiated}
}

func clonePartInfo(p storage.PartInfo) storage.PartInfo {
	p.Checksums = maps.Clone(p.Checksums)
	return p
}

// compareUploads orders uploads by key, Initiated, upload ID.
func compareUploads(a, b *upload) int {
	return cmp.Or(strings.Compare(a.key, b.key), a.initiated.Compare(b.initiated), strings.Compare(a.id, b.id))
}

// newUploadID returns a random, URL-safe upload ID (32 characters of
// [A-Za-z0-9_-]: staged part names built from it stay in the charset
// s3store's orphan recovery reclaims).
func newUploadID() string {
	var b [24]byte
	_, _ = rand.Read(b[:]) // never fails (Go >= 1.24)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// addUploadLocked registers u (s.mu held).
func (s *Store) addUploadLocked(u *upload) {
	s.uploads[u.id] = u
	list := s.byBucket[u.bucket]
	i, _ := slices.BinarySearchFunc(list, u, compareUploads)
	s.byBucket[u.bucket] = slices.Insert(list, i, u)
}

// removeUploadLocked unregisters u (s.mu held).
func (s *Store) removeUploadLocked(u *upload) {
	delete(s.uploads, u.id)
	list := s.byBucket[u.bucket]
	if i, ok := slices.BinarySearchFunc(list, u, compareUploads); ok {
		list = slices.Delete(list, i, i+1)
	}
	if len(list) == 0 {
		delete(s.byBucket, u.bucket)
	} else {
		s.byBucket[u.bucket] = list
	}
}

// findUpload returns the upload with the given ID if it belongs to
// bucket/key; an unknown bucket yields the error of Options.Resolve.
func (s *Store) findUpload(ctx context.Context, bucket, key, id string) (*upload, error) {
	if _, err := s.target(ctx, bucket); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[id]
	if u == nil || u.gone || u.bucket != bucket || u.key != key {
		return nil, fmt.Errorf("%w: %q", storage.ErrUploadNotFound, id)
	}
	return u, nil
}

func (s *Store) isGone(u *upload) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return u.gone
}

func (s *Store) remoteUploadID(u *upload) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return u.remoteID
}

// uploadTarget resolves u's bucket and points the target at the remote
// bucket (and region) of u's remote upload, filling them in from the
// binding for uploads recorded without them.
func (s *Store) uploadTarget(ctx context.Context, u *upload) (*target, error) {
	t, err := s.target(ctx, u.bucket)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if u.remoteBucket == "" {
		u.remoteBucket, u.region = t.b.Remote, t.region
	}
	rb, region := u.remoteBucket, u.region
	if r, ok := s.regions[rb]; ok {
		region = r
	}
	s.mu.Unlock()
	if rb != t.b.Remote || region != t.region {
		t.b.Remote, t.region, t.c = rb, region, s.client().ForRegion(region)
	}
	return t, nil
}

// dropUpload deletes u's records, then forgets u and deletes its staged
// parts (the remote upload, if any, must have been completed or aborted,
// or be gone). The caller holds u.op exclusively.
//
// The records are deleted even if ctx is cancelled, upload record first:
// once it is gone the upload cannot come back at Open, which removes the
// leftover part records and staged parts. If deleting the records fails,
// the staged parts are kept (a record that may still exist refers to
// them) and the error is returned; u stays registered, so the operation
// can be retried, unless force is set (the upload is consumed whatever
// happens to its records).
func (s *Store) dropUpload(ctx context.Context, u *upload, force bool) error {
	ctx = context.WithoutCancel(ctx)
	err := s.deleteUploadState(ctx, u.id)
	if err != nil && !force {
		return err
	}
	s.mu.Lock()
	u.gone = true
	s.removeUploadLocked(u)
	var files []string
	for _, p := range u.parts {
		if p.file != "" {
			files = append(files, p.file)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.cleanupStaged(ctx)
	for _, f := range files {
		s.deleteStaged(ctx, f)
	}
	return nil
}

// dropVanished drops u after its remote upload rid turned out to be gone
// (errRemoteUploadGone): its remote parts are lost, so the gos3 upload
// cannot be completed any more. The caller must not hold u.op.
func (s *Store) dropVanished(ctx context.Context, u *upload, rid string) {
	u.op.Lock()
	defer u.op.Unlock()
	s.dropVanishedLocked(ctx, u, rid)
}

// dropVanishedLocked is dropVanished with u.op held exclusively.
func (s *Store) dropVanishedLocked(ctx context.Context, u *upload, rid string) {
	s.mu.Lock()
	still := !u.gone && u.remoteID == rid
	s.mu.Unlock()
	if !still {
		return
	}
	s.log.Warn("native: remote multipart upload vanished; upload dropped", "backing", s.name, "bucket", u.bucket, "key", u.key, "upload", u.id)
	if err := s.dropUpload(ctx, u, true); err != nil {
		s.log.Warn("native: drop upload state", "backing", s.name, "upload", u.id, "err", err)
	}
}

// CreateMultipartUpload starts an upload. The remote multipart upload is
// created with the first part that is uploaded to it.
func (s *Store) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	t, err := s.target(ctx, bucket)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	if err := s.checkKey(key); err != nil {
		return storage.UploadInfo{}, err
	}
	if err := s.checkCompliance(t, meta.RetentionMode); err != nil {
		return storage.UploadInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.UploadInfo{}, err
	}
	u := &upload{
		id:           newUploadID(),
		bucket:       bucket,
		key:          key,
		initiated:    time.Now().UTC().Round(0),
		meta:         cloneMeta(meta),
		initiator:    initiator,
		remoteBucket: t.b.Remote,
		region:       t.region,
		parts:        map[int]*part{},
	}
	if s.isClosed() {
		return storage.UploadInfo{}, errClosed
	}
	// The record is written without s.mu (the state store may be remote);
	// nobody can reach u before it is registered.
	if err := s.saveUpload(ctx, u); err != nil {
		// The write may have landed anyway: remove the record, best
		// effort (no staged part refers to it).
		_ = s.state.DeleteRecords(context.WithoutCancel(ctx), uploadKey(u.id))
		return storage.UploadInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = s.state.DeleteRecords(context.WithoutCancel(ctx), uploadKey(u.id))
		return storage.UploadInfo{}, errClosed
	}
	s.addUploadLocked(u)
	return u.info(), nil
}

// GetMultipartUpload returns an in-progress upload of bucket/key.
func (s *Store) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	u, err := s.findUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	return u.info(), nil
}

// ListParts returns the committed parts sorted by part number.
func (s *Store) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	u, err := s.findUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	out := make([]storage.PartInfo, 0, len(u.parts))
	for _, p := range u.parts {
		out = append(out, clonePartInfo(p.info))
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b storage.PartInfo) int { return a.PartNumber - b.PartNumber })
	return out, nil
}

// ListMultipartUploads returns the bucket's uploads in listing order.
func (s *Store) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	if _, err := s.target(ctx, bucket); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.byBucket[bucket]
	out := make([]storage.UploadInfo, len(list))
	for i, u := range list {
		out[i] = u.info()
	}
	return out, nil
}

// walkBatch is the number of uploads WalkMultipartUploads copies per lock.
const walkBatch = 256

// WalkMultipartUploads implements storage.UploadWalker: it seeks in the
// bucket's sorted upload list and calls fn in batches without the lock.
func (s *Store) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	if fn == nil {
		return errors.New("native: nil walk function")
	}
	if _, err := s.target(ctx, bucket); err != nil {
		return err
	}
	for {
		s.mu.Lock()
		list := s.byBucket[bucket]
		i := sort.Search(len(list), func(i int) bool { return after.Precedes(list[i].info0()) })
		j := sort.Search(len(list), func(i int) bool { return list[i].key >= prefix })
		batch := make([]storage.UploadInfo, 0, min(walkBatch, len(list)))
		more := false
		for k := max(i, j); k < len(list) && strings.HasPrefix(list[k].key, prefix); k++ {
			if len(batch) == walkBatch {
				more = true
				break
			}
			batch = append(batch, list[k].info())
		}
		s.mu.Unlock()
		for _, u := range batch {
			if err := fn(u); err != nil {
				if errors.Is(err, storage.ErrStopWalk) {
					return nil
				}
				return err
			}
		}
		if !more {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		after = storage.PositionOf(batch[len(batch)-1])
	}
}

// ensureRemoteUpload returns the ID of u's remote multipart upload,
// creating it (with the creation-time metadata) on first use. It returns
// only once the upload record holding the ID is saved, so no remote part
// is recorded without it; a failed save is retried by the next call.
func (s *Store) ensureRemoteUpload(ctx context.Context, t *target, u *upload) (string, error) {
	u.createMu.Lock()
	defer u.createMu.Unlock()
	s.mu.Lock()
	id, saved := u.remoteID, u.remoteSaved
	s.mu.Unlock()
	if id == "" {
		for attempt := 0; ; attempt++ {
			// What does not travel with the object (the ACL, the
			// redirect, an overflow) is stored at completion.
			enc := encodeMeta(u.meta, encodeOpts{lock: t.b.ObjectLock, tagging: s.tagging(t)})
			err := s.call(ctx, t, func(c *s3client.Client) error {
				var err error
				id, err = c.CreateMultipartUpload(ctx, t.b.Remote, u.key, enc.header)
				return err
			})
			if err == nil {
				break
			}
			if enc.tags && attempt == 0 && s3client.IsNotImplemented(err) {
				s.noNativeTags(t, err.Error())
				continue
			}
			s.checkLockRefusal(t, enc, err)
			return "", s.remoteErr("CreateMultipartUpload", t, u.key, objectOp, err)
		}
		s.mu.Lock()
		u.remoteID = id
		s.mu.Unlock()
	}
	if !saved {
		// The remote upload exists: record it even if ctx is cancelled.
		if err := s.saveUpload(context.WithoutCancel(ctx), u); err != nil {
			return "", err
		}
		s.mu.Lock()
		u.remoteSaved = true
		s.mu.Unlock()
	}
	return id, nil
}

// --- parts ---

type partWriter struct {
	s      *Store
	u      *upload
	number int

	mu    sync.Mutex
	spool *s3client.Spool
	md5   hash.Hash
	done  bool
}

// NewPartWriter starts writing part partNumber (1..10000) of an upload.
func (s *Store) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	if partNumber < 1 || partNumber > maxPartNumber {
		return nil, fmt.Errorf("native: invalid part number %d", partNumber)
	}
	u, err := s.findUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return nil, err
	}
	return &partWriter{s: s, u: u, number: partNumber, spool: s3client.NewSpool(s.spoolDir, 0), md5: md5.New()}, nil
}

func (w *partWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	n, err := w.spool.Write(p)
	w.md5.Write(p[:n])
	return n, err
}

// Commit uploads the part to the remote upload (>= 5 MiB) or stages it in
// the StateStore, replacing an earlier part with the same number. When the
// remote upload turns out to be gone, the gos3 upload is dropped.
func (w *partWriter) Commit(ctx context.Context, meta storage.PartMeta) (storage.PartInfo, error) {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return storage.PartInfo{}, errWriterDone
	}
	w.done = true
	sum := w.md5.Sum(nil)
	w.mu.Unlock()
	defer w.spool.Close()
	if err := w.spool.Finish(); err != nil {
		return storage.PartInfo{}, err
	}
	info, rid, err := w.commit(ctx, meta, sum)
	if errors.Is(err, errRemoteUploadGone) {
		w.s.dropVanished(ctx, w.u, rid)
	}
	return info, err
}

// commit does Commit's work under u.op (shared). It also returns the
// remote upload ID it used.
func (w *partWriter) commit(ctx context.Context, meta storage.PartMeta, sum []byte) (storage.PartInfo, string, error) {
	s, u := w.s, w.u
	u.op.RLock()
	defer u.op.RUnlock()
	if s.isGone(u) {
		return storage.PartInfo{}, "", fmt.Errorf("%w: %q", storage.ErrUploadNotFound, u.id)
	}
	unlock := s.keys.lock("p\x00" + u.id + "\x00" + strconv.Itoa(w.number))
	defer unlock()

	size := w.spool.Size()
	p := &part{info: storage.PartInfo{
		PartMeta:     storage.PartMeta{ETag: meta.ETag, Checksums: maps.Clone(meta.Checksums)},
		PartNumber:   w.number,
		Size:         size,
		LastModified: time.Now().UTC().Round(0),
	}}
	var rid string
	if size >= s3client.MinPartSize {
		t, err := s.uploadTarget(ctx, u)
		if err != nil {
			return storage.PartInfo{}, "", err
		}
		rid, err = s.ensureRemoteUpload(ctx, t, u)
		if err != nil {
			return storage.PartInfo{}, "", err
		}
		var etag string
		err = s.call(ctx, t, func(c *s3client.Client) error {
			var err error
			etag, err = c.UploadPartWith(ctx, t.b.Remote, u.key, rid, w.number, w.spool, size, s3client.PartOptions{ContentMD5: sum})
			return err
		})
		if err != nil {
			return storage.PartInfo{}, rid, s.remoteErr("UploadPart", t, u.key, objectOp, err)
		}
		p.remoteETag = etag
	} else {
		file, err := s.stagePart(ctx, u.id, w.number, w.spool, size)
		if err != nil {
			return storage.PartInfo{}, "", err
		}
		p.file, p.md5 = file, hex.EncodeToString(sum)
	}
	// The part is stored: record it even if ctx is cancelled. A failed
	// write may have landed anyway, so a staged part stays (Open removes
	// it if no record refers to it).
	if err := s.savePart(context.WithoutCancel(ctx), u.id, p); err != nil {
		return storage.PartInfo{}, "", err
	}
	s.mu.Lock()
	old := u.parts[w.number]
	u.parts[w.number] = p
	s.mu.Unlock()
	if old != nil && old.file != "" {
		// No record refers to it any more. (Completions, the only
		// readers of staged parts, hold u.op exclusively: none has it
		// open.)
		s.cleanupStaged(context.WithoutCancel(ctx))
		s.deleteStaged(context.WithoutCancel(ctx), old.file)
	}
	return clonePartInfo(p.info), "", nil
}

func (w *partWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		w.done = true
		return w.spool.Close()
	}
	return nil
}

// stagePart stores a small part as a staged blob and returns its name,
// unique per commit so that a replaced part's blob stays readable until
// the new part record is saved. Names are of [A-Za-z0-9_-] only.
func (s *Store) stagePart(ctx context.Context, id string, n int, r io.ReaderAt, size int64) (string, error) {
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	name := fmt.Sprintf("%s-%05d-%s", id, n, base64.RawURLEncoding.EncodeToString(rnd[:]))
	if err := s.state.PutStaged(ctx, name, r, size); err != nil {
		s.deleteStaged(context.WithoutCancel(ctx), name)
		return "", fmt.Errorf("native %q: stage part: %w", s.name, err)
	}
	return name, nil
}

// --- completion ---

// CompleteMultipartUpload assembles the listed parts (see the package
// comment above for the two paths) and commits the object with meta and
// opts like ObjectWriter.Commit. The upload is kept when the completion
// fails before the object was published.
func (s *Store) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	u, err := s.findUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	u.op.Lock()
	defer u.op.Unlock()
	if opts.PartETags != nil && len(opts.PartETags) != len(partNumbers) {
		return storage.ObjectInfo{}, fmt.Errorf("native: %d part ETags for %d parts", len(opts.PartETags), len(partNumbers))
	}
	if len(partNumbers) == 0 {
		return storage.ObjectInfo{}, fmt.Errorf("%w: no parts listed", storage.ErrPartNotFound)
	}
	s.mu.Lock()
	gone, remoteID := u.gone, u.remoteID
	sel := make([]*part, len(partNumbers))
	for i, n := range partNumbers {
		sel[i] = u.parts[n]
	}
	s.mu.Unlock()
	if gone {
		return storage.ObjectInfo{}, fmt.Errorf("%w: %q", storage.ErrUploadNotFound, uploadID)
	}
	var total int64
	allStaged, remoteOK := true, remoteID != ""
	for i, p := range sel {
		n := partNumbers[i]
		if p == nil {
			return storage.ObjectInfo{}, fmt.Errorf("%w: part %d of upload %q", storage.ErrPartNotFound, n, uploadID)
		}
		if opts.PartETags != nil && p.info.ETag != opts.PartETags[i] {
			return storage.ObjectInfo{}, fmt.Errorf("%w: part %d of upload %q was replaced (ETag %q, want %q)",
				storage.ErrPartNotFound, n, uploadID, p.info.ETag, opts.PartETags[i])
		}
		total += p.info.Size
		if p.file == "" {
			allStaged = false
		} else if i < len(sel)-1 {
			remoteOK = false
		}
		if i > 0 && n <= partNumbers[i-1] {
			remoteOK = false
		}
	}
	if !allStaged && !remoteOK {
		return storage.ObjectInfo{}, fmt.Errorf("%w: native %q cannot complete upload %q: parts below %d bytes other than the last, or part numbers out of order, together with parts already uploaded to the remote",
			storage.ErrNotSupported, s.name, uploadID, s3client.MinPartSize)
	}

	// The bucket's settings at completion apply (versioning, object lock).
	t, err := s.uploadTarget(ctx, u)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	unlock := s.lockObject(bucket, key)
	defer unlock()
	meta = cloneMeta(meta)
	ifMatch, err := s.prePublish(ctx, t, key, &meta, opts)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := s.checkCompliance(t, meta.RetentionMode); err != nil {
		return storage.ObjectInfo{}, err
	}
	if allStaged {
		mr := s.newStagedReader(ctx, u.id, sel)
		// The MD5 of the content is computed by the client while reading.
		info, err := s.putObject(ctx, t, key, mr, total, nil, meta, opts.IfNoneMatch, ifMatch)
		// Close the staged parts before they are deleted (an open file
		// cannot be deleted on Windows).
		mr.Close()
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if remoteID != "" {
			err := s.call(context.WithoutCancel(ctx), t, func(c *s3client.Client) error {
				return c.AbortMultipartUpload(context.WithoutCancel(ctx), t.b.Remote, key, remoteID)
			})
			if err != nil && !s3client.IsCode(err, "NoSuchUpload") {
				s.log.Warn("native: abort unused remote multipart upload", "backing", s.name, "bucket", bucket, "key", key, "err", err)
			}
		}
		if err := s.dropUpload(ctx, u, true); err != nil {
			s.log.Warn("native: drop completed upload state", "backing", s.name, "upload", uploadID, "err", err)
		}
		return info, nil
	}
	return s.completeRemote(ctx, t, u, sel, meta, total, opts.IfNoneMatch, ifMatch)
}

// completeRemote completes the remote upload with the selected parts
// (uploading a staged last part first) and applies the metadata known
// only now.
func (s *Store) completeRemote(ctx context.Context, t *target, u *upload, sel []*part, meta storage.ObjectMeta, total int64, ifNoneMatch bool, ifMatch string) (storage.ObjectInfo, error) {
	rid := s.remoteUploadID(u)
	parts := make([]s3client.CompletedPart, len(sel))
	for i, p := range sel {
		etag := p.remoteETag
		if p.file != "" { // the last part
			var sum []byte
			if p.md5 != "" {
				sum, _ = hex.DecodeString(p.md5)
			}
			r := s.newStagedReader(ctx, u.id, []*part{p})
			err := s.call(ctx, t, func(c *s3client.Client) error {
				var err error
				etag, err = c.UploadPartWith(ctx, t.b.Remote, u.key, rid, p.info.PartNumber, r, p.info.Size, s3client.PartOptions{ContentMD5: sum})
				return err
			})
			r.Close()
			if err != nil {
				err = s.remoteErr("UploadPart", t, u.key, objectOp, err)
				if errors.Is(err, errRemoteUploadGone) {
					s.dropVanishedLocked(ctx, u, rid)
				}
				return storage.ObjectInfo{}, err
			}
		}
		parts[i] = s3client.CompletedPart{Number: p.info.PartNumber, ETag: etag}
	}
	nullID := overlayID{t.bucket, u.key, storage.NullVersionID}
	var hidden *overlay
	if t.nullWrites() {
		// See putObject.
		hidden = s.hideOverlay(nullID)
	}
	// The completion is sent once (see putObject): a retry of a
	// completion the service applied before failing would answer
	// NoSuchUpload. An unknown outcome (a 5xx answer, a transport error,
	// NoSuchUpload) is resolved by reading the key back.
	var res s3client.WriteResult
	err := s.call(ctx, t, func(c *s3client.Client) error {
		var err error
		res, err = c.Complete(ctx, t.b.Remote, u.key, rid, parts,
			s3client.CompleteOptions{IfNoneMatch: ifNoneMatch, IfMatch: ifMatch, NoRetry: true})
		return err
	})
	if err != nil && (ambiguous(err) || s3client.IsCode(err, "NoSuchUpload")) {
		if got, ok := s.landed(ctx, t, u.key, completedETag(parts)); ok {
			res, err = got, nil
		}
	}
	if err != nil {
		s.restoreOverlay(nullID, hidden)
		// NoSuchUpload: aborted or expired remotely (or completed and
		// replaced since). Either way the gos3 upload cannot be
		// completed (again).
		err = s.remoteErr("CompleteMultipartUpload", t, u.key, objectOp, err)
		if errors.Is(err, errRemoteUploadGone) {
			s.dropVanishedLocked(ctx, u, rid)
		}
		return storage.ObjectInfo{}, err
	}
	s.forgetOverlay(ctx, nullID, hidden)
	// The remote upload is consumed: the gos3 upload is gone whatever
	// happens to the metadata below.
	if err := s.dropUpload(ctx, u, true); err != nil {
		s.log.Warn("native: drop completed upload state", "backing", s.name, "upload", u.id, "err", err)
	}
	vid := versionOf(res.VersionID)
	switch {
	case !t.versioned() && vid != storage.NullVersionID:
		s.drift(t.bucket, "the remote bucket is versioned")
	case t.b.Versioning == storage.VersioningEnabled && vid == storage.NullVersionID:
		s.drift(t.bucket, "versioning is not enabled on the remote bucket")
	}
	info := storage.ObjectInfo{
		ObjectMeta:   meta,
		Bucket:       t.bucket,
		Key:          u.key,
		VersionID:    vid,
		IsLatest:     true,
		Size:         total,
		LastModified: truncTime(time.Now()),
	}
	v, err := s.stat(ctx, t, u.key, vid, statQuick)
	if err == nil && (res.VersionID == "" || res.VersionID == storage.NullVersionID) && v.attrs.ETag != res.ETag {
		// Replaced by someone else since: their object, their metadata.
		s.log.Info("native: completed object replaced before its metadata was stored", "backing", s.name, "bucket", t.bucket, "key", u.key)
		return info, nil
	}
	if err == nil {
		info.LastModified = v.info.LastModified
		info.ObjectMeta, err = s.applyMeta(ctx, t, u.key, v, cloneMeta(meta), true)
	}
	if err != nil {
		// The object is published and the upload consumed: the client
		// cannot retry the completion.
		s.log.Error("native: store the metadata of a completed upload", "backing", s.name, "bucket", t.bucket, "key", u.key, "err", err)
		s.drift(t.bucket, "the metadata of a completed multipart upload could not be stored: "+err.Error())
		return storage.ObjectInfo{}, err
	}
	return info, nil
}

// AbortMultipartUpload aborts the remote upload (if one was created) and
// drops the upload. If the remote abort or the deletion of the upload's
// records fails, the upload is kept so it can be aborted again (a remote
// upload that is already gone counts as aborted).
func (s *Store) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	u, err := s.findUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return err
	}
	u.op.Lock()
	defer u.op.Unlock()
	if s.isGone(u) {
		return fmt.Errorf("%w: %q", storage.ErrUploadNotFound, uploadID)
	}
	if rid := s.remoteUploadID(u); rid != "" {
		t, err := s.uploadTarget(ctx, u)
		if err != nil {
			return err
		}
		err = s.call(ctx, t, func(c *s3client.Client) error {
			return c.AbortMultipartUpload(ctx, t.b.Remote, key, rid)
		})
		if err != nil && !s3client.IsCode(err, "NoSuchUpload") {
			return s.remoteErr("AbortMultipartUpload", t, key, objectOp, err)
		}
	}
	return s.dropUpload(ctx, u, false)
}

// stagedReader is an io.ReaderAt over the concatenation of staged parts.
// Each part is opened on first use and read sequentially where possible
// (a read at another offset seeks); a part whose read fails is reopened
// by the next read, so retried requests can re-read it.
type stagedReader struct {
	ctx   context.Context
	s     *Store
	id    string
	parts []*stagedFile
	ends  []int64 // cumulative end offsets
}

type stagedFile struct {
	p   *part
	mu  sync.Mutex
	rc  io.ReadSeekCloser // nil until opened
	pos int64             // offset of rc
}

func (s *Store) newStagedReader(ctx context.Context, id string, sel []*part) *stagedReader {
	m := &stagedReader{ctx: ctx, s: s, id: id}
	var end int64
	for _, p := range sel {
		end += p.info.Size
		m.parts = append(m.parts, &stagedFile{p: p})
		m.ends = append(m.ends, end)
	}
	return m
}

func (m *stagedReader) ReadAt(p []byte, off int64) (int, error) {
	total := 0
	for len(p) > 0 {
		i := sort.Search(len(m.ends), func(i int) bool { return m.ends[i] > off })
		if i == len(m.ends) {
			return total, io.EOF
		}
		start := int64(0)
		if i > 0 {
			start = m.ends[i-1]
		}
		chunk := p[:min(int64(len(p)), m.ends[i]-off)]
		n, err := m.read(m.parts[i], chunk, off-start)
		total += n
		off += int64(n)
		p = p[n:]
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// read fills chunk from offset off of staged part f.
func (m *stagedReader) read(f *stagedFile, chunk []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rc == nil {
		rc, err := m.s.state.OpenStaged(m.ctx, f.p.file, f.p.info.Size)
		if err != nil {
			return 0, fmt.Errorf("native %q: open staged part %d of upload %q: %w", m.s.name, f.p.info.PartNumber, m.id, err)
		}
		f.rc, f.pos = rc, 0
	}
	if f.pos != off {
		if _, err := f.rc.Seek(off, io.SeekStart); err != nil {
			f.reset()
			return 0, fmt.Errorf("native %q: read staged part %d of upload %q: %w", m.s.name, f.p.info.PartNumber, m.id, err)
		}
		f.pos = off
	}
	n, err := io.ReadFull(f.rc, chunk)
	f.pos += int64(n)
	if err != nil {
		f.reset()
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF // the staged part is shorter than recorded
		}
		return n, fmt.Errorf("native %q: read staged part %d of upload %q: %w", m.s.name, f.p.info.PartNumber, m.id, err)
	}
	return n, nil
}

// reset closes the part so the next read reopens it (f.mu held).
func (f *stagedFile) reset() {
	_ = f.rc.Close()
	f.rc = nil
}

// Close closes the parts that were opened.
func (m *stagedReader) Close() {
	for _, f := range m.parts {
		f.mu.Lock()
		if f.rc != nil {
			f.reset()
		}
		f.mu.Unlock()
	}
}
