package gateway

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
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
// A gos3 upload has its own ID and a local record (bucket, key, metadata,
// initiator, parts), persisted in StateDir. Its parts go to a remote
// multipart upload of the target key, created lazily for the first part
// that needs it, or are staged locally:
//
//   - A part of at least s3client.MinPartSize (5 MiB) is uploaded to the
//     remote upload right away, under the same part number.
//   - A smaller part is staged in a local file (StateDir/parts) and only
//     uploaded at completion. S3 rejects completions whose non-last parts
//     are below 5 MiB, and parts of a remote upload cannot be read back,
//     so small parts must stay readable until it is known how they are
//     used.
//
// CompleteMultipartUpload then takes one of two paths:
//
//   - Every listed part is staged: the parts are concatenated and uploaded
//     like a new object (single PUT up to the part size, else a fresh
//     remote multipart upload); the lazily created remote upload, if any,
//     is aborted. This covers small objects uploaded in parts and every
//     upload whose parts are all small.
//   - Every listed part but the last is remote and the part numbers are
//     ascending: a staged last part is uploaded now and the remote upload
//     is completed with the remote parts. This is always the case for
//     uploads the S3 layer accepts (it enforces ascending part numbers and
//     5 MiB non-last parts), so large uploads never pass through local
//     disk.
//
// Any other combination (a small non-last part next to remote parts, or a
// non-ascending list including remote parts) cannot be assembled from
// parts the remote no longer lets anyone read, and fails with an error
// wrapping storage.ErrNotSupported; the S3 layer never asks for it.
//
// The metadata passed to Complete (ETag, checksums, part sizes) differs
// from what the remote stored at creation time; when the resulting headers
// differ, the completed object is rewritten onto itself with a server-side
// copy (until then, briefly, HEAD shows the creation-time metadata).

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
	parts    map[int]*part
	gone     bool
}

