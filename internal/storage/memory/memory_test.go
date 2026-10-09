package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

var ctx = context.Background()

func newBucket(t *testing.T, versioning storage.VersioningStatus) (*Backend, string) {
	t.Helper()
	b := New()
	t.Cleanup(func() { _ = b.Close() })
	if err := b.CreateBucket(ctx, "bkt", storage.CreateBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	if versioning != storage.VersioningUnversioned {
		if err := b.SetBucketVersioning(ctx, "bkt", versioning); err != nil {
			t.Fatal(err)
		}
	}
	return b, "bkt"
}

func putOpts(t *testing.T, b *Backend, bucket, key, data string, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	t.Helper()
	w, err := b.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	return w.Commit(ctx, storage.ObjectMeta{ETag: "etag-" + data, ContentType: "text/plain"}, opts)
}

func put(t *testing.T, b *Backend, bucket, key, data string) storage.ObjectInfo {
	t.Helper()
	oi, err := putOpts(t, b, bucket, key, data, storage.CommitOptions{})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return oi
}

func read(t *testing.T, b *Backend, bucket, key, vid string) string {
	t.Helper()
	_, r, err := b.OpenObject(ctx, bucket, key, vid)
	if err != nil {
		t.Fatalf("open %s@%s: %v", key, vid, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type ver struct {
	key, id string
	dm      bool
	latest  bool
}

func versions(t *testing.T, b *Backend, bucket, prefix, km, vm string) []ver {
	t.Helper()
	var out []ver
	err := b.WalkVersions(ctx, bucket, prefix, km, vm, func(o storage.ObjectInfo) error {
		out = append(out, ver{o.Key, o.VersionID, o.DeleteMarker, o.IsLatest})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func keysOf(t *testing.T, b *Backend, bucket, prefix, after string) []string {
	t.Helper()
	var out []string
	err := b.WalkObjects(ctx, bucket, prefix, after, func(o storage.ObjectInfo) error {
		if !o.IsLatest || o.DeleteMarker {
			t.Errorf("WalkObjects yielded non-current %+v", o)
		}
		out = append(out, o.Key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBuckets(t *testing.T) {
	b := New()
	defer b.Close()
	if err := b.CreateBucket(ctx, "", storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrInvalidName) {
		t.Fatalf("empty name: %v", err)
	}
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if err := b.CreateBucket(ctx, n, storage.CreateBucketOptions{Region: "r1", Owner: "u-" + n}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.CreateBucket(ctx, "mid", storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrBucketExists) {
		t.Fatalf("dup: %v", err)
	}
	list, err := b.ListBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, bi := range list {
		names = append(names, bi.Name)
	}
	if !slices.Equal(names, []string{"alpha", "mid", "zeta"}) {
		t.Fatalf("ListBuckets = %v", names)
	}
	bi, err := b.GetBucket(ctx, "mid")
	if err != nil || bi.Region != "r1" || bi.Owner != "u-mid" || bi.CreatedAt.IsZero() || bi.Versioning != storage.VersioningUnversioned {
		t.Fatalf("GetBucket = %+v, %v", bi, err)
	}
	if _, err := b.GetBucket(ctx, "nope"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := b.DeleteBucket(ctx, "nope"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("delete missing: %v", err)
	}

	// Object lock implies versioning.
	if err := b.CreateBucket(ctx, "locked", storage.CreateBucketOptions{ObjectLockEnabled: true}); err != nil {
		t.Fatal(err)
	}
	bi, _ = b.GetBucket(ctx, "locked")
	if !bi.ObjectLockEnabled || bi.Versioning != storage.VersioningEnabled {
		t.Fatalf("locked bucket = %+v", bi)
	}
	if err := b.SetBucketObjectLock(ctx, "mid", true); err != nil {
		t.Fatal(err)
	}
	if bi, _ = b.GetBucket(ctx, "mid"); !bi.ObjectLockEnabled {
		t.Fatal("SetBucketObjectLock did not stick")
	}

	// Versioning transitions.
	if err := b.SetBucketVersioning(ctx, "alpha", "Bogus"); err == nil {
		t.Fatal("invalid status accepted")
	}
	if err := b.SetBucketVersioning(ctx, "alpha", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBucketVersioning(ctx, "alpha", storage.VersioningUnversioned); err == nil {
		t.Fatal("revert to unversioned accepted")
	}
	if err := b.SetBucketVersioning(ctx, "nope", storage.VersioningEnabled); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing: %v", err)
	}

	// Delete then re-create gives a fresh bucket.
	if err := b.PutBucketConfig(ctx, "zeta", storage.ConfigCORS, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteBucket(ctx, "zeta"); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateBucket(ctx, "zeta", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetBucketConfig(ctx, "zeta", storage.ConfigCORS); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("config survived bucket deletion: %v", err)
	}
}

func TestDeleteBucketNotEmpty(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, b *Backend)
	}{
		{"object", func(t *testing.T, b *Backend) { put(t, b, "bkt", "k", "v") }},
		{"delete marker only", func(t *testing.T, b *Backend) {
			_ = b.SetBucketVersioning(ctx, "bkt", storage.VersioningEnabled)
			if _, err := b.DeleteObject(ctx, "bkt", "k", ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"noncurrent version", func(t *testing.T, b *Backend) {
			_ = b.SetBucketVersioning(ctx, "bkt", storage.VersioningEnabled)
			put(t, b, "bkt", "k", "v")
			_, _ = b.DeleteObject(ctx, "bkt", "k", "")
		}},
		{"upload", func(t *testing.T, b *Backend) {
			if _, err := b.CreateMultipartUpload(ctx, "bkt", "k", storage.ObjectMeta{}, ""); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newBucket(t, storage.VersioningUnversioned)
			tc.setup(t, b)
			if err := b.DeleteBucket(ctx, "bkt"); !errors.Is(err, storage.ErrBucketNotEmpty) {
				t.Fatalf("DeleteBucket = %v, want ErrBucketNotEmpty", err)
			}
		})
	}
}

func TestBucketConfig(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	if _, err := b.GetBucketConfig(ctx, bk, storage.ConfigPolicy); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("unset: %v", err)
	}
	in := []byte(`{"a":1}`)
	if err := b.PutBucketConfig(ctx, bk, storage.ConfigPolicy, in); err != nil {
		t.Fatal(err)
	}
	in[0] = 'X' // must not alias
	got, err := b.GetBucketConfig(ctx, bk, storage.ConfigPolicy)
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("Get = %q, %v", got, err)
	}
	got[0] = 'Y'
	if again, _ := b.GetBucketConfig(ctx, bk, storage.ConfigPolicy); string(again) != `{"a":1}` {
		t.Fatalf("returned slice aliases storage: %q", again)
	}
	if err := b.PutBucketConfig(ctx, bk, storage.ConfigTagging, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetBucketConfig(ctx, bk, storage.ConfigTagging); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty doc = %v, %v", got, err)
	}
	for i := 0; i < 2; i++ {
		if err := b.DeleteBucketConfig(ctx, bk, storage.ConfigPolicy); err != nil {
			t.Fatalf("delete #%d: %v", i, err)
		}
	}
	if _, err := b.GetBucketConfig(ctx, bk, storage.ConfigPolicy); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	for name, err := range map[string]error{
		"get": func() error { _, err := b.GetBucketConfig(ctx, "nope", "x"); return err }(),
		"put": b.PutBucketConfig(ctx, "nope", "x", nil),
		"del": b.DeleteBucketConfig(ctx, "nope", "x"),
	} {
		if !errors.Is(err, storage.ErrBucketNotFound) {
			t.Errorf("%s on missing bucket: %v", name, err)
		}
	}
}

func TestUnversionedObjects(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	if _, err := b.NewObjectWriter(ctx, "nope", "k"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
	if _, err := b.NewObjectWriter(ctx, bk, ""); !errors.Is(err, storage.ErrInvalidName) {
		t.Fatalf("empty key: %v", err)
	}
	if _, err := b.NewObjectWriter(ctx, bk, "bad\xffkey"); !errors.Is(err, storage.ErrInvalidName) {
		t.Fatalf("invalid utf-8 key: %v", err)
	}
	if _, err := b.GetObjectInfo(ctx, bk, "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := b.GetObjectInfo(ctx, bk, "k", "null"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("missing key with version: %v", err)
	}

	oi := put(t, b, bk, "k", "hello")
	if oi.VersionID != storage.NullVersionID || !oi.IsLatest || oi.Size != 5 || oi.Bucket != bk || oi.Key != "k" ||
		oi.ETag != "etag-hello" || oi.ContentType != "text/plain" || oi.LastModified.IsZero() || oi.LastModified.Location() != time.UTC {
		t.Fatalf("commit info = %+v", oi)
	}
	_, r1, err := b.OpenObject(ctx, bk, "k", "")
	if err != nil {
		t.Fatal(err)
	}
	put(t, b, bk, "k", "world!")
	if got := read(t, b, bk, "k", ""); got != "world!" {
		t.Fatalf("after overwrite = %q", got)
	}
	if got := read(t, b, bk, "k", storage.NullVersionID); got != "world!" {
		t.Fatalf("null version = %q", got)
	}
	if v := versions(t, b, bk, "", "", ""); len(v) != 1 {
		t.Fatalf("unversioned overwrite kept versions: %+v", v)
	}
	res, err := b.DeleteObject(ctx, bk, "k", "")
	if err != nil || res != (storage.DeleteResult{}) {
		t.Fatalf("delete = %+v, %v", res, err)
	}
	// The reader opened before the overwrite still sees the old bytes.
	if data, err := io.ReadAll(r1); err != nil || string(data) != "hello" {
		t.Fatalf("snapshot reader = %q, %v", data, err)
	}
	if _, err := r1.Seek(1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if data, _ := io.ReadAll(r1); string(data) != "ello" {
		t.Fatalf("after seek = %q", data)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after close succeeded")
	}
	if err := r1.Close(); err != nil {
		t.Fatal("second close:", err)
	}
	if _, err := b.GetObjectInfo(ctx, bk, "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	// Deleting a missing key is not an error.
	if res, err := b.DeleteObject(ctx, bk, "k", ""); err != nil || res != (storage.DeleteResult{}) {
		t.Fatalf("delete missing = %+v, %v", res, err)
	}
	if _, err := b.DeleteObject(ctx, bk, "k", "null"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("delete missing version: %v", err)
	}
	// Explicit null-version delete.
	put(t, b, bk, "k2", "x")
	if res, err := b.DeleteObject(ctx, bk, "k2", "null"); err != nil || res.VersionID != "null" || res.DeleteMarker {
		t.Fatalf("delete null = %+v, %v", res, err)
	}
	// Empty object.
	oi = put(t, b, bk, "empty", "")
	if oi.Size != 0 || read(t, b, bk, "empty", "") != "" {
		t.Fatalf("empty object = %+v", oi)
	}
}

func TestEnabledVersioning(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningEnabled)
	v1 := put(t, b, bk, "k", "one")
	v2 := put(t, b, bk, "k", "two")
	if v1.VersionID == v2.VersionID || v1.VersionID == "null" || len(v1.VersionID) > 64 {
		t.Fatalf("version ids %q %q", v1.VersionID, v2.VersionID)
	}
	if read(t, b, bk, "k", v1.VersionID) != "one" || read(t, b, bk, "k", "") != "two" {
		t.Fatal("wrong content per version")
	}
	if _, err := b.GetObjectInfo(ctx, bk, "k", "null"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("null version in enabled bucket: %v", err)
	}
	if _, err := b.GetObjectInfo(ctx, bk, "k", "0000000000000000deadbeef"); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("bogus version: %v", err)
	}
	old, err := b.GetObjectInfo(ctx, bk, "k", v1.VersionID)
	if err != nil || old.IsLatest {
		t.Fatalf("old version info = %+v, %v", old, err)
	}

	del, err := b.DeleteObject(ctx, bk, "k", "")
	if err != nil || !del.DeleteMarker || del.VersionID == "" || del.VersionID == "null" {
		t.Fatalf("delete = %+v, %v", del, err)
	}
	latest, err := b.GetObjectInfo(ctx, bk, "k", "")
	if err != nil || !latest.DeleteMarker || latest.VersionID != del.VersionID || !latest.IsLatest || latest.Size != 0 {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	info, r, err := b.OpenObject(ctx, bk, "k", "")
	if err != nil || r != nil || !info.DeleteMarker {
		t.Fatalf("open delete marker = %+v, %v, %v", info, r, err)
	}
	if got := keysOf(t, b, bk, "", ""); len(got) != 0 {
		t.Fatalf("WalkObjects shows deleted key: %v", got)
	}
	want := []ver{{"k", del.VersionID, true, true}, {"k", v2.VersionID, false, false}, {"k", v1.VersionID, false, false}}
	if got := versions(t, b, bk, "", "", ""); !slices.Equal(got, want) {
		t.Fatalf("versions = %+v, want %+v", got, want)
	}
	// A second delete adds another marker.
	del2, _ := b.DeleteObject(ctx, bk, "k", "")
	if del2.VersionID == del.VersionID {
		t.Fatal("delete marker id reused")
	}
	// Removing the markers restores v2 as latest.
	for _, id := range []string{del2.VersionID, del.VersionID} {
		res, err := b.DeleteObject(ctx, bk, "k", id)
		if err != nil || !res.DeleteMarker || res.VersionID != id {
			t.Fatalf("remove marker = %+v, %v", res, err)
		}
	}
	if latest, _ := b.GetObjectInfo(ctx, bk, "k", ""); latest.VersionID != v2.VersionID || !latest.IsLatest {
		t.Fatalf("latest after removing markers = %+v", latest)
	}
	// Delete the latest real version: v1 becomes latest.
	if res, err := b.DeleteObject(ctx, bk, "k", v2.VersionID); err != nil || res.DeleteMarker {
		t.Fatalf("delete v2 = %+v, %v", res, err)
	}
	if read(t, b, bk, "k", "") != "one" {
		t.Fatal("v1 not promoted")
	}
	if _, err := b.DeleteObject(ctx, bk, "k", v2.VersionID); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("double delete: %v", err)
	}
	if _, err := b.DeleteObject(ctx, bk, "k", v1.VersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetObjectInfo(ctx, bk, "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("key should be gone: %v", err)
	}
	// Delete marker on a key that never existed.
	if res, err := b.DeleteObject(ctx, bk, "ghost", ""); err != nil || !res.DeleteMarker {
		t.Fatalf("delete ghost = %+v, %v", res, err)
	}
	if err := b.DeleteBucket(ctx, bk); !errors.Is(err, storage.ErrBucketNotEmpty) {
		t.Fatalf("bucket with only a delete marker: %v", err)
	}
}

func TestSuspendedVersioning(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	n0 := put(t, b, bk, "k", "null-0") // null version from the unversioned era
	_ = b.SetBucketVersioning(ctx, bk, storage.VersioningEnabled)
	v1 := put(t, b, bk, "k", "v1")
	_ = b.SetBucketVersioning(ctx, bk, storage.VersioningSuspended)

	// A suspended write replaces the old null version and becomes latest.
	n1 := put(t, b, bk, "k", "null-1")
	if n1.VersionID != "null" || n0.VersionID != "null" {
		t.Fatalf("null ids: %q %q", n0.VersionID, n1.VersionID)
	}
	want := []ver{{"k", "null", false, true}, {"k", v1.VersionID, false, false}}
	if got := versions(t, b, bk, "", "", ""); !slices.Equal(got, want) {
		t.Fatalf("versions = %+v, want %+v", got, want)
	}
	if read(t, b, bk, "k", "null") != "null-1" {
		t.Fatal("null version not replaced")
	}

	// Suspended delete: null delete marker replacing the null version.
	res, err := b.DeleteObject(ctx, bk, "k", "")
	if err != nil || res.VersionID != "null" || !res.DeleteMarker {
		t.Fatalf("delete = %+v, %v", res, err)
	}
	want = []ver{{"k", "null", true, true}, {"k", v1.VersionID, false, false}}
	if got := versions(t, b, bk, "", "", ""); !slices.Equal(got, want) {
		t.Fatalf("versions = %+v, want %+v", got, want)
	}
	// Writing again replaces the null delete marker.
	put(t, b, bk, "k", "null-2")
	want = []ver{{"k", "null", false, true}, {"k", v1.VersionID, false, false}}
	if got := versions(t, b, bk, "", "", ""); !slices.Equal(got, want) {
		t.Fatalf("versions = %+v, want %+v", got, want)
	}
	// Re-enable: new versions stack on top, null stays.
	_ = b.SetBucketVersioning(ctx, bk, storage.VersioningEnabled)
	v3 := put(t, b, bk, "k", "v3")
	want = []ver{{"k", v3.VersionID, false, true}, {"k", "null", false, false}, {"k", v1.VersionID, false, false}}
	if got := versions(t, b, bk, "", "", ""); !slices.Equal(got, want) {
		t.Fatalf("versions = %+v, want %+v", got, want)
	}
	// Suspend again; a null write moves the null version to the top.
	_ = b.SetBucketVersioning(ctx, bk, storage.VersioningSuspended)
	put(t, b, bk, "k", "null-3")
	want = []ver{{"k", "null", false, true}, {"k", v3.VersionID, false, false}, {"k", v1.VersionID, false, false}}
	if got := versions(t, b, bk, "", "", ""); !slices.Equal(got, want) {
		t.Fatalf("versions = %+v, want %+v", got, want)
	}
}

func TestConditionalCommit(t *testing.T) {
	type step struct {
		name    string
		opts    storage.CommitOptions
		wantErr bool
		data    string // content written ("a" when empty)
	}
	cases := []struct {
		name       string
		versioning storage.VersioningStatus
		setup      func(t *testing.T, b *Backend)
		steps      []step
	}{
		{"missing key", storage.VersioningUnversioned, nil, []step{
			{"if-match on missing", storage.CommitOptions{IfMatch: "etag-a"}, true, ""},
			{"if-match * on missing", storage.CommitOptions{IfMatch: "*"}, true, ""},
			{"if-none-match on missing", storage.CommitOptions{IfNoneMatch: true}, false, ""},
		}},
		{"existing key", storage.VersioningUnversioned, func(t *testing.T, b *Backend) { put(t, b, "bkt", "k", "a") }, []step{
			{"if-none-match on existing", storage.CommitOptions{IfNoneMatch: true}, true, ""},
			{"if-match wrong", storage.CommitOptions{IfMatch: "etag-zzz"}, true, ""},
			{name: "if-match quoted", opts: storage.CommitOptions{IfMatch: `"etag-a"`}, data: "b"},
			{"if-match old etag after overwrite", storage.CommitOptions{IfMatch: "etag-a"}, true, ""},
			{"if-match star", storage.CommitOptions{IfMatch: "*"}, false, ""},
		}},
		{"delete marker latest", storage.VersioningEnabled, func(t *testing.T, b *Backend) {
			put(t, b, "bkt", "k", "a")
			_, _ = b.DeleteObject(ctx, "bkt", "k", "")
		}, []step{
			{"if-match against marker", storage.CommitOptions{IfMatch: "etag-a"}, true, ""},
			{"if-none-match against marker", storage.CommitOptions{IfNoneMatch: true}, false, ""},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newBucket(t, tc.versioning)
			if tc.setup != nil {
				tc.setup(t, b)
			}
			for _, s := range tc.steps {
				before := versions(t, b, "bkt", "", "", "")
				data := s.data
				if data == "" {
					data = "a"
				}
				_, err := putOpts(t, b, "bkt", "k", data, s.opts)
				if s.wantErr {
					if !errors.Is(err, storage.ErrPreconditionFailed) {
						t.Fatalf("%s: err = %v, want ErrPreconditionFailed", s.name, err)
					}
					if after := versions(t, b, "bkt", "", "", ""); !slices.Equal(before, after) {
						t.Fatalf("%s: failed commit changed state", s.name)
					}
				} else if err != nil {
					t.Fatalf("%s: %v", s.name, err)
				}
			}
		})
	}
}

func TestConditionalCommitRace(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningEnabled)
	const n = 32
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, err := b.NewObjectWriter(ctx, bk, "race")
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, "writer-%d", i)
			<-start
			_, err = w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{IfNoneMatch: true})
			switch {
			case err == nil:
				wins.Add(1)
			case !errors.Is(err, storage.ErrPreconditionFailed):
				t.Error(err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d writers won If-None-Match race, want 1", wins.Load())
	}
	if v := versions(t, b, bk, "", "", ""); len(v) != 1 {
		t.Fatalf("versions = %+v", v)
	}
}

func TestWriterLifecycle(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	w, _ := b.NewObjectWriter(ctx, bk, "k")
	buf := []byte("abc")
	_, _ = w.Write(buf)
	buf[0] = 'X' // the writer must have copied
	if _, err := b.GetObjectInfo(ctx, bk, "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatal("object visible before commit")
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if read(t, b, bk, "k", "") != "abc" {
		t.Fatal("writer aliased caller buffer")
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("write after commit succeeded")
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); err == nil {
		t.Fatal("double commit succeeded")
	}
	if err := w.Abort(); err != nil {
		t.Fatal("abort after commit:", err)
	}

	w, _ = b.NewObjectWriter(ctx, bk, "aborted")
	_, _ = w.Write([]byte("zzz"))
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); err == nil {
		t.Fatal("commit after abort succeeded")
	}
	if _, err := b.GetObjectInfo(ctx, bk, "aborted", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatal("aborted object visible")
	}

	// Commit after the bucket is deleted (and even recreated) fails.
	w, _ = b.NewObjectWriter(ctx, bk, "late")
	_, _ = w.Write([]byte("x"))
	if err := b.DeleteBucket(ctx, bk); err == nil {
		// bucket has "k"; remove it first
		t.Fatal("expected not empty")
	}
	_, _ = b.DeleteObject(ctx, bk, "k", "")
	if err := b.DeleteBucket(ctx, bk); err != nil {
		t.Fatal(err)
	}
	_ = b.CreateBucket(ctx, bk, storage.CreateBucketOptions{})
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("commit into deleted bucket: %v", err)
	}
	if _, err := b.GetObjectInfo(ctx, bk, "late", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatal("object landed in recreated bucket")
	}

	// Canceled context.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	w, _ = b.NewObjectWriter(ctx, bk, "c")
	if _, err := w.Commit(cctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit with canceled ctx: %v", err)
	}
	if _, err := b.GetObjectInfo(cctx, bk, "c", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("get with canceled ctx: %v", err)
	}
}

func fullMeta() storage.ObjectMeta {
	return storage.ObjectMeta{
		ETag:          "e",
		UserMeta:      map[string]string{"a": "1"},
		Tags:          map[string]string{"t": "v"},
		Checksums:     map[string]string{"CRC32": "AAAAAA=="},
		PartSizes:     []int64{5, 6},
		PartChecksums: []map[string]string{{"CRC32": "x"}, {"CRC32": "y"}},
		RetainUntil:   time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		RetentionMode: "GOVERNANCE",
	}
}

func mutateMeta(m *storage.ObjectMeta) {
	m.UserMeta["a"] = "mutated"
	m.Tags["t"] = "mutated"
	m.Checksums["CRC32"] = "mutated"
	m.PartSizes[0] = 999
	m.PartChecksums[0]["CRC32"] = "mutated"
}

func TestNoAliasing(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	meta := fullMeta()
	w, _ := b.NewObjectWriter(ctx, bk, "k")
	oi, err := w.Commit(ctx, meta, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mutateMeta(&meta)
	mutateMeta(&oi.ObjectMeta)
	got, _ := b.GetObjectInfo(ctx, bk, "k", "")
	want := fullMeta()
	if got.UserMeta["a"] != "1" || got.Tags["t"] != "v" || got.Checksums["CRC32"] != want.Checksums["CRC32"] ||
		got.PartSizes[0] != 5 || got.PartChecksums[0]["CRC32"] != "x" {
		t.Fatalf("stored meta was aliased: %+v", got.ObjectMeta)
	}
	mutateMeta(&got.ObjectMeta)
	_ = b.WalkObjects(ctx, bk, "", "", func(o storage.ObjectInfo) error {
		mutateMeta(&o.ObjectMeta)
		return nil
	})
	_ = b.WalkVersions(ctx, bk, "", "", "", func(o storage.ObjectInfo) error {
		mutateMeta(&o.ObjectMeta)
		return nil
	})
	if again, _ := b.GetObjectInfo(ctx, bk, "k", ""); again.UserMeta["a"] != "1" || again.PartChecksums[0]["CRC32"] != "x" {
		t.Fatalf("returned meta aliased storage: %+v", again.ObjectMeta)
	}

	// Multipart: meta and part meta.
	umeta := fullMeta()
	up, _ := b.CreateMultipartUpload(ctx, bk, "mp", umeta, "alice")
	mutateMeta(&umeta)
	mutateMeta(&up.Meta)
	pw, _ := b.NewPartWriter(ctx, bk, "mp", up.UploadID, 1)
	pm := storage.PartMeta{ETag: "p1", Checksums: map[string]string{"CRC32": "c"}}
	pi, _ := pw.Commit(ctx, pm)
	pm.Checksums["CRC32"] = "mutated"
	pi.Checksums["CRC32"] = "mutated"
	parts, _ := b.ListParts(ctx, bk, "mp", up.UploadID)
	if parts[0].Checksums["CRC32"] != "c" {
		t.Fatal("part meta aliased")
	}
	parts[0].Checksums["CRC32"] = "mutated"
	ups, _ := b.ListMultipartUploads(ctx, bk)
	if ups[0].Meta.UserMeta["a"] != "1" {
		t.Fatal("upload meta aliased")
	}
	mutateMeta(&ups[0].Meta)
	if u, _ := b.GetMultipartUpload(ctx, bk, "mp", up.UploadID); u.Meta.UserMeta["a"] != "1" || u.Meta.PartChecksums[0]["CRC32"] != "x" {
		t.Fatal("upload meta aliased via list")
	}
	if parts, _ := b.ListParts(ctx, bk, "mp", up.UploadID); parts[0].Checksums["CRC32"] != "c" {
		t.Fatal("part meta aliased via list")
	}
}

func TestUpdateObjectMeta(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningEnabled)
	w, _ := b.NewObjectWriter(ctx, bk, "k")
	_, _ = w.Write([]byte("data"))
	v1, _ := w.Commit(ctx, fullMeta(), storage.CommitOptions{})
	v2 := put(t, b, bk, "k", "two")

	var kept *storage.ObjectMeta
	oi, err := b.UpdateObjectMeta(ctx, bk, "k", v1.VersionID, func(m *storage.ObjectMeta) error {
		m.Tags = map[string]string{"new": "tag"}
		m.LegalHold = true
		kept = m
		return nil
	})
	if err != nil || oi.Tags["new"] != "tag" || !oi.LegalHold || oi.IsLatest || oi.VersionID != v1.VersionID ||
		!oi.LastModified.Equal(v1.LastModified) || oi.Size != 4 {
		t.Fatalf("update = %+v, %v", oi, err)
	}
	kept.Tags["new"] = "mutated-after"
	if got, _ := b.GetObjectInfo(ctx, bk, "k", v1.VersionID); got.Tags["new"] != "tag" || got.UserMeta["a"] != "1" {
		t.Fatalf("stored = %+v", got.ObjectMeta)
	}
	if read(t, b, bk, "k", v1.VersionID) != "data" {
		t.Fatal("data changed")
	}
	// Latest by default.
	oi, err = b.UpdateObjectMeta(ctx, bk, "k", "", func(m *storage.ObjectMeta) error { m.ACL = "public-read"; return nil })
	if err != nil || oi.VersionID != v2.VersionID || oi.ACL != "public-read" || !oi.IsLatest {
		t.Fatalf("update latest = %+v, %v", oi, err)
	}
	// fn error leaves state untouched.
	boom := errors.New("boom")
	if _, err := b.UpdateObjectMeta(ctx, bk, "k", "", func(m *storage.ObjectMeta) error { m.ACL = "x"; return boom }); !errors.Is(err, boom) {
		t.Fatalf("fn error = %v", err)
	}
	if got, _ := b.GetObjectInfo(ctx, bk, "k", ""); got.ACL != "public-read" {
		t.Fatal("failed update applied")
	}
	// Errors.
	noop := func(*storage.ObjectMeta) error { return nil }
	if _, err := b.UpdateObjectMeta(ctx, bk, "missing", "", noop); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := b.UpdateObjectMeta(ctx, bk, "k", "nope", noop); !errors.Is(err, storage.ErrVersionNotFound) {
		t.Fatalf("bad version: %v", err)
	}
	dm, _ := b.DeleteObject(ctx, bk, "k", "")
	if _, err := b.UpdateObjectMeta(ctx, bk, "k", "", noop); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("latest delete marker: %v", err)
	}
	if _, err := b.UpdateObjectMeta(ctx, bk, "k", dm.VersionID, noop); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("explicit delete marker: %v", err)
	}
	if _, err := b.UpdateObjectMeta(ctx, bk, "k", "", nil); err == nil {
		t.Fatal("nil fn accepted")
	}
}

func TestWalkObjects(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningEnabled)
	all := []string{"a", "a/b", "a/c", "ab", "b", "b/x/y", "c", "日本/語", "日本語"}
	for _, k := range all {
		put(t, b, bk, k, k)
	}
	_, _ = b.DeleteObject(ctx, bk, "ab", "") // hidden by a delete marker
	visible := slices.DeleteFunc(slices.Clone(all), func(s string) bool { return s == "ab" })

	cases := []struct {
		prefix, after string
		want          []string
	}{
		{"", "", visible},
		{"a", "", []string{"a", "a/b", "a/c"}},
		{"a/", "", []string{"a/b", "a/c"}},
		{"a", "a", []string{"a/b", "a/c"}},
		{"a", "a/b", []string{"a/c"}},
		{"", "a/c", []string{"b", "b/x/y", "c", "日本/語", "日本語"}},
		{"b", "0", []string{"b", "b/x/y"}},  // after < prefix
		{"b", "zzz", nil},                   // after beyond prefix range
		{"", "日本", []string{"日本/語", "日本語"}}, // unicode
		{"zz", "", nil},
		{"", "\xff", nil},
	}
	for _, tc := range cases {
		if got := keysOf(t, b, bk, tc.prefix, tc.after); !slices.Equal(got, tc.want) {
			t.Errorf("WalkObjects(%q, %q) = %q, want %q", tc.prefix, tc.after, got, tc.want)
		}
	}

	// Early stop and error propagation.
	var n int
	err := b.WalkObjects(ctx, bk, "", "", func(storage.ObjectInfo) error {
		n++
		if n == 2 {
			return fmt.Errorf("wrapped: %w", storage.ErrStopWalk)
		}
		return nil
	})
	if err != nil || n != 2 {
		t.Fatalf("stop: n=%d err=%v", n, err)
	}
	boom := errors.New("boom")
	if err := b.WalkObjects(ctx, bk, "", "", func(storage.ObjectInfo) error { return boom }); err != boom {
		t.Fatalf("error propagation: %v", err)
	}
	if err := b.WalkObjects(ctx, "nope", "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
}

func TestWalkLargeAndMutating(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningEnabled)
	const n = 5000
	var want []string
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("k%05d", i)
		want = append(want, k)
		put(t, b, bk, k, "x")
		if i%3 == 0 {
			put(t, b, bk, k, "y") // second version
		}
	}
	// Many delete markers in a row must not stall the walk.
	for i := 1000; i < 3000; i++ {
		_, _ = b.DeleteObject(ctx, bk, fmt.Sprintf("k%05d", i), "")
	}
	want = append(want[:1000:1000], want[3000:]...)
	if got := keysOf(t, b, bk, "", ""); !slices.Equal(got, want) {
		t.Fatalf("large walk mismatch: %d keys, want %d", len(got), len(want))
	}

	// Versions: count and per-key ordering newest first.
	var count int
	lastKey, lastStamp := "", uint64(0)
	err := b.WalkVersions(ctx, bk, "", "", "", func(o storage.ObjectInfo) error {
		count++
		s, ok := stampFromVersionID(o.VersionID)
		if !ok {
			t.Fatalf("unexpected version id %q", o.VersionID)
		}
		if o.Key == lastKey && s >= lastStamp {
			t.Fatalf("versions of %s not newest first", o.Key)
		}
		if o.Key < lastKey {
			t.Fatalf("keys out of order: %s after %s", o.Key, lastKey)
		}
		lastKey, lastStamp = o.Key, s
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if wantCount := n + (n+2)/3 + 2000; count != wantCount {
		t.Fatalf("versions = %d, want %d", count, wantCount)
	}

	// Callbacks may mutate the backend (no deadlock); deleting the keys being
	// walked must not disturb the iteration.
	var seen int
	err = b.WalkObjects(ctx, bk, "", "", func(o storage.ObjectInfo) error {
		seen++
		_, err := b.DeleteObject(ctx, bk, o.Key, "")
		return err
	})
	if err != nil || seen != len(want) {
		t.Fatalf("mutating walk: seen=%d err=%v", seen, err)
	}
	if got := keysOf(t, b, bk, "", ""); len(got) != 0 {
		t.Fatalf("after mutating walk %d keys remain visible", len(got))
	}

	cctx, cancel := context.WithCancel(ctx)
	err = b.WalkVersions(cctx, bk, "", "", "", func(storage.ObjectInfo) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled walk: %v", err)
	}
}

func TestWalkVersionsMarkers(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	put(t, b, bk, "a", "a-null")
	_ = b.SetBucketVersioning(ctx, bk, storage.VersioningEnabled)
	a1 := put(t, b, bk, "a", "a1")
	a2 := put(t, b, bk, "a", "a2")
	b1 := put(t, b, bk, "b", "b1")
	c1 := put(t, b, bk, "c", "c1")
	cdm, _ := b.DeleteObject(ctx, bk, "c", "")

	A2 := ver{"a", a2.VersionID, false, true}
	A1 := ver{"a", a1.VersionID, false, false}
	AN := ver{"a", "null", false, false}
	B1 := ver{"b", b1.VersionID, false, true}
	CD := ver{"c", cdm.VersionID, true, true}
	C1 := ver{"c", c1.VersionID, false, false}

	cases := []struct {
		name, prefix, km, vm string
		want                 []ver
	}{
		{"all", "", "", "", []ver{A2, A1, AN, B1, CD, C1}},
		{"vid ignored without key marker", "", "", a1.VersionID, []ver{A2, A1, AN, B1, CD, C1}},
		{"after key", "", "a", "", []ver{B1, CD, C1}},
		{"after missing key", "", "aa", "", []ver{B1, CD, C1}},
		{"after version", "", "a", a2.VersionID, []ver{A1, AN, B1, CD, C1}},
		{"after middle version", "", "a", a1.VersionID, []ver{AN, B1, CD, C1}},
		{"after null version", "", "a", "null", []ver{B1, CD, C1}},
		{"after delete marker", "", "c", cdm.VersionID, []ver{C1}},
		{"after last version of key", "", "c", c1.VersionID, nil},
		{"unknown non-generated vid", "", "a", "garbage", []ver{B1, CD, C1}},
		{"prefix", "c", "", "", []ver{CD, C1}},
		{"prefix with marker before it", "c", "a", a1.VersionID, []ver{CD, C1}},
		{"prefix with marker after it", "a", "b", "", nil},
		{"marker key outside prefix with vid", "b", "c", cdm.VersionID, nil},
	}
	for _, tc := range cases {
		if got := versions(t, b, bk, tc.prefix, tc.km, tc.vm); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// A version-ID marker whose version was deleted between pages resumes
	// at the position it occupied.
	if _, err := b.DeleteObject(ctx, bk, "a", a2.VersionID); err != nil {
		t.Fatal(err)
	}
	A1.latest = true
	if got := versions(t, b, bk, "", "a", a2.VersionID); !slices.Equal(got, []ver{A1, AN, B1, CD, C1}) {
		t.Errorf("deleted marker version: got %+v", got)
	}
	// Marker key deleted entirely.
	for _, id := range []string{a1.VersionID, "null"} {
		_, _ = b.DeleteObject(ctx, bk, "a", id)
	}
	if got := versions(t, b, bk, "", "a", a1.VersionID); !slices.Equal(got, []ver{B1, CD, C1}) {
		t.Errorf("deleted marker key: got %+v", got)
	}
}

func TestWalkVersionsManyVersionsAcrossBatches(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningEnabled)
	var ids []string
	for i := 0; i < 3000; i++ {
		ids = append(ids, put(t, b, bk, "k", "x").VersionID)
	}
	put(t, b, bk, "l", "x")
	slices.Reverse(ids)
	got := versions(t, b, bk, "", "", "")
	if len(got) != 3001 || got[3000].key != "l" {
		t.Fatalf("got %d versions", len(got))
	}
	for i, id := range ids {
		if got[i].id != id || got[i].latest != (i == 0) {
			t.Fatalf("version %d = %+v, want id %s", i, got[i], id)
		}
	}
	// Resume from the middle.
	got = versions(t, b, bk, "", "k", ids[1499])
	if len(got) != 1501 || got[0].id != ids[1500] {
		t.Fatalf("resume: %d versions, first %+v", len(got), got[0])
	}
}

func TestVersionIDs(t *testing.T) {
	b := New()
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return fixed } // clock not advancing
	seen := map[string]bool{}
	var prev uint64
	for i := 0; i < 1000; i++ {
		s, _ := b.nextStamp()
		if s <= prev {
			t.Fatalf("stamp not increasing: %d <= %d", s, prev)
		}
		prev = s
		id := newVersionID(s)
		if len(id) != versionIDLength || id == "null" || seen[id] {
			t.Fatalf("bad id %q", id)
		}
		seen[id] = true
		if got, ok := stampFromVersionID(id); !ok || got != s {
			t.Fatalf("stampFromVersionID(%q) = %d, %v", id, got, ok)
		}
	}
	// Clock going backwards keeps stamps increasing.
	b.now = func() time.Time { return fixed.Add(-time.Hour) }
	if s, _ := b.nextStamp(); s <= prev {
		t.Fatal("stamp went backwards")
	}
	// IDs sort in creation order.
	a, c := newVersionID(5), newVersionID(1<<40)
	if a >= c {
		t.Fatalf("ids not time ordered: %s %s", a, c)
	}
	for _, bad := range []string{"", "null", "XYZ", strings.Repeat("g", 24), strings.Repeat("A", 24), strings.Repeat("0", 23)} {
		if _, ok := stampFromVersionID(bad); ok {
			t.Errorf("stampFromVersionID(%q) accepted", bad)
		}
	}
}

func writePart(t *testing.T, b *Backend, key, uploadID string, n int, data string) storage.PartInfo {
	t.Helper()
	pw, err := b.NewPartWriter(ctx, "bkt", key, uploadID, n)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(pw, data)
	pi, err := pw.Commit(ctx, storage.PartMeta{ETag: "p-" + data})
	if err != nil {
		t.Fatal(err)
	}
	return pi
}

func TestMultipart(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningEnabled)
	if _, err := b.CreateMultipartUpload(ctx, "nope", "k", storage.ObjectMeta{}, ""); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
	up, err := b.CreateMultipartUpload(ctx, bk, "k", storage.ObjectMeta{ContentType: "video/mp4"}, "alice")
	if err != nil || up.UploadID == "" || up.Key != "k" || up.Bucket != bk || up.Initiator != "alice" || up.Initiated.IsZero() {
		t.Fatalf("create = %+v, %v", up, err)
	}
	if got, err := b.GetMultipartUpload(ctx, bk, "k", up.UploadID); err != nil || got.Meta.ContentType != "video/mp4" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := b.GetMultipartUpload(ctx, bk, "other", up.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("key mismatch: %v", err)
	}
	if _, err := b.NewPartWriter(ctx, bk, "k", "bogus", 1); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("bogus upload: %v", err)
	}
	for _, pn := range []int{0, -1, MaxPartNumber + 1} {
		if _, err := b.NewPartWriter(ctx, bk, "k", up.UploadID, pn); err == nil {
			t.Errorf("part number %d accepted", pn)
		}
	}

	writePart(t, b, "k", up.UploadID, 3, "CCC")
	writePart(t, b, "k", up.UploadID, 1, "old")
	p1 := writePart(t, b, "k", up.UploadID, 1, "AAA") // replaces
	writePart(t, b, "k", up.UploadID, 2, "BB")
	writePart(t, b, "k", up.UploadID, 7, "unused")
	if p1.Size != 3 || p1.PartNumber != 1 || p1.ETag != "p-AAA" || p1.LastModified.IsZero() {
		t.Fatalf("part info = %+v", p1)
	}
	parts, err := b.ListParts(ctx, bk, "k", up.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, p := range parts {
		nums = append(nums, p.PartNumber)
	}
	if !slices.Equal(nums, []int{1, 2, 3, 7}) || parts[0].ETag != "p-AAA" {
		t.Fatalf("parts = %+v", parts)
	}
	if _, err := b.ListParts(ctx, bk, "x", up.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("list parts key mismatch: %v", err)
	}

	meta := storage.ObjectMeta{ETag: "abc-3", PartSizes: []int64{3, 2, 3}}
	if _, err := b.CompleteMultipartUpload(ctx, bk, "k", up.UploadID, []int{1, 4}, meta, storage.CommitOptions{}); !errors.Is(err, storage.ErrPartNotFound) {
		t.Fatalf("missing part: %v", err)
	}
	put(t, b, bk, "k", "existing")
	if _, err := b.CompleteMultipartUpload(ctx, bk, "k", up.UploadID, []int{1, 2, 3}, meta, storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("precondition: %v", err)
	}
	// Upload survives a failed precondition.
	if _, err := b.GetMultipartUpload(ctx, bk, "k", up.UploadID); err != nil {
		t.Fatalf("upload removed by failed complete: %v", err)
	}
	oi, err := b.CompleteMultipartUpload(ctx, bk, "k", up.UploadID, []int{1, 2, 3}, meta, storage.CommitOptions{IfMatch: "etag-existing"})
	if err != nil || oi.Size != 8 || oi.ETag != "abc-3" || !slices.Equal(oi.PartSizes, []int64{3, 2, 3}) || oi.VersionID == "null" || !oi.IsLatest {
		t.Fatalf("complete = %+v, %v", oi, err)
	}
	if got := read(t, b, bk, "k", ""); got != "AAABBCCC" {
		t.Fatalf("content = %q", got)
	}
	if _, err := b.GetMultipartUpload(ctx, bk, "k", up.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("upload not removed: %v", err)
	}
	if _, err := b.CompleteMultipartUpload(ctx, bk, "k", up.UploadID, []int{1}, meta, storage.CommitOptions{}); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("complete twice: %v", err)
	}
	if v := versions(t, b, bk, "", "", ""); len(v) != 2 {
		t.Fatalf("versions = %+v", v)
	}

	// Abort, and a part committed after abort.
	up2, _ := b.CreateMultipartUpload(ctx, bk, "k2", storage.ObjectMeta{}, "")
	pw, _ := b.NewPartWriter(ctx, bk, "k2", up2.UploadID, 1)
	_, _ = pw.Write([]byte("x"))
	if err := b.AbortMultipartUpload(ctx, bk, "wrong", up2.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("abort key mismatch: %v", err)
	}
	if err := b.AbortMultipartUpload(ctx, bk, "k2", up2.UploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Commit(ctx, storage.PartMeta{}); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("part commit after abort: %v", err)
	}
	if err := pw.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := b.AbortMultipartUpload(ctx, bk, "k2", up2.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("abort twice: %v", err)
	}
}

func TestListMultipartUploadsOrder(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	var want []string
	for _, k := range []string{"b", "a", "b", "c", "a"} {
		u, err := b.CreateMultipartUpload(ctx, bk, k, storage.ObjectMeta{}, "")
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, k+"/"+u.Initiated.Format(time.RFC3339))
	}
	slices.Sort(want)
	ups, err := b.ListMultipartUploads(ctx, bk)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range ups {
		got = append(got, u.Key+"/"+u.Initiated.Format(time.RFC3339))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	// Same key and time: ordered by upload ID.
	b.now = func() time.Time { return clock }
	for i := 0; i < 5; i++ {
		_, _ = b.CreateMultipartUpload(ctx, bk, "z", storage.ObjectMeta{}, "")
	}
	ups, _ = b.ListMultipartUploads(ctx, bk)
	var zs []string
	for _, u := range ups {
		if u.Key == "z" {
			zs = append(zs, u.UploadID)
		}
	}
	if !slices.IsSorted(zs) || len(zs) != 5 {
		t.Fatalf("same-time uploads not sorted by id: %v", zs)
	}
}

func TestMultipartEmptyAndUnversioned(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	up, _ := b.CreateMultipartUpload(ctx, bk, "k", storage.ObjectMeta{}, "")
	writePart(t, b, "k", up.UploadID, 1, "")
	oi, err := b.CompleteMultipartUpload(ctx, bk, "k", up.UploadID, []int{1}, storage.ObjectMeta{ETag: "e-1"}, storage.CommitOptions{})
	if err != nil || oi.Size != 0 || oi.VersionID != "null" {
		t.Fatalf("complete = %+v, %v", oi, err)
	}
	if read(t, b, bk, "k", "") != "" {
		t.Fatal("expected empty object")
	}
}

func TestClose(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	put(t, b, bk, "k", "keep reading")
	_, r, _ := b.OpenObject(ctx, bk, "k", "")
	w, _ := b.NewObjectWriter(ctx, bk, "late")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal("second close:", err)
	}
	if data, err := io.ReadAll(r); err != nil || string(data) != "keep reading" {
		t.Fatalf("reader after close = %q, %v", data, err)
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("commit after close: %v", err)
	}
	checks := map[string]error{
		"list":   func() error { _, err := b.ListBuckets(ctx); return err }(),
		"create": b.CreateBucket(ctx, "x", storage.CreateBucketOptions{}),
		"get":    func() error { _, err := b.GetObjectInfo(ctx, bk, "k", ""); return err }(),
		"delete": b.DeleteBucket(ctx, bk),
		"walk":   b.WalkObjects(ctx, bk, "", "", func(storage.ObjectInfo) error { return nil }),
	}
	for name, err := range checks {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after close: %v", name, err)
		}
	}
}

func TestReaderInterfaces(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningUnversioned)
	put(t, b, bk, "k", "0123456789")
	_, r, _ := b.OpenObject(ctx, bk, "k", "")
	defer r.Close()
	ra, ok := r.(io.ReaderAt)
	if !ok {
		t.Fatal("reader does not implement io.ReaderAt")
	}
	p := make([]byte, 3)
	if n, err := ra.ReadAt(p, 4); n != 3 || err != nil || string(p) != "456" {
		t.Fatalf("ReadAt = %d %v %q", n, err, p)
	}
	if _, err := r.Seek(-2, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	var sb bytes.Buffer
	if _, err := io.Copy(&sb, r); err != nil || sb.String() != "89" {
		t.Fatalf("copy = %q, %v", sb.String(), err)
	}
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}
}

func TestConcurrentStress(t *testing.T) {
	b, bk := newBucket(t, storage.VersioningEnabled)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("k%02d", i%20)
				switch i % 5 {
				case 0, 1:
					w, err := b.NewObjectWriter(ctx, bk, key)
					if err != nil {
						t.Error(err)
						return
					}
					fmt.Fprintf(w, "g%d-%d", g, i)
					if _, err := w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{}); err != nil {
						t.Error(err)
						return
					}
				case 2:
					if _, err := b.DeleteObject(ctx, bk, key, ""); err != nil {
						t.Error(err)
						return
					}
				case 3:
					info, r, err := b.OpenObject(ctx, bk, key, "")
					if err == nil && r != nil {
						data, _ := io.ReadAll(r)
						if int64(len(data)) != info.Size {
							t.Errorf("size mismatch %d vs %d", len(data), info.Size)
						}
						r.Close()
					}
				case 4:
					_ = b.WalkVersions(ctx, bk, "", "", "", func(o storage.ObjectInfo) error { return nil })
					_, _ = b.UpdateObjectMeta(ctx, bk, key, "", func(m *storage.ObjectMeta) error {
						m.Tags = map[string]string{"g": fmt.Sprint(g)}
						return nil
					})
				}
			}
		}(g)
	}
	wg.Wait()
}
