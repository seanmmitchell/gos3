package resticsnap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// The scheduler (modelled on internal/lifecycle's worker) wakes every
// minute, reads the buckets' snapshot configurations (all of them every 10
// minutes, and a bucket's again when the router reports a change of it)
// and submits a KindSnapshot job for each enabled bucket whose next run is
// due (nextRun) unless one is already active. Its first tick is delayed by
// a random jitter, so that servers restarted together do not snapshot at
// once.
//
// Its state, <SystemDir>/restic-snapshots.json, records per bucket the
// last attempt and success, the last snapshot and error, the repository,
// the last prune and the target (so that an aux repository name survives
// DeleteConfig, and the snapshots of a deleted bucket stay reachable). It
// is written atomically after every change.

const (
	// tickInterval is the scheduler's period.
	tickInterval = time.Minute
	// reloadInterval is how often every configuration is read again.
	reloadInterval = 10 * time.Minute
	// startJitter bounds the delay of the first tick.
	startJitter = 30 * time.Second
	// stateFileName is the state file in the system directory.
	stateFileName = "restic-snapshots.json"
	// stateVersion is the state file's format version.
	stateVersion = 1
)

// bucketState is the scheduler's state of one bucket.
type bucketState struct {
	LastAttemptAt time.Time `json:"lastAttemptAt,omitzero"`
	LastSuccessAt time.Time `json:"lastSuccessAt,omitzero"`
	LastSnapshot  string    `json:"lastSnapshot,omitempty"`
	LastError     string    `json:"lastError,omitempty"`
	// Repository is restic's --repo of the bucket's snapshots (no
	// secrets).
	Repository  string    `json:"repository,omitempty"`
	LastPruneAt time.Time `json:"lastPruneAt,omitzero"`
	// Target is the last target configured.
	Target *Target `json:"target,omitempty"`
	// AuxRepo is the last aux repository name configured.
	AuxRepo string `json:"auxRepo,omitempty"`
	// Stage is the bucket's stage directory, removed with its
	// configuration or the bucket.
	Stage string `json:"stage,omitempty"`
	// Unverified are snapshots without TagPartial that failed their
	// verification and could not be forgotten: never parents, never listed
	// or restored, forgotten by the next run.
	Unverified []string `json:"unverified,omitempty"`
}

// stateFile is the state file's content.
type stateFile struct {
	Version int                     `json:"version"`
	Buckets map[string]*bucketState `json:"buckets"`
}

// statePath returns the state file's path ("" without a system directory).
func (s *Service) statePath() string {
	if s.sysDir == "" {
		return ""
	}
	return filepath.Join(s.sysDir, stateFileName)
}

// loadState reads the state file.
func (s *Service) loadState() error {
	path := s.statePath()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resticsnap: %w", err)
	}
	var f stateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("resticsnap: %s: %w", path, err)
	}
	if f.Version != stateVersion {
		return fmt.Errorf("resticsnap: %s has format version %d; this build reads version %d", path, f.Version, stateVersion)
	}
	for name, st := range f.Buckets {
		if st != nil {
			s.state[name] = st
		}
	}
	return nil
}

// updateState applies fn to bucket's state and writes the state file.
func (s *Service) updateState(bucket string, fn func(*bucketState)) {
	s.mu.Lock()
	st := s.state[bucket]
	if st == nil {
		st = &bucketState{}
		s.state[bucket] = st
	}
	fn(st)
	s.mu.Unlock()
	s.saveState()
}

// stateOf returns a copy of bucket's state.
func (s *Service) stateOf(bucket string) bucketState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.state[bucket]; st != nil {
		return *st
	}
	return bucketState{}
}

// saveState writes the state file; a failure is logged.
func (s *Service) saveState() {
	path := s.statePath()
	if path == "" {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	f := stateFile{Version: stateVersion, Buckets: make(map[string]*bucketState, len(s.state))}
	for name, st := range s.state {
		c := *st
		f.Buckets[name] = &c
	}
	s.mu.Unlock()
	raw, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = config.WriteFileAtomic(path, raw, 0o600)
	}
	if err != nil {
		s.log.Error("resticsnap: could not save the snapshot scheduler state", "path", path, "err", err)
	}
}

// remember records bucket's configured target and repository.
func (s *Service) remember(bucket string, t Target, repository string) {
	s.updateState(bucket, func(st *bucketState) {
		tc := t
		st.Target = &tc
		if t.Repo != "" {
			st.AuxRepo = t.Repo
		}
		if repository != "" {
			st.Repository = repository
		}
	})
}

// rememberedRepo returns the aux repository name last configured for
// bucket ("" for none).
func (s *Service) rememberedRepo(bucket string) string {
	return s.stateOf(bucket).AuxRepo
}

