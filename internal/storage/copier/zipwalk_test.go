package copier

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// pairDesc describes a pair for comparisons: keys and entry counts.
type pairDesc struct {
	src, dst   string
	nsrc, ndst int
}

func describe(ps []KeyPair) []pairDesc {
	out := make([]pairDesc, len(ps))
	for i, p := range ps {
		out[i] = pairDesc{p.SrcKey, p.DstKey, len(p.Src), len(p.Dst)}
	}
	return out
}

func collect(t *testing.T, src, dst Endpoint, after string, versions bool) (all []KeyPair, batches [][]KeyPair) {
	t.Helper()
	err := ZipWalk(t.Context(), src, dst, after, versions, func(ps []KeyPair) error {
		batches = append(batches, ps)
		all = append(all, ps...)
		return nil
	})
	if err != nil {
		t.Fatalf("ZipWalk: %v", err)
	}
	return all, batches
}

// zipFixture: a source and a target bucket on one memory backend, with
// keys inside and outside the walked prefixes.
func zipFixture(t *testing.T) (src, dst Endpoint) {
	b := memory.New()
	newBucket(t, b, "s", storage.VersioningEnabled, false)
	newBucket(t, b, "d", storage.VersioningEnabled, false)
	src = Endpoint{Backend: b, Bucket: "s", Prefix: "in/"}
	dst = Endpoint{Backend: b, Bucket: "d", Prefix: "out/x/"}
	putText(t, src, "in/a", "1")
	putText(t, src, "in/a", "2")
	putText(t, src, "in/b", "3")
	putText(t, src, "in/d", "4")
	del(t, src, "in/d", "") // its latest version is a delete marker
	putText(t, src, "other/z", "5")
	putText(t, dst, "out/x/b", "6")
	putText(t, dst, "out/x/c", "7")
	putText(t, dst, "out/x/e", "8")
	putText(t, dst, "out/y", "9")
	return src, dst
}

func TestZipWalkPairsKeys(t *testing.T) {
	src, dst := zipFixture(t)
	got, _ := collect(t, src, dst, "", true)
	want := []pairDesc{
		{"in/a", "out/x/a", 2, 0},
		{"in/b", "out/x/b", 1, 1},
		{"in/c", "out/x/c", 0, 1},
		{"in/d", "out/x/d", 2, 0},
		{"in/e", "out/x/e", 0, 1},
	}
	if d := describe(got); !slices.Equal(d, want) {
		t.Fatalf("versions walk:\n got %+v\nwant %+v", d, want)
	}
	if !got[0].Src[0].IsLatest || got[0].Src[0].LastModified.Before(got[0].Src[1].LastModified) {
		t.Fatalf("versions not newest first: %+v", got[0].Src)
	}
	got, _ = collect(t, src, dst, "", false)
	want = []pairDesc{
		{"in/a", "out/x/a", 1, 0},
		{"in/b", "out/x/b", 1, 1},
		{"in/c", "out/x/c", 0, 1},
		{"in/e", "out/x/e", 0, 1},
	}
	if d := describe(got); !slices.Equal(d, want) {
		t.Fatalf("objects walk:\n got %+v\nwant %+v", d, want)
	}
}

// A key with too long a history on either side is delivered flagged, with
// that side's entries dropped, and the walk goes on with the other keys
// (one such key must not stop a whole copy).
func TestVersionHistoryLimit(t *testing.T) {
	src := memEndpoint(t, "source", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "target", "", storage.VersioningEnabled)
	putText(t, src, "aaa-before", "a")
	for range MaxKeyVersions + 1 {
		putText(t, src, "hot", "x")
	}
	putText(t, dst, "hot", "old")
	for range MaxKeyVersions + 1 {
		putText(t, dst, "hot-target", "y")
	}
	putText(t, src, "zzz-after", "z")
	if _, err := KeyVersions(t.Context(), src.Backend, src.Bucket, "hot"); !errors.Is(err, ErrTooManyVersions) {
		t.Fatalf("KeyVersions: %v", err)
	}
	if _, err := LookupPair(t.Context(), src, dst, "hot", true); !errors.Is(err, ErrTooManyVersions) {
		t.Fatalf("LookupPair: %v", err)
	}
	var got []KeyPair
	maxBatch := 0
	err := ZipWalk(t.Context(), src, dst, "", true, func(ps []KeyPair) error {
		n := 0
		for _, p := range ps {
			n += len(p.Src) + len(p.Dst)
		}
		maxBatch = max(maxBatch, n)
		got = append(got, ps...)
		return nil
	})
	if err != nil {
		t.Fatalf("ZipWalk: %v", err)
	}
	want := []pairDesc{
		{"aaa-before", "aaa-before", 1, 0},
		{"hot", "hot", 0, 1},
		{"hot-target", "hot-target", 0, 0},
		{"zzz-after", "zzz-after", 1, 0},
	}
	if d := describe(got); !slices.Equal(d, want) {
		t.Fatalf("pairs:\n got %+v\nwant %+v", d, want)
	}
	for i, p := range got {
		flagged := p.SrcKey == "hot" || p.SrcKey == "hot-target"
		if flagged != errors.Is(p.Err, ErrTooManyVersions) {
			t.Fatalf("pair %d (%s): Err = %v", i, p.SrcKey, p.Err)
		}
	}
	if !strings.Contains(got[1].Err.Error(), "source") || !strings.Contains(got[2].Err.Error(), "target") {
		t.Fatalf("errors do not name the side: %v; %v", got[1].Err, got[2].Err)
	}
	if maxBatch > batchVersions {
		t.Fatalf("a batch held %d entries", maxBatch)
	}
	// The sync functions refuse a flagged pair without writing.
	for _, pol := range []Policy{{}, {Versions: VersionsCurrent}} {
		if _, err := SyncKey(t.Context(), src, dst, got[1], pol); !errors.Is(err, ErrTooManyVersions) {
			t.Fatalf("SyncKey(hot): %v", err)
		}
		if _, err := SyncKeyFingerprint(t.Context(), src, dst, got[1], NewMemStore(), pol); !errors.Is(err, ErrTooManyVersions) {
			t.Fatalf("SyncKeyFingerprint(hot): %v", err)
		}
	}
	if vs := history(t, dst, "hot"); len(vs) != 1 {
		t.Fatalf("target hot changed: %d versions", len(vs))
	}
}

