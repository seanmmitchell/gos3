package multi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// fakeS3 opens s3 backings as memory backends and records the connection
// changes it prepares and commits.
type fakeS3 struct {
	mu         sync.Mutex
	prepared   []config.BackingDefinition
	committed  []config.BackingDefinition
	prepareErr error
	// onCommit runs in each commit, before it is recorded.
	onCommit func(config.BackingDefinition)
	// newBackend, when set, makes the backend of opened s3 backings.
	newBackend func() storage.Backend
	// opening, when set, is closed when an s3 backing starts opening; the
	// opening then waits for openGate.
	opening  chan struct{}
	openGate chan struct{}
}

func (f *fakeS3) opener(reconfigure bool) BackingOpener {
	o := BackingOpener{Open: func(d config.BackingDefinition) (Backing, error) {
		b := Backing{Name: d.Name, Kind: d.Backend, Path: d.Path, Backend: memory.New()}
		if d.S3 != nil {
			if f.opening != nil {
				close(f.opening)
				<-f.openGate
			}
			if f.newBackend != nil {
				b.Backend = f.newBackend()
			}
			b.Location = d.Config().S3.Location()
		}
		return b, nil
	}}
	if reconfigure {
		o.Reconfigure = func(_ context.Context, _ Backing, d config.BackingDefinition) (func(), error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.prepareErr != nil {
				return nil, f.prepareErr
			}
			f.prepared = append(f.prepared, d)
			return func() {
				if f.onCommit != nil {
					f.onCommit(d)
				}
				f.mu.Lock()
				f.committed = append(f.committed, d)
				f.mu.Unlock()
			}, nil
		}
	}
	return o
}

func (f *fakeS3) counts() (prepared, committed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prepared), len(f.committed)
}

var testKEK = base64.StdEncoding.EncodeToString(make([]byte, 32))

// s3Def is an s3 backing definition named "remote".
func s3Def(secret string) config.BackingDefinition {
	return config.BackingDefinition{Name: "remote", Backend: config.BackendS3, S3: &config.S3Definition{
		Endpoint: "http://127.0.0.1:9000", HomeBucket: "data", HomePrefix: "gos3/", AccessKey: "ak", SecretKey: secret, EncryptionKey: testKEK,
	}}
}

// withS3 returns c with its remote backing's s3 settings changed by fn.
func withS3(c BackingConfiguration, fn func(*config.S3Definition)) BackingConfiguration {
	for i, d := range c.Backings {
		if d.Name == "remote" {
			s := *d.S3
			fn(&s)
			c.Backings = append([]config.BackingDefinition(nil), c.Backings...)
			c.Backings[i].S3 = &s
		}
	}
	return c
}

func managedS3Router(t *testing.T, path string, f *fakeS3, reconfigure bool) *Router {
	t.Helper()
	r := openRouter(t, Options{Backings: []Backing{{Name: "default", Kind: "memory", Backend: memory.New()}}})
	r.EnableBackingManagement(config.StorageConfig{Backend: "memory"}, path, f.opener(reconfigure))
	return r
}

func assertNoSecret(t *testing.T, c BackingConfiguration, secrets ...string) {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(secrets, testKEK) {
		if strings.Contains(string(data), s) {
			t.Fatalf("configuration reveals %q: %s", s, data)
		}
	}
}

