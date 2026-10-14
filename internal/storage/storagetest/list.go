package storagetest

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// listTests exercise the generic listing helpers of list.go
// (storage.ListObjects, storage.ListObjectVersions, storage.ComputeBucketUsage)
// on top of the backend's walks. Expected results come from a small
// reference model of S3 listing semantics, independent of the helpers.
func listTests() []testCase {
	return []testCase{
		{"EmptyBucket", testListEmpty},
		{"ObjectsDelimiterGrouping", testListObjectsGrouping},
		{"ObjectsPrefixDelimiterAfterMatrix", testListObjectsMatrix},
		{"ObjectsPagination", testListObjectsPagination},
		{"ObjectsAfterCommonPrefix", testListObjectsAfterCommonPrefix},
		{"ObjectsDefaultMaxKeys", testListObjectsDefaultMaxKeys},
		{"VersionsDelimiterGrouping", testListVersionsGrouping},
		{"VersionsMarkers", testListVersionsMarkers},
		{"VersionsPagination", testListVersionsPagination},
		{"BucketUsage", testListBucketUsage},
	}
}

// listFixture records the version history it creates in an Enabled bucket
// so expected listings can be computed without consulting the backend.
type listFixture struct {
	e      *env
	bucket string
	hist   map[string][]vref // newest first
}

func newListFixture(e *env, bucket string) *listFixture {
	e.t.Helper()
	e.mkVersioned(bucket, storage.VersioningEnabled)
	return &listFixture{e: e, bucket: bucket, hist: map[string][]vref{}}
}

func (f *listFixture) put(key string) {
	f.e.t.Helper()
	info := f.e.put(f.bucket, key, []byte("data of "+key))
	f.hist[key] = append([]vref{{Key: key, VersionID: info.VersionID}}, f.hist[key]...)
}

func (f *listFixture) del(key string) {
	f.e.t.Helper()
	res := f.e.del(f.bucket, key, "")
	f.hist[key] = append([]vref{{Key: key, VersionID: res.VersionID, DeleteMarker: true}}, f.hist[key]...)
}

