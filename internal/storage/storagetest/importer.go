package storagetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Tests of storage.VersionImporter: versions stored with the identity
// (version ID, LastModified, delete marker flag) they have on another
// backend, as replication and bucket moves between backings need.

// Version IDs in the formats of other backends. Every backend must accept
// and find all of them, whatever its own ID format.
const (
	importIDAWS    = "3HL4kqtJlcpXroDTDmJ.rmSpXd3dIbrHY" // AWS-style, contains '.'
	importIDMemory = "17f0a1b2c3d4e5f6deadbeef"          // memory-style: 16 hex stamp + 8 hex
	importIDFS     = "f39xLbWq3Rk8c2Zpdk1BbA"            // fs-style: 16 bytes base64url
)

func (e *env) versionImporter() storage.VersionImporter {
	e.t.Helper()
	vi, ok := e.b.(storage.VersionImporter)
	if !ok {
		e.t.Skip("backend does not implement storage.VersionImporter")
	}
	return vi
}

// importObject writes data through a VersionWriter and commits it with
// info. The writer is aborted on failure.
func importObject(ctx context.Context, vi storage.VersionImporter, bucket, key string, data []byte, info storage.ObjectInfo) (storage.ObjectInfo, error) {
	w, err := vi.NewVersionWriter(ctx, bucket, key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if len(data) > 0 {
		n, err := w.Write(data)
		if err == nil && n != len(data) {
			err = fmt.Errorf("short write: %d of %d bytes", n, len(data))
		}
		if err != nil {
			_ = w.Abort()
			return storage.ObjectInfo{}, fmt.Errorf("write: %w", err)
		}
	}
	got, err := w.Commit(ctx, info)
	if err != nil {
		_ = w.Abort()
		return storage.ObjectInfo{}, err
	}
	return got, nil
}

// importTime returns a distinct past time with nanoseconds and a non-UTC
// zone, which must survive an import exactly (compared with Equal).
func importTime(i int) time.Time {
	return time.Date(2019, 3, 4, 5, 6, 7, 891011121, time.FixedZone("UTC-7", -7*3600)).Add(time.Duration(i) * time.Hour)
}

// importMeta returns full metadata distinguishable by n.
func importMeta(data []byte, n int) storage.ObjectMeta {
	m := fullMeta()
	// Re-import tests replace these versions; lock preservation is covered
	// separately so their fixtures do not authorize replacing protected data.
	m.LegalHold, m.RetentionMode, m.RetainUntil = false, "", time.Time{}
	m.ETag = etagOf(data)
	m.UserMeta = map[string]string{"n": fmt.Sprint(n), "unicode": "日本語"}
	m.PartSizes, m.PartChecksums = nil, nil
	return m
}

// imp imports an object version and checks the returned info.
func (e *env) imp(bucket, key, versionID string, data []byte, lastModified time.Time, meta storage.ObjectMeta) storage.ObjectInfo {
	e.t.Helper()
	vi := e.versionImporter()
	in := storage.ObjectInfo{
		// Ignored by Commit.
		Bucket: "ignored", Key: "ignored", Size: 12345, IsLatest: false,
		VersionID:    versionID,
		LastModified: lastModified,
		ObjectMeta:   meta,
	}
	got, err := importObject(e.ctx, vi, bucket, key, data, in)
	e.noErr(fmt.Sprintf("import %s/%q@%s", bucket, key, versionID), err)
	e.wantInfo(fmt.Sprintf("Commit result of %s/%q@%s", bucket, key, versionID), storage.ObjectInfo{
		Bucket: bucket, Key: key, VersionID: versionID, IsLatest: true,
		Size: int64(len(data)), LastModified: lastModified, ObjectMeta: meta,
	}, got)
	return got
}

// impMarker imports a delete marker.
func (e *env) impMarker(bucket, key, versionID string, lastModified time.Time) storage.ObjectInfo {
	e.t.Helper()
	got, err := importObject(e.ctx, e.versionImporter(), bucket, key, nil, storage.ObjectInfo{
		VersionID: versionID, LastModified: lastModified, DeleteMarker: true,
	})
	e.noErr(fmt.Sprintf("import delete marker %s/%q@%s", bucket, key, versionID), err)
	if got.VersionID != versionID || !got.DeleteMarker || !got.IsLatest || !got.LastModified.Equal(lastModified) || got.Size != 0 {
		e.t.Errorf("Commit result of delete marker %s/%q@%s: %+v", bucket, key, versionID, got)
	}
	return got
}

// wantImported checks a stored imported version through GetObjectInfo and
// OpenObject.
func (e *env) wantImported(bucket, key, versionID string, data []byte, lastModified time.Time, meta storage.ObjectMeta, latest bool) {
	e.t.Helper()
	e.wantImportedVia(bucket, key, versionID, versionID, data, lastModified, meta, latest)
}

// wantImportedVia is wantImported for the version with ID versionID,
// addressed as lookup ("" = the latest version).
func (e *env) wantImportedVia(bucket, key, lookup, versionID string, data []byte, lastModified time.Time, meta storage.ObjectMeta, latest bool) {
	e.t.Helper()
	want := storage.ObjectInfo{
		Bucket: bucket, Key: key, VersionID: versionID, IsLatest: latest,
		Size: int64(len(data)), LastModified: lastModified, ObjectMeta: meta,
	}
	e.wantInfo(fmt.Sprintf("GetObjectInfo(%s/%q, %q)", bucket, key, lookup), want, e.info(bucket, key, lookup))
	got := e.wantContent(bucket, key, lookup, data)
	e.wantInfo(fmt.Sprintf("OpenObject(%s/%q, %q)", bucket, key, lookup), want, got)
}

// otherID returns an ID that differs from id only in its last character,
// so that it embeds the same stamp or sequence as id where the backend's
// IDs do.
func otherID(id string) string {
	c := byte('0')
	if id[len(id)-1] == '0' {
		c = '1'
	}
	return id[:len(id)-1] + string(c)
}

func testVersionImporter(e *env) {
	vi := e.versionImporter()
	e.run("LockedReplacement", func(e *env) {
		e.mkVersioned("protected", storage.VersioningEnabled)
		for _, id := range []string{"locked-id", storage.NullVersionID} {
			m := importMeta([]byte("original"), 0)
			m.LegalHold = true
			e.imp("protected", id, id, []byte("original"), importTime(0), m)
			info := storage.ObjectInfo{VersionID: id, LastModified: importTime(1)}
			_, err := importObject(e.ctx, e.versionImporter(), "protected", id, []byte("replacement"), info)
			e.wantErr("replace locked imported version", err, storage.ErrPreconditionFailed)
			e.wantImported("protected", id, id, []byte("original"), importTime(0), m, true)
		}
	})

	for _, status := range []storage.VersioningStatus{storage.VersioningUnversioned, storage.VersioningEnabled, storage.VersioningSuspended} {
		name := string(status)
		if name == "" {
			name = "Unversioned"
		}
		e.runFresh("RoundTrip"+name, func(e *env) { testImportRoundTrip(e, status) })
	}

	e.run("OrderAndReimport", func(e *env) {
		e.mkVersioned("ord", storage.VersioningEnabled)
		ids := []string{"v1", "v2", "v3", "v4", "v5"}
		for i, id := range ids {
			// LastModified decreasing: the order is the import order, not
			// the time order.
			e.imp("ord", "k", id, []byte(id), importTime(-i), importMeta([]byte(id), i))
		}
		e.wantHistory("ord", "k", vref{VersionID: "v5"}, vref{VersionID: "v4"}, vref{VersionID: "v3"}, vref{VersionID: "v2"}, vref{VersionID: "v1"})

		// Re-importing an existing ID replaces it and makes it the newest.
		m := importMeta([]byte("v2 again"), 9)
		e.imp("ord", "k", "v2", []byte("v2 again"), importTime(7), m)
		e.wantHistory("ord", "k", vref{VersionID: "v2"}, vref{VersionID: "v5"}, vref{VersionID: "v4"}, vref{VersionID: "v3"}, vref{VersionID: "v1"})
		e.wantImported("ord", "k", "v2", []byte("v2 again"), importTime(7), m, true)

		// Re-importing a generated ID replaces the generated version.
		native := e.put("ord", "k", []byte("native"))
		e.imp("ord", "k", "v6", []byte("v6"), importTime(8), importMeta([]byte("v6"), 6))
		m = importMeta([]byte("native again"), 10)
		e.imp("ord", "k", native.VersionID, []byte("native again"), importTime(9), m)
		e.wantHistory("ord", "k", vref{VersionID: native.VersionID}, vref{VersionID: "v6"}, vref{VersionID: "v2"},
			vref{VersionID: "v5"}, vref{VersionID: "v4"}, vref{VersionID: "v3"}, vref{VersionID: "v1"})
		e.wantImported("ord", "k", native.VersionID, []byte("native again"), importTime(9), m, true)

		// A normal write after imports is the newest and gets a fresh ID.
		after := e.put("ord", "k", []byte("after"))
		e.checkVersionID("write after imports", after.VersionID)
		if slices.Contains(append(ids, "v6", native.VersionID), after.VersionID) {
			e.t.Errorf("write after imports reused version ID %q", after.VersionID)
		}
		if h := e.history("ord", "k"); len(h) != 8 || h[0].VersionID != after.VersionID || !h[0].IsLatest {
			e.t.Errorf("history after a normal write: %v", h)
		}
		e.wantContent("ord", "k", "", []byte("after"))
	})

	e.run("ForeignIDCollidingWithLocalID", func(e *env) {
		// An imported ID that embeds the stamp or sequence of a local
		// version (another backend generated it) must not be confused with
		// that version.
		e.mkVersioned("col", storage.VersioningEnabled)
		native := e.put("col", "k", []byte("native"))
		foreign := otherID(native.VersionID)
		m := importMeta([]byte("foreign"), 1)
		e.imp("col", "k", foreign, []byte("foreign"), importTime(1), m)
		e.wantHistory("col", "k", vref{VersionID: foreign}, vref{VersionID: native.VersionID})
		e.wantContent("col", "k", native.VersionID, []byte("native"))
		e.wantImported("col", "k", foreign, []byte("foreign"), importTime(1), m, true)
		if got := e.walkVersions("col", "", "k", foreign); len(got) != 1 || got[0].VersionID != native.VersionID {
			e.t.Errorf("WalkVersions after the imported version: %v", refsOf(got))
		}
		res := e.del("col", "k", foreign)
		if res.VersionID != foreign {
			e.t.Errorf("DeleteObject(%q) = %+v", foreign, res)
		}
		e.wantHistory("col", "k", vref{VersionID: native.VersionID})
		_, err := e.b.GetObjectInfo(e.ctx, "col", "k", foreign)
		e.wantErr("GetObjectInfo of the deleted imported version", err, storage.ErrVersionNotFound)
	})

	e.run("NullVersions", func(e *env) {
		e.mkVersioned("nv", storage.VersioningEnabled)
		v1 := e.put("nv", "k", []byte("v1"))
		e.imp("nv", "k", storage.NullVersionID, []byte("n1"), importTime(1), importMeta([]byte("n1"), 1))
		e.imp("nv", "k", "x", []byte("x"), importTime(2), importMeta([]byte("x"), 2))
		e.wantHistory("nv", "k", vref{VersionID: "x"}, vref{VersionID: storage.NullVersionID}, vref{VersionID: v1.VersionID})
		// A null import replaces the null version wherever it is.
		m := importMeta([]byte("n2"), 3)
		e.imp("nv", "k", storage.NullVersionID, []byte("n2"), importTime(3), m)
		e.wantHistory("nv", "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: "x"}, vref{VersionID: v1.VersionID})
		e.wantImported("nv", "k", storage.NullVersionID, []byte("n2"), importTime(3), m, true)
		// ... including a null delete marker, in both directions.
		e.impMarker("nv", "k", storage.NullVersionID, importTime(4))
		e.wantHistory("nv", "k", vref{VersionID: storage.NullVersionID, DeleteMarker: true}, vref{VersionID: "x"}, vref{VersionID: v1.VersionID})
		if got := e.info("nv", "k", storage.NullVersionID); !got.DeleteMarker || !got.LastModified.Equal(importTime(4)) {
			e.t.Errorf("null delete marker: %+v", got)
		}
		e.imp("nv", "k", "y", []byte("y"), importTime(5), importMeta([]byte("y"), 5))
		m = importMeta([]byte("n3"), 6)
		e.imp("nv", "k", storage.NullVersionID, []byte("n3"), importTime(6), m)
		e.wantHistory("nv", "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: "y"}, vref{VersionID: "x"}, vref{VersionID: v1.VersionID})
		e.wantImported("nv", "k", storage.NullVersionID, []byte("n3"), importTime(6), m, true)

		// In a suspended bucket a normal write replaces an imported null
		// version, and a null import replaces a written one.
		e.setVersioning("nv", storage.VersioningSuspended)
		e.put("nv", "k", []byte("written"))
		e.wantHistory("nv", "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: "y"}, vref{VersionID: "x"}, vref{VersionID: v1.VersionID})
		e.wantContent("nv", "k", storage.NullVersionID, []byte("written"))
		e.imp("nv", "k", storage.NullVersionID, []byte("n4"), importTime(7), importMeta([]byte("n4"), 7))
		e.wantHistory("nv", "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: "y"}, vref{VersionID: "x"}, vref{VersionID: v1.VersionID})
		e.wantContent("nv", "k", "", []byte("n4"))

		// In an unversioned bucket DeleteObject("") removes an imported
		// null version.
		e.mkBucket("un")
		e.imp("un", "k", storage.NullVersionID, []byte("n"), importTime(1), importMeta([]byte("n"), 1))
		e.del("un", "k", "")
		_, err := e.b.GetObjectInfo(e.ctx, "un", "k", "")
		e.wantErr("GetObjectInfo after deleting the imported null version", err, storage.ErrObjectNotFound)
	})

	e.run("DeleteMarkers", func(e *env) {
		e.mkVersioned("dm", storage.VersioningEnabled)
		m := importMeta([]byte("a"), 1)
		e.imp("dm", "k", importIDAWS, []byte("a"), importTime(1), m)
		e.imp("dm", "other", importIDFS, []byte("o"), importTime(1), importMeta([]byte("o"), 1))
		e.impMarker("dm", "k", importIDMemory, importTime(2))
		e.wantHistory("dm", "k", vref{VersionID: importIDMemory, DeleteMarker: true}, vref{VersionID: importIDAWS})
		if keys := e.walkKeys("dm", "", ""); !slices.Equal(keys, []string{"other"}) {
			e.t.Errorf("WalkObjects with a latest imported delete marker: %q, want [other]", keys)
		}
		for _, vid := range []string{"", importIDMemory} {
			got := e.info("dm", "k", vid)
			if !got.DeleteMarker || got.VersionID != importIDMemory || !got.IsLatest || !got.LastModified.Equal(importTime(2)) {
				e.t.Errorf("GetObjectInfo(%q) of an imported delete marker: %+v", vid, got)
			}
			info, data, err := readObject(e.ctx, e.b, "dm", "k", vid)
			e.noErr("OpenObject of an imported delete marker", err)
			if !info.DeleteMarker || data != nil {
				e.t.Errorf("OpenObject(%q) of an imported delete marker: %+v", vid, info)
			}
		}
		_, err := e.b.UpdateObjectMeta(e.ctx, "dm", "k", importIDMemory, func(*storage.ObjectMeta) error { return nil })
		e.wantErr("UpdateObjectMeta of an imported delete marker", err, storage.ErrObjectNotFound)
		e.wantImported("dm", "k", importIDAWS, []byte("a"), importTime(1), m, false)

		// A delete marker with data is rejected, and nothing changes.
		_, err = importObject(e.ctx, vi, "dm", "k", []byte("data"), storage.ObjectInfo{VersionID: "bad", DeleteMarker: true, LastModified: importTime(3)})
		e.wantErr("import of a delete marker with data", err, storage.ErrInvalidName)
		e.wantHistory("dm", "k", vref{VersionID: importIDMemory, DeleteMarker: true}, vref{VersionID: importIDAWS})

		// Removing the marker makes the object current again.
		if res := e.del("dm", "k", importIDMemory); res.VersionID != importIDMemory || !res.DeleteMarker {
			e.t.Errorf("DeleteObject of the imported delete marker: %+v", res)
		}
		if keys := e.walkKeys("dm", "", ""); !slices.Equal(keys, []string{"k", "other"}) {
			e.t.Errorf("WalkObjects after removing the delete marker: %q", keys)
		}
	})

	e.run("InvalidVersionIDs", func(e *env) {
		e.mkVersioned("inv", storage.VersioningEnabled)
		for _, id := range []string{"", "a/b", strings.Repeat("a", 65), "a b", "a%2F", "ä", "a+b"} {
			_, err := importObject(e.ctx, vi, "inv", "k", []byte("x"), storage.ObjectInfo{VersionID: id, LastModified: importTime(0)})
			e.wantErr(fmt.Sprintf("import with version ID %q", id), err, storage.ErrInvalidName)
		}
		_, err := e.b.GetObjectInfo(e.ctx, "inv", "k", "")
		e.wantErr("key after rejected imports", err, storage.ErrObjectNotFound)
		long := strings.Repeat("Az09-_.", 10)[:64]
		e.imp("inv", "k", long, []byte("x"), importTime(0), importMeta([]byte("x"), 0))
		e.wantHistory("inv", "k", vref{VersionID: long})
	})

	e.run("MissingBucketAndAbort", func(e *env) {
		_, err := vi.NewVersionWriter(e.ctx, "nobucket", "k")
		e.wantErr("NewVersionWriter in a missing bucket", err, storage.ErrBucketNotFound)

		// The bucket is deleted between NewVersionWriter and Commit.
		e.mkBucket("gone")
		w, err := vi.NewVersionWriter(e.ctx, "gone", "k")
		e.noErr("NewVersionWriter", err)
		_, _ = w.Write([]byte("data"))
		e.noErr("DeleteBucket", e.b.DeleteBucket(e.ctx, "gone"))
		_, err = w.Commit(e.ctx, storage.ObjectInfo{VersionID: "v", LastModified: importTime(0)})
		e.wantErr("Commit after the bucket was deleted", err, storage.ErrBucketNotFound)
		_ = w.Abort()

		e.mkVersioned("ab", storage.VersioningEnabled)
		w, err = vi.NewVersionWriter(e.ctx, "ab", "k")
		e.noErr("NewVersionWriter", err)
		if _, err := w.Write(randBytes(200<<10, 1)); err != nil {
			e.t.Fatalf("Write: %v", err)
		}
		e.noErr("Abort", w.Abort())
		if _, err := w.Commit(e.ctx, storage.ObjectInfo{VersionID: "v", LastModified: importTime(0)}); err == nil {
			e.t.Error("Commit after Abort succeeded")
		}
		_, err = e.b.GetObjectInfo(e.ctx, "ab", "k", "")
		e.wantErr("key after an aborted import", err, storage.ErrObjectNotFound)
		if got := e.walkVersions("ab", "", "", ""); len(got) != 0 {
			e.t.Errorf("versions after an aborted import: %v", refsOf(got))
		}

		// Abort after Commit is a no-op.
		w, err = vi.NewVersionWriter(e.ctx, "ab", "k")
		e.noErr("NewVersionWriter", err)
		_, err = w.Commit(e.ctx, storage.ObjectInfo{VersionID: "v", LastModified: importTime(0)})
		e.noErr("Commit", err)
		e.noErr("Abort after Commit", w.Abort())
		e.wantHistory("ab", "k", vref{VersionID: "v"})
		e.wantContent("ab", "k", "v", []byte{})
	})

	e.run("LargeObject", func(e *env) {
		e.mkBucket("big")
		data := randBytes(3<<20+17, 7)
		m := importMeta(data, 1)
		e.imp("big", "k", importIDFS, data, importTime(1), m)
		e.wantImported("big", "k", importIDFS, data, importTime(1), m, true)
	})
}

// testImportRoundTrip imports versions with IDs of foreign formats into a
// bucket with the given versioning status and exercises every method that
// takes a version ID on them.
func testImportRoundTrip(e *env, status storage.VersioningStatus) {
	const bkt = "rt"
	e.mkVersioned(bkt, status)
	ids := []string{importIDAWS, importIDMemory, importIDFS}
	datas := [][]byte{[]byte("aws version"), []byte("memory version"), {}}
	metas := make([]storage.ObjectMeta, len(ids))
	for i, id := range ids {
		metas[i] = importMeta(datas[i], i)
		e.imp(bkt, "k", id, datas[i], importTime(i), metas[i])
	}
	e.imp(bkt, "l", importIDAWS, []byte("l"), importTime(0), importMeta([]byte("l"), 9))
	e.imp(bkt, "j", "j1", []byte("j"), importTime(0), importMeta([]byte("j"), 8))

	e.wantHistory(bkt, "k", vref{VersionID: importIDFS}, vref{VersionID: importIDMemory}, vref{VersionID: importIDAWS})
	for i, id := range ids {
		e.wantImported(bkt, "k", id, datas[i], importTime(i), metas[i], i == len(ids)-1)
	}
	e.wantImportedVia(bkt, "k", "", importIDFS, datas[2], importTime(2), metas[2], true)
	e.wantImported(bkt, "l", importIDAWS, []byte("l"), importTime(0), importMeta([]byte("l"), 9), true)

	// WalkObjects reports the newest imported version of each key.
	objs := e.walkObjects(bkt, "", "")
	if got := keysOf(objs); !slices.Equal(got, []string{"j", "k", "l"}) {
		e.t.Fatalf("WalkObjects keys = %q", got)
	}
	if o := objs[1]; o.VersionID != importIDFS || !o.IsLatest || !o.LastModified.Equal(importTime(2)) || o.ETag != metas[2].ETag {
		e.t.Errorf("WalkObjects entry of k: %+v", o)
	}

	// WalkVersions resumes after each imported version ID.
	all := []vref{
		{Key: "j", VersionID: "j1", IsLatest: true},
		{Key: "k", VersionID: importIDFS, IsLatest: true},
		{Key: "k", VersionID: importIDMemory},
		{Key: "k", VersionID: importIDAWS},
		{Key: "l", VersionID: importIDAWS, IsLatest: true},
	}
	if got := refsOf(e.walkVersions(bkt, "", "", "")); !slices.Equal(got, all) {
		e.t.Errorf("WalkVersions = %v, want %v", got, all)
	}
	for i := 1; i <= 3; i++ {
		got := refsOf(e.walkVersions(bkt, "", "k", all[i].VersionID))
		if want := all[i+1:]; !slices.Equal(got, want) {
			e.t.Errorf("WalkVersions after k@%s = %v, want %v", all[i].VersionID, got, want)
		}
	}
	// The same through ListObjectVersions paging, one entry per page.
	var paged []vref
	p := storage.VersionListParams{MaxKeys: 1}
	for range 10 {
		res, err := storage.ListObjectVersions(e.ctx, e.b, bkt, p)
		e.noErr("ListObjectVersions", err)
		paged = append(paged, refsOf(res.Versions)...)
		if !res.IsTruncated {
			break
		}
		p.KeyMarker, p.VersionIDMarker = res.NextKeyMarker, res.NextVersionIDMarker
	}
	if len(paged) != len(all) {
		e.t.Errorf("paged ListObjectVersions returned %d versions, want %d: %v", len(paged), len(all), paged)
	} else {
		for i := range all {
			if paged[i].Key != all[i].Key || paged[i].VersionID != all[i].VersionID {
				e.t.Errorf("paged ListObjectVersions entry %d = %v, want %v", i, paged[i], all[i])
			}
		}
	}

	// UpdateObjectMeta (and UpdateObjectMetaIf) find imported versions.
	got, err := e.b.UpdateObjectMeta(e.ctx, bkt, "k", importIDMemory, func(m *storage.ObjectMeta) error {
		m.ContentType = "image/png"
		return nil
	})
	e.noErr("UpdateObjectMeta of an imported version", err)
	metas[1].ContentType = "image/png"
	if got.VersionID != importIDMemory || got.IsLatest || got.ContentType != "image/png" || !got.LastModified.Equal(importTime(1)) {
		e.t.Errorf("UpdateObjectMeta result: %+v", got)
	}
	e.wantImported(bkt, "k", importIDMemory, datas[1], importTime(1), metas[1], false)
	if mu, ok := e.b.(storage.ConditionalMetaUpdater); ok {
		var seen storage.ObjectInfo
		_, err := mu.UpdateObjectMetaIf(e.ctx, bkt, "k", importIDAWS, func(o storage.ObjectInfo) error {
			seen = o
			return errCheckRefused
		}, func(*storage.ObjectMeta) error { return nil })
		if !errors.Is(err, errCheckRefused) || seen.VersionID != importIDAWS {
			e.t.Errorf("UpdateObjectMetaIf of an imported version: err %v, check saw %+v", err, seen)
		}
	}

	// DeleteObject and the conditional deletes find imported versions.
	if cd, ok := e.b.(storage.ConditionalDeleter); ok {
		var seen storage.ObjectInfo
		_, err := cd.DeleteObjectIf(e.ctx, bkt, "k", importIDAWS, func(o storage.ObjectInfo) error {
			seen = o
			return errCheckRefused
		})
		if !errors.Is(err, errCheckRefused) || seen.VersionID != importIDAWS || seen.IsLatest {
			e.t.Errorf("DeleteObjectIf of an imported version: err %v, check saw %+v", err, seen)
		}
	}
	if cd, ok := e.b.(storage.CheckedDeleter); ok {
		res, err := cd.DeleteObjectChecked(e.ctx, bkt, "k", importIDAWS, func(o storage.ObjectInfo, found bool) error {
			if !found || o.VersionID != importIDAWS {
				return errCheckRefused
			}
			return nil
		})
		e.noErr("DeleteObjectChecked of an imported version", err)
		if res.VersionID != importIDAWS || res.DeleteMarker {
			e.t.Errorf("DeleteObjectChecked result: %+v", res)
		}
	} else {
		e.del(bkt, "k", importIDAWS)
	}
	e.wantHistory(bkt, "k", vref{VersionID: importIDFS}, vref{VersionID: importIDMemory})
	res := e.del(bkt, "k", importIDFS)
	if res.VersionID != importIDFS || res.DeleteMarker {
		e.t.Errorf("DeleteObject of the latest imported version: %+v", res)
	}
	e.wantHistory(bkt, "k", vref{VersionID: importIDMemory})
	e.wantImportedVia(bkt, "k", "", importIDMemory, datas[1], importTime(1), metas[1], true)
	_, err = e.b.GetObjectInfo(e.ctx, bkt, "k", importIDFS)
	e.wantErr("GetObjectInfo of a deleted imported version", err, storage.ErrVersionNotFound)
	// The marker of a deleted imported version still resumes the walk
	// after its key.
	if got := refsOf(e.walkVersions(bkt, "k", "k", importIDFS)); len(got) > 1 {
		e.t.Errorf("WalkVersions after a deleted imported version: %v", got)
	}

	// A normal write after the imports becomes the newest version.
	w := e.put(bkt, "k", []byte("written"))
	switch status {
	case storage.VersioningEnabled:
		e.checkVersionID("write after imports", w.VersionID)
		if slices.Contains(ids, w.VersionID) {
			e.t.Errorf("write after imports reused version ID %q", w.VersionID)
		}
		e.wantHistory(bkt, "k", vref{VersionID: w.VersionID}, vref{VersionID: importIDMemory})
	default:
		if w.VersionID != storage.NullVersionID {
			e.t.Errorf("write after imports in a %q bucket: version ID %q, want null", status, w.VersionID)
		}
		e.wantHistory(bkt, "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: importIDMemory})
	}
	e.wantContent(bkt, "k", "", []byte("written"))
	e.wantContent(bkt, "k", importIDMemory, datas[1])
	if keys := e.walkKeys(bkt, "", ""); !slices.Equal(keys, []string{"j", "k", "l"}) {
		e.t.Errorf("WalkObjects keys at the end = %q", keys)
	}
}
