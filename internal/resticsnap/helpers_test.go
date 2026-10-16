package resticsnap

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	fsbackend "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

func TestMain(m *testing.M) { restictest.Main(m) }

// testEnv is a router with a memory backing "mem" and an fs backing
// "disk", a Service and a started jobs.Manager, with the fake restic.
type testEnv struct {
	t      *testing.T
	router *multi.Router
	svc    *Service
	mgr    *jobs.Manager
	log    string // the fake restic's invocation log
}

// newEnv returns a test environment with the fake restic whose default
// backing is def ("mem" or "disk"); o's Router, SystemDir and Logger are
// set by newEnv.
func newEnv(t *testing.T, def string, o Options) *testEnv {
	t.Helper()
	return newEnvWith(t, def, o, restictest.Install(t, "current"))
}

// newRealEnv is newEnv with the restic on PATH (the test is skipped
// without one).
func newRealEnv(t *testing.T, def string, o Options) *testEnv {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("install restic >= 0.17.1 to run integration")
	}
	return newEnvWith(t, def, o, "")
}

// newEnvWith implements newEnv; log is the fake restic's log ("" for the
// real restic).
func newEnvWith(t *testing.T, def string, o Options, log string) *testEnv {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	fsDir := t.TempDir()
	fsb, err := fsbackend.Open(fsDir, fsbackend.Options{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	r, err := multi.Open(context.Background(), multi.Options{
		Backings: []multi.Backing{
			{Name: "mem", Kind: "memory", Backend: memory.New()},
			{Name: "disk", Kind: "fs", Path: fsDir, Backend: fsb},
		},
		Default: def, Logger: logger, HealthInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	o.Router, o.SystemDir, o.Logger = r, t.TempDir(), logger
	svc, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	m, err := jobs.Open(jobs.Options{
		Runners: map[string]jobs.Runner{
			KindSnapshot: svc.Runner(KindSnapshot), KindRestore: svc.Runner(KindRestore), KindPrune: svc.Runner(KindPrune),
		},
		Logger: logger, RetryMin: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Start and stop the scheduler at once: RunNow and the other methods
	// use the manager, and tests tick the scheduler themselves.
	svc.Start(m)
	svc.Stop()
	t.Cleanup(func() {
		svc.Stop()
		_ = m.Close()
	})
	return &testEnv{t: t, router: r, svc: svc, mgr: m, log: log}
}

// createBucket creates bucket on backing with versioning.
func (e *testEnv) createBucket(bucket, backing string, versioning storage.VersioningStatus) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.router.CreateBucket(ctx, bucket, storage.CreateBucketOptions{Backing: backing}); err != nil {
		e.t.Fatal(err)
	}
	if versioning != "" {
		if err := e.router.SetBucketVersioning(ctx, bucket, versioning); err != nil {
			e.t.Fatal(err)
		}
	}
}

// put writes an object with metadata meta.
func (e *testEnv) put(bucket, key, data string, meta storage.ObjectMeta) storage.ObjectInfo {
	e.t.Helper()
	ctx := context.Background()
	w, err := e.router.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		e.t.Fatal(err)
	}
	sum := md5.Sum([]byte(data))
	meta.ETag = hex.EncodeToString(sum[:])
	oi, err := w.Commit(ctx, meta, storage.CommitOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return oi
}

// configure stores a snapshot configuration and returns it normalized.
func (e *testEnv) configure(bucket, doc string) Config {
	e.t.Helper()
	raw, err := e.svc.PutConfig(context.Background(), bucket, json.RawMessage(doc))
	if err != nil {
		e.t.Fatalf("PutConfig(%s): %v", doc, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

// wait waits for job id to finish and returns it.
func (e *testEnv) wait(id string) jobs.Job {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		j, err := e.mgr.Get(id)
		if err != nil {
			e.t.Fatal(err)
		}
		if j.State.Finished() {
			return j
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %s did not finish: %+v", id, j)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// snapshot runs a snapshot of bucket and returns the job's status.
func (e *testEnv) snapshot(bucket string) jobStatus {
	e.t.Helper()
	j, err := e.svc.RunNow(context.Background(), bucket, "test")
	if err != nil {
		e.t.Fatal(err)
	}
	j = e.wait(j.ID)
	if j.State != jobs.StateSucceeded {
		e.t.Fatalf("snapshot job %s: %s (failures %d): %s", j.ID, j.State, j.Failures, j.LastError)
	}
	var st jobStatus
	if err := json.Unmarshal(j.Status, &st); err != nil {
		e.t.Fatal(err)
	}
	if !restic.ValidSnapshot(st.Snapshot) {
		e.t.Fatalf("status %+v names no snapshot", st)
	}
	return st
}

// location returns the repository of bucket's snapshots.
func (e *testEnv) location(bucket string) restic.Location {
	e.t.Helper()
	loc, _, err := e.svc.Credentials(context.Background(), bucket)
	if err != nil {
		e.t.Fatal(err)
	}
	return loc
}

// manifest returns the header and entries of snapshot's manifest.
func (e *testEnv) manifest(bucket, snapshot string) (manifestHeader, []entry) {
	e.t.Helper()
	var buf bytes.Buffer
	if err := e.svc.rt.DumpFile(context.Background(), e.location(bucket), snapshot, "/"+ManifestName, &buf); err != nil {
		e.t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var h manifestHeader
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		e.t.Fatal(err)
	}
	var out []entry
	for _, l := range lines[1:] {
		var en entry
		if err := json.Unmarshal([]byte(l), &en); err != nil {
			e.t.Fatal(err)
		}
		if err := en.finishRead(); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, en)
	}
	return h, out
}

// checkSnapshot checks that snapshot holds exactly the objects want (key
// -> content), each in the file its manifest entry names.
func (e *testEnv) checkSnapshot(bucket, snapshot string, want map[string]string) []entry {
	e.t.Helper()
	h, entries := e.manifest(bucket, snapshot)
	if h.Format != manifestFormat || h.Bucket != bucket {
		e.t.Fatalf("manifest header %+v", h)
	}
	got := map[string]bool{}
	loc := e.location(bucket)
	for _, en := range entries {
		if en.DeleteMarker {
			continue
		}
		got[en.Key] = true
		data, ok := want[en.Key]
		if !ok {
			e.t.Fatalf("snapshot %s holds unexpected key %q", snapshot[:8], en.Key)
		}
		var buf bytes.Buffer
		if err := e.svc.rt.DumpFile(context.Background(), loc, snapshot, "/"+en.Path, &buf); err != nil {
			e.t.Fatalf("dump %q (%s): %v", en.Key, en.Path, err)
		}
		if buf.String() != data {
			e.t.Fatalf("snapshot %s: %q (%s) holds %q, want %q", snapshot[:8], en.Key, en.Path, buf.String(), data)
		}
	}
	for k := range want {
		if !got[k] {
			e.t.Fatalf("snapshot %s lacks key %q", snapshot[:8], k)
		}
	}
	return entries
}
