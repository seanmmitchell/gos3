package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var stageBucket = []byte("payloads")

var ErrStageFull = errors.New("cluster: replica memory staging limit reached")

type stagedPayload struct {
	digest    [32]byte
	data      []byte
	expiresAt time.Time
}

// expireStages runs under the store lock. Durable data is never expired.
func expireStages(memory map[string]stagedPayload, now time.Time) (freed int64) {
	for id, staged := range memory {
		if !staged.expiresAt.After(now) {
			delete(memory, id)
			freed += int64(len(staged.data))
		}
	}
	return freed
}

// StageStore keeps verified, complete payloads in memory before acknowledging
// Stage. Persist commits exactly those bytes in a syncing Bolt transaction.
// Its path must be on durable local storage; a volatile volume cannot count
// toward the configured durability threshold.
type StageStore struct {
	mu      sync.Mutex
	db      *bolt.DB
	memory  map[string]stagedPayload
	used    int64
	maxUsed int64
}

// DurableReplicaStore is the complete verified payload store of one voting
// node. Persist must not acknowledge until the local fsync or remote S3
// commit has succeeded. A durable copy also satisfies the memory threshold.
type DurableReplicaStore interface {
	ReplicaStage
	Read(context.Context, string) ([]byte, error)
	RepairCommitted(context.Context, string, []byte, [32]byte) error
	MaxMemoryBytes() int64
	Close() error
}

func (s *StageStore) MaxMemoryBytes() int64 { return s.maxUsed }

func OpenStageStore(path string, maxMemoryBytes int64) (*StageStore, error) {
	if path == "" || maxMemoryBytes < 1 {
		return nil, errors.New("cluster: staging needs a persistent path and positive memory limit")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("cluster: opening replica staging store: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucketIfNotExists(stageBucket); return err }); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &StageStore{db: db, memory: make(map[string]stagedPayload), maxUsed: maxMemoryBytes}, nil
}

// Stage rejects truncated, corrupted or conflicting payloads. A prior
// persistent commit of the same operation can be retried idempotently.
func (s *StageStore) Stage(ctx context.Context, operationID string, payload []byte, digest [32]byte) error {
	if operationID == "" || len(operationID) > 128 {
		return errors.New("cluster: invalid staging operation ID")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if sha256.Sum256(payload) != digest {
		return errors.New("cluster: staged payload digest mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	s.used -= expireStages(s.memory, now)
	if prior, ok := s.memory[operationID]; ok {
		if prior.digest != digest || !bytes.Equal(prior.data, payload) {
			return errors.New("cluster: operation ID reused with different payload")
		}
		prior.expiresAt = now.Add(stageLeaseDuration)
		s.memory[operationID] = prior
		return nil
	}
	var persisted []byte
	if err := s.db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(stageBucket).Get([]byte(operationID)); raw != nil {
			persisted = bytes.Clone(raw)
		}
		return nil
	}); err != nil {
		return err
	}
	if persisted != nil {
		if len(persisted) < 32 || !bytes.Equal(persisted[:32], digest[:]) || !bytes.Equal(persisted[32:], payload) {
			return errors.New("cluster: operation ID reused with different durable payload")
		}
		return nil
	}
	if int64(len(payload)) > s.maxUsed-s.used {
		return ErrStageFull
	}
	s.memory[operationID] = stagedPayload{digest: digest, data: bytes.Clone(payload), expiresAt: now.Add(stageLeaseDuration)}
	s.used += int64(len(payload))
	return nil
}

// Release discards volatile bytes after admission finishes. Persisted copies
// remain available for uncertain retries, committed reads and replica repair.
func (s *StageStore) Release(ctx context.Context, operationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if staged, ok := s.memory[operationID]; ok {
		delete(s.memory, operationID)
		s.used -= int64(len(staged.data))
	}
	return nil
}

// Persist acknowledges durability only after Bolt's synchronous transaction
// succeeds. The memory copy is freed after the durable copy is committed.
func (s *StageStore) Persist(ctx context.Context, operationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	staged, ok := s.memory[operationID]
	if !ok {
		return s.db.View(func(tx *bolt.Tx) error {
			if tx.Bucket(stageBucket).Get([]byte(operationID)) == nil {
				return errors.New("cluster: operation was not staged")
			}
			return nil
		})
	}
	value := make([]byte, 32+len(staged.data))
	copy(value, staged.digest[:])
	copy(value[32:], staged.data)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(stageBucket).Put([]byte(operationID), value)
	}); err != nil {
		return fmt.Errorf("cluster: replica durable commit failed: %w", err)
	}
	delete(s.memory, operationID)
	s.used -= int64(len(staged.data))
	return nil
}

// Read returns only a durable, verified payload. It is suitable for repair
// and for materializing a version after its consensus publication.
func (s *StageStore) Read(ctx context.Context, operationID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var value []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(stageBucket).Get([]byte(operationID)); raw != nil {
			value = bytes.Clone(raw)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(value) < 32 {
		return nil, errors.New("cluster: durable payload unavailable or corrupt")
	}
	payload := value[32:]
	digest := sha256.Sum256(payload)
	if !bytes.Equal(digest[:], value[:32]) {
		return nil, errors.New("cluster: durable payload digest mismatch")
	}
	return payload, nil
}

// Repair commits a verified copy fetched from another authenticated replica.
// It replaces a missing or corrupt local record, but never overwrites a valid
// record for the same operation ID with different bytes.
func (s *StageStore) Repair(ctx context.Context, operationID string, payload []byte) error {
	if operationID == "" || len(operationID) > 128 {
		return errors.New("cluster: invalid staging operation ID")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if int64(len(payload)) > s.maxUsed {
		return ErrStageFull
	}
	digest := sha256.Sum256(payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	if staged, ok := s.memory[operationID]; ok && (staged.digest != digest || !bytes.Equal(staged.data, payload)) {
		return errors.New("cluster: operation ID reused with different staged payload")
	}
	value := make([]byte, 32+len(payload))
	copy(value, digest[:])
	copy(value[32:], payload)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(stageBucket)
		if prior := bucket.Get([]byte(operationID)); len(prior) >= 32 {
			priorDigest := sha256.Sum256(prior[32:])
			if bytes.Equal(priorDigest[:], prior[:32]) {
				if !bytes.Equal(prior, value) {
					return errors.New("cluster: operation ID reused with different durable payload")
				}
				return nil
			}
		}
		return bucket.Put([]byte(operationID), value)
	}); err != nil {
		return fmt.Errorf("cluster: repairing replica: %w", err)
	}
	if staged, ok := s.memory[operationID]; ok {
		delete(s.memory, operationID)
		s.used -= int64(len(staged.data))
	}
	return nil
}

// RepairCommitted replaces a divergent local copy only after the caller has
// checked the bytes against a digest from the committed Raft version record.
func (s *StageStore) RepairCommitted(ctx context.Context, operationID string, payload []byte, expected [32]byte) error {
	if operationID == "" || len(operationID) > 128 || int64(len(payload)) > s.maxUsed || sha256.Sum256(payload) != expected {
		return errors.New("cluster: invalid committed repair payload")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	value := make([]byte, 32+len(payload))
	copy(value, expected[:])
	copy(value[32:], payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(stageBucket).Put([]byte(operationID), value)
	}); err != nil {
		return fmt.Errorf("cluster: repairing committed replica: %w", err)
	}
	if staged, ok := s.memory[operationID]; ok {
		delete(s.memory, operationID)
		s.used -= int64(len(staged.data))
	}
	return nil
}

func (s *StageStore) Close() error { return s.db.Close() }
