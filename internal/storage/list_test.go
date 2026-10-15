package storage_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

var ctx = context.Background()

// fakeBackend is a deliberately simple reference implementation of the two
// walk methods (plus ListMultipartUploads) used by the list helpers. It
// counts how many items it yields so tests can verify that common prefixes
// are skipped instead of enumerated.
type fakeBackend struct {
	storage.Backend // unused methods panic

	keys     []string                        // sorted
	versions map[string][]storage.ObjectInfo // newest first
	uploads  int
	walkErr  error
	yielded  int
	walks    int
}

func newFake() *fakeBackend { return &fakeBackend{versions: map[string][]storage.ObjectInfo{}} }

// add adds a version (newest so far) for key.
func (f *fakeBackend) add(key string, size int64, deleteMarker bool) string {
	if _, ok := f.versions[key]; !ok {
		i := sort.SearchStrings(f.keys, key)
		f.keys = slices.Insert(f.keys, i, key)
	}
	vs := f.versions[key]
	id := fmt.Sprintf("v%03d", len(vs))
	for i := range vs {
		vs[i].IsLatest = false
	}
	v := storage.ObjectInfo{Key: key, VersionID: id, IsLatest: true, DeleteMarker: deleteMarker, Size: size}
	f.versions[key] = append([]storage.ObjectInfo{v}, vs...)
	return id
}

func (f *fakeBackend) addKeys(keys ...string) *fakeBackend {
	for _, k := range keys {
		f.add(k, int64(len(k)), false)
	}
	return f
}

