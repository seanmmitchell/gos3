package copyjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
)

// store is a job's state beyond its checkpoint: the fingerprint records of
// fingerprint mode (copier.FingerprintStore, by source key) and the set of
// target keys the job wrote ("owned"), which it may delete when the source
// no longer has them. Methods are safe for concurrent use.
type store interface {
	copier.FingerprintStore
	// Owned reports whether the job wrote target key key.
	Owned(key string) (bool, error)
	// SetOwned records whether the job owns target key key.
	SetOwned(key string, owned bool) error
	// Flush makes the changes so far durable. The run flushes before it
	// saves a checkpoint, so that a checkpoint never gets ahead of the
	// state it relies on.
	Flush() error
	// Close flushes and releases the store.
	Close() error
}

// stateFile is the name of a job's database in its state directory.
const stateFile = "state.db"

// maxPending is the number of buffered changes after which a boltStore
// flushes without waiting for the next checkpoint.
const maxPending = 10_000

// bbolt buckets of a job's database.
var (
	fpBucket    = []byte("fingerprints")
	ownedBucket = []byte("owned")
)

// openStore opens the job's database in its state directory, or, for jobs
// kept in memory (jobs.ErrNoStateDir), the job's in-memory store, which
// outlives the run.
func (s *Service) openStore(h *jobs.Handle, id string) (store, error) {
	dir, err := h.StateDir()
	if errors.Is(err, jobs.ErrNoStateDir) {
		s.mu.Lock()
		defer s.mu.Unlock()
		ms := s.memStores[id]
		if ms == nil {
			ms = &memStore{MemStore: copier.NewMemStore(), owned: map[string]bool{}}
			s.memStores[id] = ms
		}
		return ms, nil
	}
	if err != nil {
		return nil, err
	}
	return openBolt(filepath.Join(dir, stateFile))
}

// dropMemStore forgets the in-memory store of a finished job.
func (s *Service) dropMemStore(id string) {
	s.mu.Lock()
	delete(s.memStores, id)
	s.mu.Unlock()
}

// boltStore is a store in a bbolt database with a write-back buffer:
// changes are kept in memory and committed together by Flush (one
// transaction, one sync), which the run calls before each checkpoint.
type boltStore struct {
	db *bbolt.DB

	flushMu sync.Mutex // serializes Flush
	mu      sync.Mutex
	// fp holds buffered records (nil: deleted); owned buffered marks.
	fp    map[string]*copier.Record
	owned map[string]bool
}

func openBolt(path string) (*boltStore, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 10 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("copyjob: opening the job database %s: %w", path, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{fpBucket, ownedBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("copyjob: preparing the job database %s: %w", path, err)
	}
	return &boltStore{db: db, fp: map[string]*copier.Record{}, owned: map[string]bool{}}, nil
}

// Get implements copier.FingerprintStore.
func (b *boltStore) Get(key string) (copier.Record, bool, error) {
	if key == "" {
		return copier.Record{}, false, nil
	}
	b.mu.Lock()
	rec, buffered := b.fp[key]
	b.mu.Unlock()
	if buffered {
		if rec == nil {
			return copier.Record{}, false, nil
		}
		out := *rec
		out.VIDMap = maps.Clone(rec.VIDMap)
		return out, true, nil
	}
	var (
		out   copier.Record
		found bool
	)
	err := b.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(fpBucket).Get([]byte(key))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &out)
	})
	if err != nil {
		return copier.Record{}, false, fmt.Errorf("copyjob: reading the record of %q: %w", key, err)
	}
	return out, found, nil
}

// Put implements copier.FingerprintStore.
func (b *boltStore) Put(key string, rec copier.Record) error {
	if key == "" {
		return nil
	}
	rec.VIDMap = maps.Clone(rec.VIDMap)
	return b.buffer(func() { b.fp[key] = &rec })
}

// Delete implements copier.FingerprintStore.
func (b *boltStore) Delete(key string) error {
	if key == "" {
		return nil
	}
	return b.buffer(func() { b.fp[key] = nil })
}

// Owned implements store.
func (b *boltStore) Owned(key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	b.mu.Lock()
	owned, buffered := b.owned[key]
	b.mu.Unlock()
	if buffered {
		return owned, nil
	}
	err := b.db.View(func(tx *bbolt.Tx) error {
		owned = tx.Bucket(ownedBucket).Get([]byte(key)) != nil
		return nil
	})
	return owned, err
}

// SetOwned implements store; it buffers a change only.
func (b *boltStore) SetOwned(key string, owned bool) error {
	if cur, err := b.Owned(key); err != nil || cur == owned || key == "" {
		return err
	}
	return b.buffer(func() { b.owned[key] = owned })
}

// buffer applies f to the buffer, flushing a full one.
func (b *boltStore) buffer(f func()) error {
	b.mu.Lock()
	f()
	full := len(b.fp)+len(b.owned) >= maxPending
	b.mu.Unlock()
	if full {
		return b.Flush()
	}
	return nil
}

// Flush implements store.
func (b *boltStore) Flush() error {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	b.mu.Lock()
	fp, owned := maps.Clone(b.fp), maps.Clone(b.owned)
	b.mu.Unlock()
	if len(fp)+len(owned) == 0 {
		return nil
	}
	err := b.db.Update(func(tx *bbolt.Tx) error {
		fb, ob := tx.Bucket(fpBucket), tx.Bucket(ownedBucket)
		for k, rec := range fp {
			if rec == nil {
				if err := fb.Delete([]byte(k)); err != nil {
					return err
				}
				continue
			}
			data, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := fb.Put([]byte(k), data); err != nil {
				return err
			}
		}
		for k, o := range owned {
			var err error
			if o {
				err = ob.Put([]byte(k), []byte{1})
			} else {
				err = ob.Delete([]byte(k))
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("copyjob: writing the job database: %w", err)
	}
	// Drop what was written, unless it changed meanwhile.
	b.mu.Lock()
	for k, rec := range fp {
		if b.fp[k] == rec {
			delete(b.fp, k)
		}
	}
	for k, o := range owned {
		if cur, ok := b.owned[k]; ok && cur == o {
			delete(b.owned, k)
		}
	}
	b.mu.Unlock()
	return nil
}

// Close implements store.
func (b *boltStore) Close() error {
	err := b.Flush()
	if cerr := b.db.Close(); err == nil {
		err = cerr
	}
	return err
}

// memStore is a store in memory, for jobs without a state directory.
type memStore struct {
	*copier.MemStore
	mu    sync.Mutex
	owned map[string]bool
}

// Owned implements store.
func (m *memStore) Owned(key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owned[key], nil
}

// SetOwned implements store.
func (m *memStore) SetOwned(key string, owned bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owned {
		m.owned[key] = true
	} else {
		delete(m.owned, key)
	}
	return nil
}

// Flush implements store (nothing to do).
func (m *memStore) Flush() error { return nil }

// Close implements store: the store stays for the job's next run.
func (m *memStore) Close() error { return nil }
