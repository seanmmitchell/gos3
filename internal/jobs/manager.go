package jobs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
)

// flushInterval bounds how often runner updates (checkpoint, status,
// phase) alone cause a write of the jobs file. State changes are written at
// once and carry pending runner updates with them.
const flushInterval = 10 * time.Second

// stopReason is why the Manager canceled a run's context.
type stopReason uint8

const (
	stopNone   stopReason = iota
	stopPause             // Pause: the job is paused when the run returns
	stopResume            // Pause, then Resume before the run returned: queued again
	stopCancel            // Cancel: the job is canceled when the run returns
	stopClose             // Close: queued (or waiting) for the next Open
)

// entry is the Manager's record of one job. All fields are guarded by
// Manager.mu.
type entry struct {
	job Job
	// seq orders the queue (see record.Seq).
	seq uint64
	// inQueue is true while the entry is in Manager.queue.
	inQueue bool
	// active is the handle of the run in progress (nil when not running).
	// Handles of earlier runs are stale: their updates are refused.
	active *Handle
	// cancel cancels the run in progress.
	cancel context.CancelFunc
	// stop is why the run in progress was canceled (stopNone: it was not).
	stop stopReason
	// kicked is set by Kick during a run: if the run goes idle, the job is
	// queued again at once instead of waiting.
	kicked bool
}

// Manager stores, runs and retries jobs (see the package documentation).
// All methods are safe for concurrent use.
type Manager struct {
	dir, path  string // "" when jobs are kept in memory
	workers    int
	runners    map[string]Runner
	log        *slog.Logger
	now        func() time.Time
	retryMin   time.Duration
	retryMax   time.Duration
	history    int
	historyAge time.Duration

	// saveMu serializes writes of the jobs file. It is taken before mu and
	// never while mu is held.
	saveMu sync.Mutex

	mu      sync.Mutex
	jobs    map[string]*entry
	queue   []*entry // queued jobs, ordered by seq
	running int      // runs in progress, including runs being stopped
	seq     uint64   // last queue sequence number handed out
	// gen counts changes to the jobs; savedGen is the gen written by the
	// last successful write of the jobs file.
	gen, savedGen uint64
	lastSave      time.Time // when the jobs file was last written
	started       bool      // runs may start (false until Start with DeferStart)
	closed        bool
	wakeTimer     *time.Timer // the next NextRunAt or history expiry
	flushTimer    *time.Timer // a pending write of the jobs file
	flushPending  bool
	flushAt       time.Time

	wg sync.WaitGroup // runs in progress
}

