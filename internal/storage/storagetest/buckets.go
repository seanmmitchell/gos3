package storagetest

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func bucketTests() []testCase {
	return []testCase{
		{"CreateAndGet", testBucketCreateAndGet},
		{"CreateWithObjectLock", testBucketCreateWithObjectLock},
		{"CreateDuplicate", testBucketCreateDuplicate},
		{"GetMissing", testBucketGetMissing},
		{"ListOrder", testBucketListOrder},
		{"Delete", testBucketDelete},
		{"DeleteNotEmpty", testBucketDeleteNotEmpty},
		{"DeleteRemovesAllState", testBucketDeleteRemovesAllState},
		{"Config", testBucketConfig},
		{"ConfigAllKeys", testBucketConfigAllKeys},
		{"ConfigMissingBucket", testBucketConfigMissingBucket},
		{"Versioning", testBucketVersioning},
		{"ObjectLock", testBucketObjectLock},
		{"MissingBucketOperations", testBucketMissingOps},
	}
}

func testBucketCreateAndGet(e *env) {
	start := time.Now()
	e.mkBucketOpts("bucket-a", storage.CreateBucketOptions{Region: "eu-central-1", Owner: "alice"})
	e.mkBucket("bucket-b")

	for _, tc := range []struct {
		name string
		want storage.BucketInfo
	}{
		{"bucket-a", storage.BucketInfo{Name: "bucket-a", Region: "eu-central-1", Owner: "alice"}},
		// Zero options are stored verbatim: the S3 layer picks the default region.
		{"bucket-b", storage.BucketInfo{Name: "bucket-b"}},
	} {
		got, err := e.b.GetBucket(e.ctx, tc.name)
		e.noErr("GetBucket("+tc.name+")", err)
		e.checkTime("GetBucket("+tc.name+").CreatedAt", got.CreatedAt, start)
		// Backing is placement information that routing backends
		// (internal/storage/multi, routed) fill in for buckets created
		// without one; ListBuckets must agree with GetBucket on it (below).
		got.CreatedAt, got.Backing = time.Time{}, ""
		if got := withoutOutputOnly(got); got != tc.want {
			e.t.Errorf("GetBucket(%q) = %+v, want %+v (CreatedAt, Backing, Kind and Location ignored)", tc.name, got, tc.want)
		}
	}

	// ListBuckets reports the same information as GetBucket.
	list, err := e.b.ListBuckets(e.ctx)
	e.noErr("ListBuckets", err)
	if len(list) != 2 {
		e.t.Fatalf("ListBuckets returned %d buckets, want 2: %+v", len(list), list)
	}
	for _, li := range list {
		gi, err := e.b.GetBucket(e.ctx, li.Name)
		e.noErr("GetBucket", err)
		if !li.CreatedAt.Equal(gi.CreatedAt) {
			e.t.Errorf("ListBuckets CreatedAt %s != GetBucket CreatedAt %s for %q", fmtTime(li.CreatedAt), fmtTime(gi.CreatedAt), li.Name)
		}
		li.CreatedAt, gi.CreatedAt = time.Time{}, time.Time{}
		if li, gi := withoutOutputOnly(li), withoutOutputOnly(gi); li != gi {
			e.t.Errorf("ListBuckets entry %+v != GetBucket %+v (Kind and Location ignored)", li, gi)
		}
	}
}

// withoutOutputOnly returns bi without the output-only fields that
// backings with bucket kinds (s3 backings) set and others leave empty:
// Kind and Location. They are not part of what CreateBucket stores, so
// whole-value comparisons ignore them.
func withoutOutputOnly(bi storage.BucketInfo) storage.BucketInfo {
	bi.Kind, bi.Location = "", ""
	return bi
}

func testBucketCreateWithObjectLock(e *env) {
	e.mkBucketOpts("locked", storage.CreateBucketOptions{ObjectLockEnabled: true, Owner: "bob", Region: "us-west-2"})
	got, err := e.b.GetBucket(e.ctx, "locked")
	e.noErr("GetBucket", err)
	if !got.ObjectLockEnabled {
		e.t.Error("ObjectLockEnabled = false, want true")
	}
	if got.Versioning != storage.VersioningEnabled {
		e.t.Errorf("Versioning = %q, want %q (object lock implies versioning)", got.Versioning, storage.VersioningEnabled)
	}
	if got.Owner != "bob" || got.Region != "us-west-2" {
		e.t.Errorf("Owner/Region = %q/%q, want bob/us-west-2", got.Owner, got.Region)
	}
	// Writes into an object-lock bucket are versioned.
	info := e.put("locked", "k", []byte("x"))
	e.checkVersionID("put into object-lock bucket", info.VersionID)
}

