package clusterjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

type memoryCatalog struct {
	mu        sync.Mutex
	records   map[string][]byte
	readErr   error
	writeErr  error
	conflicts int
}

func (m *memoryCatalog) Get(_ context.Context, id string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return nil, false, m.readErr
	}
	v, ok := m.records[id]
	return bytes.Clone(v), ok, nil
}

func (m *memoryCatalog) List(context.Context) ([]cluster.JobRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return nil, m.readErr
	}
	out := make([]cluster.JobRecord, 0, len(m.records))
	for id, v := range m.records {
		out = append(out, cluster.JobRecord{ID: id, Data: bytes.Clone(v)})
	}
	return out, nil
}

func (m *memoryCatalog) CompareAndSwap(_ context.Context, change cluster.JobMutation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return m.writeErr
	}
	if m.conflicts > 0 {
		m.conflicts--
		return cluster.ErrJobRecordConflict
	}
	if change.CatalogDigest != "" {
		var records []cluster.JobRecord
		for id, value := range m.records {
			records = append(records, cluster.JobRecord{ID: id, Data: value})
		}
		if cluster.JobCatalogDigest(records) != change.CatalogDigest {
			return cluster.ErrJobRecordConflict
		}
	}
	before, ok := m.records[change.ID]
	if ok != change.Exists || !bytes.Equal(before, change.Expected) {
		return cluster.ErrJobRecordConflict
	}
	if change.Next == nil {
		delete(m.records, change.ID)
	} else {
		m.records[change.ID] = bytes.Clone(change.Next)
	}
	return nil
}

func TestImportedJobControlsAndReplicationCheckpointIsolation(t *testing.T) {
	initial := jobs.Job{ID: "copy_1", Kind: "copy", State: jobs.StateQueued,
		Labels: map[string]string{"source": "source", "target": "target"}, CreatedAt: time.Now().Add(-time.Hour)}
	encoded, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryCatalog{records: map[string][]byte{
		"copy_1":          encoded,
		"replication_123": []byte(`{"backfillComplete":true}`),
	}}
	svc := &Service{catalog: store}
	list, err := svc.ListConsistent(context.Background(), jobs.Filter{Labels: map[string]string{"source": "source"}})
	if err != nil || len(list) != 1 || list[0].ID != "copy_1" {
		t.Fatalf("imported job list: %+v, %v", list, err)
	}
	if _, err := svc.Get("replication_123"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("replication checkpoint exposed as job: %v", err)
	}
	if err := svc.Pause("copy_1"); err != nil {
		t.Fatal(err)
	}
	if job, err := svc.Get("copy_1"); err != nil || job.State != jobs.StatePaused {
		t.Fatalf("paused state: %+v, %v", job, err)
	}
	if err := svc.Resume("copy_1"); !errors.Is(err, jobs.ErrUnknownKind) {
		t.Fatalf("resumed without a cluster runner: %v", err)
	}
	if err := svc.Cancel("copy_1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete("copy_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get("copy_1"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("deleted job: %v", err)
	}
	if _, ok := store.records["replication_123"]; !ok {
		t.Fatal("replication checkpoint lost while deleting an imported job")
	}
}

func TestUnavailableCatalogNeverClaimsSuccessfulJobChange(t *testing.T) {
	store := &memoryCatalog{records: map[string][]byte{"copy_1": []byte(`{"state":"queued","kind":"copy"}`)}}
	svc := &Service{catalog: store}
	store.readErr = context.DeadlineExceeded
	if _, err := svc.ListConsistent(context.Background(), jobs.Filter{}); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("unavailable list: %v", err)
	}
	if err := svc.Cancel("copy_1"); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("unavailable read during cancellation: %v", err)
	}
	store.readErr = nil
	store.writeErr = cluster.ErrInsufficientAcks
	if err := svc.Cancel("copy_1"); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("unacknowledged cancellation: %v", err)
	}
	job, err := svc.Get("copy_1")
	if err != nil || job.State != jobs.StateQueued {
		t.Fatalf("unacknowledged state changed: %+v, %v", job, err)
	}
}

func TestJobControlsRetryConditionalConflictAndRejectUnsafeTransitions(t *testing.T) {
	store := &memoryCatalog{records: map[string][]byte{"copy_1": []byte(`{"state":"queued","kind":"copy"}`)}, conflicts: 1}
	svc := &Service{catalog: store}
	if err := svc.Delete("copy_1"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("deleted a queued job: %v", err)
	}
	if err := svc.Cancel("copy_1"); err != nil {
		t.Fatalf("conditional conflict was not retried: %v", err)
	}
	if err := svc.Pause("copy_1"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("paused a canceled job: %v", err)
	}
	if err := svc.Cancel("copy_1"); err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
	if err := svc.Delete("copy_1"); err != nil {
		t.Fatalf("delete canceled job: %v", err)
	}
}

func TestCorruptSharedJobRecordsFailListing(t *testing.T) {
	for name, data := range map[string][]byte{
		"invalid JSON":  []byte(`{"state":`),
		"mismatched ID": []byte(`{"id":"other","state":"queued"}`),
		"unknown state": []byte(`{"state":"suspended"}`),
	} {
		t.Run(name, func(t *testing.T) {
			svc := &Service{catalog: &memoryCatalog{records: map[string][]byte{"copy_1": data}}}
			if _, err := svc.ListConsistent(context.Background(), jobs.Filter{}); err == nil {
				t.Fatal("corrupt shared job record returned a successful listing")
			}
		})
	}
}
