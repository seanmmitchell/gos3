package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// benv is a remote with a bucket for backend tests.
type benv struct {
	t      *testing.T
	remote *s3clienttest.Remote
	client *s3client.Client
}

const storeBucket = "store"

func newBenv(t *testing.T) *benv {
	t.Helper()
	r := s3clienttest.Start(t)
	r.CreateBucket(t, storeBucket)
	return &benv{t: t, remote: r, client: r.Client(t)}
}

func (v *benv) options(prefix string) Options {
	return Options{
		Name:             "test",
		Client:           v.client,
		Bucket:           storeBucket,
		Prefix:           prefix,
		PartSize:         5 << 20,
		TempDir:          v.t.TempDir(),
		LeaseInterval:    200 * time.Millisecond,
		RecoveryInterval: -1,
		Logger:           testLogger(),
		SnapshotPolicy:   SnapshotPolicy{MaxJournalBytes: -1, MaxBatches: -1, MaxAge: -1, CloseBatches: -1},
		retryMin:         20 * time.Millisecond,
	}
}

// open opens a backend (not waiting for it).
func (v *benv) open(opts Options) *Backend {
	v.t.Helper()
	b, err := Open(opts)
	if err != nil {
		v.t.Fatal(err)
	}
	v.t.Cleanup(func() { _ = b.Close() })
	return b
}

