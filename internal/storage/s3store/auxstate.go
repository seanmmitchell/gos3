package s3store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// AuxStore is the state a gateway backing keeps in its metadata backing
// (Options.Gateway): journaled key/value records and staged part blobs
// under gateway/parts/. internal/storage/gateway declares the identical
// interface.
type AuxStore interface {
	// Records returns every record.
	Records(ctx context.Context) (map[string][]byte, error)
	// PutRecord stores (replaces) one record durably.
	PutRecord(ctx context.Context, key string, val []byte) error
	// DeleteRecords removes every record whose key has the prefix.
	DeleteRecords(ctx context.Context, prefix string) error
	// PutStaged stores r[0:size] as the staged blob name.
	PutStaged(ctx context.Context, name string, r io.ReaderAt, size int64) error
	// OpenStaged opens the staged blob name of the given size.
	OpenStaged(ctx context.Context, name string, size int64) (io.ReadSeekCloser, error)
	// DeleteStaged removes a staged blob (no error if it does not exist).
	DeleteStaged(ctx context.Context, name string) error
	// ListStaged returns the names of the staged blobs, sorted.
	ListStaged(ctx context.Context) ([]string, error)
}

// auxNS is one aux namespace of the state.
type auxNS struct {
	records map[string][]byte
	staged  map[string]bool
}

// auxNS returns namespace ns, creating it.
func (st *state) auxNS(ns string) *auxNS {
	a := st.aux[ns]
	if a == nil {
		a = &auxNS{records: map[string][]byte{}, staged: map[string]bool{}}
		st.aux[ns] = a
	}
	return a
}

// AuxState returns the aux store of namespace ns. It is available on a
// gateway backing only (Options.Gateway); otherwise its methods answer
// storage.ErrNotSupported.
func (b *Backend) AuxState(ns string) AuxStore { return &auxStore{b: b, ns: ns} }

type auxStore struct {
	b  *Backend
	ns string
}

// check validates the store and returns the current state.
func (s *auxStore) check(ctx context.Context) (*state, error) {
	if !s.b.opts.Gateway {
		return nil, fmt.Errorf("%w: aux state needs a gateway backing", storage.ErrNotSupported)
	}
	if s.ns == "" || strings.Contains(s.ns, "/") {
		return nil, fmt.Errorf("%w: aux namespace %q", storage.ErrInvalidName, s.ns)
	}
	return s.b.current(ctx)
}

// lock returns the state with its aux lock held.
func (s *auxStore) lock(ctx context.Context) (*state, error) {
	st, err := s.check(ctx)
	if err != nil {
		return nil, err
	}
	st.auxMu.Lock()
	if st.stale {
		st.auxMu.Unlock()
		return nil, s.b.unavailableErr()
	}
	return st, nil
}

// append journals rec (aux lock held).
func (s *auxStore) append(st *state, rec *record, onDurable func()) <-chan error {
	rec.NS = s.ns
	lsn, done := s.b.appendRecord(rec, onDurable)
	if lsn > st.auxLSN {
		st.auxLSN = lsn
	}
	return done
}

func (s *auxStore) Records(ctx context.Context) (map[string][]byte, error) {
	st, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte)
	if a := st.aux[s.ns]; a != nil {
		for k, v := range a.records {
			out[k] = bytes.Clone(v)
		}
	}
	lsn := st.auxLSN
	st.auxMu.Unlock()
	return out, s.b.waitRead(ctx, lsn)
}

func (s *auxStore) PutRecord(ctx context.Context, key string, val []byte) error {
	if key == "" {
		return fmt.Errorf("%w: empty aux record key", storage.ErrInvalidName)
	}
	st, err := s.lock(ctx)
	if err != nil {
		return err
	}
	v := append([]byte{}, val...)
	st.auxNS(s.ns).records[key] = v
	done := s.append(st, &record{Op: opAuxPut, K: key, D: v}, nil)
	st.auxMu.Unlock()
	return await(ctx, done)
}

func (s *auxStore) DeleteRecords(ctx context.Context, prefix string) error {
	st, err := s.lock(ctx)
	if err != nil {
		return err
	}
	a := st.auxNS(s.ns)
	n := len(a.records)
	maps.DeleteFunc(a.records, func(k string, _ []byte) bool { return strings.HasPrefix(k, prefix) })
	if len(a.records) == n {
		lsn := st.auxLSN
		st.auxMu.Unlock()
		return s.b.waitRead(ctx, lsn)
	}
	done := s.append(st, &record{Op: opAuxDel, K: prefix}, nil)
	st.auxMu.Unlock()
	return await(ctx, done)
}

func validStagedName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("%w: staged blob name %q", storage.ErrInvalidName, name)
	}
	return nil
}

func (s *auxStore) PutStaged(ctx context.Context, name string, r io.ReaderAt, size int64) error {
	if err := validStagedName(name); err != nil {
		return err
	}
	if _, err := s.check(ctx); err != nil {
		return err
	}
	b := s.b
	key := b.stagedKey(s.ns, name)
	b.pending.add(key)
	defer b.pending.done(key)
	if err := b.putBlob(ctx, key, r, size); err != nil {
		return err
	}
	st, err := s.lock(ctx)
	if err != nil {
		return err
	}
	st.auxNS(s.ns).staged[name] = true
	done := s.append(st, &record{Op: opStage, K: name}, nil)
	st.auxMu.Unlock()
	return await(ctx, done)
}

func (s *auxStore) OpenStaged(ctx context.Context, name string, size int64) (io.ReadSeekCloser, error) {
	st, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	a := st.aux[s.ns]
	if a == nil || !a.staged[name] {
		st.auxMu.Unlock()
		return nil, fmt.Errorf("%w: staged blob %q", storage.ErrPartNotFound, name)
	}
	key := s.b.stagedKey(s.ns, name)
	s.b.blobs.acquire(key)
	lsn := st.auxLSN
	st.auxMu.Unlock()
	if err := s.b.waitRead(ctx, lsn); err != nil {
		s.b.releaseBlob(key)
		return nil, err
	}
	return s.b.newBlobReader(ctx, key, size, true, func(err error) error {
		return fmt.Errorf("%w: staged blob %q: %w", errBlobMissing, name, err)
	}), nil
}

func (s *auxStore) DeleteStaged(ctx context.Context, name string) error {
	st, err := s.lock(ctx)
	if err != nil {
		return err
	}
	a := st.auxNS(s.ns)
	if !a.staged[name] {
		lsn := st.auxLSN
		st.auxMu.Unlock()
		return s.b.waitRead(ctx, lsn)
	}
	delete(a.staged, name)
	key := s.b.stagedKey(s.ns, name)
	done := s.append(st, &record{Op: opUnstage, K: name}, s.b.dropOnDurable([]string{key}))
	st.auxMu.Unlock()
	return await(ctx, done)
}

func (s *auxStore) ListStaged(ctx context.Context) ([]string, error) {
	st, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	if a := st.aux[s.ns]; a != nil {
		out = slices.Sorted(maps.Keys(a.staged))
	}
	lsn := st.auxLSN
	st.auxMu.Unlock()
	if out == nil {
		out = []string{}
	}
	return out, s.b.waitRead(ctx, lsn)
}
