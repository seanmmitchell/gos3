package storagetest

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func walkTests() []testCase {
	return []testCase{
		{"EmptyBucket", testWalkEmpty},
		{"ObjectsOrder", testWalkObjectsOrder},
		{"ObjectsPrefixAfter", testWalkObjectsPrefixAfter},
		{"ObjectsSkipDeleteMarkers", testWalkObjectsSkipMarkers},
		{"ObjectsStopAndErrors", testWalkObjectsStop},
		{"VersionsOrder", testWalkVersionsOrder},
		{"VersionsMarkers", testWalkVersionsMarkers},
		{"VersionsStopAndErrors", testWalkVersionsStop},
		{"VersionsDeletedMarker", testWalkVersionsDeletedMarker},
		{"VersionsLatestUnderMutation", testWalkVersionsLatestUnderMutation},
	}
}

// walkKeySet is a key set chosen to expose byte-wise ordering bugs ('/' is
// 0x2F, '-' 0x2D, '0' 0x30; multi-byte UTF-8 sorts after ASCII; NUL sorts
// before everything, so "a\x00b" lies between "a" and "a-c").
var walkKeySet = []string{
	"b", "a", "a/b", "a-c", "a0", "a/b/c", "a/", "ab", "A", "Z", "_", "~",
	"é", "日本", "\U0001F600", "a/é", "a/z", "b/c/d", "b0", "a\x00b", "a\x00",
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	sort.Strings(c)
	return c
}

func testWalkEmpty(e *env) {
	e.mkBucket("b")
	calls := 0
	count := func(storage.ObjectInfo) error { calls++; return nil }
	e.noErr("WalkObjects", e.b.WalkObjects(e.ctx, "b", "", "", count))
	e.noErr("WalkVersions", e.b.WalkVersions(e.ctx, "b", "", "", "", count))
	e.noErr("WalkObjects with prefix/after", e.b.WalkObjects(e.ctx, "b", "p", "q", count))
	e.noErr("WalkVersions with markers", e.b.WalkVersions(e.ctx, "b", "p", "q", "v", count))
	if calls != 0 {
		e.t.Errorf("walks over an empty bucket invoked the callback %d times", calls)
	}
}

func testWalkObjectsOrder(e *env) {
	e.mkBucket("b")
	for _, k := range walkKeySet {
		e.put("b", k, []byte("data:"+k))
	}
	objs := e.walkObjects("b", "", "")
	want := sortedCopy(walkKeySet)
	if got := keysOf(objs); !slices.Equal(got, want) {
		e.t.Fatalf("WalkObjects order:\n got  %q\n want %q (byte-wise ascending)", got, want)
	}
	for _, o := range objs {
		if o.Bucket != "b" || !o.IsLatest || o.DeleteMarker || o.VersionID != storage.NullVersionID ||
			o.Size != int64(len("data:"+o.Key)) || o.ETag != etagOf([]byte("data:"+o.Key)) || o.LastModified.IsZero() {
			e.t.Errorf("WalkObjects entry for %q = %+v, want complete latest-version info", o.Key, o)
		}
	}
}

func testWalkObjectsPrefixAfter(e *env) {
	e.mkBucket("b")
	for _, k := range walkKeySet {
		e.put("b", k, []byte(k))
	}
	sorted := sortedCopy(walkKeySet)
	prefixes := []string{"", "a", "a/", "a/b", "b", "é", "zz", "a-", "A"}
	afters := append([]string{"", "0", "a", "a/", "a/a", "a.", "aa", "b/", "b/c/d", "zzz", "\U0010FFFF", "\U0010FFFF\U0010FFFF"}, walkKeySet...)
	for _, p := range prefixes {
		for _, a := range afters {
			var want []string
			for _, k := range sorted {
				if strings.HasPrefix(k, p) && k > a {
					want = append(want, k)
				}
			}
			got := e.walkKeys("b", p, a)
			if !slices.Equal(got, want) {
				e.t.Errorf("WalkObjects(prefix %q, after %q):\n got  %q\n want %q", p, a, got, want)
			}
		}
	}
}

