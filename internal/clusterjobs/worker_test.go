package clusterjobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

type testRunner struct {
	run func(context.Context, jobs.Job) error
}

func (r testRunner) Validate(_ context.Context, raw json.RawMessage) (json.RawMessage, map[string]string, error) {
	return raw, map[string]string{"source": "photos", "target": "backup"}, nil
}

func (r testRunner) Run(ctx context.Context, job jobs.Job) error { return r.run(ctx, job) }

func testService(c *memoryCatalog, leader func() bool, runner Runner) *Service {
	return &Service{catalog: c, runners: map[string]Runner{"copy": runner}, isLeader: leader,
		now: time.Now, workers: 1, active: map[string]activeRun{}}
}

func awaitJobState(t *testing.T, s *Service, id string, state jobs.State) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := s.Get(id)
		if err == nil && job.State == state {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	job, err := s.Get(id)
	t.Fatalf("job did not reach %s: %+v, %v", state, job, err)
	return jobs.Job{}
}

func TestSharedSchedulerSubmitsAndCompletesOnLeader(t *testing.T) {
	store := &memoryCatalog{records: map[string][]byte{}}
	var runs atomic.Int32
	runner := testRunner{run: func(context.Context, jobs.Job) error { runs.Add(1); return nil }}
	leader := testService(store, func() bool { return true }, runner)
	follower := testService(store, func() bool { return false }, runner)
	job, err := follower.Submit(context.Background(), "copy", []byte(`{"kind":"copy"}`), "alice")
	if err != nil || job.State != jobs.StateQueued || job.Labels["source"] != "photos" {
		t.Fatalf("committed submission: %+v, %v", job, err)
	}
	if err := follower.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("follower ran job: %d runs", got)
	}
	if err := leader.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitJobState(t, follower, job.ID, jobs.StateSucceeded)
	leader.wg.Wait()
	if got := runs.Load(); got != 1 {
		t.Fatalf("job ran %d times", got)
	}
	stored, _, err := leader.getRecord(context.Background(), job.ID)
	if err != nil || stored.RunID != "" {
		t.Fatalf("completed run still claimed: %+v, %v", stored, err)
	}
}

func TestNewLeaderFencesOldRunCompletion(t *testing.T) {
	store := &memoryCatalog{records: map[string][]byte{}}
	var aLeader, bLeader atomic.Bool
	aLeader.Store(true)
	started, release := make(chan struct{}), make(chan struct{})
	a := testService(store, aLeader.Load, testRunner{run: func(context.Context, jobs.Job) error {
		close(started)
		<-release // deliberately ignores cancellation, like a stuck I/O call
		return nil
	}})
	b := testService(store, bLeader.Load, testRunner{run: func(context.Context, jobs.Job) error {
		return jobs.Permanent(errors.New("new leader observed a permanent failure"))
	}})
	job, err := a.Submit(context.Background(), "copy", []byte(`{"kind":"copy"}`), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first leader did not start job")
	}
	old, _, err := a.getRecord(context.Background(), job.ID)
	if err != nil || old.RunID == "" {
		t.Fatalf("first leader claim: %+v, %v", old, err)
	}
	aLeader.Store(false)
	bLeader.Store(true)
	if err := b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitJobState(t, b, job.ID, jobs.StateFailed)
	close(release)
	a.wg.Wait()
	b.wg.Wait()
	final, err := a.Get(job.ID)
	if err != nil || final.State != jobs.StateFailed || final.LastError == "" {
		t.Fatalf("old run overwrote new leader: %+v, %v", final, err)
	}
}

func TestSubmissionRequiresAcknowledgedCatalogWrite(t *testing.T) {
	store := &memoryCatalog{records: map[string][]byte{}, writeErr: context.DeadlineExceeded}
	svc := testService(store, func() bool { return true }, testRunner{run: func(context.Context, jobs.Job) error { return nil }})
	if _, err := svc.Submit(context.Background(), "copy", []byte(`{"kind":"copy"}`), "alice"); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("unacknowledged job accepted: %v", err)
	}
	if len(store.records) != 0 {
		t.Fatalf("unacknowledged job persisted: %+v", store.records)
	}
}

func TestFollowerPauseCancelsLeaderRun(t *testing.T) {
	store := &memoryCatalog{records: map[string][]byte{}}
	started, stopped := make(chan struct{}), make(chan struct{})
	runner := testRunner{run: func(ctx context.Context, _ jobs.Job) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}}
	leader := testService(store, func() bool { return true }, runner)
	follower := testService(store, func() bool { return false }, runner)
	job, err := follower.Submit(context.Background(), "copy", []byte(`{"kind":"copy"}`), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("leader did not start job")
	}
	if err := follower.Pause(job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("leader kept running a job paused through another node")
	}
	leader.wg.Wait()
	if got, err := follower.Get(job.ID); err != nil || got.State != jobs.StatePaused {
		t.Fatalf("paused job state: %+v, %v", got, err)
	}
}

func TestSharedSchedulerDispatchesOldestQueuedJobFirst(t *testing.T) {
	old, _ := json.Marshal(jobs.Job{ID: "older", Kind: "copy", State: jobs.StateQueued, CreatedAt: time.Now().Add(-time.Hour)})
	newer, _ := json.Marshal(jobs.Job{ID: "newer", Kind: "copy", State: jobs.StateQueued, CreatedAt: time.Now()})
	store := &memoryCatalog{records: map[string][]byte{"newer": newer, "older": old}}
	started, release := make(chan string, 2), make(chan struct{})
	svc := testService(store, func() bool { return true }, testRunner{run: func(ctx context.Context, job jobs.Job) error {
		started <- job.ID
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		if id != "older" {
			t.Fatalf("first dispatched job %s; want older", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no queued job started")
	}
	close(release)
	svc.wg.Wait()
}

func TestRunProgressCommitsAndIsFencedAfterPause(t *testing.T) {
	store := &memoryCatalog{records: map[string][]byte{
		"copy_1": []byte(`{"id":"copy_1","kind":"copy","state":"running","clusterRunID":"run_1"}`),
	}}
	runner := testRunner{run: func(context.Context, jobs.Job) error { return nil }}
	leader := testService(store, func() bool { return true }, runner)
	follower := testService(store, func() bool { return false }, runner)
	if err := leader.updateRun(context.Background(), "copy_1", "run_1", "copying", []byte(`{"keysScanned":3}`), []byte(`{"lastKey":"k3"}`)); err != nil {
		t.Fatalf("commit progress: %v", err)
	}
	job, err := follower.Get("copy_1")
	if err != nil || job.Phase != "copying" || string(job.Checkpoint) != `{"lastKey":"k3"}` {
		t.Fatalf("shared progress: %+v, %v", job, err)
	}
	if err := follower.Pause("copy_1"); err != nil {
		t.Fatal(err)
	}
	if err := leader.updateRun(context.Background(), "copy_1", "run_1", "verifying", []byte(`{}`), []byte(`{}`)); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("stale progress update accepted after pause: %v", err)
	}
	job, err = leader.Get("copy_1")
	if err != nil || job.State != jobs.StatePaused || job.Phase != "copying" {
		t.Fatalf("paused checkpoint overwritten: %+v, %v", job, err)
	}
}
