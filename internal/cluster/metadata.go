package cluster

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.etcd.io/bbolt"
)

// MetadataNamespace separates the ordered records a cluster shares. Peer
// addresses, TLS material and local backing definitions never enter this DB.
type MetadataNamespace string

const (
	MetadataBuckets  MetadataNamespace = "buckets"
	MetadataVersions MetadataNamespace = "versions"
	MetadataIAM      MetadataNamespace = "iam"
	MetadataRoles    MetadataNamespace = "roles"
	MetadataJobs     MetadataNamespace = "jobs"
	MetadataSettings MetadataNamespace = "settings"
)

var metadataNamespaces = []MetadataNamespace{
	MetadataBuckets, MetadataVersions, MetadataIAM, MetadataRoles, MetadataJobs, MetadataSettings,
}

var (
	errInvalidMetadataCommand  = errors.New("cluster: invalid metadata command")
	errMetadataIndexRegression = errors.New("cluster: metadata apply index regressed")
)

// MetadataCommand is an idempotently applied Raft command. Value may be
// empty; Delete distinguishes deletion from an empty record. Payload bytes
// live in StageStore and are referenced by version records, never embedded
// in this command.
type MetadataCommand struct {
	Namespace     MetadataNamespace `json:"namespace"`
	Key           string            `json:"key"`
	Value         []byte            `json:"value,omitempty"`
	Delete        bool              `json:"delete,omitempty"`
	PayloadSHA256 string            `json:"payloadSha256,omitempty"`
}

func (c MetadataCommand) validate() error {
	if !validMetadataNamespace(c.Namespace) || c.Key == "" || len(c.Key) > 4096 || len(c.Value) > 1<<20 || (c.Delete && len(c.Value) != 0) || !validOptionalPayloadHash(c.PayloadSHA256) {
		return errInvalidMetadataCommand
	}
	return nil
}

func validMetadataNamespace(ns MetadataNamespace) bool {
	for _, allowed := range metadataNamespaces {
		if ns == allowed {
			return true
		}
	}
	return false
}

func EncodeMetadataCommand(c MetadataCommand) ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

func decodeMetadataCommand(data []byte) (MetadataCommand, error) {
	var c MetadataCommand
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%w: %v", errInvalidMetadataCommand, err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return c, errInvalidMetadataCommand
	}
	return c, c.validate()
}

var metadataBucket = []byte("meta")
var metadataApplied = []byte("applied")
var metadataHash = []byte("hash")

func stateBucket(ns MetadataNamespace) []byte { return []byte("state:" + string(ns)) }

// MetadataStore is the durable state machine applied by Consensus. bbolt's
// default synchronous transaction commit makes an acknowledged apply survive
// restart. Replaying an already applied index cannot repeat a mutation.
type MetadataStore struct{ db *bbolt.DB }

func OpenMetadataStore(path string) (*MetadataStore, error) {
	if path == "" {
		return nil, errors.New("cluster: metadata path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 0})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(metadataBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(metadataOutcomes); err != nil {
			return err
		}
		for _, ns := range metadataNamespaces {
			if _, err := tx.CreateBucketIfNotExists(stateBucket(ns)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &MetadataStore{db: db}, nil
}

func metadataIndex(b *bbolt.Bucket) uint64 {
	if raw := b.Get(metadataApplied); len(raw) == 8 {
		return binary.BigEndian.Uint64(raw)
	}
	return 0
}

func putMetadataIndex(b *bbolt.Bucket, index uint64) error {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], index)
	return b.Put(metadataApplied, raw[:])
}

// Apply implements Consensus's apply hook. The command and its index are
// committed atomically, including a digest that catches an index replayed
// with different bytes after an interrupted or corrupt recovery.
func (s *MetadataStore) Apply(index uint64, data []byte) error {
	if index == 0 {
		return errInvalidMetadataCommand
	}
	transaction, isTransaction, err := decodeMetadataTransaction(data)
	if err != nil {
		return err
	}
	if isTransaction {
		return s.applyTransaction(index, data, transaction)
	}
	cmd, err := decodeMetadataCommand(data)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metadataBucket)
		applied := metadataIndex(meta)
		if index < applied {
			return nil
		}
		if index == applied {
			if bytes.Equal(meta.Get(metadataHash), hash[:]) {
				return nil
			}
			return errors.New("cluster: conflicting command at applied metadata index")
		}
		bucket := tx.Bucket(stateBucket(cmd.Namespace))
		if cmd.Delete {
			err = bucket.Delete([]byte(cmd.Key))
		} else {
			err = bucket.Put([]byte(cmd.Key), cmd.Value)
		}
		if err != nil {
			return err
		}
		if err := putMetadataIndex(meta, index); err != nil {
			return err
		}
		return meta.Put(metadataHash, hash[:])
	})
}

