package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/storagetest"
)

const (
	testBacking = "remote"
	testPrefix  = "pfx/"
)

// blobBacked is a Backend whose buckets are all created on the blob
// backing testBacking. It hides the backing in bucket infos so the
// conformance suite's bucket comparisons hold; the optional interfaces
// (DeleteObjectIf, WalkMultipartUploads) are promoted from *Backend.
type blobBacked struct{ *Backend }

var (
	_ storage.ConditionalDeleter = blobBacked{}
	_ storage.UploadWalker       = blobBacked{}
)

func (b blobBacked) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	opts.Backing = testBacking
	return b.Backend.CreateBucket(ctx, name, opts)
}

func (b blobBacked) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	info, err := b.Backend.GetBucket(ctx, name)
	if err == nil && info.Backing != testBacking {
		return info, fmt.Errorf("bucket %q has backing %q", name, info.Backing)
	}
	info.Backing = ""
	return info, err
}

func (b blobBacked) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	list, err := b.Backend.ListBuckets(ctx)
	for i := range list {
		list[i].Backing = ""
	}
	return list, err
}

// remoteBucketSeq names remote buckets uniquely within the test binary.
var remoteBucketSeq atomic.Int64

// testRemote is a remote S3 service with one bucket for a backend.
type testRemote struct {
	r      *s3clienttest.Remote
	c      *s3client.Client
	bucket string
}

func newTestRemote(t testing.TB, r *s3clienttest.Remote) *testRemote {
	t.Helper()
	if r == nil {
		r = s3clienttest.Start(t)
	}
	name := fmt.Sprintf("remote-%d", remoteBucketSeq.Add(1))
	r.CreateBucket(t, name)
	return &testRemote{r: r, c: r.Client(t), bucket: name}
}

func (tr *testRemote) store() RemoteStore {
	return RemoteStore{Client: tr.c, Bucket: tr.bucket, Prefix: testPrefix}
}

func (tr *testRemote) opts() Options {
	return Options{Remotes: map[string]RemoteStore{testBacking: tr.store()}}
}

// keys lists the remote keys below prefix (relative to the bucket).
func (tr *testRemote) keys(t testing.TB, prefix string) []string {
	t.Helper()
	var out []string
	err := tr.c.Walk(context.Background(), tr.bucket, prefix, "", func(o s3client.ObjectAttrs) error {
		out = append(out, o.Key)
		return nil
	})
	if err != nil {
		t.Fatalf("list remote: %v", err)
	}
	return out
}

func (tr *testRemote) put(t testing.TB, key, data string) {
	t.Helper()
	if _, err := tr.c.PutObject(context.Background(), tr.bucket, key, strings.NewReader(data), int64(len(data)), s3client.PutOptions{}); err != nil {
		t.Fatalf("remote put %s: %v", key, err)
	}
}

