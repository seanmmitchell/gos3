package s3account

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// testLog is a slog handler writing warnings and errors to the test log
// while the test runs.
type testLog struct {
	mu   sync.Mutex
	t    testing.TB
	done bool
}

func (l *testLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.done {
		l.t.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

// testLogger returns a logger writing to t's log until t ends.
func testLogger(t testing.TB) *slog.Logger {
	l := &testLog{t: t}
	t.Cleanup(func() {
		l.mu.Lock()
		l.done = true
		l.mu.Unlock()
	})
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// testConn returns a connection to remote r.
func testConn(t testing.TB, r *s3clienttest.Remote) s3store.Connection {
	return s3store.Connection{
		Client: r.Client(t), Endpoint: r.URL, Region: s3clienttest.Region, AccessKey: s3clienttest.AccessKey,
		SecretKey: s3clienttest.SecretKey, PathStyle: true, PartSize: 5 << 20,
	}
}

// fastStores makes test stores renew their leases often and skip the
// orphan recovery.
func fastStores(o *s3store.Options) {
	o.LeaseInterval = 200 * time.Millisecond
	o.RecoveryInterval = -1
}

// testOptions returns options of a backing named name on r.
func testOptions(t testing.TB, r *s3clienttest.Remote, claims Claims, name string) Options {
	return Options{
		Name:    name,
		Conn:    testConn(t, r),
		Claims:  claims,
		TempDir: t.TempDir(),
		Logger:  testLogger(t),
		Tune:    fastStores,
	}
}

// homeOptions returns options of a backing with its home at bucket/prefix
// of r.
func homeOptions(t testing.TB, r *s3clienttest.Remote, claims Claims, bucket, prefix string) Options {
	o := testOptions(t, r, claims, "acct")
	o.Home = &Location{Bucket: bucket, Prefix: prefix}
	return o
}

// localOptions returns options of a backing keeping its records in dir.
func localOptions(t testing.TB, r *s3clienttest.Remote, claims Claims, dir string) Options {
	o := testOptions(t, r, claims, "acct")
	o.LocalDir = dir
	return o
}

// testHooks are the hooks of test backings.
func testHooks() hooks {
	return hooks{retryMin: 20 * time.Millisecond, monitorInterval: 50 * time.Millisecond}
}

// openReady opens a backing and waits until it is ready; it is closed when
// t ends.
func openReady(t testing.TB, o Options) *Backend {
	t.Helper()
	return openReadyWith(t, o, testHooks())
}

func openReadyWith(t testing.TB, o Options, h hooks) *Backend {
	t.Helper()
	b, err := open(o, h)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	waitReady(t, b)
	return b
}

// waitReady waits until b is ready.
func waitReady(t testing.TB, b *Backend) {
	t.Helper()
	select {
	case <-b.Ready():
	case <-time.After(30 * time.Second):
		t.Fatalf("backing not ready: %+v", b.Status())
	}
}

// waitContainers waits until every container of b has loaded.
func waitContainers(t testing.TB, b *Backend) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		all := true
		for _, ci := range b.Containers(context.Background()) {
			if ci.State != "ready" {
				all = false
			}
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("containers not loaded: %+v", b.Containers(context.Background()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// eventually retries cond until it holds (10s).
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// put writes an object.
func put(t testing.TB, b storage.Backend, bucket, key, data string) storage.ObjectInfo {
	t.Helper()
	ctx := context.Background()
	w, err := b.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		t.Fatalf("NewObjectWriter(%s/%s): %v", bucket, key, err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatalf("write %s/%s: %v", bucket, key, err)
	}
	info, err := w.Commit(ctx, storage.ObjectMeta{ETag: "e-" + key}, storage.CommitOptions{})
	if err != nil {
		t.Fatalf("commit %s/%s: %v", bucket, key, err)
	}
	return info
}

// read returns an object's content.
func read(t testing.TB, b storage.Backend, bucket, key string) string {
	t.Helper()
	_, r, err := b.OpenObject(context.Background(), bucket, key, "")
	if err != nil {
		t.Fatalf("OpenObject(%s/%s): %v", bucket, key, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, key, err)
	}
	return string(data)
}

// remotePut writes an object directly on the remote.
func remotePut(t testing.TB, c *s3client.Client, bucket, key, data string) {
	t.Helper()
	if _, err := c.PutObject(context.Background(), bucket, key, bytes.NewReader([]byte(data)), int64(len(data)), s3client.PutOptions{}); err != nil {
		t.Fatalf("remote put %s/%s: %v", bucket, key, err)
	}
}

// remoteHasBucket reports whether the remote has bucket.
func remoteHasBucket(t testing.TB, r *s3clienttest.Remote, bucket string) bool {
	t.Helper()
	_, err := r.Backend.GetBucket(context.Background(), bucket)
	switch {
	case err == nil:
		return true
	case errors.Is(err, storage.ErrBucketNotFound):
		return false
	}
	t.Fatalf("remote GetBucket(%s): %v", bucket, err)
	return false
}

// bucketKinds returns name -> kind of b's buckets.
func bucketKinds(t testing.TB, b storage.Backend) map[string]storage.BucketKind {
	t.Helper()
	list, err := b.ListBuckets(context.Background())
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	out := map[string]storage.BucketKind{}
	for _, info := range list {
		out[info.Name] = info.Kind
	}
	return out
}

// wantErr fails t unless err wraps target.
func wantErr(t testing.TB, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s: got %v, want an error wrapping %v", what, err, target)
	}
}

// noErr fails t on err.
func noErr(t testing.TB, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