// configChanged drops what is cached about bucket's configuration and has
// the scheduler read it again.
func (s *Service) configChanged(bucket string) {
	s.mu.Lock()
	delete(s.cache, bucket)
	s.dirty[bucket] = true
	sched := s.sched
	s.mu.Unlock()
	if sched != nil {
		sched.poke()
	}
}

// nextRun returns when the next snapshot of a bucket with configuration c
// and state st is due (zero when c is disabled). Without c.At it is an
// interval after the last attempt (now when there was none). With c.At,
// runs are at that time of day: the first such time that is after the last
// attempt and at least an interval after the slot of the last attempt (the
// latest time of day c.At at or before it); without an attempt, the next
// such time from now.
func nextRun(c Config, st bucketState, now time.Time) time.Time {
	if !c.Enabled {
		return time.Time{}
	}
	interval := c.interval()
	at, ok := c.atOffset()
	last := st.LastAttemptAt
	if !ok {
		if last.IsZero() {
			return now
		}
		return last.Add(interval)
	}
	slotOn := func(t time.Time) time.Time {
		t = t.UTC()
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(at)
	}
	if last.IsZero() {
		next := slotOn(now)
		if next.Before(now) {
			next = next.AddDate(0, 0, 1)
		}
		return next
	}
	lastSlot := slotOn(last)
	if lastSlot.After(last) {
		lastSlot = lastSlot.AddDate(0, 0, -1)
	}
	base := lastSlot.Add(interval)
	next := slotOn(base)
	for next.Before(base) || !next.After(last) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// scheduler is the goroutine started by Start.
type scheduler struct {
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
	unsub  func()
}

// poke makes the scheduler tick soon.
func (sc *scheduler) poke() {
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}

// Start starts the scheduler with the job manager m, whose runners must
// include the Service's (see Runner); RunNow, Restore and Prune submit
// their jobs to it. Calling Start again does nothing.
func (s *Service) Start(m *jobs.Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sched != nil {
		return
	}
	s.mgr = m
	ctx, cancel := context.WithCancel(context.Background())
	sc := &scheduler{cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	sc.unsub = s.router.Subscribe(s.onChange)
	s.sched = sc
	go s.loop(ctx, sc)
}

// Stop stops the scheduler and waits for it. Jobs keep running in the
// manager. Stop is idempotent.
func (s *Service) Stop() {
	s.mu.Lock()
	sc := s.sched
	s.mu.Unlock()
	if sc != nil {
		sc.unsub()
		sc.cancel()
		<-sc.done
	}
	s.removeTempStage()
}

// removeStage removes bucket's stage directory unless a run holds it (a
// later reload of the configurations retries).
func (s *Service) removeStage(bucket string) {
	dir := s.stateOf(bucket).Stage
	if dir == "" {
		return
	}
	l := s.stageLock(dir)
	if !l.TryLock() {
		return
	}
	defer l.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		s.log.Warn("resticsnap: could not remove a stage directory", "bucket", bucket, "path", dir, "err", err)
		return
	}
	s.updateState(bucket, func(st *bucketState) {
		if st.Stage == dir {
			st.Stage = ""
		}
	})
}

// sweepStages removes the stage directories of the buckets (by name) that
// have no snapshot configuration any more, or are gone.
func (s *Service) sweepStages(ctx context.Context, buckets []string) {
	for _, b := range buckets {
		if s.stateOf(b).Stage == "" {
			continue
		}
		_, err := s.loadConfig(ctx, b)
		if errors.Is(err, storage.ErrConfigNotFound) || errors.Is(err, storage.ErrBucketNotFound) {
			s.removeStage(b)
		}
	}
}

// onChange is the router subscription: it marks buckets whose snapshot
// configuration may have changed (it must not call the router).
func (s *Service) onChange(c multi.Change) {
	switch c.Kind {
	case multi.ChangeBucketConfig:
		if c.ConfigKey != storage.ConfigResticSnapshots {
			return
		}
	case multi.ChangeBucketCreated, multi.ChangeBucketDeleted:
	default:
		return
	}
	s.configChanged(c.Bucket)
}

// loop is the scheduler goroutine.
func (s *Service) loop(ctx context.Context, sc *scheduler) {
	defer close(sc.done)
	timer := time.NewTimer(5*time.Second + rand.N(startJitter))
	defer timer.Stop()
	started := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			started = true
		case <-sc.wake:
			if !started {
				continue // the jitter delays reacting to changes too
			}
		}
		s.tick(ctx)
		timer.Reset(tickInterval)
	}
}