func testWalkObjectsSkipMarkers(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	for _, k := range []string{"x", "y", "z", "y/child"} {
		e.put("b", k, []byte(k))
	}
	e.del("b", "y", "")
	e.del("b", "only-marker", "")
	e.put("b", "z", []byte("z2")) // multiple versions, latest is an object
	if got := e.walkKeys("b", "", ""); !slices.Equal(got, []string{"x", "y/child", "z"}) {
		e.t.Errorf("WalkObjects = %q, want [x y/child z] (keys whose latest is a delete marker are skipped)", got)
	}
	if got := e.walkKeys("b", "y", ""); !slices.Equal(got, []string{"y/child"}) {
		e.t.Errorf("WalkObjects(prefix y) = %q, want [y/child]", got)
	}
	if got := e.walkKeys("b", "", "x"); !slices.Equal(got, []string{"y/child", "z"}) {
		e.t.Errorf("WalkObjects(after x) = %q, want [y/child z]", got)
	}
	objs := e.walkObjects("b", "z", "")
	if len(objs) != 1 || objs[0].Size != 2 || !objs[0].IsLatest {
		e.t.Errorf("WalkObjects(prefix z) = %+v, want the latest version (2 bytes)", objs)
	}
	// Removing the marker brings the key back.
	h := e.history("b", "y")
	e.del("b", "y", h[0].VersionID)
	if got := e.walkKeys("b", "", ""); !slices.Equal(got, []string{"x", "y", "y/child", "z"}) {
		e.t.Errorf("WalkObjects after removing the marker = %q, want [x y y/child z]", got)
	}
}

