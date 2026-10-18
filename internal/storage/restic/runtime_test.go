package restic

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// memRecords is an in-memory RecoveryStore.
type memRecords struct {
	mu     sync.Mutex
	m      map[string]Record
	stores int
	fail   error // returned by Store when set
}

func (s *memRecords) Load(_ context.Context, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id], nil
}

func (s *memRecords) Store(_ context.Context, id string, rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.m == nil {
		s.m = make(map[string]Record)
	}
	s.m[id] = rec
	s.stores++
	return nil
}

// repoKind is a kind of repository the tests run against.
type repoKind struct {
	name string
	// location returns a new repository's location and a function
	// counting its snapshots.
	location func(t *testing.T) (Location, func() int)
}

var password = PasswordFromKey([]byte("0123456789abcdef0123456789abcdef"))

func localRepo(t *testing.T) (Location, func() int) {
	repo := filepath.Join(t.TempDir(), "repo")
	loc := Location{Repository: repo, Password: password, CacheDir: filepath.Join(t.TempDir(), "cache")}
	return loc, func() int {
		entries, err := os.ReadDir(filepath.Join(repo, "snapshots"))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return len(entries)
	}
}

func s3Repo(t *testing.T) (Location, func() int) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "data")
	loc := S3Repository{
		Endpoint: remote.URL, Bucket: "data", Path: "pfx/restic/b1", Region: s3clienttest.Region,
		AccessKey: s3clienttest.AccessKey, SecretKey: s3clienttest.SecretKey, PathStyle: true,
	}.Location(password, "")
	c := remote.Client(t)
	return loc, func() int {
		n := 0
		err := c.Walk(t.Context(), "data", "pfx/restic/b1/snapshots/", "", func(s3client.ObjectAttrs) error { n++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
}

var repoKinds = []repoKind{{"local", localRepo}, {"s3", s3Repo}}

// archive writes data as a new snapshot of loc.
func archive(t *testing.T, rt *Runtime, loc Location, data string) (string, error) {
	t.Helper()
	a := rt.NewArchiver(t.Context(), loc, "bucket", "key")
	if _, err := io.WriteString(a, data); err != nil {
		a.Abort()
		return "", err
	}
	if a.Size() != int64(len(data)) {
		t.Fatalf("archiver size %d, want %d", a.Size(), len(data))
	}
	return a.Finish(nil)
}

// readAll restores snapshot from loc and returns its content.
func readAll(t *testing.T, rt *Runtime, loc Location, snapshot string, size int64) string {
	t.Helper()
	r, err := rt.Open(t.Context(), loc, snapshot, size)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestRoundTrip(t *testing.T) {
	for _, kind := range repoKinds {
		t.Run(kind.name, func(t *testing.T) {
			log := restictest.Install(t, "current")
			loc, snapshots := kind.location(t)
			rt := &Runtime{TempDir: t.TempDir()}
			records := &memRecords{}
			if err := rt.Validate(t.Context(), "b1", loc, records); err != nil {
				t.Fatalf("validate a new repository: %v", err)
			}
			for range 2 {
				if err := rt.Prepare(t.Context(), "b1", loc, records); err != nil {
					t.Fatal(err)
				}
			}
			if got := records.m["b1"]; !got.Initialized || got.Repository != loc.Repository {
				t.Fatalf("record = %+v", got)
			}
			if n := restictest.Calls(t, log, "init"); n != 1 {
				t.Fatalf("init ran %d times", n)
			}
			if n := restictest.Calls(t, log, "cat config"); n != 2 { // Validate + one Prepare
				t.Fatalf("cat config ran %d times", n)
			}
			if !rt.Prepared(loc) {
				t.Fatal("not prepared")
			}
			data := strings.Repeat("restic archived object ", 20000)
			snap, err := archive(t, rt, loc, data)
			if err != nil {
				t.Fatal(err)
			}
			if !ValidSnapshot(snap) || snapshots() != 1 {
				t.Fatalf("snapshot %q, %d snapshots", snap, snapshots())
			}
			if got := readAll(t, rt, loc, snap, int64(len(data))); got != data {
				t.Fatal("restored content differs")
			}
			// Empty objects never reach restic.
			if snap, err := archive(t, rt, loc, ""); err != nil || snap != "" {
				t.Fatalf("empty archive: %q %v", snap, err)
			}
			if got := readAll(t, rt, loc, strings.Repeat("0", 64), 0); got != "" {
				t.Fatal("empty read")
			}
			if n := restictest.Calls(t, log, "backup"); n != 1 {
				t.Fatalf("backup ran %d times", n)
			}
			if n := restictest.Calls(t, log, "dump"); n != 1 {
				t.Fatalf("dump ran %d times", n)
			}
			if files := tempFiles(t, rt.TempDir); len(files) != 0 {
				t.Fatalf("temporary files left: %v", files)
			}
			if loc.CacheDir != "" {
				if _, err := os.Stat(loc.CacheDir); err != nil {
					t.Fatalf("cache directory: %v", err)
				}
			}
		})
	}
}

func TestRestoreSpoolQuota(t *testing.T) {
	tooLarge := &Runtime{}
	large, err := tooLarge.Open(t.Context(), Location{Repository: "unused"}, strings.Repeat("0", 64), DefaultRestoreSpoolBytes+1)
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Prepare(); !errors.Is(err, ErrRestoreSpoolLimit) {
		t.Fatalf("oversized restore: %v", err)
	}
	large.Close()

	restictest.Install(t, "current")
	loc, _ := localRepo(t)
	rt := &Runtime{TempDir: t.TempDir(), MaxRestoreSpoolBytes: crypt.EncryptedSize(4) + 1}
	other := &Runtime{TempDir: t.TempDir(), MaxRestoreSpoolBytes: crypt.EncryptedSize(4) + 1}
	if err := rt.Prepare(t.Context(), "b1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	first, err := archive(t, rt, loc, "abcd")
	if err != nil {
		t.Fatal(err)
	}
	second, err := archive(t, rt, loc, "efgh")
	if err != nil {
		t.Fatal(err)
	}
	a, err := rt.Open(t.Context(), loc, first, 4)
	if err != nil || a.Prepare() != nil {
		t.Fatalf("first restore: %v", err)
	}
	defer a.Close()
	b, err := other.Open(t.Context(), loc, second, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Prepare(); !errors.Is(err, ErrRestoreSpoolLimit) || !errors.Is(err, storage.ErrResourceLimit) {
		t.Fatalf("second concurrent restore: %v", err)
	}
	b.Close()
	if rt.spoolBytes != crypt.EncryptedSize(4) {
		t.Fatalf("reserved bytes = %d, want %d", rt.spoolBytes, crypt.EncryptedSize(4))
	}
	a.Close()
	if rt.spoolBytes != 0 {
		t.Fatalf("reserved bytes after close = %d", rt.spoolBytes)
	}
	c, err := other.Open(t.Context(), loc, second, 4)
	if err != nil || c.Prepare() != nil {
		t.Fatalf("restore after release: %v", err)
	}
	c.Close()
	if rt.spoolBytes != 0 || other.spoolBytes != 0 {
		t.Fatalf("reserved bytes after final close = %d and %d", rt.spoolBytes, other.spoolBytes)
	}
}

func TestRangeRestoreSpoolsOnlyRequestedBytes(t *testing.T) {
	restictest.Install(t, "current")
	loc, _ := localRepo(t)
	rt := &Runtime{TempDir: t.TempDir(), MaxRestoreSpoolBytes: crypt.EncryptedSize(3)}
	if err := rt.Prepare(t.Context(), "b1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	data := strings.Repeat("0123456789", 10000)
	snap, err := archive(t, rt, loc, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, start := range []int64{0, int64(len(data)) - 3} {
		r, err := rt.Open(t.Context(), loc, snap, int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Seek(start, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if err := r.PrepareRange(start, 3); err != nil {
			t.Fatalf("range at %d: %v", start, err)
		}
		got, err := io.ReadAll(r)
		if err != nil || string(got) != data[start:start+3] {
			t.Fatalf("range at %d: %q, %v", start, got, err)
		}
		if rt.spoolBytes != crypt.EncryptedSize(3) {
			t.Fatalf("reserved bytes = %d, want %d", rt.spoolBytes, crypt.EncryptedSize(3))
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if rt.spoolBytes != 0 || len(tempFiles(t, rt.TempDir)) != 0 {
			t.Fatalf("spool not released: %d bytes, %v files", rt.spoolBytes, tempFiles(t, rt.TempDir))
		}
	}
}

// A missing repository whose record says it was initialized is an error,
// never initialized anew, by Prepare and Validate.
func TestMissingInitializedRepository(t *testing.T) {
	for _, kind := range repoKinds {
		t.Run(kind.name, func(t *testing.T) {
			log := restictest.Install(t, "current")
			loc, _ := kind.location(t)
			rt := &Runtime{TempDir: t.TempDir()}
			records := &memRecords{m: map[string]Record{"b1": {Initialized: true, Repository: loc.Repository}}}
			if err := rt.Validate(t.Context(), "b1", loc, records); !errors.Is(err, storage.ErrEncryptionUnavailable) {
				t.Fatalf("validate: %v", err)
			}
			err := rt.Prepare(t.Context(), "b1", loc, records)
			if !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "initialized repository is missing") {
				t.Fatalf("prepare: %v", err)
			}
			if n := restictest.Calls(t, log, "init"); n != 0 {
				t.Fatalf("init ran %d times", n)
			}
		})
	}
}

// The record is written before the repository is created; a failed
// initialization leaves it uninitialized and is retried.
func TestFailedPreparationIsRetried(t *testing.T) {
	restictest.Install(t, "current")
	loc, _ := localRepo(t)
	rt := &Runtime{TempDir: t.TempDir()}
	records := &memRecords{}
	t.Setenv(restictest.EnvFail, "init")
	if err := rt.Prepare(t.Context(), "b1", loc, records); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("failed init: %v", err)
	}
	if rec := records.m["b1"]; rec.Repository != loc.Repository || rec.Initialized {
		t.Fatalf("record before init: %+v", rec)
	}
	t.Setenv(restictest.EnvFail, "")
	if err := rt.Prepare(t.Context(), "b1", loc, records); err != nil {
		t.Fatal(err)
	}
	if !records.m["b1"].Initialized {
		t.Fatal("record not marked initialized")
	}
	// A record that cannot be stored stops preparation before restic runs.
	other, _ := localRepo(t)
	records.fail = errors.New("disk full")
	if err := rt.Prepare(t.Context(), "b2", other, records); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("prepare without a record: %v", err)
	}
	if _, err := os.Stat(other.Repository); !os.IsNotExist(err) {
		t.Fatalf("repository created without a record: %v", err)
	}
}

// Failures wrap storage.ErrEncryptionUnavailable and carry restic's
// redacted diagnostics; an ended request reports its context; refused and
// aborted archives save no snapshot.
func TestFailures(t *testing.T) {
	for _, kind := range repoKinds {
		t.Run(kind.name, func(t *testing.T) {
			restictest.Install(t, "current")
			loc, snapshots := kind.location(t)
			rt := &Runtime{TempDir: t.TempDir()}
			// The canceled read below leaves its restore to finish in the
			// background; let it end before TempDir is removed.
			t.Cleanup(rt.Wait)
			if err := rt.Prepare(t.Context(), "b1", loc, &memRecords{}); err != nil {
				t.Fatal(err)
			}
			snap, err := archive(t, rt, loc, "kept")
			if err != nil {
				t.Fatal(err)
			}

			a := rt.NewArchiver(t.Context(), loc, "bucket", "key")
			_, _ = io.WriteString(a, "refused")
			refused := errors.New("precondition failed")
			if _, err := a.Finish(func() error { return refused }); !errors.Is(err, refused) {
				t.Fatalf("refused finish: %v", err)
			}
			a = rt.NewArchiver(t.Context(), loc, "bucket", "key")
			_, _ = io.WriteString(a, "aborted")
			a.Abort()
			if n := snapshots(); n != 1 {
				t.Fatalf("%d snapshots after refused and aborted archives", n)
			}

			t.Setenv(restictest.EnvFail, "backup")
			_, err = archive(t, rt, loc, "lost")
			if !errors.Is(err, storage.ErrEncryptionUnavailable) {
				t.Fatalf("backup failure not classified: %v", err)
			}
			if !strings.Contains(err.Error(), "unable to save snapshot") || strings.Contains(err.Error(), password) {
				t.Fatalf("diagnostics missing or not redacted: %v", err)
			}
			t.Setenv(restictest.EnvFail, "dump")
			r, err := rt.Open(t.Context(), loc, snap, 4)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(r)
			_ = r.Close()
			if !errors.Is(err, storage.ErrEncryptionUnavailable) || strings.Contains(err.Error(), password) {
				t.Fatalf("dump failure: %v", err)
			}
			t.Setenv(restictest.EnvFail, "")
			t.Setenv(restictest.EnvDumpDelay, "5s")
			ctx, cancel := context.WithCancel(t.Context())
			r, err = rt.Open(ctx, loc, snap, 4)
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			err = r.Prepare()
			_ = r.Close()
			if !errors.Is(err, context.Canceled) || errors.Is(err, storage.ErrEncryptionUnavailable) {
				t.Fatalf("canceled read: %v", err)
			}
		})
	}
}

// Opening restores nothing; concurrent readers share one restore, which is
// removed with the last reader.
func TestReadsAreLazyAndShared(t *testing.T) {
	log := restictest.Install(t, "current")
	loc, _ := localRepo(t)
	rt := &Runtime{TempDir: t.TempDir()}
	if err := rt.Prepare(t.Context(), "b1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	data := strings.Repeat("0123456789", 100000)
	snap, err := archive(t, rt, loc, data)
	if err != nil {
		t.Fatal(err)
	}
	r, err := rt.Open(t.Context(), loc, snap, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if n := restictest.Calls(t, log, "dump"); n != 0 {
		t.Fatalf("dump ran %d times after open/seek/close", n)
	}

	t.Setenv(restictest.EnvDumpDelay, "300ms")
	const readers = 4
	opened := make([]*Reader, readers)
	for i := range opened {
		if opened[i], err = rt.Open(t.Context(), loc, snap, int64(len(data))); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for i, r := range opened {
		wg.Go(func() {
			off := int64(i * 1000)
			buf := make([]byte, 1000)
			if _, err := r.ReadAt(buf, off); err != nil {
				errs <- err
				return
			}
			if string(buf) != data[off:off+1000] {
				errs <- errors.New("wrong range content")
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := restictest.Calls(t, log, "dump"); n != 1 {
		t.Fatalf("%d dumps for concurrent readers", n)
	}
	files := tempFiles(t, rt.TempDir)
	if len(files) != 1 {
		t.Fatalf("want one shared spool, have %v", files)
	}
	// The spool holds no plaintext.
	raw, err := os.ReadFile(filepath.Join(rt.TempDir, files[0]))
	if err != nil || strings.Contains(string(raw), "0123456789") {
		t.Fatalf("spool is not encrypted (%v)", err)
	}
	for _, r := range opened {
		_ = r.Close()
	}
	if files := tempFiles(t, rt.TempDir); len(files) != 0 {
		t.Fatalf("spool not removed: %v", files)
	}
	if _, err := opened[0].Read(make([]byte, 1)); err == nil {
		t.Fatal("read from a closed reader")
	}
}

func TestVersionCheck(t *testing.T) {
	restictest.Install(t, "old")
	t.Setenv(restictest.EnvVersion, "0.16.4")
	loc, _ := localRepo(t)
	rt := &Runtime{}
	err := rt.Prepare(t.Context(), "b1", loc, &memRecords{})
	if !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "0.17.1") {
		t.Fatalf("old restic accepted: %v", err)
	}
	rt.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if rt.Installed() {
		t.Fatal("Installed without restic")
	}
	if _, err := rt.Command(t.Context(), loc, "snapshots"); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("command without restic: %v", err)
	}
}

// Restic never inherits RESTIC_* or AWS_* variables from the server.
func TestCommandEnvironment(t *testing.T) {
	restictest.Install(t, "current")
	t.Setenv("RESTIC_REPOSITORY", "/elsewhere")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "server-secret")
	rt := &Runtime{}
	loc := Location{Repository: "/repo", Password: "pw", Env: []string{"AWS_ACCESS_KEY_ID=AK"}, Options: []string{"s3.region=x"}}
	cmd, err := rt.Command(t.Context(), loc, "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "/elsewhere") || strings.Contains(env, "server-secret") {
		t.Fatal("inherited restic or AWS settings")
	}
	if !strings.Contains(env, "RESTIC_PASSWORD=pw") || !strings.Contains(env, "AWS_ACCESS_KEY_ID=AK") {
		t.Fatalf("missing location environment: %q", cmd.Env)
	}
	args := strings.Join(cmd.Args[1:], " ")
	want := "--repo /repo --retry-lock 30s --http-user-agent " + storage.ResticUserAgent + " --no-cache -o s3.region=x snapshots"
	if args != want {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

// TestRealRestic runs the real restic (skipped when it is not on PATH)
// against a local repository and an s3 repository on an in-process S3
// service.
func TestRealRestic(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("install restic >= 0.17.1 to run integration")
	}
	for _, kind := range repoKinds {
		t.Run(kind.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			loc, snapshots := kind.location(t)
			rt := &Runtime{TempDir: t.TempDir()}
			records := &memRecords{}
			if err := rt.Validate(ctx, "b1", loc, records); err != nil {
				t.Fatalf("validate a new repository: %v", err)
			}
			if err := rt.Prepare(ctx, "b1", loc, records); err != nil {
				t.Fatal(err)
			}
			data := strings.Repeat("restic archived object ", 20000)
			a := rt.NewArchiver(ctx, loc, "bucket", "key")
			if _, err := io.WriteString(a, data); err != nil {
				t.Fatal(err)
			}
			snap, err := a.Finish(nil)
			if err != nil {
				t.Fatal(err)
			}
			if n := snapshots(); n != 1 {
				t.Fatalf("%d snapshots", n)
			}
			r, err := rt.Open(ctx, loc, snap, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Seek(123, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			_ = r.Close()
			if err != nil || string(got) != data[123:] {
				t.Fatalf("restic roundtrip: %v", err)
			}
			// A fresh runtime (a restarted server) finds the repository
			// initialized; a wrong password is refused, not initialized.
			if err := new(Runtime).Validate(ctx, "b1", loc, records); err != nil {
				t.Fatalf("validate an existing repository: %v", err)
			}
			wrong := loc
			wrong.Password = "not-the-password-7f3a"
			if err := new(Runtime).Prepare(ctx, "b1", wrong, records); !errors.Is(err, storage.ErrEncryptionUnavailable) || strings.Contains(err.Error(), wrong.Password) {
				t.Fatalf("wrong password: %v", err)
			}
			if files := tempFiles(t, rt.TempDir); len(files) != 0 {
				t.Fatalf("temporary files left: %v", files)
			}
		})
	}
}