// Advance records a committed Raft index with no new state-machine command
// (bootstrap membership, heartbeat no-op, or a deduplicated proposal).
func (s *MetadataStore) Advance(index uint64) error {
	if index == 0 {
		return errMetadataIndexRegression
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metadataBucket)
		current := metadataIndex(meta)
		if index < current {
			return nil
		} // a command at a later index already applied
		if index == current {
			return nil
		}
		if err := putMetadataIndex(meta, index); err != nil {
			return err
		}
		return meta.Delete(metadataHash)
	})
}

func (s *MetadataStore) AppliedIndex() (uint64, error) {
	var index uint64
	err := s.db.View(func(tx *bbolt.Tx) error { index = metadataIndex(tx.Bucket(metadataBucket)); return nil })
	return index, err
}

func (s *MetadataStore) Get(ns MetadataNamespace, key string) ([]byte, bool, error) {
	if err := (MetadataCommand{Namespace: ns, Key: key}).validate(); err != nil {
		return nil, false, err
	}
	var value []byte
	err := s.db.View(func(tx *bbolt.Tx) error {
		if raw := tx.Bucket(stateBucket(ns)).Get([]byte(key)); raw != nil {
			value = bytes.Clone(raw)
		}
		return nil
	})
	return value, value != nil, err
}

type MetadataRecord struct {
	Key   string
	Value []byte
}

// List returns a stable, bytewise-ordered view of records with the prefix.
// Cluster callers must complete a Raft read barrier before using this local
// state for a client-visible read.
func (s *MetadataStore) List(ns MetadataNamespace, prefix string) ([]MetadataRecord, error) {
	if !validMetadataNamespace(ns) {
		return nil, errInvalidMetadataCommand
	}
	var records []MetadataRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(stateBucket(ns)).Cursor()
		for key, value := cursor.Seek([]byte(prefix)); key != nil && bytes.HasPrefix(key, []byte(prefix)); key, value = cursor.Next() {
			records = append(records, MetadataRecord{Key: string(key), Value: bytes.Clone(value)})
		}
		return nil
	})
	return records, err
}

// ListPage returns up to limit records after the exclusive key cursor. Each
// page is read in one Bolt snapshot; callers scanning changing state can
// resume at the last returned key without loading the whole namespace.
func (s *MetadataStore) ListPage(ns MetadataNamespace, prefix, after string, limit int) ([]MetadataRecord, error) {
	if !validMetadataNamespace(ns) || prefix == "" || limit < 1 || limit > 1024 {
		return nil, errInvalidMetadataCommand
	}
	var records []MetadataRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(stateBucket(ns)).Cursor()
		start := prefix
		if after != "" {
			start = after
		}
		for key, value := cursor.Seek([]byte(start)); key != nil && bytes.HasPrefix(key, []byte(prefix)) && len(records) < limit; key, value = cursor.Next() {
			if after != "" && bytes.Compare(key, []byte(after)) <= 0 {
				continue
			}
			records = append(records, MetadataRecord{Key: string(key), Value: bytes.Clone(value)})
		}
		return nil
	})
	return records, err
}

