package copier

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// nativeSource returns a source whose listings report other ETags than
// reads, as a native bucket may for objects a foreign tool uploaded in
// parts, and the raw endpoint behind it.
func nativeSource(t *testing.T, versioning storage.VersioningStatus) (src, raw Endpoint, v *view) {
	m := memory.New()
	newBucket(t, m, "src", versioning, false)
	v = &view{Backend: m, listETag: func(oi storage.ObjectInfo) string {
		if oi.DeleteMarker {
			return ""
		}
		return oi.ETag[:8] + "-2"
	}}
	return Endpoint{Backend: v, Bucket: "src"}, Endpoint{Backend: m, Bucket: "src"}, v
}

func TestFingerprintDoesNotLoopOnListingETags(t *testing.T) {
	for _, versions := range []Versions{VersionsAll, VersionsCurrent} {
		t.Run(string(versions), func(t *testing.T) {
			src, raw, _ := nativeSource(t, storage.VersioningEnabled)
			putText(t, raw, "a", "1")
			putText(t, raw, "a", "2")
			putText(t, raw, "b", "3")
			del(t, raw, "b", "")
			putText(t, raw, "c", "4")
			dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
			st := NewMemStore()
			pol := Policy{Versions: versions}
			r := mustFP(t, src, dst, st, pol)
			want := int64(5)
			if versions == VersionsCurrent {
				want = 2
			}
			if r.VersionsCopied != want || r.PlainWrites != 0 {
				t.Fatalf("first pass: %+v", r)
			}
			if versions == VersionsAll {
				for _, k := range []string{"a", "b", "c"} {
					sameHistory(t, raw, k, dst, k) // the target holds what reads return
				}
			}
			noChange(t, "second pass", mustFP(t, src, dst, st, pol))
			records := 3
			if versions == VersionsCurrent {
				records = 2 // b's current version is a delete marker
			}
			if st.Len() != records {
				t.Fatalf("%d records, want %d", st.Len(), records)
			}

			// Identity mode would copy these keys again on every pass.
			r = syncAll(t, src, memEndpoint(t, "d2", "", storage.VersioningEnabled), pol)
			if r.Deferred == false && r.VersionsCopied == 0 {
				t.Fatalf("identity mode on listing ETags: %+v", r)
			}
		})
	}
}