func testBucketCreateDuplicate(e *env) {
	e.mkBucketOpts("dup", storage.CreateBucketOptions{Region: "r1", Owner: "alice"})
	e.put("dup", "k", []byte("keep me"))
	err := e.b.CreateBucket(e.ctx, "dup", storage.CreateBucketOptions{Region: "r2", Owner: "mallory", ObjectLockEnabled: true})
	e.wantErr("CreateBucket of an existing bucket", err, storage.ErrBucketExists)
	got, err := e.b.GetBucket(e.ctx, "dup")
	e.noErr("GetBucket", err)
	if got.Region != "r1" || got.Owner != "alice" || got.ObjectLockEnabled || got.Versioning != storage.VersioningUnversioned {
		e.t.Errorf("failed CreateBucket modified the existing bucket: %+v", got)
	}
	e.wantContent("dup", "k", "", []byte("keep me"))
	list, err := e.b.ListBuckets(e.ctx)
	e.noErr("ListBuckets", err)
	if len(list) != 1 {
		e.t.Errorf("ListBuckets returned %d buckets after duplicate create, want 1", len(list))
	}
}

func testBucketGetMissing(e *env) {
	_, err := e.b.GetBucket(e.ctx, "missing")
	e.wantErr("GetBucket(missing)", err, storage.ErrBucketNotFound)
	e.mkBucket("present")
	_, err = e.b.GetBucket(e.ctx, "presen")
	e.wantErr("GetBucket(prefix of existing name)", err, storage.ErrBucketNotFound)
	_, err = e.b.GetBucket(e.ctx, "present-2")
	e.wantErr("GetBucket(extension of existing name)", err, storage.ErrBucketNotFound)
}

func testBucketListOrder(e *env) {
	list, err := e.b.ListBuckets(e.ctx)
	e.noErr("ListBuckets on empty backend", err)
	if len(list) != 0 {
		e.t.Fatalf("ListBuckets on empty backend returned %d buckets, want 0", len(list))
	}
	names := []string{"zeta", "alpha", "beta-1", "beta", "a0", "a.b", "a-b", "1abc", "beta.0", "alpha-z"}
	for _, n := range names {
		e.mkBucket(n)
	}
	want := slices.Clone(names)
	sort.Strings(want)
	list, err = e.b.ListBuckets(e.ctx)
	e.noErr("ListBuckets", err)
	got := make([]string, len(list))
	for i, b := range list {
		got[i] = b.Name
	}
	if !slices.Equal(got, want) {
		e.t.Errorf("ListBuckets order:\n got  %q\n want %q", got, want)
	}
}

func testBucketDelete(e *env) {
	e.mkBucket("keep")
	e.mkBucket("gone")
	e.noErr("DeleteBucket(gone)", e.b.DeleteBucket(e.ctx, "gone"))
	_, err := e.b.GetBucket(e.ctx, "gone")
	e.wantErr("GetBucket after DeleteBucket", err, storage.ErrBucketNotFound)
	list, err := e.b.ListBuckets(e.ctx)
	e.noErr("ListBuckets", err)
	if len(list) != 1 || list[0].Name != "keep" {
		e.t.Errorf("ListBuckets after delete = %+v, want only \"keep\"", list)
	}
	e.wantErr("DeleteBucket twice", e.b.DeleteBucket(e.ctx, "gone"), storage.ErrBucketNotFound)
	e.wantErr("DeleteBucket(never created)", e.b.DeleteBucket(e.ctx, "never"), storage.ErrBucketNotFound)
	// The name can be reused.
	e.mkBucket("gone")
	// Deleting an object leaves the bucket deletable.
	e.put("gone", "k", []byte("v"))
	e.del("gone", "k", "")
	e.noErr("DeleteBucket after removing its only object", e.b.DeleteBucket(e.ctx, "gone"))
}

