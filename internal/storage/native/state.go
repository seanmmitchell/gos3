package native

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// State records
//
// The StateStore holds three kinds of records:
//
//	u/<upload ID>                          -> JSON uploadRecord
//	p/<upload ID>/<part:05d>               -> JSON partRecord
//	m/<bucket>/<b64(key)>/<b64(version)>/  -> JSON overlay (overlay.go)
//
// b64 is unpadded base64url, whose alphabet has no '/': the trailing '/'
// of an overlay key terminates it, so every key is the prefix of its own
// record only (DeleteRecords works by prefix), "m/<bucket>/" is the prefix
// of one bucket's overlays and "m/<bucket>/<b64(key)>/" of one key's.
// Upload IDs have a fixed length and contain no '/', so "u/<id>" and
// "p/<id>/" are prefixes of that upload's records only. Staged blobs are
// named by their part records. Every record is loaded at Open; the
// in-memory state is authoritative while the store is open and every
// change is written through.
//
// The upload and part records are those of the gateway backing of earlier
// development builds, so its state, kept in the same aux
// namespace, carries over: records without RemoteBucket belong to the
// remote bucket the gos3 bucket resolves to.

// Record key prefixes.
const (
	recUpload  = "u/"
	recPart    = "p/"
	recOverlay = "m/"
)

func uploadKey(id string) string { return recUpload + id }

// partsPrefix is the prefix of the part records of upload id.
func partsPrefix(id string) string { return recPart + id + "/" }

func partKey(id string, n int) string { return fmt.Sprintf("%s%05d", partsPrefix(id), n) }

// b64 encodes a key or version ID for a record key.
func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// overlaysPrefix is the prefix of the overlay records of bucket.
func overlaysPrefix(bucket string) string { return recOverlay + bucket + "/" }

// overlayKey is the record key of the overlay of one object version.
func overlayKey(id overlayID) string {
	return overlaysPrefix(id.bucket) + b64(id.key) + "/" + b64(id.vid) + "/"
}

