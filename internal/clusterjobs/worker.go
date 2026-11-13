package clusterjobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/jobs"
)

// Runner validates a submitted specification and executes a claimed job.
// Run must honor ctx cancellation and make object changes idempotent: a new
// leader may restart an interrupted run from its persisted specification.
type Runner interface {
	Validate(context.Context, json.RawMessage) (json.RawMessage, map[string]string, error)
	Run(context.Context, jobs.Job) error
}

// ProgressRunner can commit a checkpoint and status after a complete unit
// of work. A checkpoint is visible only after the cluster acknowledgment
// policy has accepted its Raft mutation.
type ProgressRunner interface {
	RunWithProgress(context.Context, jobs.Job, func(context.Context, string, json.RawMessage, json.RawMessage) error) error
}

func (s *Service) currentTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) leader() bool { return s.isLeader != nil && s.isLeader() }

// Submit validates and commits the job before exposing its ID. Only kinds
// with an installed cluster runner can enter the shared queue.
func (s *Service) Submit(ctx context.Context, kind string, spec json.RawMessage, createdBy string) (jobs.Job, error) {
	runner := s.runners[kind]
	if runner == nil {
		return jobs.Job{}, fmt.Errorf("%w: %q has no cluster runner", jobs.ErrUnknownKind, kind)
	}
	if !json.Valid(spec) {
		return jobs.Job{}, fmt.Errorf("%w: the spec is not valid JSON", jobs.ErrInvalidSpec)
	}
	norm, labels, err := runner.Validate(ctx, spec)
	if err != nil {
		return jobs.Job{}, err
	}
	if norm == nil {
		norm = spec
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, norm); err != nil {
		return jobs.Job{}, fmt.Errorf("cluster jobs: runner returned invalid JSON: %w", err)
	}
	now := s.currentTime()
	job := jobs.Job{ID: strings.ToLower(rand.Text()), Kind: kind, CreatedBy: createdBy,
		State: jobs.StateQueued, Labels: maps.Clone(labels), CreatedAt: now, UpdatedAt: now, Spec: compact.Bytes()}
	data, err := json.Marshal(job)
	if err != nil {
		return jobs.Job{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	for {
		records, err := s.catalog.List(ctx)
		if err != nil {
			return jobs.Job{}, unavailable(err)
		}
		if err := checkCopyTargets(job, records, false); err != nil {
			return jobs.Job{}, err
		}
		mutation, err := cluster.NewJobMutation(job.ID, nil, false, data)
		if err != nil {
			return jobs.Job{}, err
		}
		mutation.CatalogDigest = cluster.JobCatalogDigest(records)
		if err := s.catalog.CompareAndSwap(ctx, mutation); errors.Is(err, cluster.ErrJobRecordConflict) {
			continue
		} else if err != nil {
			return jobs.Job{}, unavailable(err)
		}
		break
	}
	return job, nil
}

// Resume makes a paused or failed job eligible for the next leader scan.
func (s *Service) Resume(id string) error {
	return s.change(id, func(j *jobs.Job) (bool, error) {
		if s.runners[j.Kind] == nil {
			return false, fmt.Errorf("%w: %q has no cluster runner", jobs.ErrUnknownKind, j.Kind)
		}
		switch j.State {
		case jobs.StatePaused, jobs.StateFailed:
			j.State, j.UpdatedAt = jobs.StateQueued, s.currentTime()
			j.NextRunAt, j.FinishedAt = time.Time{}, time.Time{}
			j.Failures = 0
			return true, nil
		case jobs.StateQueued, jobs.StateWaiting, jobs.StateRunning:
			return false, nil
		default:
			return false, fmt.Errorf("%w: job %s is %s", jobs.ErrConflict, id, j.State)
		}
	})
}

// RunNow wakes a waiting job; a running job is scheduled for another pass.
func (s *Service) RunNow(id string) error {
	return s.change(id, func(j *jobs.Job) (bool, error) {
		if s.runners[j.Kind] == nil {
			return false, fmt.Errorf("%w: %q has no cluster runner", jobs.ErrUnknownKind, j.Kind)
		}
		switch j.State {
		case jobs.StateWaiting:
			j.State, j.UpdatedAt = jobs.StateQueued, s.currentTime()
			j.NextRunAt = time.Time{}
			return true, nil
		case jobs.StateRunning:
			j.NextRunAt, j.UpdatedAt = s.currentTime(), s.currentTime()
			return true, nil
		case jobs.StateQueued:
			return false, nil
		default:
			return false, fmt.Errorf("%w: job %s is %s", jobs.ErrConflict, id, j.State)
		}
	})
}

func (s *Service) cancelActive(id string) {
	s.mu.Lock()
	run := s.active[id]
	s.mu.Unlock()
	if run.cancel != nil {
		run.cancel()
	}
}

func (s *Service) cancelRun(id, runID string) {
	s.mu.Lock()
	run := s.active[id]
	s.mu.Unlock()
	if run.id == runID && run.cancel != nil {
		run.cancel()
	}
}

func (s *Service) cancelAll() {
	s.mu.Lock()
	for _, run := range s.active {
		run.cancel()
	}
	s.mu.Unlock()
}

// Run scans the shared queue until ctx ends. It runs only on the leader and
// cancels local work promptly when leadership changes.
func (s *Service) Run(ctx context.Context) {
	interval := s.interval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer func() { s.cancelAll(); s.wg.Wait() }()
	for {
		_ = s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick performs one deterministic leader scan. A running record without a
// local worker is reclaimable after a crash or leader change. The RunID in
// its Raft record fences completion from any earlier worker.
func (s *Service) Tick(ctx context.Context) error {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	if !s.leader() || ctx.Err() != nil {
		s.cancelAll()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	records, err := s.catalog.List(ctx)
	if err != nil {
		return unavailable(err)
	}
	limit := s.workers
	if limit <= 0 {
		limit = jobs.DefaultWorkers
	}
	var queued []jobs.Job
	for _, candidate := range records {
		if strings.HasPrefix(candidate.ID, "replication_") {
			continue
		}
		job, err := decode(candidate.ID, candidate.Data)
		if err != nil {
			return err
		}
		if s.runners[job.Kind] == nil || !eligible(job, s.currentTime()) {
			continue
		}
		queued = append(queued, job)
	}
	slices.SortFunc(queued, func(a, b jobs.Job) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, job := range queued {
		s.mu.Lock()
		busy := len(s.active) >= limit
		_, already := s.active[job.ID]
		s.mu.Unlock()
		if busy {
			break
		}
		if already || !s.leader() {
			continue
		}
		if err := s.claim(ctx, job.ID); err != nil && !errors.Is(err, cluster.ErrJobRecordConflict) {
			return err
		}
	}
	return nil
}

func eligible(job jobs.Job, now time.Time) bool {
	switch job.State {
	case jobs.StateQueued, jobs.StateRunning:
		return true
	case jobs.StateWaiting:
		return !job.NextRunAt.IsZero() && !job.NextRunAt.After(now)
	}
	return false
}

func (s *Service) claim(ctx context.Context, id string) error {
	record, before, err := s.getRecord(ctx, id)
	if err != nil {
		return err
	}
	if !eligible(record.Job, s.currentTime()) || !s.leader() {
		return nil
	}
	records, err := s.catalog.List(ctx)
	if err != nil {
		return unavailable(err)
	}
	// Old/imported queues may contain overlaps accepted by an earlier
	// version. Never execute their destructive copies concurrently.
	if err := checkCopyTargets(record.Job, records, true); err != nil {
		return cluster.ErrJobRecordConflict
	}
	record.RunID = strings.ToLower(rand.Text())
	record.State = jobs.StateRunning
	record.UpdatedAt = s.currentTime()
	if record.StartedAt.IsZero() {
		record.StartedAt = record.UpdatedAt
	}
	record.NextRunAt = time.Time{}
	after, err := json.Marshal(record)
	if err != nil {
		return err
	}
	mutation, err := cluster.NewJobMutation(id, before, true, after)
	if err != nil {
		return err
	}
	mutation.CatalogDigest = cluster.JobCatalogDigest(records)
	if err := s.catalog.CompareAndSwap(ctx, mutation); err != nil {
		if errors.Is(err, cluster.ErrJobRecordConflict) {
			return err
		}
		return unavailable(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.active == nil {
		s.active = map[string]activeRun{}
	}
	s.active[id] = activeRun{id: record.RunID, cancel: cancel}
	s.wg.Add(1)
	s.mu.Unlock()
	go s.execute(runCtx, record)
	return nil
}

func (s *Service) execute(ctx context.Context, record jobRecord) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		if run := s.active[record.ID]; run.id == record.RunID {
			run.cancel()
			delete(s.active, record.ID)
		}
		s.mu.Unlock()
	}()
	readCtx, readCancel := context.WithTimeout(ctx, operationTimeout)
	current, _, readErr := s.getRecord(readCtx, record.ID)
	readCancel()
	if readErr != nil || current.State != jobs.StateRunning || current.RunID != record.RunID || !s.leader() {
		return
	}
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				if !s.leader() {
					s.cancelRun(record.ID, record.RunID)
					return
				}
				probeCtx, cancel := context.WithTimeout(ctx, operationTimeout)
				current, _, err := s.getRecord(probeCtx, record.ID)
				cancel()
				if err != nil || current.State != jobs.StateRunning || current.RunID != record.RunID {
					s.cancelRun(record.ID, record.RunID)
					return
				}
			}
		}
	}()
	runner := s.runners[record.Kind]
	var err error
	if progressing, ok := runner.(ProgressRunner); ok {
		err = progressing.RunWithProgress(ctx, record.Job, func(updateCtx context.Context, phase string, status, checkpoint json.RawMessage) error {
			return s.updateRun(updateCtx, record.ID, record.RunID, phase, status, checkpoint)
		})
	} else {
		err = runner.Run(ctx, record.Job)
	}
	close(stop)
	if !s.leader() || ctx.Err() != nil {
		return
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	_ = s.finish(finishCtx, record.ID, record.RunID, err)
}

func (s *Service) updateRun(ctx context.Context, id, runID, phase string, status, checkpoint json.RawMessage) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if (status != nil && !json.Valid(status)) || (checkpoint != nil && !json.Valid(checkpoint)) {
		return errors.New("cluster jobs: invalid progress document")
	}
	for {
		record, before, err := s.getRecord(ctx, id)
		if err != nil {
			return err
		}
		if record.State != jobs.StateRunning || record.RunID != runID || !s.leader() {
			return fmt.Errorf("%w: job %s is no longer owned by this run", jobs.ErrConflict, id)
		}
		record.Phase, record.Status, record.Checkpoint = phase, bytes.Clone(status), bytes.Clone(checkpoint)
		record.UpdatedAt = s.currentTime()
		after, err := json.Marshal(record)
		if err != nil {
			return err
		}
		mutation, err := cluster.NewJobMutation(id, before, true, after)
		if err != nil {
			return err
		}
		err = s.catalog.CompareAndSwap(ctx, mutation)
		if errors.Is(err, cluster.ErrJobRecordConflict) {
			continue
		}
		return unavailable(err)
	}
}

func (s *Service) finish(ctx context.Context, id, runID string, runErr error) error {
	for {
		record, before, err := s.getRecord(ctx, id)
		if err != nil {
			return err
		}
		if record.State != jobs.StateRunning || record.RunID != runID || !s.leader() {
			return nil
		}
		now := s.currentTime()
		record.UpdatedAt, record.RunID = now, ""
		switch {
		case runErr == nil:
			record.State, record.FinishedAt = jobs.StateSucceeded, now
			record.Phase = "done"
			record.LastError, record.Failures = "", 0
		case errors.Is(runErr, jobs.ErrIdle):
			record.State = jobs.StateWaiting
			record.LastError, record.Failures = "", 0
			if record.NextRunAt.IsZero() {
				record.NextRunAt = now.Add(time.Minute)
			}
		case errors.Is(runErr, jobs.ErrPermanent):
			record.State, record.FinishedAt = jobs.StateFailed, now
			record.LastError = runErr.Error()
		default:
			record.State = jobs.StateWaiting
			record.Failures++
			record.LastError = runErr.Error()
			delay := jobs.DefaultRetryMin
			for i := 1; i < record.Failures && delay < jobs.DefaultRetryMax; i++ {
				delay = min(delay*2, jobs.DefaultRetryMax)
			}
			record.NextRunAt = now.Add(delay)
		}
		after, err := json.Marshal(record)
		if err != nil {
			return err
		}
		mutation, err := cluster.NewJobMutation(id, before, true, after)
		if err != nil {
			return err
		}
		err = s.catalog.CompareAndSwap(ctx, mutation)
		if errors.Is(err, cluster.ErrJobRecordConflict) {
			continue
		}
		return unavailable(err)
	}
}