// versions returns every recorded version: keys ascending, newest first.
func (f *listFixture) versions() []vref {
	keys := make([]string, 0, len(f.hist))
	for k := range f.hist {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []vref
	for _, k := range keys {
		for i, v := range f.hist[k] {
			v.IsLatest = i == 0
			out = append(out, v)
		}
	}
	return out
}

// latestKeys returns the sorted keys whose latest version is an object.
func (f *listFixture) latestKeys() []string {
	var out []string
	for k, h := range f.hist {
		if !h[0].DeleteMarker {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// standardListFixture builds a bucket whose keys exercise delimiter
// grouping: nested "directories", empty path segments, multi-character
// delimiters, multi-byte UTF-8, keys hidden behind delete markers (which
// must not produce common prefixes in ListObjects) and keys with several
// versions.
func standardListFixture(e *env, bucket string) *listFixture {
	e.t.Helper()
	f := newListFixture(e, bucket)
	for _, k := range []string{
		"a", "a/1", "a/2", "a/b/c", "a/b/d", "a-b", "a0", "b", "c/", "c//d", "c/d/e",
		"d--x--y", "d--z", "e/é/1", "e/日本/2", "f", "z/", "gone/x", "mixed/live", "mixed/dead",
	} {
		f.put(k)
	}
	f.put("a/2")        // second version
	f.put("f")          // second version
	f.del("gone/x")     // the only key under gone/ is hidden
	f.del("mixed/dead") // mixed/ still has a live key
	f.del("h/marker")   // delete marker without any object version
	f.del("f")          // f is hidden but has two older versions
	f.put("a/1")        // second version
	return f
}

// --- reference model ---

// listEntry is one entry of a listing page: an object key, a version, or
// a common prefix.
type listEntry struct {
	cp  bool
	key string // object key or common prefix
	ver vref   // for version listings (cp == false)
}

func (le listEntry) String() string {
	if le.cp {
		return fmt.Sprintf("CP(%q)", le.key)
	}
	if le.ver.VersionID != "" {
		return le.ver.String()
	}
	return fmt.Sprintf("%q", le.key)
}

// refCommonPrefix is an independent implementation of S3 common prefix
// roll-up: the key's remainder after prefix is cut after the first
// occurrence of delimiter.
func refCommonPrefix(key, prefix, delimiter string) (string, bool) {
	if delimiter == "" || !strings.HasPrefix(key, prefix) {
		return "", false
	}
	rest := key[len(prefix):]
	if i := strings.Index(rest, delimiter); i >= 0 {
		return prefix + rest[:i+len(delimiter)], true
	}
	return "", false
}

// rollUp groups keys into common prefixes; it relies on keys with the same
// common prefix being contiguous in byte-wise order.
func rollUp(in []listEntry, prefix, delimiter string) []listEntry {
	var out []listEntry
	lastCP := ""
	for _, le := range in {
		if cp, ok := refCommonPrefix(le.key, prefix, delimiter); ok {
			if cp != lastCP {
				out = append(out, listEntry{cp: true, key: cp})
				lastCP = cp
			}
			continue
		}
		out = append(out, le)
	}
	return out
}

// modelListObjects returns the complete (untruncated) ListObjects result
// for sorted latest keys. When after is itself a common prefix of the
// listing everything under it is skipped (S3 NextMarker semantics).
func modelListObjects(keys []string, prefix, delimiter, after string) []listEntry {
	skip := ""
	if cp, ok := refCommonPrefix(after, prefix, delimiter); ok && cp == after {
		skip = cp
	}
	var in []listEntry
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || k <= after || (skip != "" && strings.HasPrefix(k, skip)) {
			continue
		}
		in = append(in, listEntry{key: k})
	}
	return rollUp(in, prefix, delimiter)
}

// modelListVersions returns the complete ListObjectVersions result for all
// versions (keys ascending, newest first) with S3 marker semantics. A
// vidMarker that is not a version of keyMarker includes every version of
// keyMarker, which is what the contract requires of a "null" marker whose
// null version was deleted; deleted generated IDs, which backends may
// resolve by position, are covered by testWalkVersionsDeletedMarker instead.
func modelListVersions(all []vref, prefix, delimiter, keyMarker, vidMarker string) []listEntry {
	skip := ""
	if cp, ok := refCommonPrefix(keyMarker, prefix, delimiter); ok && cp == keyMarker {
		skip = cp
	}
	markerIdx := -1
	if keyMarker != "" && vidMarker != "" && skip == "" {
		for i, v := range all {
			if v.Key == keyMarker && v.VersionID == vidMarker {
				markerIdx = i
			}
		}
	}
	var in []listEntry
	for i, v := range all {
		if !strings.HasPrefix(v.Key, prefix) {
			continue
		}
		switch {
		case keyMarker == "":
		case skip != "":
			if v.Key <= keyMarker || strings.HasPrefix(v.Key, skip) {
				continue
			}
		case vidMarker == "":
			if v.Key <= keyMarker {
				continue
			}
		default:
			if v.Key < keyMarker || (v.Key == keyMarker && i <= markerIdx) {
				continue
			}
		}
		in = append(in, listEntry{key: v.Key, ver: v})
	}
	return rollUp(in, prefix, delimiter)
}

// insideCommonPrefix reports whether marker lies strictly inside (not at)
// a common prefix of the listing. The list.go contract documents markers
// that are keys or common prefixes (what NextAfter/NextKeyMarker return);
// for a marker strictly inside a common prefix the doc ("strictly greater
// than After") and S3 (which reports the prefix again) disagree, so such
// markers are not asserted.
func insideCommonPrefix(marker, prefix, delimiter string) bool {
	cp, ok := refCommonPrefix(marker, prefix, delimiter)
	return ok && cp != marker
}

// --- page checks ---

func (e *env) gotObjectEntries(res storage.ListResult) (objs []string, cps []string) {
	e.t.Helper()
	for _, o := range res.Objects {
		if !o.IsLatest || o.DeleteMarker || o.Bucket == "" {
			e.t.Errorf("ListObjects returned %q with IsLatest=%v DeleteMarker=%v Bucket=%q, want a latest object version",
				o.Key, o.IsLatest, o.DeleteMarker, o.Bucket)
		}
	}
	return keysOf(res.Objects), res.CommonPrefixes
}

func splitEntries(entries []listEntry) (keys []string, vers []vref, cps []string) {
	for _, le := range entries {
		switch {
		case le.cp:
			cps = append(cps, le.key)
		default:
			keys = append(keys, le.key)
			vers = append(vers, le.ver)
		}
	}
	return
}

// eqStrings compares string slices treating nil and empty as equal.
func eqStrings(a, b []string) bool { return len(a) == len(b) && (len(a) == 0 || slices.Equal(a, b)) }

// checkListObjects issues one ListObjects call and compares the page with
// the first MaxKeys entries of want (the model listing from p.After).
// It returns the page's NextAfter and whether it was truncated.
func (e *env) checkListObjects(bucket string, p storage.ListParams, want []listEntry) (storage.ListResult, bool) {
	e.t.Helper()
	desc := fmt.Sprintf("ListObjects(prefix %q, delimiter %q, after %q, maxKeys %d)", p.Prefix, p.Delimiter, p.After, p.MaxKeys)
	res, err := storage.ListObjects(e.ctx, e.b, bucket, p)
	if err != nil {
		e.t.Errorf("%s: %v", desc, err)
		return res, false
	}
	limit := p.MaxKeys
	if limit <= 0 {
		limit = storage.DefaultMaxKeys
	}
	page := want[:min(limit, len(want))]
	wantTrunc := len(want) > limit
	wantKeys, _, wantCPs := splitEntries(page)
	gotKeys, gotCPs := e.gotObjectEntries(res)
	ok := true
	if !eqStrings(gotKeys, wantKeys) || !eqStrings(gotCPs, wantCPs) {
		e.t.Errorf("%s:\n got  objects %q prefixes %q\n want objects %q prefixes %q\n (model page %v)",
			desc, gotKeys, gotCPs, wantKeys, wantCPs, page)
		ok = false
	}
	wantNext := ""
	if wantTrunc {
		wantNext = page[len(page)-1].key
	}
	if res.IsTruncated != wantTrunc || res.NextAfter != wantNext {
		e.t.Errorf("%s: IsTruncated/NextAfter = %v/%q, want %v/%q", desc, res.IsTruncated, res.NextAfter, wantTrunc, wantNext)
		ok = false
	}
	return res, ok
}

// paginateObjects pages through a listing with maxKeys and checks every
// page and that the concatenation reproduces the full listing exactly.
func (e *env) paginateObjects(bucket string, p storage.ListParams, maxKeys int, full []listEntry) {
	e.t.Helper()
	p.MaxKeys = maxKeys
	pos := 0
	for pages := 0; ; pages++ {
		if pages > len(full)+1 {
			e.t.Errorf("ListObjects(prefix %q, delimiter %q, maxKeys %d): pagination does not terminate", p.Prefix, p.Delimiter, maxKeys)
			return
		}
		res, ok := e.checkListObjects(bucket, p, full[pos:])
		if !ok {
			return
		}
		pos += len(res.Objects) + len(res.CommonPrefixes)
		if !res.IsTruncated {
			break
		}
		p.After = res.NextAfter
	}
	if pos != len(full) {
		e.t.Errorf("ListObjects(prefix %q, delimiter %q, maxKeys %d): pages returned %d entries, want %d", p.Prefix, p.Delimiter, maxKeys, pos, len(full))
	}
}

func (e *env) checkListVersions(bucket string, p storage.VersionListParams, want []listEntry) (storage.VersionListResult, bool) {
	e.t.Helper()
	desc := fmt.Sprintf("ListObjectVersions(prefix %q, delimiter %q, keyMarker %q, versionIDMarker %q, maxKeys %d)",
		p.Prefix, p.Delimiter, p.KeyMarker, p.VersionIDMarker, p.MaxKeys)
	res, err := storage.ListObjectVersions(e.ctx, e.b, bucket, p)
	if err != nil {
		e.t.Errorf("%s: %v", desc, err)
		return res, false
	}
	limit := p.MaxKeys
	if limit <= 0 {
		limit = storage.DefaultMaxKeys
	}
	page := want[:min(limit, len(want))]
	wantTrunc := len(want) > limit
	_, wantVers, wantCPs := splitEntries(page)
	gotVers := refsOf(res.Versions)
	ok := true
	if len(gotVers) != len(wantVers) || (len(gotVers) > 0 && !slices.Equal(gotVers, wantVers)) || !eqStrings(res.CommonPrefixes, wantCPs) {
		e.t.Errorf("%s:\n got  versions %v prefixes %q\n want versions %v prefixes %q", desc, gotVers, res.CommonPrefixes, wantVers, wantCPs)
		ok = false
	}
	var wantKM, wantVM string
	if wantTrunc {
		last := page[len(page)-1]
		wantKM = last.key
		if !last.cp {
			wantVM = last.ver.VersionID
		}
	}
	if res.IsTruncated != wantTrunc || res.NextKeyMarker != wantKM || res.NextVersionIDMarker != wantVM {
		e.t.Errorf("%s: IsTruncated/NextKeyMarker/NextVersionIDMarker = %v/%q/%q, want %v/%q/%q",
			desc, res.IsTruncated, res.NextKeyMarker, res.NextVersionIDMarker, wantTrunc, wantKM, wantVM)
		ok = false
	}
	return res, ok
}

func (e *env) paginateVersions(bucket string, p storage.VersionListParams, maxKeys int, full []listEntry) {
	e.t.Helper()
	p.MaxKeys = maxKeys
	pos := 0
	for pages := 0; ; pages++ {
		if pages > len(full)+1 {
			e.t.Errorf("ListObjectVersions(prefix %q, delimiter %q, maxKeys %d): pagination does not terminate", p.Prefix, p.Delimiter, maxKeys)
			return
		}
		res, ok := e.checkListVersions(bucket, p, full[pos:])
		if !ok {
			return
		}
		pos += len(res.Versions) + len(res.CommonPrefixes)
		if !res.IsTruncated {
			break
		}
		p.KeyMarker, p.VersionIDMarker = res.NextKeyMarker, res.NextVersionIDMarker
	}
	if pos != len(full) {
		e.t.Errorf("ListObjectVersions(prefix %q, delimiter %q, maxKeys %d): pages returned %d entries, want %d", p.Prefix, p.Delimiter, maxKeys, pos, len(full))
	}
}

// --- tests ---

var (
	listPrefixes   = []string{"", "a", "a/", "a/b/", "c/", "c//", "d-", "e/", "gone/", "mixed/", "h", "nomatch", "z/"}
	listDelimiters = []string{"", "/", "--", "-", "é", "b"}
)

func testListEmpty(e *env) {
	e.mkBucket("b")
	res, err := storage.ListObjects(e.ctx, e.b, "b", storage.ListParams{Delimiter: "/", MaxKeys: 10})
	e.noErr("ListObjects on an empty bucket", err)
	if len(res.Objects) != 0 || len(res.CommonPrefixes) != 0 || res.IsTruncated || res.NextAfter != "" {
		e.t.Errorf("ListObjects on an empty bucket = %+v, want an empty, untruncated result", res)
	}
	vres, err := storage.ListObjectVersions(e.ctx, e.b, "b", storage.VersionListParams{Delimiter: "/", MaxKeys: 10})
	e.noErr("ListObjectVersions on an empty bucket", err)
	if len(vres.Versions) != 0 || len(vres.CommonPrefixes) != 0 || vres.IsTruncated || vres.NextKeyMarker != "" || vres.NextVersionIDMarker != "" {
		e.t.Errorf("ListObjectVersions on an empty bucket = %+v, want an empty, untruncated result", vres)
	}
}

func testListObjectsGrouping(e *env) {
	standardListFixture(e, "b")
	// Hand-written expectations for the most common shapes (the matrix test
	// covers the rest through the model).
	for _, tc := range []struct {
		prefix, delimiter string
		objects, prefixes []string
	}{
		{"", "/", []string{"a", "a-b", "a0", "b", "d--x--y", "d--z"}, []string{"a/", "c/", "e/", "mixed/", "z/"}},
		{"a/", "/", []string{"a/1", "a/2"}, []string{"a/b/"}},
		{"c/", "/", []string{"c/"}, []string{"c//", "c/d/"}},
		{"c//", "/", []string{"c//d"}, nil},
		{"", "--", []string{"a", "a-b", "a/1", "a/2", "a/b/c", "a/b/d", "a0", "b", "c/", "c//d", "c/d/e", "e/é/1", "e/日本/2", "mixed/live", "z/"}, []string{"d--"}},
		{"d--", "--", []string{"d--z"}, []string{"d--x--"}},
		{"d--", "", []string{"d--x--y", "d--z"}, nil},
		{"e/", "é", []string{"e/日本/2"}, []string{"e/é"}},
		{"gone/", "/", nil, nil},
		{"mixed/", "/", []string{"mixed/live"}, nil},
		// Every key starting with "a" rolls up into the common prefix "a".
		{"", "a", []string{"b", "c/", "c//d", "c/d/e", "d--x--y", "d--z", "e/é/1", "e/日本/2", "mixed/live", "z/"}, []string{"a"}},
	} {
		res, err := storage.ListObjects(e.ctx, e.b, "b", storage.ListParams{Prefix: tc.prefix, Delimiter: tc.delimiter})
		e.noErr("ListObjects", err)
		gotKeys, gotCPs := e.gotObjectEntries(res)
		if !eqStrings(gotKeys, tc.objects) || !eqStrings(gotCPs, tc.prefixes) || res.IsTruncated {
			e.t.Errorf("ListObjects(prefix %q, delimiter %q):\n got  objects %q prefixes %q truncated %v\n want objects %q prefixes %q",
				tc.prefix, tc.delimiter, gotKeys, gotCPs, res.IsTruncated, tc.objects, tc.prefixes)
		}
	}
}

func testListObjectsMatrix(e *env) {
	f := standardListFixture(e, "b")
	keys := f.latestKeys()
	afters := []string{"", "0", "a", "a/", "a/1", "a/b/", "a0", "b", "c/", "c//", "d--", "d--x--", "e/", "gone/", "mixed/", "zzz", "\U0010FFFF"}
	for _, prefix := range listPrefixes {
		for _, delim := range listDelimiters {
			for _, after := range afters {
				if insideCommonPrefix(after, prefix, delim) {
					continue
				}
				want := modelListObjects(keys, prefix, delim, after)
				e.checkListObjects("b", storage.ListParams{Prefix: prefix, Delimiter: delim, After: after, MaxKeys: 1000}, want)
				if len(want) > 0 {
					// The same listing truncated at one entry.
					e.checkListObjects("b", storage.ListParams{Prefix: prefix, Delimiter: delim, After: after, MaxKeys: 1}, want)
				}
			}
		}
	}
}

func testListObjectsPagination(e *env) {
	f := standardListFixture(e, "b")
	keys := f.latestKeys()
	for _, prefix := range listPrefixes {
		for _, delim := range listDelimiters {
			full := modelListObjects(keys, prefix, delim, "")
			limits := []int{1, 2, 3, len(full) - 1, len(full), len(full) + 1}
			for _, n := range limits {
				if n <= 0 {
					continue
				}
				e.paginateObjects("b", storage.ListParams{Prefix: prefix, Delimiter: delim}, n, full)
			}
		}
	}
	// MaxKeys counts common prefixes: three entries = "a", "a-b" and "a/"
	// ("-" sorts before "/").
	res, err := storage.ListObjects(e.ctx, e.b, "b", storage.ListParams{Prefix: "a", Delimiter: "/", MaxKeys: 3})
	e.noErr("ListObjects", err)
	if !slices.Equal(keysOf(res.Objects), []string{"a", "a-b"}) || !slices.Equal(res.CommonPrefixes, []string{"a/"}) ||
		!res.IsTruncated || res.NextAfter != "a/" {
		e.t.Errorf("ListObjects(prefix a, delimiter /, maxKeys 3) = objects %q prefixes %q truncated %v next %q; want [a a-b] [a/] true \"a/\"",
			keysOf(res.Objects), res.CommonPrefixes, res.IsTruncated, res.NextAfter)
	}
}

// testListObjectsAfterCommonPrefix: when After is a common prefix (as a
// truncated page's NextAfter is), nothing under it is listed again.
func testListObjectsAfterCommonPrefix(e *env) {
	e.mkBucket("b")
	for i := range 30 {
		e.put("b", fmt.Sprintf("dir/%02d", i), nil)
	}
	e.put("b", "dir", nil)
	e.put("b", "dir0", nil)
	e.put("b", "dis", nil)
	res, err := storage.ListObjects(e.ctx, e.b, "b", storage.ListParams{Delimiter: "/", After: "dir/"})
	e.noErr("ListObjects", err)
	if got := keysOf(res.Objects); !slices.Equal(got, []string{"dir0", "dis"}) || len(res.CommonPrefixes) != 0 {
		e.t.Errorf("ListObjects(delimiter /, after \"dir/\") = objects %q prefixes %q, want [dir0 dis] and no prefixes", got, res.CommonPrefixes)
	}
	// Without a delimiter "dir/" is just a key position.
	res, err = storage.ListObjects(e.ctx, e.b, "b", storage.ListParams{After: "dir/", MaxKeys: 3})
	e.noErr("ListObjects", err)
	if got := keysOf(res.Objects); !slices.Equal(got, []string{"dir/00", "dir/01", "dir/02"}) || !res.IsTruncated || res.NextAfter != "dir/02" {
		e.t.Errorf("ListObjects(after \"dir/\", maxKeys 3) = %q truncated %v next %q, want [dir/00 dir/01 dir/02] true \"dir/02\"", got, res.IsTruncated, res.NextAfter)
	}
	// The same for versions: KeyMarker equal to a common prefix.
	vres, err := storage.ListObjectVersions(e.ctx, e.b, "b", storage.VersionListParams{Delimiter: "/", KeyMarker: "dir/", VersionIDMarker: "ignored"})
	e.noErr("ListObjectVersions", err)
	if got := keysOf(vres.Versions); !slices.Equal(got, []string{"dir0", "dis"}) || len(vres.CommonPrefixes) != 0 {
		e.t.Errorf("ListObjectVersions(delimiter /, keyMarker \"dir/\") = %q prefixes %q, want [dir0 dis] and no prefixes", got, vres.CommonPrefixes)
	}
}

// testListObjectsDefaultMaxKeys: MaxKeys <= 0 means DefaultMaxKeys.
func testListObjectsDefaultMaxKeys(e *env) {
	e.mkBucket("b")
	for i := range 25 {
		e.put("b", fmt.Sprintf("k%02d", i), nil)
	}
	for _, mk := range []int{0, -1} {
		res, err := storage.ListObjects(e.ctx, e.b, "b", storage.ListParams{MaxKeys: mk})
		e.noErr("ListObjects", err)
		if len(res.Objects) != 25 || res.IsTruncated || res.NextAfter != "" {
			e.t.Errorf("ListObjects(maxKeys %d) returned %d objects truncated %v next %q, want 25 untruncated", mk, len(res.Objects), res.IsTruncated, res.NextAfter)
		}
		vres, err := storage.ListObjectVersions(e.ctx, e.b, "b", storage.VersionListParams{MaxKeys: mk})
		e.noErr("ListObjectVersions", err)
		if len(vres.Versions) != 25 || vres.IsTruncated {
			e.t.Errorf("ListObjectVersions(maxKeys %d) returned %d versions truncated %v, want 25 untruncated", mk, len(vres.Versions), vres.IsTruncated)
		}
	}
}

func testListVersionsGrouping(e *env) {
	f := standardListFixture(e, "b")
	all := f.versions()
	// Delete-marker-only keys still produce common prefixes in version
	// listings (S3 lists delete markers there).
	res, err := storage.ListObjectVersions(e.ctx, e.b, "b", storage.VersionListParams{Delimiter: "/"})
	e.noErr("ListObjectVersions", err)
	wantCPs := []string{"a/", "c/", "e/", "gone/", "h/", "mixed/", "z/"}
	if !slices.Equal(res.CommonPrefixes, wantCPs) {
		e.t.Errorf("ListObjectVersions(delimiter /) prefixes = %q, want %q", res.CommonPrefixes, wantCPs)
	}
	var wantVers []vref
	for _, v := range all {
		if !strings.Contains(v.Key, "/") {
			wantVers = append(wantVers, v)
		}
	}
	if got := refsOf(res.Versions); !slices.Equal(got, wantVers) {
		e.t.Errorf("ListObjectVersions(delimiter /) versions:\n got  %v\n want %v", got, wantVers)
	}
	for _, v := range res.Versions {
		if v.Bucket != "b" || v.LastModified.IsZero() {
			e.t.Errorf("ListObjectVersions entry %s@%s: Bucket %q LastModified %s", v.Key, v.VersionID, v.Bucket, fmtTime(v.LastModified))
		}
	}
	for _, prefix := range listPrefixes {
		for _, delim := range listDelimiters {
			e.checkListVersions("b", storage.VersionListParams{Prefix: prefix, Delimiter: delim}, modelListVersions(all, prefix, delim, "", ""))
		}
	}
}

func testListVersionsMarkers(e *env) {
	f := standardListFixture(e, "b")
	all := f.versions()
	for _, prefix := range []string{"", "a", "a/", "f", "mixed/"} {
		for _, delim := range []string{"", "/"} {
			// Key markers alone, including keys that do not exist and common prefixes.
			for _, km := range []string{"", "0", "a", "a/", "a/1", "a/2", "b", "f", "gone/", "gone/x", "h/", "zzz"} {
				if insideCommonPrefix(km, prefix, delim) {
					continue
				}
				e.checkListVersions("b", storage.VersionListParams{Prefix: prefix, Delimiter: delim, KeyMarker: km},
					modelListVersions(all, prefix, delim, km, ""))
			}
			// Every version as a key + version ID marker.
			for _, v := range all {
				if insideCommonPrefix(v.Key, prefix, delim) {
					continue
				}
				p := storage.VersionListParams{Prefix: prefix, Delimiter: delim, KeyMarker: v.Key, VersionIDMarker: v.VersionID}
				e.checkListVersions("b", p, modelListVersions(all, prefix, delim, v.Key, v.VersionID))
				p.MaxKeys = 2
				e.checkListVersions("b", p, modelListVersions(all, prefix, delim, v.Key, v.VersionID))
			}
		}
	}
}

func testListVersionsPagination(e *env) {
	f := standardListFixture(e, "b")
	all := f.versions()
	for _, prefix := range listPrefixes {
		for _, delim := range listDelimiters {
			full := modelListVersions(all, prefix, delim, "", "")
			for _, n := range []int{1, 2, 3, len(full) - 1, len(full), len(full) + 1} {
				if n <= 0 {
					continue
				}
				e.paginateVersions("b", storage.VersionListParams{Prefix: prefix, Delimiter: delim}, n, full)
			}
		}
	}
	// Pagination that stops in the middle of a key's versions resumes with
	// the next older version of that key.
	res, err := storage.ListObjectVersions(e.ctx, e.b, "b", storage.VersionListParams{Prefix: "f", MaxKeys: 2})
	e.noErr("ListObjectVersions", err)
	h := f.hist["f"]
	if !res.IsTruncated || res.NextKeyMarker != "f" || res.NextVersionIDMarker != h[1].VersionID {
		e.t.Errorf("ListObjectVersions(prefix f, maxKeys 2): truncated %v next %q/%q, want true f/%q",
			res.IsTruncated, res.NextKeyMarker, res.NextVersionIDMarker, h[1].VersionID)
	}
}

func testListBucketUsage(e *env) {
	e.mkBucket("b")
	u, err := storage.ComputeBucketUsage(e.ctx, e.b, "b")
	e.noErr("ComputeBucketUsage(empty)", err)
	if u != (storage.BucketUsage{}) {
		e.t.Errorf("ComputeBucketUsage(empty) = %+v, want zero", u)
	}
	e.put("b", "one", []byte("123"))   // null version, becomes noncurrent
	e.put("b", "two", []byte("12345")) // null version, hidden by a marker
	e.put("b", "three", nil)           // empty current object
	e.setVersioning("b", storage.VersioningEnabled)
	e.put("b", "one", []byte("1234567"))
	e.del("b", "two", "")
	e.del("b", "ghost", "")
	e.createUpload("b", "up1", storage.ObjectMeta{})
	up := e.createUpload("b", "up2", storage.ObjectMeta{})
	e.part("b", "up2", up.UploadID, 1, []byte("part data"))
	e.mkBucket("other")
	e.put("other", "x", []byte("not counted"))

	u, err = storage.ComputeBucketUsage(e.ctx, e.b, "b")
	e.noErr("ComputeBucketUsage", err)
	want := storage.BucketUsage{Objects: 2, Bytes: 7, StoredBytes: 7, Versions: 4, VersionBytes: 15, StoredVersionBytes: 15,
		DeleteMarkers: 2, PendingUploads: 2}
	if u != want {
		e.t.Errorf("ComputeBucketUsage = %+v, want %+v", u, want)
	}
}
