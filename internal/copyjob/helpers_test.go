package copyjob

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

func testLogger() *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

// memBacking is a memory backing named name.
func memBacking(name string) multi.Backing {
	return multi.Backing{Name: name, Kind: "memory", Backend: memory.New()}
}

// plainMember is what a backing that cannot import versions offers: the
// storage contract and the conditional operations the router relies on.
type plainMember interface {
	storage.Backend
	storage.ConditionalDeleter
	storage.CheckedDeleter
	storage.ConditionalMetaUpdater
}

// plainBackend hides every other optional interface of a backend (version
// imports in particular), like a backing of native buckets.
type plainBackend struct{ plainMember }

// slowBackend is a memory backend whose reads take delay, counting them.
type slowBackend struct {
	*memory.Backend
	delay time.Duration
	opens atomic.Int64
}

func (b *slowBackend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	b.opens.Add(1)
	select {
	case <-time.After(b.delay):
	case <-ctx.Done():
		return storage.ObjectInfo{}, nil, ctx.Err()
	}
	return b.Backend.OpenObject(ctx, bucket, key, versionID)
}

// openRouter opens a router over backings (the first is the default).
func openRouter(t *testing.T, backings ...multi.Backing) *multi.Router {
	t.Helper()
	r, err := multi.Open(context.Background(), multi.Options{
		Backings:       backings,
		Logger:         testLogger(),
		HealthInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("multi.Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// env is a Service with a jobs Manager.
type env struct {
	t   *testing.T
	r   *multi.Router
	s   *Service
	m   *jobs.Manager
	dir string
}

// newEnv starts a Service and a Manager (jobs kept in dir; "" in memory)
// with fast timings.
func newEnv(t *testing.T, r *multi.Router, dir string) *env {
	t.Helper()
	s := New(Options{Router: r, Logger: testLogger(), Region: func() string { return "test-1" }})
	s.debounce = 20 * time.Millisecond
	s.checkpointEvery = 50 * time.Millisecond
	s.statusEvery = 20 * time.Millisecond
	m, err := jobs.Open(jobs.Options{
		Dir:      dir,
		Runners:  map[string]jobs.Runner{KindCopy: s.Runner(KindCopy), KindMirror: s.Runner(KindMirror)},
		Logger:   testLogger(),
		RetryMin: 20 * time.Millisecond,
		RetryMax: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("jobs.Open: %v", err)
	}
	s.Attach(m)
	e := &env{t: t, r: r, s: s, m: m, dir: dir}
	t.Cleanup(e.close)
	return e
}

func (e *env) close() {
	_ = e.m.Close()
	e.s.Close()
}

// submit submits a job of spec's kind.
func (e *env) submit(spec map[string]any) (jobs.Job, error) {
	e.t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		e.t.Fatal(err)
	}
	kind, _ := spec["kind"].(string)
	return e.m.Submit(context.Background(), kind, data, "tester")
}

func (e *env) mustSubmit(spec map[string]any) jobs.Job {
	e.t.Helper()
	j, err := e.submit(spec)
	if err != nil {
		e.t.Fatalf("Submit: %v", err)
	}
	return j
}

// waitJob waits until cond holds for job id.
func (e *env) waitJob(id, what string, cond func(jobs.Job) bool) jobs.Job {
	e.t.Helper()
	var j jobs.Job
	waitFor(e.t, what, func() bool {
		var err error
		if j, err = e.m.Get(id); err != nil {
			e.t.Fatalf("Get: %v", err)
		}
		return cond(j)
	})
	return j
}

// waitState waits until job id is in state want (failing early when it
// fails instead).
func (e *env) waitState(id string, want jobs.State) jobs.Job {
	e.t.Helper()
	return e.waitJob(id, "job "+id+" to be "+string(want), func(j jobs.Job) bool {
		if j.State == jobs.StateFailed && want != jobs.StateFailed {
			e.t.Fatalf("job %s failed: %s", id, j.LastError)
		}
		return j.State == want
	})
}

// waitIdle waits until mirror job id waits after a run that left nothing
// pending.
func (e *env) waitIdle(id string) jobs.Job {
	e.t.Helper()
	return e.waitJob(id, "mirror "+id+" to be idle", func(j jobs.Job) bool {
		if j.State == jobs.StateFailed {
			e.t.Fatalf("job %s failed: %s", id, j.LastError)
		}
		return j.State == jobs.StateWaiting && j.Phase == phaseWatching && j.LastError == ""
	})
}

func statusOf(t *testing.T, j jobs.Job) Status {
	t.Helper()
	var st Status
	if err := json.Unmarshal(j.Status, &st); err != nil {
		t.Fatalf("status %s: %v", j.Status, err)
	}
	return st
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func createBucket(t *testing.T, b storage.Backend, name string, opts storage.CreateBucketOptions, versioning storage.VersioningStatus) {
	t.Helper()
	ctx := context.Background()
	if err := b.CreateBucket(ctx, name, opts); err != nil {
		t.Fatalf("CreateBucket %s: %v", name, err)
	}
	if versioning != storage.VersioningUnversioned {
		if err := b.SetBucketVersioning(ctx, name, versioning); err != nil {
			t.Fatalf("SetBucketVersioning %s: %v", name, err)
		}
	}
}

func put(t *testing.T, b storage.Backend, bucket, key, body string) storage.ObjectInfo {
	t.Helper()
	ctx := context.Background()
	w, err := b.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		t.Fatalf("NewObjectWriter %s/%s: %v", bucket, key, err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum([]byte(body))
	oi, err := w.Commit(ctx, storage.ObjectMeta{ETag: hex.EncodeToString(sum[:]), ContentType: "text/plain"}, storage.CommitOptions{})
	if err != nil {
		t.Fatalf("Commit %s/%s: %v", bucket, key, err)
	}
	return oi
}

func del(t *testing.T, b storage.Backend, bucket, key, versionID string) storage.DeleteResult {
	t.Helper()
	res, err := b.DeleteObject(context.Background(), bucket, key, versionID)
	if err != nil {
		t.Fatalf("DeleteObject %s/%s@%s: %v", bucket, key, versionID, err)
	}
	return res
}

func read(t *testing.T, b storage.Backend, bucket, key string) string {
	t.Helper()
	_, rc, err := b.OpenObject(context.Background(), bucket, key, "")
	if err != nil {
		t.Fatalf("OpenObject %s/%s: %v", bucket, key, err)
	}
	if rc == nil {
		t.Fatalf("OpenObject %s/%s: a delete marker", bucket, key)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// versions lists every version of bucket/key, newest first.
func versions(t *testing.T, b storage.Backend, bucket, key string) []storage.ObjectInfo {
	t.Helper()
	vs, err := copier.KeyVersions(context.Background(), b, bucket, key)
	if err != nil {
		t.Fatalf("listing %s/%s: %v", bucket, key, err)
	}
	return vs
}

// keys lists the current keys of bucket.
func keys(t *testing.T, b storage.Backend, bucket string) []string {
	t.Helper()
	var out []string
	err := b.WalkObjects(context.Background(), bucket, "", "", func(oi storage.ObjectInfo) error {
		out = append(out, oi.Key)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkObjects %s: %v", bucket, err)
	}
	return out
}

// readOrEmpty is read returning "" for a missing key or a delete marker
// (a key being replaced may be missing for a moment).
func readOrEmpty(t *testing.T, b storage.Backend, bucket, key string) string {
	t.Helper()
	_, rc, err := b.OpenObject(context.Background(), bucket, key, "")
	if errors.Is(err, storage.ErrObjectNotFound) || errors.Is(err, storage.ErrVersionNotFound) || err == nil && rc == nil {
		return ""
	}
	if err != nil {
		t.Fatalf("OpenObject %s/%s: %v", bucket, key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
