// Package jobs runs persistent background jobs: copy and mirror jobs
// (internal/copyjob) and restic snapshot and restore jobs
// (internal/resticsnap). It knows nothing about what a job does: each job
// kind has a Runner that validates the job's spec and does the work, and
// the Manager stores the jobs, runs them, retries them and keeps their
// history.
//
// # States
//
//   - queued: waiting for a free worker. At most Options.Workers jobs run at
//     once; queued jobs start in the order they were queued (FIFO).
//   - running: Runner.Run is in progress.
//   - waiting: not running and re-queued when Job.NextRunAt passes (a zero
//     NextRunAt waits for Kick only). A continuous job (a mirror) waits
//     here between passes after Run returned ErrIdle; a job whose run
//     failed with a retryable error waits here for its retry (Failures > 0,
//     LastError set). Kick re-queues a waiting job at once.
//   - paused: stopped by Pause; Resume re-queues it.
//   - succeeded, failed, canceled: finished. Finished jobs are kept as
//     history (Options.History, Options.HistoryAge) until they are pruned
//     or deleted.
//
// # Persistence
//
// With Options.Dir set, every job is kept in <Dir>/jobs.json
// ({"version":1,"jobs":[...]}), written atomically on every state change;
// a runner's checkpoint, status and phase changes are written at most every
// 10 seconds (and with the next state change, and at Close). A file with
// another format version is refused at Open; unknown fields are ignored. A
// job's private state directory (Handle.StateDir) is <Dir>/<id>; it is
// deleted with the job. Without Dir, jobs live in memory only.
//
// After a restart, jobs that were running are queued again (their next run
// starts from the last saved checkpoint), paused jobs stay paused, and
// waiting jobs keep waiting for their NextRunAt.
//
// CONTRACT: the exported names in this file are used by the job runners
// (internal/copyjob, internal/resticsnap), the admin API and the server.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// State is a job's state (see the package documentation).
type State string

// Job states.
const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateWaiting   State = "waiting"
	StatePaused    State = "paused"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
)

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	switch s {
	case StateQueued, StateRunning, StateWaiting, StatePaused, StateSucceeded, StateFailed, StateCanceled:
		return true
	}
	return false
}

// Finished reports whether s is a final state (succeeded, failed or
// canceled). Only finished jobs can be deleted.
func (s State) Finished() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCanceled
}

