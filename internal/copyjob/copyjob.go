// Package copyjob runs copy and mirror jobs (the "copy" and "mirror" job
// kinds of internal/jobs): it copies one bucket (or a key prefix of it) into
// another, on any backings and of any kinds, under another name, once
// (copy) or continuously (mirror). The job specification (Spec) and the
// progress report (Status) are those of the admin API (docs/admin-api.md,
// "Jobs: copy and mirror").
//
// # Copy
//
// A copy runs in phases, each resumable from the job's checkpoint:
//
//   - preparing: the target is created when it does not exist (with
//     Router.CreateBucket, so the backing's placement and default replicas
//     apply), its versioning is set to follow the source's when every
//     version is copied, the bucket configuration is copied (Options
//     "configs"; see syncConfigs) and the source is measured for the ETA;
//   - copying: the keys are copied (copier.ZipWalk over both buckets,
//     "concurrency" keys at once), resuming after the low-watermark key of
//     the checkpoint;
//   - verifying: a second pass ("finalPass") copies what changed during the
//     first;
//   - done.
//
// Keys are copied in identity mode (copier.SyncKey: version IDs,
// modification times and metadata are kept) when both buckets import
// versions, else in fingerprint mode (copier.SyncKeyFingerprint), whose
// records are kept in a bbolt database in the job's state directory.
//
// # Mirror
//
// A mirror copies everything once, then watches: changes made through the
// router to the source (Router.Subscribe) mark keys dirty and kick the job
// (debounced); a run copies the dirty keys, or makes a full pass when more
// than 100,000 keys are dirty, when verifyInterval has passed, after every
// restart of the server and, for native sources (which other tools may
// change), every pollInterval. Deletes propagate unless propagateDeletes is
// false; target keys the source does not have are deleted only when the job
// wrote them or with deleteExtraneous. A mirror whose source or target
// bucket is deleted fails.
//
// # Safety
//
// Validate refuses a spec that copies a range of keys onto itself, a target
// that is not empty (onExisting "fail"), a mirror that would close a cycle
// of mirrors, a target that an active mirror writes to (and a mirror into a
// target another active job writes to), every version into a target that
// cannot keep them, encrypted data into a target that cannot store it
// (unless allowPlaintext) and locked data into a target without object lock
// (unless dropRetention). The checks of other jobs are made again when a
// job starts, against the jobs created before it, so two jobs submitted at
// once cannot both pass.
package copyjob

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// Job kinds run by the Service.
const (
	KindCopy   = "copy"
	KindMirror = "mirror"
)

// DefaultMaxConcurrency is the default of Options.MaxConcurrency.
const DefaultMaxConcurrency = 32

// Default timings.
const (
	// debounceDelay is how long a mirror's change notifications are
	// collected before the job is kicked.
	debounceDelay = time.Second
	// checkpointInterval is the least time between two checkpoints of a
	// pass (the jobs Manager writes them at most every 10 seconds anyway).
	checkpointInterval = 3 * time.Second
	// statusInterval is how often a run's progress report is refreshed.
	statusInterval = time.Second
	// retryKeysDelay is when a mirror retries keys that failed in its last
	// run.
	retryKeysDelay = 30 * time.Second
)

// Options configure a Service.
type Options struct {
	// Router stores the buckets copied from and to (required).
	Router *multi.Router
	// Region returns the server's region, given to the target buckets jobs
	// create (nil: none).
	Region func() string
	// Logger receives the jobs' events (default slog.Default()).
	Logger *slog.Logger
	// MaxConcurrency bounds the keys copied at once by all jobs together,
	// and so each job's "concurrency" (default DefaultMaxConcurrency).
	MaxConcurrency int
	// Compression returns the server's compression policy, which decides
	// how copies are compressed with the "compression": "target" option
	// (nil: the default policy, compression optional per bucket).
	Compression func() config.CompressionSettings
}

// Service runs copy and mirror jobs. Create it with New, register its
// runners (Runner) with the jobs Manager, then Attach the Manager.
type Service struct {
	router      *multi.Router
	region      func() string
	log         *slog.Logger
	maxConc     int
	sem         chan struct{}
	compression func() config.CompressionSettings

	// Timings; tests shorten them.
	debounce        time.Duration
	checkpointEvery time.Duration
	statusEvery     time.Duration

	mgr         atomic.Pointer[jobs.Manager]
	unsubscribe func()

	mu      sync.Mutex
	closed  bool
	watches map[string]*watch // active mirrors, by job ID
	// index holds the watches by source and target bucket for the change
	// notifications, which read it without locking.
	index atomic.Pointer[watchIndex]
	// memStores keeps the state of jobs without a state directory (jobs
	// kept in memory) between their runs.
	memStores map[string]*memStore
}

