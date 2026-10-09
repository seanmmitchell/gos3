package storagetest

import (
	"fmt"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func conditionalTests() []testCase {
	return []testCase{
		{"IfNoneMatch", testCondIfNoneMatch},
		{"IfMatch", testCondIfMatch},
		{"FailedCommitIsInvisible", testCondFailedInvisible},
		{"IfNoneMatchRace", testCondIfNoneMatchRace},
		{"IfMatchRace", testCondIfMatchRace},
		{"MultipartIfNoneMatch", testCondMultipartIfNoneMatch},
		{"MultipartIfMatch", testCondMultipartIfMatch},
		{"MultipartIfNoneMatchRace", testCondMultipartRace},
	}
}

var (
	ifNoneMatch = storage.CommitOptions{IfNoneMatch: true}
	pf          = storage.ErrPreconditionFailed
)

func (e *env) tryPut(bucket, key string, data []byte, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	e.t.Helper()
	return putObject(e.ctx, e.b, bucket, key, data, simpleMeta(data), opts)
}

func testCondIfNoneMatch(e *env) {
	e.run("NoObject", func(e *env) {
		e.mkBucket("b1")
		_, err := e.tryPut("b1", "k", []byte("first"), ifNoneMatch)
		e.noErr("IfNoneMatch on a new key", err)
		e.wantContent("b1", "k", "", []byte("first"))
	})
	e.run("ExistingObject", func(e *env) {
		e.mkBucket("b2")
		e.put("b2", "k", []byte("original"))
		_, err := e.tryPut("b2", "k", []byte("second"), ifNoneMatch)
		e.wantErr("IfNoneMatch on an existing key", err, pf)
		e.wantContent("b2", "k", "", []byte("original"))
	})
	e.run("ExistingEmptyObject", func(e *env) {
		e.mkBucket("b3")
		e.put("b3", "k", nil)
		_, err := e.tryPut("b3", "k", []byte("second"), ifNoneMatch)
		e.wantErr("IfNoneMatch on an existing empty object", err, pf)
	})
	e.run("AfterUnversionedDelete", func(e *env) {
		e.mkBucket("b4")
		e.put("b4", "k", []byte("gone"))
		e.del("b4", "k", "")
		_, err := e.tryPut("b4", "k", []byte("again"), ifNoneMatch)
		e.noErr("IfNoneMatch after delete", err)
	})
	e.run("LatestIsDeleteMarker", func(e *env) {
		e.mkVersioned("b5", storage.VersioningEnabled)
		v := e.put("b5", "k", []byte("old"))
		m := e.del("b5", "k", "")
		info, err := e.tryPut("b5", "k", []byte("new"), ifNoneMatch)
		e.noErr("IfNoneMatch when the latest version is a delete marker", err)
		e.wantHistory("b5", "k",
			vref{VersionID: info.VersionID},
			vref{VersionID: m.VersionID, DeleteMarker: true},
			vref{VersionID: v.VersionID})
	})
	e.run("LatestIsNullDeleteMarker", func(e *env) {
		e.mkVersioned("b6", storage.VersioningSuspended)
		e.put("b6", "k", []byte("old"))
		e.del("b6", "k", "")
		_, err := e.tryPut("b6", "k", []byte("new"), ifNoneMatch)
		e.noErr("IfNoneMatch when the latest version is a null delete marker", err)
		e.wantContent("b6", "k", "", []byte("new"))
	})
	e.run("EnabledExisting", func(e *env) {
		e.mkVersioned("b7", storage.VersioningEnabled)
		v := e.put("b7", "k", []byte("old"))
		_, err := e.tryPut("b7", "k", []byte("new"), ifNoneMatch)
		e.wantErr("IfNoneMatch on an existing versioned key", err, pf)
		e.wantHistory("b7", "k", vref{VersionID: v.VersionID})
	})
}

func testCondIfMatch(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	v1 := e.put("b", "k", []byte("one"))
	ifMatch := func(etag string) storage.CommitOptions { return storage.CommitOptions{IfMatch: etag} }

	_, err := e.tryPut("b", "missing", []byte("x"), ifMatch(etagOf([]byte("x"))))
	e.wantErr("IfMatch on a missing key", err, pf)
	if h := e.history("b", "missing"); len(h) != 0 {
		e.t.Errorf("failed IfMatch commit created versions: %v", h)
	}

	_, err = e.tryPut("b", "k", []byte("two"), ifMatch("0123456789abcdef0123456789abcdef"))
	e.wantErr("IfMatch with a wrong ETag", err, pf)
	e.wantContent("b", "k", "", []byte("one"))

	v2, err := e.tryPut("b", "k", []byte("two"), ifMatch(v1.ETag))
	e.noErr("IfMatch with the current ETag", err)
	e.wantContent("b", "k", "", []byte("two"))

	// Only the latest version counts: v1's ETag no longer matches.
	_, err = e.tryPut("b", "k", []byte("three"), ifMatch(v1.ETag))
	e.wantErr("IfMatch with a noncurrent version's ETag", err, pf)

	// A delete marker as latest never matches.
	e.del("b", "k", "")
	_, err = e.tryPut("b", "k", []byte("three"), ifMatch(v2.ETag))
	e.wantErr("IfMatch when the latest version is a delete marker", err, pf)

	// Unversioned bucket.
	e.mkBucket("u")
	u1 := e.put("u", "k", []byte("one"))
	_, err = e.tryPut("u", "k", []byte("two"), ifMatch(u1.ETag))
	e.noErr("IfMatch in an unversioned bucket", err)
	_, err = e.tryPut("u", "k", []byte("three"), ifMatch(u1.ETag))
	e.wantErr("IfMatch with the replaced ETag", err, pf)
	e.wantContent("u", "k", "", []byte("two"))
}

func testCondFailedInvisible(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	v := e.put("b", "k", []byte("keep"))
	for i := range 3 {
		_, err := e.tryPut("b", "k", fmt.Appendf(nil, "attempt %d", i), ifNoneMatch)
		e.wantErr("IfNoneMatch on an existing key", err, pf)
	}
	e.wantHistory("b", "k", vref{VersionID: v.VersionID})
	e.wantContent("b", "k", "", []byte("keep"))
}

// raceCommits commits n writers concurrently (all data written before any
// commit starts) and returns the indexes that succeeded. Every failure must
// be ErrPreconditionFailed.
func (e *env) raceCommits(n int, bucket, key string, opts func(i int) storage.CommitOptions) []int {
	e.t.Helper()
	writers := make([]storage.ObjectWriter, n)
	for i := range writers {
		w, err := e.b.NewObjectWriter(e.ctx, bucket, key)
		e.noErr("NewObjectWriter", err)
		if _, err := w.Write(raceData(i)); err != nil {
			e.t.Fatalf("Write: %v", err)
		}
		writers[i] = w
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []int
		start   = make(chan struct{})
	)
	for i, w := range writers {
		wg.Go(func() {
			<-start
			_, err := w.Commit(e.ctx, simpleMeta(raceData(i)), opts(i))
			if err != nil {
				_ = w.Abort()
				if !isAny(err, pf) {
					e.t.Errorf("concurrent commit #%d: got error %v, want nil or ErrPreconditionFailed", i, err)
				}
				return
			}
			mu.Lock()
			winners = append(winners, i)
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	return winners
}

func raceData(i int) []byte { return fmt.Appendf(nil, "racer %03d", i) }

func testCondIfNoneMatchRace(e *env) {
	const n = 16
	for _, status := range []storage.VersioningStatus{storage.VersioningUnversioned, storage.VersioningEnabled} {
		e.runFresh(versioningName(status), func(e *env) {
			e.mkVersioned("b", status)
			for round := range 5 {
				key := fmt.Sprintf("race-%d", round)
				winners := e.raceCommits(n, "b", key, func(int) storage.CommitOptions { return ifNoneMatch })
				if len(winners) != 1 {
					e.t.Fatalf("round %d: %d concurrent If-None-Match commits succeeded (%v), want exactly 1", round, len(winners), winners)
				}
				e.wantContent("b", key, "", raceData(winners[0]))
				if h := e.history("b", key); len(h) != 1 {
					e.t.Errorf("round %d: %d versions stored, want 1: %v", round, len(h), h)
				}
			}
		})
	}
}

func testCondIfMatchRace(e *env) {
	const n = 16
	e.mkVersioned("b", storage.VersioningEnabled)
	base := e.put("b", "k", []byte("base"))
	winners := e.raceCommits(n, "b", "k", func(int) storage.CommitOptions { return storage.CommitOptions{IfMatch: base.ETag} })
	if len(winners) != 1 {
		e.t.Fatalf("%d concurrent If-Match commits succeeded (%v), want exactly 1 (compare-and-swap)", len(winners), winners)
	}
	e.wantContent("b", "k", "", raceData(winners[0]))
	if h := e.history("b", "k"); len(h) != 2 {
		e.t.Errorf("%d versions stored, want 2 (base + winner): %v", len(h), h)
	}
}

// uploadWithPart creates an upload of key with a single part holding data
// and returns its ID.
func (e *env) uploadWithPart(bucket, key string, data []byte) string {
	e.t.Helper()
	u := e.createUpload(bucket, key, storage.ObjectMeta{})
	e.part(bucket, key, u.UploadID, 1, data)
	return u.UploadID
}

func testCondMultipartIfNoneMatch(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	id := e.uploadWithPart("b", "k", []byte("mp-1"))
	_, err := e.b.CompleteMultipartUpload(e.ctx, "b", "k", id, []int{1}, simpleMeta([]byte("mp-1")), ifNoneMatch)
	e.noErr("CompleteMultipartUpload IfNoneMatch on a new key", err)

	id = e.uploadWithPart("b", "k", []byte("mp-2"))
	_, err = e.b.CompleteMultipartUpload(e.ctx, "b", "k", id, []int{1}, simpleMeta([]byte("mp-2")), ifNoneMatch)
	e.wantErr("CompleteMultipartUpload IfNoneMatch on an existing key", err, pf)
	e.wantContent("b", "k", "", []byte("mp-1"))
	if h := e.history("b", "k"); len(h) != 1 {
		e.t.Errorf("failed conditional completion stored a version: %v", h)
	}

	e.del("b", "k", "")
	id = e.uploadWithPart("b", "k", []byte("mp-3"))
	_, err = e.b.CompleteMultipartUpload(e.ctx, "b", "k", id, []int{1}, simpleMeta([]byte("mp-3")), ifNoneMatch)
	e.noErr("CompleteMultipartUpload IfNoneMatch over a delete marker", err)
	e.wantContent("b", "k", "", []byte("mp-3"))
}

func testCondMultipartIfMatch(e *env) {
	e.mkBucket("b")
	cur := e.put("b", "k", []byte("current"))
	id := e.uploadWithPart("b", "k", []byte("mp"))
	_, err := e.b.CompleteMultipartUpload(e.ctx, "b", "k", id, []int{1}, simpleMeta([]byte("mp")), storage.CommitOptions{IfMatch: "ffffffffffffffffffffffffffffffff"})
	e.wantErr("CompleteMultipartUpload with a wrong IfMatch", err, pf)
	e.wantContent("b", "k", "", []byte("current"))

	id2 := e.uploadWithPart("b", "k", []byte("mp2"))
	_, err = e.b.CompleteMultipartUpload(e.ctx, "b", "k", id2, []int{1}, simpleMeta([]byte("mp2")), storage.CommitOptions{IfMatch: cur.ETag})
	e.noErr("CompleteMultipartUpload with a matching IfMatch", err)
	e.wantContent("b", "k", "", []byte("mp2"))

	id3 := e.uploadWithPart("b", "missing", []byte("x"))
	_, err = e.b.CompleteMultipartUpload(e.ctx, "b", "missing", id3, []int{1}, simpleMeta([]byte("x")), storage.CommitOptions{IfMatch: cur.ETag})
	e.wantErr("CompleteMultipartUpload IfMatch on a missing key", err, pf)
	_, err = e.b.GetObjectInfo(e.ctx, "b", "missing", "")
	e.wantErr("GetObjectInfo after failed conditional completion", err, storage.ErrObjectNotFound)
}

func testCondMultipartRace(e *env) {
	const n = 8
	e.mkVersioned("b", storage.VersioningEnabled)
	ids := make([]string, n)
	for i := range ids {
		ids[i] = e.uploadWithPart("b", "k", raceData(i))
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []int
		start   = make(chan struct{})
	)
	for i, id := range ids {
		wg.Go(func() {
			<-start
			_, err := e.b.CompleteMultipartUpload(e.ctx, "b", "k", id, []int{1}, simpleMeta(raceData(i)), ifNoneMatch)
			if err != nil {
				if !isAny(err, pf) {
					e.t.Errorf("concurrent completion #%d: got %v, want nil or ErrPreconditionFailed", i, err)
				}
				return
			}
			mu.Lock()
			winners = append(winners, i)
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	if len(winners) != 1 {
		e.t.Fatalf("%d concurrent If-None-Match completions succeeded (%v), want exactly 1", len(winners), winners)
	}
	e.wantContent("b", "k", "", raceData(winners[0]))
	if h := e.history("b", "k"); len(h) != 1 {
		e.t.Errorf("%d versions stored, want 1: %v", len(h), h)
	}
}
