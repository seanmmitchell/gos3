package server

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	fsbackend "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

const adminV1 = "/_gos3/admin/v1"

// twoFSBackings returns a configuration with the fs default backing in a
// temporary data directory plus the fs backing "second".
func twoFSBackings(t *testing.T) config.Config {
	t.Helper()
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.Backings = []config.BackingConfig{{Name: "second", Backend: config.BackendFS, Path: t.TempDir()}}
	return cfg
}

// mustS3 sends an S3 request as root and fails the test unless it answers
// 200 or 204.
func mustS3(t *testing.T, method, url string, body []byte) response {
	t.Helper()
	res := do(t, method, url, body, &rootCreds, nil)
	if res.status != 200 && res.status != 204 {
		t.Fatalf("%s %s: %d %s", method, url, res.status, res.body)
	}
	return res
}

// submitJob creates a job through POST /jobs and returns it.
func submitJob(t *testing.T, url string, spec string) jobs.Job {
	t.Helper()
	res := do(t, "POST", url+adminV1+"/jobs", []byte(spec), &rootCreds, nil)
	if res.status != 201 {
		t.Fatalf("POST /jobs: %d %s", res.status, res.body)
	}
	var j jobs.Job
	res.json(t, &j)
	return j
}

// getJob returns the job id through GET /jobs/{id}.
func getJob(t *testing.T, url, id string) jobs.Job {
	t.Helper()
	res := do(t, "GET", url+adminV1+"/jobs/"+id, nil, &rootCreds, nil)
	if res.status != 200 {
		t.Fatalf("GET /jobs/%s: %d %s", id, res.status, res.body)
	}
	var j jobs.Job
	res.json(t, &j)
	return j
}

// waitJob polls the job until done reports true for it, failing the test
// when it fails or is canceled first.
func waitJob(t *testing.T, url, id, what string, done func(jobs.Job) bool) jobs.Job {
	t.Helper()
	var j jobs.Job
	waitFor(t, 30*time.Second, what, func() bool {
		j = getJob(t, url, id)
		if j.State == jobs.StateFailed || j.State == jobs.StateCanceled {
			t.Fatalf("job %s %s: %s (status %s)", id, j.State, j.LastError, j.Status)
		}
		return done(j)
	})
	return j
}

// readObject returns the content of bucket/key, or "" with the status when
// it cannot be read.
func readObject(t *testing.T, url, bucket, key string) (string, int) {
	t.Helper()
	res := do(t, "GET", url+"/"+bucket+"/"+key, nil, &rootCreds, nil)
	if res.status != 200 {
		return "", res.status
	}
	return string(res.body), res.status
}