// Open loads the jobs from opts.Dir (see the package documentation for
// what happens to jobs interrupted by a restart), writes the jobs file once
// (so an unwritable directory fails here), and starts running queued jobs
// (with opts.DeferStart: at Start). Close stops it.
func Open(opts Options) (*Manager, error) {
	m := &Manager{
		started:    !opts.DeferStart,
		workers:    opts.Workers,
		runners:    make(map[string]Runner, len(opts.Runners)),
		log:        opts.Logger,
		now:        opts.Now,
		retryMin:   opts.RetryMin,
		retryMax:   opts.RetryMax,
		history:    opts.History,
		historyAge: opts.HistoryAge,
		jobs:       map[string]*entry{},
	}
	if m.workers <= 0 {
		m.workers = DefaultWorkers
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.retryMin <= 0 {
		m.retryMin = DefaultRetryMin
	}
	if m.retryMax <= 0 {
		m.retryMax = DefaultRetryMax
	}
	m.retryMax = max(m.retryMax, m.retryMin)
	if m.history <= 0 {
		m.history = DefaultHistory
	}
	if m.historyAge <= 0 {
		m.historyAge = DefaultHistoryAge
	}
	for kind, r := range opts.Runners {
		if kind == "" || r == nil {
			return nil, fmt.Errorf("jobs: invalid runner for job kind %q", kind)
		}
		m.runners[kind] = r
	}
	if opts.Dir == "" {
		m.log.Warn("jobs: no data directory; background jobs are kept in memory and lost when the server stops")
	} else {
		if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("jobs: %w", err)
		}
		m.dir, m.path = opts.Dir, filepath.Join(opts.Dir, fileName)
	}

	recs, err := loadFile(m.path)
	if err != nil {
		return nil, err
	}
	var queued []*entry
	for _, r := range recs {
		e := &entry{job: r.Job, seq: r.Seq}
		if e.job.State == StateRunning {
			// Interrupted without Close (a crash): run it again from its
			// last saved checkpoint.
			e.job.State = StateQueued
		}
		m.jobs[e.job.ID] = e
		m.seq = max(m.seq, r.Seq)
		if !e.job.State.Finished() && m.runners[e.job.Kind] == nil {
			m.log.Warn("jobs: no runner for the job's kind; the job is kept but does not run",
				"job", e.job.ID, "kind", e.job.Kind, "state", e.job.State)
			continue
		}
		if e.job.State == StateQueued {
			queued = append(queued, e)
		}
	}
	slices.SortFunc(queued, func(a, b *entry) int {
		if a.seq != b.seq {
			return cmp.Compare(a.seq, b.seq)
		}
		if c := a.job.CreatedAt.Compare(b.job.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.job.ID, b.job.ID)
	})

	m.mu.Lock()
	for _, e := range queued {
		e.inQueue = true
	}
	m.queue = queued
	removed := m.pruneLocked(m.now())
	m.gen = 1 // differs from savedGen: write the file once now
	m.mu.Unlock()
	if err := m.persist(); err != nil {
		return nil, err
	}
	m.removeDirs(removed)
	m.sweep()

	m.mu.Lock()
	m.armLocked()
	m.dispatchLocked()
	m.mu.Unlock()
	return m, nil
}

// Start starts running jobs on a Manager opened with Options.DeferStart:
// queued jobs start (in queue order, up to Options.Workers) and waiting
// jobs' NextRunAt timers are armed (a NextRunAt that passed meanwhile
// queues the job at once). It is idempotent, and does nothing after Close
// or without DeferStart.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true
	m.armLocked()
	m.dispatchLocked()
}