func (tr *testRemote) get(t testing.TB, key string) (string, bool) {
	t.Helper()
	body, _, err := tr.c.GetObject(context.Background(), tr.bucket, key, s3client.GetOptions{})
	if s3client.IsNotFound(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("remote get %s: %v", key, err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

// failWith makes the remote answer every request with status (0 = heal).
func (tr *testRemote) failWith(status int) {
	if status == 0 {
		tr.r.Fail.Store(nil)
		return
	}
	f := func(*http.Request) int { return status }
	tr.r.Fail.Store(&f)
}

// openRemoteTest opens a backend in dir (new temp dir when "") whose
// remote is tr, waits for its recovery pass and closes it at the end.
func openRemoteTest(t testing.TB, dir string, tr *testRemote, opts Options) *Backend {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	if opts.Remotes == nil {
		opts.Remotes = tr.opts().Remotes
	}
	b, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b.db.NoSync = true
	t.Cleanup(func() { _ = b.Close() })
	waitRecovery(t, b)
	return b
}

func mkRemoteBucket(t testing.TB, b *Backend, name string, versioning storage.VersioningStatus) string {
	t.Helper()
	if err := b.CreateBucket(ctx, name, storage.CreateBucketOptions{Backing: testBacking}); err != nil {
		t.Fatal(err)
	}
	if versioning != storage.VersioningUnversioned {
		if err := b.SetBucketVersioning(ctx, name, versioning); err != nil {
			t.Fatal(err)
		}
	}
	info, err := b.GetBucket(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "Backing", info.Backing, testBacking)
	return bucketIDOf(t, b, name)
}

func bucketIDOf(t testing.TB, b *Backend, name string) string {
	t.Helper()
	var id string
	if err := b.view(func(tx *bbolt.Tx) error {
		rec, _, err := loadBucket(tx, name)
		if err != nil {
			return err
		}
		id = rec.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestConformanceBlob runs the conformance suite against buckets stored on
// a blob backing. One remote service serves every subtest, each with its
// own remote bucket.
func TestConformanceBlob(t *testing.T) {
	r := s3clienttest.Start(t)
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		tr := newTestRemote(t, r)
		b, err := Open(t.TempDir(), tr.opts())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		b.db.NoSync = true
		return blobBacked{b}
	})
}

func TestRemoteDataLandsRemotely(t *testing.T) {
	tr := newTestRemote(t, nil)
	b := openRemoteTest(t, "", tr, Options{})
	id := mkRemoteBucket(t, b, "bkt", storage.VersioningUnversioned)

	put(t, b, "bkt", "a", "hello remote")
	put(t, b, "bkt", "empty", "")
	eq(t, "read a", read(t, b, "bkt", "a", ""), "hello remote")
	eq(t, "read empty", read(t, b, "bkt", "empty", ""), "")

	keys := tr.keys(t, "")
	if len(keys) != 1 || !strings.HasPrefix(keys[0], testPrefix+"data/"+id+"/") {
		t.Fatalf("remote keys = %q, want one data blob of bucket %s", keys, id)
	}
	dataID := strings.TrimPrefix(keys[0], testPrefix+"data/"+id+"/")
	if !validDataID(dataID) {
		t.Fatalf("data blob name %q is not a data ID", dataID)
	}
	if got, _ := tr.get(t, keys[0]); got != "hello remote" {
		t.Fatalf("blob content = %q", got)
	}
	if f := dataFiles(t, b); len(f) != 0 {
		t.Fatalf("local data files: %q", f)
	}

	// Overwriting replaces the blob.
	put(t, b, "bkt", "a", "second")
	keys = tr.keys(t, testPrefix+"data/")
	if len(keys) != 1 || keys[0] == testPrefix+"data/"+id+"/"+dataID {
		t.Fatalf("after overwrite remote keys = %q", keys)
	}

	// Parts land under uploads/, a replaced part's blob is deleted, and
	// completing removes all part blobs of the upload.
	up, err := b.CreateMultipartUpload(ctx, "bkt", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	writePart(t, b, "bkt", "mp", up.UploadID, 1, "one")
	writePart(t, b, "bkt", "mp", up.UploadID, 1, "ONE")
	writePart(t, b, "bkt", "mp", up.UploadID, 2, "")
	writePart(t, b, "bkt", "mp", up.UploadID, 3, "unused")
	parts := tr.keys(t, testPrefix+"uploads/"+up.UploadID+"/")
	if len(parts) != 3 {
		t.Fatalf("part blobs = %q, want 3", parts)
	}
	if f := listFiles(t, b.uploadDir); len(f) != 0 {
		t.Fatalf("local part files: %q", f)
	}
	if _, err := b.CompleteMultipartUpload(ctx, "bkt", "mp", up.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "x-2"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	eq(t, "read mp", read(t, b, "bkt", "mp", ""), "ONE")
	if p := tr.keys(t, testPrefix+"uploads/"); len(p) != 0 {
		t.Fatalf("part blobs after complete: %q", p)
	}

	// Aborting removes the part blobs.
	up2, err := b.CreateMultipartUpload(ctx, "bkt", "mp2", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	writePart(t, b, "bkt", "mp2", up2.UploadID, 1, "x")
	if err := b.AbortMultipartUpload(ctx, "bkt", "mp2", up2.UploadID); err != nil {
		t.Fatal(err)
	}
	if p := tr.keys(t, testPrefix+"uploads/"); len(p) != 0 {
		t.Fatalf("part blobs after abort: %q", p)
	}

	// Deleting the objects removes their blobs; deleting the bucket removes
	// the part blobs of its pending uploads.
	for _, k := range []string{"a", "empty", "mp"} {
		if _, err := b.DeleteObject(ctx, "bkt", k, ""); err != nil {
			t.Fatal(err)
		}
	}
	up3, err := b.CreateMultipartUpload(ctx, "bkt", "mp3", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	writePart(t, b, "bkt", "mp3", up3.UploadID, 1, "x")
	if err := b.DeleteBucket(ctx, "bkt"); err != nil {
		t.Fatal(err)
	}
	if k := tr.keys(t, ""); len(k) != 0 {
		t.Fatalf("remote keys after deleting everything: %q", k)
	}
	if f := listFiles(t, b.sysDir); slices.ContainsFunc(f, func(p string) bool {
		return strings.HasPrefix(p, "data") || strings.HasPrefix(p, "uploads") || strings.HasPrefix(p, "tmp")
	}) {
		t.Fatalf("local files: %q", f)
	}
}

func writePart(t testing.TB, b storage.Backend, bucket, key, uploadID string, n int, data string) storage.PartInfo {
	t.Helper()
	w, err := b.NewPartWriter(ctx, bucket, key, uploadID, n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	pi, err := w.Commit(ctx, storage.PartMeta{ETag: md5hex(data)})
	if err != nil {
		t.Fatalf("commit part %d: %v", n, err)
	}
	return pi
}

func TestRemoteReaderKeepsVersion(t *testing.T) {
	tr := newTestRemote(t, nil)
	b := openRemoteTest(t, "", tr, Options{})
	id := mkRemoteBucket(t, b, "bkt", storage.VersioningUnversioned)
	dataPrefix := testPrefix + "data/" + id + "/"

	put(t, b, "bkt", "k", "first version")
	_, r1, err := b.OpenObject(ctx, "bkt", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	old := tr.keys(t, dataPrefix)

	// Overwrite while r1 is open (and has not read anything yet).
	put(t, b, "bkt", "k", "second version")
	if k := tr.keys(t, dataPrefix); len(k) != 2 {
		t.Fatalf("blobs while the old version is open = %q, want old and new", k)
	}
	got, err := io.ReadAll(r1)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "old reader", string(got), "first version")

	// Delete while r2 is open, after a partial read and a seek.
	_, r2, err := b.OpenObject(ctx, "bkt", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(r2, buf); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DeleteObject(ctx, "bkt", "k", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(r2)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "deleted reader", string(buf)+"|"+string(rest), "second|version")

	// Closing the readers deletes the blobs; Close is idempotent.
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	if k := tr.keys(t, dataPrefix); slices.Contains(k, old[0]) {
		t.Fatalf("old blob %s still present after its reader closed: %q", old[0], k)
	}
	if k := tr.keys(t, dataPrefix); len(k) != 1 {
		t.Fatalf("blobs with the deleted version open = %q, want 1", k)
	}
	_ = r2.Close()
	_ = r2.Close()
	_ = r1.Close()
	if k := tr.keys(t, dataPrefix); len(k) != 0 {
		t.Fatalf("blobs after all readers closed = %q", k)
	}

	// Two readers of the same blob: only the last close deletes it.
	put(t, b, "bkt", "k", "third")
	_, ra, _ := b.OpenObject(ctx, "bkt", "k", "")
	_, rb, _ := b.OpenObject(ctx, "bkt", "k", "")
	if _, err := b.DeleteObject(ctx, "bkt", "k", ""); err != nil {
		t.Fatal(err)
	}
	_ = ra.Close()
	if k := tr.keys(t, dataPrefix); len(k) != 1 {
		t.Fatalf("blob deleted while a second reader is open: %q", k)
	}
	if got, _ := io.ReadAll(rb); string(got) != "third" {
		t.Fatalf("second reader = %q", got)
	}
	_ = rb.Close()
	if k := tr.keys(t, dataPrefix); len(k) != 0 {
		t.Fatalf("blobs after the last reader closed = %q", k)
	}
}

// TestRemoteConcurrentOverwriteReads overwrites a key while readers open
// and read it; every reader must see one complete version.
func TestRemoteConcurrentOverwriteReads(t *testing.T) {
	tr := newTestRemote(t, nil)
	b := openRemoteTest(t, "", tr, Options{})
	mkRemoteBucket(t, b, "bkt", storage.VersioningUnversioned)
	payload := func(i int) string { return strings.Repeat(fmt.Sprintf("<%04d>", i), 200) }
	put(t, b, "bkt", "k", payload(0))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 30; i++ {
			put(t, b, "bkt", "k", payload(i))
		}
		close(stop)
	}()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, r, err := b.OpenObject(ctx, "bkt", "k", "")
				if err != nil {
					t.Errorf("open: %v", err)
					return
				}
				time.Sleep(time.Millisecond)
				data, err := io.ReadAll(r)
				_ = r.Close()
				if err != nil {
					t.Errorf("read: %v", err)
					return
				}
				if len(data) != len(payload(0)) || string(data) != strings.Repeat(string(data[:6]), 200) {
					t.Errorf("torn read: %.40q...", data)
					return
				}
			}
		}()
	}
	wg.Wait()
	if k := tr.keys(t, testPrefix+"data/"); len(k) != 1 {
		t.Fatalf("blobs after the readers closed = %d, want 1", len(k))
	}
}

// copyCounter records the server-side copy requests the remote receives.
type copyCounter struct {
	mu                 sync.Mutex
	partCopies, copies int
}

func (cc *copyCounter) install(tr *testRemote) {
	f := func(r *http.Request) int {
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			cc.mu.Lock()
			if r.URL.Query().Get("uploadId") != "" {
				cc.partCopies++
			} else {
				cc.copies++
			}
			cc.mu.Unlock()
		}
		return 0
	}
	tr.r.Fail.Store(&f)
}

func (cc *copyCounter) get() (int, int) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.partCopies, cc.copies
}

func TestRemoteCompleteServerSide(t *testing.T) {
	tr := newTestRemote(t, nil)
	b := openRemoteTest(t, "", tr, Options{})
	mkRemoteBucket(t, b, "bkt", storage.VersioningUnversioned)
	var cc copyCounter
	cc.install(tr)

	big := func(c byte) string { return strings.Repeat(string(c), s3client.MinPartSize) }
	want := big('a') + big('b') + "tail"
	up, err := b.CreateMultipartUpload(ctx, "bkt", "big", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	writePart(t, b, "bkt", "big", up.UploadID, 1, big('a'))
	writePart(t, b, "bkt", "big", up.UploadID, 2, big('b'))
	writePart(t, b, "bkt", "big", up.UploadID, 3, "tail")
	info, err := b.CompleteMultipartUpload(ctx, "bkt", "big", up.UploadID, []int{1, 2, 3}, storage.ObjectMeta{ETag: "x-3"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "size", info.Size, int64(len(want)))
	if got := read(t, b, "bkt", "big", ""); got != want {
		t.Fatalf("composed content differs (len %d, want %d)", len(got), len(want))
	}
	if pc, _ := cc.get(); pc != 3 {
		t.Fatalf("UploadPartCopy requests = %d, want 3", pc)
	}

	// A single part is copied.
	up, err = b.CreateMultipartUpload(ctx, "bkt", "one", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	writePart(t, b, "bkt", "one", up.UploadID, 7, "single part")
	if _, err := b.CompleteMultipartUpload(ctx, "bkt", "one", up.UploadID, []int{7}, storage.ObjectMeta{ETag: "x-1"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	eq(t, "single", read(t, b, "bkt", "one", ""), "single part")
	if _, c := cc.get(); c != 1 {
		t.Fatalf("CopyObject requests = %d, want 1", c)
	}

	// Small parts are streamed (no further server-side copies).
	up, err = b.CreateMultipartUpload(ctx, "bkt", "small", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	writePart(t, b, "bkt", "small", up.UploadID, 1, "small-1;")
	writePart(t, b, "bkt", "small", up.UploadID, 2, "")
	writePart(t, b, "bkt", "small", up.UploadID, 3, "small-3")
	if _, err := b.CompleteMultipartUpload(ctx, "bkt", "small", up.UploadID, []int{1, 2, 3}, storage.ObjectMeta{ETag: "x-3"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	eq(t, "small", read(t, b, "bkt", "small", ""), "small-1;small-3")
	if pc, c := cc.get(); pc != 3 || c != 1 {
		t.Fatalf("copies after the streamed completion = %d/%d, want 3/1", pc, c)
	}
	if k := tr.keys(t, testPrefix+"uploads/"); len(k) != 0 {
		t.Fatalf("part blobs left: %q", k)
	}
	if k := tr.keys(t, testPrefix+"data/"); len(k) != 3 {
		t.Fatalf("data blobs = %q, want 3", k)
	}
}

func TestComposeRanges(t *testing.T) {
	const mib = 1 << 20
	keys := []string{"a", "b", "c"}
	nums := []int{1, 2, 3}
	if _, ok := composeRanges(keys, []int64{4 * mib, 5 * mib, 1}, nums); ok {
		t.Fatal("composed with a small first part")
	}
	r, ok := composeRanges(keys, []int64{5 * mib, 5 * mib, 0}, nums)
	if !ok || len(r) != 2 {
		t.Fatalf("ranges = %+v, %v; want 2 (empty last part skipped)", r, ok)
	}
	huge := int64(s3client.MaxCopySize) + 10*mib
	r, ok = composeRanges(keys[:2], []int64{huge, 1}, nums)
	if !ok || len(r) != 3 {
		t.Fatalf("ranges = %+v, %v; want the big part split in two", r, ok)
	}
	var total int64
	for _, cr := range r[:2] {
		if cr.length < s3client.MinPartSize || cr.length > s3client.MaxCopySize {
			t.Fatalf("range %+v out of limits", cr)
		}
		total += cr.length
	}
	eq(t, "split total", total, huge)
	eq(t, "second range offset", r[1].off, r[0].length)
}

func TestRemoteUnconfiguredBacking(t *testing.T) {
	tr := newTestRemote(t, nil)
	dir := t.TempDir()
	b := openRemoteTest(t, dir, tr, Options{})

	// A backing fs does not serve (e.g. a gateway backing).
	if err := b.CreateBucket(ctx, "gw", storage.CreateBucketOptions{Backing: "gateway"}); err != nil {
		t.Fatal(err)
	}
	if info, err := b.GetBucket(ctx, "gw"); err != nil || info.Backing != "gateway" {
		t.Fatalf("GetBucket = %+v, %v", info, err)
	}
	if err := b.PutBucketConfig(ctx, "gw", "cors", []byte("x")); err != nil {
		t.Fatal(err)
	}
	_, err := b.NewObjectWriter(ctx, "gw", "k")
	wantUnavailable(t, "NewObjectWriter", err, "gateway")
	up, err := b.CreateMultipartUpload(ctx, "gw", "k", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.NewPartWriter(ctx, "gw", "k", up.UploadID, 1)
	wantUnavailable(t, "NewPartWriter", err, "gateway")
	_, err = b.CompleteMultipartUpload(ctx, "gw", "k", up.UploadID, nil, storage.ObjectMeta{}, storage.CommitOptions{})
	wantUnavailable(t, "CompleteMultipartUpload", err, "gateway")
	if err := b.AbortMultipartUpload(ctx, "gw", "k", up.UploadID); err != nil {
		t.Fatal(err)
	}

	// A blob backing removed from the configuration.
	mkRemoteBucket(t, b, "bkt", storage.VersioningUnversioned)
	put(t, b, "bkt", "k", "data")
	put(t, b, "bkt", "empty", "")
	_ = b.Close()
	b2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b2.Close() })
	waitRecovery(t, b2)
	if info, err := b2.GetObjectInfo(ctx, "bkt", "k", ""); err != nil || info.Size != 4 {
		t.Fatalf("GetObjectInfo = %+v, %v", info, err)
	}
	_, _, err = b2.OpenObject(ctx, "bkt", "k", "")
	wantUnavailable(t, "OpenObject", err, testBacking)
	eq(t, "empty object", read(t, b2, "bkt", "empty", ""), "")
	_, err = b2.NewObjectWriter(ctx, "bkt", "k2")
	wantUnavailable(t, "NewObjectWriter", err, testBacking)
	if _, err := b2.DeleteObject(ctx, "bkt", "k", ""); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if k := tr.keys(t, testPrefix+"data/"); len(k) != 1 {
		t.Fatalf("remote blobs = %q, want the one fs could not delete", k)
	}
}

func wantUnavailable(t testing.TB, what string, err error, backing string) {
	t.Helper()
	if !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("%s: err = %v, want ErrBackingUnavailable", what, err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", backing)) {
		t.Fatalf("%s: error %q does not name backing %q", what, err, backing)
	}
}

func TestRemoteUnavailable(t *testing.T) {
	tr := newTestRemote(t, nil)
	b := openRemoteTest(t, "", tr, Options{})
	mkRemoteBucket(t, b, "bkt", storage.VersioningUnversioned)
	put(t, b, "bkt", "k", "data")
	up, err := b.CreateMultipartUpload(ctx, "bkt", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	writePart(t, b, "bkt", "mp", up.UploadID, 1, "p1")
	writePart(t, b, "bkt", "mp", up.UploadID, 2, "p2")

	_, r, err := b.OpenObject(ctx, "bkt", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	tr.failWith(http.StatusServiceUnavailable)
	_, err = putOpts(t, b, "bkt", "k2", "new data", storage.CommitOptions{})
	wantUnavailable(t, "Commit", err, testBacking)
	w, err := b.NewPartWriter(ctx, "bkt", "mp", up.UploadID, 3)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "p3")
	_, err = w.Commit(ctx, storage.PartMeta{ETag: md5hex("p3")})
	wantUnavailable(t, "part Commit", err, testBacking)
	_, err = b.CompleteMultipartUpload(ctx, "bkt", "mp", up.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "x-2"}, storage.CommitOptions{})
	wantUnavailable(t, "CompleteMultipartUpload", err, testBacking)
	_, err = io.ReadAll(r)
	wantUnavailable(t, "Read", err, testBacking)

	tr.failWith(0)
	// Nothing changed: the failed writes are invisible, the upload intact.
	if _, err := b.GetObjectInfo(ctx, "bkt", "k2", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("k2 after failed put: %v", err)
	}
	if _, err := b.CompleteMultipartUpload(ctx, "bkt", "mp", up.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "x-2"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	eq(t, "mp", read(t, b, "bkt", "mp", ""), "p1p2")
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "reader after recovery", string(got), "data")
}

func TestRemoteMissingBlobIsCorrupt(t *testing.T) {
	tr := newTestRemote(t, nil)
	b := openRemoteTest(t, "", tr, Options{})
	mkRemoteBucket(t, b, "bkt", storage.VersioningUnversioned)
	put(t, b, "bkt", "k", "data")
	for _, k := range tr.keys(t, "") {
		if err := tr.c.DeleteObject(ctx, tr.bucket, k); err != nil {
			t.Fatal(err)
		}
	}
	_, r, err := b.OpenObject(ctx, "bkt", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.ReadAll(r); !errors.Is(err, errCorrupt) {
		t.Fatalf("read of a missing blob: %v, want errCorrupt", err)
	}
}

func TestRemoteRecovery(t *testing.T) {
	tr := newTestRemote(t, nil)
	var logBuf syncBuffer
	b := openRemoteTest(t, "", tr, Options{OrphanRetention: time.Hour, Logger: slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	id := mkRemoteBucket(t, b, "bkt", storage.VersioningUnversioned)
	put(t, b, "bkt", "k", "referenced")
	up, err := b.CreateMultipartUpload(ctx, "bkt", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	writePart(t, b, "bkt", "mp", up.UploadID, 1, "live part")
	referenced := tr.keys(t, "")

	// A held, superseded blob: deleted when its reader closes, never by
	// the recovery pass.
	put(t, b, "bkt", "held", "old")
	_, hr, err := b.OpenObject(ctx, "bkt", "held", "")
	if err != nil {
		t.Fatal(err)
	}
	var heldKey string
	for _, k := range tr.keys(t, testPrefix+"data/") {
		if !slices.Contains(referenced, k) {
			heldKey = k
		}
	}
	if heldKey == "" {
		t.Fatal("held blob not found")
	}
	put(t, b, "bkt", "held", "new")
	referenced = tr.keys(t, "") // k, the live part, held's old (open) and new blobs
	eq(t, "referenced blobs", len(referenced), 4)

	orphans := []string{
		testPrefix + "data/" + id + "/" + randomHex(16),           // crash before commit
		testPrefix + "data/" + randomHex(8) + "/" + randomHex(16), // deleted bucket
		testPrefix + "uploads/" + newUploadID() + "/1-" + randomHex(8),
		testPrefix + "uploads/" + up.UploadID + "/1-" + randomHex(8), // replaced part
	}
	unrecognized := []string{
		testPrefix + "data/README",
		testPrefix + "data/" + id + "/NOT-HEX",
		testPrefix + "uploads/short/1-" + randomHex(8),
		testPrefix + "uploads/" + newUploadID() + "/0-" + randomHex(8),
		"elsewhere/data/" + id + "/" + randomHex(16),
	}
	for _, k := range append(slices.Clone(orphans), unrecognized...) {
		tr.put(t, k, "x")
	}
	all := tr.keys(t, "")

	// Young orphans are kept.
	b.recover(context.Background())
	if got := tr.keys(t, ""); !slices.Equal(got, all) {
		t.Fatalf("recovery deleted young blobs:\n got %q\nwant %q", got, all)
	}
	if !strings.Contains(logBuf.String(), "kept_young_blobs=4") {
		t.Fatalf("log does not report the young blobs:\n%s", logBuf.String())
	}

	// Old ones are deleted, unless referenced, held or unrecognized.
	b.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	b.recover(context.Background())
	got := tr.keys(t, "")
	want := slices.Concat(referenced, unrecognized)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("after recovery:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(logBuf.String(), "orphan_data_blobs=2") || !strings.Contains(logBuf.String(), "orphan_part_blobs=2") ||
		!strings.Contains(logBuf.String(), "unrecognized_keys=4") {
		t.Fatalf("log does not report the deletions:\n%s", logBuf.String())
	}

	_ = hr.Close()
	if slices.Contains(tr.keys(t, ""), heldKey) {
		t.Fatal("held blob not deleted on close")
	}
	eq(t, "k", read(t, b, "bkt", "k", ""), "referenced")
	eq(t, "held", read(t, b, "bkt", "held", ""), "new")
}

func TestRemoteRefuseEmptyMetadata(t *testing.T) {
	tr := newTestRemote(t, nil)
	dir := t.TempDir()
	b, err := Open(dir, tr.opts())
	if err != nil {
		t.Fatal(err)
	}
	waitRecovery(t, b)
	mkRemoteBucket(t, b, "bkt", storage.VersioningUnversioned)
	put(t, b, "bkt", "k", "data")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, ".gos3", "meta.db")
	for _, state := range []string{"missing", "empty"} {
		if state == "missing" {
			if err := os.Remove(dbPath); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Open(dir, tr.opts())
		if err == nil || !strings.Contains(err.Error(), state) || !strings.Contains(err.Error(), `"remote"`) {
			t.Fatalf("Open with a %s meta.db: %v, want a refusal naming the remote", state, err)
		}
	}
	// Without the remote configured (or with an unreachable one) the
	// empty store starts.
	unreachable, err := s3client.New(s3client.Config{Endpoint: "http://127.0.0.1:1", AccessKey: "a", SecretKey: "b", PathStyle: true, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Open(dir, Options{Remotes: map[string]RemoteStore{testBacking: {Client: unreachable, Bucket: tr.bucket, Prefix: testPrefix}}})
	if err != nil {
		t.Fatalf("Open with an unreachable remote: %v", err)
	}
	waitRecovery(t, b2)
	_ = b2.Close()

	// Uploads alone also count.
	tr2 := newTestRemote(t, tr.r)
	tr2.put(t, testPrefix+"uploads/"+newUploadID()+"/1-"+randomHex(8), "part")
	if _, err := Open(t.TempDir(), tr2.opts()); err == nil {
		t.Fatal("Open succeeded with part blobs on the remote and no meta.db")
	}
}

func TestRemoteBackingPersisted(t *testing.T) {
	tr := newTestRemote(t, nil)
	dir := t.TempDir()
	b, err := Open(dir, tr.opts())
	if err != nil {
		t.Fatal(err)
	}
	waitRecovery(t, b)
	mkRemoteBucket(t, b, "bkt", storage.VersioningEnabled)
	if err := b.CreateBucket(ctx, "local", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, b, "bkt", "k", "remote data")
	put(t, b, "local", "k", "local data")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = openRemoteTest(t, dir, tr, Options{})
	list, err := b.ListBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "bkt" || list[0].Backing != testBacking || list[1].Backing != "" {
		t.Fatalf("ListBuckets = %+v", list)
	}
	eq(t, "remote object", read(t, b, "bkt", "k", ""), "remote data")
	eq(t, "local object", read(t, b, "local", "k", ""), "local data")
	if n := len(dataFiles(t, b)); n != 1 {
		t.Fatalf("local data files = %d, want 1", n)
	}
	if n := len(tr.keys(t, "")); n != 1 {
		t.Fatalf("remote blobs = %d, want 1", n)
	}
}

func TestOpenRejectsBadRemotes(t *testing.T) {
	tr := newTestRemote(t, nil)
	for name, rs := range map[string]RemoteStore{
		"":       tr.store(),
		"nocli":  {Bucket: "x"},
		"nobkt":  {Client: tr.c},
		"negpsz": {Client: tr.c, Bucket: "x", PartSize: -1},
	} {
		if _, err := Open(t.TempDir(), Options{Remotes: map[string]RemoteStore{name: rs}}); err == nil {
			t.Errorf("Open accepted remote %q = %+v", name, rs)
		}
	}
}