// tick reads changed configurations and submits the snapshots that are
// due.
func (s *Service) tick(ctx context.Context) {
	now := s.now()
	s.reload(ctx, now)
	s.mu.Lock()
	type due struct {
		bucket string
		cfg    Config
	}
	var list []due
	for b, c := range s.configs {
		list = append(list, due{b, c})
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].bucket < list[j].bucket })
	for _, d := range list {
		if ctx.Err() != nil {
			return
		}
		next := nextRun(d.cfg, s.stateOf(d.bucket), now)
		if next.IsZero() || next.After(now) {
			continue
		}
		if _, err := s.submitSnapshot(ctx, d.bucket, "schedule", "scheduler"); err != nil && !errors.Is(err, jobs.ErrConflict) {
			s.log.Warn("resticsnap: could not submit a scheduled snapshot", "bucket", d.bucket, "err", err)
		}
	}
}

// reload reads every configuration when the last full read is older than
// reloadInterval, else those of the buckets marked dirty.
func (s *Service) reload(ctx context.Context, now time.Time) {
	s.mu.Lock()
	full := s.loadedAt.IsZero() || now.Sub(s.loadedAt) >= reloadInterval
	var dirty []string
	for b := range s.dirty {
		dirty = append(dirty, b)
	}
	s.dirty = map[string]bool{}
	s.mu.Unlock()
	if full {
		infos, err := s.router.ListBuckets(ctx)
		if err != nil {
			s.log.Warn("resticsnap: could not list the buckets", "err", err)
			return
		}
		configs := map[string]Config{}
		for _, in := range infos {
			if c, ok := s.readConfig(ctx, in.Name); ok {
				configs[in.Name] = c
			}
		}
		s.mu.Lock()
		s.configs, s.loadedAt = configs, now
		var staged []string
		for b, st := range s.state {
			if _, ok := configs[b]; !ok && st.Stage != "" {
				staged = append(staged, b)
			}
		}
		s.mu.Unlock()
		s.sweepStages(ctx, staged)
		return
	}
	var gone []string
	for _, b := range dirty {
		c, ok := s.readConfig(ctx, b)
		s.mu.Lock()
		if ok {
			s.configs[b] = c
		} else {
			delete(s.configs, b)
			gone = append(gone, b)
		}
		s.mu.Unlock()
	}
	s.sweepStages(ctx, gone)
}

// readConfig reads bucket's configuration for the scheduler.
func (s *Service) readConfig(ctx context.Context, bucket string) (Config, bool) {
	c, err := s.loadConfig(ctx, bucket)
	switch {
	case err == nil:
		return c, true
	case errors.Is(err, storage.ErrConfigNotFound), errors.Is(err, storage.ErrBucketNotFound):
	default:
		s.log.Warn("resticsnap: could not read a snapshot configuration", "bucket", bucket, "err", err)
	}
	return Config{}, false
}

// activeJob returns the ID of an unfinished job of kind for bucket ("" for
// none).
func activeJob(m *jobs.Manager, kind, bucket string) string {
	label := "bucket"
	if kind == KindRestore {
		label = "source"
	}
	for _, j := range m.List(jobs.Filter{Kind: kind, Labels: map[string]string{label: bucket}}) {
		if !j.State.Finished() {
			return j.ID
		}
	}
	return ""
}

// statusDoc is SnapshotStatus.
type statusDoc struct {
	LastAttemptAt time.Time `json:"lastAttemptAt,omitzero"`
	LastSuccessAt time.Time `json:"lastSuccessAt,omitzero"`
	LastSnapshot  string    `json:"lastSnapshot"`
	LastError     string    `json:"lastError"`
	NextRunAt     time.Time `json:"nextRunAt,omitzero"`
	Running       string    `json:"running"`
}

// Status returns the bucket's SnapshotStatus: the scheduler's record of
// the last run, the next scheduled run (none without an enabled
// configuration) and the active snapshot job.
func (s *Service) Status(ctx context.Context, bucket string) (json.RawMessage, error) {
	st := s.stateOf(bucket)
	doc := statusDoc{
		LastAttemptAt: st.LastAttemptAt, LastSuccessAt: st.LastSuccessAt,
		LastSnapshot: st.LastSnapshot, LastError: st.LastError,
	}
	c, err := s.loadConfig(ctx, bucket)
	switch {
	case err == nil:
		doc.NextRunAt = nextRun(c, st, s.now())
	case errors.Is(err, storage.ErrConfigNotFound):
	case errors.Is(err, jobs.ErrInvalidSpec):
		doc.LastError = err.Error()
	default:
		return nil, err
	}
	s.mu.Lock()
	m := s.mgr
	s.mu.Unlock()
	if m != nil {
		doc.Running = activeJob(m, KindSnapshot, bucket)
	}
	return json.Marshal(doc)
}