func TestConfigureBackingsS3Connection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backings.json")
	f := &fakeS3{}
	r := managedS3Router(t, path, f, true)

	in, _ := r.BackingConfiguration()
	in.Backings = []config.BackingDefinition{s3Def("secret-one")}
	out, err := r.ConfigureBackings(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, out, "secret-one")
	if s := out.Backings[0].S3; s.SecretKey != "" || !s.SecretKeyConfigured || !s.EncryptionKeyConfigured {
		t.Fatalf("public s3 settings: %+v", s)
	}
	if st := r.Backings(); len(st) != 2 || st[1].Location != "s3://data/gos3/" {
		t.Fatalf("Backings() = %+v", st)
	}
	if err := r.CreateBucket(ctx, "photos", storage.CreateBucketOptions{Backing: "remote"}); err != nil {
		t.Fatal(err)
	}
	remote := r.topology.Load().backings["remote"]

	// Resubmitting the public settings (blank secrets) changes nothing.
	if out, err = r.ConfigureBackings(ctx, out); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.counts(); p != 0 {
		t.Fatal("unchanged settings were reconfigured")
	}

	// Another home bucket or prefix is another backing, which is in use.
	for _, change := range []func(*config.S3Definition){
		func(s *config.S3Definition) { s.HomeBucket = "other" },
		func(s *config.S3Definition) { s.HomePrefix = "elsewhere/" },
	} {
		if _, err := r.ConfigureBackings(ctx, withS3(out, change)); !errors.Is(err, ErrConflict) {
			t.Fatalf("identity change of a backing in use: %v", err)
		}
	}

	// A secret-only change reconfigures the backing in place, committed
	// after the configuration is saved.
	f.onCommit = func(config.BackingDefinition) {
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "secret-two") {
			t.Errorf("committed before the configuration was saved: %s, %v", data, err)
		}
	}
	next, err := r.ConfigureBackings(ctx, withS3(out, func(s *config.S3Definition) { s.SecretKey = "secret-two" }))
	if err != nil {
		t.Fatal(err)
	}
	if p, c := f.counts(); p != 1 || c != 1 || f.committed[0].S3.SecretKey != "secret-two" {
		t.Fatalf("prepared %d, committed %d: %+v", p, c, f.committed)
	}
	if next.Revision == out.Revision {
		t.Fatal("a secret-only change kept the revision")
	}
	assertNoSecret(t, next, "secret-one", "secret-two")
	if r.topology.Load().backings["remote"] != remote {
		t.Fatal("reconfiguration replaced the backing")
	}
	if p, err := r.Placement("photos"); err != nil || p.Primary != "remote" {
		t.Fatalf("bucket after reconfiguration: %+v, %v", p, err)
	}

	// An endpoint change too; a failed preparation changes nothing.
	f.prepareErr = errors.New("marker mismatch")
	moved := withS3(next, func(s *config.S3Definition) { s.Endpoint = "http://127.0.0.1:9001" })
	if _, err := r.ConfigureBackings(ctx, moved); err == nil || !strings.Contains(err.Error(), "marker mismatch") {
		t.Fatalf("failed preparation: %v", err)
	}
	if got, _ := r.BackingConfiguration(); got.Revision != next.Revision {
		t.Fatal("failed preparation changed the configuration")
	}
	f.prepareErr = nil
	f.onCommit = nil
	if next, err = r.ConfigureBackings(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if _, c := f.counts(); c != 2 {
		t.Fatalf("committed %d", c)
	}

	// The default bucket kind is not part of the identity either.
	if _, err := r.ConfigureBackings(ctx, withS3(next, func(s *config.S3Definition) { s.DefaultKind = config.BucketKindNative })); err != nil {
		t.Fatal(err)
	}
	if _, c := f.counts(); c != 3 || r.topology.Load().backings["remote"] != remote {
		t.Fatalf("default kind change: committed %d, backing replaced: %v", c, r.topology.Load().backings["remote"] != remote)
	}
}

