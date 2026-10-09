package fs

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func uploadPart(t testing.TB, b storage.Backend, bucket, key, id string, n int, data string) storage.PartInfo {
	t.Helper()
	w, err := b.NewPartWriter(ctx, bucket, key, id, n)
	if err != nil {
		t.Fatalf("NewPartWriter %d: %v", n, err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	pi, err := w.Commit(ctx, storage.PartMeta{ETag: md5hex(data), Checksums: map[string]string{"CRC32": "c"}})
	if err != nil {
		t.Fatalf("part %d commit: %v", n, err)
	}
	return pi
}

func partFiles(t testing.TB, b *Backend) []string { return listFiles(t, b.uploadDir) }

func TestMultipart(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	meta := storage.ObjectMeta{ContentType: "application/x-test", UserMeta: map[string]string{"m": "1"}}
	up, err := b.CreateMultipartUpload(ctx, bkt, "big", meta, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !validUploadID(up.UploadID) || up.Key != "big" || up.Bucket != bkt || up.Initiator != "alice" || up.Initiated.IsZero() || up.Meta.UserMeta["m"] != "1" {
		t.Fatalf("upload %+v", up)
	}
	got, err := b.GetMultipartUpload(ctx, bkt, "big", up.UploadID)
	if err != nil || got.UploadID != up.UploadID || got.Meta.ContentType != "application/x-test" {
		t.Fatalf("GetMultipartUpload = %+v, %v", got, err)
	}
	for _, c := range []struct{ key, id string }{
		{"other", up.UploadID},
		{"big", "nope"},
		{"big", strings.Repeat("A", uploadIDLen)},
		{"big", "../../../../etc/passwd" + strings.Repeat("A", 10)},
	} {
		if _, err := b.GetMultipartUpload(ctx, bkt, c.key, c.id); !errors.Is(err, storage.ErrUploadNotFound) {
			t.Errorf("GetMultipartUpload(%q, %q): %v", c.key, c.id, err)
		}
		if _, err := b.NewPartWriter(ctx, bkt, c.key, c.id, 1); !errors.Is(err, storage.ErrUploadNotFound) {
			t.Errorf("NewPartWriter(%q, %q): %v", c.key, c.id, err)
		}
	}
	for _, n := range []int{0, -1, MaxPartNumber + 1} {
		if _, err := b.NewPartWriter(ctx, bkt, "big", up.UploadID, n); err == nil {
			t.Errorf("part number %d accepted", n)
		}
	}

	p3 := uploadPart(t, b, bkt, "big", up.UploadID, 3, "ccc")
	uploadPart(t, b, bkt, "big", up.UploadID, 1, "a-old")
	p1 := uploadPart(t, b, bkt, "big", up.UploadID, 1, "aaaa") // replaces
	uploadPart(t, b, bkt, "big", up.UploadID, 2, "bb")
	uploadPart(t, b, bkt, "big", up.UploadID, 5, "unlisted")
	if p3.PartNumber != 3 || p3.Size != 3 || p3.ETag != md5hex("ccc") || p3.Checksums["CRC32"] != "c" || p3.LastModified.IsZero() {
		t.Fatalf("part info %+v", p3)
	}
	eq(t, "part files (replaced part removed)", len(partFiles(t, b)), 4)

	parts, err := b.ListParts(ctx, bkt, "big", up.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, p := range parts {
		nums = append(nums, p.PartNumber)
	}
	if len(nums) != 4 || nums[0] != 1 || nums[1] != 2 || nums[2] != 3 || nums[3] != 5 || parts[0].ETag != p1.ETag || parts[0].Size != 4 {
		t.Fatalf("ListParts = %+v", parts)
	}
	if _, err := b.ListParts(ctx, bkt, "other", up.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("ListParts wrong key: %v", err)
	}

	// Missing part.
	if _, err := b.CompleteMultipartUpload(ctx, bkt, "big", up.UploadID, []int{1, 4}, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrPartNotFound) {
		t.Fatalf("complete with missing part: %v", err)
	}
	// Failed precondition keeps the upload.
	put(t, b, bkt, "big", "existing")
	if _, err := b.CompleteMultipartUpload(ctx, bkt, "big", up.UploadID, []int{1, 2, 3}, storage.ObjectMeta{}, storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("complete with failed precondition: %v", err)
	}
	if _, err := b.GetMultipartUpload(ctx, bkt, "big", up.UploadID); err != nil {
		t.Fatalf("upload gone after failed complete: %v", err)
	}

	final := storage.ObjectMeta{ETag: "abc-3", PartSizes: []int64{4, 2, 3}}
	oi, err := b.CompleteMultipartUpload(ctx, bkt, "big", up.UploadID, []int{1, 2, 3}, final, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if oi.Size != 9 || oi.ETag != "abc-3" || !oi.IsLatest || oi.VersionID == storage.NullVersionID || len(oi.PartSizes) != 3 {
		t.Fatalf("complete info %+v", oi)
	}
	eq(t, "content", read(t, b, bkt, "big", ""), "aaaabbccc")
	eq(t, "part files after complete", len(partFiles(t, b)), 0)
	if _, err := os.Stat(b.uploadPath(up.UploadID)); !os.IsNotExist(err) {
		t.Fatalf("upload dir still exists: %v", err)
	}
	if _, err := b.GetMultipartUpload(ctx, bkt, "big", up.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("upload after complete: %v", err)
	}
	if _, err := b.CompleteMultipartUpload(ctx, bkt, "big", up.UploadID, []int{1}, final, storage.CommitOptions{}); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("second complete: %v", err)
	}
	if ups, _ := b.ListMultipartUploads(ctx, bkt); len(ups) != 0 {
		t.Fatalf("uploads left: %+v", ups)
	}
	eq(t, "tmp files", len(tmpFiles(t, b)), 0)
}

func TestMultipartAbortAndStaleWriter(t *testing.T) {
	b, bkt := newBucket(t, "")
	up, _ := b.CreateMultipartUpload(ctx, bkt, "k", storage.ObjectMeta{}, "")
	uploadPart(t, b, bkt, "k", up.UploadID, 1, "one")
	w, err := b.NewPartWriter(ctx, bkt, "k", up.UploadID, 2)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "two")
	if err := b.AbortMultipartUpload(ctx, bkt, "other", up.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("abort wrong key: %v", err)
	}
	if err := b.AbortMultipartUpload(ctx, bkt, "k", up.UploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, storage.PartMeta{}); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("commit part of aborted upload: %v", err)
	}
	eq(t, "part files", len(partFiles(t, b)), 0)
	eq(t, "tmp files", len(tmpFiles(t, b)), 0)
	if err := b.AbortMultipartUpload(ctx, bkt, "k", up.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("double abort: %v", err)
	}
	if err := b.AbortMultipartUpload(ctx, "nope", "k", up.UploadID); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("abort missing bucket: %v", err)
	}

	// Part writer abort.
	up2, _ := b.CreateMultipartUpload(ctx, bkt, "k", storage.ObjectMeta{}, "")
	w, _ = b.NewPartWriter(ctx, bkt, "k", up2.UploadID, 1)
	io.WriteString(w, "x")
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, storage.PartMeta{}); err == nil {
		t.Fatal("commit after abort succeeded")
	}
	if parts, _ := b.ListParts(ctx, bkt, "k", up2.UploadID); len(parts) != 0 {
		t.Fatalf("aborted part listed: %+v", parts)
	}
	eq(t, "tmp files", len(tmpFiles(t, b)), 0)

	// Completing with no parts yields an empty object; unversioned → null.
	oi, err := b.CompleteMultipartUpload(ctx, bkt, "k", up2.UploadID, nil, storage.ObjectMeta{}, storage.CommitOptions{})
	if err != nil || oi.Size != 0 || oi.VersionID != storage.NullVersionID {
		t.Fatalf("empty complete = %+v, %v", oi, err)
	}
}