func testBucketDeleteNotEmpty(e *env) {
	cases := []struct {
		name    string
		setup   func(e *env, bucket string)
		cleanup func(e *env, bucket string)
	}{
		{
			name:    "CurrentObject",
			setup:   func(e *env, b string) { e.put(b, "obj", []byte("data")) },
			cleanup: func(e *env, b string) { e.del(b, "obj", "") },
		},
		{
			name:    "EmptyObject",
			setup:   func(e *env, b string) { e.put(b, "empty", nil) },
			cleanup: func(e *env, b string) { e.del(b, "empty", "") },
		},
		{
			name: "NoncurrentVersionsOnly",
			setup: func(e *env, b string) {
				e.setVersioning(b, storage.VersioningEnabled)
				e.put(b, "obj", []byte("v1"))
				e.put(b, "obj", []byte("v2"))
				e.del(b, "obj", "") // hides the versions behind a delete marker
				if keys := e.walkKeys(b, "", ""); len(keys) != 0 {
					e.t.Fatalf("WalkObjects lists %q, want nothing (latest is a delete marker)", keys)
				}
			},
			cleanup: func(e *env, b string) { e.deleteAllVersions(b) },
		},
		{
			name: "OnlyDeleteMarker",
			setup: func(e *env, b string) {
				e.setVersioning(b, storage.VersioningEnabled)
				res := e.del(b, "never-existed", "")
				if !res.DeleteMarker {
					e.t.Fatalf("DeleteObject in Enabled bucket = %+v, want a delete marker", res)
				}
			},
			cleanup: func(e *env, b string) { e.deleteAllVersions(b) },
		},
		{
			name: "OnlyNullDeleteMarker",
			setup: func(e *env, b string) {
				e.setVersioning(b, storage.VersioningSuspended)
				e.del(b, "never-existed", "")
			},
			cleanup: func(e *env, b string) { e.del(b, "never-existed", storage.NullVersionID) },
		},
		// In-progress multipart uploads alone do not make a bucket
		// non-empty; see testMultipartDeleteBucketAbortsUploads.
	}
	for _, tc := range cases {
		e.runFresh(tc.name, func(e *env) {
			e.mkBucket("full")
			tc.setup(e, "full")
			e.wantErr("DeleteBucket of a non-empty bucket", e.b.DeleteBucket(e.ctx, "full"), storage.ErrBucketNotEmpty)
			if _, err := e.b.GetBucket(e.ctx, "full"); err != nil {
				e.t.Fatalf("GetBucket after failed DeleteBucket: %v", err)
			}
			tc.cleanup(e, "full")
			e.noErr("DeleteBucket after emptying", e.b.DeleteBucket(e.ctx, "full"))
			_, err := e.b.GetBucket(e.ctx, "full")
			e.wantErr("GetBucket after DeleteBucket", err, storage.ErrBucketNotFound)
		})
	}
}

func testBucketDeleteRemovesAllState(e *env) {
	e.mkBucketOpts("recycle", storage.CreateBucketOptions{Region: "r1", Owner: "alice", ObjectLockEnabled: true})
	e.noErr("PutBucketConfig", e.b.PutBucketConfig(e.ctx, "recycle", storage.ConfigPolicy, []byte(`{"Version":"2012-10-17"}`)))
	e.noErr("PutBucketConfig", e.b.PutBucketConfig(e.ctx, "recycle", storage.ConfigTagging, []byte("<Tagging/>")))
	e.put("recycle", "obj", []byte("v1"))
	e.put("recycle", "obj", []byte("v2"))
	e.del("recycle", "obj", "")
	u := e.createUpload("recycle", "mp", storage.ObjectMeta{})
	e.part("recycle", "mp", u.UploadID, 1, []byte("p"))
	e.deleteAllVersions("recycle")
	e.noErr("DeleteBucket", e.b.DeleteBucket(e.ctx, "recycle"))

	e.mkBucket("recycle")
	got, err := e.b.GetBucket(e.ctx, "recycle")
	e.noErr("GetBucket", err)
	if got.Region != "" || got.Owner != "" || got.ObjectLockEnabled || got.Versioning != storage.VersioningUnversioned {
		e.t.Errorf("recreated bucket has stale settings: %+v", got)
	}
	for _, k := range []string{storage.ConfigPolicy, storage.ConfigTagging, storage.ConfigObjectLock} {
		_, err := e.b.GetBucketConfig(e.ctx, "recycle", k)
		e.wantErr("GetBucketConfig("+k+") on recreated bucket", err, storage.ErrConfigNotFound)
	}
	if v := e.walkVersions("recycle", "", "", ""); len(v) != 0 {
		e.t.Errorf("recreated bucket has %d stale versions: %v", len(v), refsOf(v))
	}
	ups, err := e.b.ListMultipartUploads(e.ctx, "recycle")
	e.noErr("ListMultipartUploads", err)
	if len(ups) != 0 {
		e.t.Errorf("recreated bucket has %d stale uploads", len(ups))
	}
	_, err = e.b.GetMultipartUpload(e.ctx, "recycle", "mp", u.UploadID)
	e.wantErr("GetMultipartUpload of an upload from the deleted bucket", err, storage.ErrUploadNotFound)
	_, err = e.b.GetObjectInfo(e.ctx, "recycle", "obj", "")
	e.wantErr("GetObjectInfo on recreated bucket", err, storage.ErrObjectNotFound)
	// The recreated bucket is unversioned: writes create null versions.
	if info := e.put("recycle", "obj", []byte("new")); info.VersionID != storage.NullVersionID {
		e.t.Errorf("put into recreated bucket: VersionID = %q, want %q", info.VersionID, storage.NullVersionID)
	}
}