// ready opens a backend and waits until it serves.
func (v *benv) ready(opts Options) *Backend {
	v.t.Helper()
	b := v.open(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.WaitReady(ctx); err != nil {
		v.t.Fatalf("WaitReady: %v", err)
	}
	return b
}

func (v *benv) list(prefix string) []string {
	v.t.Helper()
	var out []string
	err := v.client.Walk(context.Background(), storeBucket, prefix, "", func(o s3client.ObjectAttrs) error {
		out = append(out, o.Key)
		return nil
	})
	if err != nil {
		v.t.Fatal(err)
	}
	return out
}

func putObject(t *testing.T, b storage.Backend, bucket, key string, data []byte) storage.ObjectInfo {
	t.Helper()
	w, err := b.NewObjectWriter(t.Context(), bucket, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	info, err := w.Commit(t.Context(), storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func readObject(t *testing.T, b storage.Backend, bucket, key, version string) []byte {
	t.Helper()
	_, r, err := b.OpenObject(t.Context(), bucket, key, version)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// waitFor polls cond for up to 10s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReadersSurviveOverwriteAndDelete(t *testing.T) {
	v := newBenv(t)
	b := v.ready(v.options("p/"))
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	first := bytes.Repeat([]byte("first-"), 20000)
	putObject(t, b, "bk", "k", first)
	_, r, err := b.OpenObject(ctx, "bk", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 100)
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatal(err)
	}
	putObject(t, b, "bk", "k", []byte("second"))
	if _, err := b.DeleteObject(ctx, "bk", "k", ""); err != nil {
		t.Fatal(err)
	}
	// The second version's blob goes; the first stays while it is read.
	waitFor(t, "the second blob's delete", func() bool { b.flushDeletes(); return len(v.list("p/data/")) == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := v.list("p/data/"); len(got) != 1 {
		t.Fatalf("data blobs while the reader is open = %v, want only the first version's", got)
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(head, rest...), first) {
		t.Fatal("reader content changed after overwrite and delete")
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if again, _ := io.ReadAll(r); !bytes.Equal(again, first) {
		t.Fatal("re-read after seek differs")
	}
	_ = r.Close()
	waitFor(t, "deferred blob delete", func() bool { return len(v.list("p/data/")) == 0 })
}

func TestCompleteComposesServerSide(t *testing.T) {
	v := newBenv(t)
	b := v.ready(v.options("p/"))
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	var copies atomic.Int32
	f := func(r *http.Request) int {
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			copies.Add(1)
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	defer v.remote.Fail.Store(nil)

	upload := func(key string, sizes ...int) ([]byte, storage.UploadInfo) {
		u, err := b.CreateMultipartUpload(ctx, "bk", key, storage.ObjectMeta{}, "")
		if err != nil {
			t.Fatal(err)
		}
		var all []byte
		for i, n := range sizes {
			data := bytes.Repeat([]byte{byte('a' + i)}, n)
			w, err := b.NewPartWriter(ctx, "bk", key, u.UploadID, i+1)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(data)
			if _, err := w.Commit(ctx, storage.PartMeta{ETag: fmt.Sprint(i)}); err != nil {
				t.Fatal(err)
			}
			all = append(all, data...)
		}
		return all, u
	}
	want, u := upload("big", 5<<20, 5<<20, 100)
	if _, err := b.CompleteMultipartUpload(ctx, "bk", "big", u.UploadID, []int{1, 2, 3}, storage.ObjectMeta{ETag: "x-3"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if copies.Load() != 3 {
		t.Fatalf("UploadPartCopy requests = %d, want 3 (server-side compose)", copies.Load())
	}
	if got := readObject(t, b, "bk", "big", ""); !bytes.Equal(got, want) {
		t.Fatalf("composed object differs (%d bytes, want %d)", len(got), len(want))
	}
	waitFor(t, "part blobs deleted", func() bool { b.flushDeletes(); return len(v.list("p/parts/")) == 0 })

	// Small parts are streamed, as is anything compressed.
	copies.Store(0)
	want, u = upload("small", 1000, 10)
	if _, err := b.CompleteMultipartUploadWith(ctx, "bk", "small", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "y-2"}, storage.CommitOptions{},
		storage.WriteOptions{Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 5}}); err != nil {
		t.Fatal(err)
	}
	if copies.Load() != 0 {
		t.Fatalf("UploadPartCopy requests = %d for small parts", copies.Load())
	}
	info, err := b.GetObjectInfo(ctx, "bk", "small", "")
	if err != nil || info.Compression != storage.CompressionDeflate {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if got := readObject(t, b, "bk", "small", ""); !bytes.Equal(got, want) {
		t.Fatal("streamed object differs")
	}
}

func TestOrphanRecovery(t *testing.T) {
	v := newBenv(t)
	opts := v.options("p/")
	opts.OrphanRetention = time.Minute
	b := v.ready(opts)
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	putObject(t, b, "bk", "kept", []byte("referenced data"))
	u, err := b.CreateMultipartUpload(ctx, "bk", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := b.NewPartWriter(ctx, "bk", "mp", u.UploadID, 1)
	_, _ = w.Write([]byte("part"))
	if _, err := w.Commit(ctx, storage.PartMeta{}); err != nil {
		t.Fatal(err)
	}
	b.mu.RLock()
	bid := b.st.buckets["bk"].id
	b.mu.RUnlock()
	put := func(key string) {
		if _, err := v.client.PutObject(ctx, storeBucket, key, strings.NewReader("x"), 1, s3client.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	strayData := b.dataKey(bid, randomID(16))
	strayPart := b.partKey(randomID(24), 1, randomID(16))
	pendingKey := b.dataKey(bid, randomID(16))
	unrecognized := "p/data/not-ours"
	for _, k := range []string{strayData, strayPart, pendingKey, unrecognized} {
		put(k)
	}
	b.pending.add(pendingKey)

	// Young orphans are kept.
	st, err := b.recoverOrphans(ctx)
	if err != nil || st.deleted != 0 || st.young != 2 {
		t.Fatalf("young pass: %+v, %v", st, err)
	}
	b.now = func() time.Time { return time.Now().Add(time.Hour) }
	st, err = b.recoverOrphans(ctx)
	if err != nil || st.deleted != 2 {
		t.Fatalf("pass: %+v, %v", st, err)
	}
	all := v.list("p/")
	for _, k := range []string{strayData, strayPart} {
		if slices.Contains(all, k) {
			t.Errorf("orphan %s kept", k)
		}
	}
	for _, k := range []string{pendingKey, unrecognized} {
		if !slices.Contains(all, k) {
			t.Errorf("%s deleted", k)
		}
	}
	if got := readObject(t, b, "bk", "kept", ""); string(got) != "referenced data" {
		t.Fatalf("referenced object damaged: %q", got)
	}
	if len(v.list("p/parts/")) != 1 {
		t.Fatal("referenced part deleted")
	}
}

func TestLazyOpenWhileRemoteDown(t *testing.T) {
	v := newBenv(t)
	f := func(*http.Request) int { return http.StatusServiceUnavailable }
	v.remote.Fail.Store(&f)
	b := v.open(v.options("p/"))
	ctx := t.Context()
	waitFor(t, "a failed open", func() bool { return b.Status().Err != nil })
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("CreateBucket while down = %v", err)
	}
	if _, err := b.ListBuckets(ctx); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("ListBuckets while down = %v", err)
	}
	select {
	case <-b.Ready():
		t.Fatal("ready while the remote is down")
	default:
	}
	v.remote.Fail.Store(nil)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := b.WaitReady(wctx); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); st.State != StateServing {
		t.Fatalf("status = %+v", st)
	}
}

func TestOpenHardErrorsReported(t *testing.T) {
	v := newBenv(t)
	v.remote.IgnoreConditionals.Store(true)
	b := v.open(v.options("p/"))
	waitFor(t, "a failed open", func() bool { return b.Status().Err != nil })
	_, err := b.ListBuckets(t.Context())
	if !errors.Is(err, storage.ErrBackingUnavailable) || !errors.Is(err, ErrNoConditionalWrites) {
		t.Fatalf("ListBuckets = %v", err)
	}
	if !strings.Contains(err.Error(), "conditional writes") || !strings.Contains(err.Error(), b.Location()) {
		t.Fatalf("error message %q lacks the cause or location", err)
	}
	if b.Location() != "s3://store/p/" {
		t.Fatalf("Location = %q", b.Location())
	}
}

func TestFailStopReloads(t *testing.T) {
	v := newBenv(t)
	opts := v.options("p/")
	opts.StallTimeout = 300 * time.Millisecond
	b := v.ready(opts)
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	putObject(t, b, "bk", "a", []byte("1"))
	f := func(r *http.Request) int {
		if strings.Contains(r.URL.Path, "/meta/journal/") {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	w, _ := b.NewObjectWriter(ctx, "bk", "b")
	_, _ = w.Write([]byte("2"))
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("commit during a journal outage = %v", err)
	}
	if _, err := b.GetObjectInfo(ctx, "bk", "a", ""); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("read after fail-stop = %v", err)
	}
	v.remote.Fail.Store(nil)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	waitFor(t, "reload", func() bool { return b.WaitReady(wctx) == nil && b.Status().State == StateServing })
	if got := readObject(t, b, "bk", "a", ""); string(got) != "1" {
		t.Fatalf("after reload a = %q", got)
	}
	if _, err := b.GetObjectInfo(ctx, "bk", "b", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("unacknowledged write after reload: %v", err)
	}
}

func TestPrepareConnection(t *testing.T) {
	v := newBenv(t)
	kek := bytes.Repeat([]byte{7}, 32)
	opts := v.options("p/")
	opts.KEK = kek
	b := v.ready(opts)
	ctx := t.Context()

	// Same backing through a new client: accepted and used.
	c2 := v.remote.Client(t)
	commit, err := b.PrepareConnection(ctx, Connection{Client: c2, KEK: kek})
	if err != nil {
		t.Fatal(err)
	}
	commit()
	if b.client() != c2 || b.eng.client() != c2 {
		t.Fatal("commit did not swap the client")
	}
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	// Built from settings rather than a client.
	cfg := v.remote.Config()
	if _, err := b.PrepareConnection(ctx, Connection{Endpoint: cfg.Endpoint, Region: cfg.Region, AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey, PathStyle: true}); err != nil {
		t.Fatalf("settings connection: %v", err)
	}
	// A wrong KEK.
	if _, err := b.PrepareConnection(ctx, Connection{Client: c2, KEK: bytes.Repeat([]byte{8}, 32)}); !errors.Is(err, ErrWrongKEK) {
		t.Fatalf("wrong KEK: %v", err)
	}
	// Another service: no marker, then another backing's marker.
	other := newBenv(t)
	if _, err := b.PrepareConnection(ctx, Connection{Client: other.client}); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("no marker: %v", err)
	}
	ob := other.ready(other.options("p/"))
	_ = ob.Close()
	if _, err := b.PrepareConnection(ctx, Connection{Client: other.client}); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("other backing: %v", err)
	}
	if b.client() != c2 {
		t.Fatal("refused connection was used")
	}
}

func TestGatewayBacking(t *testing.T) {
	v := newBenv(t)
	opts := v.options("gw/")
	opts.Gateway = true
	b := v.ready(opts)
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{Backing: "gw"}); err != nil {
		t.Fatal(err)
	}
	if info, err := b.GetBucket(ctx, "bk"); err != nil || info.Backing != "gw" {
		t.Fatalf("GetBucket = %+v, %v", info, err)
	}
	if err := b.PutBucketConfig(ctx, "bk", storage.ConfigTagging, []byte("<t/>")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.NewObjectWriter(ctx, "bk", "k"); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("NewObjectWriter = %v", err)
	}
	if _, err := b.CreateMultipartUpload(ctx, "bk", "k", storage.ObjectMeta{}, ""); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("CreateMultipartUpload = %v", err)
	}
	if err := b.WalkObjects(ctx, "bk", "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("WalkObjects = %v", err)
	}
	if err := b.SetBucketVersioning(ctx, "bk", storage.VersioningEnabled); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("SetBucketVersioning = %v", err)
	}

	aux := b.AuxState("gw")
	for k, val := range map[string]string{"up/1": "a", "up/2": "b", "obj/x": "c"} {
		if err := aux.PutRecord(ctx, k, []byte(val)); err != nil {
			t.Fatal(err)
		}
	}
	if err := aux.DeleteRecords(ctx, "up/"); err != nil {
		t.Fatal(err)
	}
	staged := bytes.Repeat([]byte("staged"), 1000)
	if err := aux.PutStaged(ctx, "u1/00001", bytes.NewReader(staged), int64(len(staged))); err != nil {
		t.Fatal(err)
	}
	if err := aux.PutStaged(ctx, "u1/00002", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatal(err)
	}
	if err := aux.DeleteStaged(ctx, "u1/00002"); err != nil {
		t.Fatal(err)
	}
	check := func(b *Backend, what string) {
		t.Helper()
		aux := b.AuxState("gw")
		recs, err := aux.Records(ctx)
		if err != nil || len(recs) != 1 || string(recs["obj/x"]) != "c" {
			t.Fatalf("%s: records = %v, %v", what, recs, err)
		}
		names, err := aux.ListStaged(ctx)
		if err != nil || !slices.Equal(names, []string{"u1/00001"}) {
			t.Fatalf("%s: staged = %v, %v", what, names, err)
		}
		r, err := aux.OpenStaged(ctx, "u1/00001", int64(len(staged)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || !bytes.Equal(got, staged) {
			t.Fatalf("%s: staged content differs (%v)", what, err)
		}
		if data, err := b.GetBucketConfig(ctx, "bk", storage.ConfigTagging); err != nil || string(data) != "<t/>" {
			t.Fatalf("%s: config = %q, %v", what, data, err)
		}
	}
	check(b, "before reopen")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = v.ready(opts)
	check(b, "after reopen")
	if err := b.eng.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	b = v.ready(opts)
	check(b, "after snapshot and reopen")
	if got := v.list("gw/gateway/parts/"); !slices.Equal(got, []string{"gw/gateway/parts/gw/u1/00001"}) {
		t.Fatalf("staged blobs = %v", got)
	}
	// A regular backing refuses the gateway prefix.
	opts.Gateway = false
	_ = b.Close()
	rb := v.open(opts)
	waitFor(t, "refusal", func() bool { return errors.Is(rb.Status().Err, ErrMarkerMismatch) })
	if err := b.AuxState("gw").PutRecord(ctx, "k", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed backend: %v", err)
	}
	if err := rb.AuxState("gw").PutRecord(ctx, "k", nil); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("aux on a regular backing: %v", err)
	}
}