func (f *fakeBackend) WalkObjects(_ context.Context, _ string, prefix, after string, fn func(storage.ObjectInfo) error) error {
	f.walks++
	if f.walkErr != nil {
		return f.walkErr
	}
	i := sort.Search(len(f.keys), func(i int) bool { return f.keys[i] > after && f.keys[i] >= prefix })
	for ; i < len(f.keys) && strings.HasPrefix(f.keys[i], prefix); i++ {
		v := f.versions[f.keys[i]][0]
		if v.DeleteMarker {
			continue
		}
		f.yielded++
		if err := fn(v); err != nil {
			if errors.Is(err, storage.ErrStopWalk) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (f *fakeBackend) WalkVersions(_ context.Context, _ string, prefix, keyMarker, vidMarker string, fn func(storage.ObjectInfo) error) error {
	f.walks++
	if f.walkErr != nil {
		return f.walkErr
	}
	started := keyMarker == ""
	for _, k := range f.keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		for _, v := range f.versions[k] {
			if !started {
				switch {
				case k < keyMarker:
					continue
				case k == keyMarker:
					if vidMarker == "" {
						continue
					}
					if v.VersionID == vidMarker {
						started = true
						continue
					}
					if v.VersionID > vidMarker { // ids grow with age reversed: newer = larger
						continue
					}
				}
				started = true
			}
			f.yielded++
			if err := fn(v); err != nil {
				if errors.Is(err, storage.ErrStopWalk) {
					return nil
				}
				return err
			}
		}
	}
	return nil
}

func (f *fakeBackend) ListMultipartUploads(context.Context, string) ([]storage.UploadInfo, error) {
	return make([]storage.UploadInfo, f.uploads), nil
}

// entry is one listing element: an object/version or a common prefix.
type entry struct {
	key, vid string
	cp       bool
}

func objEntries(r storage.ListResult) []entry {
	var out []entry
	// Merge objects and prefixes in key order (they never compare equal).
	i, j := 0, 0
	for i < len(r.Objects) || j < len(r.CommonPrefixes) {
		if j == len(r.CommonPrefixes) || (i < len(r.Objects) && r.Objects[i].Key < r.CommonPrefixes[j]) {
			out = append(out, entry{key: r.Objects[i].Key})
			i++
		} else {
			out = append(out, entry{key: r.CommonPrefixes[j], cp: true})
			j++
		}
	}
	return out
}

// referenceList computes the full (unpaginated) listing straight from the
// definition: every current key with the prefix that is > after, rolled up
// into common prefixes, except that a common prefix equal to after is
// skipped entirely.
func referenceList(f *fakeBackend, prefix, delim, after string) []entry {
	var out []entry
	seen := map[string]bool{}
	for _, k := range f.keys {
		if !strings.HasPrefix(k, prefix) || k <= after || f.versions[k][0].DeleteMarker {
			continue
		}
		if cp, ok := storage.CommonPrefix(k, prefix, delim); ok {
			if cp == after || seen[cp] {
				continue
			}
			seen[cp] = true
			out = append(out, entry{key: cp, cp: true})
			continue
		}
		out = append(out, entry{key: k})
	}
	return out
}

// paginate lists everything with MaxKeys = pageSize and returns the
// concatenated entries, checking per-page invariants.
func paginate(t *testing.T, b storage.Backend, prefix, delim, after string, pageSize int) []entry {
	t.Helper()
	var all []entry
	for page := 0; ; page++ {
		if page > 10000 {
			t.Fatal("pagination does not terminate")
		}
		res, err := storage.ListObjects(ctx, b, "bkt", storage.ListParams{Prefix: prefix, Delimiter: delim, After: after, MaxKeys: pageSize})
		if err != nil {
			t.Fatal(err)
		}
		es := objEntries(res)
		if len(es) > pageSize {
			t.Fatalf("page has %d entries > MaxKeys %d", len(es), pageSize)
		}
		all = append(all, es...)
		if !res.IsTruncated {
			if res.NextAfter != "" {
				t.Fatalf("NextAfter %q set on final page", res.NextAfter)
			}
			return all
		}
		if len(es) != pageSize {
			t.Fatalf("truncated page has %d entries, want %d", len(es), pageSize)
		}
		if res.NextAfter != es[len(es)-1].key {
			t.Fatalf("NextAfter = %q, want last entry %q", res.NextAfter, es[len(es)-1].key)
		}
		after = res.NextAfter
	}
}

func TestCommonPrefix(t *testing.T) {
	cases := []struct {
		key, prefix, delim, want string
		ok                       bool
	}{
		{"a/b/c", "", "/", "a/", true},
		{"a/b/c", "a/", "/", "a/b/", true},
		{"a/b/c", "a", "/", "a/", true},
		{"a/b/c", "a/b/", "/", "", false},
		{"/x", "", "/", "/", true},
		{"//x", "/", "/", "//", true},
		{"abc", "", "", "", false},
		{"abc", "x", "/", "", false},
		{"a--b--c", "", "--", "a--", true},
		{"a-b--c", "a-", "--", "a-b--", true},
		{"photos日2024日x", "photos", "日", "photos日", true},
		{"photos日2024日x", "photos日", "日", "photos日2024日", true},
		{"abab", "ab", "ab", "abab", true},
	}
	for _, tc := range cases {
		got, ok := storage.CommonPrefix(tc.key, tc.prefix, tc.delim)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CommonPrefix(%q,%q,%q) = %q,%v want %q,%v", tc.key, tc.prefix, tc.delim, got, ok, tc.want, tc.ok)
		}
	}
}

func TestListObjectsTable(t *testing.T) {
	f := newFake().addKeys("a/b/c", "a/b/d", "a/e", "a.txt", "b", "c/", "c/d", "d-1-x", "d-2", "/root", "//double", "日本/語", "日本語")
	f.add("gone/x", 1, false)
	f.add("gone/x", 0, true) // only key under gone/ is deleted: no CommonPrefix

	type want struct {
		objects []string
		cps     []string
		trunc   bool
		next    string
	}
	cases := []struct {
		name string
		p    storage.ListParams
		want want
	}{
		{"no delimiter", storage.ListParams{}, want{objects: []string{"//double", "/root", "a.txt", "a/b/c", "a/b/d", "a/e", "b", "c/", "c/d", "d-1-x", "d-2", "日本/語", "日本語"}}},
		{"slash root", storage.ListParams{Delimiter: "/"}, want{objects: []string{"a.txt", "b", "d-1-x", "d-2", "日本語"}, cps: []string{"/", "a/", "c/", "日本/"}}},
		{"prefix at delimiter", storage.ListParams{Prefix: "a/", Delimiter: "/"}, want{objects: []string{"a/e"}, cps: []string{"a/b/"}}},
		{"prefix not ending at delimiter", storage.ListParams{Prefix: "a", Delimiter: "/"}, want{objects: []string{"a.txt"}, cps: []string{"a/"}}},
		{"prefix equals key", storage.ListParams{Prefix: "c/", Delimiter: "/"}, want{objects: []string{"c/", "c/d"}}},
		{"delimiter at start of key", storage.ListParams{Prefix: "/", Delimiter: "/"}, want{objects: []string{"/root"}, cps: []string{"//"}}},
		{"dash delimiter", storage.ListParams{Prefix: "d", Delimiter: "-"}, want{cps: []string{"d-"}}},
		{"dash delimiter deeper", storage.ListParams{Prefix: "d-", Delimiter: "-"}, want{objects: []string{"d-2"}, cps: []string{"d-1-"}}},
		{"multi-char delimiter", storage.ListParams{Delimiter: "/b"}, want{objects: []string{"//double", "/root", "a.txt", "a/e", "b", "c/", "c/d", "d-1-x", "d-2", "日本/語", "日本語"}, cps: []string{"a/b"}}},
		{"unicode delimiter", storage.ListParams{Delimiter: "本"}, want{objects: []string{"//double", "/root", "a.txt", "a/b/c", "a/b/d", "a/e", "b", "c/", "c/d", "d-1-x", "d-2"}, cps: []string{"日本"}}},
		{"after a common prefix skips it", storage.ListParams{Delimiter: "/", After: "a/"}, want{objects: []string{"b", "d-1-x", "d-2", "日本語"}, cps: []string{"c/", "日本/"}}},
		{"after inside a common prefix re-rolls it", storage.ListParams{Delimiter: "/", After: "a/b/c"}, want{objects: []string{"b", "d-1-x", "d-2", "日本語"}, cps: []string{"a/", "c/", "日本/"}}},
		{"after a key", storage.ListParams{Delimiter: "/", After: "b"}, want{objects: []string{"d-1-x", "d-2", "日本語"}, cps: []string{"c/", "日本/"}}},
		{"after before prefix", storage.ListParams{Prefix: "c", After: "a"}, want{objects: []string{"c/", "c/d"}}},
		{"after beyond prefix", storage.ListParams{Prefix: "c", After: "d"}, want{}},
		{"truncated on key", storage.ListParams{Delimiter: "/", MaxKeys: 2}, want{cps: []string{"/"}, objects: []string{"a.txt"}, trunc: true, next: "a.txt"}},
		{"truncated on prefix", storage.ListParams{Delimiter: "/", MaxKeys: 3}, want{cps: []string{"/", "a/"}, objects: []string{"a.txt"}, trunc: true, next: "a/"}},
		{"exact fit not truncated", storage.ListParams{Prefix: "a/", Delimiter: "/", MaxKeys: 2}, want{objects: []string{"a/e"}, cps: []string{"a/b/"}}},
		{"no match", storage.ListParams{Prefix: "zzz", Delimiter: "/"}, want{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := storage.ListObjects(ctx, f, "bkt", tc.p)
			if err != nil {
				t.Fatal(err)
			}
			var objs []string
			for _, o := range res.Objects {
				objs = append(objs, o.Key)
				if o.Size != int64(len(o.Key)) {
					t.Errorf("object %q lost its info", o.Key)
				}
			}
			if !slices.Equal(objs, tc.want.objects) || !slices.Equal(res.CommonPrefixes, tc.want.cps) ||
				res.IsTruncated != tc.want.trunc || res.NextAfter != tc.want.next {
				t.Fatalf("got objects=%q cps=%q trunc=%v next=%q\nwant objects=%q cps=%q trunc=%v next=%q",
					objs, res.CommonPrefixes, res.IsTruncated, res.NextAfter,
					tc.want.objects, tc.want.cps, tc.want.trunc, tc.want.next)
			}
		})
	}
}

func TestListObjectsPaginationMatchesReference(t *testing.T) {
	f := newFake().addKeys("a/b/c", "a/b/d", "a/e", "a.txt", "b", "c/", "c/d", "d-1-x", "d-2", "/root", "//double",
		"日本/語", "日本語", "x/1", "x/2", "x/3", "y--z", "y--w--q", "y-", "z")
	for _, tc := range []struct{ prefix, delim, after string }{
		{"", "", ""}, {"", "/", ""}, {"a", "/", ""}, {"a/", "/", ""}, {"/", "/", ""},
		{"", "-", ""}, {"y", "--", ""}, {"", "--", ""}, {"", "本", ""}, {"日", "/", ""},
		{"", "/", "a/"}, {"", "/", "a/b/c"}, {"", "/", "/"}, {"x", "/", "x/1"}, {"", "/", "zz"},
	} {
		want := referenceList(f, tc.prefix, tc.delim, tc.after)
		for _, pageSize := range []int{1, 2, 3, 5, 1000, 0} {
			size := pageSize
			if size == 0 {
				size = storage.DefaultMaxKeys
			}
			got := paginate(t, f, tc.prefix, tc.delim, tc.after, size)
			if !slices.Equal(got, want) {
				t.Fatalf("prefix=%q delim=%q after=%q page=%d:\n got  %v\n want %v", tc.prefix, tc.delim, tc.after, pageSize, got, want)
			}
		}
	}
}

func TestListObjectsRandomized(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	alphabet := []string{"a", "b", "/", "-", "é"}
	randStr := func(maxLen int) string {
		var sb strings.Builder
		for n := rng.IntN(maxLen + 1); n > 0; n-- {
			sb.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		return sb.String()
	}
	for iter := 0; iter < 200; iter++ {
		f := newFake()
		for i := 0; i < 40; i++ {
			k := randStr(6)
			if k == "" {
				continue
			}
			f.add(k, 1, rng.IntN(8) == 0)
		}
		prefix := randStr(2)
		delim := []string{"", "/", "-", "a/", "é"}[rng.IntN(5)]
		after := ""
		if rng.IntN(2) == 0 {
			after = randStr(4)
		}
		want := referenceList(f, prefix, delim, after)
		for _, size := range []int{1, 2, 7} {
			if got := paginate(t, f, prefix, delim, after, size); !slices.Equal(got, want) {
				t.Fatalf("iter %d prefix=%q delim=%q after=%q size=%d:\n got  %v\n want %v", iter, prefix, delim, after, size, got, want)
			}
		}
	}
}

func TestListObjectsSkipsCommonPrefixes(t *testing.T) {
	f := newFake()
	for i := 0; i < 5000; i++ {
		f.add(fmt.Sprintf("dir/%05d", i), 1, false)
	}
	f.addKeys("a", "z")
	res, err := storage.ListObjects(ctx, f, "bkt", storage.ListParams{Delimiter: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.CommonPrefixes, []string{"dir/"}) || len(res.Objects) != 2 || res.IsTruncated {
		t.Fatalf("got %+v", res)
	}
	if f.yielded > 5 {
		t.Fatalf("walk yielded %d items; common prefix was enumerated instead of skipped", f.yielded)
	}
}

func TestListObjectsMaxKeysDefault(t *testing.T) {
	f := newFake()
	for i := 0; i < 1001; i++ {
		f.add(fmt.Sprintf("k%04d", i), 1, false)
	}
	for _, mk := range []int{0, -5} {
		res, err := storage.ListObjects(ctx, f, "bkt", storage.ListParams{MaxKeys: mk})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Objects) != 1000 || !res.IsTruncated || res.NextAfter != "k0999" {
			t.Fatalf("MaxKeys=%d: %d objects, truncated=%v next=%q", mk, len(res.Objects), res.IsTruncated, res.NextAfter)
		}
	}
}

func TestListErrors(t *testing.T) {
	f := newFake().addKeys("a")
	boom := errors.New("boom")
	f.walkErr = boom
	if _, err := storage.ListObjects(ctx, f, "bkt", storage.ListParams{}); !errors.Is(err, boom) {
		t.Fatalf("ListObjects err = %v", err)
	}
	if _, err := storage.ListObjectVersions(ctx, f, "bkt", storage.VersionListParams{}); !errors.Is(err, boom) {
		t.Fatalf("ListObjectVersions err = %v", err)
	}
	if _, err := storage.ComputeBucketUsage(ctx, f, "bkt"); !errors.Is(err, boom) {
		t.Fatalf("ComputeBucketUsage err = %v", err)
	}
	m := memory.New()
	defer m.Close()
	if _, err := storage.ListObjects(ctx, m, "nope", storage.ListParams{}); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
}

// --- versions ---

func verEntries(r storage.VersionListResult) []entry {
	var out []entry
	i, j := 0, 0
	for i < len(r.Versions) || j < len(r.CommonPrefixes) {
		if j == len(r.CommonPrefixes) || (i < len(r.Versions) && r.Versions[i].Key < r.CommonPrefixes[j]) {
			out = append(out, entry{key: r.Versions[i].Key, vid: r.Versions[i].VersionID})
			i++
		} else {
			out = append(out, entry{key: r.CommonPrefixes[j], cp: true})
			j++
		}
	}
	return out
}

// referenceVersions is the unpaginated version listing from the definition.
func referenceVersions(f *fakeBackend, prefix, delim, keyMarker string) []entry {
	var out []entry
	seen := map[string]bool{}
	for _, k := range f.keys {
		if !strings.HasPrefix(k, prefix) || (keyMarker != "" && k <= keyMarker) {
			continue
		}
		if cp, ok := storage.CommonPrefix(k, prefix, delim); ok {
			if cp == keyMarker || seen[cp] {
				continue
			}
			seen[cp] = true
			out = append(out, entry{key: cp, cp: true})
			continue
		}
		for _, v := range f.versions[k] {
			out = append(out, entry{key: k, vid: v.VersionID})
		}
	}
	return out
}

func paginateVersions(t *testing.T, b storage.Backend, prefix, delim, keyMarker string, pageSize int) []entry {
	t.Helper()
	var all []entry
	vidMarker := ""
	for page := 0; ; page++ {
		if page > 10000 {
			t.Fatal("pagination does not terminate")
		}
		res, err := storage.ListObjectVersions(ctx, b, "bkt", storage.VersionListParams{
			Prefix: prefix, Delimiter: delim, KeyMarker: keyMarker, VersionIDMarker: vidMarker, MaxKeys: pageSize,
		})
		if err != nil {
			t.Fatal(err)
		}
		es := verEntries(res)
		if len(es) > pageSize {
			t.Fatalf("page has %d entries > MaxKeys %d", len(es), pageSize)
		}
		all = append(all, es...)
		if !res.IsTruncated {
			if res.NextKeyMarker != "" || res.NextVersionIDMarker != "" {
				t.Fatalf("next markers set on final page: %q %q", res.NextKeyMarker, res.NextVersionIDMarker)
			}
			return all
		}
		last := es[len(es)-1]
		if len(es) != pageSize || res.NextKeyMarker != last.key || res.NextVersionIDMarker != last.vid {
			t.Fatalf("truncated page: %d entries, next=(%q,%q), last=%+v", len(es), res.NextKeyMarker, res.NextVersionIDMarker, last)
		}
		keyMarker, vidMarker = res.NextKeyMarker, res.NextVersionIDMarker
	}
}

func versionedFake() *fakeBackend {
	f := newFake()
	for _, k := range []string{"a/1", "a/2", "b", "c/x/y", "c/z", "d", "e-1", "e-2", "日/本"} {
		f.add(k, 1, false)
	}
	f.add("b", 2, false)
	f.add("b", 0, true)
	f.add("d", 0, true)
	f.add("a/1", 3, false)
	f.add("e-1", 3, false)
	f.add("e-1", 3, false)
	return f
}

func TestListObjectVersionsTable(t *testing.T) {
	f := versionedFake()
	res, err := storage.ListObjectVersions(ctx, f, "bkt", storage.VersionListParams{Delimiter: "/"})
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{{key: "a/", cp: true}, {key: "b", vid: "v002"}, {key: "b", vid: "v001"}, {key: "b", vid: "v000"},
		{key: "c/", cp: true}, {key: "d", vid: "v001"}, {key: "d", vid: "v000"},
		{key: "e-1", vid: "v002"}, {key: "e-1", vid: "v001"}, {key: "e-1", vid: "v000"}, {key: "e-2", vid: "v000"}, {key: "日/", cp: true}}
	if got := verEntries(res); !slices.Equal(got, want) || res.IsTruncated {
		t.Fatalf("got %v (trunc %v)\nwant %v", got, res.IsTruncated, want)
	}
	if !res.Versions[0].IsLatest || !res.Versions[0].DeleteMarker || res.Versions[1].IsLatest {
		t.Fatalf("version flags lost: %+v", res.Versions[:2])
	}

	// Truncated in the middle of a key's versions.
	res, _ = storage.ListObjectVersions(ctx, f, "bkt", storage.VersionListParams{Delimiter: "/", MaxKeys: 3})
	if !res.IsTruncated || res.NextKeyMarker != "b" || res.NextVersionIDMarker != "v001" {
		t.Fatalf("mid-key truncation: %+v", res)
	}
	// Resume from there.
	res, _ = storage.ListObjectVersions(ctx, f, "bkt", storage.VersionListParams{Delimiter: "/", MaxKeys: 2, KeyMarker: "b", VersionIDMarker: "v001"})
	if got := verEntries(res); !slices.Equal(got, []entry{{key: "b", vid: "v000"}, {key: "c/", cp: true}}) ||
		!res.IsTruncated || res.NextKeyMarker != "c/" || res.NextVersionIDMarker != "" {
		t.Fatalf("resume: %v %+v", got, res)
	}
	// KeyMarker equal to a common prefix skips it, even with a version marker.
	res, _ = storage.ListObjectVersions(ctx, f, "bkt", storage.VersionListParams{Delimiter: "/", MaxKeys: 1, KeyMarker: "c/", VersionIDMarker: "v000"})
	if got := verEntries(res); !slices.Equal(got, []entry{{key: "d", vid: "v001"}}) || res.NextKeyMarker != "d" || res.NextVersionIDMarker != "v001" {
		t.Fatalf("cp marker: %v %+v", got, res)
	}
	// Version marker without key marker is ignored.
	res, _ = storage.ListObjectVersions(ctx, f, "bkt", storage.VersionListParams{Prefix: "e", VersionIDMarker: "v001"})
	if len(res.Versions) != 4 {
		t.Fatalf("vid marker without key marker: %+v", res)
	}
	// Prefix + delimiter inside.
	res, _ = storage.ListObjectVersions(ctx, f, "bkt", storage.VersionListParams{Prefix: "c/", Delimiter: "/"})
	if got := verEntries(res); !slices.Equal(got, []entry{{key: "c/x/", cp: true}, {key: "c/z", vid: "v000"}}) {
		t.Fatalf("prefix c/: %v", got)
	}
	// Multi-char delimiter.
	res, _ = storage.ListObjectVersions(ctx, f, "bkt", storage.VersionListParams{Prefix: "e", Delimiter: "-1"})
	if got := verEntries(res); !slices.Equal(got, []entry{{key: "e-1", cp: true}, {key: "e-2", vid: "v000"}}) {
		t.Fatalf("multi-char delimiter: %v", got)
	}
}

func TestListObjectVersionsPaginationMatchesReference(t *testing.T) {
	f := versionedFake()
	for _, tc := range []struct{ prefix, delim, keyMarker string }{
		{"", "", ""}, {"", "/", ""}, {"a", "/", ""}, {"c/", "/", ""}, {"", "-", ""}, {"e", "-", ""},
		{"", "/", "a/"}, {"", "/", "b"}, {"", "", "b"}, {"", "日", ""}, {"", "/", "c/x/y"},
	} {
		want := referenceVersions(f, tc.prefix, tc.delim, tc.keyMarker)
		for _, size := range []int{1, 2, 3, 4, 1000} {
			if got := paginateVersions(t, f, tc.prefix, tc.delim, tc.keyMarker, size); !slices.Equal(got, want) {
				t.Fatalf("%+v size=%d:\n got  %v\n want %v", tc, size, got, want)
			}
		}
	}
}

func TestListAgainstMemoryBackend(t *testing.T) {
	m := memory.New()
	defer m.Close()
	if err := m.CreateBucket(ctx, "bkt", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBucketVersioning(ctx, "bkt", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	rng := rand.New(rand.NewPCG(3, 4))
	ids := map[string]map[string]string{} // key -> memory id -> fake id
	for i := 0; i < 300; i++ {
		key := fmt.Sprintf("%c/%c%c", 'a'+rng.IntN(4), 'a'+rng.IntN(3), '0'+rng.IntN(10))
		if rng.IntN(3) == 0 {
			key = fmt.Sprintf("top%d", rng.IntN(20))
		}
		var mid string
		dm := rng.IntN(6) == 0
		if dm {
			res, err := m.DeleteObject(ctx, "bkt", key, "")
			if err != nil {
				t.Fatal(err)
			}
			mid = res.VersionID
		} else {
			w, err := m.NewObjectWriter(ctx, "bkt", key)
			if err != nil {
				t.Fatal(err)
			}
			oi, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{})
			if err != nil {
				t.Fatal(err)
			}
			mid = oi.VersionID
		}
		fid := f.add(key, 0, dm)
		if ids[key] == nil {
			ids[key] = map[string]string{}
		}
		ids[key][mid] = fid
	}
	for _, tc := range []struct{ prefix, delim string }{{"", ""}, {"", "/"}, {"a", "/"}, {"b/", "/"}, {"t", "p"}} {
		for _, size := range []int{1, 3, 1000} {
			if got, want := paginate(t, m, tc.prefix, tc.delim, "", size), referenceList(f, tc.prefix, tc.delim, ""); !slices.Equal(got, want) {
				t.Fatalf("objects %+v size=%d:\n got  %v\n want %v", tc, size, got, want)
			}
			got := paginateVersions(t, m, tc.prefix, tc.delim, "", size)
			for i := range got {
				if !got[i].cp {
					got[i].vid = ids[got[i].key][got[i].vid]
				}
			}
			if want := referenceVersions(f, tc.prefix, tc.delim, ""); !slices.Equal(got, want) {
				t.Fatalf("versions %+v size=%d:\n got  %v\n want %v", tc, size, got, want)
			}
		}
	}
}

func TestComputeBucketUsage(t *testing.T) {
	f := versionedFake()
	f.uploads = 3
	u, err := storage.ComputeBucketUsage(ctx, f, "bkt")
	if err != nil {
		t.Fatal(err)
	}
	// Current objects: a/1(3) a/2(1) c/x/y(1) c/z(1) e-1(3) e-2(1) 日/本(1); b and d are deleted.
	want := storage.BucketUsage{Objects: 7, Bytes: 11, StoredBytes: 11, Versions: 13, VersionBytes: 20, StoredVersionBytes: 20,
		DeleteMarkers: 2, PendingUploads: 3}
	if u != want {
		t.Fatalf("usage = %+v, want %+v", u, want)
	}

	m := memory.New()
	defer m.Close()
	_ = m.CreateBucket(ctx, "bkt", storage.CreateBucketOptions{})
	if u, err := storage.ComputeBucketUsage(ctx, m, "bkt"); err != nil || u != (storage.BucketUsage{}) {
		t.Fatalf("empty bucket usage = %+v, %v", u, err)
	}
	if _, err := storage.ComputeBucketUsage(ctx, m, "nope"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
}

// TestListingInfoTrimmed: listings keep only the fixed-size fields of each
// entry, not the content headers, user metadata and tags a writer chose.
func TestListingInfoTrimmed(t *testing.T) {
	m := memory.New()
	defer m.Close()
	if err := m.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	meta := storage.ObjectMeta{
		ETag: "e", ContentType: strings.Repeat("t", 4096), ContentDisposition: "d", CacheControl: "c",
		UserMeta: map[string]string{"k": "v"}, Tags: map[string]string{"a": "b"}, StorageClass: "GLACIER",
		Checksums: map[string]string{"CRC32": "AAAAAA=="}, ChecksumType: "FULL_OBJECT", PartSizes: []int64{1},
	}
	w, err := m.NewObjectWriter(ctx, "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("x"))
	if _, err := w.Commit(ctx, meta, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateMultipartUpload(ctx, "b", "k", meta, ""); err != nil {
		t.Fatal(err)
	}
	check := func(what string, got storage.ObjectMeta) {
		t.Helper()
		if got.ContentType != "" || got.ContentDisposition != "" || got.CacheControl != "" || got.UserMeta != nil || got.Tags != nil || got.PartSizes != nil {
			t.Errorf("%s: metadata not trimmed: %+v", what, got)
		}
		if got.ETag != "e" || got.StorageClass != "GLACIER" || got.Checksums["CRC32"] == "" || got.ChecksumType != "FULL_OBJECT" {
			t.Errorf("%s: listed fields lost: %+v", what, got)
		}
	}
	lr, err := storage.ListObjects(ctx, m, "b", storage.ListParams{})
	if err != nil || len(lr.Objects) != 1 {
		t.Fatalf("ListObjects: %v %+v", err, lr)
	}
	check("ListObjects", lr.Objects[0].ObjectMeta)
	vr, err := storage.ListObjectVersions(ctx, m, "b", storage.VersionListParams{})
	if err != nil || len(vr.Versions) != 1 {
		t.Fatalf("ListObjectVersions: %v %+v", err, vr)
	}
	check("ListObjectVersions", vr.Versions[0].ObjectMeta)
	ur, err := storage.ListUploads(ctx, m, "b", storage.UploadListParams{MaxUploads: 10})
	if err != nil || len(ur.Uploads) != 1 {
		t.Fatalf("ListUploads: %v %+v", err, ur)
	}
	check("ListUploads", ur.Uploads[0].Meta)
}
