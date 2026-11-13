package clusterjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/jobs"
)

type simultaneousCatalog struct {
	*memoryCatalog
	reads atomic.Int32
	ready sync.WaitGroup
}

func (c *simultaneousCatalog) List(ctx context.Context) ([]cluster.JobRecord, error) {
	records, err := c.memoryCatalog.List(ctx)
	if c.reads.Add(1) <= 2 {
		c.ready.Done()
		c.ready.Wait()
	}
	return records, err
}

func TestConcurrentCopySubmissionsReserveOverlappingTargets(t *testing.T) {
	backend := copyTestBackend(t)
	copyTestPut(t, backend, "source", "key", "payload")
	runner := &CopyRunner{Backend: backend, IsLeader: func() bool { return true }}
	for _, policy := range []string{"fail", "merge"} {
		t.Run(policy, func(t *testing.T) {
			store := &simultaneousCatalog{memoryCatalog: &memoryCatalog{records: map[string][]byte{}}}
			store.ready.Add(2)
			results := make(chan error, 2)
			// Separate services model requests entering different voters. Both
			// are forced to validate the same empty catalog before either CAS.
			for _, p := range []string{"fail", policy} {
				go func() {
					s := &Service{catalog: store, runners: map[string]Runner{"copy": runner}}
					_, err := s.Submit(t.Context(), "copy", json.RawMessage(fmt.Sprintf(`{"kind":"copy","source":{"bucket":"source"},"target":{"bucket":"target"},"options":{"configs":"none","onExisting":%q}}`, p)), "root")
					results <- err
				}()
			}
			accepted, conflicted := 0, 0
			for range 2 {
				err := <-results
				if err == nil {
					accepted++
				} else if errors.Is(err, jobs.ErrConflict) {
					conflicted++
				} else {
					t.Fatal(err)
				}
			}
			if accepted != 1 || conflicted != 1 {
				t.Fatalf("accepted=%d conflicted=%d, want one exclusive reservation", accepted, conflicted)
			}
		})
	}
}

func TestClaimDoesNotRunOverlappingLegacyCopyJobs(t *testing.T) {
	backend := copyTestBackend(t)
	runner := &CopyRunner{Backend: backend, IsLeader: func() bool { return true }}
	spec, _, err := runner.Validate(t.Context(), []byte(`{"kind":"copy","source":{"bucket":"source"},"target":{"bucket":"target"},"options":{"configs":"none","onExisting":"fail"}}`))
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryCatalog{records: map[string][]byte{}}
	for i, state := range []jobs.State{jobs.StateRunning, jobs.StateQueued} {
		id := fmt.Sprintf("copy_%d", i)
		store.records[id], err = json.Marshal(jobs.Job{ID: id, Kind: "copy", State: state, Spec: spec, CreatedAt: time.Unix(int64(i+1), 0)})
		if err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{catalog: store, runners: map[string]Runner{"copy": runner}, isLeader: func() bool { return true }}
	if err := s.claim(t.Context(), "copy_1"); !errors.Is(err, cluster.ErrJobRecordConflict) {
		t.Fatalf("overlapping legacy job was claimed: %v", err)
	}
	job, err := s.Get("copy_1")
	if err != nil || job.State != jobs.StateQueued {
		t.Fatalf("overlapping job state changed: %+v %v", job, err)
	}
}