func testBucketConfig(e *env) {
	e.mkBucket("cfg")
	_, err := e.b.GetBucketConfig(e.ctx, "cfg", storage.ConfigPolicy)
	e.wantErr("GetBucketConfig unset", err, storage.ErrConfigNotFound)

	policy := []byte(`{"Version":"2012-10-17","Statement":[]}`)
	in := bytes.Clone(policy)
	e.noErr("PutBucketConfig", e.b.PutBucketConfig(e.ctx, "cfg", storage.ConfigPolicy, in))
	for i := range in {
		in[i] = 'X' // caller reuses its buffer
	}
	got, err := e.b.GetBucketConfig(e.ctx, "cfg", storage.ConfigPolicy)
	e.noErr("GetBucketConfig", err)
	if !bytes.Equal(got, policy) {
		e.t.Fatalf("GetBucketConfig = %q, want %q (mutating the caller's slice after Put must not affect the stored document)", got, policy)
	}
	for i := range got {
		got[i] = 'Y' // caller mutates the returned slice
	}
	got, err = e.b.GetBucketConfig(e.ctx, "cfg", storage.ConfigPolicy)
	e.noErr("GetBucketConfig", err)
	if !bytes.Equal(got, policy) {
		e.t.Fatalf("GetBucketConfig = %q, want %q (mutating a returned slice must not affect the stored document)", got, policy)
	}

	// Other keys stay independent.
	_, err = e.b.GetBucketConfig(e.ctx, "cfg", storage.ConfigCORS)
	e.wantErr("GetBucketConfig of another key", err, storage.ErrConfigNotFound)

	// Overwrite with a larger binary document.
	big := randBytes(64<<10, 7)
	big[0], big[1] = 0x00, 0xff
	e.noErr("PutBucketConfig overwrite", e.b.PutBucketConfig(e.ctx, "cfg", storage.ConfigPolicy, big))
	got, err = e.b.GetBucketConfig(e.ctx, "cfg", storage.ConfigPolicy)
	e.noErr("GetBucketConfig", err)
	if !bytes.Equal(got, big) {
		e.t.Errorf("GetBucketConfig after overwrite: %s", describeBytesDiff(big, got))
	}

	e.noErr("DeleteBucketConfig", e.b.DeleteBucketConfig(e.ctx, "cfg", storage.ConfigPolicy))
	_, err = e.b.GetBucketConfig(e.ctx, "cfg", storage.ConfigPolicy)
	e.wantErr("GetBucketConfig after delete", err, storage.ErrConfigNotFound)
	e.noErr("DeleteBucketConfig twice (idempotent)", e.b.DeleteBucketConfig(e.ctx, "cfg", storage.ConfigPolicy))
	e.noErr("DeleteBucketConfig never set (idempotent)", e.b.DeleteBucketConfig(e.ctx, "cfg", storage.ConfigWebsite))

	// Configs are per bucket.
	e.mkBucket("cfg-2")
	e.noErr("PutBucketConfig", e.b.PutBucketConfig(e.ctx, "cfg-2", storage.ConfigCORS, []byte("<CORSConfiguration/>")))
	_, err = e.b.GetBucketConfig(e.ctx, "cfg", storage.ConfigCORS)
	e.wantErr("GetBucketConfig of a key set on another bucket", err, storage.ErrConfigNotFound)

	// Configs are removed with the bucket.
	e.noErr("DeleteBucket", e.b.DeleteBucket(e.ctx, "cfg-2"))
	e.mkBucket("cfg-2")
	_, err = e.b.GetBucketConfig(e.ctx, "cfg-2", storage.ConfigCORS)
	e.wantErr("GetBucketConfig after bucket delete + recreate", err, storage.ErrConfigNotFound)
}

