package s3account

// Regression tests for defects found in review: stale native
// configurations after a delete/re-create, connection changes missing
// containers, a pinned KEK after a failed preparation, wrappers hiding
// optional interfaces, encryption providers of move targets and
// concurrent containers at one location.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// R1: PutBucketConfig on a native bucket looks the bucket up before taking
// adminMu and never re-checks: racing a DeleteBucket, it writes a config
// for a bucket that no longer exists, and a native bucket created later
// under the same name inherits it.
func TestRegressionStaleConfigRace(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	dir := t.TempDir()
	h := testHooks()
	var b *Backend
	var putErr error
	putDone := make(chan struct{})
	var once sync.Once
	h.step = func(name string) error {
		if name != "delete-native:detached" {
			return nil
		}
		once.Do(func() {
			go func() {
				defer close(putDone)
				putErr = b.PutBucketConfig(ctx, "web", storage.ConfigPolicy, []byte(`{"stale":"public-read"}`))
			}()
			// Let the put look the bucket up and block on adminMu.
			time.Sleep(300 * time.Millisecond)
		})
		return nil
	}
	b = openReadyWith(t, localOptions(t, r, NewClaims(), dir), h)
	noErr(t, "create", b.CreateBucket(ctx, "web", storage.CreateBucketOptions{}))
	if k := bucketKinds(t, b)["web"]; k != storage.BucketKindNative {
		t.Fatalf("kind %q", k)
	}
	noErr(t, "delete", b.DeleteBucket(ctx, "web"))
	<-putDone
	t.Logf("PutBucketConfig racing DeleteBucket returned: %v", putErr)
	noErr(t, "re-create", b.CreateBucket(ctx, "web", storage.CreateBucketOptions{}))
	data, err := b.GetBucketConfig(ctx, "web", storage.ConfigPolicy)
	if err == nil {
		t.Errorf("BUG: the new native bucket 'web' has the deleted bucket's policy (in memory): %s", data)
	} else {
		t.Logf("in memory: %v", err)
	}
	noErr(t, "close", b.Close())
	b2 := openReady(t, localOptions(t, r, NewClaims(), dir))
	data, err = b2.GetBucketConfig(ctx, "web", storage.ConfigPolicy)
	if err == nil {
		t.Errorf("BUG: after restart the new native bucket 'web' has the deleted bucket's policy: %s", data)
	} else {
		t.Logf("after restart: %v", err)
	}
}

// failingDeletes fails DeleteRecords for one prefix once.
type failingDeletes struct {
	s3store.AuxStore
	fail atomic.Pointer[string]
}

func (f *failingDeletes) DeleteRecords(ctx context.Context, prefix string) error {
	if p := f.fail.Load(); p != nil && prefix == *p {
		f.fail.Store(nil)
		return fmt.Errorf("injected delete failure: %w", storage.ErrBackingUnavailable)
	}
	return f.AuxStore.DeleteRecords(ctx, prefix)
}

// R2: a failed deletion of a dropped native bucket's configuration records
// is "retried at the next start" only when no native bucket of that name
// exists then; a bucket re-created meanwhile inherits them at the restart.
func TestRegressionStaleConfigAfterFailedDelete(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	dir := t.TempDir()
	var fd *failingDeletes
	h := testHooks()
	h.wrapRecords = func(s s3store.AuxStore) s3store.AuxStore {
		fd = &failingDeletes{AuxStore: s}
		return fd
	}
	b := openReadyWith(t, localOptions(t, r, NewClaims(), dir), h)
	noErr(t, "create", b.CreateBucket(ctx, "web", storage.CreateBucketOptions{}))
	noErr(t, "policy", b.PutBucketConfig(ctx, "web", storage.ConfigPolicy, []byte(`{"old":"public-read"}`)))
	p := configPrefix("web")
	fd.fail.Store(&p)
	noErr(t, "delete", b.DeleteBucket(ctx, "web"))
	noErr(t, "re-create", b.CreateBucket(ctx, "web", storage.CreateBucketOptions{}))
	if _, err := b.GetBucketConfig(ctx, "web", storage.ConfigPolicy); err == nil {
		t.Errorf("stale policy in memory already")
	}
	noErr(t, "close", b.Close())
	b2 := openReady(t, localOptions(t, r, NewClaims(), dir))
	data, err := b2.GetBucketConfig(ctx, "web", storage.ConfigPolicy)
	if err == nil {
		t.Errorf("BUG: after restart the re-created native bucket has the deleted bucket's policy: %s", data)
	} else {
		t.Logf("after restart: %v", err)
	}
}

