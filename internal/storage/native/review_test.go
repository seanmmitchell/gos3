package native_test

// Regressions of the M1 code review of native buckets.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
)

// A conditional PUT refused by the remote keeps the overlay of the
// existing (null) version.
func TestReviewOverlayKeptOnFailedConditionalPut(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.mk("b", storage.CreateBucketOptions{})
	h.mustPut("b", "k", []byte("data"), storage.ObjectMeta{})
	if _, err := h.UpdateObjectMeta(ctx, "b", "k", "", func(m *storage.ObjectMeta) error {
		m.ACL = "public-read"
		m.Tags = map[string]string{"bad\ttag": "x"} // not native: overlay
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := h.put("b", "k", []byte("other"), storage.ObjectMeta{}, storage.CommitOptions{IfNoneMatch: true})
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("conditional put: %v", err)
	}
	if info := h.info("b", "k", ""); info.ACL != "public-read" || len(info.Tags) == 0 {
		t.Errorf("overlay of the unchanged object lost: ACL %q tags %v", info.ACL, info.Tags)
	}
	// Also after a restart (the record stayed).
	h.open()
	if info := h.info("b", "k", ""); info.ACL != "public-read" {
		t.Errorf("after a restart: ACL %q", info.ACL)
	}
}

// Same for the metadata of a remote multipart completion.
func TestReviewMultipartMetadataKeptOnFailedConditionalPut(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.mk("b", storage.CreateBucketOptions{})
	u, err := h.CreateMultipartUpload(ctx, "b", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p1 := uploadPart(t, h, "b", "mp", u.UploadID, 1, randData(5<<20, 1))
	p2 := uploadPart(t, h, "b", "mp", u.UploadID, 2, randData(100, 2))
	meta := storage.ObjectMeta{ETag: "deadbeefdeadbeefdeadbeefdeadbeef-2", Checksums: map[string]string{"CRC32": "AAAAAA=="}, ChecksumType: "COMPOSITE",
		PartSizes: []int64{p1.Size, p2.Size}}
	if _, err := h.CompleteMultipartUpload(ctx, "b", "mp", u.UploadID, []int{1, 2}, meta, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	before := h.info("b", "mp", "")
	if _, err := h.put("b", "mp", []byte("x"), storage.ObjectMeta{}, storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("conditional put: %v", err)
	}
	after := h.info("b", "mp", "")
	if after.ETag != before.ETag || len(after.Checksums) == 0 || len(after.PartSizes) != 2 {
		t.Errorf("metadata of the unchanged object lost: %+v", after.ObjectMeta)
	}
}

// A failed unversioned DELETE keeps the overlay.
func TestReviewOverlayKeptOnFailedDelete(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.mk("b", storage.CreateBucketOptions{})
	h.mustPut("b", "k", []byte("data"), storage.ObjectMeta{})
	if _, err := h.UpdateObjectMeta(ctx, "b", "k", "", func(m *storage.ObjectMeta) error { m.ACL = "public-read"; return nil }); err != nil {
		t.Fatal(err)
	}
	fail := func(r *http.Request) int {
		if r.Method == http.MethodDelete {
			return http.StatusForbidden
		}
		return 0
	}
	h.remote.Fail.Store(&fail)
	if _, err := h.DeleteObject(ctx, "b", "k", ""); err == nil {
		t.Fatal("delete succeeded")
	}
	h.remote.Fail.Store(nil)
	if info := h.info("b", "k", ""); info.ACL != "public-read" {
		t.Errorf("after a failed delete: ACL %q", info.ACL)
	}
}

// failFirst answers the first request matching match with a 500 after
// applying it.
func failFirst(match func(*http.Request) bool) *func(*http.Request) s3clienttest.Fault {
	var n atomic.Int32
	f := func(r *http.Request) s3clienttest.Fault {
		if match(r) && n.Add(1) == 1 {
			return s3clienttest.Status500
		}
		return s3clienttest.Pass
	}
	return &f
}

// A conditional PUT whose (only) attempt was applied but answered 500 is
// a success.
func TestReviewConditionalPutApplied(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("b", storage.CreateBucketOptions{})
	h.remote.After.Store(failFirst(func(r *http.Request) bool {
		return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/lock")
	}))
	_, err := h.put("b", "lock", []byte("mine"), storage.ObjectMeta{ACL: "public-read"}, storage.CommitOptions{IfNoneMatch: true})
	h.remote.After.Store(nil)
	if err != nil {
		t.Fatalf("applied write reported as %v", err)
	}
	info, data := h.read("b", "lock", "")
	if string(data) != "mine" || info.ACL != "public-read" {
		t.Errorf("object %q, ACL %q", data, info.ACL)
	}
	// A conditional PUT that was not applied still fails.
	if _, err := h.put("b", "lock", []byte("again"), storage.ObjectMeta{}, storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Errorf("If-None-Match on the existing object: %v", err)
	}
}

// A remote completion applied but answered 500 succeeds with gos3's
// metadata.
func TestReviewCompletionApplied(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.mk("b", storage.CreateBucketOptions{})
	u, err := h.CreateMultipartUpload(ctx, "b", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p1 := uploadPart(t, h, "b", "mp", u.UploadID, 1, randData(5<<20, 1))
	h.remote.After.Store(failFirst(func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.Query().Has("uploadId")
	}))
	meta := storage.ObjectMeta{ETag: "0123456789abcdef0123456789abcdef-1", Checksums: map[string]string{"CRC32": "AAAAAA=="}, PartSizes: []int64{p1.Size}}
	_, err = h.CompleteMultipartUpload(ctx, "b", "mp", u.UploadID, []int{1}, meta, storage.CommitOptions{})
	h.remote.After.Store(nil)
	if err != nil {
		t.Fatalf("applied completion reported as %v", err)
	}
	if info := h.info("b", "mp", ""); info.ETag != meta.ETag || len(info.Checksums) != 1 {
		t.Errorf("completed object %+v", info.ObjectMeta)
	}
}

// A large PUT (remote multipart) whose completion was applied but
// answered 500 succeeds.
func TestReviewLargePutApplied(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, func(o *native.Options) { o.PartSize = 5 << 20 })
	h.mk("b", storage.CreateBucketOptions{})
	h.remote.After.Store(failFirst(func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.Query().Has("uploadId")
	}))
	data := randData(11<<20, 3)
	_, err := h.put("b", "large", data, storage.ObjectMeta{ETag: md5hex(data)}, storage.CommitOptions{})
	h.remote.After.Store(nil)
	if err != nil {
		t.Fatalf("applied PUT reported as %v", err)
	}
	if info := h.info("b", "large", ""); info.Size != int64(len(data)) || info.ETag != md5hex(data) {
		t.Errorf("object %d bytes, ETag %q", info.Size, info.ETag)
	}
	// A PUT whose internal multipart upload vanishes never reports
	// NoSuchUpload.
	h.remote.After.Store(nil)
	abort := func(r *http.Request) int {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploadId") {
			return http.StatusNotFound
		}
		return 0
	}
	h.remote.Fail.Store(&abort)
	_, err = h.put("b", "large2", data, storage.ObjectMeta{ETag: md5hex(data)}, storage.CommitOptions{})
	h.remote.Fail.Store(nil)
	if err == nil || errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("PUT with a failing completion: %v", err)
	}
}