func TestListMultipartUploadsOrder(t *testing.T) {
	b, bkt := newBucket(t, "")
	var ids []string
	for _, k := range []string{"b", "a", "b", "c"} {
		up, err := b.CreateMultipartUpload(ctx, bkt, k, storage.ObjectMeta{}, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, up.UploadID)
		time.Sleep(2 * time.Millisecond) // distinct initiation times
	}
	ups, err := b.ListMultipartUploads(ctx, bkt)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ids[1], ids[0], ids[2], ids[3]}
	if len(ups) != len(want) {
		t.Fatalf("got %d uploads", len(ups))
	}
	for i, u := range ups {
		if u.UploadID != want[i] {
			t.Fatalf("upload %d = %s (%s), want %s", i, u.UploadID, u.Key, want[i])
		}
	}
	if _, err := b.ListMultipartUploads(ctx, "nope"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
	if _, err := b.CreateMultipartUpload(ctx, bkt, "", storage.ObjectMeta{}, ""); !errors.Is(err, storage.ErrInvalidName) {
		t.Fatalf("empty key: %v", err)
	}
}

func TestMultipartReaderStableAcrossPartReplace(t *testing.T) {
	// Re-uploading a part while the upload is being completed must never
	// corrupt the result: either the completion uses the old part or it
	// fails cleanly.
	b, bkt := newBucket(t, "")
	up, _ := b.CreateMultipartUpload(ctx, bkt, "k", storage.ObjectMeta{}, "")
	uploadPart(t, b, bkt, "k", up.UploadID, 1, strings.Repeat("a", 1<<20))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			w, err := b.NewPartWriter(ctx, bkt, "k", up.UploadID, 1)
			if err != nil {
				return // completed already
			}
			io.WriteString(w, strings.Repeat("b", 1<<20))
			if _, err := w.Commit(ctx, storage.PartMeta{}); err != nil {
				return
			}
		}
	}()
	oi, err := b.CompleteMultipartUpload(ctx, bkt, "k", up.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{})
	<-done
	if err != nil {
		if !errors.Is(err, errPartsChanged) {
			t.Fatalf("complete: %v", err)
		}
		return
	}
	data := read(t, b, bkt, "k", "")
	if oi.Size != 1<<20 || (data != strings.Repeat("a", 1<<20) && data != strings.Repeat("b", 1<<20)) {
		t.Fatalf("corrupt result: size %d", oi.Size)
	}
	waitRecovery(t, b)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// Whatever the interleaving, nothing of the upload may survive a restart.
	b2 := openTest(t, b.Root())
	waitRecovery(t, b2)
	eq(t, "part files", len(partFiles(t, b2)), 0)
}

