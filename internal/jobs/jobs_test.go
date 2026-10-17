package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// testRunner is a Runner made of functions.
type testRunner struct {
	validate func(ctx context.Context, spec json.RawMessage) (json.RawMessage, map[string]string, error)
	run      func(ctx context.Context, h *Handle) error
}

func (r *testRunner) Validate(ctx context.Context, spec json.RawMessage) (json.RawMessage, map[string]string, error) {
	if r.validate == nil {
		return nil, nil, nil
	}
	return r.validate(ctx, spec)
}

func (r *testRunner) Run(ctx context.Context, h *Handle) error { return r.run(ctx, h) }

// testSpec is the spec used by most tests.
type testSpec struct {
	Mode string `json:"mode,omitempty"`
	Name string `json:"name,omitempty"`
}

func specOf(t *testing.T, h *Handle) testSpec {
	t.Helper()
	var s testSpec
	if err := h.Spec(&s); err != nil {
		t.Errorf("Spec: %v", err)
	}
	return s
}

type counter struct {
	N int `json:"n"`
}

func openManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	m, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return m
}

func submit(t *testing.T, m *Manager, kind string, spec any) Job {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Submit(context.Background(), kind, data, "tester")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return j
}

func getJob(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	j, err := m.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return j
}

func wantState(t *testing.T, m *Manager, id string, want State) Job {
	t.Helper()
	j := getJob(t, m, id)
	if j.State != want {
		t.Fatalf("job %s: state %s, want %s (lastError %q)", id, j.State, want, j.LastError)
	}
	return j
}

// readJobsFile reads and checks the jobs file in dir.
func readJobsFile(t *testing.T, dir string) map[string]record {
	t.Helper()
	recs, err := loadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if recs == nil {
		t.Fatal("no jobs file")
	}
	out := map[string]record{}
	for _, r := range recs {
		out[r.ID] = r
	}
	return out
}

func writeJobsFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		validate := func(_ context.Context, spec json.RawMessage) (json.RawMessage, map[string]string, error) {
			var s testSpec
			if err := json.Unmarshal(spec, &s); err != nil {
				return nil, nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
			}
			norm, _ := json.Marshal(map[string]any{"mode": s.Mode, "normalized": true})
			return norm, map[string]string{"source": "src", "mode": s.Mode}, nil
		}
		r1 := &testRunner{validate: validate, run: func(ctx context.Context, h *Handle) error {
			switch specOf(t, h).Mode {
			case "ok":
				h.SetPhase("done")
				if err := h.SetStatus(map[string]int{"copied": 3}); err != nil {
					t.Error(err)
				}
				return nil
			case "fail":
				return Permanent(errors.New("bad target"))
			case "idle":
				h.SetNextRun(time.Now().Add(time.Hour))
				return ErrIdle
			default: // "block", "pause"
				if err := h.SaveCheckpoint(counter{7}); err != nil {
					t.Error(err)
				}
				<-ctx.Done()
				return ctx.Err()
			}
		}}
		m := openManager(t, Options{Dir: dir, Workers: 8, Runners: map[string]Runner{"test": r1}})
		ids := map[string]string{}
		for _, mode := range []string{"ok", "fail", "idle", "block", "pause"} {
			ids[mode] = submit(t, m, "test", testSpec{Mode: mode}).ID
			time.Sleep(time.Second) // distinct creation times
		}
		synctest.Wait()
		if err := m.Pause(ids["pause"]); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		wantState(t, m, ids["ok"], StateSucceeded)
		wantState(t, m, ids["fail"], StateFailed)
		wantState(t, m, ids["idle"], StateWaiting)
		wantState(t, m, ids["block"], StateRunning)
		wantState(t, m, ids["pause"], StatePaused)
		before := map[string]Job{}
		for _, j := range m.List(Filter{}) {
			before[j.ID] = j
		}
		if err := m.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		recs := readJobsFile(t, dir)
		if len(recs) != 5 {
			t.Fatalf("jobs file has %d jobs, want 5", len(recs))
		}
		if rec := recs[ids["block"]]; rec.State != StateQueued || string(rec.Checkpoint) != `{"n":7}` {
			t.Fatalf("running job saved as %s with checkpoint %s, want queued with {\"n\":7}", rec.State, rec.Checkpoint)
		}

		var restored atomic.Int32
		var idleRuns atomic.Int32
		r2 := &testRunner{validate: validate, run: func(ctx context.Context, h *Handle) error {
			switch specOf(t, h).Mode {
			case "idle":
				idleRuns.Add(1)
				h.SetNextRun(time.Now().Add(time.Hour))
				return ErrIdle
			case "block":
				var cp counter
				if ok, err := h.Checkpoint(&cp); !ok || err != nil {
					t.Errorf("Checkpoint after restart: %v, %v", ok, err)
				}
				restored.Store(int32(cp.N))
				return nil
			}
			t.Errorf("unexpected run of %s", h.Job().ID)
			return nil
		}}
		m2 := openManager(t, Options{Dir: dir, Workers: 8, Runners: map[string]Runner{"test": r2}})
		defer m2.Close()
		synctest.Wait()
		for _, mode := range []string{"ok", "fail", "idle", "pause"} {
			got, want := getJob(t, m2, ids[mode]), before[ids[mode]]
			if jsonOf(t, got) != jsonOf(t, want) {
				t.Errorf("%s job after restart:\n got %s\nwant %s", mode, jsonOf(t, got), jsonOf(t, want))
			}
		}
		ok := getJob(t, m2, ids["ok"])
		if ok.Phase != "done" || string(ok.Status) != `{"copied":3}` || ok.CreatedBy != "tester" ||
			ok.Labels["source"] != "src" || string(ok.Spec) != `{"mode":"ok","normalized":true}` {
			t.Errorf("restored job lost fields: %+v", ok)
		}
		if j := wantState(t, m2, ids["block"], StateSucceeded); restored.Load() != 7 || j.Checkpoint == nil {
			t.Errorf("interrupted job resumed from checkpoint %d, want 7", restored.Load())
		}
		if idleRuns.Load() != 0 {
			t.Fatal("waiting job ran before its NextRunAt after the restart")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if idleRuns.Load() != 1 {
			t.Fatalf("waiting job ran %d times after its NextRunAt, want 1", idleRuns.Load())
		}
	})
}