// countingProxy is a reverse proxy to r counting the requests it forwards.
func countingProxy(t *testing.T, r *s3clienttest.Remote) (*httptest.Server, *atomic.Int64) {
	u, _ := url.Parse(r.URL)
	var n atomic.Int64
	rp := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n.Add(1)
		rp.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func proxyConn(t *testing.T, p *httptest.Server) s3store.Connection {
	c, err := s3client.New(s3client.Config{Endpoint: p.URL, Region: s3clienttest.Region, AccessKey: s3clienttest.AccessKey,
		SecretKey: s3clienttest.SecretKey, PathStyle: true, MaxAttempts: 2})
	noErr(t, "client", err)
	return s3store.Connection{Client: c, Endpoint: p.URL, Region: s3clienttest.Region, AccessKey: s3clienttest.AccessKey,
		SecretKey: s3clienttest.SecretKey, PathStyle: true, PartSize: 5 << 20}
}

// R3a: a container added between PrepareConnection and its commit keeps
// the old connection.
func TestRegressionPrepareConnectionMissesAddedContainer(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	b := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	p, n := countingProxy(t, r)
	commit, err := b.PrepareConnection(ctx, proxyConn(t, p))
	noErr(t, "prepare", err)
	ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "cont", CreateBucket: true})
	noErr(t, "AddContainer", err)
	commit()
	noErr(t, "create wrapped", b.CreateBucket(ctx, "w", storage.CreateBucketOptions{Target: &storage.BucketTarget{Container: ci.ID}}))
	before := n.Load()
	for i := range 5 {
		put(t, b, "w", fmt.Sprintf("k%d", i), strings.Repeat("x", 100))
	}
	if got := n.Load() - before; got == 0 {
		t.Errorf("BUG: after commit, the container added between prepare and commit sent 0 requests over the new connection (still on the old one)")
	} else {
		t.Logf("requests over the new connection: %d", got)
	}
	// Control: a container added after the commit uses the new connection.
	ci2, err := b.AddContainer(ctx, ContainerSpec{Bucket: "cont2", CreateBucket: true})
	noErr(t, "AddContainer2", err)
	noErr(t, "create wrapped2", b.CreateBucket(ctx, "w2", storage.CreateBucketOptions{Target: &storage.BucketTarget{Container: ci2.ID}}))
	before = n.Load()
	put(t, b, "w2", "k", "v")
	t.Logf("control: requests over the new connection for a container added after commit: %d", n.Load()-before)
}