// Submit creates a job of the given kind and queues it. spec is checked and
// normalized by the kind's Runner.Validate, whose error Submit returns
// unchanged; ErrUnknownKind, ErrInvalidSpec (spec is not JSON) and
// ErrClosed come from the Manager. The job is saved before Submit returns.
func (m *Manager) Submit(ctx context.Context, kind string, spec json.RawMessage, createdBy string) (Job, error) {
	r := m.runners[kind]
	if r == nil {
		return Job{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	if m.isClosed() {
		return Job{}, ErrClosed
	}
	if !json.Valid(spec) {
		return Job{}, fmt.Errorf("%w: the spec is not valid JSON", ErrInvalidSpec)
	}
	norm, labels, err := r.Validate(ctx, spec)
	if err != nil {
		return Job{}, err
	}
	if norm == nil {
		norm = spec
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, norm); err != nil {
		return Job{}, fmt.Errorf("jobs: the %q runner normalized the spec to invalid JSON: %w", kind, err)
	}

	now := m.now()
	e := &entry{job: Job{
		ID:        newID(),
		Kind:      kind,
		CreatedBy: createdBy,
		State:     StateQueued,
		Labels:    maps.Clone(labels),
		CreatedAt: now,
		UpdatedAt: now,
		Spec:      buf.Bytes(),
	}}
	id := e.job.ID
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Job{}, ErrClosed
	}
	m.seq++
	e.seq = m.seq
	m.jobs[id] = e
	m.gen++
	m.mu.Unlock()

	// Save before running it: a job whose creation was reported survives a
	// crash.
	if err := m.persist(); err != nil {
		m.mu.Lock()
		if m.jobs[id] == e {
			delete(m.jobs, id)
			m.gen++
		}
		m.mu.Unlock()
		return Job{}, fmt.Errorf("jobs: saving the new job: %w", err)
	}

	m.mu.Lock()
	// Pause, Cancel or Close may have acted on the job in between.
	if !m.closed && m.jobs[id] == e && e.job.State == StateQueued && !e.inQueue {
		m.pushLocked(e)
		m.dispatchLocked()
	}
	j := e.job.clone()
	m.mu.Unlock()
	m.log.Info("job submitted", "job", id, "kind", kind, "createdBy", createdBy)
	return j, nil
}

// Get returns the job with the given ID (ErrNotFound if there is none).
func (m *Manager) Get(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.jobs[id]
	if e == nil {
		return Job{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return e.job.clone(), nil
}

// List returns the jobs that pass f, newest first (by creation time).
func (m *Manager) List(f Filter) []Job {
	m.mu.Lock()
	out := make([]Job, 0, len(m.jobs))
	for _, e := range m.jobs {
		if f.matchesFields(&e.job) {
			out = append(out, e.job.clone())
		}
	}
	m.mu.Unlock()
	if f.Match != nil {
		out = slices.DeleteFunc(out, func(j Job) bool { return !f.Match(j) })
	}
	slices.SortFunc(out, func(a, b Job) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	return out
}

// Pause stops a queued, waiting or running job until Resume. A running
// job's context is canceled and the job is paused once Run returns (its
// checkpoint is kept; if Run returns nil, the job succeeded instead);
// until then it stays running and keeps its worker. Pausing a paused job
// does nothing; finished jobs and jobs being canceled give ErrConflict.
func (m *Manager) Pause(id string) error {
	m.mu.Lock()
	e, err := m.lookupLocked(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	switch e.job.State {
	case StatePaused:
		m.mu.Unlock()
		return nil
	case StateQueued, StateWaiting:
		m.unqueueLocked(e)
		e.job.State = StatePaused
		e.job.UpdatedAt = m.now()
		m.gen++
		m.armLocked()
	case StateRunning:
		if e.stop == stopCancel {
			m.mu.Unlock()
			return fmt.Errorf("%w: job %s is being canceled", ErrConflict, id)
		}
		e.stop = stopPause
		e.cancel()
		m.mu.Unlock()
		m.log.Info("job pausing", "job", id)
		return nil
	default:
		state := e.job.State
		m.mu.Unlock()
		return fmt.Errorf("%w: job %s is %s", ErrConflict, id, state)
	}
	m.mu.Unlock()
	m.log.Info("job paused", "job", id)
	m.save()
	return nil
}

// Resume queues a paused job again; its next run starts from its last
// checkpoint. It also retries a failed job (from its last checkpoint, with
// Failures reset). On a running job being paused it cancels the pause: the
// job is queued again once its run returns. Resuming a queued, waiting or
// running job does nothing; succeeded and canceled jobs, and jobs being
// canceled, give ErrConflict; a job whose kind has no runner gives
// ErrUnknownKind.
func (m *Manager) Resume(id string) error {
	m.mu.Lock()
	e, err := m.lookupLocked(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	switch e.job.State {
	case StatePaused, StateFailed:
		if m.runners[e.job.Kind] == nil {
			m.mu.Unlock()
			return fmt.Errorf("%w: %q", ErrUnknownKind, e.job.Kind)
		}
		if e.job.State == StateFailed {
			e.job.Failures = 0
			e.job.FinishedAt = time.Time{}
		}
		e.job.NextRunAt = time.Time{}
		m.enqueueLocked(e, m.now())
		m.dispatchLocked()
		m.armLocked()
	case StateRunning:
		var err error
		switch e.stop {
		case stopPause:
			e.stop = stopResume
		case stopCancel:
			err = fmt.Errorf("%w: job %s is being canceled", ErrConflict, id)
		}
		m.mu.Unlock()
		return err
	case StateQueued, StateWaiting:
		m.mu.Unlock()
		return nil
	default:
		state := e.job.State
		m.mu.Unlock()
		return fmt.Errorf("%w: job %s is %s", ErrConflict, id, state)
	}
	m.mu.Unlock()
	m.log.Info("job resumed", "job", id)
	m.save()
	return nil
}

// Cancel ends an unfinished job as canceled. A running job's context is
// canceled and the job is canceled once Run returns (unless Run returns
// nil: then it succeeded). Canceling a canceled job does nothing;
// succeeded and failed jobs give ErrConflict.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	e, err := m.lookupLocked(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	var removed []string
	switch e.job.State {
	case StateCanceled:
		m.mu.Unlock()
		return nil
	case StateQueued, StateWaiting, StatePaused:
		now := m.now()
		m.unqueueLocked(e)
		e.job.State = StateCanceled
		e.job.FinishedAt, e.job.UpdatedAt = now, now
		e.job.NextRunAt = time.Time{}
		m.gen++
		removed = m.pruneLocked(now)
		m.armLocked()
	case StateRunning:
		e.stop = stopCancel
		e.cancel()
		m.mu.Unlock()
		m.log.Info("job canceling", "job", id)
		return nil
	default:
		state := e.job.State
		m.mu.Unlock()
		return fmt.Errorf("%w: job %s is %s", ErrConflict, id, state)
	}
	m.mu.Unlock()
	m.log.Info("job canceled", "job", id)
	m.save()
	m.removeDirs(removed)
	return nil
}

// Delete removes a finished job and its state directory. Unfinished jobs
// give ErrConflict (cancel them first).
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	e, err := m.lookupLocked(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if !e.job.State.Finished() {
		state := e.job.State
		m.mu.Unlock()
		return fmt.Errorf("%w: job %s is %s (cancel it first)", ErrConflict, id, state)
	}
	delete(m.jobs, id)
	m.gen++
	m.armLocked()
	m.mu.Unlock()
	m.log.Info("job deleted", "job", id)
	m.save()
	m.removeDirs([]string{id})
	return nil
}

// Kick runs a waiting job now (it is queued at once). Kicking a running
// job makes it run again right after the current run if that run goes
// idle, so a change noticed during a pass is never lost. Kick does nothing
// for other states and unknown IDs, and never blocks on I/O: it is safe to
// call from change notifications.
func (m *Manager) Kick(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.jobs[id]
	if e == nil || m.closed {
		return
	}
	switch e.job.State {
	case StateWaiting:
		if m.runners[e.job.Kind] == nil {
			return
		}
		m.enqueueLocked(e, m.now())
		m.dispatchLocked()
		m.armLocked()
		m.saveLaterLocked(0)
	case StateRunning:
		e.kicked = true
	}
}

// SetNextRun sets when a waiting job is queued again (a time that has
// passed queues it now; zero waits for Kick). For a running job it sets
// the time for the wait after the run, if the run goes idle. It does
// nothing for other states and unknown IDs.
func (m *Manager) SetNextRun(id string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.jobs[id]
	if e == nil || m.closed {
		return
	}
	switch e.job.State {
	case StateWaiting:
		e.job.NextRunAt = t
		e.job.UpdatedAt = m.now()
		m.gen++
		m.armLocked()
		m.saveLaterLocked(0)
	case StateRunning:
		e.job.NextRunAt = t
		m.gen++
	}
}

// Close stops the Manager: running jobs' contexts are canceled, Close
// waits for their runs to return, and the jobs are saved. A stopped run's
// job is queued for the next Open, unless Run returned nil (succeeded) or
// ErrIdle (waiting, or queued if it was kicked during the run), or a Pause
// or Cancel was pending. Run must therefore return promptly when its
// context ends, and must not call Close. Afterwards Get and List still
// work; other methods give ErrClosed or do nothing. Close is idempotent;
// it returns the error of the final write.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	for _, e := range m.jobs {
		if e.active == nil {
			continue
		}
		if e.stop == stopNone || e.stop == stopResume {
			e.stop = stopClose
		}
		e.cancel()
	}
	if m.wakeTimer != nil {
		m.wakeTimer.Stop()
	}
	if m.flushTimer != nil {
		m.flushTimer.Stop()
	}
	m.mu.Unlock()
	m.wg.Wait()
	return m.persist()
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// lookupLocked returns the job for a state-changing operation.
func (m *Manager) lookupLocked(id string) (*entry, error) {
	if m.closed {
		return nil, ErrClosed
	}
	e := m.jobs[id]
	if e == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return e, nil
}

// enqueueLocked queues e behind every queued job.
func (m *Manager) enqueueLocked(e *entry, now time.Time) {
	m.seq++
	e.seq = m.seq
	e.job.State = StateQueued
	e.job.UpdatedAt = now
	m.gen++
	m.pushLocked(e)
}

// pushLocked inserts e into the queue at its seq position.
func (m *Manager) pushLocked(e *entry) {
	i := len(m.queue)
	for i > 0 && m.queue[i-1].seq > e.seq {
		i--
	}
	m.queue = slices.Insert(m.queue, i, e)
	e.inQueue = true
}

// unqueueLocked removes e from the queue, if it is there.
func (m *Manager) unqueueLocked(e *entry) {
	if !e.inQueue {
		return
	}
	if i := slices.Index(m.queue, e); i >= 0 {
		m.queue = slices.Delete(m.queue, i, i+1)
	}
	e.inQueue = false
}

// dispatchLocked starts queued jobs while workers are free (once started).
func (m *Manager) dispatchLocked() {
	for m.started && !m.closed && m.running < m.workers && len(m.queue) > 0 {
		e := m.queue[0]
		m.queue = slices.Delete(m.queue, 0, 1)
		e.inQueue = false
		m.startLocked(e)
	}
}

// startLocked starts a run of e in a new goroutine.
func (m *Manager) startLocked(e *entry) {
	r := m.runners[e.job.Kind]
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handle{
		m:    m,
		e:    e,
		id:   e.job.ID,
		spec: e.job.Spec,
		log:  m.log.With("job", e.job.ID, "kind", e.job.Kind),
	}
	e.active, e.cancel, e.stop, e.kicked = h, cancel, stopNone, false
	now := m.now()
	e.job.State = StateRunning
	if e.job.StartedAt.IsZero() {
		e.job.StartedAt = now
	}
	e.job.UpdatedAt = now
	e.job.NextRunAt = time.Time{}
	m.running++
	m.gen++
	// Not saved synchronously: a job found running at Open is queued, as
	// it was before this change.
	m.saveLaterLocked(0)
	m.wg.Add(1)
	go m.run(ctx, r, h)
}

// run is a job run's goroutine.
func (m *Manager) run(ctx context.Context, r Runner, h *Handle) {
	defer m.wg.Done()
	h.log.Debug("job run started")
	err := runSafely(ctx, r, h)
	m.finish(h, err)
}

// runSafely calls r.Run, turning a panic into a permanent failure.
func runSafely(ctx context.Context, r Runner, h *Handle) (err error) {
	defer func() {
		if p := recover(); p != nil {
			h.log.Error("job runner panicked", "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("%w: the job runner panicked: %v", ErrPermanent, p)
		}
	}()
	return r.Run(ctx, h)
}

// finish applies the result of a run of h's job.
func (m *Manager) finish(h *Handle, err error) {
	e := h.e
	m.mu.Lock()
	e.active = nil
	e.cancel()
	e.cancel = nil
	m.running--
	stop, kicked := e.stop, e.kicked
	e.stop, e.kicked = stopNone, false
	now := m.now()
	j := &e.job
	j.UpdatedAt = now
	idle := errors.Is(err, ErrIdle)
	var (
		msg   string
		level = slog.LevelInfo
		attrs []any
	)
	switch {
	case err == nil:
		j.State, j.FinishedAt, j.NextRunAt = StateSucceeded, now, time.Time{}
		j.Failures, j.LastError = 0, ""
		msg = "job succeeded"
	case stop == stopCancel:
		j.State, j.FinishedAt, j.NextRunAt = StateCanceled, now, time.Time{}
		msg = "job canceled"
	case stop == stopPause:
		j.State = StatePaused
		msg = "job paused"
	case stop == stopResume:
		m.enqueueLocked(e, now)
		msg = "job resumed"
	case idle:
		j.Failures, j.LastError = 0, ""
		switch {
		case kicked:
			// Kicked during the run: run again (on Close: at the next Open).
			if m.closed {
				j.State = StateQueued
			} else {
				m.enqueueLocked(e, now)
			}
		default:
			j.State = StateWaiting
		}
		msg, level = "job idle", slog.LevelDebug
		attrs = append(attrs, "nextRunAt", j.NextRunAt)
	case stop == stopClose:
		j.State = StateQueued // runs again from its checkpoint at the next Open
		msg, level = "job stopped for shutdown", slog.LevelDebug
	case errors.Is(err, ErrPermanent):
		j.Failures++
		j.State, j.FinishedAt, j.NextRunAt = StateFailed, now, time.Time{}
		j.LastError = err.Error()
		msg, level = "job failed", slog.LevelWarn
		attrs = append(attrs, "error", err)
	default:
		j.Failures++
		delay := m.backoff(j.Failures)
		j.State, j.NextRunAt = StateWaiting, now.Add(delay)
		j.LastError = err.Error()
		msg, level = "job run failed; will retry", slog.LevelWarn
		attrs = append(attrs, "error", err, "failures", j.Failures, "retryIn", delay)
	}
	m.gen++
	var removed []string
	if j.State.Finished() {
		removed = m.pruneLocked(now)
	}
	m.dispatchLocked()
	m.armLocked()
	m.mu.Unlock()

	h.log.Log(context.Background(), level, msg, attrs...)
	m.save()
	m.removeDirs(removed)
}

// backoff returns the wait before the retry after the given number of
// consecutive failures.
func (m *Manager) backoff(failures int) time.Duration {
	d := m.retryMin
	for i := 1; i < failures && d < m.retryMax; i++ {
		d *= 2
	}
	return min(d, m.retryMax)
}

// pruneLocked drops finished jobs beyond the history limits and returns
// their IDs (whose state directories are to be deleted).
func (m *Manager) pruneLocked(now time.Time) []string {
	var done []*entry
	for _, e := range m.jobs {
		if e.job.State.Finished() {
			done = append(done, e)
		}
	}
	if len(done) == 0 {
		return nil
	}
	slices.SortFunc(done, func(a, b *entry) int {
		if c := b.job.FinishedAt.Compare(a.job.FinishedAt); c != 0 {
			return c
		}
		return strings.Compare(b.job.ID, a.job.ID)
	})
	// Expired at FinishedAt+historyAge exactly, the time armLocked wakes
	// for (pruning only after it would wake the timer again and again).
	cutoff := now.Add(-m.historyAge)
	var removed []string
	for i, e := range done {
		if i >= m.history || !e.job.FinishedAt.After(cutoff) {
			delete(m.jobs, e.job.ID)
			removed = append(removed, e.job.ID)
		}
	}
	if len(removed) > 0 {
		m.gen++
		m.log.Debug("jobs: pruned finished jobs from the history", "count", len(removed))
	}
	return removed
}

// armLocked sets the wake timer to the earliest NextRunAt of a waiting job
// or history expiry of a finished job. Before Start no timer is armed
// (Start arms it).
func (m *Manager) armLocked() {
	if m.closed || !m.started {
		return
	}
	var at time.Time
	for _, e := range m.jobs {
		var t time.Time
		switch {
		case e.job.State == StateWaiting:
			if e.job.NextRunAt.IsZero() || m.runners[e.job.Kind] == nil {
				continue
			}
			t = e.job.NextRunAt
		case e.job.State.Finished():
			t = e.job.FinishedAt.Add(m.historyAge)
		default:
			continue
		}
		if at.IsZero() || t.Before(at) {
			at = t
		}
	}
	if at.IsZero() {
		if m.wakeTimer != nil {
			m.wakeTimer.Stop()
		}
		return
	}
	d := max(at.Sub(m.now()), 0)
	if m.wakeTimer == nil {
		m.wakeTimer = time.AfterFunc(d, m.wake)
	} else {
		m.wakeTimer.Reset(d)
	}
}

// wake is the wake timer's function: it queues waiting jobs whose
// NextRunAt passed and prunes expired history.
func (m *Manager) wake() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	now := m.now()
	var due []*entry
	for _, e := range m.jobs {
		if e.job.State == StateWaiting && !e.job.NextRunAt.IsZero() && !e.job.NextRunAt.After(now) &&
			m.runners[e.job.Kind] != nil {
			due = append(due, e)
		}
	}
	slices.SortFunc(due, func(a, b *entry) int {
		if c := a.job.NextRunAt.Compare(b.job.NextRunAt); c != 0 {
			return c
		}
		return cmp.Compare(a.seq, b.seq)
	})
	for _, e := range due {
		m.enqueueLocked(e, now)
	}
	removed := m.pruneLocked(now)
	m.dispatchLocked()
	m.armLocked()
	m.mu.Unlock()
	if len(due) > 0 || len(removed) > 0 {
		m.save()
		m.removeDirs(removed)
	}
}

// saveLaterLocked makes sure the jobs file is written within d.
func (m *Manager) saveLaterLocked(d time.Duration) {
	if m.path == "" || m.closed {
		return
	}
	at := time.Now().Add(d)
	if m.flushPending && !m.flushAt.After(at) {
		return
	}
	m.flushPending, m.flushAt = true, at
	if m.flushTimer == nil {
		m.flushTimer = time.AfterFunc(d, m.flush)
	} else {
		m.flushTimer.Reset(d)
	}
}

// flushDelayLocked is how long a runner update waits to be written: until
// flushInterval after the last write.
func (m *Manager) flushDelayLocked() time.Duration {
	return max(time.Until(m.lastSave.Add(flushInterval)), 0)
}

// flush is the flush timer's function.
func (m *Manager) flush() {
	m.mu.Lock()
	m.flushPending = false
	closed := m.closed
	m.mu.Unlock()
	if !closed { // Close writes the file itself
		m.save()
	}
}

// save writes the jobs file. A failure is logged and the write retried
// later; the change stays in effect in memory.
func (m *Manager) save() {
	if err := m.persist(); err != nil {
		m.log.Error("jobs: could not save the jobs; retrying later", "error", err)
		m.mu.Lock()
		m.saveLaterLocked(flushInterval)
		m.mu.Unlock()
	}
}

// persist writes the current jobs to the jobs file unless an earlier or
// concurrent write already saved them.
func (m *Manager) persist() error {
	if m.path == "" {
		return nil
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	gen := m.gen
	if gen == m.savedGen {
		m.mu.Unlock()
		return nil
	}
	recs := make([]record, 0, len(m.jobs))
	for _, e := range m.jobs {
		// The documents and labels are never modified in place, so the
		// shallow copy can be encoded without the lock.
		recs = append(recs, record{Job: e.job, Seq: e.seq})
	}
	m.mu.Unlock()
	sortRecords(recs)
	err := writeFile(m.path, recs)
	m.mu.Lock()
	if err == nil {
		m.savedGen = gen
		m.lastSave = time.Now()
	}
	m.mu.Unlock()
	return err
}

// removeDirs deletes the state directories of the given jobs.
func (m *Manager) removeDirs(ids []string) {
	if m.dir == "" {
		return
	}
	for _, id := range ids {
		if err := os.RemoveAll(stateDir(m.dir, id)); err != nil {
			m.log.Warn("jobs: could not delete a job's state directory", "job", id, "error", err)
		}
	}
}

// sweep deletes state directories of jobs that no longer exist (left by a
// crash between saving a deletion and removing the directory).
func (m *Manager) sweep() {
	if m.dir == "" {
		return
	}
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		m.log.Warn("jobs: could not list the jobs directory", "dir", m.dir, "error", err)
		return
	}
	var orphans []string
	m.mu.Lock()
	for _, d := range ents {
		if d.IsDir() && validID(d.Name()) && m.jobs[d.Name()] == nil {
			orphans = append(orphans, d.Name())
		}
	}
	m.mu.Unlock()
	if len(orphans) > 0 {
		m.log.Info("jobs: deleting state directories of deleted jobs", "count", len(orphans))
		m.removeDirs(orphans)
	}
}
