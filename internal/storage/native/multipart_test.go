package native_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
)

func uploadPart(t *testing.T, h *harness, bucket, key, id string, n int, data []byte) storage.PartInfo {
	t.Helper()
	w, err := h.NewPartWriter(t.Context(), bucket, key, id, n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	p, err := w.Commit(t.Context(), storage.PartMeta{ETag: md5hex(data), Checksums: map[string]string{"CRC32": fmt.Sprint(n)}})
	if err != nil {
		t.Fatalf("part %d: %v", n, err)
	}
	return p
}

// stagedFiles returns the staged parts in the store's StateStore.
func stagedFiles(t *testing.T, h *harness) []string {
	t.Helper()
	state := h.opts.State
	if fs, ok := state.(*faultyState); ok {
		state = fs.StateStore // bypass injected failures
	}
	names, err := state.ListStaged(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestMultipartRemoteParts(t *testing.T) {
	log := &reqLog{}
	h := newHarness(t, s3clienttest.Options{Wrap: log.wrap}, nil)
	h.mkVersioned("bkt")
	base := storage.ObjectMeta{ContentType: "application/x-big", Tags: map[string]string{"a": "b"}, ACL: "public-read"}
	u, err := h.CreateMultipartUpload(t.Context(), "bkt", "big", base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{randData(5<<20, 1), randData(5<<20+3, 2), randData(1000, 3)}
	for i, p := range parts {
		uploadPart(t, h, "bkt", "big", u.UploadID, i+1, p)
	}
	if staged := stagedFiles(t, h); len(staged) != 1 {
		t.Errorf("staged files %q, want only the small last part", staged)
	}
	ups, err := h.remote.Backend.ListMultipartUploads(t.Context(), "nb-bkt")
	if err != nil || len(ups) != 1 || ups[0].Key != "big" {
		t.Fatalf("remote uploads %+v, %v; want one of big", ups, err)
	}
	meta := base
	meta.ETag = "0123456789abcdef0123456789abcdef-3"
	meta.PartSizes = []int64{int64(len(parts[0])), int64(len(parts[1])), int64(len(parts[2]))}
	meta.Checksums = map[string]string{"CRC32": "AAAAAA=="}
	meta.ChecksumType = "COMPOSITE"
	meta.Tags = map[string]string{"a": "b", "final": "yes"} // changed by the completion
	log.reset()
	info, err := h.CompleteMultipartUpload(t.Context(), "bkt", "big", u.UploadID, []int{1, 2, 3}, meta, storage.CommitOptions{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Concat(parts...)
	got, data := h.read("bkt", "big", "")
	if !bytes.Equal(data, want) {
		t.Error("content differs")
	}
	metaEqual(t, "completed", meta, got.ObjectMeta)
	if !got.LastModified.Equal(info.LastModified) || got.VersionID != info.VersionID || info.Size != int64(len(want)) {
		t.Errorf("GET %v/%s, Complete said %v/%s", got.LastModified, got.VersionID, info.LastModified, info.VersionID)
	}
	// The metadata known only at completion was stored without copying
	// the object: tags natively, the rest in an overlay.
	for _, r := range log.all() {
		if r.header.Get("X-Amz-Copy-Source") != "" {
			t.Errorf("completion copied the object: %s %s", r.method, r.path)
		}
	}
	if vs := h.remoteVersions("nb-bkt"); len(vs) != 1 {
		t.Errorf("remote versions %v, want one", vrefs(vs))
	}
	a := h.remoteHead("bkt", "big", "")
	if a.Size != int64(len(want)) || a.Header.Get("Content-Type") != "application/x-big" || a.Header.Get("X-Amz-Tagging-Count") != "2" {
		t.Errorf("remote object %+v %v, want native headers and the final tags", a, a.Header)
	}
	if ups, _ := h.remote.Backend.ListMultipartUploads(t.Context(), "nb-bkt"); len(ups) != 0 {
		t.Errorf("remote uploads left: %+v", ups)
	}
	if staged := stagedFiles(t, h); len(staged) != 0 {
		t.Errorf("staged files left: %q", staged)
	}
	if _, err := h.GetMultipartUpload(t.Context(), "bkt", "big", u.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("upload after completion: %v", err)
	}
	// Content-MD5 was sent with every part.
	for _, r := range log.all() {
		if r.method == http.MethodPut && r.query.Has("partNumber") && r.header.Get("Content-Md5") == "" {
			t.Errorf("part upload without Content-MD5: %s %v", r.path, r.query)
		}
	}
}

func TestMultipartSmallParts(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("bkt", storage.CreateBucketOptions{})
	u, err := h.CreateMultipartUpload(t.Context(), "bkt", "small", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{[]byte("one,"), []byte("two,"), []byte("three")}
	for i, p := range parts {
		uploadPart(t, h, "bkt", "small", u.UploadID, i+1, p)
	}
	if ups, _ := h.remote.Backend.ListMultipartUploads(t.Context(), "nb-bkt"); len(ups) != 0 {
		t.Errorf("small parts created a remote upload: %+v", ups)
	}
	meta := storage.ObjectMeta{ETag: "aaaa-3", PartSizes: []int64{4, 4, 5}}
	if _, err := h.CompleteMultipartUpload(t.Context(), "bkt", "small", u.UploadID, []int{1, 2, 3}, meta, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	info, data := h.read("bkt", "small", "")
	if string(data) != "one,two,three" || info.ETag != "aaaa-3" || !slices.Equal(info.PartSizes, meta.PartSizes) {
		t.Errorf("completed %+v %q", info, data)
	}
	if a := h.remoteHead("bkt", "small", ""); a.ETag != md5hex(data) {
		t.Errorf("remote ETag %q, want a single PUT", a.ETag)
	}

	// A small non-last part next to a remote part cannot be assembled.
	u, err = h.CreateMultipartUpload(t.Context(), "bkt", "mixed", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "bkt", "mixed", u.UploadID, 1, []byte("small"))
	uploadPart(t, h, "bkt", "mixed", u.UploadID, 2, randData(5<<20, 9))
	_, err = h.CompleteMultipartUpload(t.Context(), "bkt", "mixed", u.UploadID, []int{1, 2}, storage.ObjectMeta{}, storage.CommitOptions{})
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("mixed completion: %v, want ErrNotSupported", err)
	}
	// Completing only the small part works (the remote upload is aborted).
	if _, err := h.CompleteMultipartUpload(t.Context(), "bkt", "mixed", u.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if ups, _ := h.remote.Backend.ListMultipartUploads(t.Context(), "nb-bkt"); len(ups) != 0 {
		t.Errorf("remote uploads left: %+v", ups)
	}
}

func TestUploadStatePersists(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, func(o *native.Options) { o.SpoolDir = filepath.Join(t.TempDir(), "tmp") })
	h.mk("bkt", storage.CreateBucketOptions{})
	meta := storage.ObjectMeta{ContentType: "a/b", UserMeta: map[string]string{"k": "v"}}
	u, err := h.CreateMultipartUpload(t.Context(), "bkt", "obj", meta, "bob")
	if err != nil {
		t.Fatal(err)
	}
	big, small := randData(5<<20, 6), []byte("tail")
	p1 := uploadPart(t, h, "bkt", "obj", u.UploadID, 1, big)
	uploadPart(t, h, "bkt", "obj", u.UploadID, 2, small)
	other, err := h.CreateMultipartUpload(t.Context(), "bkt", "other", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	// A stale spool file is removed at Open.
	stale := filepath.Join(h.opts.SpoolDir, "gos3-spool-stale")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.open()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale spool file kept: %v", err)
	}
	ups, err := h.ListMultipartUploads(t.Context(), "bkt")
	if err != nil || len(ups) != 2 || ups[0].UploadID != u.UploadID || ups[0].Initiator != "bob" || !ups[0].Initiated.Equal(u.Initiated) ||
		ups[0].Meta.UserMeta["k"] != "v" || ups[1].UploadID != other.UploadID {
		t.Fatalf("uploads after reopen: %+v, %v", ups, err)
	}
	parts, err := h.ListParts(t.Context(), "bkt", "obj", u.UploadID)
	if err != nil || len(parts) != 2 || parts[0].ETag != p1.ETag || parts[0].Size != int64(len(big)) || parts[1].Size != 4 ||
		parts[0].Checksums["CRC32"] != "1" || !parts[0].LastModified.Equal(p1.LastModified) {
		t.Fatalf("parts after reopen: %+v, %v", parts, err)
	}
	if _, err := h.CompleteMultipartUpload(t.Context(), "bkt", "obj", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2", ContentType: "a/b"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, data := h.read("bkt", "obj", ""); !bytes.Equal(data, slices.Concat(big, small)) {
		t.Error("content after reopen differs")
	}
	if err := h.AbortMultipartUpload(t.Context(), "bkt", "other", other.UploadID); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	h.open()
	if ups, _ := h.ListMultipartUploads(t.Context(), "bkt"); len(ups) != 0 {
		t.Errorf("finished uploads came back: %+v", ups)
	}
}

// TestLegacyUploadRecords: upload state of the former gateway backing
// (records without the remote bucket) loads, also while the bucket cannot
// be resolved yet, and completes in the bucket's remote bucket.
func TestLegacyUploadRecords(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("bkt", storage.CreateBucketOptions{})
	u, err := h.CreateMultipartUpload(t.Context(), "bkt", "obj", storage.ObjectMeta{ContentType: "a/b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	big := randData(5<<20, 1)
	uploadPart(t, h, "bkt", "obj", u.UploadID, 1, big)
	uploadPart(t, h, "bkt", "obj", u.UploadID, 2, []byte("tail"))
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	// Rewrite the upload record as the gateway wrote it.
	recs, err := h.opts.State.Records(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(recs["u/"+u.UploadID], &rec); err != nil {
		t.Fatal(err)
	}
	if rec["remoteBucket"] != "nb-bkt" {
		t.Fatalf("upload record %v, want remoteBucket", rec)
	}
	delete(rec, "remoteBucket")
	delete(rec, "region")
	data, _ := json.Marshal(rec)
	if err := h.opts.State.PutRecord(t.Context(), "u/"+u.UploadID, data); err != nil {
		t.Fatal(err)
	}
	// Open does not need the bucket.
	resolve := h.opts.Resolve
	h.opts.Resolve = func(context.Context, string) (native.Bucket, error) {
		return native.Bucket{}, storage.ErrBucketNotFound
	}
	h.open()
	h.opts.Resolve = resolve
	h.open()
	if _, err := h.CompleteMultipartUpload(t.Context(), "bkt", "obj", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2", ContentType: "a/b"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, data := h.read("bkt", "obj", ""); !bytes.Equal(data, append(big, "tail"...)) {
		t.Error("content differs")
	}
}

func TestCheckEmptyAndDetach(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(fmt.Sprintf("versioned=%v", versioned), func(t *testing.T) {
			h := newHarness(t, s3clienttest.Options{}, nil)
			mk := func(name string) {
				if versioned {
					h.mkVersioned(name)
				} else {
					h.mk(name, storage.CreateBucketOptions{})
				}
			}
			mk("bkt")
			mk("nxt") // a neighbour bucket's objects must not count
			h.mustPut("nxt", "k", []byte("x"), storage.ObjectMeta{})
			if err := h.st.CheckEmpty(t.Context(), "bkt"); err != nil {
				t.Errorf("CheckEmpty(empty): %v", err)
			}
			h.mustPut("bkt", "k", []byte("x"), storage.ObjectMeta{})
			if _, err := h.st.UpdateObjectMeta(t.Context(), "bkt", "k", "", func(m *storage.ObjectMeta) error { m.ACL = "public-read"; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := h.st.CheckEmpty(t.Context(), "bkt"); !errors.Is(err, storage.ErrBucketNotEmpty) {
				t.Errorf("CheckEmpty(non-empty): %v", err)
			}
			if _, err := h.DeleteObject(t.Context(), "bkt", "k", ""); err != nil {
				t.Fatal(err)
			}
			if err := h.st.CheckEmpty(t.Context(), "bkt"); versioned != errors.Is(err, storage.ErrBucketNotEmpty) {
				t.Errorf("CheckEmpty after a delete: %v (a versioned bucket keeps versions)", err)
			}
			u, err := h.CreateMultipartUpload(t.Context(), "bkt", "up", storage.ObjectMeta{}, "")
			if err != nil {
				t.Fatal(err)
			}
			uploadPart(t, h, "bkt", "up", u.UploadID, 1, randData(5<<20, 7))
			uploadPart(t, h, "bkt", "up", u.UploadID, 2, []byte("small"))
			w, err := h.NewObjectWriter(t.Context(), "bkt", "late")
			if err != nil {
				t.Fatal(err)
			}
			if err := h.st.DetachBucket(t.Context(), "bkt", false); err != nil {
				t.Fatalf("DetachBucket: %v", err)
			}
			if _, err := w.Commit(t.Context(), storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrBucketNotFound) {
				t.Errorf("commit of a writer from before DetachBucket: %v", err)
			}
			if ups, _ := h.ListMultipartUploads(t.Context(), "bkt"); len(ups) != 0 {
				t.Errorf("uploads after DetachBucket: %+v", ups)
			}
			if ups, _ := h.remote.Backend.ListMultipartUploads(t.Context(), "nb-bkt"); len(ups) != 0 {
				t.Errorf("remote uploads after DetachBucket: %+v", ups)
			}
			if staged := stagedFiles(t, h); len(staged) != 0 {
				t.Errorf("staged parts after DetachBucket: %q", staged)
			}
			if n := native.OverlayCount(h.st, "bkt", ""); n != 0 {
				t.Errorf("%d overlays after DetachBucket", n)
			}
			// The remote bucket stays (removeRemote false) with its objects.
			if err := h.c.HeadBucket(t.Context(), "nb-bkt"); err != nil {
				t.Errorf("remote bucket after DetachBucket(false): %v", err)
			}
			if got := h.walkKeys("nxt", "", ""); !slices.Equal(got, []string{"k"}) {
				t.Errorf("neighbour bucket: %q", got)
			}
		})
	}
}

func TestOpenOptions(t *testing.T) {
	r := s3clienttest.Start(t)
	resolve := func(context.Context, string) (native.Bucket, error) {
		return native.Bucket{}, storage.ErrBucketNotFound
	}
	if _, err := native.Open(native.Options{Name: "nat", Client: r.Client(t), Resolve: resolve}); err == nil {
		t.Error("Open without State succeeded")
	}
	if _, err := native.Open(native.Options{Name: "nat", State: native.MemoryState(), Resolve: resolve}); err == nil {
		t.Error("Open without Client succeeded")
	}
	if _, err := native.Open(native.Options{Name: "nat", Client: r.Client(t), State: native.MemoryState()}); err == nil {
		t.Error("Open without Resolve succeeded")
	}
	dir := filepath.Join(t.TempDir(), "a", "b")
	st, err := native.Open(native.Options{Name: "nat", Client: r.Client(t), State: native.MemoryState(), Resolve: resolve, SpoolDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("spool directory %s: %v", dir, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// faultyState is a persistent StateStore (it outlives the stores opened
// on it) whose operations can be made to fail.
type faultyState struct {
	native.StateStore

	mu                                 sync.Mutex
	failRecords, failList              bool
	failPutRecord, failPutStaged       bool
	failDelete                         bool // DeleteRecords
	landThenFail                       bool // PutRecord writes, then fails
	recordsCalls, listCalls, openCalls int
}

var errInjected = errors.New("injected state failure")

func (s *faultyState) fail(flag *bool, calls *int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if calls != nil {
		*calls++
	}
	if *flag {
		return errInjected
	}
	return nil
}

func (s *faultyState) set(flag *bool, v bool) {
	s.mu.Lock()
	*flag = v
	s.mu.Unlock()
}

func (s *faultyState) Records(ctx context.Context) (map[string][]byte, error) {
	if err := s.fail(&s.failRecords, &s.recordsCalls); err != nil {
		return nil, err
	}
	return s.StateStore.Records(ctx)
}

func (s *faultyState) PutRecord(ctx context.Context, key string, val []byte) error {
	if err := s.fail(&s.failPutRecord, nil); err != nil {
		return err
	}
	if err := s.StateStore.PutRecord(ctx, key, val); err != nil {
		return err
	}
	return s.fail(&s.landThenFail, nil)
}

func (s *faultyState) DeleteRecords(ctx context.Context, prefix string) error {
	if err := s.fail(&s.failDelete, nil); err != nil {
		return err
	}
	return s.StateStore.DeleteRecords(ctx, prefix)
}

func (s *faultyState) PutStaged(ctx context.Context, name string, r io.ReaderAt, size int64) error {
	if err := s.fail(&s.failPutStaged, nil); err != nil {
		return err
	}
	return s.StateStore.PutStaged(ctx, name, r, size)
}

func (s *faultyState) OpenStaged(ctx context.Context, name string, size int64) (io.ReadSeekCloser, error) {
	s.mu.Lock()
	s.openCalls++
	s.mu.Unlock()
	return s.StateStore.OpenStaged(ctx, name, size)
}

func (s *faultyState) ListStaged(ctx context.Context) ([]string, error) {
	if err := s.fail(&s.failList, &s.listCalls); err != nil {
		return nil, err
	}
	return s.StateStore.ListStaged(ctx)
}

// stagedOf returns the staged parts of upload id.
func stagedOf(t *testing.T, h *harness, id string) []string {
	t.Helper()
	var out []string
	for _, name := range stagedFiles(t, h) {
		if strings.HasPrefix(name, id+"-") {
			out = append(out, name)
		}
	}
	return out
}

func newFaulty(t *testing.T) (*harness, *faultyState) {
	state := &faultyState{StateStore: native.MemoryState()}
	h := newHarness(t, s3clienttest.Options{}, func(o *native.Options) { o.State = state })
	h.mk("bkt", storage.CreateBucketOptions{})
	return h, state
}

func TestStateStorePersistence(t *testing.T) {
	h, state := newFaulty(t)
	ctx := t.Context()
	u, err := h.CreateMultipartUpload(ctx, "bkt", "obj", storage.ObjectMeta{ContentType: "a/b"}, "bob")
	if err != nil {
		t.Fatal(err)
	}
	big, small := randData(5<<20, 6), []byte("tail")
	uploadPart(t, h, "bkt", "obj", u.UploadID, 1, big)
	uploadPart(t, h, "bkt", "obj", u.UploadID, 2, []byte("replaced"))
	uploadPart(t, h, "bkt", "obj", u.UploadID, 2, small)
	if staged := stagedOf(t, h, u.UploadID); len(staged) != 1 {
		t.Errorf("staged parts %q, want one (the replaced part's blob deleted)", staged)
	}
	for _, name := range stagedFiles(t, h) {
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
				t.Errorf("staged name %q has %q, outside the charset s3store's recovery reclaims", name, c)
			}
		}
	}
	other, err := h.CreateMultipartUpload(ctx, "bkt", "other", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "bkt", "other", other.UploadID, 1, []byte("lost"))
	uploadPart(t, h, "bkt", "other", other.UploadID, 2, randData(5<<20, 8))
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}

	// Damage the state while the store is closed: an unreferenced staged
	// blob, part records without an upload, a part whose blob is gone, a
	// malformed overlay.
	if err := state.PutStaged(ctx, "orphan", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatal(err)
	}
	if err := state.PutRecord(ctx, "p/nosuchupload/00001", []byte(`{"n":1,"size":1,"etag":"e","file":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := state.PutRecord(ctx, "m/bkt/broken", []byte(`{`)); err != nil {
		t.Fatal(err)
	}
	lost := stagedOf(t, h, other.UploadID)
	if len(lost) != 1 {
		t.Fatalf("staged parts of the other upload: %q", lost)
	}
	if err := state.DeleteStaged(ctx, lost[0]); err != nil {
		t.Fatal(err)
	}

	// Failing to read the records fails Open.
	state.set(&state.failRecords, true)
	if _, err := native.Open(h.opts); !errors.Is(err, errInjected) {
		t.Errorf("Open with failing records: %v", err)
	}
	state.set(&state.failRecords, false)

	// Failing to list staged parts does not: stale state is kept.
	state.set(&state.failList, true)
	h.open()
	if !slices.Contains(stagedFiles(t, h), "orphan") {
		t.Error("orphan blob removed although listing failed")
	}
	if parts, err := h.ListParts(ctx, "bkt", "other", other.UploadID); err != nil || len(parts) != 2 {
		t.Errorf("parts of the other upload without a listing: %+v, %v", parts, err)
	}
	state.set(&state.failList, false)

	// A clean Open removes the stale state.
	h.open()
	if slices.Contains(stagedFiles(t, h), "orphan") {
		t.Error("orphan blob kept")
	}
	recs, _ := state.Records(ctx)
	if _, ok := recs["p/nosuchupload/00001"]; ok {
		t.Error("part record without an upload kept")
	}
	if _, ok := recs[fmt.Sprintf("p/%s/00001", other.UploadID)]; ok {
		t.Error("part record of a missing staged part kept")
	}
	parts, err := h.ListParts(ctx, "bkt", "other", other.UploadID)
	if err != nil || len(parts) != 1 || parts[0].PartNumber != 2 {
		t.Errorf("parts of the other upload: %+v, %v; want part 2 only", parts, err)
	}
	if _, err := h.CompleteMultipartUpload(ctx, "bkt", "obj", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2", ContentType: "a/b"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, data := h.read("bkt", "obj", ""); !bytes.Equal(data, slices.Concat(big, small)) {
		t.Error("content after reopen differs")
	}
	if staged := stagedOf(t, h, u.UploadID); len(staged) != 0 {
		t.Errorf("staged parts left after completion: %q", staged)
	}
	recs, _ = state.Records(ctx)
	for k := range recs {
		if strings.Contains(k, u.UploadID) {
			t.Errorf("record %q left after completion", k)
		}
	}

	// Write failures fail the operation and change nothing.
	state.set(&state.failPutStaged, true)
	w, err := h.NewPartWriter(ctx, "bkt", "other", other.UploadID, 3)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("small"))
	if _, err := w.Commit(ctx, storage.PartMeta{ETag: "x"}); !errors.Is(err, errInjected) {
		t.Errorf("part commit with failing staging: %v", err)
	}
	state.set(&state.failPutStaged, false)
	state.set(&state.failPutRecord, true)
	w, err = h.NewPartWriter(ctx, "bkt", "other", other.UploadID, 3)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("small"))
	if _, err := w.Commit(ctx, storage.PartMeta{ETag: "x"}); !errors.Is(err, errInjected) {
		t.Errorf("part commit with failing records: %v", err)
	}
	if staged := stagedOf(t, h, other.UploadID); len(staged) != 1 {
		t.Errorf("staged blobs after a failed part record write: %q, want the new one kept", staged)
	}
	if _, err := h.CreateMultipartUpload(ctx, "bkt", "new", storage.ObjectMeta{}, ""); !errors.Is(err, errInjected) {
		t.Errorf("CreateMultipartUpload with failing records: %v", err)
	}
	// An overlay that cannot be stored fails the metadata update.
	h.mustPut("bkt", "o", []byte("x"), storage.ObjectMeta{})
	if _, err := h.UpdateObjectMeta(ctx, "bkt", "o", "", func(m *storage.ObjectMeta) error { m.ACL = "public-read"; return nil }); !errors.Is(err, errInjected) {
		t.Errorf("UpdateObjectMeta with failing records: %v", err)
	}
	state.set(&state.failPutRecord, false)
	if acl := h.info("bkt", "o", "").ACL; acl != "" {
		t.Errorf("ACL %q after a failed update", acl)
	}
	if parts, _ := h.ListParts(ctx, "bkt", "other", other.UploadID); len(parts) != 1 {
		t.Errorf("parts after failed commits: %+v", parts)
	}
	if ups, _ := h.ListMultipartUploads(ctx, "bkt"); len(ups) != 1 {
		t.Errorf("uploads after a failed create: %+v", ups)
	}
}

func TestStagedCompletionReadsLazily(t *testing.T) {
	h, state := newFaulty(t)
	u, err := h.CreateMultipartUpload(t.Context(), "bkt", "k", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for n := 1; n <= 4; n++ {
		p := randData(1<<20+n, byte(n))
		want = append(want, p...)
		uploadPart(t, h, "bkt", "k", u.UploadID, n, p)
	}
	// Complete parts 1, 3 and 4 only: part 2 is never opened.
	if _, err := h.CompleteMultipartUpload(t.Context(), "bkt", "k", u.UploadID, []int{1, 3, 4}, storage.ObjectMeta{ETag: "e-3"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	p2 := 1<<20 + 1
	want = slices.Concat(want[:p2], want[p2+1<<20+2:])
	if _, data := h.read("bkt", "k", ""); !bytes.Equal(data, want) {
		t.Error("content differs")
	}
	state.mu.Lock()
	opened := state.openCalls
	state.mu.Unlock()
	if opened != 3 {
		t.Errorf("staged parts opened %d times, want 3", opened)
	}
}

// TestAmbiguousStateWrites covers state writes that report an error but
// landed: nothing they refer to may be deleted.
func TestAmbiguousStateWrites(t *testing.T) {
	h, state := newFaulty(t)
	u, err := h.CreateMultipartUpload(t.Context(), "bkt", "k", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "bkt", "k", u.UploadID, 1, []byte("first,"))
	state.set(&state.landThenFail, true)
	if _, err := h.CreateMultipartUpload(t.Context(), "bkt", "ghost", storage.ObjectMeta{}, ""); !errors.Is(err, errInjected) {
		t.Errorf("CreateMultipartUpload: %v", err)
	}
	w, err := h.NewPartWriter(t.Context(), "bkt", "k", u.UploadID, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("second"))
	if _, err := w.Commit(t.Context(), storage.PartMeta{ETag: "e2"}); !errors.Is(err, errInjected) {
		t.Errorf("part commit: %v", err)
	}
	state.set(&state.landThenFail, false)
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	h.open()
	ups, err := h.ListMultipartUploads(t.Context(), "bkt")
	if err != nil || len(ups) != 1 || ups[0].UploadID != u.UploadID {
		t.Fatalf("uploads after reopen: %+v, %v", ups, err)
	}
	if parts, err := h.ListParts(t.Context(), "bkt", "k", u.UploadID); err != nil || len(parts) != 2 {
		t.Fatalf("parts after reopen: %+v, %v", parts, err)
	}
	if _, err := h.CompleteMultipartUpload(t.Context(), "bkt", "k", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, data := h.read("bkt", "k", ""); string(data) != "first,second" {
		t.Errorf("content %q", data)
	}
}

func TestAbortRetriesRecordDeletion(t *testing.T) {
	h, state := newFaulty(t)
	u, err := h.CreateMultipartUpload(t.Context(), "bkt", "k", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "bkt", "k", u.UploadID, 1, randData(5<<20, 1))
	uploadPart(t, h, "bkt", "k", u.UploadID, 2, []byte("tail"))
	state.set(&state.failDelete, true)
	if err := h.AbortMultipartUpload(t.Context(), "bkt", "k", u.UploadID); !errors.Is(err, errInjected) {
		t.Errorf("Abort with failing record deletion: %v", err)
	}
	if _, err := h.GetMultipartUpload(t.Context(), "bkt", "k", u.UploadID); err != nil {
		t.Errorf("upload after a failed abort: %v", err)
	}
	if staged := stagedOf(t, h, u.UploadID); len(staged) != 1 {
		t.Errorf("staged parts after a failed abort: %q", staged)
	}
	state.set(&state.failDelete, false)
	if err := h.AbortMultipartUpload(t.Context(), "bkt", "k", u.UploadID); err != nil {
		t.Fatalf("retried Abort: %v", err)
	}
	if staged := stagedOf(t, h, u.UploadID); len(staged) != 0 {
		t.Errorf("staged parts after abort: %q", staged)
	}
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	h.open()
	if ups, _ := h.ListMultipartUploads(t.Context(), "bkt"); len(ups) != 0 {
		t.Errorf("aborted upload came back: %+v", ups)
	}
}

func TestVanishedRemoteUpload(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("bkt", storage.CreateBucketOptions{})
	abortRemote := func(key string) {
		t.Helper()
		ups, err := h.remote.Backend.ListMultipartUploads(t.Context(), "nb-bkt")
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range ups {
			if u.Key == key {
				if err := h.c.AbortMultipartUpload(t.Context(), "nb-bkt", key, u.UploadID); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatalf("no remote upload of %s", key)
	}
	u, err := h.CreateMultipartUpload(t.Context(), "bkt", "a", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "bkt", "a", u.UploadID, 1, randData(5<<20, 1))
	abortRemote("a")
	w, err := h.NewPartWriter(t.Context(), "bkt", "a", u.UploadID, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(randData(5<<20, 2))
	if _, err := w.Commit(t.Context(), storage.PartMeta{ETag: "e"}); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("part commit to a vanished remote upload: %v, want ErrUploadNotFound", err)
	}
	if _, err := h.GetMultipartUpload(t.Context(), "bkt", "a", u.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("upload after its remote upload vanished: %v", err)
	}
	u, err = h.CreateMultipartUpload(t.Context(), "bkt", "b", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "bkt", "b", u.UploadID, 1, randData(5<<20, 3))
	uploadPart(t, h, "bkt", "b", u.UploadID, 2, []byte("tail"))
	abortRemote("b")
	_, err = h.CompleteMultipartUpload(t.Context(), "bkt", "b", u.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "e-2"}, storage.CommitOptions{})
	if !errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("completion of a vanished remote upload: %v, want ErrUploadNotFound", err)
	}
	if ups, _ := h.ListMultipartUploads(t.Context(), "bkt"); len(ups) != 0 {
		t.Errorf("uploads left: %+v", ups)
	}
	if staged := stagedFiles(t, h); len(staged) != 0 {
		t.Errorf("staged parts left: %q", staged)
	}
}

// windowsState is a StateStore that, like files on Windows, refuses to
// delete a staged blob while a reader has it open; failNext makes the
// next deletion fail anyway.
type windowsState struct {
	native.StateStore

	mu       sync.Mutex
	open     map[string]int
	refused  []string
	failNext bool
}

type trackedStaged struct {
	io.ReadSeekCloser
	s    *windowsState
	name string
	once sync.Once
}

func (r *trackedStaged) Close() error {
	r.once.Do(func() {
		r.s.mu.Lock()
		r.s.open[r.name]--
		r.s.mu.Unlock()
	})
	return r.ReadSeekCloser.Close()
}

func (s *windowsState) OpenStaged(ctx context.Context, name string, size int64) (io.ReadSeekCloser, error) {
	rc, err := s.StateStore.OpenStaged(ctx, name, size)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.open[name]++
	s.mu.Unlock()
	return &trackedStaged{ReadSeekCloser: rc, s: s, name: name}, nil
}

func (s *windowsState) DeleteStaged(ctx context.Context, name string) error {
	s.mu.Lock()
	busy, fail := s.open[name] > 0, s.failNext
	s.failNext = false
	if busy {
		s.refused = append(s.refused, name)
	}
	s.mu.Unlock()
	if busy {
		return fmt.Errorf("staged blob %q: being used by another process", name)
	}
	if fail {
		return errInjected
	}
	return s.StateStore.DeleteStaged(ctx, name)
}

// TestStagedPartsClosedBeforeDelete: no staged part is deleted while a
// reader has it open (completions, aborts, replaced parts, detaching), and
// a deletion that fails is retried later.
func TestStagedPartsClosedBeforeDelete(t *testing.T) {
	state := &windowsState{StateStore: native.MemoryState(), open: map[string]int{}}
	h := newHarness(t, s3clienttest.Options{}, func(o *native.Options) { o.State = state })
	ctx := t.Context()
	h.mk("bkt", storage.CreateBucketOptions{})
	upload := func(key string, parts ...[]byte) string {
		t.Helper()
		u, err := h.CreateMultipartUpload(ctx, "bkt", key, storage.ObjectMeta{}, "")
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range parts {
			uploadPart(t, h, "bkt", key, u.UploadID, i+1, p)
		}
		return u.UploadID
	}
	// All parts staged (one PUT), with a replaced part.
	id := upload("staged", []byte("one,"), []byte("two"))
	uploadPart(t, h, "bkt", "staged", id, 2, []byte("TWO"))
	if _, err := h.CompleteMultipartUpload(ctx, "bkt", "staged", id, []int{1, 2}, storage.ObjectMeta{ETag: "e-2"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	// A staged last part after remote parts.
	id = upload("remote", randData(5<<20, 1), []byte("tail"))
	if _, err := h.CompleteMultipartUpload(ctx, "bkt", "remote", id, []int{1, 2}, storage.ObjectMeta{ETag: "e-2"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	// Aborted, and dropped with the bucket.
	id = upload("aborted", []byte("x"), []byte("y"))
	if err := h.AbortMultipartUpload(ctx, "bkt", "aborted", id); err != nil {
		t.Fatal(err)
	}
	upload("detached", []byte("x"))
	state.mu.Lock()
	refused := slices.Clone(state.refused)
	state.mu.Unlock()
	if len(refused) != 0 {
		t.Errorf("staged parts deleted while open: %q", refused)
	}

	// A deletion that fails is retried by the next cleanup.
	id = upload("retry", []byte("x"))
	state.mu.Lock()
	state.failNext = true
	state.mu.Unlock()
	if err := h.AbortMultipartUpload(ctx, "bkt", "retry", id); err != nil {
		t.Fatal(err)
	}
	if staged := stagedOf(t, h, id); len(staged) != 1 {
		t.Fatalf("staged parts after a failed deletion: %q", staged)
	}
	if err := h.st.DetachBucket(ctx, "bkt", false); err != nil {
		t.Fatal(err)
	}
	if staged := stagedFiles(t, h); len(staged) != 0 {
		t.Errorf("staged parts left: %q", staged)
	}
}
