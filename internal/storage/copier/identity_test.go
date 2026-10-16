package copier

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// historyFixture: a versioned source with a history (versions, a delete
// marker, a key with user metadata) and an empty versioned target.
func historyFixture(t *testing.T) (src, dst Endpoint) {
	src = memEndpoint(t, "src", "data/", storage.VersioningEnabled)
	dst = memEndpoint(t, "dst", "copy/", storage.VersioningEnabled)
	putText(t, src, "data/a", "a1")
	put(t, src, "data/a", "a2", storage.ObjectMeta{ContentType: "text/x", UserMeta: map[string]string{"k": "v"}, Tags: map[string]string{"t": "1"}})
	putText(t, src, "data/b", "b1")
	del(t, src, "data/b", "")
	putText(t, src, "data/b", "b2")
	putText(t, src, "data/c", "c1")
	return src, dst
}

func TestIdentityCopiesHistories(t *testing.T) {
	src, dst := historyFixture(t)
	r := syncAll(t, src, dst, Policy{})
	if r.VersionsCopied != 6 || r.BytesCopied != 10 || r.VersionsDeleted != 0 || r.PlainWrites != 0 {
		t.Fatalf("first pass: %+v", r)
	}
	for _, k := range []string{"a", "b", "c"} {
		sameHistory(t, src, "data/"+k, dst, "copy/"+k)
	}
	noChange(t, "second pass", syncAll(t, src, dst, Policy{}))
}