func TestCompleteRetriesWhenPartReplaced(t *testing.T) {
	b, bkt := newBucket(t, "")
	up, _ := b.CreateMultipartUpload(ctx, bkt, "k", storage.ObjectMeta{}, "")
	uploadPart(t, b, bkt, "k", up.UploadID, 1, "old-1|")
	uploadPart(t, b, bkt, "k", up.UploadID, 2, "old-2")

	var calls int
	afterPartsCopied = func(id string) {
		calls++
		if calls == 1 {
			// Replace part 2 after its old content was copied.
			uploadPart(t, b, bkt, "k", id, 2, "new-2")
		}
	}
	t.Cleanup(func() { afterPartsCopied = nil })

	oi, err := b.CompleteMultipartUpload(ctx, bkt, "k", up.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "x-2"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "attempts", calls, 2)
	eq(t, "size", oi.Size, int64(len("old-1|new-2")))
	eq(t, "content", read(t, b, bkt, "k", ""), "old-1|new-2")
	// The data file of the discarded first attempt was removed.
	eq(t, "data files", len(dataFiles(t, b)), 1)
	eq(t, "part files", len(partFiles(t, b)), 0)

	// Constant replacement gives up after a bounded number of attempts and
	// leaves the upload intact.
	up2, _ := b.CreateMultipartUpload(ctx, bkt, "k2", storage.ObjectMeta{}, "")
	uploadPart(t, b, bkt, "k2", up2.UploadID, 1, "p")
	calls = 0
	afterPartsCopied = func(id string) {
		calls++
		uploadPart(t, b, bkt, "k2", id, 1, "q")
	}
	_, err = b.CompleteMultipartUpload(ctx, bkt, "k2", up2.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{})
	if !errors.Is(err, errPartsChanged) {
		t.Fatalf("constant replacement: %v", err)
	}
	eq(t, "attempts", calls, completeRetries)
	afterPartsCopied = nil
	if _, err := b.GetMultipartUpload(ctx, bkt, "k2", up2.UploadID); err != nil {
		t.Fatalf("upload removed after failed completion: %v", err)
	}
	if _, err := b.GetObjectInfo(ctx, bkt, "k2", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("object created by failed completion: %v", err)
	}
	eq(t, "data files", len(dataFiles(t, b)), 1)
}
