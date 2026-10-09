package storagetest

import (
	"fmt"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func versioningTests() []testCase {
	return []testCase{
		{"UnversionedNullVersion", testVersioningUnversioned},
		{"EnabledUniqueIDsAndHistory", testVersioningEnabledHistory},
		{"EnabledGetByVersion", testVersioningEnabledGetByVersion},
		{"EnabledDeleteMarker", testVersioningEnabledDeleteMarker},
		{"EnabledRemoveDeleteMarkerRestores", testVersioningRemoveMarkerRestores},
		{"EnabledDeleteNonLatestVersion", testVersioningDeleteNonLatest},
		{"EnabledDeleteLatestVersion", testVersioningDeleteLatest},
		{"EnabledDeleteAllVersions", testVersioningDeleteAll},
		{"EnabledDeleteMissingKeyCreatesMarker", testVersioningDeleteMissingKey},
		{"EnabledStackedDeleteMarkers", testVersioningStackedMarkers},
		{"UnversionedToEnabledKeepsNull", testVersioningUnversionedToEnabled},
		{"Suspended", testVersioningSuspended},
		{"SuspendedReplacesNoncurrentNull", testVersioningSuspendedReplacesNoncurrentNull},
		{"SuspendedDeleteKeepsVersions", testVersioningSuspendedDeleteKeeps},
		{"NotFoundErrors", testVersioningNotFoundErrors},
	}
}

func testVersioningUnversioned(e *env) {
	e.mkBucket("b")
	for i := range 3 {
		info := e.put("b", "k", fmt.Appendf(nil, "v%d", i))
		if info.VersionID != storage.NullVersionID {
			e.t.Errorf("put #%d VersionID = %q, want %q", i, info.VersionID, storage.NullVersionID)
		}
	}
	// Overwrites replace the null version: exactly one version remains.
	e.wantHistory("b", "k", vref{VersionID: storage.NullVersionID})
	e.wantContent("b", "k", storage.NullVersionID, []byte("v2"))
}

// putVersions writes n versions of key and returns their infos, oldest first.
func (e *env) putVersions(bucket, key string, n int) []storage.ObjectInfo {
	e.t.Helper()
	out := make([]storage.ObjectInfo, n)
	for i := range n {
		out[i] = e.put(bucket, key, fmt.Appendf(nil, "%s version %d", key, i))
	}
	return out
}

func versionData(key string, i int) []byte { return fmt.Appendf(nil, "%s version %d", key, i) }

func testVersioningEnabledHistory(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	vs := e.putVersions("b", "k", 5)
	seen := map[string]bool{}
	for i, v := range vs {
		e.checkVersionID(fmt.Sprintf("put #%d", i), v.VersionID)
		if seen[v.VersionID] {
			e.t.Errorf("put #%d reused version ID %q", i, v.VersionID)
		}
		seen[v.VersionID] = true
		if !v.IsLatest {
			e.t.Errorf("put #%d: Commit IsLatest = false", i)
		}
	}
	res := e.del("b", "k", "")
	e.checkVersionID("delete marker", res.VersionID)
	if seen[res.VersionID] {
		e.t.Errorf("delete marker reused version ID %q", res.VersionID)
	}
	want := []vref{{VersionID: res.VersionID, DeleteMarker: true}}
	for i := len(vs) - 1; i >= 0; i-- {
		want = append(want, vref{VersionID: vs[i].VersionID})
	}
	e.wantHistory("b", "k", want...)

	// Version IDs are unique across keys too (they identify versions in
	// ListObjectVersions markers).
	other := e.put("b", "other", []byte("x"))
	if seen[other.VersionID] || other.VersionID == res.VersionID {
		e.t.Errorf("version ID %q of another key collides", other.VersionID)
	}
}

func testVersioningEnabledGetByVersion(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	vs := e.putVersions("b", "k", 4)
	for i, v := range vs {
		latest := i == len(vs)-1
		info := e.wantContent("b", "k", v.VersionID, versionData("k", i))
		if info.VersionID != v.VersionID || info.IsLatest != latest || info.DeleteMarker {
			e.t.Errorf("OpenObject(version #%d) VersionID/IsLatest/DeleteMarker = %q/%v/%v, want %q/%v/false",
				i, info.VersionID, info.IsLatest, info.DeleteMarker, v.VersionID, latest)
		}
		head := e.info("b", "k", v.VersionID)
		want := v
		want.IsLatest = latest
		e.wantInfo(fmt.Sprintf("GetObjectInfo(version #%d)", i), want, head)
	}
	e.wantContent("b", "k", "", versionData("k", 3))
	if got := e.info("b", "k", ""); got.VersionID != vs[3].VersionID {
		e.t.Errorf("GetObjectInfo(latest).VersionID = %q, want %q", got.VersionID, vs[3].VersionID)
	}
	// WalkObjects shows only the latest version.
	objs := e.walkObjects("b", "", "")
	if len(objs) != 1 || objs[0].VersionID != vs[3].VersionID || !objs[0].IsLatest {
		e.t.Errorf("WalkObjects = %v, want only the latest version %q", refsOf(objs), vs[3].VersionID)
	}
}

func testVersioningEnabledDeleteMarker(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	vs := e.putVersions("b", "k", 2)
	start := time.Now()
	res := e.del("b", "k", "")
	if !res.DeleteMarker {
		e.t.Fatalf("DeleteObject in Enabled bucket = %+v, want DeleteMarker=true", res)
	}
	e.checkVersionID("delete marker", res.VersionID)

	// Latest is the delete marker: nil error, DeleteMarker=true.
	info, err := e.b.GetObjectInfo(e.ctx, "b", "k", "")
	if err != nil {
		e.t.Fatalf("GetObjectInfo(latest = delete marker): %v, want nil error", err)
	}
	if !info.DeleteMarker || info.VersionID != res.VersionID || !info.IsLatest || info.Key != "k" || info.Bucket != "b" {
		e.t.Errorf("GetObjectInfo(latest) = %+v, want delete marker %q, IsLatest", info, res.VersionID)
	}
	if info.Size != 0 {
		e.t.Errorf("delete marker Size = %d, want 0", info.Size)
	}
	e.checkTime("delete marker LastModified", info.LastModified, start)

	info, r, err := e.b.OpenObject(e.ctx, "b", "k", "")
	if err != nil {
		e.t.Fatalf("OpenObject(latest = delete marker): %v, want nil error", err)
	}
	if r != nil {
		_ = r.Close()
		e.t.Error("OpenObject(latest = delete marker) returned a non-nil reader")
	}
	if !info.DeleteMarker || info.VersionID != res.VersionID {
		e.t.Errorf("OpenObject(latest) = %+v, want delete marker %q", info, res.VersionID)
	}

	// The marker addressed by its version ID.
	info, err = e.b.GetObjectInfo(e.ctx, "b", "k", res.VersionID)
	if err != nil || !info.DeleteMarker || !info.IsLatest {
		e.t.Errorf("GetObjectInfo(marker version) = %+v, %v; want delete marker, IsLatest", info, err)
	}
	info, r, err = e.b.OpenObject(e.ctx, "b", "k", res.VersionID)
	if err != nil || r != nil || !info.DeleteMarker {
		if r != nil {
			_ = r.Close()
		}
		e.t.Errorf("OpenObject(marker version) = %+v, reader non-nil=%v, %v; want delete marker with nil reader", info, r != nil, err)
	}

	// Previous versions are still readable and noncurrent.
	for i, v := range vs {
		got := e.wantContent("b", "k", v.VersionID, versionData("k", i))
		if got.IsLatest {
			e.t.Errorf("version #%d IsLatest = true behind a delete marker", i)
		}
	}
	if keys := e.walkKeys("b", "", ""); len(keys) != 0 {
		e.t.Errorf("WalkObjects lists %q, want nothing (latest is a delete marker)", keys)
	}
	e.wantHistory("b", "k",
		vref{VersionID: res.VersionID, DeleteMarker: true},
		vref{VersionID: vs[1].VersionID},
		vref{VersionID: vs[0].VersionID})
}

func testVersioningRemoveMarkerRestores(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	vs := e.putVersions("b", "k", 2)
	marker := e.del("b", "k", "")
	res := e.del("b", "k", marker.VersionID)
	if res.VersionID != marker.VersionID || !res.DeleteMarker {
		e.t.Errorf("DeleteObject(marker version) = %+v, want {VersionID: %q, DeleteMarker: true}", res, marker.VersionID)
	}
	info := e.wantContent("b", "k", "", versionData("k", 1))
	if info.VersionID != vs[1].VersionID || !info.IsLatest {
		e.t.Errorf("after removing the delete marker latest = %q (IsLatest %v), want %q", info.VersionID, info.IsLatest, vs[1].VersionID)
	}
	if keys := e.walkKeys("b", "", ""); len(keys) != 1 || keys[0] != "k" {
		e.t.Errorf("WalkObjects = %q, want [k]", keys)
	}
	e.wantHistory("b", "k", vref{VersionID: vs[1].VersionID}, vref{VersionID: vs[0].VersionID})
	_, err := e.b.GetObjectInfo(e.ctx, "b", "k", marker.VersionID)
	e.wantErr("GetObjectInfo(removed marker)", err, storage.ErrVersionNotFound)
}

func testVersioningDeleteNonLatest(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	vs := e.putVersions("b", "k", 3)
	res := e.del("b", "k", vs[1].VersionID)
	if res.VersionID != vs[1].VersionID || res.DeleteMarker {
		e.t.Errorf("DeleteObject(version) = %+v, want {VersionID: %q, DeleteMarker: false}", res, vs[1].VersionID)
	}
	info := e.wantContent("b", "k", "", versionData("k", 2))
	if info.VersionID != vs[2].VersionID {
		e.t.Errorf("latest changed to %q after deleting a noncurrent version, want %q", info.VersionID, vs[2].VersionID)
	}
	e.wantHistory("b", "k", vref{VersionID: vs[2].VersionID}, vref{VersionID: vs[0].VersionID})
	_, err := e.b.GetObjectInfo(e.ctx, "b", "k", vs[1].VersionID)
	e.wantErr("GetObjectInfo(deleted version)", err, storage.ErrVersionNotFound)
	_, r, err := e.b.OpenObject(e.ctx, "b", "k", vs[1].VersionID)
	e.wantErr("OpenObject(deleted version)", err, storage.ErrVersionNotFound)
	if r != nil {
		_ = r.Close()
		e.t.Error("OpenObject(deleted version) returned a reader")
	}
	_, err = e.b.DeleteObject(e.ctx, "b", "k", vs[1].VersionID)
	e.wantErr("DeleteObject(deleted version) again", err, storage.ErrVersionNotFound)
	e.wantContent("b", "k", vs[0].VersionID, versionData("k", 0))
}

func testVersioningDeleteLatest(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	vs := e.putVersions("b", "k", 3)
	e.del("b", "k", vs[2].VersionID)
	info := e.wantContent("b", "k", "", versionData("k", 1))
	if info.VersionID != vs[1].VersionID || !info.IsLatest {
		e.t.Errorf("after deleting the latest version: latest = %q (IsLatest %v), want %q", info.VersionID, info.IsLatest, vs[1].VersionID)
	}
	if got := e.info("b", "k", vs[1].VersionID); !got.IsLatest {
		e.t.Error("GetObjectInfo(new latest by version ID).IsLatest = false")
	}
	e.wantHistory("b", "k", vref{VersionID: vs[1].VersionID}, vref{VersionID: vs[0].VersionID})
}

func testVersioningDeleteAll(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	vs := e.putVersions("b", "k", 2)
	m := e.del("b", "k", "")
	e.put("b", "neighbour", []byte("n"))
	for _, id := range []string{vs[0].VersionID, m.VersionID, vs[1].VersionID} {
		e.del("b", "k", id)
	}
	_, err := e.b.GetObjectInfo(e.ctx, "b", "k", "")
	e.wantErr("GetObjectInfo after deleting all versions", err, storage.ErrObjectNotFound)
	if got := refsOf(e.walkVersions("b", "", "", "")); len(got) != 1 || got[0].Key != "neighbour" {
		e.t.Errorf("WalkVersions after deleting all versions of k = %v, want only neighbour", got)
	}
}

func testVersioningDeleteMissingKey(e *env) {
	// S3 creates a delete marker even when the key does not exist.
	e.mkVersioned("b", storage.VersioningEnabled)
	res := e.del("b", "ghost", "")
	if !res.DeleteMarker {
		e.t.Fatalf("DeleteObject(missing key) in Enabled bucket = %+v, want a delete marker", res)
	}
	e.checkVersionID("delete marker", res.VersionID)
	info, err := e.b.GetObjectInfo(e.ctx, "b", "ghost", "")
	if err != nil || !info.DeleteMarker || info.VersionID != res.VersionID {
		e.t.Errorf("GetObjectInfo(ghost) = %+v, %v; want the delete marker %q with nil error", info, err, res.VersionID)
	}
	e.wantHistory("b", "ghost", vref{VersionID: res.VersionID, DeleteMarker: true})
}

func testVersioningStackedMarkers(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	v := e.put("b", "k", []byte("v"))
	m1 := e.del("b", "k", "")
	m2 := e.del("b", "k", "")
	if m1.VersionID == m2.VersionID {
		e.t.Fatalf("two delete markers share version ID %q", m1.VersionID)
	}
	e.wantHistory("b", "k",
		vref{VersionID: m2.VersionID, DeleteMarker: true},
		vref{VersionID: m1.VersionID, DeleteMarker: true},
		vref{VersionID: v.VersionID})
	// Removing the newest marker exposes the older marker, not the object.
	e.del("b", "k", m2.VersionID)
	if info := e.info("b", "k", ""); !info.DeleteMarker || info.VersionID != m1.VersionID {
		e.t.Errorf("latest after removing the newest marker = %+v, want delete marker %q", info, m1.VersionID)
	}
	if keys := e.walkKeys("b", "", ""); len(keys) != 0 {
		e.t.Errorf("WalkObjects = %q, want nothing", keys)
	}
}

func testVersioningUnversionedToEnabled(e *env) {
	e.mkBucket("b")
	e.put("b", "k", []byte("null data"))
	e.setVersioning("b", storage.VersioningEnabled)
	v := e.put("b", "k", []byte("versioned"))
	e.checkVersionID("put after enabling versioning", v.VersionID)
	e.wantHistory("b", "k", vref{VersionID: v.VersionID}, vref{VersionID: storage.NullVersionID})
	info := e.wantContent("b", "k", storage.NullVersionID, []byte("null data"))
	if info.IsLatest || info.VersionID != storage.NullVersionID {
		e.t.Errorf("null version = %q IsLatest %v, want null, noncurrent", info.VersionID, info.IsLatest)
	}
	e.wantContent("b", "k", "", []byte("versioned"))
}

func testVersioningSuspended(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	v1 := e.put("b", "k", []byte("v1"))
	e.setVersioning("b", storage.VersioningSuspended)

	n1 := e.put("b", "k", []byte("null-1"))
	if n1.VersionID != storage.NullVersionID {
		e.t.Fatalf("put in Suspended bucket VersionID = %q, want null", n1.VersionID)
	}
	e.wantHistory("b", "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: v1.VersionID})

	// A second put replaces the null version.
	e.put("b", "k", []byte("null-2"))
	e.wantHistory("b", "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: v1.VersionID})
	e.wantContent("b", "k", storage.NullVersionID, []byte("null-2"))

	// Delete adds a null delete marker replacing the null version.
	res := e.del("b", "k", "")
	if res.VersionID != storage.NullVersionID || !res.DeleteMarker {
		e.t.Errorf("DeleteObject in Suspended bucket = %+v, want {VersionID: null, DeleteMarker: true}", res)
	}
	e.wantHistory("b", "k", vref{VersionID: storage.NullVersionID, DeleteMarker: true}, vref{VersionID: v1.VersionID})
	info, err := e.b.GetObjectInfo(e.ctx, "b", "k", "")
	if err != nil || !info.DeleteMarker || info.VersionID != storage.NullVersionID {
		e.t.Errorf("GetObjectInfo(latest) = %+v, %v; want null delete marker", info, err)
	}
	info, err = e.b.GetObjectInfo(e.ctx, "b", "k", storage.NullVersionID)
	if err != nil || !info.DeleteMarker {
		e.t.Errorf("GetObjectInfo(null) = %+v, %v; want null delete marker", info, err)
	}
	e.wantContent("b", "k", v1.VersionID, []byte("v1"))

	// A put replaces the null delete marker.
	e.put("b", "k", []byte("null-3"))
	e.wantHistory("b", "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: v1.VersionID})
	e.wantContent("b", "k", "", []byte("null-3"))

	// Re-enabling keeps the null version as a noncurrent version.
	e.setVersioning("b", storage.VersioningEnabled)
	v2 := e.put("b", "k", []byte("v2"))
	e.checkVersionID("put after re-enabling", v2.VersionID)
	e.wantHistory("b", "k", vref{VersionID: v2.VersionID}, vref{VersionID: storage.NullVersionID}, vref{VersionID: v1.VersionID})
	e.wantContent("b", "k", storage.NullVersionID, []byte("null-3"))

	// Deleting the null version by ID keeps the others.
	e.del("b", "k", storage.NullVersionID)
	e.wantHistory("b", "k", vref{VersionID: v2.VersionID}, vref{VersionID: v1.VersionID})
}

// testVersioningSuspendedReplacesNoncurrentNull: in a Suspended bucket a
// new null version replaces the existing null version even when that one
// is not the latest (S3 semantics).
func testVersioningSuspendedReplacesNoncurrentNull(e *env) {
	e.mkBucket("b")
	e.put("b", "k", []byte("null-A")) // unversioned null version
	e.setVersioning("b", storage.VersioningEnabled)
	v1 := e.put("b", "k", []byte("v1"))
	e.wantHistory("b", "k", vref{VersionID: v1.VersionID}, vref{VersionID: storage.NullVersionID})
	e.setVersioning("b", storage.VersioningSuspended)
	e.put("b", "k", []byte("null-B"))
	e.wantHistory("b", "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: v1.VersionID})
	e.wantContent("b", "k", storage.NullVersionID, []byte("null-B"))
	e.wantContent("b", "k", v1.VersionID, []byte("v1"))

	// Same for a null delete marker replacing a noncurrent null version.
	e.setVersioning("b", storage.VersioningEnabled)
	v2 := e.put("b", "k", []byte("v2"))
	e.setVersioning("b", storage.VersioningSuspended)
	e.del("b", "k", "")
	e.wantHistory("b", "k",
		vref{VersionID: storage.NullVersionID, DeleteMarker: true},
		vref{VersionID: v2.VersionID},
		vref{VersionID: v1.VersionID})
}

func testVersioningSuspendedDeleteKeeps(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	v1 := e.put("b", "k", []byte("v1"))
	v2 := e.put("b", "k", []byte("v2"))
	e.setVersioning("b", storage.VersioningSuspended)
	res := e.del("b", "k", "")
	if res.VersionID != storage.NullVersionID || !res.DeleteMarker {
		e.t.Errorf("DeleteObject = %+v, want null delete marker", res)
	}
	e.wantHistory("b", "k",
		vref{VersionID: storage.NullVersionID, DeleteMarker: true},
		vref{VersionID: v2.VersionID},
		vref{VersionID: v1.VersionID})
	// A second delete replaces the null marker rather than stacking.
	e.del("b", "k", "")
	e.wantHistory("b", "k",
		vref{VersionID: storage.NullVersionID, DeleteMarker: true},
		vref{VersionID: v2.VersionID},
		vref{VersionID: v1.VersionID})
	// Removing the null marker restores v2.
	e.del("b", "k", storage.NullVersionID)
	if info := e.info("b", "k", ""); info.VersionID != v2.VersionID || info.DeleteMarker {
		e.t.Errorf("latest after removing null marker = %+v, want %q", info, v2.VersionID)
	}
}

func testVersioningNotFoundErrors(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	v := e.put("b", "k", []byte("x"))

	_, err := e.b.GetObjectInfo(e.ctx, "b", "missing", "")
	e.wantErr("GetObjectInfo(missing key, latest)", err, storage.ErrObjectNotFound)
	_, err = e.b.GetObjectInfo(e.ctx, "b", "k", "no-such-version")
	e.wantErr("GetObjectInfo(existing key, unknown version)", err, storage.ErrVersionNotFound)
	_, err = e.b.GetObjectInfo(e.ctx, "b", "k", storage.NullVersionID)
	e.wantErr("GetObjectInfo(existing key, null version absent)", err, storage.ErrVersionNotFound)
	_, r, err := e.b.OpenObject(e.ctx, "b", "k", "no-such-version")
	e.wantErr("OpenObject(existing key, unknown version)", err, storage.ErrVersionNotFound)
	if r != nil {
		_ = r.Close()
	}
	// The contract is ambiguous for an unknown version of a missing key
	// ("ErrObjectNotFound if the key has no versions at all; ErrVersionNotFound
	// if versionID is given but unknown"), so either is accepted.
	_, err = e.b.GetObjectInfo(e.ctx, "b", "missing", v.VersionID)
	e.wantErr("GetObjectInfo(missing key, version of another key)", err, storage.ErrVersionNotFound, storage.ErrObjectNotFound)
	// A version ID is scoped to its key.
	e.put("b", "k2", []byte("y"))
	_, err = e.b.GetObjectInfo(e.ctx, "b", "k2", v.VersionID)
	e.wantErr("GetObjectInfo(k2, version of k)", err, storage.ErrVersionNotFound)

	_, err = e.b.DeleteObject(e.ctx, "b", "k", "no-such-version")
	e.wantErr("DeleteObject(existing key, unknown version)", err, storage.ErrVersionNotFound)
	_, err = e.b.DeleteObject(e.ctx, "b", "k2", v.VersionID)
	e.wantErr("DeleteObject(k2, version of k)", err, storage.ErrVersionNotFound)
	_, err = e.b.DeleteObject(e.ctx, "b", "missing", "no-such-version")
	e.wantErr("DeleteObject(missing key, version)", err, storage.ErrVersionNotFound)
	if v := e.history("b", "missing"); len(v) != 0 {
		e.t.Errorf("failed DeleteObject(version) created %v", v)
	}
	e.wantContent("b", "k", v.VersionID, []byte("x"))
}
