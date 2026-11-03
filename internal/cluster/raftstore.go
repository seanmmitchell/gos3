package cluster

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

var (
	raftMetaBucket = []byte("meta")
	raftLogBucket  = []byte("entries")
	raftOpsBucket  = []byte("operations")
	raftHardKey    = []byte("hard-state")
	raftSnapKey    = []byte("snapshot")
	raftAppliedKey = []byte("applied")
	raftConfKey    = []byte("conf-state")
	raftNodeKey    = []byte("node-id")
	raftVotersKey  = []byte("fixed-voters")
)

// raftStore is the local, synchronous storage of a Raft voting member.
// Bolt's commit fsyncs the log before a Ready batch may be sent to peers.
// Its in-memory storage is only a read cache for the Raft algorithm.
type raftStore struct {
	mu      sync.Mutex
	db      *bolt.DB
	memory  *raft.MemoryStorage
	applied uint64
	conf    *pb.ConfState
}

func openRaftStore(path string) (_ *raftStore, restart bool, err error) {
	if path == "" {
		return nil, false, errors.New("cluster: a persistent Raft log path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("cluster: creating Raft log directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, false, fmt.Errorf("cluster: opening Raft log: %w", err)
	}
	s := &raftStore{db: db, memory: raft.NewMemoryStorage()}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(raftMetaBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(raftLogBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(raftOpsBucket)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("cluster: initializing Raft log: %w", err)
	}
	err = db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(raftMetaBucket)
		if raw := meta.Get(raftConfKey); raw != nil {
			s.conf = &pb.ConfState{}
			if err := proto.Unmarshal(raw, s.conf); err != nil {
				return fmt.Errorf("decoding configuration: %w", err)
			}
		}
		if raw := meta.Get(raftSnapKey); raw != nil {
			var snap pb.Snapshot
			if err := proto.Unmarshal(raw, &snap); err != nil {
				return fmt.Errorf("decoding snapshot: %w", err)
			}
			if err := s.memory.ApplySnapshot(&snap); err != nil {
				return fmt.Errorf("restoring snapshot: %w", err)
			}
			restart = true
		} else if s.conf != nil {
			// The initial snapshot may have only a voting configuration at
			// index zero. Raft needs it to restart before log compaction.
			if err := s.memory.ApplySnapshot(&pb.Snapshot{Metadata: &pb.SnapshotMetadata{ConfState: s.conf}}); err != nil {
				return err
			}
			restart = true
		}
		var entries []*pb.Entry
		if err := tx.Bucket(raftLogBucket).ForEach(func(_, raw []byte) error {
			entry := &pb.Entry{}
			if err := proto.Unmarshal(raw, entry); err != nil {
				return fmt.Errorf("decoding entry: %w", err)
			}
			entries = append(entries, entry)
			return nil
		}); err != nil {
			return err
		}
		if len(entries) > 0 {
			if err := s.memory.Append(entries); err != nil {
				return fmt.Errorf("restoring entries: %w", err)
			}
			restart = true
		}
		if raw := meta.Get(raftHardKey); raw != nil {
			var hard pb.HardState
			if err := proto.Unmarshal(raw, &hard); err != nil {
				return fmt.Errorf("decoding hard state: %w", err)
			}
			if err := s.memory.SetHardState(&hard); err != nil {
				return fmt.Errorf("restoring hard state: %w", err)
			}
			restart = true
		}
		if raw := meta.Get(raftAppliedKey); raw != nil {
			if len(raw) != 8 {
				return errors.New("invalid applied index")
			}
			s.applied = binary.BigEndian.Uint64(raw)
		}
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("cluster: corrupt Raft log: %w", err)
	}
	return s, restart, nil
}

// save persists a Ready batch atomically. It must finish before any of the
// batch's messages are sent or its committed entries are applied.
func (s *raftStore) save(rd raft.Ready) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if raft.IsEmptyHardState(rd.HardState) && raft.IsEmptySnap(rd.Snapshot) && len(rd.Entries) == 0 {
		return nil
	}
	var snapRaw, hardRaw []byte
	var err error
	if !raft.IsEmptySnap(rd.Snapshot) {
		snapRaw, err = proto.Marshal(rd.Snapshot)
		if err != nil {
			return err
		}
	}
	if !raft.IsEmptyHardState(rd.HardState) {
		hardRaw, err = proto.Marshal(rd.HardState)
		if err != nil {
			return err
		}
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta, log := tx.Bucket(raftMetaBucket), tx.Bucket(raftLogBucket)
		if snapRaw != nil {
			if err := meta.Put(raftSnapKey, snapRaw); err != nil {
				return err
			}
			cursor := log.Cursor()
			for key, _ := cursor.First(); key != nil && binary.BigEndian.Uint64(key) <= rd.Snapshot.GetMetadata().GetIndex(); key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		if len(rd.Entries) > 0 {
			cursor := log.Cursor()
			for key, _ := cursor.Seek(indexKey(rd.Entries[0].GetIndex())); key != nil; key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
			for _, entry := range rd.Entries {
				raw, err := proto.Marshal(entry)
				if err != nil {
					return err
				}
				if err := log.Put(indexKey(entry.GetIndex()), raw); err != nil {
					return err
				}
			}
		}
		if hardRaw != nil {
			return meta.Put(raftHardKey, hardRaw)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("cluster: persisting Raft Ready: %w", err)
	}
	if snapRaw != nil {
		if err := s.memory.ApplySnapshot(rd.Snapshot); err != nil {
			return fmt.Errorf("cluster: applying persisted Raft snapshot: %w", err)
		}
	}
	if len(rd.Entries) > 0 {
		if err := s.memory.Append(rd.Entries); err != nil {
			return fmt.Errorf("cluster: caching persisted Raft entries: %w", err)
		}
	}
	if hardRaw != nil {
		if err := s.memory.SetHardState(rd.HardState); err != nil {
			return fmt.Errorf("cluster: caching persisted Raft state: %w", err)
		}
	}
	return nil
}

// createSnapshot records a state-machine image at the last applied index,
// then compacts older log entries. The image is fsynced before the in-memory
// Raft storage may offer it to a lagging peer.
func (s *raftStore) createSnapshot(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applied == 0 || s.conf == nil {
		return errors.New("cluster: no applied state with fixed membership to snapshot")
	}
	prior, err := s.memory.Snapshot()
	if err != nil {
		return err
	}
	if s.applied <= prior.GetMetadata().GetIndex() {
		return nil
	}
	term, err := s.memory.Term(s.applied)
	if err != nil {
		return fmt.Errorf("cluster: snapshot applied index is unavailable: %w", err)
	}
	snapshot := &pb.Snapshot{Data: bytes.Clone(data), Metadata: &pb.SnapshotMetadata{Index: new(s.applied), Term: new(term), ConfState: proto.Clone(s.conf).(*pb.ConfState)}}
	raw, err := proto.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(raftMetaBucket).Put(raftSnapKey, raw); err != nil {
			return err
		}
		cursor := tx.Bucket(raftLogBucket).Cursor()
		for key, _ := cursor.First(); key != nil && binary.BigEndian.Uint64(key) <= s.applied; key, _ = cursor.Next() {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("cluster: persisting Raft snapshot: %w", err)
	}
	if _, err := s.memory.CreateSnapshot(s.applied, s.conf, data); err != nil {
		return fmt.Errorf("cluster: caching persisted Raft snapshot: %w", err)
	}
	if err := s.memory.Compact(s.applied); err != nil {
		return fmt.Errorf("cluster: compacting persisted Raft log: %w", err)
	}
	return nil
}

func indexKey(index uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, index)
	return key
}

func (s *raftStore) markApplied(index uint64, conf *pb.ConfState) error {
	return s.markOperationApplied(index, conf, nil, [32]byte{})
}

func (s *raftStore) operationApplied(id [16]byte, digest [32]byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(raftOpsBucket).Get(id[:]); raw != nil {
			if len(raw) != len(digest) || !slices.Equal(raw, digest[:]) {
				return errors.New("cluster: operation ID reused with a different command")
			}
			found = true
		}
		return nil
	})
	return found, err
}