// Job is a snapshot of one job. Values returned by the Manager and the
// Handle are copies: changing them changes nothing.
type Job struct {
	// ID identifies the job: 26 lower-case letters and digits, safe in
	// URLs and file names.
	ID string `json:"id"`
	// Kind selects the Runner (Options.Runners).
	Kind string `json:"kind"`
	// CreatedBy names who submitted the job (informational).
	CreatedBy string `json:"createdBy,omitempty"`

	State State `json:"state"`
	// Phase is the runner's current phase (Handle.SetPhase), e.g.
	// "copying".
	Phase string `json:"phase,omitempty"`
	// LastError is the error of the last failed run (cleared when a run
	// succeeds or goes idle).
	LastError string `json:"lastError,omitempty"`
	// Failures counts the failed runs since the last run that succeeded or
	// went idle; retries back off with it.
	Failures int `json:"failures,omitempty"`

	// Labels are set by the runner's Validate (e.g. the "source" and
	// "target" bucket names of a copy job) for filtering and authorization.
	Labels map[string]string `json:"labels,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	// StartedAt is when the job's first run started.
	StartedAt time.Time `json:"startedAt,omitzero"`
	// UpdatedAt is the time of the job's last change (state, phase, status
	// or checkpoint).
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	// FinishedAt is when the job reached a finished state.
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// NextRunAt is when a waiting job is queued again (zero: on Kick
	// only). A run starts with it cleared; the runner (Handle.SetNextRun)
	// or the job's owner (Manager.SetNextRun) sets it for the wait after
	// the run, and a failed run sets it to the retry time.
	NextRunAt time.Time `json:"nextRunAt,omitzero"`

	// Spec is the job's specification as normalized by Runner.Validate.
	Spec json.RawMessage `json:"spec"`
	// Status is the runner's progress report (Handle.SetStatus), in a
	// kind-specific format.
	Status json.RawMessage `json:"status,omitempty"`
	// Checkpoint is where the next run resumes (Handle.SaveCheckpoint), in
	// a kind-specific format.
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
}

// Runner does the work of one job kind. Its methods are called
// concurrently for different jobs.
type Runner interface {
	// Validate checks spec before a job is created and returns its
	// normalized form (nil: spec as given) and the job's labels. It may do
	// I/O (e.g. check that buckets exist) but must not start work. Submit
	// returns its error unchanged: wrap ErrInvalidSpec for mistakes in the
	// spec.
	Validate(ctx context.Context, spec json.RawMessage) (norm json.RawMessage, labels map[string]string, err error)
	// Run does (or continues) the job's work. A run after a pause, a
	// failure or a restart starts from the last saved checkpoint
	// (h.Checkpoint). ctx is canceled when the job is paused or canceled
	// and when the Manager closes; Run must then return promptly (its
	// result decides nothing in that case unless it is nil). The result:
	//   - nil: the job succeeded;
	//   - an error wrapping ErrIdle: a continuous job finished a pass and
	//     waits (state waiting) until Job.NextRunAt or Kick;
	//   - an error wrapping ErrPermanent: the job failed and is not retried;
	//   - any other error: the job is retried after a backoff (Options.
	//     RetryMin doubling up to RetryMax).
	Run(ctx context.Context, h *Handle) error
}

var (
	// ErrIdle is returned by Runner.Run when a continuous job has nothing
	// to do until it is kicked or its NextRunAt passes.
	ErrIdle = errors.New("jobs: idle")
	// ErrPermanent marks run errors that must not be retried (see
	// Permanent).
	ErrPermanent = errors.New("jobs: permanent failure")
	// ErrNotFound: no job has the ID.
	ErrNotFound = errors.New("jobs: no such job")
	// ErrConflict: the job's state does not allow the operation (e.g.
	// deleting an active job).
	ErrConflict = errors.New("jobs: conflicting job state")
	// ErrUnknownKind: no runner is registered for the job kind.
	ErrUnknownKind = errors.New("jobs: unknown job kind")
	// ErrInvalidSpec: the job's spec is invalid. Submit returns it for a
	// spec that is not JSON; runners wrap it in Validate errors.
	ErrInvalidSpec = errors.New("jobs: invalid job spec")
	// ErrClosed: the Manager is closed.
	ErrClosed = errors.New("jobs: manager closed")
	// ErrNoStateDir is returned by Handle.StateDir when jobs are kept in
	// memory (no Options.Dir).
	ErrNoStateDir = errors.New("jobs: no state directory (jobs are kept in memory)")
)

// Permanent returns err marked as permanent: errors.Is(result,
// ErrPermanent) and errors.Is(result, err) both hold, and the message is
// err's. Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

type permanentError struct{ err error }

func (e permanentError) Error() string   { return e.err.Error() }
func (e permanentError) Unwrap() []error { return []error{ErrPermanent, e.err} }

// Defaults of the Options fields.
const (
	DefaultWorkers    = 2
	DefaultRetryMin   = time.Second
	DefaultRetryMax   = 5 * time.Minute
	DefaultHistory    = 200
	DefaultHistoryAge = 30 * 24 * time.Hour
)

// Options configure a Manager.
type Options struct {
	// Dir holds jobs.json and the jobs' state directories ("": jobs are
	// kept in memory and lost when the process exits; a warning is logged).
	Dir string
	// Workers is the number of jobs that run at once (default
	// DefaultWorkers).
	Workers int
	// Runners maps each job kind to its runner. Jobs of a kind without a
	// runner (e.g. loaded from jobs.json) are kept but never run.
	Runners map[string]Runner
	// Logger receives job events (default slog.Default()).
	Logger *slog.Logger
	// Now returns the current time for job timestamps and NextRunAt
	// comparisons (default time.Now). Timers still wait in real time.
	Now func() time.Time
	// RetryMin and RetryMax bound the retry backoff: the n-th consecutive
	// failure waits RetryMin·2^(n-1), at most RetryMax (defaults
	// DefaultRetryMin, DefaultRetryMax).
	RetryMin, RetryMax time.Duration
	// History is the number of finished jobs kept (default DefaultHistory);
	// HistoryAge is how long a finished job is kept (default
	// DefaultHistoryAge). Older jobs are deleted with their state
	// directories.
	History    int
	HistoryAge time.Duration
	// DeferStart makes Open load the jobs without running any until
	// Manager.Start: queued jobs (including jobs submitted, resumed or
	// kicked in the meantime) wait in the queue and waiting jobs' NextRunAt
	// timers are not armed. Use it when runners depend on components that
	// are attached to the Manager after Open. Close works either way.
	DeferStart bool
}

// Filter selects jobs for Manager.List. Zero fields match everything.
type Filter struct {
	// Kind matches the job kind.
	Kind string
	// State matches the job state.
	State State
	// Labels must all be present on the job with these values.
	Labels map[string]string
	// Match, if set, must also return true (e.g. "source or target is
	// bucket b", or an authorization check). It gets a copy of the job and
	// is called without the Manager's lock held.
	Match func(Job) bool
}

// matchesFields reports whether j passes the filter's Kind, State and
// Labels (not Match).
func (f Filter) matchesFields(j *Job) bool {
	if f.Kind != "" && j.Kind != f.Kind {
		return false
	}
	if f.State != "" && j.State != f.State {
		return false
	}
	for k, v := range f.Labels {
		if got, ok := j.Labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}
