package s3store

import (
	"bytes"
	"context"
	"encoding/json"
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
	bid := bucketID(t, b, "bk")
	strayData := b.dataKey(bid, randomID(16))
	strayPart := b.partKey(randomID(24), 1, randomID(16))
	pendingKey := b.dataKey(bid, randomID(16))
	unrecognized := "p/data/not-ours"
	for _, k := range []string{strayData, strayPart, pendingKey, unrecognized} {
		v.putTagged(k, b.Marker().BackingID)
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

// putTagged writes a one-byte object, with the blob tag of backing id
// unless id is "".
func (v *benv) putTagged(key, id string) {
	v.t.Helper()
	var h http.Header
	if id != "" {
		h = http.Header{blobTagHeader: {id}}
	}
	if _, err := v.client.PutObject(context.Background(), storeBucket, key, strings.NewReader("x"), 1, s3client.PutOptions{Header: h}); err != nil {
		v.t.Fatal(err)
	}
}

// tagOf returns the blob tag of key.
func (v *benv) tagOf(key string) string {
	v.t.Helper()
	attrs, err := v.client.HeadObject(context.Background(), storeBucket, key)
	if err != nil {
		v.t.Fatalf("HEAD %s: %v", key, err)
	}
	return blobTag(attrs)
}

// Every blob the backend writes carries its tag, and the recovery deletes
// only old unreferenced blobs with that tag: never untagged objects or
// those of another backing, whatever their names, in data/, parts/ or the
// staged blobs of aux namespaces.
func TestRecoveryDeletesOnlyTaggedBlobs(t *testing.T) {
	v := newBenv(t)
	opts := v.options("p/")
	opts.OrphanRetention = time.Minute
	b := v.ready(opts)
	ctx := t.Context()
	id := b.Marker().BackingID
	if len(id) != 32 || !b.Marker().BlobTags {
		t.Fatalf("marker = %+v", b.Marker())
	}
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	bid := bucketID(t, b, "bk")

	// Blobs of every kind are tagged: data, parts, server-side compositions
	// (of 5 MiB parts, and of a single part) and staged blobs.
	putObject(t, b, "bk", "small", []byte("data"))
	u, err := b.CreateMultipartUpload(ctx, "bk", "big", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		w, _ := b.NewPartWriter(ctx, "bk", "big", u.UploadID, n)
		_, _ = w.Write(bytes.Repeat([]byte{byte('0' + n)}, 5<<20))
		if _, err := w.Commit(ctx, storage.PartMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range v.list("p/parts/") {
		if tag := v.tagOf(k); tag != id {
			t.Errorf("part blob %s tag = %q, want %q", k, tag, id)
		}
	}
	if _, err := b.CompleteMultipartUpload(ctx, "bk", "big", u.UploadID, []int{1, 2}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	u, _ = b.CreateMultipartUpload(ctx, "bk", "one", storage.ObjectMeta{}, "")
	w, _ := b.NewPartWriter(ctx, "bk", "one", u.UploadID, 1)
	_, _ = w.Write([]byte("single part"))
	if _, err := w.Commit(ctx, storage.PartMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CompleteMultipartUpload(ctx, "bk", "one", u.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	aux := b.AuxState("ns")
	if err := aux.PutStaged(ctx, "up1-00001-AbC_d", strings.NewReader("staged"), 6); err != nil {
		t.Fatal(err)
	}
	dataBlobs := v.list("p/data/")
	if len(dataBlobs) != 3 {
		t.Fatalf("data blobs = %v", dataBlobs)
	}
	stagedBlobs := v.list("p/gateway/parts/")
	if len(stagedBlobs) != 1 {
		t.Fatalf("staged blobs = %v", stagedBlobs)
	}
	for _, k := range append(slices.Clone(dataBlobs), stagedBlobs...) {
		if tag := v.tagOf(k); tag != id {
			t.Errorf("blob %s tag = %q, want %q", k, tag, id)
		}
	}

	// Old orphans with generated names: own, untagged and foreign.
	other := randomID(16)
	type stray struct{ key, tag string }
	var own, kept []stray
	for _, tag := range []string{id, "", other} {
		set := []stray{
			{b.dataKey(bid, randomID(16)), tag},
			{b.dataKey(randomID(16), randomID(16)), tag}, // of a deleted bucket
			{b.partKey(randomID(24), 7, randomID(16)), tag},
			{b.stagedKey("ns", "up9-00002-"+randomID(4)), tag},
		}
		if tag == id {
			own = set
		} else {
			kept = append(kept, set...)
		}
	}
	// Tagged as ours, but outside the generated names or namespaces.
	kept = append(kept,
		stray{"p/data/" + bid + "/" + randomID(8), id},
		stray{"p/parts/" + randomID(16) + "/00001-" + randomID(16), id},
		stray{b.stagedKey("unknown-ns", "up1-00001-x"), id},
		stray{b.stagedKey("ns", "nested/name"), id},
	)
	for _, s := range append(slices.Clone(own), kept...) {
		v.putTagged(s.key, s.tag)
	}
	b.now = func() time.Time { return time.Now().Add(time.Hour) }
	st, err := b.recoverOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.deleted != len(own) || st.untagged != 4 || st.foreign != 4 || st.errors != 0 {
		t.Fatalf("recovery stats = %+v, want %d deleted, 4 untagged and 4 foreign kept", st, len(own))
	}
	all := v.list("p/")
	for _, s := range own {
		if slices.Contains(all, s.key) {
			t.Errorf("own orphan %s kept", s.key)
		}
	}
	for _, s := range kept {
		if !slices.Contains(all, s.key) {
			t.Errorf("%s (tag %q) deleted", s.key, s.tag)
		}
	}
	// Referenced blobs are untouched.
	for _, k := range append(slices.Clone(dataBlobs), stagedBlobs...) {
		if !slices.Contains(all, k) {
			t.Errorf("referenced blob %s deleted", k)
		}
	}
	if got := readObject(t, b, "bk", "big", ""); len(got) != 10<<20 {
		t.Fatalf("composed object has %d bytes", len(got))
	}

	// Kept objects are remembered: the next pass sends no HEAD for them,
	// until one changes.
	var heads atomic.Int32
	f := func(r *http.Request) int {
		if r.Method == http.MethodHead {
			heads.Add(1)
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	defer v.remote.Fail.Store(nil)
	st, err = b.recoverOrphans(ctx)
	if err != nil || st.deleted != 0 || st.untagged != 4 || st.foreign != 4 || heads.Load() != 0 {
		t.Fatalf("second pass: %+v, %v, %d HEADs; want the kept objects counted without HEADs", st, err, heads.Load())
	}
	changed := kept[0].key
	if _, err := v.client.PutObject(ctx, storeBucket, changed, strings.NewReader("xy"), 2, s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err = b.recoverOrphans(ctx)
	if err != nil || st.deleted != 0 || st.untagged != 4 || st.foreign != 4 || heads.Load() != 1 {
		t.Fatalf("pass after a change: %+v, %v, %d HEADs; want one HEAD (of %s)", st, err, heads.Load(), changed)
	}
}

// A recovery pass sends a bounded number of HEADs; later passes check the
// rest.
func TestRecoveryHeadLimit(t *testing.T) {
	v := newBenv(t)
	opts := v.options("p/")
	opts.OrphanRetention = time.Minute
	b := v.ready(opts)
	ctx := t.Context()
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	bid := bucketID(t, b, "bk")
	for range 5 {
		v.putTagged(b.dataKey(bid, randomID(16)), b.Marker().BackingID)
	}
	b.recoveryHeads = 2
	b.now = func() time.Time { return time.Now().Add(time.Hour) }
	var heads atomic.Int32
	f := func(r *http.Request) int {
		if r.Method == http.MethodHead {
			heads.Add(1)
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	defer v.remote.Fail.Store(nil)
	for i, want := range []recoveryStats{{deleted: 2, deferred: 3}, {deleted: 2, deferred: 1}, {deleted: 1}} {
		heads.Store(0)
		st, err := b.recoverOrphans(ctx)
		if err != nil || st != want || heads.Load() > 2 {
			t.Fatalf("pass %d: %+v, %v, %d HEADs; want %+v with at most 2 HEADs", i+1, st, err, heads.Load(), want)
		}
	}
	if got := v.list("p/data/"); len(got) != 0 {
		t.Fatalf("orphans left: %v", got)
	}
}

// AuxState is available on every backing: records and staged blobs are
// journaled, snapshotted and survive reopening, next to buckets and
// objects.
func TestAuxState(t *testing.T) {
	v := newBenv(t)
	opts := v.options("home/")
	opts.Role = RoleHome
	b := v.ready(opts)
	ctx := t.Context()
	if b.Marker().Role != RoleHome {
		t.Fatalf("marker = %+v", b.Marker())
	}
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{Backing: "gw"}); err != nil {
		t.Fatal(err)
	}
	if err := b.PutBucketConfig(ctx, "bk", storage.ConfigTagging, []byte("<t/>")); err != nil {
		t.Fatal(err)
	}
	putObject(t, b, "bk", "obj", []byte("object data"))

	aux := b.AuxState("gateway")
	for k, val := range map[string]string{"up/1": "a", "up/2": "b", "obj/x": "c"} {
		if err := aux.PutRecord(ctx, k, []byte(val)); err != nil {
			t.Fatal(err)
		}
	}
	if err := aux.DeleteRecords(ctx, "up/"); err != nil {
		t.Fatal(err)
	}
	if err := b.AuxState("account").PutRecord(ctx, "v", []byte(`{"schema":1}`)); err != nil {
		t.Fatal(err)
	}
	staged := bytes.Repeat([]byte("staged"), 1000)
	if err := aux.PutStaged(ctx, "u1-00001-a", bytes.NewReader(staged), int64(len(staged))); err != nil {
		t.Fatal(err)
	}
	if err := aux.PutStaged(ctx, "u1-00002-b", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatal(err)
	}
	if err := aux.DeleteStaged(ctx, "u1-00002-b"); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"", "a/b"} {
		if err := b.AuxState(ns).PutRecord(ctx, "k", nil); !errors.Is(err, storage.ErrInvalidName) {
			t.Fatalf("aux namespace %q: %v", ns, err)
		}
	}
	check := func(b *Backend, what string) {
		t.Helper()
		aux := b.AuxState("gateway")
		recs, err := aux.Records(ctx)
		if err != nil || len(recs) != 1 || string(recs["obj/x"]) != "c" {
			t.Fatalf("%s: records = %v, %v", what, recs, err)
		}
		if recs, err := b.AuxState("account").Records(ctx); err != nil || len(recs) != 1 || string(recs["v"]) != `{"schema":1}` {
			t.Fatalf("%s: account records = %v, %v", what, recs, err)
		}
		names, err := aux.ListStaged(ctx)
		if err != nil || !slices.Equal(names, []string{"u1-00001-a"}) {
			t.Fatalf("%s: staged = %v, %v", what, names, err)
		}
		r, err := aux.OpenStaged(ctx, "u1-00001-a", int64(len(staged)))
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
		if got := readObject(t, b, "bk", "obj", ""); string(got) != "object data" {
			t.Fatalf("%s: object = %q", what, got)
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
	if got := v.list("home/gateway/parts/"); !slices.Equal(got, []string{"home/gateway/parts/gateway/u1-00001-a"}) {
		t.Fatalf("staged blobs = %v", got)
	}
	_ = b.Close()
	if err := b.AuxState("gateway").PutRecord(ctx, "k", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed backend: %v", err)
	}
}

// CreateIfEmpty creates a backing only at a location without keys (a
// folder placeholder aside), checked again right before the marker is
// written; it opens only the marker it created (or the expected one).
func TestCreateIfEmpty(t *testing.T) {
	v := newBenv(t)
	refused := func(b *Backend) {
		t.Helper()
		waitFor(t, "the refusal", func() bool { return b.Status().Err != nil })
		if st := b.Status(); st.State != StateFailed || !errors.Is(st.Err, ErrPrefixNotEmpty) {
			t.Fatalf("status = %+v, want ErrPrefixNotEmpty", st)
		}
		if _, err := b.ListBuckets(t.Context()); !errors.Is(err, ErrPrefixNotEmpty) || !errors.Is(err, storage.ErrBackingUnavailable) {
			t.Fatalf("ListBuckets = %v", err)
		}
	}
	empty := func(prefix string) Options {
		o := v.options(prefix)
		o.Create, o.Role = CreateIfEmpty, RoleContainer
		return o
	}

	v.putTagged("full/user/file.txt", "")
	b := v.open(empty("full/"))
	refused(b)
	if err := b.Status().Err; !strings.Contains(err.Error(), "full/user/file.txt") {
		t.Fatalf("refusal %q does not name the key found", err)
	}
	_ = b.Close()
	if got := v.list("full/"); !slices.Equal(got, []string{"full/user/file.txt"}) {
		t.Fatalf("refused location now holds %v", got)
	}

	// A folder placeholder (and nothing else) is accepted.
	if _, err := v.client.PutObject(t.Context(), storeBucket, "folder/", strings.NewReader(""), 0, s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	b = v.ready(empty("folder/"))
	if mk := b.Marker(); mk.Role != RoleContainer || !mk.BlobTags {
		t.Fatalf("marker = %+v", mk)
	}
	id := b.Marker().BackingID
	// A reload after a fail-stop accepts the backing's own marker.
	b.eng.failCurrent(errors.New("test"))
	waitFor(t, "the reload", func() bool { return b.Status().State == StateServing && b.WaitReady(t.Context()) == nil })
	if b.Marker().BackingID != id {
		t.Fatal("reload opened another backing")
	}
	_ = b.Close()
	// Another Backend did not create it: refused, unless expected.
	b = v.open(empty("folder/"))
	refused(b)
	_ = b.Close()
	o := empty("folder/")
	o.ExpectBackingID = id
	b = v.ready(o)
	if b.Marker().BackingID != id {
		t.Fatal("reopen created another backing")
	}
	_ = b.Close()
	// A non-empty placeholder is a key like any other.
	if _, err := v.client.PutObject(t.Context(), storeBucket, "nonempty/", strings.NewReader("x"), 1, s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	b = v.open(empty("nonempty/"))
	refused(b)
	_ = b.Close()

	// A key appearing after the first check (during the probe) is found
	// by the check right before the marker is written.
	var once atomic.Bool
	injected := make(chan error, 1)
	f := func(r *http.Request) int {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/race/meta/probe-") && once.CompareAndSwap(false, true) {
			// A request of its own, served while this one waits.
			_, err := v.client.PutObject(context.Background(), storeBucket, "race/late.txt", strings.NewReader("x"), 1, s3client.PutOptions{})
			injected <- err
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	b = v.open(empty("race/"))
	refused(b)
	_ = b.Close()
	v.remote.Fail.Store(nil)
	select {
	case err := <-injected:
		if err != nil {
			t.Fatalf("injecting a key during the probe: %v", err)
		}
	default:
		t.Fatal("the probe was not intercepted")
	}
	if got := v.list("race/"); !slices.Equal(got, []string{"race/late.txt"}) {
		t.Fatalf("raced location holds %v, want only the late key (no marker)", got)
	}

	// A marker someone else creates right before this backing's own
	// conditional PUT makes that PUT fail; the other marker is not
	// adopted.
	foreign := Marker{Format: markerFormat, Kind: markerKind, BackingID: strings.Repeat("f", 32), BlobTags: true,
		Created: time.Now().UTC().Truncate(time.Second)}
	raw, _ := json.Marshal(foreign)
	var raced atomic.Bool
	injected = make(chan error, 1)
	f = func(r *http.Request) int {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/race2/"+markerKey) && r.Header.Get("If-None-Match") == "*" &&
			raced.CompareAndSwap(false, true) {
			_, err := v.client.PutObject(context.Background(), storeBucket, "race2/"+markerKey, bytes.NewReader(raw), int64(len(raw)),
				s3client.PutOptions{IfNoneMatch: true})
			injected <- err
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	b = v.open(empty("race2/"))
	refused(b)
	_ = b.Close()
	v.remote.Fail.Store(nil)
	if err := <-injected; err != nil {
		t.Fatalf("injecting a foreign marker: %v", err)
	}
	if m, _, err := ReadMarker(t.Context(), v.client, storeBucket, "race2/"); err != nil || m.BackingID != foreign.BackingID {
		t.Fatalf("marker = %+v, %v; want the foreign one untouched", m, err)
	}

	// The answer to the backing's own marker PUT is lost: the marker read
	// back is recognized as its own.
	var lost atomic.Bool
	after := func(r *http.Request) s3clienttest.Fault {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/lost/"+markerKey) && r.Header.Get("If-None-Match") == "*" &&
			lost.CompareAndSwap(false, true) {
			return s3clienttest.Status500
		}
		return s3clienttest.Pass
	}
	v.remote.After.Store(&after)
	b = v.ready(empty("lost/"))
	v.remote.After.Store(nil)
	if !lost.Load() {
		t.Fatal("the marker PUT was not intercepted")
	}
	if m, _, err := ReadMarker(t.Context(), v.client, storeBucket, "lost/"); err != nil || m.BackingID != b.Marker().BackingID || m.Role != RoleContainer {
		t.Fatalf("marker = %+v, %v; backend has %+v", m, err, b.Marker())
	}
}

// MustExist and ExpectBackingID never create a backing and refuse another
// one; the refusals are hard errors reported through Status.
func TestOpenExistingBacking(t *testing.T) {
	v := newBenv(t)
	opts := v.options("c/")
	opts.Create = MustExist
	b := v.open(opts)
	waitFor(t, "the refusal", func() bool { return b.Status().Err != nil })
	if st := b.Status(); st.State != StateFailed || !errors.Is(st.Err, ErrMarkerMissing) {
		t.Fatalf("status = %+v, want ErrMarkerMissing", st)
	}
	_ = b.Close()
	if got := v.list("c/"); len(got) != 0 {
		t.Fatalf("MustExist wrote %v", got)
	}

	b = v.ready(v.options("c/"))
	id := b.Marker().BackingID
	_ = b.Close()
	b = v.ready(opts)
	if b.Marker().BackingID != id {
		t.Fatal("MustExist opened another backing")
	}
	_ = b.Close()

	opts.ExpectBackingID = randomID(16)
	b = v.open(opts)
	waitFor(t, "the refusal", func() bool { return b.Status().Err != nil })
	if st := b.Status(); st.State != StateFailed || !errors.Is(st.Err, ErrMarkerMismatch) || errors.Is(st.Err, ErrMarkerMissing) {
		t.Fatalf("status = %+v, want ErrMarkerMismatch", st)
	}
	// PrepareConnection checks the expected ID before the first load.
	if _, err := b.PrepareConnection(t.Context(), Connection{Client: v.client}); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("PrepareConnection = %v", err)
	}
	_ = b.Close()
	opts.ExpectBackingID = id
	b = v.ready(opts)
	if _, err := b.PrepareConnection(t.Context(), Connection{Client: v.client}); err != nil {
		t.Fatal(err)
	}
}

// A legacy gateway marker opens like any other; ClearLegacyGateway clears
// the flag (once), and a backing of a build before blob tags gets BlobTags
// at its first open.
func TestLegacyMarker(t *testing.T) {
	v := newBenv(t)
	ctx := t.Context()
	const legacyID = "0123456789abcdef0123456789abcdef"
	legacy := `{"format":1,"kind":"s3","backingId":"` + legacyID + `","gateway":true,"created":"2026-01-02T03:04:05Z"}`
	if _, err := v.client.PutObject(ctx, storeBucket, "gw/"+markerKey, strings.NewReader(legacy), int64(len(legacy)), s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	opts := v.options("gw/")
	opts.Create = MustExist
	b := v.ready(opts)
	if mk := b.Marker(); !mk.Gateway || !mk.BlobTags || mk.BackingID != legacyID {
		t.Fatalf("marker = %+v", mk)
	}
	stored := func() (Marker, string) {
		t.Helper()
		m, _, err := ReadMarker(ctx, v.client, storeBucket, "gw/")
		if err != nil {
			t.Fatal(err)
		}
		r, _, err := v.client.GetObject(ctx, storeBucket, "gw/"+markerKey, s3client.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		raw, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return m, string(raw)
	}
	if m, _ := stored(); !m.Gateway || !m.BlobTags {
		t.Fatalf("stored marker = %+v, want gateway and BlobTags", m)
	}
	// The backing serves objects (and tags their blobs).
	if err := b.CreateBucket(ctx, "bk", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	putObject(t, b, "bk", "k", []byte("v"))
	if blobs := v.list("gw/data/"); len(blobs) != 1 || v.tagOf(blobs[0]) != legacyID {
		t.Fatalf("data blobs = %v", blobs)
	}

	var markerPuts atomic.Int32
	f := func(r *http.Request) int {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/gw/"+markerKey) {
			markerPuts.Add(1)
		}
		return 0
	}
	v.remote.Fail.Store(&f)
	defer v.remote.Fail.Store(nil)
	if err := b.ClearLegacyGateway(ctx); err != nil {
		t.Fatal(err)
	}
	m, raw := stored()
	if m.Gateway || !m.BlobTags || m.BackingID != legacyID || strings.Contains(raw, "gateway") {
		t.Fatalf("stored marker after clearing = %s", raw)
	}
	if b.Marker().Gateway {
		t.Fatal("Marker() still reports the gateway flag")
	}
	if err := b.ClearLegacyGateway(ctx); err != nil {
		t.Fatal(err)
	}
	if n := markerPuts.Load(); n != 1 {
		t.Fatalf("marker writes = %d, want 1 (the second clear is a no-op)", n)
	}
	// Still serving; and a reopen finds the cleared marker.
	putObject(t, b, "bk", "k2", []byte("v2"))
	_ = b.Close()
	b = v.ready(opts)
	if mk := b.Marker(); mk.Gateway || !mk.BlobTags {
		t.Fatalf("marker after reopen = %+v", mk)
	}
	if got := readObject(t, b, "bk", "k2", ""); string(got) != "v2" {
		t.Fatalf("k2 = %q", got)
	}
}