func TestIdentityFollowsChanges(t *testing.T) {
	src, dst := historyFixture(t)
	syncAll(t, src, dst, Policy{})
	// A new version, a version deleted from the middle of a history, a
	// metadata change, a key deleted entirely.
	putText(t, src, "data/a", "a3")
	b := history(t, src, "data/b")
	del(t, src, "data/b", b[1].VersionID) // the delete marker
	c := history(t, src, "data/c")
	if _, err := src.Backend.UpdateObjectMeta(t.Context(), src.Bucket, "data/c", c[0].VersionID, func(m *storage.ObjectMeta) error {
		m.Tags = map[string]string{"new": "tag"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := syncAll(t, src, dst, Policy{})
	if r.VersionsCopied != 1 || r.VersionsDeleted != 1 || r.MetaUpdated != 1 {
		t.Fatalf("changes: %+v", r)
	}
	for _, k := range []string{"a", "b", "c"} {
		sameHistory(t, src, "data/"+k, dst, "copy/"+k)
	}
	noChange(t, "after the changes", syncAll(t, src, dst, Policy{}))
}

func TestIdentityRepairsOrder(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	putText(t, src, "k", "1")
	put2 := putText(t, src, "k", "2")
	putText(t, src, "k", "3")
	syncAll(t, src, dst, Policy{})
	// Re-import the middle version on the target: it moves to the top.
	w, err := dst.Backend.(storage.VersionImporter).NewVersionWriter(t.Context(), dst.Bucket, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("2")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(t.Context(), put2); err != nil {
		t.Fatal(err)
	}
	r := syncAll(t, src, dst, Policy{})
	// The oldest version is kept; the two above it are replaced.
	if r.VersionsDeleted != 2 || r.VersionsCopied != 2 {
		t.Fatalf("repair: %+v", r)
	}
	sameHistory(t, src, "k", dst, "k")
}

func TestIdentityTargetOnlyKeys(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	putText(t, dst, "mine", "x")
	putText(t, dst, "theirs", "y")
	owns := func(k string) bool { return k == "mine" }
	r := syncAll(t, src, dst, Policy{OwnsKey: owns, Merge: true})
	noChange(t, "merge", r)
	r = syncAll(t, src, dst, Policy{OwnsKey: owns})
	if r.VersionsDeleted != 1 || len(history(t, dst, "mine")) != 0 || len(history(t, dst, "theirs")) != 1 {
		t.Fatalf("owned target-only key: %+v", r)
	}
}

func TestIdentityMergeKeepsTargetVersions(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	putText(t, dst, "k", "foreign")
	putText(t, src, "k", "1")
	putText(t, src, "k", "2")
	r := syncAll(t, src, dst, Policy{Merge: true})
	if r.VersionsCopied != 2 || r.VersionsDeleted != 0 {
		t.Fatalf("merge: %+v", r)
	}
	if got := ids(history(t, dst, "k")); len(got) != 3 {
		t.Fatalf("target history %v", got)
	}
	if read(t, dst, "k", "") != "2" {
		t.Fatal("the source's current version is not current on the target")
	}
	noChange(t, "second merge", syncAll(t, src, dst, Policy{Merge: true}))
	// Without merge the foreign version goes.
	r = syncAll(t, src, dst, Policy{})
	if r.VersionsDeleted != 1 || r.VersionsCopied != 0 {
		t.Fatalf("after merge: %+v", r)
	}
	sameHistory(t, src, "k", dst, "k")
}

func TestIdentityMergeOverwritesNullVersions(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningUnversioned)
	dst := memEndpoint(t, "dst", "", storage.VersioningUnversioned)
	putText(t, dst, "k", "old")
	putText(t, src, "k", "new")
	r := syncAll(t, src, dst, Policy{Merge: true})
	if r.VersionsCopied != 1 || read(t, dst, "k", "") != "new" {
		t.Fatalf("merge over a null version: %+v", r)
	}
	sameHistory(t, src, "k", dst, "k")
}

func TestIdentityCurrentVersions(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	putText(t, src, "k", "1")
	v2 := putText(t, src, "k", "2")
	putText(t, src, "gone", "x")
	del(t, src, "gone", "")
	pol := Policy{Versions: VersionsCurrent, OwnsKey: func(string) bool { return true }}
	r := syncAll(t, src, dst, pol)
	if r.VersionsCopied != 1 {
		t.Fatalf("first pass: %+v", r)
	}
	h := history(t, dst, "k")
	if len(h) != 1 || h[0].VersionID != storage.NullVersionID || !h[0].LastModified.Equal(v2.LastModified) || h[0].ETag != v2.ETag {
		t.Fatalf("target %+v, want the null version of %+v", h, v2)
	}
	noChange(t, "second pass", syncAll(t, src, dst, pol))

	v3 := putText(t, src, "k", "3")
	if r := syncAll(t, src, dst, pol); r.VersionsCopied != 1 || r.VersionsDeleted != 0 {
		t.Fatalf("new version: %+v", r)
	}
	if h := history(t, dst, "k"); len(h) != 1 || !h[0].LastModified.Equal(v3.LastModified) || read(t, dst, "k", "") != "3" {
		t.Fatalf("target after an update: %+v", h)
	}

	// The source's current version becomes a delete marker.
	del(t, src, "k", "")
	if r := syncAll(t, src, dst, Policy{Versions: VersionsCurrent, Merge: true, OwnsKey: pol.OwnsKey}); r.Changed() {
		t.Fatalf("merge deleted: %+v", r)
	}
	if r := syncAll(t, src, dst, Policy{Versions: VersionsCurrent}); r.Changed() {
		t.Fatalf("deleted a key it does not own: %+v", r)
	}
	if r := syncAll(t, src, dst, pol); r.VersionsDeleted != 1 || len(history(t, dst, "k")) != 0 {
		t.Fatalf("delete: %+v, target %v", r, ids(history(t, dst, "k")))
	}
}

func TestIdentityCurrentHidesForeignVersions(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	putText(t, dst, "k", "foreign")
	pol := Policy{Versions: VersionsCurrent, OwnsKey: func(string) bool { return true }}
	r := syncAll(t, src, dst, pol)
	h := history(t, dst, "k")
	if r.VersionsDeleted != 1 || len(h) != 2 || !h[0].DeleteMarker {
		t.Fatalf("owned key with a foreign current version: %+v, target %v", r, ids(h))
	}
	noChange(t, "second pass", syncAll(t, src, dst, pol))
}

func TestIdentityNeedsAnImporter(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	putText(t, src, "k", "1")
	for name, dst := range map[string]Endpoint{
		"refuses": nativeEndpoint(t, "dst", "", storage.VersioningEnabled),
		"bare":    {Backend: bareTarget{Backend: memory.New(), ConditionalDeleter: nil}, Bucket: "dst"},
	} {
		if name == "bare" {
			m := memory.New()
			newBucket(t, m, "dst", storage.VersioningEnabled, false)
			dst.Backend = bareTarget{Backend: m, ConditionalDeleter: m}
		}
		_, err := syncAllErr(t, src, dst, Policy{})
		if !errors.Is(err, ErrNoImport) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s: %v, want ErrNoImport", name, err)
		}
	}
}

func TestIdentityKeyConflicts(t *testing.T) {
	src := memEndpoint(t, "src", "p/", storage.VersioningEnabled)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	long := "p/" + fmt.Sprintf("%01020d", 0)
	putText(t, src, long, "x")
	putText(t, src, "p/", "the prefix itself")
	dst.Prefix = "longer-prefix/"
	r := syncAll(t, src, dst, Policy{})
	if len(r.Conflicts) != 1 || r.Conflicts[0].Key != long || r.VersionsCopied != 1 {
		t.Fatalf("long key: %+v", r)
	}
	dst.Prefix = ""
	r = syncAll(t, src, dst, Policy{})
	if !hasConflict(r, "empty target key") {
		t.Fatalf("empty target key: %+v", r)
	}
}

func TestIdentityEncryptionPreserve(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	enc := storage.WriteOptions{Encryption: storage.EncryptionAES256}
	put(t, src, "k", "secret", storage.ObjectMeta{SSE: "AES256"}, enc)
	putText(t, src, "plain", "x")
	syncAll(t, src, dst, Policy{})
	h := history(t, dst, "k")
	if len(h) != 1 || h[0].Encryption != storage.EncryptionAES256 || h[0].SSE != "AES256" {
		t.Fatalf("target %+v", h)
	}
	if h := history(t, dst, "plain"); h[0].Encryption != "" {
		t.Fatalf("plain version encrypted: %+v", h)
	}
	sameHistory(t, src, "k", dst, "k")
	noChange(t, "second pass", syncAll(t, src, dst, Policy{}))
}

func TestIdentityEncryptionTarget(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	put(t, src, "enc", "secret", storage.ObjectMeta{SSE: "AES256"}, storage.WriteOptions{Encryption: storage.EncryptionAES256})
	putText(t, src, "plain", "x")

	// Decrypting needs AllowPlaintext.
	pol := Policy{Encryption: EncryptionTarget}
	if _, err := syncAllErr(t, src, dst, pol); !errors.Is(err, ErrPlaintext) {
		t.Fatalf("decrypting without AllowPlaintext: %v", err)
	}
	pol.AllowPlaintext = true
	syncAll(t, src, dst, pol)
	h := history(t, dst, "enc")
	if len(h) != 1 || h[0].Encryption != "" || h[0].SSE != "" || read(t, dst, "enc", "") != "secret" {
		t.Fatalf("decrypted copy %+v", h)
	}
	noChange(t, "second decrypting pass", syncAll(t, src, dst, pol))

	// Encrypting with the target's choice; existing copies stay as they are.
	pol = Policy{Encryption: EncryptionTarget, TargetEncryption: func(storage.ObjectInfo) string { return storage.EncryptionAES256 }}
	noChange(t, "provider change", syncAll(t, src, dst, pol))
	dst2 := memEndpoint(t, "dst2", "", storage.VersioningEnabled)
	syncAll(t, src, dst2, pol)
	if h := history(t, dst2, "plain"); h[0].Encryption != storage.EncryptionAES256 {
		t.Fatalf("target encryption not applied: %+v", h)
	}
	noChange(t, "second encrypting pass", syncAll(t, src, dst2, pol))
}

func TestIdentityEncryptionUnsupported(t *testing.T) {
	m := memory.New()
	newBucket(t, m, "src", storage.VersioningEnabled, false)
	raw := Endpoint{Backend: m, Bucket: "src"}
	putText(t, raw, "k", "archived")
	// The source reports restic, which the memory target cannot store.
	src := Endpoint{Backend: &view{Backend: m, encryption: storage.EncryptionRestic}, Bucket: "src"}
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	if _, err := syncAllErr(t, src, dst, Policy{}); !errors.Is(err, ErrEncryptionUnsupported) || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("restic to memory: %v", err)
	}
	if len(history(t, dst, "k")) != 0 {
		t.Fatal("stored something")
	}
	pol := Policy{AllowPlaintext: true}
	syncAll(t, src, dst, pol)
	if h := history(t, dst, "k"); len(h) != 1 || h[0].Encryption != "" {
		t.Fatalf("plaintext copy %+v", h)
	}
	noChange(t, "second plaintext pass", syncAll(t, src, dst, pol))
}

func TestIdentityCompression(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningEnabled)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	body := fmt.Sprintf("%0500d", 7)
	put(t, src, "z", body, storage.ObjectMeta{}, storage.WriteOptions{Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 6}})
	putText(t, src, "plain", body)
	syncAll(t, src, dst, Policy{})
	if h := history(t, dst, "z"); h[0].Compression != storage.CompressionDeflate || read(t, dst, "z", "") != body {
		t.Fatalf("compressed source: %+v", h)
	}
	if h := history(t, dst, "plain"); h[0].Compression != "" {
		t.Fatalf("plain source compressed: %+v", h)
	}
	dst2 := memEndpoint(t, "dst2", "", storage.VersioningEnabled)
	syncAll(t, src, dst2, Policy{Compression: CompressionTarget})
	if h := history(t, dst2, "z"); h[0].Compression != "" {
		t.Fatalf("target compression without a choice: %+v", h)
	}
	dst3 := memEndpoint(t, "dst3", "", storage.VersioningEnabled)
	syncAll(t, src, dst3, Policy{Compression: CompressionTarget, TargetCompression: func(storage.ObjectInfo) storage.Compression {
		return storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1}
	}})
	if h := history(t, dst3, "plain"); h[0].Compression != storage.CompressionDeflate {
		t.Fatalf("target compression: %+v", h)
	}
	noChange(t, "second pass", syncAll(t, src, dst3, Policy{Compression: CompressionTarget}))
}

