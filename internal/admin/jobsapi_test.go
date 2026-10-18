package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// fakeCopyRunner is the runner of the "copy" and "mirror" kinds in the
// admin tests. Validate decodes the spec strictly (so a "dryRun" left in
// it fails) and labels the job with its buckets. A copy run blocks until
// release is closed (then it succeeds) or its context ends; a mirror run
// saves a checkpoint and goes idle.
type fakeCopyRunner struct {
	release chan struct{}

	mu    sync.Mutex
	specs []json.RawMessage // validated specs
	runs  map[string]int    // runs per job
}

// fakeCopySpec is the spec the fake runner accepts.
type fakeCopySpec struct {
	Kind   string `json:"kind"`
	Source struct {
		Bucket string `json:"bucket"`
		Prefix string `json:"prefix"`
	} `json:"source"`
	Target struct {
		Bucket string `json:"bucket"`
		Prefix string `json:"prefix"`
	} `json:"target"`
	Options map[string]any `json:"options"`
}

func newFakeCopyRunner() *fakeCopyRunner {
	return &fakeCopyRunner{release: make(chan struct{}), runs: map[string]int{}}
}

func decodeFakeSpec(spec json.RawMessage) (fakeCopySpec, error) {
	var s fakeCopySpec
	dec := json.NewDecoder(bytes.NewReader(spec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%w: %v", jobs.ErrInvalidSpec, err)
	}
	if s.Source.Bucket == s.Target.Bucket {
		return s, fmt.Errorf("%w: a bucket cannot be copied onto itself", jobs.ErrInvalidSpec)
	}
	return s, nil
}

func (r *fakeCopyRunner) Validate(_ context.Context, spec json.RawMessage) (json.RawMessage, map[string]string, error) {
	s, err := decodeFakeSpec(spec)
	if err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return nil, map[string]string{"source": s.Source.Bucket, "target": s.Target.Bucket}, nil
}

func (r *fakeCopyRunner) Run(ctx context.Context, h *jobs.Handle) error {
	j := h.Job()
	r.mu.Lock()
	r.runs[j.ID]++
	r.mu.Unlock()
	_ = h.SaveCheckpoint(map[string]string{"after": "secret-internal-key"})
	_ = h.SetStatus(map[string]int{"keysCopied": 1})
	if j.Kind == jobKindMirror {
		return jobs.ErrIdle
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}

func (r *fakeCopyRunner) runCount(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[id]
}

func (r *fakeCopyRunner) lastSpec() json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.specs) == 0 {
		return nil
	}
	return r.specs[len(r.specs)-1]
}

// fakeCopyJobs is the CopyJobService of the admin tests.
type fakeCopyJobs struct {
	mu    sync.Mutex
	specs []json.RawMessage
}

func (f *fakeCopyJobs) DryRun(_ context.Context, spec json.RawMessage) (json.RawMessage, []string, error) {
	if _, err := decodeFakeSpec(spec); err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	return json.RawMessage(`{"normalized":true}`), []string{"lifecycle is not copied"}, nil
}

// jobsEnv is a test environment with a job manager running the fake
// copy runner and the fake copy job service, and buckets src, dst and
// other.
type jobsEnv struct {
	*testEnv
	runner  *fakeCopyRunner
	copies  *fakeCopyJobs
	manager *jobs.Manager
}

