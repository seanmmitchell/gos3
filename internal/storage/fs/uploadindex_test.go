package fs

import (
	"fmt"
	"slices"
	"testing"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func uploadIDs(t *testing.T, b *Backend, bucket string) []string {
	t.Helper()
	ups, err := b.ListMultipartUploads(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, u := range ups {
		ids = append(ids, u.UploadID)
	}
	return ids
}

// TestUploadIndexBuiltAtOpen: databases without the listing-order index
// (written before it existed), or with one that is out of step with the
// upload records, are (re)indexed when opened.
func TestUploadIndexBuiltAtOpen(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir)
	mkBucket(t, b, "b", storage.VersioningUnversioned)
	mkBucket(t, b, "empty", storage.VersioningUnversioned)
	for i := range 30 {
		if _, err := b.CreateMultipartUpload(ctx, "b", fmt.Sprintf("k%02d", (i*7)%30), storage.ObjectMeta{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	want := uploadIDs(t, b, "b")
	if len(want) != 30 {
		t.Fatalf("%d uploads", len(want))
	}

	mutate := func(fn func(bb *bbolt.Bucket) error) {
		t.Helper()
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := bbolt.Open(dir+"/.gos3/meta.db", 0o600, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket(topBuckets).ForEach(func(_, v []byte) error {
				var br bucketRecord
				if err := decodeRecord(v, &br); err != nil {
					return err
				}
				return fn(tx.Bucket(bucketDataName(br.ID)))
			})
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		b = openTest(t, dir)
	}

	// No index at all.
	mutate(func(bb *bbolt.Bucket) error { return bb.DeleteBucket(subUploadOrder) })
	if got := uploadIDs(t, b, "b"); !slices.Equal(got, want) {
		t.Fatalf("after reindex: %v, want %v", got, want)
	}
	if got := uploadIDs(t, b, "empty"); len(got) != 0 {
		t.Fatalf("empty bucket lists %v", got)
	}

	// An index missing entries (an older version created uploads).
	mutate(func(bb *bbolt.Bucket) error {
		c := bb.Bucket(subUploadOrder).Cursor()
		if k, _ := c.First(); k != nil {
			return c.Delete()
		}
		return nil
	})
	if got := uploadIDs(t, b, "b"); !slices.Equal(got, want) {
		t.Fatalf("after rebuilding a short index: %v, want %v", got, want)
	}

	// The index is maintained by create, complete and abort.
	if err := b.AbortMultipartUpload(ctx, "b", "k00", want[0]); err != nil {
		t.Fatal(err)
	}
	u, err := b.CreateMultipartUpload(ctx, "b", "a", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, w := uploadIDs(t, b, "b"), append([]string{u.UploadID}, want[1:]...); !slices.Equal(got, w) {
		t.Fatalf("after abort and create: %v, want %v", got, w)
	}
	part, err := b.NewPartWriter(ctx, "b", "a", u.UploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	pi, err := part.Commit(ctx, storage.PartMeta{ETag: md5hex("")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CompleteMultipartUpload(ctx, "b", "a", u.UploadID, []int{pi.PartNumber}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := uploadIDs(t, b, "b"); !slices.Equal(got, want[1:]) {
		t.Fatalf("after complete: %v, want %v", got, want[1:])
	}
	if err := b.db.View(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucket(tx, "b")
		if err != nil {
			return err
		}
		if n, m := bb.Bucket(subUploadOrder).Stats().KeyN, bb.Bucket(subUploads).Stats().KeyN; n != m || n != 29 {
			t.Errorf("index has %d entries for %d uploads (want 29)", n, m)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