// lockFixture: a source and a target with object lock.
func lockFixture(t *testing.T) (src, dst Endpoint, until time.Time) {
	ms, md := memory.New(), memory.New()
	newBucket(t, ms, "src", storage.VersioningEnabled, true)
	newBucket(t, md, "dst", storage.VersioningEnabled, true)
	return Endpoint{Backend: ms, Bucket: "src"}, Endpoint{Backend: md, Bucket: "dst"}, time.Now().Add(time.Hour).UTC()
}

func TestIdentityNeverDeletesLockedVersions(t *testing.T) {
	src, dst, until := lockFixture(t)
	putText(t, src, "k", "1")
	putText(t, src, "k", "2")
	syncAll(t, src, dst, Policy{})
	// A locked version only the target has, and a legal hold on a copy of
	// a version the source loses.
	put(t, dst, "k", "locked", storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: until})
	h := history(t, src, "k")
	if _, err := dst.Backend.UpdateObjectMeta(t.Context(), dst.Bucket, "k", h[1].VersionID, func(m *storage.ObjectMeta) error {
		m.LegalHold = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	del(t, src, "k", h[1].VersionID)
	r := syncAll(t, src, dst, Policy{})
	if r.VersionsDeleted != 0 || len(r.Conflicts) != 2 || !hasConflict(r, "legal hold") || !hasConflict(r, "GOVERNANCE retention") {
		t.Fatalf("locked versions: %+v", r)
	}
	if got := len(history(t, dst, "k")); got != 3 {
		t.Fatalf("target has %d versions, want 3", got)
	}
	// No churn: the next pass only reports the conflicts again.
	r = syncAll(t, src, dst, Policy{})
	if r.Changed() || len(r.Conflicts) != 2 {
		t.Fatalf("second pass: %+v", r)
	}
	// Retention expired, hold kept: one deletion.
	later := func() time.Time { return until.Add(time.Minute) }
	r = syncAll(t, src, dst, Policy{Now: later})
	if r.VersionsDeleted != 1 || len(r.Conflicts) != 1 {
		t.Fatalf("after the retention: %+v", r)
	}
}

func TestIdentityNeverReplacesALockedNullVersion(t *testing.T) {
	src := memEndpoint(t, "src", "", storage.VersioningUnversioned)
	md := memory.New()
	newBucket(t, md, "dst", storage.VersioningEnabled, true)
	dst := Endpoint{Backend: md, Bucket: "dst"}
	putText(t, src, "k", "1")
	syncAll(t, src, dst, Policy{})
	if _, err := md.UpdateObjectMeta(t.Context(), "dst", "k", storage.NullVersionID, func(m *storage.ObjectMeta) error {
		m.LegalHold = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	putText(t, src, "k", "2") // the source's null version changes
	for _, pol := range []Policy{{}, {Merge: true}, {Versions: VersionsCurrent}, {Versions: VersionsCurrent, Merge: true}} {
		r := syncAll(t, src, dst, pol)
		if r.Changed() || !hasConflict(r, "legal hold") {
			t.Fatalf("%+v: %+v", pol, r)
		}
		if read(t, dst, "k", "") != "1" {
			t.Fatalf("%+v: the locked version was replaced", pol)
		}
	}
}

func TestIdentityLockWeakeningUpdatesAreConflicts(t *testing.T) {
	src, dst, until := lockFixture(t)
	v := put(t, src, "k", "1", storage.ObjectMeta{RetentionMode: "COMPLIANCE", RetainUntil: until})
	syncAll(t, src, dst, Policy{})
	sameHistory(t, src, "k", dst, "k")
	// Shorten the source's retention (as a governance bypass could).
	if _, err := src.Backend.UpdateObjectMeta(t.Context(), src.Bucket, "k", v.VersionID, func(m *storage.ObjectMeta) error {
		m.RetainUntil = until.Add(-time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := syncAll(t, src, dst, Policy{})
	if r.MetaUpdated != 0 || !hasConflict(r, "would shorten") {
		t.Fatalf("shortened retention: %+v", r)
	}
	// Extending is fine.
	if _, err := src.Backend.UpdateObjectMeta(t.Context(), src.Bucket, "k", v.VersionID, func(m *storage.ObjectMeta) error {
		m.RetainUntil = until.Add(time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if r := syncAll(t, src, dst, Policy{}); r.MetaUpdated != 1 || len(r.Conflicts) != 0 {
		t.Fatalf("extended retention: %+v", r)
	}
	sameHistory(t, src, "k", dst, "k")
}

func TestIdentityRetentionNeedsObjectLock(t *testing.T) {
	src, _, until := lockFixture(t)
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	put(t, src, "k", "1", storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: until, LegalHold: true})
	if _, err := syncAllErr(t, src, dst, Policy{}); !errors.Is(err, ErrRetention) {
		t.Fatalf("retention without object lock: %v", err)
	}
	pol := Policy{DropRetention: true}
	syncAll(t, src, dst, pol)
	h := history(t, dst, "k")
	if len(h) != 1 || h[0].RetentionMode != "" || !h[0].RetainUntil.IsZero() || h[0].LegalHold {
		t.Fatalf("retention kept: %+v", h)
	}
	noChange(t, "second pass", syncAll(t, src, dst, pol))
}

func TestIdentitySourceChangesDuringTheCopy(t *testing.T) {
	m := memory.New()
	newBucket(t, m, "src", storage.VersioningUnversioned, false)
	raw := Endpoint{Backend: m, Bucket: "src"}
	putText(t, raw, "k", "0")
	n := 0
	v := &view{Backend: m}
	src := Endpoint{Backend: v, Bucket: "src"}
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)

	// Overwritten once while being read: copied on the next attempt.
	v.beforeOpen = func(key, _ string) {
		if n < 1 {
			n++
			putText(t, raw, key, fmt.Sprintf("changed %d", n))
		}
	}
	r := syncAll(t, src, dst, Policy{})
	if r.Deferred || r.VersionsCopied != 1 || read(t, dst, "k", "") != "changed 1" {
		t.Fatalf("one change: %+v", r)
	}
	sameHistory(t, raw, "k", dst, "k")

	// Overwritten on every read: deferred after four attempts.
	putText(t, raw, "k", "x")
	n = 0
	v.beforeOpen = func(key, _ string) {
		n++
		putText(t, raw, key, fmt.Sprintf("again %d", n))
	}
	r = syncAll(t, src, dst, Policy{})
	if !r.Deferred || n != maxAttempts {
		t.Fatalf("constant changes: %+v after %d reads", r, n)
	}
}

func TestPolicyValidate(t *testing.T) {
	for _, p := range []Policy{{Versions: "some"}, {Encryption: "x"}, {Compression: "y"}} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
		if _, err := SyncKey(t.Context(), Endpoint{}, Endpoint{}, KeyPair{}, p); err == nil {
			t.Errorf("SyncKey accepted %+v", p)
		}
	}
}
