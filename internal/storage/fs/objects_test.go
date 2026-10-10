package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestBuckets(t *testing.T) {
	b := openTest(t, "")
	if bs, err := b.ListBuckets(ctx); err != nil || len(bs) != 0 {
		t.Fatalf("ListBuckets on empty = %v, %v", bs, err)
	}
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if err := b.CreateBucket(ctx, n, storage.CreateBucketOptions{Region: "eu-west-1", Owner: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.CreateBucket(ctx, "alpha", storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrBucketExists) {
		t.Fatalf("duplicate create: %v", err)
	}
	for _, bad := range []string{"", strings.Repeat("x", maxBucketNameLen+1), "\xff"} {
		if err := b.CreateBucket(ctx, bad, storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrInvalidName) {
			t.Errorf("create %q: %v", bad, err)
		}
	}
	bs, err := b.ListBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, bi := range bs {
		names = append(names, bi.Name)
		if bi.Region != "eu-west-1" || bi.Owner != "alice" || bi.CreatedAt.IsZero() || bi.Versioning != "" {
			t.Errorf("bucket info %+v", bi)
		}
	}
	eq(t, "names", strings.Join(names, ","), "alpha,mid,zeta")

	if err := b.CreateBucket(ctx, "locked", storage.CreateBucketOptions{ObjectLockEnabled: true}); err != nil {
		t.Fatal(err)
	}
	bi, err := b.GetBucket(ctx, "locked")
	if err != nil || !bi.ObjectLockEnabled || bi.Versioning != storage.VersioningEnabled {
		t.Fatalf("locked bucket = %+v, %v", bi, err)
	}
	if _, err := b.GetBucket(ctx, "nope"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("GetBucket missing: %v", err)
	}

	// Versioning transitions.
	if err := b.SetBucketVersioning(ctx, "mid", "Bogus"); err == nil {
		t.Fatal("invalid status accepted")
	}
	if err := b.SetBucketVersioning(ctx, "mid", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBucketVersioning(ctx, "mid", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBucketVersioning(ctx, "mid", storage.VersioningUnversioned); err == nil {
		t.Fatal("versioned -> unversioned accepted")
	}
	if err := b.SetBucketVersioning(ctx, "nope", storage.VersioningEnabled); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("versioning missing bucket: %v", err)
	}
	if err := b.SetBucketObjectLock(ctx, "mid", true); err != nil {
		t.Fatal(err)
	}
	bi, _ = b.GetBucket(ctx, "mid")
	if !bi.ObjectLockEnabled || bi.Versioning != storage.VersioningSuspended {
		t.Fatalf("mid = %+v", bi)
	}

	if err := b.DeleteBucket(ctx, "zeta"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteBucket(ctx, "zeta"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestDeleteBucketNotEmpty(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	put(t, b, bkt, "k", "v")
	dr, err := b.DeleteObject(ctx, bkt, "k", "")
	if err != nil {
		t.Fatal(err)
	}
	// Only a delete marker and a noncurrent version remain: still not empty.
	if err := b.DeleteBucket(ctx, bkt); !errors.Is(err, storage.ErrBucketNotEmpty) {
		t.Fatalf("delete with versions: %v", err)
	}
	vs := versions(t, b, bkt, "", "", "")
	for _, v := range vs {
		if _, err := b.DeleteObject(ctx, bkt, v.key, v.id); err != nil {
			t.Fatal(err)
		}
	}
	_ = dr
	up, err := b.CreateMultipartUpload(ctx, bkt, "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// An in-progress upload does not keep the bucket alive: DeleteBucket
	// aborts it.
	if err := b.PutBucketConfig(ctx, bkt, storage.ConfigCORS, []byte("<x/>")); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteBucket(ctx, bkt); err != nil {
		t.Fatal(err)
	}
	// Recreating starts empty (no configs, unversioned).
	mkBucket(t, b, bkt, storage.VersioningUnversioned)
	if _, err := b.GetBucketConfig(ctx, bkt, storage.ConfigCORS); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("config survived recreate: %v", err)
	}
	if bi, _ := b.GetBucket(ctx, bkt); bi.Versioning != "" {
		t.Fatalf("versioning survived recreate: %+v", bi)
	}
	if got := keysOf(t, b, bkt, "", ""); len(got) != 0 {
		t.Fatalf("objects survived recreate: %v", got)
	}
	if _, err := b.GetMultipartUpload(ctx, bkt, "mp", up.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("upload survived recreate: %v", err)
	}
}

func TestBucketConfig(t *testing.T) {
	b, bkt := newBucket(t, "")
	if _, err := b.GetBucketConfig(ctx, bkt, storage.ConfigPolicy); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("unset config: %v", err)
	}
	if _, err := b.GetBucketConfig(ctx, "nope", storage.ConfigPolicy); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
	doc := []byte(`{"Version":"2012-10-17"}`)
	if err := b.PutBucketConfig(ctx, bkt, storage.ConfigPolicy, doc); err != nil {
		t.Fatal(err)
	}
	doc[0] = 'X' // the backend must have copied it
	got, err := b.GetBucketConfig(ctx, bkt, storage.ConfigPolicy)
	if err != nil || string(got) != `{"Version":"2012-10-17"}` {
		t.Fatalf("get = %q, %v", got, err)
	}
	got[0] = 'Y'
	got2, _ := b.GetBucketConfig(ctx, bkt, storage.ConfigPolicy)
	eq(t, "config after caller mutation", string(got2), `{"Version":"2012-10-17"}`)

	if err := b.PutBucketConfig(ctx, bkt, storage.ConfigACL, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetBucketConfig(ctx, bkt, storage.ConfigACL); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty config = %v, %v", got, err)
	}
	if err := b.PutBucketConfig(ctx, bkt, "", doc); !errors.Is(err, storage.ErrInvalidName) {
		t.Fatalf("empty config key: %v", err)
	}
	if err := b.PutBucketConfig(ctx, "nope", "x", doc); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("put missing bucket: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := b.DeleteBucketConfig(ctx, bkt, storage.ConfigPolicy); err != nil {
			t.Fatalf("delete #%d: %v", i, err)
		}
	}
	if _, err := b.GetBucketConfig(ctx, bkt, storage.ConfigPolicy); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("deleted config: %v", err)
	}
	if err := b.DeleteBucketConfig(ctx, "nope", "x"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("delete missing bucket: %v", err)
	}
}

func TestUnversionedObjects(t *testing.T) {
	b, bkt := newBucket(t, "")
	meta := storage.ObjectMeta{
		ETag:          md5hex("hello"),
		ContentType:   "text/plain",
		UserMeta:      map[string]string{"a": "1"},
		Tags:          map[string]string{"t": "v"},
		Checksums:     map[string]string{"CRC32": "AAAAAA=="},
		PartSizes:     []int64{5},
		PartChecksums: []map[string]string{{"CRC32": "x"}},
	}
	w, err := b.NewObjectWriter(ctx, bkt, "dir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "hello"); err != nil {
		t.Fatal(err)
	}
	oi, err := w.Commit(ctx, meta, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	meta.UserMeta["a"] = "mutated"
	if oi.VersionID != storage.NullVersionID || !oi.IsLatest || oi.Size != 5 || oi.LastModified.IsZero() || oi.Bucket != bkt || oi.Key != "dir/a.txt" {
		t.Fatalf("commit info %+v", oi)
	}
	got, err := b.GetObjectInfo(ctx, bkt, "dir/a.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserMeta["a"] != "1" || got.Tags["t"] != "v" || got.PartSizes[0] != 5 || got.PartChecksums[0]["CRC32"] != "x" {
		t.Fatalf("stored meta %+v", got.ObjectMeta)
	}
	if !got.LastModified.Equal(oi.LastModified) || got.ETag != md5hex("hello") {
		t.Fatalf("GetObjectInfo %+v vs commit %+v", got, oi)
	}
	eq(t, "content", read(t, b, bkt, "dir/a.txt", ""), "hello")
	eq(t, "content by null id", read(t, b, bkt, "dir/a.txt", storage.NullVersionID), "hello")
	eq(t, "data files", len(dataFiles(t, b)), 1)

	// Overwrite replaces the null version and unlinks the old data file.
	put(t, b, bkt, "dir/a.txt", "world!")
	eq(t, "content", read(t, b, bkt, "dir/a.txt", ""), "world!")
	eq(t, "versions", joinVers(versions(t, b, bkt, "", "", "")), "dir/a.txt@null*")
	eq(t, "data files after overwrite", len(dataFiles(t, b)), 1)

	// Errors.
	if _, err := b.GetObjectInfo(ctx, bkt, "missing", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := b.GetObjectInfo(ctx, bkt, "missing", "null"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("missing key with version: %v", err)
	}
	for _, vid := range []string{"bogus", "AAAAAAAAAAAAAAAAAAAAAA", strings.Repeat("x", 300)} {
		if _, err := b.GetObjectInfo(ctx, bkt, "dir/a.txt", vid); !errors.Is(err, storage.ErrVersionNotFound) {
			t.Fatalf("unknown version %q: %v", vid, err)
		}
	}
	if _, err := b.GetObjectInfo(ctx, "nope", "k", ""); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
	if _, err := b.NewObjectWriter(ctx, "nope", "k"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("writer missing bucket: %v", err)
	}
	for _, bad := range []string{"", "\xff\xfe", strings.Repeat("k", maxKeyLen+1)} {
		if _, err := b.NewObjectWriter(ctx, bkt, bad); !errors.Is(err, storage.ErrInvalidName) {
			t.Fatalf("writer key %q: %v", bad, err)
		}
		if _, err := b.GetObjectInfo(ctx, bkt, bad, ""); !errors.Is(err, storage.ErrObjectNotFound) {
			t.Fatalf("get invalid key: %v", err)
		}
	}

	// Delete: missing keys are fine, result is always empty when unversioned.
	if dr, err := b.DeleteObject(ctx, bkt, "missing", ""); err != nil || dr != (storage.DeleteResult{}) {
		t.Fatalf("delete missing = %+v, %v", dr, err)
	}
	if dr, err := b.DeleteObject(ctx, bkt, "dir/a.txt", ""); err != nil || dr != (storage.DeleteResult{}) {
		t.Fatalf("delete = %+v, %v", dr, err)
	}
	if _, err := b.GetObjectInfo(ctx, bkt, "dir/a.txt", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("deleted key: %v", err)
	}
	eq(t, "data files after delete", len(dataFiles(t, b)), 0)
	if _, err := b.DeleteObject(ctx, bkt, "dir/a.txt", "null"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("delete missing null version: %v", err)
	}
}

func TestEmptyObject(t *testing.T) {
	b, bkt := newBucket(t, "")
	put(t, b, bkt, "folder/", "")
	eq(t, "data files", len(dataFiles(t, b)), 0)
	info, r, err := b.OpenObject(ctx, bkt, "folder/", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	eq(t, "size", info.Size, int64(0))
	if n, err := r.Seek(0, io.SeekEnd); err != nil || n != 0 {
		t.Fatalf("seek end = %d, %v", n, err)
	}
	data, err := io.ReadAll(r)
	if err != nil || len(data) != 0 {
		t.Fatalf("read = %q, %v", data, err)
	}
}

func TestEnabledVersioning(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	v1 := put(t, b, bkt, "k", "one")
	v2 := put(t, b, bkt, "k", "two")
	if v1.VersionID == storage.NullVersionID || v1.VersionID == v2.VersionID || len(v1.VersionID) > 64 {
		t.Fatalf("version ids %q %q", v1.VersionID, v2.VersionID)
	}
	for _, c := range v1.VersionID {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", c) {
			t.Fatalf("version id %q not URL-safe", v1.VersionID)
		}
	}
	eq(t, "latest", read(t, b, bkt, "k", ""), "two")
	eq(t, "v1", read(t, b, bkt, "k", v1.VersionID), "one")
	info, _ := b.GetObjectInfo(ctx, bkt, "k", v1.VersionID)
	if info.IsLatest {
		t.Fatal("v1 reported latest")
	}
	want := fmt.Sprintf("k@%s* k@%s", v2.VersionID, v1.VersionID)
	eq(t, "versions", joinVers(versions(t, b, bkt, "", "", "")), want)

	// Delete marker hides the key.
	dr, err := b.DeleteObject(ctx, bkt, "k", "")
	if err != nil || !dr.DeleteMarker || dr.VersionID == "" || dr.VersionID == storage.NullVersionID {
		t.Fatalf("delete = %+v, %v", dr, err)
	}
	li, err := b.GetObjectInfo(ctx, bkt, "k", "")
	if err != nil || !li.DeleteMarker || !li.IsLatest || li.VersionID != dr.VersionID {
		t.Fatalf("latest after delete = %+v, %v", li, err)
	}
	if info, r, err := b.OpenObject(ctx, bkt, "k", ""); err != nil || r != nil || !info.DeleteMarker {
		t.Fatalf("open delete marker = %+v, %v, %v", info, r, err)
	}
	if got := keysOf(t, b, bkt, "", ""); len(got) != 0 {
		t.Fatalf("WalkObjects shows deleted key: %v", got)
	}
	if _, err := b.UpdateObjectMeta(ctx, bkt, "k", "", func(*storage.ObjectMeta) error { return nil }); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("update delete marker: %v", err)
	}
	// Delete marker of a key that never existed.
	if dr, err := b.DeleteObject(ctx, bkt, "ghost", ""); err != nil || !dr.DeleteMarker {
		t.Fatalf("delete ghost = %+v, %v", dr, err)
	}

	// Removing the delete marker restores v2.
	if dr2, err := b.DeleteObject(ctx, bkt, "k", dr.VersionID); err != nil || !dr2.DeleteMarker || dr2.VersionID != dr.VersionID {
		t.Fatalf("delete marker removal = %+v, %v", dr2, err)
	}
	eq(t, "restored", read(t, b, bkt, "k", ""), "two")
	eq(t, "keys", strings.Join(keysOf(t, b, bkt, "", ""), ","), "k")

	// Removing the latest version promotes the next newest.
	if dr, err := b.DeleteObject(ctx, bkt, "k", v2.VersionID); err != nil || dr.DeleteMarker || dr.VersionID != v2.VersionID {
		t.Fatalf("delete v2 = %+v, %v", dr, err)
	}
	li, _ = b.GetObjectInfo(ctx, bkt, "k", "")
	eq(t, "promoted", li.VersionID, v1.VersionID)
	if !li.IsLatest {
		t.Fatal("promoted version not latest")
	}
	if _, err := b.DeleteObject(ctx, bkt, "k", v2.VersionID); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("double version delete: %v", err)
	}
	if _, err := b.DeleteObject(ctx, bkt, "k", "null"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("delete missing null: %v", err)
	}
	eq(t, "data files", len(dataFiles(t, b)), 1)
}

func TestSuspendedVersioning(t *testing.T) {
	b, bkt := newBucket(t, "")
	put(t, b, bkt, "k", "null-1") // null version from the unversioned era
	if err := b.SetBucketVersioning(ctx, bkt, storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	ve := put(t, b, bkt, "k", "enabled")
	if err := b.SetBucketVersioning(ctx, bkt, storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	vs := versions(t, b, bkt, "", "", "")
	eq(t, "before", joinVers(vs), fmt.Sprintf("k@%s* k@null", ve.VersionID))

	// A suspended write replaces the null version and becomes latest.
	n := put(t, b, bkt, "k", "null-2")
	eq(t, "vid", n.VersionID, storage.NullVersionID)
	eq(t, "after put", joinVers(versions(t, b, bkt, "", "", "")), fmt.Sprintf("k@null* k@%s", ve.VersionID))
	eq(t, "null content", read(t, b, bkt, "k", "null"), "null-2")
	eq(t, "data files", len(dataFiles(t, b)), 2)

	// A suspended delete creates a null delete marker replacing the null version.
	dr, err := b.DeleteObject(ctx, bkt, "k", "")
	if err != nil || dr != (storage.DeleteResult{VersionID: storage.NullVersionID, DeleteMarker: true}) {
		t.Fatalf("delete = %+v, %v", dr, err)
	}
	eq(t, "after delete", joinVers(versions(t, b, bkt, "", "", "")), fmt.Sprintf("k@null(dm)* k@%s", ve.VersionID))
	eq(t, "data files", len(dataFiles(t, b)), 1)

	// A suspended put replaces the null delete marker.
	put(t, b, bkt, "k", "null-3")
	eq(t, "after put 2", joinVers(versions(t, b, bkt, "", "", "")), fmt.Sprintf("k@null* k@%s", ve.VersionID))
	eq(t, "latest", read(t, b, bkt, "k", ""), "null-3")
}

func TestConditionalCommit(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	none := storage.CommitOptions{IfNoneMatch: true}
	if _, err := putOpts(t, b, bkt, "k", "a", none); err != nil {
		t.Fatalf("If-None-Match on new key: %v", err)
	}
	before := len(dataFiles(t, b))
	if _, err := putOpts(t, b, bkt, "k", "b", none); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("If-None-Match on existing: %v", err)
	}
	eq(t, "data files after failed commit", len(dataFiles(t, b)), before)
	eq(t, "tmp files after failed commit", len(tmpFiles(t, b)), 0)

	cases := []struct {
		ifMatch string
		ok      bool
	}{
		{md5hex("a"), true},
		{`"` + md5hex("c") + `"`, true}, // previous write made it "c"
		{"*", true},
		{"nope", false},
	}
	data := "c"
	for _, c := range cases {
		_, err := putOpts(t, b, bkt, "k", data, storage.CommitOptions{IfMatch: c.ifMatch})
		if c.ok != (err == nil) {
			t.Fatalf("If-Match %q: %v", c.ifMatch, err)
		}
		if err != nil && !errors.Is(err, storage.ErrPreconditionFailed) {
			t.Fatalf("If-Match %q: unexpected error %v", c.ifMatch, err)
		}
	}
	if _, err := putOpts(t, b, bkt, "missing", "x", storage.CommitOptions{IfMatch: "*"}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("If-Match on missing: %v", err)
	}
	// A delete marker counts as "no current object".
	if _, err := b.DeleteObject(ctx, bkt, "k", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := putOpts(t, b, bkt, "k", "x", storage.CommitOptions{IfMatch: "*"}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("If-Match on delete marker: %v", err)
	}
	if _, err := putOpts(t, b, bkt, "k", "x", none); err != nil {
		t.Fatalf("If-None-Match on delete marker: %v", err)
	}
}

func TestConditionalCommitRace(t *testing.T) {
	b, bkt := newBucket(t, "")
	const n = 16
	errs := make(chan error, n)
	writers := make([]storage.ObjectWriter, n)
	for i := range writers {
		w, err := b.NewObjectWriter(ctx, bkt, "k")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "writer-%d", i)
		writers[i] = w
	}
	for i, w := range writers {
		go func() {
			_, err := w.Commit(ctx, storage.ObjectMeta{ETag: fmt.Sprint(i)}, storage.CommitOptions{IfNoneMatch: true})
			errs <- err
		}()
	}
	wins := 0
	for range n {
		err := <-errs
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, storage.ErrPreconditionFailed):
			t.Fatal(err)
		}
	}
	eq(t, "winners", wins, 1)
	eq(t, "data files", len(dataFiles(t, b)), 1)
}

func TestWriterLifecycle(t *testing.T) {
	b, bkt := newBucket(t, "")
	w, err := b.NewObjectWriter(ctx, bkt, "k")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "data")
	eq(t, "tmp files while buffering", len(tmpFiles(t, b)), 0) // created lazily
	w.Write(make([]byte, writeBufSize))
	eq(t, "tmp files while writing", len(tmpFiles(t, b)), 1)
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	eq(t, "tmp files after abort", len(tmpFiles(t, b)), 0)
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("write after abort succeeded")
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); err == nil {
		t.Fatal("commit after abort succeeded")
	}
	if _, err := b.GetObjectInfo(ctx, bkt, "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("aborted write visible: %v", err)
	}

	w, _ = b.NewObjectWriter(ctx, bkt, "k")
	io.WriteString(w, "data")
	if _, err := b.GetObjectInfo(ctx, bkt, "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("uncommitted write visible: %v", err)
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("abort after commit: %v", err)
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); err == nil {
		t.Fatal("double commit succeeded")
	}
	eq(t, "content", read(t, b, bkt, "k", ""), "data")

	// Canceled context at commit discards the data.
	w, _ = b.NewObjectWriter(ctx, bkt, "k2")
	io.WriteString(w, "data")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.Commit(cctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit with canceled ctx: %v", err)
	}
	eq(t, "tmp files", len(tmpFiles(t, b)), 0)
	if _, err := b.GetObjectInfo(cctx, bkt, "k", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("get with canceled ctx: %v", err)
	}
}