// part is a committed part: remote (remoteETag set) or staged (file set).
type part struct {
	info       storage.PartInfo
	remoteETag string
	file       string // base name in Store.stageDir
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

func clonePartInfo(p storage.PartInfo) storage.PartInfo {
	p.Checksums = maps.Clone(p.Checksums)
	return p
}

// compareUploads orders uploads by key, Initiated, upload ID.
func compareUploads(a, b *upload) int {
	return cmp.Or(strings.Compare(a.key, b.key), a.initiated.Compare(b.initiated), strings.Compare(a.id, b.id))
}

// newUploadID returns a random, URL-safe upload ID.
func newUploadID() string {
	var b [24]byte
	_, _ = rand.Read(b[:]) // never fails (Go >= 1.24)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// addUpload registers u (s.mu held).
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
// bucket/key.
func (s *Store) findUpload(bucket, key, id string) (*upload, error) {
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

// dropUpload forgets u and its staged parts (the remote upload, if any,
// must have been completed or aborted). The caller holds u.op.
func (s *Store) dropUpload(u *upload) error {
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
	for _, f := range files {
		_ = os.Remove(filepath.Join(s.stageDir, f))
	}
	return s.deleteUploadState(u.id)
}

// CreateMultipartUpload starts an upload. The remote multipart upload is
// created with the first part that is uploaded to it.
func (s *Store) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	if err := s.checkKey(bucket, key); err != nil {
		return storage.UploadInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.UploadInfo{}, err
	}
	u := &upload{
		id:        newUploadID(),
		bucket:    bucket,
		key:       key,
		initiated: time.Now().UTC().Round(0),
		meta:      cloneMeta(meta),
		initiator: initiator,
		parts:     map[int]*part{},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return storage.UploadInfo{}, errClosed
	}
	if err := s.saveUpload(u); err != nil {
		return storage.UploadInfo{}, err
	}
	s.addUploadLocked(u)
	return u.info(), nil
}

// GetMultipartUpload returns an in-progress upload of bucket/key.
func (s *Store) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	u, err := s.findUpload(bucket, key, uploadID)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	return u.info(), nil
}

// ListParts returns the committed parts sorted by part number.
func (s *Store) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	u, err := s.findUpload(bucket, key, uploadID)
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
		return errors.New("gateway: nil walk function")
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

// info0 is info without the metadata copy (for comparisons).
func (u *upload) info0() storage.UploadInfo {
	return storage.UploadInfo{UploadID: u.id, Bucket: u.bucket, Key: u.key, Initiated: u.initiated}
}

// ensureRemoteUpload returns the ID of u's remote multipart upload,
// creating it (with the creation-time metadata) on first use.
func (s *Store) ensureRemoteUpload(ctx context.Context, u *upload) (string, error) {
	u.createMu.Lock()
	defer u.createMu.Unlock()
	if id := s.remoteUploadID(u); id != "" {
		return id, nil
	}
	rb, rk := s.remoteKey(u.bucket, u.key)
	id, err := s.c.CreateMultipartUpload(ctx, rb, rk, encodeHeaders(u.meta, headerOpts{}))
	if err != nil {
		return "", s.remoteErr("CreateMultipartUpload", u.bucket, u.key, err)
	}
	s.mu.Lock()
	u.remoteID = id
	s.mu.Unlock()
	if err := s.saveUpload(u); err != nil {
		return "", err
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
	done  bool
}

// NewPartWriter starts writing part partNumber (1..10000) of an upload.
func (s *Store) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	if partNumber < 1 || partNumber > maxPartNumber {
		return nil, fmt.Errorf("gateway: invalid part number %d", partNumber)
	}
	u, err := s.findUpload(bucket, key, uploadID)
	if err != nil {
		return nil, err
	}
	return &partWriter{s: s, u: u, number: partNumber, spool: s3client.NewSpool(s.spoolDir, 0)}, nil
}

func (w *partWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	return w.spool.Write(p)
}

// Commit uploads the part to the remote upload (>= 5 MiB) or stages it
// locally, replacing an earlier part with the same number.
func (w *partWriter) Commit(ctx context.Context, meta storage.PartMeta) (storage.PartInfo, error) {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return storage.PartInfo{}, errWriterDone
	}
	w.done = true
	w.mu.Unlock()
	defer w.spool.Close()
	if err := w.spool.Finish(); err != nil {
		return storage.PartInfo{}, err
	}
	s, u := w.s, w.u
	u.op.RLock()
	defer u.op.RUnlock()
	if s.isGone(u) {
		return storage.PartInfo{}, fmt.Errorf("%w: %q", storage.ErrUploadNotFound, u.id)
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
	if size >= s3client.MinPartSize {
		rid, err := s.ensureRemoteUpload(ctx, u)
		if err != nil {
			return storage.PartInfo{}, err
		}
		rb, rk := s.remoteKey(u.bucket, u.key)
		etag, err := s.c.UploadPart(ctx, rb, rk, rid, w.number, w.spool, size)
		if err != nil {
			return storage.PartInfo{}, s.remoteErr("UploadPart", u.bucket, u.key, err)
		}
		p.remoteETag = etag
	} else {
		file, err := s.stagePart(u.id, w.number, w.spool, size)
		if err != nil {
			return storage.PartInfo{}, err
		}
		p.file = file
	}
	if err := s.savePart(u.id, p); err != nil {
		if p.file != "" {
			_ = os.Remove(filepath.Join(s.stageDir, p.file))
		}
		return storage.PartInfo{}, err
	}
	s.mu.Lock()
	old := u.parts[w.number]
	u.parts[w.number] = p
	s.mu.Unlock()
	if old != nil && old.file != "" {
		_ = os.Remove(filepath.Join(s.stageDir, old.file))
	}
	return clonePartInfo(p.info), nil
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

// stagePart copies a small part into a staging file and returns its name.
func (s *Store) stagePart(id string, n int, r io.ReaderAt, size int64) (string, error) {
	f, err := os.CreateTemp(s.stageDir, fmt.Sprintf("%s-%05d-*", id, n))
	if err != nil {
		return "", fmt.Errorf("gateway %q: stage part: %w", s.name, err)
	}
	_, err = io.Copy(f, io.NewSectionReader(r, 0, size))
	if err == nil && s.db != nil {
		err = f.Sync() // the part record that refers to it is durable too
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("gateway %q: stage part: %w", s.name, err)
	}
	return filepath.Base(f.Name()), nil
}

// --- completion ---

// CompleteMultipartUpload assembles the listed parts (see the package
// comment above for the two paths) and commits the object with meta and
// opts like ObjectWriter.Commit. The upload is kept when the completion
// fails before the object was published.
func (s *Store) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	u, err := s.findUpload(bucket, key, uploadID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	u.op.Lock()
	defer u.op.Unlock()
	if opts.PartETags != nil && len(opts.PartETags) != len(partNumbers) {
		return storage.ObjectInfo{}, fmt.Errorf("gateway: %d part ETags for %d parts", len(opts.PartETags), len(partNumbers))
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
		return storage.ObjectInfo{}, fmt.Errorf("%w: gateway %q cannot complete upload %q: parts below %d bytes other than the last, or part numbers out of order, together with parts already uploaded to the remote",
			storage.ErrNotSupported, s.name, uploadID, s3client.MinPartSize)
	}

	unlock := s.lockObject(bucket, key)
	defer unlock()
	ifMatch := ""
	if opts.IfMatch != "" {
		cur, a, err := s.head(ctx, bucket, key, "")
		if errors.Is(err, storage.ErrObjectNotFound) {
			return storage.ObjectInfo{}, fmt.Errorf("%w: %s/%s does not exist (If-Match)", storage.ErrPreconditionFailed, bucket, key)
		}
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if !etagMatches(opts.IfMatch, cur.ETag) {
			return storage.ObjectInfo{}, fmt.Errorf("%w: ETag mismatch (If-Match)", storage.ErrPreconditionFailed)
		}
		ifMatch = a.ETag
	}
	info := storage.ObjectInfo{
		ObjectMeta: cloneMeta(meta),
		Bucket:     bucket,
		Key:        key,
		VersionID:  storage.NullVersionID,
		IsLatest:   true,
		Size:       total,
	}
	if allStaged {
		etag, err := s.completeStaged(ctx, u, sel, total, meta, opts.IfNoneMatch, ifMatch)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if remoteID != "" {
			rb, rk := s.remoteKey(bucket, key)
			if err := s.c.AbortMultipartUpload(context.WithoutCancel(ctx), rb, rk, remoteID); err != nil {
				s.log.Warn("gateway: abort unused remote multipart upload", "backing", s.name, "bucket", bucket, "key", key, "err", err)
			}
		}
		if err := s.dropUpload(u); err != nil {
			s.log.Warn("gateway: drop completed upload state", "backing", s.name, "upload", uploadID, "err", err)
		}
		info.LastModified = s.lastModifiedAfterWrite(ctx, bucket, key, etag)
		return info, nil
	}
	return s.completeRemote(ctx, u, sel, info, opts.IfNoneMatch, ifMatch)
}

// completeStaged uploads the concatenation of staged parts as the object.
func (s *Store) completeStaged(ctx context.Context, u *upload, sel []*part, total int64, meta storage.ObjectMeta, ifNoneMatch bool, ifMatch string) (string, error) {
	mr := &multiFileReaderAt{}
	defer mr.Close()
	for _, p := range sel {
		f, err := os.Open(filepath.Join(s.stageDir, p.file))
		if err != nil {
			return "", fmt.Errorf("gateway %q: open staged part %d of upload %q: %w", s.name, p.info.PartNumber, u.id, err)
		}
		mr.add(f, p.info.Size)
	}
	rb, rk := s.remoteKey(u.bucket, u.key)
	// A multipart ETag never equals the MD5 the remote computes.
	h := encodeHeaders(meta, headerOpts{etag: true})
	etag, _, err := s.c.Upload(ctx, rb, rk, mr, total, s3client.UploadOptions{
		PutOptions: s3client.PutOptions{Header: h, IfNoneMatch: ifNoneMatch, IfMatch: ifMatch},
		PartSize:   s.partSize,
	})
	if err != nil {
		return "", s.remoteErr("CompleteMultipartUpload", u.bucket, u.key, err)
	}
	return etag, nil
}

// completeRemote completes the remote upload with the selected parts
// (uploading a staged last part first) and fixes up the metadata.
func (s *Store) completeRemote(ctx context.Context, u *upload, sel []*part, info storage.ObjectInfo, ifNoneMatch bool, ifMatch string) (storage.ObjectInfo, error) {
	rb, rk := s.remoteKey(u.bucket, u.key)
	rid := s.remoteUploadID(u)
	parts := make([]s3client.CompletedPart, len(sel))
	for i, p := range sel {
		etag := p.remoteETag
		if p.file != "" { // the last part
			f, err := os.Open(filepath.Join(s.stageDir, p.file))
			if err != nil {
				return storage.ObjectInfo{}, fmt.Errorf("gateway %q: open staged part %d of upload %q: %w", s.name, p.info.PartNumber, u.id, err)
			}
			etag, err = s.c.UploadPart(ctx, rb, rk, rid, p.info.PartNumber, f, p.info.Size)
			_ = f.Close()
			if err != nil {
				return storage.ObjectInfo{}, s.remoteErr("UploadPart", u.bucket, u.key, err)
			}
		}
		parts[i] = s3client.CompletedPart{Number: p.info.PartNumber, ETag: etag}
	}
	etag, err := s.c.CompleteMultipartUpload(ctx, rb, rk, rid, parts, s3client.CompleteOptions{IfNoneMatch: ifNoneMatch, IfMatch: ifMatch})
	if err != nil {
		return storage.ObjectInfo{}, s.remoteErr("CompleteMultipartUpload", u.bucket, u.key, err)
	}
	// The remote upload is consumed: the gos3 upload is gone whatever
	// happens to the metadata fix-up below.
	if err := s.dropUpload(u); err != nil {
		s.log.Warn("gateway: drop completed upload state", "backing", s.name, "upload", u.id, "err", err)
	}
	created := encodeHeaders(u.meta, headerOpts{})
	final := encodeHeaders(info.ObjectMeta, headerOpts{etag: info.ETag != etag})
	if !sameHeaders(created, final) {
		// A server-side copy may change the remote ETag: keep the gos3 one.
		final = encodeHeaders(info.ObjectMeta, headerOpts{etag: true})
		_, err := s.c.Copy(ctx, rb, rk, rk, info.Size, s3client.CopyOptions{ReplaceMetadata: true, Header: final, IfMatch: etag})
		switch {
		case s3client.IsPreconditionFailed(err):
			// Replaced by someone else since: their object, their metadata.
			s.log.Info("gateway: completed object replaced before its metadata was stored", "backing", s.name, "bucket", u.bucket, "key", u.key)
		case err != nil:
			// The object is published and the upload consumed: failing
			// now would make the client retry a completion that cannot
			// succeed. The object keeps its creation-time metadata.
			s.log.Warn("gateway: store the metadata of a completed upload", "backing", s.name, "bucket", u.bucket, "key", u.key, "err", err)
		}
		etag = ""
	}
	info.LastModified = s.lastModifiedAfterWrite(ctx, u.bucket, u.key, etag)
	return info, nil
}

// AbortMultipartUpload aborts the remote upload (if one was created) and
// drops the upload. If the remote abort fails, the upload is kept so it
// can be aborted again.
func (s *Store) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	u, err := s.findUpload(bucket, key, uploadID)
	if err != nil {
		return err
	}
	u.op.Lock()
	defer u.op.Unlock()
	if s.isGone(u) {
		return fmt.Errorf("%w: %q", storage.ErrUploadNotFound, uploadID)
	}
	if rid := s.remoteUploadID(u); rid != "" {
		rb, rk := s.remoteKey(bucket, key)
		if err := s.c.AbortMultipartUpload(ctx, rb, rk, rid); err != nil {
			return s.remoteErr("AbortMultipartUpload", bucket, key, err)
		}
	}
	return s.dropUpload(u)
}

// multiFileReaderAt is an io.ReaderAt over the concatenation of files.
type multiFileReaderAt struct {
	files []*os.File
	ends  []int64 // cumulative end offsets
}

func (m *multiFileReaderAt) add(f *os.File, size int64) {
	end := size
	if n := len(m.ends); n > 0 {
		end += m.ends[n-1]
	}
	m.files = append(m.files, f)
	m.ends = append(m.ends, end)
}

func (m *multiFileReaderAt) ReadAt(p []byte, off int64) (int, error) {
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
		n, err := m.files[i].ReadAt(chunk, off-start)
		total += n
		off += int64(n)
		p = p[n:]
		if n < len(chunk) {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF // the staged file shrank
			}
			return total, err
		}
	}
	return total, nil
}

func (m *multiFileReaderAt) Close() {
	for _, f := range m.files {
		_ = f.Close()
	}
}
