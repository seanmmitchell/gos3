package multi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Mode is a bucket's replication mode.
type Mode string

const (
	// ModeSingle keeps one copy, on the primary backing.
	ModeSingle Mode = "single"
	// ModeActivePassive serves every request from the primary and copies
	// changes to the replicas in the background. A replica serves reads
	// when the primary's backing fails, and takes over writes when it is
	// promoted (by an administrator, or automatically with
	// Options.AutoFailover).
	ModeActivePassive Mode = "active-passive"
	// ModeActiveActive writes every change to all in-sync members before
	// acknowledging it and spreads object reads over them. When the
	// primary's backing fails, an in-sync replica is promoted
	// automatically.
	ModeActiveActive Mode = "active-active"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	return m == ModeSingle || m == ModeActivePassive || m == ModeActiveActive
}

// stateVersion is the format version of the state file.
const stateVersion = 1

// stateFile is the persisted form of the placements (placements.json).
type stateFile struct {
	Version int                         `json:"version"`
	Buckets map[string]*placementRecord `json:"buckets"`
	// Purges are copies of buckets on backings that no longer hold a
	// member (removed replicas, the source of a finished move, the copies
	// of a deleted bucket) and are still to be deleted.
	Purges []purgeRecord `json:"purges,omitempty"`
	// DivergedPurges are the Purges of copies that were Diverged members
	// when they were removed (see purgeLocked).
	DivergedPurges []purgeRecord `json:"divergedPurges,omitempty"`
}

// placementRecord is the persisted placement of one bucket. Records are
// treated as immutable once published in bucketState.rec: changes build a
// new record (clone) and swap it in.
type placementRecord struct {
	Mode Mode `json:"mode"`
	// Members hold a copy of the bucket; Members[0] is the primary.
	Members []memberRecord `json:"members"`
	Move    *moveRecord    `json:"move,omitempty"`
}

// memberRecord is one backing holding a copy of a bucket.
type memberRecord struct {
	Backing string `json:"backing"`
	// Synced is true once the member's initial full copy completed (always
	// true for the primary). A replica that is not Synced never serves
	// reads and cannot be promoted without force.
	Synced bool `json:"synced"`
	// Diverged is set on the old primary by a forced promote or a
	// failover: its copy may hold acknowledged writes the new primary never
	// received. Until its next full copy completes (it is not Synced
	// meanwhile), the versions it has under object lock that the primary
	// lacks are copied back to the primary rather than deleted (see
	// syncKey), also when the copy is removed (see purgeLocked).
	Diverged bool `json:"diverged,omitempty"`
}

// moveRecord is a move of a member's copy to another backing. The target
// is not a member until the cutover.
type moveRecord struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"startedAt"`
	// Target is the kind and location of the bucket created on To (nil:
	// the backing's choice for copies). It is optional, so state files
	// written before it existed load unchanged (same stateVersion).
	Target *storage.BucketTarget `json:"target,omitempty"`
}

// purgeRecord is a bucket copy to delete from a backing.
type purgeRecord struct {
	Bucket  string `json:"bucket"`
	Backing string `json:"backing"`
}

func (p *placementRecord) clone() *placementRecord {
	c := *p
	c.Members = slices.Clone(p.Members)
	if p.Move != nil {
		m := *p.Move
		m.Target = cloneTarget(p.Move.Target)
		c.Move = &m
	}
	return &c
}

// cloneTarget returns a copy of t (nil for nil).
func cloneTarget(t *storage.BucketTarget) *storage.BucketTarget {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func (p *placementRecord) primary() string { return p.Members[0].Backing }

// member returns the index of backing among the members, or -1.
func (p *placementRecord) member(backing string) int {
	for i, m := range p.Members {
		if m.Backing == backing {
			return i
		}
	}
	return -1
}

// holds reports whether backing holds (or is receiving) a copy: a member
// or the target of the move.
func (p *placementRecord) holds(backing string) bool {
	return p.member(backing) >= 0 || (p.Move != nil && p.Move.To == backing)
}

// loadState reads the state file. A missing file is an empty state.
func loadState(path string) (*stateFile, error) {
	st := &stateFile{Version: stateVersion, Buckets: map[string]*placementRecord{}}
	if path == "" {
		return st, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("%s: unsupported format version %d (this gos3 reads version %d)", path, st.Version, stateVersion)
	}
	if st.Buckets == nil {
		st.Buckets = map[string]*placementRecord{}
	}
	for name, p := range st.Buckets {
		if p == nil || len(p.Members) == 0 {
			return nil, fmt.Errorf("%s: bucket %q has no members", path, name)
		}
		if !p.Mode.Valid() {
			return nil, fmt.Errorf("%s: bucket %q has an unknown mode %q", path, name, p.Mode)
		}
	}
	return st, nil
}

// writeState writes st to path atomically (temporary file, fsync, rename).
func writeState(path string, st *stateFile) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".placements-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = ""
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // best effort: not supported on every platform
		_ = d.Close()
	}
	return nil
}
