package gateway

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Persistent upload state (StateDir/gateway.db, bbolt):
//
//	uploads: <upload ID>                       -> JSON uploadRecord
//	parts:   <upload ID> 0x00 <uint16 BE part> -> JSON partRecord
//
// Staged part files live in StateDir/parts and are referenced by name from
// their part records. Everything is loaded into memory at Open; the
// in-memory state is authoritative while the store is open and every
// change is written through.

var (
	bucketUploads = []byte("uploads")
	bucketParts   = []byte("parts")
)

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

func partKey(id string, n int) []byte {
	k := append([]byte(id), 0)
	return binary.BigEndian.AppendUint16(k, uint16(n))
}

// openState opens (creating) the state database, loads every upload and
// removes staged part files no record refers to.
func (s *Store) openState(path string) error {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("gateway %q: open state database: %w", s.name, err)
	}
	referenced := map[string]bool{}
	err = db.Update(func(tx *bolt.Tx) error {
		ub, err := tx.CreateBucketIfNotExists(bucketUploads)
		if err != nil {
			return err
		}
		pb, err := tx.CreateBucketIfNotExists(bucketParts)
		if err != nil {
			return err
		}
		if err := ub.ForEach(func(k, v []byte) error {
			var r uploadRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("upload record %q: %w", k, err)
			}
			s.addUploadLocked(&upload{
				id: r.ID, bucket: r.Bucket, key: r.Key, initiated: r.Initiated,
				meta: r.Meta, initiator: r.Initiator, remoteID: r.RemoteID,
				parts: map[int]*part{},
			})
			return nil
		}); err != nil {
			return err
		}
		var stale [][]byte
		if err := pb.ForEach(func(k, v []byte) error {
			var r partRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("part record %q: %w", k, err)
			}
			id := string(k[:max(len(k)-3, 0)])
			u := s.uploads[id]
			if u == nil {
				stale = append(stale, append([]byte(nil), k...))
				return nil
			}
			if r.File != "" {
				if _, err := os.Stat(filepath.Join(s.stageDir, r.File)); err != nil {
					s.log.Warn("gateway: staged part file missing; part dropped", "backing", s.name, "upload", id, "part", r.Number, "err", err)
					stale = append(stale, append([]byte(nil), k...))
					return nil
				}
				referenced[r.File] = true
			}
			u.parts[r.Number] = &part{
				info: storage.PartInfo{
					PartMeta:     storage.PartMeta{ETag: r.ETag, Checksums: r.Checksums},
					PartNumber:   r.Number,
					Size:         r.Size,
					LastModified: r.LastModified,
				},
				remoteETag: r.RemoteETag,
				file:       r.File,
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range stale {
			if err := pb.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		s.uploads, s.byBucket = map[string]*upload{}, map[string][]*upload{}
		return fmt.Errorf("gateway %q: load state database: %w", s.name, err)
	}
	s.db = db
	// Staged files without a record: parts whose commit did not finish.
	if entries, err := os.ReadDir(s.stageDir); err == nil {
		for _, e := range entries {
			if !referenced[e.Name()] {
				_ = os.Remove(filepath.Join(s.stageDir, e.Name()))
			}
		}
	}
	return nil
}

// saveUpload writes u's record (no-op without a database).
func (s *Store) saveUpload(u *upload) error {
	if s.db == nil {
		return nil
	}
	r := uploadRecord{ID: u.id, Bucket: u.bucket, Key: u.key, Initiated: u.initiated, Meta: u.meta, Initiator: u.initiator, RemoteID: u.remoteID}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketUploads).Put([]byte(u.id), data) }); err != nil {
		return fmt.Errorf("gateway %q: save upload %q: %w", s.name, u.id, err)
	}
	return nil
}

// savePart writes a part record.
func (s *Store) savePart(id string, p *part) error {
	if s.db == nil {
		return nil
	}
	r := partRecord{
		Number: p.info.PartNumber, Size: p.info.Size, ETag: p.info.ETag, Checksums: p.info.Checksums,
		LastModified: p.info.LastModified, RemoteETag: p.remoteETag, File: p.file,
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketParts).Put(partKey(id, p.info.PartNumber), data) }); err != nil {
		return fmt.Errorf("gateway %q: save part %d of upload %q: %w", s.name, p.info.PartNumber, id, err)
	}
	return nil
}

// deleteUploadState removes an upload's records.
func (s *Store) deleteUploadState(id string) error {
	if s.db == nil {
		return nil
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketUploads).Delete([]byte(id)); err != nil {
			return err
		}
		c := tx.Bucket(bucketParts).Cursor()
		prefix := append([]byte(id), 0)
		for k, _ := c.Seek(prefix); k != nil && len(k) == len(prefix)+2 && string(k[:len(prefix)]) == string(prefix); k, _ = c.Seek(prefix) {
			if err := c.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("gateway %q: delete upload %q: %w", s.name, id, err)
	}
	return nil
}