func TestFingerprintRemapsVersionIDs(t *testing.T) {
	m := memory.New()
	newBucket(t, m, "src", storage.VersioningEnabled, false)
	raw := Endpoint{Backend: m, Bucket: "src"}
	v1 := putText(t, raw, "k", "1")
	putText(t, raw, "k", "2")
	src := Endpoint{Backend: &view{Backend: m,
		idOut: func(id string) string { return "remote:" + id + "/" + strings.Repeat("z", 70) },
		idIn: func(id string) string {
			id = strings.TrimPrefix(id, "remote:")
			return id[:strings.IndexByte(id, '/')]
		},
	}, Bucket: "src"}
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	st := NewMemStore()
	r := mustFP(t, src, dst, st, Policy{})
	if r.VersionsCopied != 2 || r.PlainWrites != 0 {
		t.Fatalf("first pass: %+v", r)
	}
	h := history(t, dst, "k")
	srcID := "remote:" + v1.VersionID + "/" + strings.Repeat("z", 70)
	if len(h) != 2 || h[1].VersionID != MapVersionID(srcID) || !h[1].LastModified.Equal(v1.LastModified) {
		t.Fatalf("target %+v, want version %s", h, MapVersionID(srcID))
	}
	rec, ok, _ := st.Get("k")
	if !ok || len(rec.VIDMap) != 2 || !rec.Owned {
		t.Fatalf("record %+v", rec)
	}
	found := false
	for tok, id := range rec.VIDMap {
		if strings.HasPrefix(tok, srcID+"@") && id == MapVersionID(srcID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("VIDMap %v lacks %s", rec.VIDMap, srcID)
	}
	noChange(t, "second pass", mustFP(t, src, dst, st, Policy{}))
}

func TestFingerprintPlainWritesToAVersionedTarget(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	putText(t, src, "k", "1")
	putText(t, src, "k", "2")
	del(t, src, "k", "")
	putText(t, src, "k", "3")
	dst := nativeEndpoint(t, "dst", "", storage.VersioningEnabled)
	st := NewMemStore()
	r := mustFP(t, src, dst, st, Policy{})
	if r.VersionsCopied != 4 || r.PlainWrites != 4 {
		t.Fatalf("first pass: %+v", r)
	}
	// Same order, same contents and markers; the target's own IDs.
	shape := func(e Endpoint) string {
		var b strings.Builder
		for _, v := range history(t, e, "k") {
			if v.DeleteMarker {
				b.WriteString("dm ")
			} else {
				b.WriteString(read(t, e, "k", v.VersionID) + " ")
			}
		}
		return b.String()
	}
	if got, want := shape(dst), shape(src); got != want {
		t.Fatalf("target %q, want %q", got, want)
	}
	if h := history(t, dst, "k"); h[0].VersionID == history(t, src, "k")[0].VersionID {
		t.Fatal("plain writes kept the source's version IDs")
	}
	if rec, _, _ := st.Get("k"); len(rec.VIDMap) != 4 {
		t.Fatalf("VIDMap %v", rec.VIDMap)
	}
	noChange(t, "second pass", mustFP(t, src, dst, st, Policy{}))

	// A new version: one write.
	putText(t, src, "k", "4")
	if r := mustFP(t, src, dst, st, Policy{}); r.VersionsCopied != 1 || r.VersionsDeleted != 0 {
		t.Fatalf("new version: %+v", r)
	}
	// A version deleted from the middle: its copy goes.
	h := history(t, src, "k")
	del(t, src, "k", h[3].VersionID) // "1"
	if r := mustFP(t, src, dst, st, Policy{}); r.VersionsCopied != 0 || r.VersionsDeleted != 1 {
		t.Fatalf("deleted version: %+v", r)
	}
	if got, want := shape(dst), shape(src); got != want {
		t.Fatalf("target %q, want %q", got, want)
	}
	noChange(t, "last pass", mustFP(t, src, dst, st, Policy{}))
}

func TestFingerprintPlainWritesNeedVersioningForHistories(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	putText(t, src, "k", "1")
	dst := nativeEndpoint(t, "dst", "", storage.VersioningUnversioned)
	st := NewMemStore()
	if _, err := syncAllFP(t, src, dst, st, Policy{}); !errors.Is(err, ErrNotVersioned) {
		t.Fatalf("history to an unversioned plain target: %v", err)
	}
	if st.Len() != 0 {
		t.Fatal("a key the copy never wrote was recorded")
	}
}

func TestFingerprintCurrentToAPlainTarget(t *testing.T) {
	src, raw, _ := nativeSource(t, storage.VersioningUnversioned)
	putText(t, raw, "k", "1")
	dst := nativeEndpoint(t, "dst", "", storage.VersioningUnversioned)
	st := NewMemStore()
	pol := Policy{Versions: VersionsCurrent}
	if r := mustFP(t, src, dst, st, pol); r.VersionsCopied != 1 || r.PlainWrites != 1 {
		t.Fatalf("first pass: %+v", r)
	}
	noChange(t, "second pass", mustFP(t, src, dst, st, pol))
	putText(t, raw, "k", "2")
	if r := mustFP(t, src, dst, st, pol); r.VersionsCopied != 1 || read(t, dst, "k", "") != "2" {
		t.Fatalf("update: %+v", r)
	}
	// Deleted on the source: the copy is owned (the record says so).
	del(t, raw, "k", "")
	if r := mustFP(t, src, dst, st, pol); r.VersionsDeleted != 1 || len(history(t, dst, "k")) != 0 || st.Len() != 0 {
		t.Fatalf("delete: %+v, %d records", r, st.Len())
	}
}

func TestFingerprintRecopiesWhenTheTargetDrifts(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	putText(t, src, "k", "1")
	putText(t, src, "k", "2")
	dst := nativeEndpoint(t, "dst", "", storage.VersioningEnabled)
	st := NewMemStore()
	mustFP(t, src, dst, st, Policy{})
	h := history(t, dst, "k")
	del(t, dst, "k", h[0].VersionID) // behind the copy's back
	r := mustFP(t, src, dst, st, Policy{})
	if r.VersionsCopied != 1 || r.VersionsDeleted != 0 {
		t.Fatalf("drift: %+v", r)
	}
	noChange(t, "after the repair", mustFP(t, src, dst, st, Policy{}))
}

func TestFingerprintEncryptionToANativeTarget(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	put(t, src, "k", "secret", storage.ObjectMeta{SSE: "AES256"}, storage.WriteOptions{Encryption: storage.EncryptionAES256})
	dst := nativeEndpoint(t, "dst", "", storage.VersioningEnabled)
	st := NewMemStore()
	if _, err := syncAllFP(t, src, dst, st, Policy{}); !errors.Is(err, ErrEncryptionUnsupported) {
		t.Fatalf("encrypted data to a native target: %v", err)
	}
	if len(history(t, dst, "k")) != 0 {
		t.Fatal("stored something")
	}
	pol := Policy{AllowPlaintext: true}
	if r := mustFP(t, src, dst, st, pol); r.VersionsCopied != 1 {
		t.Fatalf("plaintext copy: %+v", r)
	}
	if h := history(t, dst, "k"); h[0].Encryption != "" || h[0].SSE != "" || read(t, dst, "k", "") != "secret" {
		t.Fatalf("target %+v", h)
	}
	noChange(t, "second pass", mustFP(t, src, dst, st, pol))
}

func TestFingerprintTargetOnlyKeys(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	dst := nativeEndpoint(t, "dst", "", storage.VersioningEnabled)
	putText(t, dst, "foreign", "x")
	putText(t, src, "k", "1")
	putText(t, src, "k", "2")
	st := NewMemStore()
	mustFP(t, src, dst, st, Policy{})
	// The source loses every version of k.
	for _, v := range history(t, src, "k") {
		del(t, src, "k", v.VersionID)
	}
	if r := mustFP(t, src, dst, st, Policy{Merge: true}); r.Changed() {
		t.Fatalf("merge deleted: %+v", r)
	}
	r := mustFP(t, src, dst, st, Policy{})
	if r.VersionsDeleted != 2 || len(history(t, dst, "k")) != 0 {
		t.Fatalf("owned key: %+v", r)
	}
	if len(history(t, dst, "foreign")) != 1 || st.Len() != 0 {
		t.Fatalf("foreign key touched or records left: %d", st.Len())
	}
	// With OwnsKey every key is the copy's.
	r = mustFP(t, src, dst, st, Policy{OwnsKey: func(string) bool { return true }})
	if r.VersionsDeleted != 1 || len(history(t, dst, "foreign")) != 0 {
		t.Fatalf("adopted key: %+v", r)
	}
}

func TestFingerprintAdoptsCopies(t *testing.T) {
	for _, versions := range []Versions{VersionsAll, VersionsCurrent} {
		t.Run(string(versions), func(t *testing.T) {
			src, dst := historyFixture(t)
			pol := Policy{Versions: versions}
			syncAll(t, src, dst, pol) // an identity copy
			st := NewMemStore()
			noChange(t, "first fingerprint pass", mustFP(t, src, dst, st, pol))
			if st.Len() != 3 {
				t.Fatalf("%d records", st.Len())
			}
			noChange(t, "second fingerprint pass", mustFP(t, src, dst, st, pol))
		})
	}
}

func TestFingerprintLockedTargetVersions(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	md := memory.New()
	newBucket(t, md, "dst", storage.VersioningEnabled, true)
	dst := Endpoint{Backend: md, Bucket: "dst"}
	v := putText(t, src, "k", "1")
	st := NewMemStore()
	mustFP(t, src, dst, st, Policy{})
	if _, err := md.UpdateObjectMeta(t.Context(), "dst", "k", v.VersionID, func(m *storage.ObjectMeta) error {
		m.LegalHold = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	del(t, src, "k", v.VersionID)
	r := mustFP(t, src, dst, st, Policy{})
	if r.VersionsDeleted != 0 || !hasConflict(r, "legal hold") || len(history(t, dst, "k")) != 1 {
		t.Fatalf("locked copy: %+v", r)
	}
	// Recorded: not tried again until something changes.
	noChange(t, "second pass", mustFP(t, src, dst, st, Policy{}))
}

func TestFingerprintCopiesChangesMadeDuringTheCopy(t *testing.T) {
	src, raw, v := nativeSource(t, storage.VersioningUnversioned)
	putText(t, raw, "k", "1")
	dst := memEndpoint(t, "dst", "", storage.VersioningUnversioned)
	st := NewMemStore()
	pol := Policy{Versions: VersionsCurrent}
	changed := false
	v.afterOpen = func(key, _ string) {
		if !changed {
			changed = true
			putText(t, raw, key, "2") // after the read: this pass copies "1"
		}
	}
	if r := mustFP(t, src, dst, st, pol); r.VersionsCopied != 1 || read(t, dst, "k", "") != "1" {
		t.Fatalf("first pass: %+v", r)
	}
	if r := mustFP(t, src, dst, st, pol); r.VersionsCopied != 1 || read(t, dst, "k", "") != "2" {
		t.Fatalf("second pass: %+v", r)
	}
	noChange(t, "third pass", mustFP(t, src, dst, st, pol))
}

func TestFingerprintDefersAKeyThatKeepsChanging(t *testing.T) {
	src, raw, v := nativeSource(t, storage.VersioningUnversioned)
	putText(t, raw, "k", "0")
	dst := memEndpoint(t, "dst", "", storage.VersioningUnversioned)
	st := NewMemStore()
	n := 0
	v.beforeOpen = func(key, _ string) {
		n++
		// A new size each time: on coarse clocks two quick writes may get
		// the same modification time.
		putText(t, raw, key, strings.Repeat("x", n+1))
	}
	r := mustFP(t, src, dst, st, Policy{Versions: VersionsCurrent})
	if !r.Deferred || r.VersionsCopied != 0 || n != maxAttempts {
		t.Fatalf("%+v after %d reads", r, n)
	}
	if rec, ok, _ := st.Get("k"); ok {
		t.Fatalf("record %+v of a key nothing was written for", rec)
	}
	v.beforeOpen = nil
	if r := mustFP(t, src, dst, st, Policy{Versions: VersionsCurrent}); r.VersionsCopied != 1 {
		t.Fatalf("calm pass: %+v", r)
	}
}

func TestFingerprintAndVersionTokens(t *testing.T) {
	now := time.Unix(1700000000, 5)
	a := storage.ObjectInfo{VersionID: "v", Size: 1, LastModified: now, ObjectMeta: storage.ObjectMeta{ETag: "e"}}
	if Fingerprint(nil, true) != "" || Fingerprint([]storage.ObjectInfo{a}, true) == "" {
		t.Fatal("empty and non-empty fingerprints")
	}
	b := a
	b.VersionID = "w"
	if Fingerprint([]storage.ObjectInfo{a}, false) != Fingerprint([]storage.ObjectInfo{b}, false) {
		t.Fatal("object fingerprints depend on version IDs")
	}
	if Fingerprint([]storage.ObjectInfo{a}, true) == Fingerprint([]storage.ObjectInfo{b}, true) {
		t.Fatal("version fingerprints ignore version IDs")
	}
	c := a
	c.LastModified = now.Add(time.Nanosecond)
	if VersionToken(a) == VersionToken(c) || !strings.HasPrefix(VersionToken(a), "v@") {
		t.Fatalf("tokens %s %s", VersionToken(a), VersionToken(c))
	}
}

func TestFingerprintNeverDeletesUnconditionally(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	m := memory.New()
	newBucket(t, m, "dst", storage.VersioningEnabled, false)
	// A target with no optional interfaces: plain writes only.
	dst := Endpoint{Backend: struct{ storage.Backend }{m}, Bucket: "dst"}
	v1 := putText(t, src, "k", "1")
	putText(t, src, "k", "2")
	st := NewMemStore()
	if r := mustFP(t, src, dst, st, Policy{}); r.PlainWrites != 2 {
		t.Fatalf("first pass: %+v", r)
	}
	del(t, src, "k", v1.VersionID)
	r := mustFP(t, src, dst, st, Policy{})
	if r.VersionsDeleted != 0 || !hasConflict(r, "cannot delete versions conditionally") || len(history(t, dst, "k")) != 2 {
		t.Fatalf("delete without ConditionalDeleter: %+v", r)
	}
	noChange(t, "second pass", mustFP(t, src, dst, st, Policy{}))
}

// A locked target version with the ID of a source version that is not
// recorded as its copy (here: the null version, after the source's null
// version was overwritten) must not be replaced by an import, merging or
// not.
func TestFingerprintNeverReplacesALockedVersion(t *testing.T) {
	for _, pol := range []Policy{{Merge: true}, {}} {
		t.Run(fmt.Sprintf("merge=%v", pol.Merge), func(t *testing.T) {
			src := memEndpoint(t, "src", "", storage.VersioningUnversioned)
			md := memory.New()
			newBucket(t, md, "dst", storage.VersioningEnabled, true)
			dst := Endpoint{Backend: md, Bucket: "dst"}
			putText(t, src, "k", "1")
			st := NewMemStore()
			mustFP(t, src, dst, st, pol)
			if _, err := md.UpdateObjectMeta(t.Context(), "dst", "k", storage.NullVersionID, func(m *storage.ObjectMeta) error {
				m.LegalHold = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			putText(t, src, "k", "22")
			r := mustFP(t, src, dst, st, pol)
			if r.Changed() || !hasConflict(r, "legal hold") {
				t.Fatalf("second pass: %+v", r)
			}
			oi, err := md.GetObjectInfo(t.Context(), "dst", "k", storage.NullVersionID)
			if got := read(t, dst, "k", storage.NullVersionID); err != nil || got != "1" || !oi.LegalHold {
				t.Fatalf("the legal-held null version was replaced: %q, %+v, %v", got, oi, err)
			}
			noChange(t, "third pass", mustFP(t, src, dst, st, pol))
		})
	}
}
