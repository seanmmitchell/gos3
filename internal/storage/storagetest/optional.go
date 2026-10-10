package storagetest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Tests of the optional Backend extensions (internal/storage/optional.go).
// Each test is skipped when the backend does not implement the extension.

func optionalTests() []testCase {
	return []testCase{
		{"DeleteObjectIf", testDeleteObjectIf},
		{"DeleteObjectIfRace", testDeleteObjectIfRace},
		{"WalkMultipartUploads", testWalkUploads},
	}
}

func (e *env) conditionalDeleter() storage.ConditionalDeleter {
	e.t.Helper()
	cd, ok := e.b.(storage.ConditionalDeleter)
	if !ok {
		e.t.Skip("backend does not implement storage.ConditionalDeleter")
	}
	return cd
}

var errCheckRefused = errors.New("storagetest: check refused")

func testDeleteObjectIf(e *env) {
	cd := e.conditionalDeleter()
	e.run("MissingKeyOrVersion", func(e *env) {
		e.mkBucket("m1")
		e.mkVersioned("m2", storage.VersioningEnabled)
		called := false
		check := func(storage.ObjectInfo) error { called = true; return nil }
		for _, b := range []string{"m1", "m2"} {
			_, err := cd.DeleteObjectIf(e.ctx, b, "nokey", "", check)
			e.wantErr("DeleteObjectIf of a missing key in "+b, err, storage.ErrObjectNotFound)
			_, err = cd.DeleteObjectIf(e.ctx, b, "nokey", storage.NullVersionID, check)
			e.wantErr("DeleteObjectIf of a missing version in "+b, err, storage.ErrVersionNotFound)
		}
		e.put("m2", "k", []byte("x"))
		_, err := cd.DeleteObjectIf(e.ctx, "m2", "k", storage.NullVersionID, check)
		e.wantErr("DeleteObjectIf of a missing null version", err, storage.ErrVersionNotFound)
		_, err = cd.DeleteObjectIf(e.ctx, "nobucket", "k", "", check)
		e.wantErr("DeleteObjectIf in a missing bucket", err, storage.ErrBucketNotFound)
		if called {
			e.t.Error("check was called for a missing key, version or bucket")
		}
	})
	e.run("RefusedUnversioned", func(e *env) {
		e.mkBucket("u")
		v := e.put("u", "k", []byte("data"))
		var got storage.ObjectInfo
		_, err := cd.DeleteObjectIf(e.ctx, "u", "k", "", func(o storage.ObjectInfo) error { got = o; return errCheckRefused })
		if !errors.Is(err, errCheckRefused) {
			e.t.Errorf("refused DeleteObjectIf: err = %v, want the check's error", err)
		}
		if got.VersionID != v.VersionID || !got.IsLatest || got.DeleteMarker || got.ETag != v.ETag || got.Size != v.Size {
			e.t.Errorf("check got %+v, want the current version %+v", got, v)
		}
		e.wantContent("u", "k", "", []byte("data"))
		res, err := cd.DeleteObjectIf(e.ctx, "u", "k", "", func(storage.ObjectInfo) error { return nil })
		e.noErr("accepted DeleteObjectIf", err)
		if res.DeleteMarker {
			e.t.Errorf("unversioned DeleteObjectIf result %+v", res)
		}
		_, err = e.b.GetObjectInfo(e.ctx, "u", "k", "")
		e.wantErr("object after accepted DeleteObjectIf", err, storage.ErrObjectNotFound)
	})
	e.run("Versioned", func(e *env) {
		e.mkVersioned("v", storage.VersioningEnabled)
		old := e.put("v", "k", []byte("old"))
		cur := e.put("v", "k", []byte("cur"))
		var got storage.ObjectInfo
		_, err := cd.DeleteObjectIf(e.ctx, "v", "k", old.VersionID, func(o storage.ObjectInfo) error { got = o; return errCheckRefused })
		if !errors.Is(err, errCheckRefused) {
			e.t.Errorf("refused DeleteObjectIf: err = %v", err)
		}
		if got.VersionID != old.VersionID || got.IsLatest {
			e.t.Errorf("check got version %q latest=%v, want noncurrent %q", got.VersionID, got.IsLatest, old.VersionID)
		}
		e.wantHistory("v", "k", vref{VersionID: cur.VersionID}, vref{VersionID: old.VersionID})

		// "" adds a delete marker after checking the latest version.
		res, err := cd.DeleteObjectIf(e.ctx, "v", "k", "", func(o storage.ObjectInfo) error {
			got = o
			return nil
		})
		e.noErr("DeleteObjectIf creating a delete marker", err)
		if !res.DeleteMarker || res.VersionID == "" {
			e.t.Errorf("DeleteObjectIf result %+v, want a new delete marker", res)
		}
		if got.VersionID != cur.VersionID || !got.IsLatest {
			e.t.Errorf("check got %q latest=%v, want the latest %q", got.VersionID, got.IsLatest, cur.VersionID)
		}
		// The check sees the delete marker as the latest version.
		_, err = cd.DeleteObjectIf(e.ctx, "v", "k", "", func(o storage.ObjectInfo) error {
			got = o
			return errCheckRefused
		})
		if !errors.Is(err, errCheckRefused) || !got.DeleteMarker || got.VersionID != res.VersionID {
			e.t.Errorf("check on a delete marker: err %v, got %+v", err, got)
		}
		// A specific version is removed permanently.
		r2, err := cd.DeleteObjectIf(e.ctx, "v", "k", old.VersionID, func(storage.ObjectInfo) error { return nil })
		e.noErr("DeleteObjectIf of a version", err)
		if r2.VersionID != old.VersionID || r2.DeleteMarker {
			e.t.Errorf("DeleteObjectIf of a version: result %+v", r2)
		}
		e.wantHistory("v", "k", vref{VersionID: res.VersionID, DeleteMarker: true}, vref{VersionID: cur.VersionID})
	})
	e.run("Suspended", func(e *env) {
		e.mkVersioned("s", storage.VersioningSuspended)
		e.put("s", "k", []byte("null"))
		res, err := cd.DeleteObjectIf(e.ctx, "s", "k", "", func(o storage.ObjectInfo) error {
			if o.VersionID != storage.NullVersionID {
				return errCheckRefused
			}
			return nil
		})
		e.noErr("suspended DeleteObjectIf", err)
		if !res.DeleteMarker || res.VersionID != storage.NullVersionID {
			e.t.Errorf("suspended DeleteObjectIf result %+v, want a null delete marker", res)
		}
		e.wantHistory("s", "k", vref{VersionID: storage.NullVersionID, DeleteMarker: true})
	})
}

