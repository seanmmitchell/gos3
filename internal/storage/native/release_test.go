package native_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
)

func TestDiscardBucketLocalStateRetriesAfterFailedDetach(t *testing.T) {
	h, state := newFaulty(t)
	h.mk("neighbor", storage.CreateBucketOptions{})
	ctx := t.Context()
	old, err := h.CreateMultipartUpload(ctx, "bkt", "old", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "bkt", "old", old.UploadID, 1, []byte("old part"))
	neighbor, err := h.CreateMultipartUpload(ctx, "neighbor", "keep", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "neighbor", "keep", neighbor.UploadID, 1, []byte("neighbor part"))
	state.set(&state.failDelete, true)
	if err := h.st.DetachBucket(ctx, "bkt", false); !errors.Is(err, errInjected) {
		t.Fatalf("failed detach: %v", err)
	}
	state.set(&state.failDelete, false)
	state.set(&state.failRecords, true)
	if err := h.st.DiscardBucketLocalState(ctx, "bkt"); !errors.Is(err, errInjected) {
		t.Fatalf("failed state read: %v", err)
	}
	state.set(&state.failRecords, false)
	state.set(&state.failDelete, true)
	if err := h.st.DiscardBucketLocalState(ctx, "bkt"); !errors.Is(err, errInjected) {
		t.Fatalf("failed upload deletion: %v", err)
	}
	state.set(&state.failDelete, false)
	if err := h.st.DiscardBucketLocalState(ctx, "bkt"); err != nil {
		t.Fatal(err)
	}
	recs, err := state.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for key := range recs {
		if strings.Contains(key, old.UploadID) || strings.HasPrefix(key, "m/bkt/") {
			t.Errorf("old bucket state survived: %s", key)
		}
	}
	if staged := stagedOf(t, h, old.UploadID); len(staged) != 0 {
		t.Errorf("old staged parts survived: %q", staged)
	}
	h.open()
	if uploads, err := h.ListMultipartUploads(ctx, "bkt"); err != nil || len(uploads) != 0 {
		t.Fatalf("old uploads after restart: %+v, %v", uploads, err)
	}
	if _, err := h.CompleteMultipartUpload(ctx, "bkt", "old", old.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("old upload completion after restart: %v", err)
	}
	if uploads, err := h.ListMultipartUploads(ctx, "neighbor"); err != nil || len(uploads) != 1 || uploads[0].UploadID != neighbor.UploadID {
		t.Fatalf("neighbor uploads: %+v, %v", uploads, err)
	}
}

func TestDiscardBucketLocalStateRetriesOverlayDeletion(t *testing.T) {
	h, state := newFaulty(t)
	h.mustPut("bkt", "key", []byte("data"), storage.ObjectMeta{ACL: "public-read"})
	state.set(&state.failDelete, true)
	if err := h.st.DiscardBucketLocalState(t.Context(), "bkt"); !errors.Is(err, errInjected) {
		t.Fatalf("failed overlay deletion: %v", err)
	}
	state.set(&state.failDelete, false)
	if err := h.st.DiscardBucketLocalState(t.Context(), "bkt"); err != nil {
		t.Fatal(err)
	}
	h.open()
	if got := native.OverlayCount(h.st, "bkt", ""); got != 0 {
		t.Fatalf("%d overlays returned after restart", got)
	}
}