// A connection change prepared before a failed save is never committed.
func TestConfigureBackingsReconfigureSaveFailure(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{}
	r := managedS3Router(t, "", f, true)
	in, _ := r.BackingConfiguration()
	in.Backings = []config.BackingDefinition{s3Def("secret-one")}
	out, err := r.ConfigureBackings(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateBucket(ctx, "photos", storage.CreateBucketOptions{Backing: "remote"}); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.adminMu.Lock()
	r.management.path = filepath.Join(blocked, "backings.json")
	r.adminMu.Unlock()
	if _, err := r.ConfigureBackings(ctx, withS3(out, func(s *config.S3Definition) { s.SecretKey = "secret-two" })); err == nil {
		t.Fatal("save succeeded through a file parent")
	}
	if p, c := f.counts(); p != 1 || c != 0 {
		t.Fatalf("prepared %d, committed %d", p, c)
	}
	if got, _ := r.BackingConfiguration(); got.Revision != out.Revision {
		t.Fatal("failed save changed the revision")
	}
}

// Without BackingOpener.Reconfigure a connection change replaces the
// backing, which needs it unused.
func TestConfigureBackingsWithoutReconfigure(t *testing.T) {
	ctx := context.Background()
	r := managedS3Router(t, "", &fakeS3{}, false)
	in, _ := r.BackingConfiguration()
	in.Backings = []config.BackingDefinition{s3Def("secret-one")}
	out, err := r.ConfigureBackings(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	old := r.topology.Load().backings["remote"]
	if out, err = r.ConfigureBackings(ctx, withS3(out, func(s *config.S3Definition) { s.SecretKey = "secret-two" })); err != nil {
		t.Fatal(err)
	}
	if r.topology.Load().backings["remote"] == old {
		t.Fatal("unused backing was not replaced")
	}
	if err := r.CreateBucket(ctx, "photos", storage.CreateBucketOptions{Backing: "remote"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ConfigureBackings(ctx, withS3(out, func(s *config.S3Definition) { s.SecretKey = "secret-three" })); !errors.Is(err, ErrConflict) {
		t.Fatalf("replacing a backing in use: %v", err)
	}
}

// pendingS3Router has an s3 backing "remote" that could not be listed at
// startup (its adoption is pending) and is not probed again by the test.
func pendingS3Router(t *testing.T, f *fakeS3) *Router {
	t.Helper()
	down := &faulty{Backend: memory.New()}
	down.down.Store(true)
	r := openRouter(t, Options{
		Backings: []Backing{
			{Name: "default", Kind: "memory", Backend: memory.New()},
			{Name: "remote", Kind: "s3", Backend: down},
		},
		HealthInterval: time.Hour,
	})
	base := config.StorageConfig{Backend: "memory", Backings: []config.BackingConfig{s3Def("secret-one").Config()}}
	r.EnableBackingManagement(base, "", f.opener(true))
	return r
}

// A backing that was never listed and holds no placement can be removed
// live, although its contents are unknown.
func TestConfigureBackingsRemovesUnlistedBacking(t *testing.T) {
	ctx := context.Background()
	r := pendingS3Router(t, &fakeS3{})
	if _, err := r.GetBucket(ctx, "x"); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("unknown bucket while remote is unlisted: %v", err)
	}
	in, _ := r.BackingConfiguration()
	in.Backings = nil
	if _, err := r.ConfigureBackings(ctx, in); err != nil {
		t.Fatalf("removing an unlisted backing: %v", err)
	}
	if len(r.Backings()) != 1 {
		t.Fatal("backing not removed")
	}
	if _, err := r.GetBucket(ctx, "x"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("unknown bucket after the removal: %v", err)
	}
}

// A failed reconfiguration of a backing that was never listed replaces it;
// the replacement's buckets are adopted.
func TestConfigureBackingsFixesUnlistedBacking(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{prepareErr: errors.New("no gos3.json marker with the old endpoint")}
	f.newBackend = func() storage.Backend {
		m := memory.New()
		if err := m.CreateBucket(ctx, "found", storage.CreateBucketOptions{}); err != nil {
			t.Error(err)
		}
		return m
	}
	r := pendingS3Router(t, f)
	in, _ := r.BackingConfiguration()
	if _, err := r.ConfigureBackings(ctx, withS3(in, func(s *config.S3Definition) { s.Endpoint = "http://127.0.0.1:9001" })); err != nil {
		t.Fatalf("fixing an unlisted backing: %v", err)
	}
	if p, err := r.Placement("found"); err != nil || p.Primary != "remote" {
		t.Fatalf("bucket of the fixed backing: %+v, %v", p, err)
	}
	if st := r.Backings(); !st[1].Healthy {
		t.Fatalf("fixed backing: %+v", st[1])
	}
	if _, err := r.GetBucket(ctx, "nothing"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("unknown bucket after the fix: %v", err)
	}
}

// Opening a backing does not hold the placement lock; a configuration
// changed meanwhile makes the slower change fail.
func TestConfigureBackingsOpensWithoutLock(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{opening: make(chan struct{}), openGate: make(chan struct{})}
	r := managedS3Router(t, "", f, true)
	in, _ := r.BackingConfiguration()
	slow := in
	slow.Backings = []config.BackingDefinition{s3Def("secret-one")}
	done := make(chan error, 1)
	go func() {
		_, err := r.ConfigureBackings(ctx, slow)
		done <- err
	}()
	<-f.opening
	fast := in
	fast.Backings = []config.BackingDefinition{{Name: "fast", Backend: "memory"}}
	if _, err := r.ConfigureBackings(ctx, fast); err != nil {
		t.Fatalf("configuration while another opens a backing: %v", err)
	}
	if err := r.CreateBucket(ctx, "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	close(f.openGate)
	if err := <-done; !errors.Is(err, ErrConflict) {
		t.Fatalf("change based on a stale revision: %v", err)
	}
	if st := r.Backings(); len(st) != 2 || st[1].Name != "fast" {
		t.Fatalf("Backings() = %+v", st)
	}
}