func (s *raftStore) markOperationApplied(index uint64, conf *pb.ConfState, operationID *[16]byte, digest [32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index <= s.applied {
		return nil
	}
	var raw []byte
	var err error
	if conf != nil {
		raw, err = proto.Marshal(conf)
		if err != nil {
			return err
		}
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(raftMetaBucket)
		if operationID != nil {
			ops := tx.Bucket(raftOpsBucket)
			if prior := ops.Get(operationID[:]); prior != nil && !slices.Equal(prior, digest[:]) {
				return errors.New("cluster: operation ID reused with a different command")
			}
			if err := ops.Put(operationID[:], digest[:]); err != nil {
				return err
			}
		}
		if err := meta.Put(raftAppliedKey, indexKey(index)); err != nil {
			return err
		}
		if raw != nil {
			return meta.Put(raftConfKey, raw)
		}
		return nil
	})
	if err == nil {
		s.applied = index
		if conf != nil {
			s.conf = proto.Clone(conf).(*pb.ConfState)
		}
	}
	return err
}

func (s *raftStore) bindNodeID(id uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(raftMetaBucket)
		if raw := meta.Get(raftNodeKey); raw != nil {
			if len(raw) != 8 || binary.BigEndian.Uint64(raw) != id {
				return errors.New("cluster: Raft log belongs to a different node ID")
			}
			return nil
		}
		return meta.Put(raftNodeKey, indexKey(id))
	})
}

// bindVoters records the fixed peer set before StartNode can emit its first
// Ready. A crash after saving bootstrap entries but before applying their
// ConfChanges otherwise leaves the log with no applied ConfState to compare.
func (s *raftStore) bindVoters(voters []uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	canonical := slices.Clone(voters)
	slices.Sort(canonical)
	raw := make([]byte, len(canonical)*8)
	for i, id := range canonical {
		binary.BigEndian.PutUint64(raw[i*8:], id)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(raftMetaBucket)
		if saved := meta.Get(raftVotersKey); saved != nil {
			if !slices.Equal(saved, raw) {
				return errors.New("cluster: configured voters differ from persisted membership")
			}
			return nil
		}
		if s.conf != nil {
			persisted := slices.Clone(s.conf.GetVoters())
			slices.Sort(persisted)
			if !slices.Equal(persisted, canonical) {
				return errors.New("cluster: configured voters differ from persisted membership")
			}
		}
		return meta.Put(raftVotersKey, raw)
	})
}

func (s *raftStore) close() error { return s.db.Close() }
