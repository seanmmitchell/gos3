// Package clusterjobs stores background job records in the Raft metadata
// state machine and runs registered job kinds on the current leader.
package clusterjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

const operationTimeout = 10 * time.Second

// Service reads and changes the shared background job namespace. A stopped
// cluster imported from another peer set can expose and retire its records.
type catalog interface {
	Get(context.Context, string) ([]byte, bool, error)
	List(context.Context) ([]cluster.JobRecord, error)
	CompareAndSwap(context.Context, cluster.JobMutation) error
}

type Service struct {
	catalog  catalog
	runners  map[string]Runner
	isLeader func() bool
	now      func() time.Time
	workers  int
	interval time.Duration

	tickMu sync.Mutex
	mu     sync.Mutex
	active map[string]activeRun
	wg     sync.WaitGroup
}

type activeRun struct {
	id     string
	cancel context.CancelFunc
}

// Options configure execution. With no registered runners, imported records
// can be inspected and retired, but no new job is accepted.
type Options struct {
	Runners      map[string]Runner
	IsLeader     func() bool
	Workers      int
	PollInterval time.Duration
}

func New(node *cluster.Node, opts ...Options) *Service {
	s := &Service{catalog: cluster.NewJobCatalog(node), now: time.Now, workers: jobs.DefaultWorkers,
		interval: time.Second, active: map[string]activeRun{}}
	if len(opts) > 0 {
		s.runners = opts[0].Runners
		s.isLeader = opts[0].IsLeader
		if opts[0].Workers > 0 {
			s.workers = opts[0].Workers
		}
		if opts[0].PollInterval > 0 {
			s.interval = opts[0].PollInterval
		}
	}
	return s
}

func unavailable(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", storage.ErrBackingUnavailable, err)
}

type jobRecord struct {
	jobs.Job
	RunID string `json:"clusterRunID,omitempty"`
}

func decodeRecord(id string, data []byte) (jobRecord, error) {
	var record jobRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return jobRecord{}, fmt.Errorf("cluster jobs: corrupt record %q: %w", id, err)
	}
	if record.ID != "" && record.ID != id {
		return jobRecord{}, fmt.Errorf("cluster jobs: record %q has mismatched ID %q", id, record.ID)
	}
	if !record.State.Valid() {
		return jobRecord{}, fmt.Errorf("cluster jobs: record %q has invalid state %q", id, record.State)
	}
	record.ID = id
	return record, nil
}

func decode(id string, data []byte) (jobs.Job, error) {
	record, err := decodeRecord(id, data)
	return record.Job, err
}

func validJobID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i, c := range id {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '-' || c == '_') {
			continue
		}
		return false
	}
	return true
}

func (s *Service) get(ctx context.Context, id string) (jobs.Job, []byte, error) {
	record, data, err := s.getRecord(ctx, id)
	return record.Job, data, err
}

func (s *Service) getRecord(ctx context.Context, id string) (jobRecord, []byte, error) {
	if !validJobID(id) || strings.HasPrefix(id, "replication_") {
		return jobRecord{}, nil, fmt.Errorf("%w: %s", jobs.ErrNotFound, id)
	}
	data, found, err := s.catalog.Get(ctx, id)
	if err != nil {
		return jobRecord{}, nil, unavailable(err)
	}
	if !found {
		return jobRecord{}, nil, fmt.Errorf("%w: %s", jobs.ErrNotFound, id)
	}
	record, err := decodeRecord(id, data)
	return record, data, err
}

func (s *Service) Get(id string) (jobs.Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	job, _, err := s.get(ctx, id)
	return job, err
}

// ListConsistent returns an error if a quorum read cannot finish. Admin
// handlers call this method so an unavailable cluster never returns a false
// empty list with HTTP 200.
func (s *Service) ListConsistent(ctx context.Context, filter jobs.Filter) ([]jobs.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	records, err := s.catalog.List(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	out := make([]jobs.Job, 0, len(records))
	for _, record := range records {
		// Replication scan checkpoints share the Raft jobs namespace, but
		// are not administrative background jobs.
		if strings.HasPrefix(record.ID, "replication_") {
			continue
		}
		job, err := decode(record.ID, record.Data)
		if err != nil {
			return nil, err
		}
		if filter.Kind != "" && job.Kind != filter.Kind || filter.State != "" && job.State != filter.State {
			continue
		}
		match := true
		for k, v := range filter.Labels {
			if job.Labels[k] != v {
				match = false
				break
			}
		}
		if match && (filter.Match == nil || filter.Match(job)) {
			out = append(out, job)
		}
	}
	slices.SortFunc(out, func(a, b jobs.Job) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	return out, nil
}

// List satisfies the standalone-compatible admin interface. Cluster admin
// handlers use ListConsistent to retain quorum errors.
func (s *Service) List(filter jobs.Filter) []jobs.Job {
	out, _ := s.ListConsistent(context.Background(), filter)
	return out
}

func (s *Service) change(id string, fn func(*jobs.Job) (bool, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	for {
		record, before, err := s.getRecord(ctx, id)
		if err != nil {
			return err
		}
		wasFinished := record.State.Finished()
		write, err := fn(&record.Job)
		if err != nil || !write {
			return err
		}
		var after []byte
		if record.ID != "" {
			if record.State != jobs.StateRunning {
				record.RunID = ""
			}
			after, err = json.Marshal(record)
			if err != nil {
				return err
			}
		}
		mutation, err := cluster.NewJobMutation(id, before, true, after)
		if err != nil {
			return err
		}
		if wasFinished && record.State == jobs.StateQueued {
			records, err := s.catalog.List(ctx)
			if err != nil {
				return unavailable(err)
			}
			if err := checkCopyTargets(record.Job, records, false); err != nil {
				return err
			}
			mutation.CatalogDigest = cluster.JobCatalogDigest(records)
		}
		err = s.catalog.CompareAndSwap(ctx, mutation)
		if errors.Is(err, cluster.ErrJobRecordConflict) {
			continue
		}
		if err != nil {
			return unavailable(err)
		}
		if record.State == jobs.StatePaused || record.State == jobs.StateCanceled || after == nil {
			s.cancelActive(id)
		}
		return nil
	}
}

func (s *Service) Pause(id string) error {
	return s.change(id, func(j *jobs.Job) (bool, error) {
		switch j.State {
		case jobs.StatePaused:
			return false, nil
		case jobs.StateQueued, jobs.StateWaiting, jobs.StateRunning:
			j.State, j.UpdatedAt = jobs.StatePaused, time.Now().UTC()
			j.NextRunAt = time.Time{}
			return true, nil
		default:
			return false, fmt.Errorf("%w: job %s is %s", jobs.ErrConflict, id, j.State)
		}
	})
}

func (s *Service) Cancel(id string) error {
	return s.change(id, func(j *jobs.Job) (bool, error) {
		switch j.State {
		case jobs.StateCanceled:
			return false, nil
		case jobs.StateQueued, jobs.StateWaiting, jobs.StatePaused, jobs.StateRunning:
			now := time.Now().UTC()
			j.State, j.UpdatedAt, j.FinishedAt = jobs.StateCanceled, now, now
			j.NextRunAt = time.Time{}
			return true, nil
		default:
			return false, fmt.Errorf("%w: job %s is %s", jobs.ErrConflict, id, j.State)
		}
	})
}

func (s *Service) Delete(id string) error {
	return s.change(id, func(j *jobs.Job) (bool, error) {
		if !j.State.Finished() {
			return false, fmt.Errorf("%w: job %s is %s", jobs.ErrConflict, id, j.State)
		}
		*j = jobs.Job{}
		return true, nil
	})
}