// Invalid UTF-8 in user metadata does not disturb the ACL.
func TestReviewACLWithInvalidUTF8(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("b", storage.CreateBucketOptions{})
	h.mustPut("b", "k", []byte("x"), storage.ObjectMeta{ACL: "public-read", WebsiteRedirect: "/r", UserMeta: map[string]string{"v": "caf\xe9"}})
	if info := h.info("b", "k", ""); info.ACL != "public-read" || info.WebsiteRedirect != "/r" {
		t.Errorf("ACL %q redirect %q", info.ACL, info.WebsiteRedirect)
	}
}

// A foreign overwrite between gos3's PUT and its read-back is an
// object-level race, not a backing failure.
func TestReviewForeignRaceIsObjectError(t *testing.T) {
	var remote atomic.Pointer[s3clienttest.Remote]
	var fired atomic.Bool
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/big") && fired.CompareAndSwap(false, true) {
				ow, err := remote.Load().Backend.NewObjectWriter(r.Context(), "nb-b", "big")
				if err != nil {
					panic(err)
				}
				_, _ = ow.Write([]byte("foreign"))
				if _, err := ow.Commit(r.Context(), storage.ObjectMeta{ETag: md5hex([]byte("foreign"))}, storage.CommitOptions{}); err != nil {
					panic(err)
				}
			}
		})
	}
	h := newHarness(t, s3clienttest.Options{Wrap: wrap}, nil)
	remote.Store(h.remote)
	h.mk("b", storage.CreateBucketOptions{})
	_, err := h.put("b", "big", []byte("mine"), storage.ObjectMeta{ACL: "public-read"}, storage.CommitOptions{})
	if !errors.Is(err, storage.ErrPreconditionFailed) || errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("race with a foreign write: %v, want ErrPreconditionFailed", err)
	}
	if info := h.info("b", "big", ""); info.ACL != "" {
		t.Errorf("foreign object: ACL %q", info.ACL)
	}
}

// Overlays of versions removed outside gos3 are pruned: by lookups, by
// listings and by the background sweep.
func TestReviewOverlayPruning(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.mkVersioned("b")
	put := func(key string, n int) []storage.ObjectInfo {
		var vs []storage.ObjectInfo
		for i := range n {
			v := h.mustPut("b", key, []byte(fmt.Sprint(i)), storage.ObjectMeta{ACL: "public-read"})
			vs = append(vs, v)
		}
		return vs
	}
	removeRemote := func(key string, vs []storage.ObjectInfo) {
		for _, v := range vs {
			if _, err := h.c.DeleteObjectWith(ctx, "nb-b", key, s3client.DeleteOptions{VersionID: v.VersionID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	lookup := put("lookup", 2)
	listed := put("listed", 3)
	swept := put("swept", 2)
	removeRemote("lookup", lookup)
	removeRemote("listed", listed[:2])
	removeRemote("swept", swept)

	for _, v := range lookup {
		if _, err := h.GetObjectInfo(ctx, "b", "lookup", v.VersionID); !errors.Is(err, storage.ErrVersionNotFound) {
			t.Fatal(err)
		}
	}
	if n := native.OverlayCount(h.st, "b", "lookup"); n != 0 {
		t.Errorf("%d overlays left after lookups found the versions gone", n)
	}
	h.versions("b", "listed", "", "")
	if n := native.OverlayCount(h.st, "b", "listed"); n != 1 {
		t.Errorf("%d overlays of listed left after a listing of its versions, want 1 (the remaining version's)", n)
	}
	if info := h.info("b", "listed", ""); info.ACL != "public-read" {
		t.Errorf("remaining version: ACL %q", info.ACL)
	}
	if n := native.OverlayCount(h.st, "b", "swept"); n != 2 {
		t.Fatalf("%d overlays of swept before the sweep", n)
	}
	native.SweepOverlays(h.st, ctx)
	if n := native.OverlayCount(h.st, "b", ""); n != 1 {
		t.Errorf("%d overlays after the sweep, want 1", n)
	}
	// The records are gone too.
	h.open()
	if n := native.OverlayCount(h.st, "b", ""); n != 1 {
		t.Errorf("%d overlays after a restart, want 1", n)
	}
}
