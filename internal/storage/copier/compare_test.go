package copier

import (
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestSameVersionAndContent(t *testing.T) {
	now := time.Now()
	a := storage.ObjectInfo{VersionID: "v1", Size: 3, LastModified: now, ObjectMeta: storage.ObjectMeta{ETag: "e", Encryption: storage.EncryptionAES256}}
	b := a
	b.LastModified = now.In(time.FixedZone("x", 3600)) // same instant
	if !SameVersion(a, b) || !SameContent(a, b) {
		t.Fatal("identical versions differ")
	}
	b.Encryption = ""
	if SameVersion(a, b) {
		t.Fatal("SameVersion ignores encryption")
	}
	if !SameContent(a, b) {
		t.Fatal("SameContent compares encryption")
	}
	for name, mod := range map[string]func(*storage.ObjectInfo){
		"id":     func(o *storage.ObjectInfo) { o.VersionID = "v2" },
		"size":   func(o *storage.ObjectInfo) { o.Size++ },
		"etag":   func(o *storage.ObjectInfo) { o.ETag = "f" },
		"time":   func(o *storage.ObjectInfo) { o.LastModified = o.LastModified.Add(time.Nanosecond) },
		"marker": func(o *storage.ObjectInfo) { o.DeleteMarker = true },
	} {
		c := a
		mod(&c)
		if SameVersion(a, c) || SameContent(a, c) {
			t.Errorf("%s: versions compare equal", name)
		}
	}
}

func TestMetaEqual(t *testing.T) {
	until := time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC)
	a := storage.ObjectMeta{ETag: "e", UserMeta: map[string]string{}, PartChecksums: []map[string]string{{}}, RetainUntil: until}
	b := storage.ObjectMeta{ETag: "e", PartChecksums: []map[string]string{nil}, RetainUntil: until.In(time.FixedZone("y", -7200))}
	if !MetaEqual(a, b) {
		t.Fatalf("equal metadata compare different:\n%+v\n%+v", NormMeta(a), NormMeta(b))
	}
	b.Tags = map[string]string{"k": "v"}
	if MetaEqual(a, b) {
		t.Fatal("different tags compare equal")
	}
}

func TestCloneMetaIsDeep(t *testing.T) {
	m := storage.ObjectMeta{
		UserMeta: map[string]string{"a": "1"}, Tags: map[string]string{"t": "1"}, Checksums: map[string]string{"CRC32": "x"},
		PartSizes: []int64{1, 2}, PartChecksums: []map[string]string{{"CRC32": "y"}},
	}
	c := CloneMeta(m)
	c.UserMeta["a"], c.Tags["t"], c.Checksums["CRC32"] = "2", "2", "z"
	c.PartSizes[0], c.PartChecksums[0]["CRC32"] = 9, "z"
	if m.UserMeta["a"] != "1" || m.Tags["t"] != "1" || m.Checksums["CRC32"] != "x" || m.PartSizes[0] != 1 || m.PartChecksums[0]["CRC32"] != "y" {
		t.Fatalf("clone shares state with the original: %+v", m)
	}
}

func TestKeyVersionsStopsAtTheKey(t *testing.T) {
	e := memEndpoint(t, "b", "", storage.VersioningEnabled)
	putText(t, e, "a", "1")
	putText(t, e, "a", "2")
	putText(t, e, "a/b", "3")
	putText(t, e, "ab", "4")
	vs := history(t, e, "a")
	if len(vs) != 2 || vs[0].Key != "a" || vs[1].Key != "a" || !vs[0].IsLatest {
		t.Fatalf("KeyVersions(a) = %+v", vs)
	}
	if vs := history(t, e, "missing"); len(vs) != 0 {
		t.Fatalf("KeyVersions(missing) = %+v", vs)
	}
	got, err := keyLatest(t.Context(), e.Backend, e.Bucket, "a")
	if err != nil || len(got) != 1 || got[0].VersionID != vs0(vs) {
		t.Fatalf("keyLatest(a) = %+v, %v", got, err)
	}
	del(t, e, "a", "")
	if got, err := keyLatest(t.Context(), e.Backend, e.Bucket, "a"); err != nil || len(got) != 0 {
		t.Fatalf("keyLatest of a deleted key = %+v, %v", got, err)
	}
}

func vs0(vs []storage.ObjectInfo) string { return vs[0].VersionID }