// R3b: a container that was loading at PrepareConnection and finished
// loading before the commit keeps the old connection.
func TestRegressionPrepareConnectionMissesContainerLoadedInWindow(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	dir := t.TempDir()
	b := openReady(t, localOptions(t, r, NewClaims(), dir))
	ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "cont", CreateBucket: true})
	noErr(t, "AddContainer", err)
	noErr(t, "create wrapped", b.CreateBucket(ctx, "w", storage.CreateBucketOptions{Target: &storage.BucketTarget{Container: ci.ID}}))
	noErr(t, "close", b.Close())

	var block atomic.Bool
	block.Store(true)
	f := func(req *http.Request) int {
		if block.Load() && strings.HasPrefix(req.URL.Path, "/cont") {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	r.Fail.Store(&f)
	b2 := openReady(t, localOptions(t, r, NewClaims(), dir))
	time.Sleep(200 * time.Millisecond)
	if st := b2.containerByID(ci.ID); st == nil || st.loaded() {
		t.Fatalf("container should be loading")
	}
	p, n := countingProxy(t, r)
	commit, err := b2.PrepareConnection(ctx, proxyConn(t, p))
	noErr(t, "prepare", err)
	block.Store(false)
	eventually(t, "container loaded", func() bool { return b2.containerByID(ci.ID).loaded() })
	commit()
	before := n.Load()
	for i := range 5 {
		put(t, b2, "w", fmt.Sprintf("k%d", i), "v")
	}
	if got := n.Load() - before; got == 0 {
		t.Errorf("BUG: the container that loaded between prepare and commit still uses the old connection")
	} else {
		t.Logf("requests over the new connection: %d", got)
	}
}

type prepReader struct{ io.ReadSeekCloser }

func (prepReader) Prepare() error                  { return nil }
func (prepReader) PrepareRange(int64, int64) error { return nil }

// R4: the error-classifying reader of non-home containers hides
// storage.PreparingReader (restic readers).
func TestRegressionWrapReaderHidesPrepare(t *testing.T) {
	c := &container{id: "c1"}
	in := prepReader{readCloser{bytes.NewReader(nil)}}
	if _, ok := any(in).(storage.PreparingReader); !ok {
		t.Fatal("setup")
	}
	out := c.wrapReader(in)
	if _, ok := out.(storage.PreparingReader); !ok {
		t.Errorf("BUG: container.wrapReader drops storage.PreparingReader (%T)", out)
	}
	if _, ok := out.(storage.RangePreparingReader); !ok {
		t.Errorf("container.wrapReader drops storage.RangePreparingReader (%T)", out)
	}
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

// R5: EncryptionProviders for a bucket the backing does not hold (a move
// target) answers the default container's providers: on a backing
// without a home and without a default container it answers none even
// when the move names a container that stores AES256.
func TestRegressionEncryptionProvidersMoveTarget(t *testing.T) {
	r := s3clienttest.Start(t)
	ctx := context.Background()
	o := localOptions(t, r, NewClaims(), t.TempDir())
	o.Conn.KEK = bytes.Repeat([]byte{7}, 32)
	b := openReady(t, o)
	ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "cont", CreateBucket: true})
	noErr(t, "AddContainer", err)
	caps, err := b.NewBucketCaps(ctx, storage.CreateBucketOptions{Import: true, Target: &storage.BucketTarget{Container: ci.ID}})
	noErr(t, "NewBucketCaps", err)
	t.Logf("move target caps: %+v", caps)
	p, err := b.EncryptionProviders(ctx, "moved")
	noErr(t, "EncryptionProviders(unknown)", err)
	noErr(t, "create wrapped", b.CreateBucket(ctx, "w", storage.CreateBucketOptions{Target: &storage.BucketTarget{Container: ci.ID}}))
	pw, err := b.EncryptionProviders(ctx, "w")
	noErr(t, "EncryptionProviders(w)", err)
	t.Logf("providers: unknown bucket %v, bucket in the container %v", p, pw)
	if len(p) == 0 && len(pw) > 0 {
		t.Errorf("BUG: a move target in container %s is reported as storing no encryption (%v) although the container stores %v", ci.ID, p, pw)
	}
}

// R6: two concurrent AddContainer calls for the same location: one wins,
// the other is refused by the claim.
func TestRegressionConcurrentAddSameLocation(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "shared")
	ctx := context.Background()
	b := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	var wg sync.WaitGroup
	errs := make([]error, 2)
	ids := make([]string, 2)
	for i := range 2 {
		wg.Go(func() {
			ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "shared", Prefix: "p/"})
			errs[i], ids[i] = err, ci.ID
		})
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("want exactly one container added: %v / %v (ids %v)", errs[0], errs[1], ids)
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, ErrClaimed) {
			t.Fatalf("the losing AddContainer: %v, want ErrClaimed", err)
		}
	}
	if cs := b.Containers(ctx); len(cs) != 1 {
		t.Fatalf("containers %+v", cs)
	}
	if hs := b.holders("shared"); len(hs) != 1 {
		t.Fatalf("claims on shared: %+v", hs)
	}
}

// R7: a failed PrepareConnection is not rolled back: the home's prepare
// records the new KEK's check value in the home marker before a
// container's prepare fails, pinning the backing to that KEK.
func TestRegressionPrepareConnectionKEKPinnedOnFailure(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	ctx := context.Background()
	b := openReady(t, homeOptions(t, r, NewClaims(), "home", ""))
	_, err := b.AddContainer(ctx, ContainerSpec{Bucket: "cont", CreateBucket: true})
	noErr(t, "AddContainer", err)
	waitContainers(t, b)
	var block atomic.Bool
	f := func(req *http.Request) int {
		if block.Load() && strings.HasPrefix(req.URL.Path, "/cont/gos3.json") {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	r.Fail.Store(&f)
	block.Store(true)
	c1 := testConn(t, r)
	c1.KEK = bytes.Repeat([]byte{1}, 32)
	_, err = b.PrepareConnection(ctx, c1)
	t.Logf("prepare with K1 (container marker unreachable): %v", err)
	if err == nil {
		t.Fatal("expected the prepare to fail")
	}
	block.Store(false)
	c2 := testConn(t, r)
	c2.KEK = bytes.Repeat([]byte{2}, 32)
	_, err = b.PrepareConnection(ctx, c2)
	t.Logf("prepare with K2 afterwards: %v", err)
	if err != nil {
		t.Errorf("BUG: the failed prepare left the home pinned to K1: %v", err)
	}
}