// HasPrefix checks one cursor position without loading a potentially large
// object namespace. Callers need a Raft read barrier for external reads.
func (s *MetadataStore) HasPrefix(ns MetadataNamespace, prefix string) (bool, error) {
	if !validMetadataNamespace(ns) || prefix == "" {
		return false, errInvalidMetadataCommand
	}
	var found bool
	err := s.db.View(func(tx *bbolt.Tx) error {
		key, _ := tx.Bucket(stateBucket(ns)).Cursor().Seek([]byte(prefix))
		found = bytes.HasPrefix(key, []byte(prefix))
		return nil
	})
	return found, err
}

type metadataImage struct {
	Applied  uint64                                  `json:"applied"`
	Records  map[MetadataNamespace]map[string][]byte `json:"records"`
	Outcomes map[string][]byte                       `json:"outcomes"`
}

// Snapshot returns the ordered state at a Raft applied index. Conf-change
// entries can make index exceed the last metadata command without changing
// the state image.
func (s *MetadataStore) Snapshot(index uint64) ([]byte, error) {
	var image metadataImage
	err := s.db.View(func(tx *bbolt.Tx) error {
		image.Applied = metadataIndex(tx.Bucket(metadataBucket))
		if index < image.Applied {
			return errMetadataIndexRegression
		}
		image.Records = make(map[MetadataNamespace]map[string][]byte, len(metadataNamespaces))
		for _, ns := range metadataNamespaces {
			items := make(map[string][]byte)
			if err := tx.Bucket(stateBucket(ns)).ForEach(func(k, v []byte) error { items[string(k)] = bytes.Clone(v); return nil }); err != nil {
				return err
			}
			image.Records[ns] = items
		}
		image.Outcomes = make(map[string][]byte)
		if err := tx.Bucket(metadataOutcomes).ForEach(func(k, v []byte) error {
			image.Outcomes[string(k)] = bytes.Clone(v)
			return nil
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(image)
}

// Restore replaces every shared namespace in one synchronous transaction.
// The caller supplies the Raft snapshot index; the image may have an earlier
// last command index when only membership entries followed that command.
func (s *MetadataStore) Restore(index uint64, data []byte) error {
	var image metadataImage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&image); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errInvalidMetadataCommand
	}
	if index == 0 || image.Applied > index || len(image.Records) != len(metadataNamespaces) || image.Outcomes == nil {
		return errInvalidMetadataCommand
	}
	for id, outcome := range image.Outcomes {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 || len(outcome) != 33 || (outcome[32] != 0 && outcome[32] != 1) {
			return errInvalidMetadataCommand
		}
	}
	for _, ns := range metadataNamespaces {
		items, ok := image.Records[ns]
		if !ok {
			return errInvalidMetadataCommand
		}
		for key, value := range items {
			if err := (MetadataCommand{Namespace: ns, Key: key, Value: value}).validate(); err != nil {
				return err
			}
		}
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metadataBucket)
		if metadataIndex(meta) > index {
			return errMetadataIndexRegression
		}
		for _, ns := range metadataNamespaces {
			name := stateBucket(ns)
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
			bucket, err := tx.CreateBucket(name)
			if err != nil {
				return err
			}
			for key, value := range image.Records[ns] {
				if err := bucket.Put([]byte(key), value); err != nil {
					return err
				}
			}
		}
		if err := tx.DeleteBucket(metadataOutcomes); err != nil {
			return err
		}
		outcomes, err := tx.CreateBucket(metadataOutcomes)
		if err != nil {
			return err
		}
		for id, outcome := range image.Outcomes {
			if err := outcomes.Put([]byte(id), outcome); err != nil {
				return err
			}
		}
		if err := putMetadataIndex(meta, index); err != nil {
			return err
		}
		return meta.Delete(metadataHash)
	})
}

func (s *MetadataStore) Close() error { return s.db.Close() }