// New returns a Service. It subscribes to the router's changes (for
// mirrors) until Close.
func New(opts Options) *Service {
	s := &Service{
		router:          opts.Router,
		region:          opts.Region,
		log:             opts.Logger,
		maxConc:         opts.MaxConcurrency,
		compression:     opts.Compression,
		debounce:        debounceDelay,
		checkpointEvery: checkpointInterval,
		statusEvery:     statusInterval,
		watches:         map[string]*watch{},
		memStores:       map[string]*memStore{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.maxConc <= 0 {
		s.maxConc = DefaultMaxConcurrency
	}
	s.sem = make(chan struct{}, s.maxConc)
	if s.router != nil {
		s.unsubscribe = s.router.Subscribe(s.onChange)
	}
	return s
}

// Runner returns the jobs.Runner of a job kind (KindCopy or KindMirror;
// nil for other kinds).
func (s *Service) Runner(kind string) jobs.Runner {
	switch kind {
	case KindCopy, KindMirror:
		return &runner{s: s, kind: kind}
	}
	return nil
}

// Attach gives the Service the Manager running its jobs: mirrors are kicked
// through it when their source changes, and Validate checks new jobs
// against the active ones. Every active mirror is kicked now, so that it
// makes a full pass after the restart. Attach is called once, after
// jobs.Open.
func (s *Service) Attach(m *jobs.Manager) {
	if m == nil {
		return
	}
	s.mgr.Store(m)
	for _, j := range m.List(jobs.Filter{Kind: KindMirror}) {
		if j.State.Finished() {
			continue
		}
		var sp Spec
		if err := json.Unmarshal(j.Spec, &sp); err != nil {
			s.log.Warn("copyjob: ignoring a mirror job with an unreadable spec", "job", j.ID, "error", err)
			continue
		}
		s.register(j.ID, sp)
		m.Kick(j.ID)
	}
}

// DryRun validates spec as Submit would (the kind is the spec's "kind")
// without creating anything, and returns its normalized form and warnings
// about what the job would do (a lifecycle configuration on the target,
// versions or encryption that would not be kept, ...). Its errors are those
// of Validate: wrapping jobs.ErrInvalidSpec for mistakes in the spec and
// jobs.ErrConflict for conflicts with the target's content or other jobs;
// other errors are failures to inspect the buckets.
func (s *Service) DryRun(ctx context.Context, spec json.RawMessage) (norm json.RawMessage, warnings []string, err error) {
	sp, warnings, err := s.check(ctx, "", spec, true)
	if err != nil {
		return nil, nil, err
	}
	sp.DryRun = false
	norm, err = json.Marshal(sp)
	if err != nil {
		return nil, nil, err
	}
	if warnings == nil {
		warnings = []string{}
	}
	return norm, warnings, nil
}

// Close stops watching the router's changes and the mirrors' pending
// kicks. Jobs still running are stopped by the Manager (close it first).
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ws := make([]*watch, 0, len(s.watches))
	for _, w := range s.watches {
		ws = append(ws, w)
	}
	s.mu.Unlock()
	if s.unsubscribe != nil {
		s.unsubscribe()
	}
	for _, w := range ws {
		w.stopTimer()
	}
}

// acquire takes one of the service-wide copy slots.
func (s *Service) acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) release() { <-s.sem }

// runner is the jobs.Runner of one kind.
type runner struct {
	s    *Service
	kind string
}

// Validate implements jobs.Runner (see the package documentation, Safety).
func (r *runner) Validate(ctx context.Context, spec json.RawMessage) (json.RawMessage, map[string]string, error) {
	sp, _, err := r.s.check(ctx, r.kind, spec, false)
	if err != nil {
		return nil, nil, err
	}
	if sp.DryRun {
		return nil, nil, invalidf("a dry run is not submitted as a job")
	}
	norm, err := json.Marshal(sp)
	if err != nil {
		return nil, nil, err
	}
	return norm, map[string]string{"source": sp.Source.Bucket, "target": sp.Target.Bucket}, nil
}

// Run implements jobs.Runner.
func (r *runner) Run(ctx context.Context, h *jobs.Handle) error {
	return r.s.run(ctx, h, r.kind)
}