// TestCopyJobEndToEnd copies a versioned bucket to a new bucket on another
// backing through the admin API and checks that the target holds the same
// versions.
func TestCopyJobEndToEnd(t *testing.T) {
	srv := start(t, twoFSBackings(t))
	mustS3(t, "PUT", srv.url+"/src", nil)
	mustS3(t, "PUT", srv.url+"/src?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`))
	want := map[string]string{"a.txt": "alpha", "dir/b.txt": "bravo", "dir/c d.txt": "charlie"}
	for k, v := range want {
		mustS3(t, "PUT", srv.url+"/src/"+k, []byte(v+"-old"))
		mustS3(t, "PUT", srv.url+"/src/"+k, []byte(v))
	}

	j := submitJob(t, srv.url, `{"kind":"copy","source":{"bucket":"src"},"target":{"bucket":"dst","create":{"backing":"second"}}}`)
	if j.Kind != "copy" || j.Labels["source"] != "src" || j.Labels["target"] != "dst" {
		t.Fatalf("job = %+v", j)
	}
	waitJob(t, srv.url, j.ID, "the copy job to succeed", func(j jobs.Job) bool { return j.State == jobs.StateSucceeded })

	for k, v := range want {
		if got, status := readObject(t, srv.url, "dst", strings.ReplaceAll(k, " ", "%20")); got != v {
			t.Errorf("dst/%s = %q (%d), want %q", k, got, status, v)
		}
	}
	// Every version is kept, with its ID.
	srcVersions := do(t, "GET", srv.url+"/src?versions", nil, &rootCreds, nil)
	dstVersions := do(t, "GET", srv.url+"/dst?versions", nil, &rootCreds, nil)
	if n := bytes.Count(dstVersions.body, []byte("<Version>")); n != 2*len(want) {
		t.Errorf("dst has %d versions, want %d:\n%s", n, 2*len(want), dstVersions.body)
	}
	for _, id := range versionIDs(srcVersions.body) {
		if !bytes.Contains(dstVersions.body, []byte("<VersionId>"+id+"</VersionId>")) {
			t.Errorf("version %s not copied", id)
		}
	}
	// The target was created on the backing the spec named.
	res := do(t, "GET", srv.url+adminV1+"/buckets/dst/placement", nil, &rootCreds, nil)
	if res.status != 200 || !bytes.Contains(res.body, []byte(`"second"`)) {
		t.Errorf("placement of dst: %d %s", res.status, res.body)
	}
	// The bucket's job listing has it.
	res = do(t, "GET", srv.url+adminV1+"/buckets/dst/jobs", nil, &rootCreds, nil)
	if res.status != 200 || !bytes.Contains(res.body, []byte(j.ID)) {
		t.Errorf("bucket jobs: %d %s", res.status, res.body)
	}
}

// versionIDs returns the VersionId elements of a ListObjectVersions
// response.
func versionIDs(body []byte) []string {
	var ids []string
	for _, part := range strings.Split(string(body), "<VersionId>")[1:] {
		id, _, _ := strings.Cut(part, "</VersionId>")
		ids = append(ids, id)
	}
	return ids
}

// TestMirrorResumesAfterRestart: a mirror survives a restart of the server.
// It makes a full (verify) pass after the restart, which copies a change
// the server did not see (written to the backing while it was stopped),
// and goes on following changes.
func TestMirrorResumesAfterRestart(t *testing.T) {
	cfg := twoFSBackings(t)
	srv := start(t, cfg)
	mustS3(t, "PUT", srv.url+"/src", nil)
	mustS3(t, "PUT", srv.url+"/src/one", []byte("first"))
	j := submitJob(t, srv.url, `{"kind":"mirror","source":{"bucket":"src"},"target":{"bucket":"dst","create":{"backing":"second"}}}`)
	waitJob(t, srv.url, j.ID, "the mirror's first pass", func(j jobs.Job) bool { return j.State == jobs.StateWaiting })
	if got, _ := readObject(t, srv.url, "dst", "one"); got != "first" {
		t.Fatalf("dst/one = %q", got)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}

	// The jobs file kept the mirror.
	data, err := os.ReadFile(filepath.Join(cfg.SystemDir(), JobsDirName, "jobs.json"))
	if err != nil || !bytes.Contains(data, []byte(j.ID)) {
		t.Fatalf("jobs.json: %v\n%s", err, data)
	}

	// A change the server does not see: written to the source's backing
	// directly while the server is stopped.
	writeDirect(t, cfg.Storage.Path, "src", "missed", "written while stopped")

	srv = start(t, cfg)
	waitFor(t, 30*time.Second, "the restart's verify pass to copy the missed change", func() bool {
		got, _ := readObject(t, srv.url, "dst", "missed")
		return got == "written while stopped"
	})
	if j2 := getJob(t, srv.url, j.ID); j2.State.Finished() {
		t.Fatalf("mirror after restart: %+v", j2)
	}
	// Changes made through the server are followed again.
	mustS3(t, "PUT", srv.url+"/src/two", []byte("second"))
	waitFor(t, 30*time.Second, "the mirror to copy a new object", func() bool {
		got, _ := readObject(t, srv.url, "dst", "two")
		return got == "second"
	})
}

// writeDirect writes bucket/key on the fs backend at dir, which no server
// has open.
func writeDirect(t *testing.T, dir, bucket, key, content string) {
	t.Helper()
	b, err := fsbackend.Open(dir, fsbackend.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	w, err := b.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum([]byte(content))
	if _, err := w.Commit(ctx, storage.ObjectMeta{ETag: hex.EncodeToString(sum[:]), ContentType: "text/plain"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotRunNow configures whole-bucket snapshots of a bucket through
// the admin API and takes one with "Snapshot now", with the fake restic.
func TestSnapshotRunNow(t *testing.T) {
	log := restictest.Install(t, "snapshots")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	srv := start(t, cfg)
	mustS3(t, "PUT", srv.url+"/photos", nil)
	mustS3(t, "PUT", srv.url+"/photos/cat.jpg", []byte("meow"))
	mustS3(t, "PUT", srv.url+"/photos/2026/dog.jpg", []byte("woof"))

	res := do(t, "PUT", srv.url+adminV1+"/buckets/photos/restic/snapshots/config",
		[]byte(`{"enabled":false,"retention":{"keepLast":3}}`), &rootCreds, nil)
	if res.status != 200 {
		t.Fatalf("PUT snapshot config: %d %s", res.status, res.body)
	}
	var stored struct {
		Enabled bool `json:"enabled"`
		Target  struct {
			Kind string `json:"kind"`
		} `json:"target"`
	}
	res.json(t, &stored)
	if stored.Enabled || stored.Target.Kind != "bucket" {
		t.Fatalf("stored config: %s", res.body)
	}

	res = do(t, "POST", srv.url+adminV1+"/buckets/photos/restic/snapshots", nil, &rootCreds, nil)
	if res.status != 200 && res.status != 201 && res.status != 202 {
		t.Fatalf("snapshot now: %d %s", res.status, res.body)
	}
	var j jobs.Job
	res.json(t, &j)
	if j.Kind != "restic-snapshot" {
		t.Fatalf("job = %s", res.body)
	}
	waitJob(t, srv.url, j.ID, "the snapshot job to succeed", func(j jobs.Job) bool { return j.State == jobs.StateSucceeded })
	if restictest.Calls(t, log, "backup") == 0 {
		t.Error("restic backup never ran")
	}

	res = do(t, "GET", srv.url+adminV1+"/buckets/photos/restic/snapshots", nil, &rootCreds, nil)
	if res.status != 200 || !bytes.Contains(res.body, []byte("gos3-snapshot")) {
		t.Fatalf("snapshots: %d %s", res.status, res.body)
	}
	// The stage is in the system directory by default.
	if _, err := os.Stat(filepath.Join(cfg.SystemDir(), "restic-stage")); err != nil {
		t.Errorf("stage directory: %v", err)
	}
	if got := srv.SnapshotStageDir(); got != filepath.Join(cfg.SystemDir(), "restic-stage") {
		t.Errorf("SnapshotStageDir = %q", got)
	}
}

// TestJobsDirectory: the jobs live in <system dir>/jobs, private to the
// server's user; without a system directory they are kept in memory.
func TestJobsDirectory(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	srv, err := New(cfg, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.SystemDir(), JobsDirName)
	if srv.JobsDir() != dir || srv.JobsWorkers() != config.DefaultJobsWorkers || srv.Jobs() == nil {
		t.Errorf("JobsDir %q, workers %d", srv.JobsDir(), srv.JobsWorkers())
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("jobs directory: %v", err)
	}
	file, err := os.Stat(filepath.Join(dir, "jobs.json"))
	if err != nil {
		t.Fatalf("jobs.json: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("jobs directory mode %v, want 0700", perm)
		}
		if perm := file.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("jobs.json mode %v, want no group or other access", perm)
		}
	}

	mem, err := New(testConfig(), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	if mem.JobsDir() != "" || mem.Jobs() == nil {
		t.Errorf("memory server: JobsDir %q", mem.JobsDir())
	}
	if got := mem.SnapshotStageDir(); got != "" {
		t.Errorf("memory server stage: %q, want \"\" (a private temporary directory)", got)
	}
}

// TestJobsFileOfAnotherVersionFailsStartup: a jobs file this build cannot
// read stops the server at startup, naming the file, instead of losing the
// jobs in it.
func TestJobsFileOfAnotherVersionFailsStartup(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	dir := filepath.Join(cfg.SystemDir(), JobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(file, []byte(`{"version":99,"jobs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, testLogger(t))
	if err == nil {
		srv.Close()
		t.Fatal("started with a jobs file of an unknown version")
	}
	if msg := err.Error(); !strings.Contains(msg, "background jobs") || !strings.Contains(msg, file) || !strings.Contains(msg, "version 99") {
		t.Fatalf("error: %v", err)
	}
	// The file is left as it was.
	if data, _ := os.ReadFile(file); !bytes.Contains(data, []byte(`"version":99`)) {
		t.Fatalf("jobs.json was rewritten: %s", data)
	}
	// The server released its locks: a corrected file starts.
	if err := os.WriteFile(file, []byte(`{"version":1,"jobs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err = New(cfg, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestJobsWorkersConfig: jobs.workers sizes the Manager.
func TestJobsWorkersConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Jobs.Workers = 5
	cfg.Restic.Snapshots.Workers = 3
	srv, err := New(cfg, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.JobsWorkers() != 5 {
		t.Errorf("workers = %d", srv.JobsWorkers())
	}
}