func TestOpenRefusesBadFiles(t *testing.T) {
	for name, content := range map[string]string{
		"version":   `{"version":2,"jobs":[]}`,
		"noVersion": `{"jobs":[]}`,
		"corrupt":   `{"version":1,"jobs":[`,
		"state":     `{"version":1,"jobs":[{"id":"abc","kind":"test","state":"exploded","spec":{}}]}`,
		"id":        `{"version":1,"jobs":[{"id":"../x","kind":"test","state":"paused","spec":{}}]}`,
		"duplicate": `{"version":1,"jobs":[{"id":"abc","kind":"test","state":"paused"},{"id":"abc","kind":"test","state":"paused"}]}`,
		"kind":      `{"version":1,"jobs":[{"id":"abc","state":"paused"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeJobsFile(t, dir, content)
			m, err := Open(Options{Dir: dir, Logger: slog.New(slog.DiscardHandler)})
			if err == nil {
				m.Close()
				t.Fatal("Open accepted the file")
			}
			if name == "version" && !strings.Contains(err.Error(), "version 2") {
				t.Errorf("error %q does not name the version", err)
			}
		})
	}
}

func TestOpenToleratesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	writeJobsFile(t, dir, `{"version":1,"future":true,"jobs":[
		{"id":"abc","kind":"test","state":"paused","spec":{"x":1},"createdAt":"2026-01-01T00:00:00Z","newField":{"a":1}},
		{"id":"def","kind":"gone","state":"queued","spec":{},"createdAt":"2026-01-01T00:00:00Z"}]}`)
	m := openManager(t, Options{Dir: dir, Runners: map[string]Runner{"test": &testRunner{run: func(context.Context, *Handle) error { return nil }}}})
	defer m.Close()
	wantState(t, m, "abc", StatePaused)
	// A job whose kind has no runner is kept but never runs.
	wantState(t, m, "def", StateQueued)
	if err := m.Resume("def"); err != nil {
		t.Fatalf("Resume of a queued job: %v", err)
	}
	if err := m.Pause("def"); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume("def"); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Resume of a job without runner: %v, want ErrUnknownKind", err)
	}
	if err := m.Cancel("def"); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedJobResumesFromCheckpoint(t *testing.T) {
	// A crash leaves the job "running" in the file.
	dir := t.TempDir()
	writeJobsFile(t, dir, `{"version":1,"jobs":[{"id":"crashed","kind":"test","state":"running","spec":{},
		"checkpoint":{"n":5},"createdAt":"2026-01-01T00:00:00Z","startedAt":"2026-01-01T00:00:01Z"}]}`)
	got := make(chan int, 1)
	m := openManager(t, Options{Dir: dir, Runners: map[string]Runner{"test": &testRunner{run: func(ctx context.Context, h *Handle) error {
		var cp counter
		if _, err := h.Checkpoint(&cp); err != nil {
			t.Error(err)
		}
		got <- cp.N
		return nil
	}}}})
	defer m.Close()
	select {
	case n := <-got:
		if n != 5 {
			t.Fatalf("checkpoint %d, want 5", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted job did not run")
	}
}

func TestPauseResumeCancelDelete(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		release := make(chan struct{})
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			runs.Add(1)
			var cp counter
			if _, err := h.Checkpoint(&cp); err != nil {
				t.Error(err)
			}
			if err := h.SaveCheckpoint(counter{cp.N + 1}); err != nil {
				t.Error(err)
			}
			if _, err := h.StateDir(); err != nil {
				t.Error(err)
			}
			if specOf(t, h).Mode == "stubborn" {
				<-release // ignores ctx until released
			}
			<-ctx.Done()
			return ctx.Err()
		}}
		m := openManager(t, Options{Dir: dir, Workers: 1, Runners: map[string]Runner{"test": r}})
		defer m.Close()
		a := submit(t, m, "test", testSpec{}).ID
		b := submit(t, m, "test", testSpec{}).ID
		synctest.Wait()
		wantState(t, m, a, StateRunning)
		wantState(t, m, b, StateQueued)

		// Pause and cancel a queued job.
		if err := m.Pause(b); err != nil {
			t.Fatal(err)
		}
		wantState(t, m, b, StatePaused)
		if err := m.Cancel(b); err != nil {
			t.Fatal(err)
		}
		if j := wantState(t, m, b, StateCanceled); j.FinishedAt.IsZero() {
			t.Error("canceled job has no FinishedAt")
		}

		// Pause a running job: its context is canceled, its checkpoint kept.
		if err := m.Pause(a); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		wantState(t, m, a, StatePaused)
		if err := m.Pause(a); err != nil {
			t.Fatalf("second Pause: %v", err)
		}
		if err := m.Resume(a); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		j := wantState(t, m, a, StateRunning)
		if runs.Load() != 2 || string(j.Checkpoint) != `{"n":2}` {
			t.Fatalf("after resume: %d runs, checkpoint %s; want 2 runs, {\"n\":2}", runs.Load(), j.Checkpoint)
		}
		if err := m.Resume(a); err != nil {
			t.Fatalf("Resume of a running job: %v", err)
		}
		if err := m.Delete(a); !errors.Is(err, ErrConflict) {
			t.Fatalf("Delete of a running job: %v, want ErrConflict", err)
		}

		// Cancel a running job.
		if err := m.Cancel(a); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		wantState(t, m, a, StateCanceled)
		if err := m.Cancel(a); err != nil {
			t.Fatalf("second Cancel: %v", err)
		}
		for name, op := range map[string]func(string) error{"Pause": m.Pause, "Resume": m.Resume} {
			if err := op(a); !errors.Is(err, ErrConflict) {
				t.Errorf("%s of a canceled job: %v, want ErrConflict", name, err)
			}
		}

		// Delete removes the job and its state directory.
		if _, err := os.Stat(filepath.Join(dir, a)); err != nil {
			t.Fatalf("state directory: %v", err)
		}
		if err := m.Delete(a); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, a)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("state directory after Delete: %v", err)
		}
		if _, err := m.Get(a); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get after Delete: %v", err)
		}
		if err := m.Delete(a); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second Delete: %v", err)
		}
		if _, ok := readJobsFile(t, dir)[a]; ok {
			t.Fatal("deleted job still in the jobs file")
		}

		// Resume while a pause is pending: queued again when the run returns.
		c := submit(t, m, "test", testSpec{Mode: "stubborn"}).ID
		synctest.Wait()
		runsBefore := runs.Load()
		if err := m.Pause(c); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		wantState(t, m, c, StateRunning) // still stopping
		if err := m.Resume(c); err != nil {
			t.Fatal(err)
		}
		close(release)
		synctest.Wait()
		wantState(t, m, c, StateRunning)
		if runs.Load() != runsBefore+1 {
			t.Fatalf("job ran %d more times, want 1", runs.Load()-runsBefore)
		}

		// Cancel while being paused wins; pausing a job being canceled is
		// a conflict.
		if err := m.Pause(c); err != nil {
			t.Fatal(err)
		}
		if err := m.Cancel(c); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		wantState(t, m, c, StateCanceled)
	})
}

func TestRetryBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var starts []time.Time
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			mu.Lock()
			starts = append(starts, time.Now())
			n := len(starts)
			mu.Unlock()
			if n <= 5 {
				return fmt.Errorf("transient %d", n)
			}
			return nil
		}}
		m := openManager(t, Options{Runners: map[string]Runner{"test": r}, RetryMin: time.Second, RetryMax: 4 * time.Second})
		defer m.Close()
		id := submit(t, m, "test", testSpec{}).ID
		synctest.Wait()
		j := wantState(t, m, id, StateWaiting)
		mu.Lock()
		first := starts[0]
		mu.Unlock()
		if j.Failures != 1 || j.LastError != "transient 1" || !j.NextRunAt.Equal(first.Add(time.Second)) {
			t.Fatalf("after the first failure: failures %d, lastError %q, nextRunAt %v", j.Failures, j.LastError, j.NextRunAt)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		j = wantState(t, m, id, StateSucceeded)
		if j.Failures != 0 || j.LastError != "" {
			t.Errorf("succeeded job keeps failures %d, lastError %q", j.Failures, j.LastError)
		}
		mu.Lock()
		defer mu.Unlock()
		var gaps []time.Duration
		for i := 1; i < len(starts); i++ {
			gaps = append(gaps, starts[i].Sub(starts[i-1]))
		}
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
		if !slices.Equal(gaps, want) {
			t.Fatalf("retry gaps %v, want %v", gaps, want)
		}
	})
}

func TestKickRetriesNow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			if runs.Add(1) == 1 {
				return errors.New("transient")
			}
			return nil
		}}
		m := openManager(t, Options{Runners: map[string]Runner{"test": r}})
		defer m.Close()
		id := submit(t, m, "test", testSpec{}).ID
		synctest.Wait()
		wantState(t, m, id, StateWaiting)
		m.Kick(id)
		synctest.Wait()
		wantState(t, m, id, StateSucceeded)
	})
}

func TestPermanentFailure(t *testing.T) {
	base := errors.New("bad spec")
	perm := Permanent(base)
	if !errors.Is(perm, ErrPermanent) || !errors.Is(perm, base) || perm.Error() != "bad spec" || Permanent(nil) != nil {
		t.Fatalf("Permanent(%v) = %v", base, perm)
	}
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			runs.Add(1)
			switch specOf(t, h).Mode {
			case "permanent":
				if runs.Load() > 2 {
					return nil // fixed: a resumed run succeeds
				}
				return Permanent(base)
			case "wrapped":
				return fmt.Errorf("%w: target missing", ErrPermanent)
			default:
				panic("runner bug")
			}
		}}
		m := openManager(t, Options{Workers: 1, Runners: map[string]Runner{"test": r}})
		defer m.Close()
		p := submit(t, m, "test", testSpec{Mode: "permanent"}).ID
		w := submit(t, m, "test", testSpec{Mode: "wrapped"}).ID
		synctest.Wait()
		if j := wantState(t, m, p, StateFailed); j.LastError != "bad spec" || j.Failures != 1 || j.FinishedAt.IsZero() {
			t.Fatalf("failed job: %+v", j)
		}
		wantState(t, m, w, StateFailed)
		time.Sleep(time.Hour)
		synctest.Wait()
		if runs.Load() != 2 {
			t.Fatalf("failed jobs ran %d times, want 2 (no retries)", runs.Load())
		}
		pn := submit(t, m, "test", testSpec{Mode: "panic"}).ID
		synctest.Wait()
		if j := wantState(t, m, pn, StateFailed); !strings.Contains(j.LastError, "panicked") {
			t.Fatalf("panicking job: lastError %q", j.LastError)
		}
		// Resume retries a failed job.
		if err := m.Resume(p); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if j := wantState(t, m, p, StateSucceeded); j.Failures != 0 || j.LastError != "" {
			t.Fatalf("resumed job: %+v", j)
		}
		if err := m.Resume(p); !errors.Is(err, ErrConflict) {
			t.Fatalf("Resume of a succeeded job: %v, want ErrConflict", err)
		}
	})
}

func TestWorkerLimitAndOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n = 6
		release := map[string]chan struct{}{}
		for i := range n {
			release[fmt.Sprint(i)] = make(chan struct{})
		}
		var mu sync.Mutex
		var started []string
		var active, peak atomic.Int32
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			name := specOf(t, h).Name
			cur := active.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			mu.Lock()
			started = append(started, name)
			mu.Unlock()
			<-release[name]
			active.Add(-1)
			return nil
		}}
		m := openManager(t, Options{Workers: 2, Runners: map[string]Runner{"test": r}})
		defer m.Close()
		released := map[string]bool{}
		finish := func(name string) {
			released[name] = true
			close(release[name])
		}
		defer func() { // unblock the runs so that Close returns after a failure
			for name := range release {
				if !released[name] {
					finish(name)
				}
			}
		}()
		for i := range n {
			submit(t, m, "test", testSpec{Name: fmt.Sprint(i)})
		}
		// Each step frees one worker, so exactly the next job in submission
		// order must start. (Two runs started together append in either
		// order: compare sets.)
		check := func(want ...string) {
			t.Helper()
			synctest.Wait()
			mu.Lock()
			got := slices.Sorted(slices.Values(started))
			mu.Unlock()
			if !slices.Equal(got, want) {
				t.Fatalf("started %v, want %v", got, want)
			}
			if running := len(m.List(Filter{State: StateRunning})); running > 2 {
				t.Fatalf("%d jobs running with 2 workers", running)
			}
		}
		check("0", "1")
		finish("1")
		check("0", "1", "2")
		finish("0")
		check("0", "1", "2", "3")
		finish("3")
		check("0", "1", "2", "3", "4")
		finish("2")
		check("0", "1", "2", "3", "4", "5")
		finish("4")
		finish("5")
		synctest.Wait()
		if peak.Load() != 2 {
			t.Fatalf("peak concurrency %d, want 2", peak.Load())
		}
		if got := len(m.List(Filter{State: StateSucceeded})); got != n {
			t.Fatalf("%d jobs succeeded, want %d", got, n)
		}
	})
}

func TestContinuousJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		var blockRun atomic.Int32
		gate := make(chan struct{})
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			n := runs.Add(1)
			if n == blockRun.Load() {
				<-gate
			}
			if specOf(t, h).Mode != "kickOnly" {
				h.SetNextRun(time.Now().Add(time.Hour))
			}
			return fmt.Errorf("pass %d done: %w", n, ErrIdle)
		}}
		m := openManager(t, Options{Runners: map[string]Runner{"test": r}})
		defer m.Close()
		id := submit(t, m, "test", testSpec{}).ID
		synctest.Wait()
		start := time.Now()
		j := wantState(t, m, id, StateWaiting)
		if runs.Load() != 1 || !j.NextRunAt.Equal(start.Add(time.Hour)) || j.LastError != "" {
			t.Fatalf("after the first pass: %d runs, %+v", runs.Load(), j)
		}

		// Kick runs it now.
		m.Kick(id)
		synctest.Wait()
		wantState(t, m, id, StateWaiting)
		if runs.Load() != 2 {
			t.Fatalf("after Kick: %d runs, want 2", runs.Load())
		}

		// NextRunAt runs it (a timer, no polling).
		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		if runs.Load() != 2 {
			t.Fatalf("ran before NextRunAt: %d runs", runs.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if runs.Load() != 3 {
			t.Fatalf("at NextRunAt: %d runs, want 3", runs.Load())
		}

		// A Kick during a run is not lost: the job runs again right away.
		blockRun.Store(4)
		m.Kick(id)
		synctest.Wait()
		wantState(t, m, id, StateRunning)
		m.Kick(id)
		close(gate)
		synctest.Wait()
		if runs.Load() != 5 {
			t.Fatalf("after a Kick during a run: %d runs, want 5", runs.Load())
		}
		wantState(t, m, id, StateWaiting)

		// SetNextRun reschedules a waiting job.
		m.SetNextRun(id, time.Now().Add(time.Minute))
		time.Sleep(time.Minute)
		synctest.Wait()
		if runs.Load() != 6 {
			t.Fatalf("after SetNextRun: %d runs, want 6", runs.Load())
		}

		// Pausing a waiting job stops its timer.
		if err := m.Pause(id); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Hour)
		m.Kick(id) // no effect on a paused job
		synctest.Wait()
		if runs.Load() != 6 {
			t.Fatalf("paused job ran: %d runs", runs.Load())
		}
		if err := m.Resume(id); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if runs.Load() != 7 {
			t.Fatalf("after Resume: %d runs, want 7", runs.Load())
		}
		if err := m.Cancel(id); err != nil {
			t.Fatal(err)
		}
		wantState(t, m, id, StateCanceled)

		// Without NextRunAt a waiting job waits for Kick only.
		k := submit(t, m, "test", testSpec{Mode: "kickOnly"}).ID
		synctest.Wait()
		if j := wantState(t, m, k, StateWaiting); !j.NextRunAt.IsZero() || runs.Load() != 8 {
			t.Fatalf("kick-only job: %d runs, NextRunAt %v", runs.Load(), j.NextRunAt)
		}
		time.Sleep(1000 * time.Hour)
		synctest.Wait()
		if runs.Load() != 8 {
			t.Fatalf("kick-only job ran without a Kick: %d runs", runs.Load())
		}
		m.Kick(k)
		synctest.Wait()
		if runs.Load() != 9 {
			t.Fatalf("Kick of a kick-only job: %d runs, want 9", runs.Load())
		}
	})
}

func TestHistoryPruning(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			d, err := h.StateDir()
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(d, "state"), []byte("x"), 0o600)
		}}
		m := openManager(t, Options{Dir: dir, Workers: 1, History: 3, HistoryAge: time.Hour, Runners: map[string]Runner{"test": r}})
		var ids []string
		for range 5 {
			ids = append(ids, submit(t, m, "test", testSpec{}).ID)
			synctest.Wait()
			time.Sleep(time.Second)
		}
		var kept []string
		for _, j := range m.List(Filter{}) {
			kept = append(kept, j.ID)
		}
		if want := []string{ids[4], ids[3], ids[2]}; !slices.Equal(kept, want) {
			t.Fatalf("kept %v, want the newest %v", kept, want)
		}
		for i, id := range ids {
			_, err := os.Stat(filepath.Join(dir, id))
			if exists := err == nil; exists != (i >= 2) {
				t.Errorf("job %d: state directory exists %v", i, exists)
			}
		}

		// Expiry by age, by timer.
		time.Sleep(time.Hour)
		synctest.Wait()
		if jobs := m.List(Filter{}); len(jobs) != 0 {
			t.Fatalf("%d jobs left after HistoryAge", len(jobs))
		}
		for _, id := range ids {
			if _, err := os.Stat(filepath.Join(dir, id)); err == nil {
				t.Errorf("state directory of %s kept", id)
			}
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		if recs := readJobsFile(t, dir); len(recs) != 0 {
			t.Fatalf("jobs file keeps %d pruned jobs", len(recs))
		}

		// Open prunes old jobs and deletes orphaned state directories.
		old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
		writeJobsFile(t, dir, `{"version":1,"jobs":[
			{"id":"old","kind":"test","state":"succeeded","spec":{},"createdAt":"`+old+`","finishedAt":"`+old+`"},
			{"id":"fresh","kind":"test","state":"failed","spec":{},"createdAt":"`+old+`","finishedAt":"`+time.Now().UTC().Format(time.RFC3339)+`"}]}`)
		for _, d := range []string{"old", "fresh", "orphan", "Not.A.Job"} {
			if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		m2 := openManager(t, Options{Dir: dir, HistoryAge: time.Hour, Runners: map[string]Runner{"test": r}})
		defer m2.Close()
		if _, err := m2.Get("old"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired job loaded: %v", err)
		}
		wantState(t, m2, "fresh", StateFailed)
		for d, want := range map[string]bool{"old": false, "orphan": false, "fresh": true, "Not.A.Job": true} {
			_, err := os.Stat(filepath.Join(dir, d))
			if exists := err == nil; exists != want {
				t.Errorf("directory %s exists %v, want %v", d, exists, want)
			}
		}
	})
}

func TestCheckpointWriteThrottle(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		step := make(chan int)
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			for {
				select {
				case n := <-step:
					if err := h.SaveCheckpoint(counter{n}); err != nil {
						t.Error(err)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}}
		m := openManager(t, Options{Dir: dir, Runners: map[string]Runner{"test": r}})
		defer m.Close()
		id := submit(t, m, "test", testSpec{}).ID
		time.Sleep(time.Millisecond)
		synctest.Wait()
		checkpoint := func() string {
			t.Helper()
			synctest.Wait()
			return string(readJobsFile(t, dir)[id].Checkpoint)
		}
		if rec := readJobsFile(t, dir)[id]; rec.State != StateRunning {
			t.Fatalf("saved state %s, want running", rec.State)
		}
		step <- 1
		if cp := checkpoint(); cp != "" {
			t.Fatalf("checkpoint written at once: %s", cp)
		}
		time.Sleep(flushInterval / 2)
		if cp := checkpoint(); cp != "" {
			t.Fatalf("checkpoint written before the flush interval: %s", cp)
		}
		time.Sleep(flushInterval/2 + time.Second)
		if cp := checkpoint(); cp != `{"n":1}` {
			t.Fatalf("checkpoint after the flush interval: %q", cp)
		}
		step <- 2
		step <- 3
		if cp := checkpoint(); cp != `{"n":1}` {
			t.Fatalf("checkpoint written within the interval: %s", cp)
		}
		// A state change writes pending checkpoints with it.
		if err := m.Pause(id); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if rec := readJobsFile(t, dir)[id]; rec.State != StatePaused || string(rec.Checkpoint) != `{"n":3}` {
			t.Fatalf("after Pause: state %s, checkpoint %s", rec.State, rec.Checkpoint)
		}
	})
}

func TestStaleHandle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var h1 *Handle
		var mu sync.Mutex
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			mu.Lock()
			first := h1 == nil
			if first {
				h1 = h
			}
			mu.Unlock()
			if first {
				return errors.New("retry")
			}
			<-ctx.Done()
			return ctx.Err()
		}}
		m := openManager(t, Options{Runners: map[string]Runner{"test": r}})
		defer m.Close()
		id := submit(t, m, "test", testSpec{}).ID
		synctest.Wait()
		m.Kick(id)
		synctest.Wait()
		wantState(t, m, id, StateRunning)
		mu.Lock()
		h := h1
		mu.Unlock()
		if err := h.SaveCheckpoint(counter{1}); err == nil {
			t.Error("SaveCheckpoint of an ended run accepted")
		}
		if err := h.SetStatus(counter{1}); err == nil {
			t.Error("SetStatus of an ended run accepted")
		}
		h.SetPhase("stale")
		h.SetNextRun(time.Now().Add(time.Hour))
		j := h.Job()
		if j.ID != id || j.Checkpoint != nil || j.Status != nil || j.Phase != "" || !j.NextRunAt.IsZero() {
			t.Fatalf("a stale handle changed the job: %+v", j)
		}
		if _, err := h.StateDir(); !errors.Is(err, ErrNoStateDir) {
			t.Fatalf("StateDir in memory: %v, want ErrNoStateDir", err)
		}
	})
}

func TestSubmitAndList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errNoBucket := errors.New("no such bucket")
		r := &testRunner{
			validate: func(_ context.Context, spec json.RawMessage) (json.RawMessage, map[string]string, error) {
				var s testSpec
				if err := json.Unmarshal(spec, &s); err != nil {
					return nil, nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
				}
				if s.Name == "missing" {
					return nil, nil, errNoBucket
				}
				return nil, map[string]string{"source": s.Name, "target": "t-" + s.Name}, nil
			},
			run: func(ctx context.Context, h *Handle) error {
				if specOf(t, h).Mode == "fail" {
					return Permanent(errors.New("x"))
				}
				return nil
			},
		}
		other := &testRunner{run: func(ctx context.Context, h *Handle) error { <-ctx.Done(); return ctx.Err() }}
		m := openManager(t, Options{Runners: map[string]Runner{"copy": r, "snap": other}})
		ctx := context.Background()
		if _, err := m.Submit(ctx, "nope", json.RawMessage(`{}`), ""); !errors.Is(err, ErrUnknownKind) {
			t.Fatalf("unknown kind: %v", err)
		}
		if _, err := m.Submit(ctx, "copy", json.RawMessage(`{"name":`), ""); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("invalid JSON: %v", err)
		}
		if _, err := m.Submit(ctx, "copy", nil, ""); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("empty spec: %v", err)
		}
		if _, err := m.Submit(ctx, "copy", json.RawMessage(`{"name":1}`), ""); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("Validate's invalid-spec error: %v", err)
		}
		if _, err := m.Submit(ctx, "copy", json.RawMessage(`{"name":"missing"}`), ""); err != errNoBucket {
			t.Fatalf("Validate's error not returned unchanged: %v", err)
		}
		a := submit(t, m, "copy", testSpec{Name: "a"})
		if string(a.Spec) != `{"name":"a"}` || a.Labels["source"] != "a" || a.CreatedBy != "tester" || a.Status != nil ||
			(a.State != StateQueued && a.State != StateRunning) {
			t.Fatalf("submitted job: %+v", a)
		}
		if len(a.ID) != 26 || !validID(a.ID) {
			t.Fatalf("job ID %q", a.ID)
		}
		a.Labels["source"] = "changed" // copies only
		time.Sleep(time.Second)
		b := submit(t, m, "copy", testSpec{Name: "b", Mode: "fail"})
		time.Sleep(time.Second)
		c := submit(t, m, "snap", testSpec{Name: "a"})
		synctest.Wait()

		ids := func(js []Job) []string {
			var out []string
			for _, j := range js {
				out = append(out, j.ID)
			}
			return out
		}
		for name, tc := range map[string]struct {
			f    Filter
			want []string
		}{
			"all":     {Filter{}, []string{c.ID, b.ID, a.ID}},
			"kind":    {Filter{Kind: "copy"}, []string{b.ID, a.ID}},
			"state":   {Filter{State: StateFailed}, []string{b.ID}},
			"labels":  {Filter{Labels: map[string]string{"source": "a", "target": "t-a"}}, []string{a.ID}},
			"nolabel": {Filter{Labels: map[string]string{"source": "zzz"}}, nil},
			"match": {Filter{Match: func(j Job) bool {
				return j.Labels["source"] == "b" || j.Kind == "snap"
			}}, []string{c.ID, b.ID}},
		} {
			if got := ids(m.List(tc.f)); !slices.Equal(got, tc.want) {
				t.Errorf("%s: List = %v, want %v", name, got, tc.want)
			}
		}
		if j := getJob(t, m, a.ID); j.Labels["source"] != "a" {
			t.Fatal("a returned job shares its labels with the Manager")
		}
		if _, err := m.Get("nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get unknown: %v", err)
		}
		for name, op := range map[string]func(string) error{"Pause": m.Pause, "Resume": m.Resume, "Cancel": m.Cancel, "Delete": m.Delete} {
			if err := op("nope"); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s unknown: %v", name, err)
			}
		}
		m.Kick("nope")
		m.SetNextRun("nope", time.Now())

		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if _, err := m.Submit(ctx, "copy", json.RawMessage(`{}`), ""); !errors.Is(err, ErrClosed) {
			t.Fatalf("Submit after Close: %v", err)
		}
		if err := m.Pause(c.ID); !errors.Is(err, ErrClosed) {
			t.Fatalf("Pause after Close: %v", err)
		}
		// The running job was stopped by Close and is queued for the next
		// Open; Get still works.
		wantState(t, m, c.ID, StateQueued)
	})
}

func TestInMemory(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: &logs, mu: &mu}, nil))
	done := make(chan string, 1)
	m := openManager(t, Options{Logger: logger, Runners: map[string]Runner{"test": &testRunner{run: func(ctx context.Context, h *Handle) error {
		_, err := h.StateDir()
		if !errors.Is(err, ErrNoStateDir) {
			t.Errorf("StateDir: %v", err)
		}
		if err := h.SaveCheckpoint(counter{1}); err != nil {
			t.Error(err)
		}
		done <- h.Job().ID
		return nil
	}}}})
	defer m.Close()
	mu.Lock()
	warned := strings.Count(logs.String(), "level=WARN")
	mu.Unlock()
	if warned != 1 {
		t.Fatalf("%d warnings at Open, want 1", warned)
	}
	id := submit(t, m, "test", testSpec{}).ID
	select {
	case got := <-done:
		if got != id {
			t.Fatalf("ran %s, want %s", got, id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("job did not run")
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestConcurrency submits many jobs from many goroutines while others read
// and kick them; run it with -race.
func TestConcurrency(t *testing.T) {
	dir := t.TempDir()
	const workers, n = 3, 64
	var active, peak atomic.Int32
	r := &testRunner{
		validate: func(_ context.Context, spec json.RawMessage) (json.RawMessage, map[string]string, error) {
			var s testSpec
			err := json.Unmarshal(spec, &s)
			return nil, map[string]string{"name": s.Name}, err
		},
		run: func(ctx context.Context, h *Handle) error {
			cur := active.Add(1)
			defer active.Add(-1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			for i := range 3 {
				h.SetPhase(fmt.Sprint("step", i))
				if err := h.SetStatus(counter{i}); err != nil {
					return err
				}
				if err := h.SaveCheckpoint(counter{i}); err != nil {
					return err
				}
				time.Sleep(time.Millisecond)
			}
			if specOf(t, h).Mode == "idle" && h.Job().Failures == 0 {
				return ErrIdle
			}
			return nil
		},
	}
	m := openManager(t, Options{Dir: dir, Workers: workers, Runners: map[string]Runner{"test": r}})
	var wg sync.WaitGroup
	ids := make(chan string, n)
	for i := range n {
		wg.Go(func() {
			mode := ""
			if i%8 == 0 {
				mode = "idle"
			}
			j, err := m.Submit(context.Background(), "test", json.RawMessage(fmt.Sprintf(`{"name":"j%d","mode":%q}`, i, mode)), "")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- j.ID
			for _, x := range m.List(Filter{Labels: map[string]string{"name": fmt.Sprint("j", i)}}) {
				_ = x.Status
			}
		})
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, j := range m.List(Filter{}) {
					m.Kick(j.ID)
					if _, err := m.Get(j.ID); err != nil {
						t.Error(err)
					}
				}
				time.Sleep(100 * time.Microsecond)
			}
		})
	}
	wg.Wait()
	close(ids)
	// Every eighth job is continuous: it goes idle after each pass and the
	// readers keep kicking it. The others succeed.
	const continuous = n / 8
	waitCount := func(s State, want int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for len(m.List(Filter{State: s})) != want {
			if time.Now().After(deadline) {
				t.Fatalf("after 30s: %d jobs %s, want %d", len(m.List(Filter{State: s})), s, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitCount(StateSucceeded, n-continuous)
	close(stop)
	readers.Wait()
	waitCount(StateWaiting, continuous)
	if p := peak.Load(); p > workers {
		t.Fatalf("%d jobs ran at once with %d workers", p, workers)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	recs := readJobsFile(t, dir)
	if len(recs) != n {
		t.Fatalf("jobs file has %d jobs, want %d", len(recs), n)
	}
	for id := range ids {
		if _, ok := recs[id]; !ok {
			t.Fatalf("job %s missing from the jobs file", id)
		}
	}
}

func TestDeferStart(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		// Jobs from an earlier run: queued, interrupted, waiting with a
		// NextRunAt that has passed, waiting for a Kick, and paused.
		past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		writeJobsFile(t, dir, `{"version":1,"jobs":[
			{"id":"queued","kind":"test","state":"queued","seq":1,"spec":{},"createdAt":"`+past+`"},
			{"id":"interrupted","kind":"test","state":"running","seq":2,"spec":{},"createdAt":"`+past+`"},
			{"id":"due","kind":"test","state":"waiting","spec":{},"createdAt":"`+past+`","nextRunAt":"`+past+`"},
			{"id":"kicked","kind":"test","state":"waiting","spec":{},"createdAt":"`+past+`"},
			{"id":"paused","kind":"test","state":"paused","spec":{},"createdAt":"`+past+`"}]}`)
		var mu sync.Mutex
		var ran []string
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			mu.Lock()
			ran = append(ran, h.Job().ID)
			mu.Unlock()
			return nil
		}}
		m := openManager(t, Options{Dir: dir, Workers: 2, DeferStart: true, Runners: map[string]Runner{"test": r}})
		defer m.Close()
		fresh := submit(t, m, "test", testSpec{}).ID
		m.Kick("kicked")
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		mu.Lock()
		if len(ran) != 0 {
			t.Fatalf("ran before Start: %v", ran)
		}
		mu.Unlock()
		for id, want := range map[string]State{
			"queued": StateQueued, "interrupted": StateQueued, "due": StateWaiting,
			"kicked": StateQueued, "paused": StatePaused, fresh: StateQueued,
		} {
			wantState(t, m, id, want)
		}

		m.Start()
		m.Start() // idempotent
		time.Sleep(time.Second)
		synctest.Wait()
		mu.Lock()
		got := slices.Sorted(slices.Values(ran))
		mu.Unlock()
		if want := slices.Sorted(slices.Values([]string{"queued", "interrupted", "due", "kicked", fresh})); !slices.Equal(got, want) {
			t.Fatalf("ran after Start: %v, want %v", got, want)
		}
		for _, id := range []string{"queued", "interrupted", "due", "kicked", fresh} {
			wantState(t, m, id, StateSucceeded)
		}
		wantState(t, m, "paused", StatePaused)
	})
}

func TestDeferStartClose(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		r := &testRunner{run: func(ctx context.Context, h *Handle) error {
			runs.Add(1)
			return nil
		}}
		m := openManager(t, Options{Dir: dir, DeferStart: true, Runners: map[string]Runner{"test": r}})
		id := submit(t, m, "test", testSpec{}).ID
		if err := m.Close(); err != nil {
			t.Fatalf("Close before Start: %v", err)
		}
		m.Start() // no effect after Close
		time.Sleep(time.Second)
		synctest.Wait()
		if runs.Load() != 0 {
			t.Fatalf("%d runs after Close", runs.Load())
		}
		if rec := readJobsFile(t, dir)[id]; rec.State != StateQueued {
			t.Fatalf("saved state %s, want queued", rec.State)
		}
		// Without DeferStart the job runs at Open.
		m2 := openManager(t, Options{Dir: dir, Runners: map[string]Runner{"test": r}})
		defer m2.Close()
		synctest.Wait()
		wantState(t, m2, id, StateSucceeded)
	})
}

func TestStateHelpers(t *testing.T) {
	for _, s := range []State{StateQueued, StateRunning, StateWaiting, StatePaused} {
		if !s.Valid() || s.Finished() {
			t.Errorf("%s: valid %v finished %v", s, s.Valid(), s.Finished())
		}
	}
	for _, s := range []State{StateSucceeded, StateFailed, StateCanceled} {
		if !s.Valid() || !s.Finished() {
			t.Errorf("%s: valid %v finished %v", s, s.Valid(), s.Finished())
		}
	}
	if State("x").Valid() {
		t.Error("unknown state is valid")
	}
}