func newJobsEnv(t *testing.T, opts ...envOption) *jobsEnv {
	t.Helper()
	runner := newFakeCopyRunner()
	// A clock that advances on every reading orders jobs created within
	// one tick of the system clock.
	var clockMu sync.Mutex
	clock := time.Now()
	m, err := jobs.Open(jobs.Options{
		Workers: 4,
		Runners: map[string]jobs.Runner{jobKindCopy: runner, jobKindMirror: runner},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			clock = clock.Add(time.Millisecond)
			return clock
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	copies := &fakeCopyJobs{}
	opts = append([]envOption{func(d *Deps) { d.Jobs, d.CopyJobs = m, copies }}, opts...)
	e := newEnv(t, opts...)
	for _, b := range []string{"src", "dst", "other"} {
		e.createBucket(b, storage.CreateBucketOptions{})
	}
	return &jobsEnv{testEnv: e, runner: runner, copies: copies, manager: m}
}

// copySpec returns a CopyJobSpec body.
func copySpec(kind, source, target string, extra ...func(map[string]any)) map[string]any {
	s := map[string]any{"kind": kind, "source": map[string]any{"bucket": source}, "target": map[string]any{"bucket": target}}
	for _, f := range extra {
		f(s)
	}
	return s
}

// waitJob waits until the job reaches state.
func (e *jobsEnv) waitJob(id string, state jobs.State) jobs.Job {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, err := e.manager.Get(id)
		if err != nil {
			e.t.Fatal(err)
		}
		if j.State == state {
			return j
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %s is %s, want %s", id, j.State, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *jobsEnv) createJob(body any) jobs.Job {
	e.t.Helper()
	var j jobs.Job
	r := e.root("POST", "/jobs", body)
	r.expectStatus(e.t, 201)
	r.json(e.t, &j)
	return j
}

// Creating a job needs admin:CreateCopyJob on both of its buckets; it is
// evaluated before the spec is validated.
func TestCreateJobAuthorization(t *testing.T) {
	e := newJobsEnv(t)
	e.putPolicy("src-only", map[string]any{"Effect": "Allow", "Action": "admin:CreateCopyJob", "Resource": "arn:gos3:admin:::bucket/src"})
	e.putPolicy("both", map[string]any{"Effect": "Allow", "Action": "admin:CreateCopyJob",
		"Resource": []string{"arn:gos3:admin:::bucket/src", "arn:gos3:admin:::bucket/dst"}})
	srcOnly := e.addUser("alice", "src-only")
	both := e.addUser("bob", "both")

	msg := e.do(&srcOnly, "POST", Prefix+"/jobs", copySpec("copy", "src", "dst")).expectError(t, 403, codeAccessDenied)
	if !strings.Contains(msg, "admin:CreateCopyJob") || !strings.Contains(msg, "bucket/dst") {
		t.Fatalf("message %q", msg)
	}
	e.do(&srcOnly, "POST", Prefix+"/jobs", copySpec("copy", "dst", "src")).expectError(t, 403, codeAccessDenied)
	// A dry run is authorized the same way.
	e.do(&srcOnly, "POST", Prefix+"/jobs", copySpec("copy", "src", "dst", func(s map[string]any) { s["dryRun"] = true })).
		expectError(t, 403, codeAccessDenied)
	// Authorization comes first: an invalid spec on other buckets is 403.
	e.do(&srcOnly, "POST", Prefix+"/jobs", copySpec("restic-snapshot", "src", "other")).expectError(t, 403, codeAccessDenied)

	var j jobs.Job
	r := e.do(&both, "POST", Prefix+"/jobs", copySpec("copy", "src", "dst"))
	r.expectStatus(t, 201)
	r.json(t, &j)
	if j.Kind != "copy" || j.CreatedBy != "bob" || j.Labels["source"] != "src" || j.Labels["target"] != "dst" {
		t.Fatalf("job = %+v", j)
	}
	if bytes.Contains(r.body, []byte("checkpoint")) {
		t.Fatalf("job shows its checkpoint: %s", r.body)
	}

	// Both readers of the spec see the same buckets: encoding/json takes
	// the last of differently cased duplicate members, and so does the
	// authorization (here "other", which bob may not use).
	body := `{"kind":"copy","source":{"bucket":"src"},"SOURCE":{"bucket":"other"},"target":{"bucket":"dst"}}`
	e.do(&both, "POST", Prefix+"/jobs", body).expectError(t, 403, codeAccessDenied)
	body = `{"kind":"copy","source":{"bucket":"src","BUCKET":"other"},"target":{"bucket":"dst"}}`
	e.do(&both, "POST", Prefix+"/jobs", body).expectError(t, 403, codeAccessDenied)
}

func TestCreateJobValidation(t *testing.T) {
	e := newJobsEnv(t)
	for _, tc := range []struct {
		body any
		want string
	}{
		{copySpec("restic-snapshot", "src", "dst"), "kind must be"},
		{copySpec("", "src", "dst"), "kind must be"},
		{map[string]any{"kind": "copy", "target": map[string]any{"bucket": "dst"}}, "source"},
		{map[string]any{"kind": "copy", "source": map[string]any{"bucket": "src"}}, "target"},
		{`{"kind":"copy","source":"src"}`, "invalid job spec"},
		{`[1]`, "JSON object"},
		{`null`, "JSON object"},
		{"", "required"},
		{copySpec("copy", "src", "src"), "onto itself"}, // the runner's validation
		{copySpec("copy", "src", "dst", func(s map[string]any) { s["bogus"] = 1 }), "unknown field \"bogus\""}, // ditto
	} {
		msg := e.root("POST", "/jobs", tc.body).expectError(t, 400, codeInvalidRequest)
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%v: message %q, want %q", tc.body, msg, tc.want)
		}
	}
	if n := len(e.manager.List(jobs.Filter{})); n != 0 {
		t.Fatalf("%d jobs created", n)
	}
}

// A dry run validates and normalizes the spec without creating a job;
// "dryRun" is never part of the spec the runner or the service gets.
func TestCreateJobDryRun(t *testing.T) {
	e := newJobsEnv(t)
	var out dryRunResponse
	e.root("POST", "/jobs", copySpec("copy", "src", "dst", func(s map[string]any) { s["dryRun"] = true })).json(t, &out)
	if string(out.Spec) != `{"normalized":true}` || len(out.Warnings) != 1 {
		t.Fatalf("dry run = %+v", out)
	}
	if n := len(e.manager.List(jobs.Filter{})); n != 0 {
		t.Fatalf("a dry run created %d jobs", n)
	}
	// Matched like encoding/json matches it: case-insensitively.
	r := e.root("POST", "/jobs", `{"DryRun":true,"kind":"mirror","source":{"bucket":"src"},"target":{"bucket":"dst"},"dryrun":true}`)
	r.expectStatus(t, 200)
	e.copies.mu.Lock()
	got := string(e.copies.specs[len(e.copies.specs)-1])
	e.copies.mu.Unlock()
	if got != `{"kind":"mirror","source":{"bucket":"src"},"target":{"bucket":"dst"}}` {
		t.Fatalf("dry run spec = %s", got)
	}
	// "dryRun": false creates the job, without the member.
	j := e.createJob(`{"kind":"copy", "dryRun":false, "source":{"bucket":"src"},"target":{"bucket":"dst"}}`)
	if s := string(e.runner.lastSpec()); s != `{"kind":"copy","source":{"bucket":"src"},"target":{"bucket":"dst"}}` {
		t.Fatalf("submitted spec = %s", s)
	}
	if string(j.Spec) != `{"kind":"copy","source":{"bucket":"src"},"target":{"bucket":"dst"}}` {
		t.Fatalf("job spec = %s", j.Spec)
	}
	e.root("POST", "/jobs", copySpec("copy", "src", "dst", func(s map[string]any) { s["dryRun"] = "yes" })).
		expectError(t, 400, codeInvalidRequest)

	// Without the copy job service, dry runs are not available.
	e.api.d.CopyJobs = nil
	e.root("POST", "/jobs", copySpec("copy", "src", "dst", func(s map[string]any) { s["dryRun"] = true })).
		expectError(t, 404, codeNotFound)
}

func TestListJobsFilters(t *testing.T) {
	e := newJobsEnv(t)
	cp := e.createJob(copySpec("copy", "src", "dst"))
	mi := e.createJob(copySpec("mirror", "other", "src"))
	e.waitJob(cp.ID, jobs.StateRunning)
	e.waitJob(mi.ID, jobs.StateWaiting)

	ids := func(path string) []string {
		t.Helper()
		var l jobList
		e.root("GET", path, nil).json(t, &l)
		out := []string{}
		for _, j := range l.Jobs {
			if j.Checkpoint != nil {
				t.Fatalf("%s: job %s shows its checkpoint", path, j.ID)
			}
			out = append(out, j.ID)
		}
		return out
	}
	check := func(path string, want ...string) {
		t.Helper()
		got := ids(path)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	check("/jobs", mi.ID, cp.ID) // newest first
	check("/jobs?kind=mirror", mi.ID)
	check("/jobs?kind=restic-snapshot")
	check("/jobs?state=waiting", mi.ID)
	check("/jobs?state=running", cp.ID)
	check("/jobs?bucket=src", mi.ID, cp.ID)
	check("/jobs?bucket=dst", cp.ID)
	check("/jobs?bucket=other&kind=copy")
	check("/buckets/dst/jobs", cp.ID)
	check("/buckets/src/jobs?kind=mirror", mi.ID)
	check("/buckets/nope/jobs")
	e.root("GET", "/jobs?state=done", nil).expectError(t, 400, codeInvalidRequest)
}

// The state rules of pause, resume, cancel, run and delete.
func TestJobStateRules(t *testing.T) {
	e := newJobsEnv(t)
	cp := e.createJob(copySpec("copy", "src", "dst"))
	e.waitJob(cp.ID, jobs.StateRunning)
	path := "/jobs/" + cp.ID

	// An unfinished job cannot be deleted.
	e.root("DELETE", path, nil).expectError(t, 409, codeConflict)
	// Pause a running job; it is paused once its run returns.
	e.root("POST", path+"/pause", nil).expectStatus(t, 200)
	e.waitJob(cp.ID, jobs.StatePaused)
	if msg := e.root("POST", path+"/run", nil).expectError(t, 409, codeConflict); !strings.Contains(msg, "resume") {
		t.Fatalf("run of a paused job: %q", msg)
	}
	var j jobs.Job
	e.root("POST", path+"/resume", nil).json(t, &j)
	if j.State != jobs.StateQueued && j.State != jobs.StateRunning {
		t.Fatalf("resumed job = %+v", j)
	}
	e.waitJob(cp.ID, jobs.StateRunning)
	e.root("POST", path+"/cancel", nil).expectStatus(t, 200)
	e.waitJob(cp.ID, jobs.StateCanceled)
	e.root("POST", path+"/pause", nil).expectError(t, 409, codeConflict)
	e.root("POST", path+"/run", nil).expectError(t, 409, codeConflict)
	e.root("DELETE", path, nil).expectStatus(t, 204)
	e.root("GET", path, nil).expectError(t, 404, codeNotFound)
	e.root("POST", path+"/resume", nil).expectError(t, 404, codeNotFound)

	// A succeeded job cannot be resumed or canceled.
	done := e.createJob(copySpec("copy", "src", "other"))
	close(e.runner.release)
	e.waitJob(done.ID, jobs.StateSucceeded)
	e.root("POST", "/jobs/"+done.ID+"/resume", nil).expectError(t, 409, codeConflict)
	e.root("POST", "/jobs/"+done.ID+"/cancel", nil).expectError(t, 409, codeConflict)

	// run starts a waiting mirror's next pass now.
	mi := e.createJob(copySpec("mirror", "other", "dst"))
	e.waitJob(mi.ID, jobs.StateWaiting)
	runs := e.runner.runCount(mi.ID)
	e.root("POST", "/jobs/"+mi.ID+"/run", nil).json(t, &j)
	if j.ID != mi.ID || j.Checkpoint != nil {
		t.Fatalf("run = %+v", j)
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.runner.runCount(mi.ID) == runs {
		if time.Now().After(deadline) {
			t.Fatal("run did not start the mirror's next pass")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.root("POST", "/jobs/"+mi.ID+"/bogus", nil).expectError(t, 404, codeNotFound)
	e.root("PUT", "/jobs/"+mi.ID+"/pause", nil).expectError(t, 405, codeMethodNotAllowed)
}

// GetJob and UpdateJob are evaluated on every bucket of the job; unknown
// jobs at service level.
func TestJobAuthorization(t *testing.T) {
	e := newJobsEnv(t)
	cp := e.createJob(copySpec("copy", "src", "dst"))
	e.putPolicy("src-jobs", map[string]any{"Effect": "Allow", "Action": []string{"admin:GetJob", "admin:UpdateJob"},
		"Resource": "arn:gos3:admin:::bucket/src"})
	e.putPolicy("all-jobs", map[string]any{"Effect": "Allow", "Action": []string{"admin:GetJob", "admin:UpdateJob"},
		"Resource": "arn:gos3:admin:::bucket/*"})
	e.putPolicy("list-jobs", map[string]any{"Effect": "Allow", "Action": "admin:ListJobs", "Resource": "*"})
	srcOnly := e.addUser("alice", "src-jobs")
	all := e.addUser("bob", "all-jobs")
	lister := e.addUser("carol", "list-jobs")

	e.do(&srcOnly, "GET", Prefix+"/jobs/"+cp.ID, nil).expectError(t, 403, codeAccessDenied)
	e.do(&srcOnly, "POST", Prefix+"/jobs/"+cp.ID+"/pause", nil).expectError(t, 403, codeAccessDenied)
	e.do(&srcOnly, "DELETE", Prefix+"/jobs/"+cp.ID, nil).expectError(t, 403, codeAccessDenied)
	// The bucket listing needs GetJob on that bucket, and shows the jobs
	// whose other buckets the caller may see too.
	var l jobList
	e.do(&srcOnly, "GET", Prefix+"/buckets/src/jobs", nil).json(t, &l)
	if len(l.Jobs) != 0 {
		t.Fatalf("bucket jobs shown without GetJob on the target: %+v", l)
	}
	l = jobList{}
	e.do(&all, "GET", Prefix+"/buckets/src/jobs", nil).json(t, &l)
	if len(l.Jobs) != 1 {
		t.Fatalf("bucket jobs = %+v", l)
	}
	e.do(&srcOnly, "GET", Prefix+"/buckets/dst/jobs", nil).expectError(t, 403, codeAccessDenied)
	e.do(&srcOnly, "GET", Prefix+"/jobs", nil).expectError(t, 403, codeAccessDenied)
	e.do(&lister, "GET", Prefix+"/jobs", nil).json(t, &l)
	if len(l.Jobs) != 1 {
		t.Fatalf("jobs = %+v", l)
	}
	// Bucket-scoped grants do not reveal which job IDs exist.
	e.do(&all, "GET", Prefix+"/jobs/nosuchjob", nil).expectError(t, 403, codeAccessDenied)
	e.root("GET", "/jobs/nosuchjob", nil).expectError(t, 404, codeNotFound)

	var j jobs.Job
	e.do(&all, "GET", Prefix+"/jobs/"+cp.ID, nil).json(t, &j)
	if j.ID != cp.ID {
		t.Fatalf("job = %+v", j)
	}
	e.do(&all, "POST", Prefix+"/jobs/"+cp.ID+"/cancel", nil).expectStatus(t, 200)
	e.waitJob(cp.ID, jobs.StateCanceled)
	e.do(&all, "DELETE", Prefix+"/jobs/"+cp.ID, nil).expectStatus(t, 204)
}

// plainRunner runs jobs without bucket labels: they go idle.
type plainRunner struct{}

func (plainRunner) Validate(context.Context, json.RawMessage) (json.RawMessage, map[string]string, error) {
	return nil, nil, nil
}
func (plainRunner) Run(context.Context, *jobs.Handle) error { return jobs.ErrIdle }

// A job's buckets are its source, target and bucket labels; a job without
// any is only reachable at service level.
func TestJobBuckets(t *testing.T) {
	j := jobs.Job{Labels: map[string]string{"bucket": "b", "source": "b", "target": "c", "other": "d"}}
	if got := jobBuckets(j); strings.Join(got, ",") != "b,c" {
		t.Fatalf("jobBuckets = %v", got)
	}

	e := newJobsEnv(t)
	m, err := jobs.Open(jobs.Options{Runners: map[string]jobs.Runner{"plain": plainRunner{}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	e.api.d.Jobs = m
	plain, err := m.Submit(context.Background(), "plain", json.RawMessage(`{}`), "root")
	if err != nil {
		t.Fatal(err)
	}
	e.putPolicy("all-buckets", map[string]any{"Effect": "Allow", "Action": "admin:GetJob", "Resource": "arn:gos3:admin:::bucket/*"})
	u := e.addUser("alice", "all-buckets")
	e.do(&u, "GET", Prefix+"/jobs/"+plain.ID, nil).expectError(t, 403, codeAccessDenied)
	e.root("GET", "/jobs/"+plain.ID, nil).expectStatus(t, 200)
}

// Without a job manager the endpoints say so (after authorization).
func TestJobsUnavailable(t *testing.T) {
	e := newEnv(t)
	nobody := e.addUser("nobody")
	e.root("GET", "/jobs", nil).expectError(t, 404, codeNotFound)
	e.root("GET", "/jobs/x", nil).expectError(t, 404, codeNotFound)
	e.root("POST", "/jobs", copySpec("copy", "a", "b")).expectError(t, 404, codeNotFound)
	e.root("GET", "/buckets/a/jobs", nil).expectError(t, 404, codeNotFound)
	e.do(&nobody, "GET", Prefix+"/jobs/x", nil).expectError(t, 403, codeAccessDenied)
	e.do(&nobody, "POST", Prefix+"/jobs/x/run", nil).expectError(t, 403, codeAccessDenied)
}

// A runner that labels a job with a bucket the request did not name gets
// the job canceled.
func TestCheckJobBuckets(t *testing.T) {
	e := newJobsEnv(t)
	j := e.createJob(copySpec("copy", "src", "dst"))
	if err := e.api.checkJobBuckets(j, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if err := e.api.checkJobBuckets(j, "src"); err == nil {
		t.Fatal("a job on another bucket passed")
	}
	e.waitJob(j.ID, jobs.StateCanceled)
}
