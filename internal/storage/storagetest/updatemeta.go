package storagetest

import (
	"errors"
	"maps"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func updateMetaTests() []testCase {
	return []testCase{
		{"Basic", testUpdateMetaBasic},
		{"SpecificVersion", testUpdateMetaSpecificVersion},
		{"CallbackError", testUpdateMetaCallbackError},
		{"Errors", testUpdateMetaErrors},
		{"Isolation", testUpdateMetaIsolation},
	}
}

func testUpdateMetaBasic(e *env) {
	e.mkBucket("b")
	data := []byte("immutable data")
	orig := fullMeta()
	orig.Tags = map[string]string{"a": "1"}
	orig.LegalHold = false
	orig.RetentionMode, orig.RetainUntil = "", time.Time{}
	committed := e.putMeta("b", "k", data, orig)

	time.Sleep(5 * time.Millisecond) // a backend wrongly touching LastModified would be visible
	retain := time.Date(2035, 6, 7, 8, 9, 10, 500_000_000, time.UTC)
	var seen storage.ObjectMeta
	updated, err := e.b.UpdateObjectMeta(e.ctx, "b", "k", "", func(m *storage.ObjectMeta) error {
		seen = *m
		m.Tags = map[string]string{"b": "2", "c": "3"}
		m.LegalHold = true
		m.RetentionMode = "GOVERNANCE"
		m.RetainUntil = retain
		m.ACL = "private"
		return nil
	})
	e.noErr("UpdateObjectMeta", err)
	e.wantMeta("metadata passed to the update callback", orig, seen)

	want := orig
	want.Tags = map[string]string{"b": "2", "c": "3"}
	want.LegalHold = true
	want.RetentionMode = "GOVERNANCE"
	want.RetainUntil = retain
	want.ACL = "private"
	e.wantMeta("UpdateObjectMeta result", want, updated.ObjectMeta)

	// Data, version, size and LastModified are unchanged.
	wantInfo := committed
	wantInfo.ObjectMeta = want
	e.wantInfo("UpdateObjectMeta result", wantInfo, updated)
	e.wantInfo("GetObjectInfo after update", wantInfo, e.info("b", "k", ""))
	got := e.wantContent("b", "k", "", data)
	e.wantInfo("OpenObject after update", wantInfo, got)
	e.wantHistory("b", "k", vref{VersionID: storage.NullVersionID})
}

func testUpdateMetaSpecificVersion(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	v1 := e.put("b", "k", []byte("one"))
	v2 := e.put("b", "k", []byte("two"))
	updated, err := e.b.UpdateObjectMeta(e.ctx, "b", "k", v1.VersionID, func(m *storage.ObjectMeta) error {
		m.Tags = map[string]string{"v": "1"}
		return nil
	})
	e.noErr("UpdateObjectMeta(noncurrent version)", err)
	if updated.VersionID != v1.VersionID || updated.IsLatest {
		e.t.Errorf("UpdateObjectMeta result VersionID/IsLatest = %q/%v, want %q/false", updated.VersionID, updated.IsLatest, v1.VersionID)
	}
	if got := e.info("b", "k", v1.VersionID); !maps.Equal(got.Tags, map[string]string{"v": "1"}) {
		e.t.Errorf("tags of updated version = %v, want map[v:1]", got.Tags)
	}
	if got := e.info("b", "k", ""); len(got.Tags) != 0 || got.VersionID != v2.VersionID {
		e.t.Errorf("latest after updating a noncurrent version: VersionID %q tags %v, want %q without tags", got.VersionID, got.Tags, v2.VersionID)
	}
	e.wantContent("b", "k", v1.VersionID, []byte("one"))
	e.wantHistory("b", "k", vref{VersionID: v2.VersionID}, vref{VersionID: v1.VersionID})
	// The null version can be addressed explicitly.
	e.setVersioning("b", storage.VersioningSuspended)
	e.put("b", "k", []byte("null"))
	_, err = e.b.UpdateObjectMeta(e.ctx, "b", "k", storage.NullVersionID, func(m *storage.ObjectMeta) error {
		m.LegalHold = true
		return nil
	})
	e.noErr("UpdateObjectMeta(null)", err)
	if !e.info("b", "k", storage.NullVersionID).LegalHold {
		e.t.Error("LegalHold not persisted on the null version")
	}
}

func testUpdateMetaCallbackError(e *env) {
	e.mkBucket("b")
	orig := fullMeta()
	e.putMeta("b", "k", []byte("x"), orig)
	sentinel := errors.New("callback refused")
	_, err := e.b.UpdateObjectMeta(e.ctx, "b", "k", "", func(m *storage.ObjectMeta) error {
		m.Tags["project"] = "changed"
		m.UserMeta = nil
		m.LegalHold = false
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		e.t.Errorf("UpdateObjectMeta with a failing callback: err = %v, want the callback's error", err)
	}
	e.wantMeta("metadata after a failed update", orig, e.info("b", "k", "").ObjectMeta)
}

func testUpdateMetaErrors(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	called := false
	fn := func(*storage.ObjectMeta) error { called = true; return nil }

	_, err := e.b.UpdateObjectMeta(e.ctx, "b", "missing", "", fn)
	e.wantErr("UpdateObjectMeta(missing key)", err, storage.ErrObjectNotFound)
	v := e.put("b", "k", []byte("x"))
	_, err = e.b.UpdateObjectMeta(e.ctx, "b", "k", "no-such-version", fn)
	e.wantErr("UpdateObjectMeta(unknown version)", err, storage.ErrVersionNotFound)

	// Delete markers have no metadata to update. The contract does not name
	// the error; S3 answers NoSuchKey/MethodNotAllowed, so ErrObjectNotFound
	// or ErrVersionNotFound are accepted.
	m := e.del("b", "k", "")
	_, err = e.b.UpdateObjectMeta(e.ctx, "b", "k", "", fn)
	e.wantErr("UpdateObjectMeta(latest is a delete marker)", err, storage.ErrObjectNotFound, storage.ErrVersionNotFound)
	_, err = e.b.UpdateObjectMeta(e.ctx, "b", "k", m.VersionID, fn)
	e.wantErr("UpdateObjectMeta(delete marker version)", err, storage.ErrObjectNotFound, storage.ErrVersionNotFound)
	if called {
		e.t.Error("UpdateObjectMeta invoked the callback for a missing version or delete marker")
	}
	if info := e.info("b", "k", m.VersionID); !info.DeleteMarker {
		e.t.Errorf("delete marker changed by a failed update: %+v", info)
	}
	// The noncurrent version behind the marker is still updatable.
	_, err = e.b.UpdateObjectMeta(e.ctx, "b", "k", v.VersionID, func(m *storage.ObjectMeta) error {
		m.LegalHold = true
		return nil
	})
	e.noErr("UpdateObjectMeta(version behind a delete marker)", err)
}

func testUpdateMetaIsolation(e *env) {
	e.mkBucket("b")
	e.putMeta("b", "k", []byte("x"), storage.ObjectMeta{ETag: "e", Tags: map[string]string{"a": "1"}})
	var kept map[string]string
	_, err := e.b.UpdateObjectMeta(e.ctx, "b", "k", "", func(m *storage.ObjectMeta) error {
		m.Tags["b"] = "2" // in-place mutation of the provided map
		kept = m.Tags
		return nil
	})
	e.noErr("UpdateObjectMeta", err)
	want := map[string]string{"a": "1", "b": "2"}
	if got := e.info("b", "k", "").Tags; !maps.Equal(got, want) {
		e.t.Fatalf("tags after in-place update = %v, want %v", got, want)
	}
	kept["evil"] = "mutation after the callback returned"
	if got := e.info("b", "k", "").Tags; !maps.Equal(got, want) {
		e.t.Errorf("mutating a map retained from the callback changed stored tags: %v", got)
	}
}
