package gateway

import (
	"bytes"
	"context"
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

// StateStore keeps the store's multipart upload state outside the process
// (in the gateway server, in the metadata bucket of the remote service; see
// s3store's AuxState): small records and staged part blobs.
//
// Records are keyed
//
//	u/<upload ID>              -> JSON uploadRecord
//	p/<upload ID>/<part:05d>   -> JSON partRecord
//
// and staged blobs are named by their part records. Every record is loaded
// at Open; the in-memory state is authoritative while the store is open
// and every change is written through. Upload IDs have a fixed length and
// contain no '/', so "u/<id>" and "p/<id>/" are prefixes of that upload's
// records only.
//
// All methods must be safe for concurrent use.
type StateStore interface {
	// Records returns every record (key -> value). It is called by Open.
	Records(ctx context.Context) (map[string][]byte, error)
	// PutRecord creates or replaces a record. It must be durable when it
	// returns nil; an error leaves the outcome unknown (the write may
	// have landed), and the store copes with either.
	PutRecord(ctx context.Context, key string, val []byte) error
	// DeleteRecords deletes every record whose key starts with prefix
	// (none is not an error).
	DeleteRecords(ctx context.Context, prefix string) error
	// PutStaged stores the size bytes of r as the staged blob name. It
	// must be durable when it returns nil.
	PutStaged(ctx context.Context, name string, r io.ReaderAt, size int64) error
	// OpenStaged opens a staged blob of the given size for reading.
	OpenStaged(ctx context.Context, name string, size int64) (io.ReadSeekCloser, error)
	// DeleteStaged deletes a staged blob (a missing one is not an error).
	DeleteStaged(ctx context.Context, name string) error
	// ListStaged returns the names of every staged blob, for the removal
	// of blobs no record refers to at Open.
	ListStaged(ctx context.Context) ([]string, error)
}

// Record key prefixes.
const (
	recUpload = "u/"
	recPart   = "p/"
)

func uploadKey(id string) string { return recUpload + id }

// partsPrefix is the prefix of the part records of upload id.
func partsPrefix(id string) string { return recPart + id + "/" }

func partKey(id string, n int) string { return fmt.Sprintf("%s%05d", partsPrefix(id), n) }

type uploadRecord struct {
	ID        string             `json:"id"`
	Bucket    string             `json:"bucket"`
	Key       string             `json:"key"`
	Initiated time.Time          `json:"initiated"`
	Meta      storage.ObjectMeta `json:"meta"`
	Initiator string             `json:"initiator,omitempty"`
	RemoteID  string             `json:"remoteId,omitempty"`
}

type partRecord struct {
	Number       int               `json:"n"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	Checksums    map[string]string `json:"checksums,omitempty"`
	LastModified time.Time         `json:"lastModified"`
	RemoteETag   string            `json:"remoteETag,omitempty"`
	File         string            `json:"file,omitempty"`
}

// cleanupTimeout bounds the best-effort removal of stale state at Open.
const cleanupTimeout = 30 * time.Second

// loadState loads every upload from the state store. Part records without
// an upload, part records whose staged blob is missing and staged blobs no
// record refers to are removed, best effort: only a failure to read the
// records fails.
func (s *Store) loadState() error {
	recs, err := s.state.Records(context.Background())
	if err != nil {
		return fmt.Errorf("gateway %q: load upload state: %w", s.name, err)
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
				return fmt.Errorf("gateway %q: load upload state: record %q: %w", s.name, k, err)
			}
			s.addUploadLocked(&upload{
				id: r.ID, bucket: r.Bucket, key: r.Key, initiated: r.Initiated,
				meta: r.Meta, initiator: r.Initiator, remoteID: r.RemoteID,
				remoteSaved: r.RemoteID != "", parts: map[int]*part{},
			})
		case strings.HasPrefix(k, recPart):
			rest := k[len(recPart):]
			i := strings.LastIndexByte(rest, '/')
			if i < 0 {
				s.log.Warn("gateway: ignoring malformed state record", "backing", s.name, "key", k)
				continue
			}
			if _, err := strconv.Atoi(rest[i+1:]); err != nil {
				s.log.Warn("gateway: ignoring malformed state record", "backing", s.name, "key", k)
				continue
			}
			var r partRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("gateway %q: load upload state: record %q: %w", s.name, k, err)
			}
			parts = append(parts, partRec{id: rest[:i], key: k, r: r})
		default:
			s.log.Warn("gateway: ignoring unknown state record", "backing", s.name, "key", k)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	staged, lerr := s.state.ListStaged(ctx)
	if lerr != nil {
		s.log.Warn("gateway: list staged parts; stale ones are kept", "backing", s.name, "err", lerr)
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
				s.log.Warn("gateway: staged part missing; part dropped", "backing", s.name, "upload", p.id, "part", p.r.Number)
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
		}
	}
	for _, prefix := range slices.Compact(stale) {
		if err := s.state.DeleteRecords(ctx, prefix); err != nil {
			s.log.Warn("gateway: delete stale part records", "backing", s.name, "prefix", prefix, "err", err)
		}
	}
	// Staged blobs without a record: parts whose commit did not finish,
	// or uploads whose staged parts could not be deleted.
	for _, name := range staged {
		if referenced[name] {
			continue
		}
		if err := s.state.DeleteStaged(ctx, name); err != nil {
			s.log.Warn("gateway: delete stale staged part", "backing", s.name, "name", name, "err", err)
		}
	}
	return nil
}

// saveUpload writes u's record.
func (s *Store) saveUpload(ctx context.Context, u *upload) error {
	s.mu.Lock()
	r := uploadRecord{ID: u.id, Bucket: u.bucket, Key: u.key, Initiated: u.initiated, Meta: u.meta, Initiator: u.initiator, RemoteID: u.remoteID}
	s.mu.Unlock()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.state.PutRecord(ctx, uploadKey(u.id), data); err != nil {
		return fmt.Errorf("gateway %q: save upload %q: %w", s.name, u.id, err)
	}
	return nil
}

// savePart writes a part record.
func (s *Store) savePart(ctx context.Context, id string, p *part) error {
	r := partRecord{
		Number: p.info.PartNumber, Size: p.info.Size, ETag: p.info.ETag, Checksums: p.info.Checksums,
		LastModified: p.info.LastModified, RemoteETag: p.remoteETag, File: p.file,
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.state.PutRecord(ctx, partKey(id, p.info.PartNumber), data); err != nil {
		return fmt.Errorf("gateway %q: save part %d of upload %q: %w", s.name, p.info.PartNumber, id, err)
	}
	return nil
}

// deleteUploadState removes an upload's records, the upload record first:
// once it is gone the upload cannot come back, and part records left by a
// failure in between are removed by the next Open.
func (s *Store) deleteUploadState(ctx context.Context, id string) error {
	if err := s.state.DeleteRecords(ctx, uploadKey(id)); err != nil {
		return fmt.Errorf("gateway %q: delete upload %q: %w", s.name, id, err)
	}
	if err := s.state.DeleteRecords(ctx, partsPrefix(id)); err != nil {
		return fmt.Errorf("gateway %q: delete upload %q: %w", s.name, id, err)
	}
	return nil
}

// deleteStaged removes a staged blob, best effort (Open removes blobs no
// record refers to).
func (s *Store) deleteStaged(ctx context.Context, name string) {
	if err := s.state.DeleteStaged(ctx, name); err != nil {
		s.log.Warn("gateway: delete staged part", "backing", s.name, "name", name, "err", err)
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
		return nil, fmt.Errorf("gateway: staged part %q: %w", name, fs.ErrNotExist)
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("gateway: staged part %q has %d bytes, want %d", name, len(data), size)
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
