package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// errRunEnded is returned by Handle updates after the run returned.
var errRunEnded = errors.New("jobs: the job's run has ended")

// Handle is a run's access to its job, passed to Runner.Run. Updates
// (SaveCheckpoint, SetStatus, SetPhase, SetNextRun) are accepted only
// while that run is in progress: after Run returns they are refused or
// ignored, so a run that outlives its context cannot overwrite the state
// of a later run. The other methods work at any time. Methods are safe for
// concurrent use.
type Handle struct {
	m    *Manager
	e    *entry
	id   string
	spec json.RawMessage // immutable
	log  *slog.Logger
}

// Job returns a snapshot of the job.
func (h *Handle) Job() Job {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.e.job.clone()
}

// Spec decodes the job's (normalized) spec into v.
func (h *Handle) Spec(v any) error {
	spec := h.spec
	if len(spec) == 0 {
		spec = json.RawMessage("null") // a hand-edited jobs file without one
	}
	if err := json.Unmarshal(spec, v); err != nil {
		return fmt.Errorf("jobs: decoding the spec of job %s: %w", h.id, err)
	}
	return nil
}

// Checkpoint decodes the job's last saved checkpoint into v. It reports
// false (and leaves v alone) when no checkpoint was saved.
func (h *Handle) Checkpoint(v any) (bool, error) {
	h.m.mu.Lock()
	cp := h.e.job.Checkpoint
	h.m.mu.Unlock()
	if isNullDoc(cp) {
		return false, nil
	}
	if err := json.Unmarshal(cp, v); err != nil {
		return false, fmt.Errorf("jobs: decoding the checkpoint of job %s: %w", h.id, err)
	}
	return true, nil
}

// SaveCheckpoint records v (encoded as JSON) as the point the next run
// resumes from; nil clears it. It is written to disk within 10 seconds
// (and at the next state change or Close), so a run must be able to
// resume from an older checkpoint after a crash.
func (h *Handle) SaveCheckpoint(v any) error {
	data, err := marshalDoc(v)
	if err != nil {
		return fmt.Errorf("jobs: encoding the checkpoint of job %s: %w", h.id, err)
	}
	return h.update(func(j *Job) bool {
		j.Checkpoint = data
		return true
	})
}

// SetStatus records v (encoded as JSON) as the job's progress report
// (Job.Status); nil clears it. It is written to disk like checkpoints.
func (h *Handle) SetStatus(v any) error {
	data, err := marshalDoc(v)
	if err != nil {
		return fmt.Errorf("jobs: encoding the status of job %s: %w", h.id, err)
	}
	return h.update(func(j *Job) bool {
		if bytes.Equal(j.Status, data) {
			return false
		}
		j.Status = data
		return true
	})
}

// SetPhase sets the job's phase (Job.Phase), e.g. "copying".
func (h *Handle) SetPhase(phase string) {
	_ = h.update(func(j *Job) bool {
		if j.Phase == phase {
			return false
		}
		j.Phase = phase
		return true
	})
}

// SetNextRun sets when the job runs again if this run returns ErrIdle
// (zero: only on Kick). A run starts with Job.NextRunAt cleared.
func (h *Handle) SetNextRun(t time.Time) {
	m := h.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if h.e.active != h {
		return
	}
	h.e.job.NextRunAt = t
	m.gen++
}

// StateDir returns the job's private directory, <Dir>/<id>, creating it on
// first use (mode 0700), for state too large for a checkpoint (e.g. a bbolt
// database). It is deleted with the job. Without Options.Dir it returns ""
// and ErrNoStateDir.
func (h *Handle) StateDir() (string, error) {
	if h.m.dir == "" {
		return "", ErrNoStateDir
	}
	d := stateDir(h.m.dir, h.id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("jobs: creating the state directory of job %s: %w", h.id, err)
	}
	return d, nil
}

// Log returns a logger that names the job and its kind.
func (h *Handle) Log() *slog.Logger { return h.log }

// update applies f to the job while this run is in progress; f reports
// whether it changed anything. Changes are written within flushInterval.
func (h *Handle) update(f func(*Job) bool) error {
	m := h.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if h.e.active != h {
		return errRunEnded
	}
	if !f(&h.e.job) {
		return nil
	}
	h.e.job.UpdatedAt = m.now()
	m.gen++
	m.saveLaterLocked(m.flushDelayLocked())
	return nil
}