func TestCommitAfterBucketDeleted(t *testing.T) {
	b, bkt := newBucket(t, "")
	w, err := b.NewObjectWriter(ctx, bkt, "k")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "orphan-to-be")
	if err := b.DeleteBucket(ctx, bkt); err != nil {
		t.Fatal(err)
	}
	mkBucket(t, b, bkt, "") // recreated: a different bucket
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("commit into deleted bucket: %v", err)
	}
	eq(t, "data files", len(dataFiles(t, b)), 0)
	eq(t, "tmp files", len(tmpFiles(t, b)), 0)
	if _, err := b.GetObjectInfo(ctx, bkt, "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("object leaked into recreated bucket: %v", err)
	}
}

func TestUpdateObjectMeta(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	v1 := put(t, b, bkt, "k", "one")
	v2 := put(t, b, bkt, "k", "two")
	got, err := b.UpdateObjectMeta(ctx, bkt, "k", v1.VersionID, func(m *storage.ObjectMeta) error {
		m.Tags = map[string]string{"x": "1"}
		m.LegalHold = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tags["x"] != "1" || !got.LegalHold || got.IsLatest || got.VersionID != v1.VersionID || !got.LastModified.Equal(v1.LastModified) || got.Size != 3 {
		t.Fatalf("update result %+v", got)
	}
	got.Tags["x"] = "mutated"
	if i, _ := b.GetObjectInfo(ctx, bkt, "k", v1.VersionID); i.Tags["x"] != "1" || !i.LegalHold {
		t.Fatalf("stored %+v", i.ObjectMeta)
	}
	eq(t, "data unchanged", read(t, b, bkt, "k", v1.VersionID), "one")

	// Latest: WalkObjects must observe the update too.
	if _, err := b.UpdateObjectMeta(ctx, bkt, "k", "", func(m *storage.ObjectMeta) error {
		m.ACL = "public-read"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err = b.WalkObjects(ctx, bkt, "", "", func(o storage.ObjectInfo) error {
		if o.VersionID != v2.VersionID || o.ACL != "public-read" {
			t.Errorf("walk sees %+v", o)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// fn errors abort without changes.
	boom := errors.New("boom")
	if _, err := b.UpdateObjectMeta(ctx, bkt, "k", "", func(m *storage.ObjectMeta) error {
		m.ACL = "changed"
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("fn error: %v", err)
	}
	if i, _ := b.GetObjectInfo(ctx, bkt, "k", ""); i.ACL != "public-read" {
		t.Fatalf("failed update applied: %+v", i.ObjectMeta)
	}

	// fn may call back into the backend.
	if _, err := b.UpdateObjectMeta(ctx, bkt, "k", "", func(m *storage.ObjectMeta) error {
		_, err := b.GetObjectInfo(ctx, bkt, "k", "")
		return err
	}); err != nil {
		t.Fatalf("reentrant fn: %v", err)
	}

	if _, err := b.UpdateObjectMeta(ctx, bkt, "missing", "", func(*storage.ObjectMeta) error { return nil }); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := b.UpdateObjectMeta(ctx, bkt, "k", "bogus", func(*storage.ObjectMeta) error { return nil }); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("bogus version: %v", err)
	}
	dr, _ := b.DeleteObject(ctx, bkt, "k", "")
	if _, err := b.UpdateObjectMeta(ctx, bkt, "k", dr.VersionID, func(*storage.ObjectMeta) error { return nil }); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("explicit delete marker: %v", err)
	}
}

func TestUpdateObjectMetaConcurrent(t *testing.T) {
	b, bkt := newBucket(t, "")
	put(t, b, bkt, "k", "v")
	const n = 20
	done := make(chan error, n)
	for i := range n {
		go func() {
			_, err := b.UpdateObjectMeta(ctx, bkt, "k", "", func(m *storage.ObjectMeta) error {
				if m.UserMeta == nil {
					m.UserMeta = map[string]string{}
				}
				m.UserMeta[fmt.Sprint(i)] = "x"
				return nil
			})
			done <- err
		}()
	}
	for range n {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	info, _ := b.GetObjectInfo(ctx, bkt, "k", "")
	eq(t, "all updates applied", len(info.UserMeta), n)
}

func TestWalkObjects(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	keys := []string{"a", "a/b", "a/c", "ab", "b", "b/x/y", "c", "日本/語", "a\x00z", "a\x01"}
	for _, k := range keys {
		put(t, b, bkt, k, k)
	}
	b.DeleteObject(ctx, bkt, "b", "") // hidden by a delete marker
	sorted := slices.Clone(keys)
	sorted = slices.DeleteFunc(sorted, func(s string) bool { return s == "b" })
	slices.Sort(sorted)

	cases := []struct {
		prefix, after string
		want          []string
	}{
		{"", "", sorted},
		{"a", "", []string{"a", "a\x00z", "a\x01", "a/b", "a/c", "ab"}},
		{"a/", "", []string{"a/b", "a/c"}},
		{"a/", "a/b", []string{"a/c"}},
		{"a/", "a/", []string{"a/b", "a/c"}},
		{"a/", "0", []string{"a/b", "a/c"}},
		{"a/", "b", nil},
		{"", "a/c", []string{"ab", "b/x/y", "c", "日本/語"}},
		{"", "a/\xff", []string{"ab", "b/x/y", "c", "日本/語"}},
		{"b", "", []string{"b/x/y"}},
		{"zzz", "", nil},
		{"", "\xff", nil},
	}
	for _, c := range cases {
		got := keysOf(t, b, bkt, c.prefix, c.after)
		if !slices.Equal(got, c.want) {
			t.Errorf("WalkObjects(%q, %q) = %q, want %q", c.prefix, c.after, got, c.want)
		}
	}

	// ErrStopWalk stops cleanly; other errors propagate unchanged.
	n := 0
	err := b.WalkObjects(ctx, bkt, "", "", func(storage.ObjectInfo) error {
		n++
		return storage.ErrStopWalk
	})
	if err != nil || n != 1 {
		t.Fatalf("stop walk: n=%d err=%v", n, err)
	}
	boom := errors.New("boom")
	if err := b.WalkObjects(ctx, bkt, "", "", func(storage.ObjectInfo) error { return boom }); err != boom {
		t.Fatalf("walk error = %v", err)
	}
	if err := b.WalkObjects(ctx, "nope", "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("walk missing bucket: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := b.WalkObjects(cctx, bkt, "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("walk canceled: %v", err)
	}
}

func TestWalkLargeAndMutating(t *testing.T) {
	b, bkt := newBucket(t, "")
	const n = 1200 // spans several geometrically growing batches
	for i := range n {
		put(t, b, bkt, fmt.Sprintf("k%05d", i), "x")
	}
	// The callback mutates the bucket (deletes what it sees and adds keys
	// after the walk position): no deadlock, every original key seen once.
	seen := 0
	last := ""
	err := b.WalkObjects(ctx, bkt, "k", "", func(o storage.ObjectInfo) error {
		if o.Key <= last {
			t.Fatalf("order: %q after %q", o.Key, last)
		}
		last = o.Key
		if strings.HasPrefix(o.Key, "k") && len(o.Key) == 6 {
			seen++
			if _, err := b.DeleteObject(ctx, bkt, o.Key, ""); err != nil {
				return err
			}
		}
		if seen == 10 {
			put(t, b, bkt, "kz-added", "y")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "seen", seen, n)
	eq(t, "last", last, "kz-added")
	eq(t, "remaining", strings.Join(keysOf(t, b, bkt, "", ""), ","), "kz-added")
}

func TestWalkVersionsMarkers(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	a1 := put(t, b, bkt, "a", "1")
	a2 := put(t, b, bkt, "a", "2")
	a3, _ := b.DeleteObject(ctx, bkt, "a", "")
	b1 := put(t, b, bkt, "b", "1")
	if err := b.SetBucketVersioning(ctx, bkt, storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	bn := put(t, b, bkt, "b", "null")
	c1 := put(t, b, bkt, "c/x", "1")

	all := []ver{
		{"a", a3.VersionID, true, true},
		{"a", a2.VersionID, false, false},
		{"a", a1.VersionID, false, false},
		{"b", bn.VersionID, false, true},
		{"b", b1.VersionID, false, false},
		{"c/x", c1.VersionID, false, true},
	}
	cases := []struct {
		name, prefix, km, vm string
		want                 []ver
	}{
		{"all", "", "", "", all},
		{"prefix", "b", "", "", all[3:5]},
		{"key marker", "", "a", "", all[3:]},
		{"version marker", "", "a", a3.VersionID, all[1:]},
		{"version marker mid", "", "a", a2.VersionID, all[2:]},
		{"version marker last", "", "a", a1.VersionID, all[3:]},
		{"null marker", "", "b", "null", all[4:]},
		{"missing null marker", "", "a", "null", all[3:]},
		{"garbage marker", "", "a", "garbage", all[3:]},
		{"marker before prefix", "c", "a", "", all[5:]},
		{"marker after prefix", "a", "b", "", nil},
		{"unknown key marker", "", "aa", "", all[3:]},
	}
	for _, c := range cases {
		got := versions(t, b, bkt, c.prefix, c.km, c.vm)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: got %s\nwant %s", c.name, joinVers(got), joinVers(c.want))
		}
	}

	// A marker version deleted between pages still resumes at its position.
	if _, err := b.DeleteObject(ctx, bkt, "a", a2.VersionID); err != nil {
		t.Fatal(err)
	}
	got := versions(t, b, bkt, "", "a", a2.VersionID)
	want := append([]ver{{"a", a1.VersionID, false, false}}, all[3:]...)
	if !slices.Equal(got, want) {
		t.Errorf("deleted marker: got %s\nwant %s", joinVers(got), joinVers(want))
	}
}

func TestWalkVersionsManyAcrossBatches(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	const n = 1200
	ids := make([]string, n)
	for i := range n {
		ids[n-1-i] = put(t, b, bkt, "k", fmt.Sprint(i)).VersionID
	}
	put(t, b, bkt, "l", "x")
	vs := versions(t, b, bkt, "", "", "")
	eq(t, "count", len(vs), n+1)
	for i := range n {
		if vs[i].id != ids[i] || vs[i].latest != (i == 0) {
			t.Fatalf("version %d = %+v, want id %s latest=%v", i, vs[i], ids[i], i == 0)
		}
	}
	if !vs[n].latest || vs[n].key != "l" {
		t.Fatalf("last = %+v", vs[n])
	}
	// Resume mid-way.
	vs = versions(t, b, bkt, "", "k", ids[700])
	eq(t, "resumed count", len(vs), n-701+1)
	if vs[0].id != ids[701] || vs[0].latest {
		t.Fatalf("resumed first = %+v", vs[0])
	}
}

func TestLongAndBinaryKeys(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	long := strings.Repeat("k", 1024)
	longUTF8 := strings.Repeat("é", 512) // 1024 bytes
	keys := []string{long, longUTF8, long[:1023] + "\x00", "x\x00", "x", "x\x00\x00", "x\x01", "x\x00\x01"}
	for _, k := range keys {
		put(t, b, bkt, k, k)
		put(t, b, bkt, k, k+"2")
	}
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	if got := keysOf(t, b, bkt, "", ""); !slices.Equal(got, sorted) {
		t.Fatalf("keys = %q\nwant %q", got, sorted)
	}
	vs := versions(t, b, bkt, "", "", "")
	eq(t, "versions", len(vs), 2*len(keys))
	for i, v := range vs {
		if v.key != sorted[i/2] || v.latest != (i%2 == 0) {
			t.Fatalf("version %d = %+v", i, v)
		}
	}
	for _, k := range keys {
		eq(t, "content", read(t, b, bkt, k, ""), k+"2")
	}
	if got := versions(t, b, bkt, "x\x00", "", ""); len(got) != 6 {
		t.Fatalf("prefix x\\x00 = %s", joinVers(got))
	}
}

func TestClose(t *testing.T) {
	b, bkt := newBucket(t, "")
	put(t, b, bkt, "k", "data")
	_, r, err := b.OpenObject(ctx, bkt, "k", "")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := b.NewObjectWriter(ctx, bkt, "k2")
	io.WriteString(w, "x")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("commit after close: %v", err)
	}
	checks := map[string]error{}
	_, checks["ListBuckets"] = b.ListBuckets(ctx)
	_, checks["GetBucket"] = b.GetBucket(ctx, bkt)
	checks["CreateBucket"] = b.CreateBucket(ctx, "x", storage.CreateBucketOptions{})
	_, checks["GetObjectInfo"] = b.GetObjectInfo(ctx, bkt, "k", "")
	_, _, checks["OpenObject"] = b.OpenObject(ctx, bkt, "k", "")
	_, checks["NewObjectWriter"] = b.NewObjectWriter(ctx, bkt, "k")
	_, checks["DeleteObject"] = b.DeleteObject(ctx, bkt, "k", "")
	checks["WalkObjects"] = b.WalkObjects(ctx, bkt, "", "", func(storage.ObjectInfo) error { return nil })
	checks["WalkVersions"] = b.WalkVersions(ctx, bkt, "", "", "", func(storage.ObjectInfo) error { return nil })
	_, checks["ListMultipartUploads"] = b.ListMultipartUploads(ctx, bkt)
	for name, err := range checks {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after close: %v", name, err)
		}
	}
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "data" {
		t.Fatalf("reader after close = %q, %v", data, err)
	}
	r.Close()
}

func TestWalkHugeRecords(t *testing.T) {
	// Records far larger than the per-batch byte budget still all show up,
	// in order, in both walks.
	b, bkt := newBucket(t, storage.VersioningEnabled)
	pcs := make([]map[string]string, 3000)
	for i := range pcs {
		pcs[i] = map[string]string{"CRC32": fmt.Sprintf("%08d==", i), "SHA256": strings.Repeat("s", 44)}
	}
	const n = 40
	for i := range n {
		w, _ := b.NewObjectWriter(ctx, bkt, fmt.Sprintf("k%02d", i))
		w.Write([]byte("x"))
		if _, err := w.Commit(ctx, storage.ObjectMeta{PartChecksums: pcs, PartSizes: make([]int64, len(pcs))}, storage.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	keys := keysOf(t, b, bkt, "", "")
	eq(t, "objects", len(keys), n)
	for i, k := range keys {
		eq(t, "key", k, fmt.Sprintf("k%02d", i))
	}
	eq(t, "versions", len(versions(t, b, bkt, "", "", "")), n)
}

// TestIndeterminateCommitKeepsData: when the metadata commit reports an
// error after the transaction function succeeded, the commit may still have
// reached the disk, so the new data file must not be deleted (it would
// leave the persisted version pointing at nothing).
func TestIndeterminateCommitKeepsData(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningUnversioned)
	put(t, b, bkt, "k", "old")
	boom := errors.New("fdatasync: input/output error")
	b.commitFault = func() error { return boom }
	if _, err := putOpts(t, b, bkt, "k", "new", storage.CommitOptions{}); !errors.Is(err, boom) {
		t.Fatalf("put with failing commit: %v", err)
	}
	b.commitFault = nil
	// Here the commit did persist: the object must be readable.
	eq(t, "after indeterminate commit", read(t, b, bkt, "k", ""), "new")

	up, err := b.CreateMultipartUpload(ctx, bkt, "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	b.commitFault = func() error { return boom }
	w, err := b.NewPartWriter(ctx, bkt, "mp", up.UploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "part"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, storage.PartMeta{ETag: md5hex("part")}); !errors.Is(err, boom) {
		t.Fatalf("part commit: %v", err)
	}
	b.commitFault = nil
	if _, err := b.CompleteMultipartUpload(ctx, bkt, "mp", up.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	eq(t, "multipart", read(t, b, bkt, "mp", ""), "part")

	// A failure of the transaction itself still removes the new file.
	before := len(dataFiles(t, b))
	if _, err := putOpts(t, b, bkt, "k", "x", storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("conditional put: %v", err)
	}
	eq(t, "data files after failed precondition", len(dataFiles(t, b)), before)
}