func testBucketConfigAllKeys(e *env) {
	keys := []string{
		storage.ConfigPolicy, storage.ConfigCORS, storage.ConfigLifecycle, storage.ConfigTagging,
		storage.ConfigEncryption, storage.ConfigObjectLock, storage.ConfigACL, storage.ConfigWebsite,
		storage.ConfigOwnership, storage.ConfigNotification, storage.ConfigLogging,
		storage.ConfigPublicAccessBlock, storage.ConfigReplication, storage.ConfigAccelerate,
		storage.ConfigRequestPayment,
	}
	e.mkBucket("cfg")
	doc := func(k string) []byte { return fmt.Appendf(nil, "<doc key=%q>\x00\xff</doc>", k) }
	for _, k := range keys {
		e.noErr("PutBucketConfig("+k+")", e.b.PutBucketConfig(e.ctx, "cfg", k, doc(k)))
	}
	for _, k := range keys {
		got, err := e.b.GetBucketConfig(e.ctx, "cfg", k)
		e.noErr("GetBucketConfig("+k+")", err)
		if !bytes.Equal(got, doc(k)) {
			e.t.Errorf("GetBucketConfig(%q) = %q, want %q", k, got, doc(k))
		}
	}
	for i, k := range keys {
		if i%2 == 0 {
			e.noErr("DeleteBucketConfig("+k+")", e.b.DeleteBucketConfig(e.ctx, "cfg", k))
		}
	}
	for i, k := range keys {
		got, err := e.b.GetBucketConfig(e.ctx, "cfg", k)
		if i%2 == 0 {
			e.wantErr("GetBucketConfig("+k+") after delete", err, storage.ErrConfigNotFound)
		} else if err != nil || !bytes.Equal(got, doc(k)) {
			e.t.Errorf("GetBucketConfig(%q) after deleting other keys = %q, %v; want %q", k, got, err, doc(k))
		}
	}
}

func testBucketConfigMissingBucket(e *env) {
	_, err := e.b.GetBucketConfig(e.ctx, "missing", storage.ConfigPolicy)
	e.wantErr("GetBucketConfig on missing bucket", err, storage.ErrBucketNotFound)
	err = e.b.PutBucketConfig(e.ctx, "missing", storage.ConfigPolicy, []byte("{}"))
	e.wantErr("PutBucketConfig on missing bucket", err, storage.ErrBucketNotFound)
	err = e.b.DeleteBucketConfig(e.ctx, "missing", storage.ConfigPolicy)
	e.wantErr("DeleteBucketConfig on missing bucket", err, storage.ErrBucketNotFound)
	if _, err := e.b.GetBucket(e.ctx, "missing"); !isAny(err, storage.ErrBucketNotFound) {
		e.t.Errorf("config operations on a missing bucket created it (GetBucket error %v)", err)
	}
}

func testBucketVersioning(e *env) {
	e.mkBucket("ver")
	for _, status := range []storage.VersioningStatus{
		storage.VersioningEnabled, storage.VersioningSuspended, storage.VersioningEnabled, storage.VersioningSuspended,
	} {
		e.setVersioning("ver", status)
		got, err := e.b.GetBucket(e.ctx, "ver")
		e.noErr("GetBucket", err)
		if got.Versioning != status {
			e.t.Errorf("after SetBucketVersioning(%q): Versioning = %q", status, got.Versioning)
		}
		list, err := e.b.ListBuckets(e.ctx)
		e.noErr("ListBuckets", err)
		if len(list) != 1 || list[0].Versioning != status {
			e.t.Errorf("after SetBucketVersioning(%q): ListBuckets = %+v", status, list)
		}
	}
	err := e.b.SetBucketVersioning(e.ctx, "missing", storage.VersioningEnabled)
	e.wantErr("SetBucketVersioning on missing bucket", err, storage.ErrBucketNotFound)
}