// parseOverlayKey reverses overlayKey.
func parseOverlayKey(k string) (overlayID, bool) {
	rest, ok := strings.CutPrefix(k, recOverlay)
	if !ok {
		return overlayID{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[0] == "" || parts[3] != "" {
		return overlayID{}, false
	}
	key, err1 := base64.RawURLEncoding.DecodeString(parts[1])
	vid, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(key) == 0 || len(vid) == 0 {
		return overlayID{}, false
	}
	return overlayID{bucket: parts[0], key: string(key), vid: string(vid)}, true
}

type uploadRecord struct {
	ID        string             `json:"id"`
	Bucket    string             `json:"bucket"`
	Key       string             `json:"key"`
	Initiated time.Time          `json:"initiated"`
	Meta      storage.ObjectMeta `json:"meta"`
	Initiator string             `json:"initiator,omitempty"`
	RemoteID  string             `json:"remoteId,omitempty"`
	// RemoteBucket and Region locate the remote multipart upload (empty in
	// records of the former gateway backing: the bucket's binding).
	RemoteBucket string `json:"remoteBucket,omitempty"`
	Region       string `json:"region,omitempty"`
}

type partRecord struct {
	Number       int               `json:"n"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	Checksums    map[string]string `json:"checksums,omitempty"`
	LastModified time.Time         `json:"lastModified"`
	RemoteETag   string            `json:"remoteETag,omitempty"`
	File         string            `json:"file,omitempty"`
	// MD5 is the hex MD5 of a staged part, sent as Content-MD5 when it is
	// uploaded at completion.
	MD5 string `json:"md5,omitempty"`
}

// cleanupTimeout bounds the best-effort removal of stale state at Open.
const cleanupTimeout = 30 * time.Second

// loadState loads every upload and overlay from the state store. Part
// records without an upload, part records whose staged blob is missing and
// staged blobs no record refers to are removed, best effort: only a
// failure to read the records fails.
func (s *Store) loadState() error {
	recs, err := s.state.Records(context.Background())
	if err != nil {
		return fmt.Errorf("native %q: load state: %w", s.name, err)
	}
	type partRec struct {
		id  string
		key string
		r   partRecord
	}
	var parts []partRec
	for _, k := range slices.Sorted(maps.Keys(recs)) {
		v := recs[k]
		switch {
		case strings.HasPrefix(k, recUpload):
			var r uploadRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("native %q: load state: record %q: %w", s.name, k, err)
			}
			s.addUploadLocked(&upload{
				id: r.ID, bucket: r.Bucket, key: r.Key, initiated: r.Initiated,
				meta: r.Meta, initiator: r.Initiator, remoteID: r.RemoteID,
				remoteBucket: r.RemoteBucket, region: r.Region,
				remoteSaved: r.RemoteID != "", parts: map[int]*part{},
			})
		case strings.HasPrefix(k, recPart):
			rest := k[len(recPart):]
			i := strings.LastIndexByte(rest, '/')
			if i < 0 {
				s.log.Warn("native: ignoring malformed state record", "backing", s.name, "key", k)
				continue
			}
			if _, err := strconv.Atoi(rest[i+1:]); err != nil {
				s.log.Warn("native: ignoring malformed state record", "backing", s.name, "key", k)
				continue
			}
			var r partRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("native %q: load state: record %q: %w", s.name, k, err)
			}
			parts = append(parts, partRec{id: rest[:i], key: k, r: r})
		case strings.HasPrefix(k, recOverlay):
			id, ok := parseOverlayKey(k)
			var o overlay
			if ok {
				ok = json.Unmarshal(v, &o) == nil
			}
			if !ok {
				// An overlay that cannot be read is not applied: the
				// object shows its remote metadata (an ACL it granted is
				// dropped, which fails closed).
				s.log.Warn("native: ignoring malformed overlay record", "backing", s.name, "key", k)
				continue
			}
			s.setOverlayLocked(id, &o)
		default:
			s.log.Warn("native: ignoring unknown state record", "backing", s.name, "key", k)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	staged, lerr := s.state.ListStaged(ctx)
	if lerr != nil {
		s.log.Warn("native: list staged parts; stale ones are kept", "backing", s.name, "err", lerr)
	}
	exists := make(map[string]bool, len(staged))
	for _, name := range staged {
		exists[name] = true
	}
	referenced := map[string]bool{}
	var stale []string // record prefixes to delete
	for _, p := range parts {
		u := s.uploads[p.id]
		if u == nil {
			stale = append(stale, partsPrefix(p.id))
			continue
		}
		if p.r.File != "" {
			if lerr == nil && !exists[p.r.File] {
				s.log.Warn("native: staged part missing; part dropped", "backing", s.name, "upload", p.id, "part", p.r.Number)
				stale = append(stale, p.key)
				continue
			}
			referenced[p.r.File] = true
		}
		u.parts[p.r.Number] = &part{
			info: storage.PartInfo{
				PartMeta:     storage.PartMeta{ETag: p.r.ETag, Checksums: p.r.Checksums},
				PartNumber:   p.r.Number,
				Size:         p.r.Size,
				LastModified: p.r.LastModified,
			},
			remoteETag: p.r.RemoteETag,
			file:       p.r.File,
			md5:        p.r.MD5,
		}
	}
	for _, prefix := range slices.Compact(stale) {
		if err := s.state.DeleteRecords(ctx, prefix); err != nil {
			s.log.Warn("native: delete stale part records", "backing", s.name, "prefix", prefix, "err", err)
		}
	}
	// Staged blobs without a record: parts whose commit did not finish,
	// or uploads whose staged parts could not be deleted.
	for _, name := range staged {
		if referenced[name] {
			continue
		}
		if err := s.state.DeleteStaged(ctx, name); err != nil {
			s.log.Warn("native: delete stale staged part", "backing", s.name, "name", name, "err", err)
		}
	}
	return nil
}

// saveUpload writes u's record.
func (s *Store) saveUpload(ctx context.Context, u *upload) error {
	s.mu.Lock()
	r := uploadRecord{
		ID: u.id, Bucket: u.bucket, Key: u.key, Initiated: u.initiated, Meta: u.meta, Initiator: u.initiator,
		RemoteID: u.remoteID, RemoteBucket: u.remoteBucket, Region: u.region,
	}
	s.mu.Unlock()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.state.PutRecord(ctx, uploadKey(u.id), data); err != nil {
		return fmt.Errorf("native %q: save upload %q: %w", s.name, u.id, err)
	}
	return nil
}

// savePart writes a part record.
func (s *Store) savePart(ctx context.Context, id string, p *part) error {
	r := partRecord{
		Number: p.info.PartNumber, Size: p.info.Size, ETag: p.info.ETag, Checksums: p.info.Checksums,
		LastModified: p.info.LastModified, RemoteETag: p.remoteETag, File: p.file, MD5: p.md5,
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.state.PutRecord(ctx, partKey(id, p.info.PartNumber), data); err != nil {
		return fmt.Errorf("native %q: save part %d of upload %q: %w", s.name, p.info.PartNumber, id, err)
	}
	return nil
}

// deleteUploadState removes an upload's records, the upload record first:
// once it is gone the upload cannot come back, and part records left by a
// failure in between are removed by the next Open.
func (s *Store) deleteUploadState(ctx context.Context, id string) error {
	if err := s.state.DeleteRecords(ctx, uploadKey(id)); err != nil {
		return fmt.Errorf("native %q: delete upload %q: %w", s.name, id, err)
	}
	if err := s.state.DeleteRecords(ctx, partsPrefix(id)); err != nil {
		return fmt.Errorf("native %q: delete upload %q: %w", s.name, id, err)
	}
	return nil
}

// deleteStaged removes a staged blob that no record refers to any more.
// A failure is logged and the deletion retried by the next cleanupStaged
// (after the next upload is dropped or a part replaced, and at Close); Open
// removes blobs no record refers to as well.
func (s *Store) deleteStaged(ctx context.Context, name string) {
	if err := s.state.DeleteStaged(ctx, name); err != nil {
		s.log.Warn("native: delete staged part; retried later", "backing", s.name, "name", name, "err", err)
		s.mu.Lock()
		s.pendingStaged[name] = true
		s.mu.Unlock()
	}
}

// cleanupStaged retries the deletion of staged blobs whose deletion failed
// before. Staged blob names are unique per part commit, so a pending name
// is never referenced again.
func (s *Store) cleanupStaged(ctx context.Context) {
	s.mu.Lock()
	if len(s.pendingStaged) == 0 {
		s.mu.Unlock()
		return
	}
	names := slices.Sorted(maps.Keys(s.pendingStaged))
	s.mu.Unlock()
	for _, name := range names {
		err := s.state.DeleteStaged(ctx, name)
		if err != nil {
			s.log.Debug("native: delete staged part (retry)", "backing", s.name, "name", name, "err", err)
			continue
		}
		s.mu.Lock()
		delete(s.pendingStaged, name)
		s.mu.Unlock()
	}
}

// --- in-memory state ---

// memoryState is the StateStore returned by MemoryState.
type memoryState struct {
	mu      sync.Mutex
	records map[string][]byte
	staged  map[string][]byte
}

// MemoryState returns a StateStore that keeps everything in memory: the
// state survives closing and reopening a Store with the same StateStore,
// but not the process. It serves tests and as a reference implementation.
func MemoryState() StateStore {
	return &memoryState{records: map[string][]byte{}, staged: map[string][]byte{}}
}

func (m *memoryState) Records(ctx context.Context) (map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]byte, len(m.records))
	for k, v := range m.records {
		out[k] = bytes.Clone(v)
	}
	return out, nil
}

func (m *memoryState) PutRecord(ctx context.Context, key string, val []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[key] = bytes.Clone(val)
	return nil
}

func (m *memoryState) DeleteRecords(ctx context.Context, prefix string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.records, func(k string, _ []byte) bool { return strings.HasPrefix(k, prefix) })
	return nil
}

func (m *memoryState) PutStaged(ctx context.Context, name string, r io.ReaderAt, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(r, 0, size), data); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.staged[name] = data
	return nil
}

func (m *memoryState) OpenStaged(ctx context.Context, name string, size int64) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	data, ok := m.staged[name]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("native: staged part %q: %w", name, fs.ErrNotExist)
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("native: staged part %q has %d bytes, want %d", name, len(data), size)
	}
	return nopCloser{bytes.NewReader(data)}, nil
}

func (m *memoryState) DeleteStaged(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.staged, name)
	return nil
}

func (m *memoryState) ListStaged(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.staged)), nil
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