func TestZipWalkResumes(t *testing.T) {
	src, dst := zipFixture(t)
	for _, tc := range []struct {
		after string
		first string // "" = no pairs
	}{
		{"", "in/a"},
		{"a", "in/a"}, // before the prefix
		{"in/", "in/a"},
		{"in/a", "in/b"},
		{"in/b", "in/c"},
		{"in/bb", "in/c"},   // between keys
		{"in/c", "in/d"},    // a target-only key
		{"in/e", ""},        // the last key
		{"in0", ""},         // after every key with the prefix
		{"other/a", ""},     // ditto
		{"in/\xff\xff", ""}, /* after every valid key */
	} {
		got, _ := collect(t, src, dst, tc.after, true)
		first := ""
		if len(got) > 0 {
			first = got[0].SrcKey
		}
		if first != tc.first {
			t.Errorf("after %q: first pair %q, want %q", tc.after, first, tc.first)
		}
	}
}

func TestZipWalkBatchesAtKeyBoundaries(t *testing.T) {
	old := batchVersions
	batchVersions = 3
	t.Cleanup(func() { batchVersions = old })
	src := memEndpoint(t, "s", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "d", "p/", storage.VersioningEnabled)
	var keys []string
	for i := range 12 {
		k := fmt.Sprintf("k%02d", i)
		keys = append(keys, k)
		putText(t, src, k, "x")
		if i == 5 {
			for range 6 {
				putText(t, src, k, "more") // a key larger than a batch
			}
		}
		if i%3 == 0 {
			putText(t, dst, "p/"+k, "y")
		}
	}
	putText(t, dst, "p/k99", "target only")
	keys = append(keys, "k99")
	got, batches := collect(t, src, dst, "", true)
	var seen []string
	for _, p := range got {
		seen = append(seen, p.SrcKey)
		if p.SrcKey == "k05" && len(p.Src) != 7 {
			t.Fatalf("k05 delivered with %d versions, want 7", len(p.Src))
		}
	}
	if !slices.Equal(seen, keys) {
		t.Fatalf("keys %v, want %v", seen, keys)
	}
	if len(batches) < 4 {
		t.Fatalf("only %d batches", len(batches))
	}
	for i, b := range batches {
		entries := 0
		for _, p := range b[:len(b)-1] {
			entries += len(p.Src) + len(p.Dst)
		}
		if entries >= batchVersions {
			t.Fatalf("batch %d continues after %d entries: %+v", i, entries, describe(b))
		}
	}
}

func TestZipWalkStopsOnCallbackError(t *testing.T) {
	src, dst := zipFixture(t)
	boom := errors.New("boom")
	calls := 0
	err := ZipWalk(t.Context(), src, dst, "", true, func([]KeyPair) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("ZipWalk = %v after %d calls", err, calls)
	}
}

func TestZipWalkReportsTheSideThatFailed(t *testing.T) {
	src, dst := zipFixture(t)
	dst.Bucket = "missing"
	err := ZipWalk(t.Context(), src, dst, "", true, func([]KeyPair) error { return nil })
	if !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("ZipWalk = %v", err)
	}
}

func TestLookupPair(t *testing.T) {
	src, dst := zipFixture(t)
	p, err := LookupPair(t.Context(), src, dst, "in/b", true)
	if err != nil || p.DstKey != "out/x/b" || len(p.Src) != 1 || len(p.Dst) != 1 {
		t.Fatalf("LookupPair(in/b) = %+v, %v", p, err)
	}
	p, err = LookupPair(t.Context(), src, dst, "in/d", false)
	if err != nil || len(p.Src) != 0 || len(p.Dst) != 0 {
		t.Fatalf("LookupPair(in/d, objects) = %+v, %v", p, err)
	}
	if _, err := LookupPair(t.Context(), src, dst, "other/z", true); err == nil {
		t.Fatal("LookupPair accepted a key outside the prefix")
	}
}