func testBucketObjectLock(e *env) {
	e.mkVersioned("lock", storage.VersioningEnabled)
	err := e.b.SetBucketObjectLock(e.ctx, "lock", true)
	e.skipIfNotSupported(err)
	e.noErr("SetBucketObjectLock", err)
	got, err := e.b.GetBucket(e.ctx, "lock")
	e.noErr("GetBucket", err)
	if !got.ObjectLockEnabled || got.Versioning != storage.VersioningEnabled {
		e.t.Errorf("after SetBucketObjectLock(true): %+v, want ObjectLockEnabled and Versioning Enabled", got)
	}
	err = e.b.SetBucketObjectLock(e.ctx, "missing", true)
	e.wantErr("SetBucketObjectLock on missing bucket", err, storage.ErrBucketNotFound)
}

// testBucketMissingOps checks every bucket-scoped operation reports
// ErrBucketNotFound for a bucket that never existed and for a deleted one,
// and that none of them (re)creates the bucket.
func testBucketMissingOps(e *env) {
	e.mkBucket("deleted")
	e.noErr("DeleteBucket", e.b.DeleteBucket(e.ctx, "deleted"))
	noop := func(storage.ObjectInfo) error { return nil }
	for _, bucket := range []string{"never", "deleted"} {
		e.run(bucket, func(e *env) {
			nf := storage.ErrBucketNotFound
			_, err := e.b.NewObjectWriter(e.ctx, bucket, "k")
			e.wantErr("NewObjectWriter", err, nf)
			_, err = e.b.GetObjectInfo(e.ctx, bucket, "k", "")
			e.wantErr("GetObjectInfo", err, nf)
			_, r, err := e.b.OpenObject(e.ctx, bucket, "k", "")
			e.wantErr("OpenObject", err, nf)
			if r != nil {
				e.t.Error("OpenObject returned a non-nil reader with an error")
				_ = r.Close()
			}
			_, err = e.b.UpdateObjectMeta(e.ctx, bucket, "k", "", func(*storage.ObjectMeta) error { return nil })
			e.wantErr("UpdateObjectMeta", err, nf)
			_, err = e.b.DeleteObject(e.ctx, bucket, "k", "")
			e.wantErr("DeleteObject", err, nf)
			_, err = e.b.DeleteObject(e.ctx, bucket, "k", "some-version")
			e.wantErr("DeleteObject(version)", err, nf)
			e.wantErr("WalkObjects", e.b.WalkObjects(e.ctx, bucket, "", "", noop), nf)
			e.wantErr("WalkVersions", e.b.WalkVersions(e.ctx, bucket, "", "", "", noop), nf)
			_, err = e.b.CreateMultipartUpload(e.ctx, bucket, "k", storage.ObjectMeta{}, "")
			e.wantErr("CreateMultipartUpload", err, nf)
			_, err = e.b.ListMultipartUploads(e.ctx, bucket)
			e.wantErr("ListMultipartUploads", err, nf)
			// Upload-scoped operations may report either error.
			_, err = e.b.GetMultipartUpload(e.ctx, bucket, "k", "upload")
			e.wantErr("GetMultipartUpload", err, nf, storage.ErrUploadNotFound)
			_, err = e.b.NewPartWriter(e.ctx, bucket, "k", "upload", 1)
			e.wantErr("NewPartWriter", err, nf, storage.ErrUploadNotFound)
			_, err = e.b.ListParts(e.ctx, bucket, "k", "upload")
			e.wantErr("ListParts", err, nf, storage.ErrUploadNotFound)
			_, err = e.b.CompleteMultipartUpload(e.ctx, bucket, "k", "upload", []int{1}, storage.ObjectMeta{}, storage.CommitOptions{})
			e.wantErr("CompleteMultipartUpload", err, nf, storage.ErrUploadNotFound)
			e.wantErr("AbortMultipartUpload", e.b.AbortMultipartUpload(e.ctx, bucket, "k", "upload"), nf, storage.ErrUploadNotFound)
			_, err = storage.ListObjects(e.ctx, e.b, bucket, storage.ListParams{})
			e.wantErr("storage.ListObjects", err, nf)
			_, err = storage.ListObjectVersions(e.ctx, e.b, bucket, storage.VersionListParams{})
			e.wantErr("storage.ListObjectVersions", err, nf)
			_, err = storage.ComputeBucketUsage(e.ctx, e.b, bucket)
			e.wantErr("storage.ComputeBucketUsage", err, nf)

			_, err = e.b.GetBucket(e.ctx, bucket)
			e.wantErr("GetBucket after operations on a missing bucket", err, nf)
		})
	}
	list, err := e.b.ListBuckets(e.ctx)
	e.noErr("ListBuckets", err)
	if len(list) != 0 {
		e.t.Errorf("operations on missing buckets created buckets: %+v", list)
	}
}