// testDeleteObjectIfRace checks atomicity: a delete conditional on the
// version it last saw never removes a version committed concurrently.
func testDeleteObjectIfRace(e *env) {
	cd := e.conditionalDeleter()
	for _, status := range []storage.VersioningStatus{storage.VersioningUnversioned, storage.VersioningSuspended} {
		name := "race-unversioned"
		if status == storage.VersioningSuspended {
			name = "race-suspended"
		}
		e.run(name, func(e *env) {
			if status == storage.VersioningUnversioned {
				e.mkBucket(name)
			} else {
				e.mkVersioned(name, status)
			}
			for i := range 200 {
				old := e.put(name, "k", []byte("old"))
				newData := []byte{byte(i), 'n', 'e', 'w'}
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					if _, err := putObject(e.ctx, e.b, name, "k", newData, simpleMeta(newData), storage.CommitOptions{}); err != nil {
						e.t.Error(err)
					}
				}()
				go func() {
					defer wg.Done()
					_, err := cd.DeleteObjectIf(e.ctx, name, "k", "", func(o storage.ObjectInfo) error {
						if o.DeleteMarker || o.ETag != old.ETag {
							return errCheckRefused
						}
						return nil
					})
					if err != nil && !errors.Is(err, errCheckRefused) {
						e.t.Error(err)
					}
				}()
				wg.Wait()
				// Whatever the interleaving, the concurrent write survives:
				// either the delete ran first or its check refused.
				if o, data := e.read(name, "k", ""); o.DeleteMarker || string(data) != string(newData) {
					e.t.Fatalf("iteration %d: concurrently written object was deleted (delete marker %v, data %q)", i, o.DeleteMarker, data)
				}
			}
		})
	}
}