func testWalkObjectsStop(e *env) {
	e.mkBucket("b")
	for i := range 50 {
		e.put("b", fmt.Sprintf("k%02d", i), nil)
	}
	var got []string
	err := e.b.WalkObjects(e.ctx, "b", "", "", func(o storage.ObjectInfo) error {
		got = append(got, o.Key)
		if len(got) == 3 {
			return storage.ErrStopWalk
		}
		return nil
	})
	if err != nil {
		e.t.Errorf("WalkObjects stopped with ErrStopWalk returned %v, want nil", err)
	}
	if !slices.Equal(got, []string{"k00", "k01", "k02"}) {
		e.t.Errorf("WalkObjects visited %q after ErrStopWalk, want [k00 k01 k02]", got)
	}

	sentinel := errors.New("callback failure")
	calls := 0
	err = e.b.WalkObjects(e.ctx, "b", "", "k10", func(storage.ObjectInfo) error {
		calls++
		if calls == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		e.t.Errorf("WalkObjects returned %v, want the callback's error", err)
	}
	if calls != 2 {
		e.t.Errorf("WalkObjects invoked the callback %d times, want 2 (stop at the first error)", calls)
	}
}

// buildVersionedFixture creates a bucket with a known version history and
// returns it as an ordered list (keys ascending, newest first per key).
func (e *env) buildVersionedFixture(bucket string) []vref {
	e.t.Helper()
	e.mkBucket(bucket)
	hist := map[string][]vref{} // newest first
	add := func(key string, info storage.ObjectInfo, marker bool) {
		hist[key] = append([]vref{{Key: key, VersionID: info.VersionID, DeleteMarker: marker}}, hist[key]...)
	}
	addDel := func(key string) {
		res := e.del(bucket, key, "")
		add(key, storage.ObjectInfo{VersionID: res.VersionID}, true)
	}
	// Unversioned phase: null versions.
	add("c", e.put(bucket, "c", []byte("c-null")), false)
	add("m", e.put(bucket, "m", []byte("m-null")), false)
	e.setVersioning(bucket, storage.VersioningEnabled)
	for i := range 3 {
		add("a", e.put(bucket, "a", fmt.Appendf(nil, "a%d", i)), false)
	}
	addDel("a")
	add("a", e.put(bucket, "a", []byte("a-after-marker")), false)
	// A NUL byte sorts between "a" and every other key starting with "a".
	add("a\x00b", e.put(bucket, "a\x00b", []byte("nul1")), false)
	add("a\x00b", e.put(bucket, "a\x00b", []byte("nul2")), false)
	addDel("b")
	add("c", e.put(bucket, "c", []byte("c1")), false)
	add("d/e", e.put(bucket, "d/e", []byte("de")), false)
	add("d/f", e.put(bucket, "d/f", []byte("df")), false)
	addDel("d/f")
	add("m", e.put(bucket, "m", []byte("m1")), false)
	e.setVersioning(bucket, storage.VersioningSuspended)
	// Replaces m's null version (noncurrent) with a new latest null version.
	hist["m"] = slices.DeleteFunc(hist["m"], func(v vref) bool { return v.VersionID == storage.NullVersionID })
	add("m", e.put(bucket, "m", []byte("m-null-2")), false)
	e.setVersioning(bucket, storage.VersioningEnabled)
	add("m", e.put(bucket, "m", []byte("m2")), false)
	add("é", e.put(bucket, "é", []byte("e")), false)
	addDel("é")

	keys := make([]string, 0, len(hist))
	for k := range hist {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []vref
	for _, k := range keys {
		for i, v := range hist[k] {
			v.IsLatest = i == 0
			out = append(out, v)
		}
	}
	return out
}

func testWalkVersionsOrder(e *env) {
	want := e.buildVersionedFixture("b")
	all := e.walkVersions("b", "", "", "")
	if got := refsOf(all); !slices.Equal(got, want) {
		e.t.Fatalf("WalkVersions order:\n got  %v\n want %v", got, want)
	}
	for _, v := range all {
		if v.Bucket != "b" || v.LastModified.IsZero() {
			e.t.Errorf("WalkVersions entry %v: Bucket %q LastModified %s", refsOf([]storage.ObjectInfo{v})[0], v.Bucket, fmtTime(v.LastModified))
		}
		if !v.DeleteMarker {
			_, data := e.read("b", v.Key, v.VersionID)
			if v.Size != int64(len(data)) || v.ETag != etagOf(data) {
				e.t.Errorf("WalkVersions entry %s@%s: Size %d ETag %s disagree with its content", v.Key, v.VersionID, v.Size, v.ETag)
			}
		}
	}
	// Prefix restriction.
	for _, p := range []string{"a", "d/", "m", "zz", "é"} {
		var wantP []vref
		for _, v := range want {
			if strings.HasPrefix(v.Key, p) {
				wantP = append(wantP, v)
			}
		}
		if got := refsOf(e.walkVersions("b", p, "", "")); !slices.Equal(got, wantP) {
			e.t.Errorf("WalkVersions(prefix %q):\n got  %v\n want %v", p, got, wantP)
		}
	}
}

func testWalkVersionsMarkers(e *env) {
	all := e.buildVersionedFixture("b")
	check := func(prefix, km, vm string, want []vref) {
		e.t.Helper()
		got := refsOf(e.walkVersions("b", prefix, km, vm))
		if !slices.Equal(got, want) {
			e.t.Errorf("WalkVersions(prefix %q, keyMarker %q, versionIDMarker %q):\n got  %v\n want %v", prefix, km, vm, got, want)
		}
	}
	filter := func(prefix string, from int, keep func(vref) bool) []vref {
		var out []vref
		for _, v := range all[from:] {
			if strings.HasPrefix(v.Key, prefix) && keep(v) {
				out = append(out, v)
			}
		}
		return out
	}
	keepAll := func(vref) bool { return true }

	// Mode 1: no key marker (versionIDMarker is ignored without keyMarker
	// per S3; the contract only defines it together with keyMarker, so it
	// is not exercised here).
	check("", "", "", all)

	// Mode 2: keyMarker only: start strictly after all versions of keyMarker.
	for _, km := range []string{"a", "a\x00", "a\x00b", "b", "c", "c0", "d/", "d/e", "m", "zz", "é", "0", "\U0010FFFF"} {
		check("", km, "", filter("", 0, func(v vref) bool { return v.Key > km }))
		check("d/", km, "", filter("d/", 0, func(v vref) bool { return v.Key > km }))
	}

	// Mode 3: keyMarker + versionIDMarker: resume right after that version
	// (including null versions and delete markers in the middle of a key).
	for i, v := range all {
		check("", v.Key, v.VersionID, all[i+1:])
		check(v.Key, v.Key, v.VersionID, filter(v.Key, i+1, keepAll))
	}
}

func testWalkVersionsStop(e *env) {
	all := e.buildVersionedFixture("b")
	var got []vref
	err := e.b.WalkVersions(e.ctx, "b", "", "", "", func(o storage.ObjectInfo) error {
		got = append(got, refsOf([]storage.ObjectInfo{o})...)
		if len(got) == 4 {
			return storage.ErrStopWalk
		}
		return nil
	})
	if err != nil {
		e.t.Errorf("WalkVersions stopped with ErrStopWalk returned %v, want nil", err)
	}
	if !slices.Equal(got, all[:4]) {
		e.t.Errorf("WalkVersions visited %v after ErrStopWalk, want %v", got, all[:4])
	}
	sentinel := errors.New("callback failure")
	calls := 0
	err = e.b.WalkVersions(e.ctx, "b", "", "", "", func(storage.ObjectInfo) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		e.t.Errorf("WalkVersions returned %v, want the callback's error", err)
	}
	if calls != 1 {
		e.t.Errorf("WalkVersions invoked the callback %d times after an error, want 1", calls)
	}
}

// testWalkVersionsDeletedMarker: the version named by a versionIDMarker may
// be deleted between two pages, as in the usual loop that empties a
// versioned bucket (list a page, delete what it returned, list the next page
// from the returned markers). No remaining version may be skipped. A
// generated version ID resumes at the position it occupied; "null" carries
// no position, so a "null" marker of a key without a null version resumes
// with the newest remaining version of that key (re-listing a version the
// caller kept is acceptable, losing older ones is not).
func testWalkVersionsDeletedMarker(e *env) {
	// setup creates key "k" with three versions, the middle one null when
	// nullMid, then key "l"; it returns them in walk order.
	setup := func(bucket string, nullMid bool) []vref {
		e.t.Helper()
		e.mkVersioned(bucket, storage.VersioningEnabled)
		var h []vref
		add := func(info storage.ObjectInfo) {
			h = append([]vref{{Key: "k", VersionID: info.VersionID}}, h...)
		}
		add(e.put(bucket, "k", []byte("v1")))
		if nullMid {
			e.setVersioning(bucket, storage.VersioningSuspended)
			add(e.put(bucket, "k", []byte("null")))
			e.setVersioning(bucket, storage.VersioningEnabled)
		} else {
			add(e.put(bucket, "k", []byte("v2")))
		}
		add(e.put(bucket, "k", []byte("v3")))
		h[0].IsLatest = true
		l := e.put(bucket, "l", []byte("l"))
		return append(h, vref{Key: "l", VersionID: l.VersionID, IsLatest: true})
	}
	latest := func(v vref) vref { v.IsLatest = true; return v }
	check := func(bucket, km, vm string, want []vref) {
		e.t.Helper()
		if got := refsOf(e.walkVersions(bucket, "", km, vm)); !slices.Equal(got, want) {
			e.t.Errorf("%s: WalkVersions(keyMarker %q, versionIDMarker %q):\n got  %v\n want %v", bucket, km, vm, got, want)
		}
		var wantK []vref
		for _, v := range want {
			if v.Key == km {
				wantK = append(wantK, v)
			}
		}
		if got := refsOf(e.walkVersions(bucket, km, km, vm)); !slices.Equal(got, wantK) {
			e.t.Errorf("%s: WalkVersions(prefix %q, keyMarker %q, versionIDMarker %q):\n got  %v\n want %v", bucket, km, km, vm, got, wantK)
		}
		res, err := storage.ListObjectVersions(e.ctx, e.b, bucket, storage.VersionListParams{KeyMarker: km, VersionIDMarker: vm})
		e.noErr("ListObjectVersions", err)
		if got := refsOf(res.Versions); !slices.Equal(got, want) || res.IsTruncated {
			e.t.Errorf("%s: ListObjectVersions(keyMarker %q, versionIDMarker %q):\n got  %v (truncated %v)\n want %v",
				bucket, km, vm, got, res.IsTruncated, want)
		}
	}

	for _, nullMid := range []bool{true, false} {
		kind := "generated"
		if nullMid {
			kind = "null"
		}

		// The page's versions, including the marker, are deleted.
		bucket := kind + "-page"
		h := setup(bucket, nullMid)
		res, err := storage.ListObjectVersions(e.ctx, e.b, bucket, storage.VersionListParams{MaxKeys: 2})
		e.noErr("ListObjectVersions", err)
		if got := refsOf(res.Versions); !slices.Equal(got, h[:2]) || !res.IsTruncated ||
			res.NextKeyMarker != "k" || res.NextVersionIDMarker != h[1].VersionID {
			e.t.Fatalf("%s: first page = %v truncated %v next %q/%q, want %v true k/%q",
				bucket, got, res.IsTruncated, res.NextKeyMarker, res.NextVersionIDMarker, h[:2], h[1].VersionID)
		}
		e.del(bucket, "k", h[0].VersionID)
		e.del(bucket, "k", h[1].VersionID)
		check(bucket, "k", h[1].VersionID, []vref{latest(h[2]), h[3]})

		// Only the marker version is deleted.
		bucket = kind + "-kept"
		h = setup(bucket, nullMid)
		e.del(bucket, "k", h[1].VersionID)
		if nullMid {
			check(bucket, "k", storage.NullVersionID, []vref{h[0], h[2], h[3]})
		} else {
			check(bucket, "k", h[1].VersionID, []vref{h[2], h[3]})
		}
	}

	// The emptying loop removes everything whatever the page size.
	for n := 1; n <= 4; n++ {
		bucket := fmt.Sprintf("empty-%d", n)
		setup(bucket, true)
		var p storage.VersionListParams
		p.MaxKeys = n
		for pages := 0; ; pages++ {
			if pages > 10 {
				e.t.Fatalf("%s: emptying loop does not terminate", bucket)
			}
			res, err := storage.ListObjectVersions(e.ctx, e.b, bucket, p)
			e.noErr("ListObjectVersions", err)
			for _, v := range res.Versions {
				e.del(bucket, v.Key, v.VersionID)
			}
			if !res.IsTruncated {
				break
			}
			p.KeyMarker, p.VersionIDMarker = res.NextKeyMarker, res.NextVersionIDMarker
		}
		if left := refsOf(e.walkVersions(bucket, "", "", "")); len(left) != 0 {
			e.t.Errorf("%s: deleting every listed version page by page (MaxKeys %d) left %v", bucket, n, left)
		}
	}
}

// testWalkVersionsLatestUnderMutation removes a key's delete marker (an S3
// "undelete") right after the walk delivered it, at various positions
// relative to the backend's internal batch boundaries. Consumers such as
// the lifecycle worker rely on IsLatest marking at most the first entry
// delivered for a key: a later entry of the same key must never be reported
// as latest, or two versions of one key would both claim to be current.
func testWalkVersionsLatestUnderMutation(e *env) {
	for n := 0; n <= 20; n++ {
		bucket := fmt.Sprintf("b%02d", n)
		e.mkVersioned(bucket, storage.VersioningEnabled)
		for i := range n {
			e.put(bucket, fmt.Sprintf("a%02d", i), []byte("x"))
		}
		e.put(bucket, "k", []byte("v1"))
		e.put(bucket, "k", []byte("v2"))
		e.del(bucket, "k", "")
		var got []storage.ObjectInfo
		err := e.b.WalkVersions(e.ctx, bucket, "", "", "", func(o storage.ObjectInfo) error {
			got = append(got, o)
			if o.Key == "k" && o.DeleteMarker {
				if _, err := e.b.DeleteObject(e.ctx, bucket, "k", o.VersionID); err != nil {
					return err
				}
			}
			return nil
		})
		e.noErr("WalkVersions", err)
		for i, o := range got {
			if o.IsLatest && i > 0 && got[i-1].Key == o.Key {
				e.t.Errorf("%d filler keys: WalkVersions reported %s as latest after an earlier entry of the same key:\n%v",
					n, refsOf([]storage.ObjectInfo{o})[0], refsOf(got))
			}
		}
	}
}