func (e *env) uploadWalker() storage.UploadWalker {
	e.t.Helper()
	w, ok := e.b.(storage.UploadWalker)
	if !ok {
		e.t.Skip("backend does not implement storage.UploadWalker")
	}
	return w
}

// walkUploadIDs returns the IDs of the uploads WalkMultipartUploads
// delivers, stopping after limit (<= 0: all).
func (e *env) walkUploadIDs(w storage.UploadWalker, bucket, prefix string, after storage.UploadPosition, limit int) []string {
	e.t.Helper()
	ids := []string{}
	err := w.WalkMultipartUploads(e.ctx, bucket, prefix, after, func(u storage.UploadInfo) error {
		if u.Bucket != bucket || !strings.HasPrefix(u.Key, prefix) || u.UploadID == "" {
			e.t.Errorf("walk delivered %+v", u)
		}
		ids = append(ids, u.UploadID)
		if limit > 0 && len(ids) == limit {
			return storage.ErrStopWalk
		}
		return nil
	})
	e.noErr(fmt.Sprintf("WalkMultipartUploads(%q, %+v)", prefix, after), err)
	return ids
}

func testWalkUploads(e *env) {
	w := e.uploadWalker()
	e.mkBucket("w")
	e.mkBucket("other")
	keys := []string{"b", "a/1", "a/2", "a", "c/x", "a/1", "b", "b", "日本", "a/1"}
	for i, k := range keys {
		e.createUpload("w", k, storage.ObjectMeta{UserMeta: map[string]string{"i": fmt.Sprint(i)}})
	}
	e.createUpload("other", "a", storage.ObjectMeta{})
	all, err := e.b.ListMultipartUploads(e.ctx, "w")
	e.noErr("ListMultipartUploads", err)
	var allIDs []string
	for _, u := range all {
		allIDs = append(allIDs, u.UploadID)
	}

	// The full walk is the ListMultipartUploads order, with the metadata.
	n := 0
	err = w.WalkMultipartUploads(e.ctx, "w", "", storage.UploadPosition{}, func(u storage.UploadInfo) error {
		if n < len(all) {
			e.wantInfo("walked upload", storage.ObjectInfo{Key: all[n].Key, ObjectMeta: all[n].Meta}, storage.ObjectInfo{Key: u.Key, ObjectMeta: u.Meta})
			if !u.Initiated.Equal(all[n].Initiated) {
				e.t.Errorf("walked upload %q initiated %v, want %v", u.UploadID, u.Initiated, all[n].Initiated)
			}
		}
		n++
		return nil
	})
	e.noErr("WalkMultipartUploads", err)
	if got := e.walkUploadIDs(w, "w", "", storage.UploadPosition{}, 0); !slices.Equal(got, allIDs) {
		e.t.Errorf("walk order %v, want %v", got, allIDs)
	}

	// Every prefix and every start position agrees with filtering the
	// full listing.
	var positions []storage.UploadPosition
	positions = append(positions, storage.UploadPosition{})
	for _, k := range []string{"", "a", "a/", "a/1", "a/3", "b", "c", "zz"} {
		positions = append(positions, storage.UploadPosition{Key: k, AfterKey: true}, storage.UploadPosition{Key: k})
	}
	for _, u := range all {
		positions = append(positions, storage.PositionOf(u))
	}
	for _, prefix := range []string{"", "a", "a/", "a/1", "b", "c/", "x", "日"} {
		for _, pos := range positions {
			var want []string
			for _, u := range all {
				if strings.HasPrefix(u.Key, prefix) && pos.Precedes(u) {
					want = append(want, u.UploadID)
				}
			}
			if want == nil {
				want = []string{}
			}
			if got := e.walkUploadIDs(w, "w", prefix, pos, 0); !slices.Equal(got, want) {
				e.t.Errorf("prefix %q after %+v: %v, want %v", prefix, pos, got, want)
			}
			if len(want) > 0 {
				if got := e.walkUploadIDs(w, "w", prefix, pos, 1); !slices.Equal(got, want[:1]) {
					e.t.Errorf("prefix %q after %+v, stopped after one: %v, want %v", prefix, pos, got, want[:1])
				}
			}
		}
	}

	// The position of a removed upload still works.
	gone := all[2]
	e.noErr("abort", e.b.AbortMultipartUpload(e.ctx, "w", gone.Key, gone.UploadID))
	if got := e.walkUploadIDs(w, "w", "", storage.PositionOf(gone), 0); !slices.Equal(got, allIDs[3:]) {
		e.t.Errorf("after removed upload: %v, want %v", got, allIDs[3:])
	}

	// fn may call into the backend (here: abort every upload walked).
	err = w.WalkMultipartUploads(e.ctx, "w", "", storage.UploadPosition{}, func(u storage.UploadInfo) error {
		return e.b.AbortMultipartUpload(e.ctx, "w", u.Key, u.UploadID)
	})
	e.noErr("WalkMultipartUploads aborting", err)
	if got := e.walkUploadIDs(w, "w", "", storage.UploadPosition{}, 0); len(got) != 0 {
		e.t.Errorf("uploads left after aborting all: %v", got)
	}

	// Errors.
	sentinel := errors.New("stop")
	e.createUpload("w", "k", storage.ObjectMeta{})
	err = w.WalkMultipartUploads(e.ctx, "w", "", storage.UploadPosition{}, func(storage.UploadInfo) error { return sentinel })
	if !errors.Is(err, sentinel) {
		e.t.Errorf("callback error: %v", err)
	}
	err = w.WalkMultipartUploads(e.ctx, "nobucket", "", storage.UploadPosition{}, func(storage.UploadInfo) error { return nil })
	e.wantErr("walk of a missing bucket", err, storage.ErrBucketNotFound)

	// Many uploads (several internal batches).
	e.run("Many", func(e *env) {
		e.mkBucket("many")
		var want []string
		for i := range 700 {
			want = append(want, e.createUpload("many", fmt.Sprintf("k%03d", i%350), storage.ObjectMeta{}).UploadID)
		}
		all, err := e.b.ListMultipartUploads(e.ctx, "many")
		e.noErr("ListMultipartUploads", err)
		var ids []string
		for _, u := range all {
			ids = append(ids, u.UploadID)
		}
		if got := e.walkUploadIDs(w, "many", "", storage.UploadPosition{}, 0); !slices.Equal(got, ids) {
			e.t.Errorf("walk of 700 uploads differs from the listing (%d vs %d)", len(got), len(ids))
		}
		slices.Sort(want)
		slices.Sort(ids)
		if !slices.Equal(want, ids) {
			e.t.Errorf("listing does not hold the created uploads")
		}
		// Resuming from each delivered upload's position covers the rest.
		got := e.walkUploadIDs(w, "many", "k1", storage.UploadPosition{}, 0)
		var resumed []string
		pos := storage.UploadPosition{}
		for {
			var page []storage.UploadInfo
			err := w.WalkMultipartUploads(e.ctx, "many", "k1", pos, func(u storage.UploadInfo) error {
				page = append(page, u)
				if len(page) == 7 {
					return storage.ErrStopWalk
				}
				return nil
			})
			e.noErr("paged walk", err)
			for _, u := range page {
				resumed = append(resumed, u.UploadID)
			}
			if len(page) < 7 {
				break
			}
			pos = storage.PositionOf(page[len(page)-1])
		}
		if !slices.Equal(got, resumed) || len(got) != 200 {
			e.t.Errorf("paged walk: %d uploads, unpaged %d (want 200)", len(resumed), len(got))
		}
	})
}
