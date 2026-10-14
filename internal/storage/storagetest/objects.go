package storagetest

import (
	"bytes"
	"crypto/sha256"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func objectTests() []testCase {
	return []testCase{
		{"PutGetHead", testObjectPutGetHead},
		{"MetadataRoundTrip", testObjectMetadataRoundTrip},
		{"BackendSetsSizeAndLastModified", testObjectBackendSetsFields},
		{"Overwrite", testObjectOverwrite},
		{"Empty", testObjectEmpty},
		{"LargeIntegrityAndSeek", testObjectLarge},
		{"Keys", testObjectKeys},
		{"ReaderSurvivesOverwrite", testObjectReaderSurvivesOverwrite},
		{"ReaderSurvivesDelete", testObjectReaderSurvivesDelete},
		{"Abort", testObjectAbort},
		{"AbortAfterCommit", testObjectAbortAfterCommit},
		{"WriterMissingBucket", testObjectWriterMissingBucket},
		{"WriterBucketDeleted", testObjectWriterBucketDeleted},
		{"MetadataIsolation", testObjectMetadataIsolation},
		{"WriteBufferReuse", testObjectWriteBufferReuse},
		{"NotFound", testObjectNotFound},
		{"UnversionedDelete", testObjectUnversionedDelete},
		{"BucketIsolation", testObjectBucketIsolation},
	}
}

func testObjectPutGetHead(e *env) {
	e.mkBucket("b")
	data := []byte("hello, world")
	meta := simpleMeta(data)
	start := time.Now()
	committed := e.putMeta("b", "greeting.txt", data, meta)
	if committed.Bucket != "b" || committed.Key != "greeting.txt" {
		e.t.Errorf("Commit Bucket/Key = %q/%q, want b/greeting.txt", committed.Bucket, committed.Key)
	}
	if committed.VersionID != storage.NullVersionID {
		e.t.Errorf("Commit VersionID = %q, want %q in an unversioned bucket", committed.VersionID, storage.NullVersionID)
	}
	if !committed.IsLatest || committed.DeleteMarker {
		e.t.Errorf("Commit IsLatest/DeleteMarker = %v/%v, want true/false", committed.IsLatest, committed.DeleteMarker)
	}
	if committed.Size != int64(len(data)) {
		e.t.Errorf("Commit Size = %d, want %d", committed.Size, len(data))
	}
	e.checkTime("Commit LastModified", committed.LastModified, start)
	e.wantMeta("Commit", meta, committed.ObjectMeta)

	// GetObjectInfo (HEAD) and OpenObject (GET) report exactly what Commit returned.
	e.wantInfo("GetObjectInfo vs Commit", committed, e.info("b", "greeting.txt", ""))
	e.wantInfo("GetObjectInfo(null) vs Commit", committed, e.info("b", "greeting.txt", storage.NullVersionID))
	got := e.wantContent("b", "greeting.txt", "", data)
	e.wantInfo("OpenObject vs Commit", committed, got)
	got = e.wantContent("b", "greeting.txt", storage.NullVersionID, data)
	e.wantInfo("OpenObject(null) vs Commit", committed, got)
}

func testObjectMetadataRoundTrip(e *env) {
	e.mkBucket("b")
	for _, tc := range []struct {
		name string
		meta storage.ObjectMeta
	}{
		{"Full", fullMeta()},
		{"Zero", storage.ObjectMeta{}},
		{"EmptyMaps", storage.ObjectMeta{ETag: "x", UserMeta: map[string]string{}, Tags: map[string]string{}, Checksums: map[string]string{}}},
		{"UTCRetention", storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: time.Date(2040, 12, 31, 23, 59, 59, 999999999, time.UTC)}},
	} {
		e.run(tc.name, func(e *env) {
			key := "meta-" + tc.name
			data := []byte(tc.name)
			committed := e.putMeta("b", key, data, tc.meta)
			e.wantMeta("Commit", tc.meta, committed.ObjectMeta)
			e.wantMeta("GetObjectInfo", tc.meta, e.info("b", key, "").ObjectMeta)
			info, _ := e.read("b", key, "")
			e.wantMeta("OpenObject", tc.meta, info.ObjectMeta)
			walked := e.walkObjects("b", key, "")
			if len(walked) != 1 {
				e.t.Fatalf("WalkObjects(prefix %q) returned %d objects, want 1", key, len(walked))
			}
			e.wantMeta("WalkObjects", tc.meta, walked[0].ObjectMeta)
			versions := e.walkVersions("b", key, "", "")
			if len(versions) != 1 {
				e.t.Fatalf("WalkVersions(prefix %q) returned %d versions, want 1", key, len(versions))
			}
			e.wantMeta("WalkVersions", tc.meta, versions[0].ObjectMeta)
		})
	}
}

// testObjectBackendSetsFields checks that Size and LastModified come from
// the backend, not from anything the caller supplies.
func testObjectBackendSetsFields(e *env) {
	e.mkBucket("b")
	meta := simpleMeta(nil)
	meta.PartSizes = []int64{100, 200} // must not influence Size
	start := time.Now()
	info := e.putMeta("b", "k", []byte("12345"), meta)
	if info.Size != 5 {
		e.t.Errorf("Size = %d, want 5 (number of bytes written)", info.Size)
	}
	e.checkTime("LastModified", info.LastModified, start)
	time.Sleep(10 * time.Millisecond)
	info2 := e.put("b", "k", []byte("123"))
	if info2.Size != 3 {
		e.t.Errorf("Size after overwrite = %d, want 3", info2.Size)
	}
	if info2.LastModified.Before(info.LastModified) {
		e.t.Errorf("LastModified went backwards on overwrite: %s then %s", fmtTime(info.LastModified), fmtTime(info2.LastModified))
	}
}

func testObjectOverwrite(e *env) {
	e.mkBucket("b")
	m1 := simpleMeta([]byte("first"))
	m1.UserMeta = map[string]string{"old": "1"}
	m1.Tags = map[string]string{"t": "1"}
	m1.ContentType = "text/plain"
	e.putMeta("b", "k", []byte("first"), m1)
	m2 := simpleMeta([]byte("second, longer"))
	committed := e.putMeta("b", "k", []byte("second, longer"), m2)
	if committed.VersionID != storage.NullVersionID {
		e.t.Errorf("overwrite VersionID = %q, want null", committed.VersionID)
	}
	got := e.wantContent("b", "k", "", []byte("second, longer"))
	// Metadata is replaced, never merged with the previous version's.
	e.wantMeta("after overwrite", m2, got.ObjectMeta)
	e.wantHistory("b", "k", vref{VersionID: storage.NullVersionID})
	// Overwrite with a shorter object truncates.
	e.put("b", "k", []byte("x"))
	e.wantContent("b", "k", "", []byte("x"))
}

func testObjectEmpty(e *env) {
	e.mkBucket("b")
	// No Write call at all.
	info := e.put("b", "no-write", nil)
	if info.Size != 0 {
		e.t.Errorf("Size = %d, want 0", info.Size)
	}
	e.wantContent("b", "no-write", "", []byte{})
	// Zero-length writes.
	w, err := e.b.NewObjectWriter(e.ctx, "b", "zero-writes")
	e.noErr("NewObjectWriter", err)
	for range 3 {
		if n, err := w.Write([]byte{}); n != 0 || err != nil {
			e.t.Fatalf("Write(empty) = %d, %v; want 0, nil", n, err)
		}
	}
	_, err = w.Commit(e.ctx, simpleMeta(nil), storage.CommitOptions{})
	e.noErr("Commit", err)
	e.wantContent("b", "zero-writes", "", []byte{})
	if keys := e.walkKeys("b", "", ""); !slices.Equal(keys, []string{"no-write", "zero-writes"}) {
		e.t.Errorf("WalkObjects = %q, want both empty objects", keys)
	}
	// Overwriting a non-empty object with an empty one.
	e.put("b", "shrink", []byte("data"))
	e.put("b", "shrink", nil)
	e.wantContent("b", "shrink", "", []byte{})
}

func testObjectLarge(e *env) {
	e.mkBucket("b")
	const size = 8 << 20
	data := randBytes(size, 42)
	w, err := e.b.NewObjectWriter(e.ctx, "b", "large.bin")
	e.noErr("NewObjectWriter", err)
	// Irregular chunk sizes exercise any internal buffering.
	chunks := []int{1, 7, 4096, 1<<20 + 3, 65535, 3 << 20, 13}
	off := 0
	for i := 0; off < size; i++ {
		n := min(chunks[i%len(chunks)], size-off)
		wn, err := w.Write(data[off : off+n])
		if err != nil || wn != n {
			e.t.Fatalf("Write(%d bytes at offset %d) = %d, %v", n, off, wn, err)
		}
		off += n
	}
	info, err := w.Commit(e.ctx, simpleMeta(data), storage.CommitOptions{})
	e.noErr("Commit", err)
	if info.Size != size {
		e.t.Errorf("Commit Size = %d, want %d", info.Size, size)
	}

	oi, r, err := e.b.OpenObject(e.ctx, "b", "large.bin", "")
	e.noErr("OpenObject", err)
	if r == nil {
		e.t.Fatal("OpenObject returned a nil reader")
	}
	defer r.Close()
	if oi.Size != size {
		e.t.Errorf("OpenObject Size = %d, want %d", oi.Size, size)
	}
	h := sha256.New()
	n, err := io.Copy(h, r)
	e.noErr("reading large object", err)
	want := sha256.Sum256(data)
	if n != size || !bytes.Equal(h.Sum(nil), want[:]) {
		e.t.Fatalf("large object content differs: read %d bytes, sha256 %x, want %d bytes sha256 %x", n, h.Sum(nil), size, want)
	}

	// Seeking.
	seekRead := func(what string, offset int64, whence int, wantPos int64, length int) {
		e.t.Helper()
		pos, err := r.Seek(offset, whence)
		if err != nil || pos != wantPos {
			e.t.Fatalf("%s: Seek(%d, %d) = %d, %v; want %d", what, offset, whence, pos, err, wantPos)
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(r, buf); err != nil {
			e.t.Fatalf("%s: reading %d bytes at %d: %v", what, length, wantPos, err)
		}
		if !bytes.Equal(buf, data[wantPos:wantPos+int64(length)]) {
			e.t.Errorf("%s: content at offset %d differs", what, wantPos)
		}
	}
	seekRead("SeekStart", 1000, io.SeekStart, 1000, 4096)
	seekRead("SeekCurrent", 100, io.SeekCurrent, 1000+4096+100, 10)
	seekRead("SeekEnd", -10, io.SeekEnd, size-10, 10)
	if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		e.t.Errorf("Read at end = %d, %v; want 0, io.EOF", n, err)
	}
	seekRead("SeekStart(5MiB)", 5<<20, io.SeekStart, 5<<20, 1<<20)
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		e.t.Fatalf("Seek(0, SeekStart): %v", err)
	}
	all, err := io.ReadAll(r)
	e.noErr("re-reading after seek", err)
	if !bytes.Equal(all, data) {
		e.t.Errorf("re-read after Seek(0): %s", describeBytesDiff(data, all))
	}
}

// testKeys are object keys that must be stored and listed verbatim.
var testKeys = []string{
	"plain",
	"with space",
	"unicode/日本語/ファイル.txt",
	"emoji-\U0001F600",
	"accent-é",
	"a/b/c",
	"a//b",
	"a/b",
	"a",
	"trailing/",
	"/leading",
	"//",
	"dots/../x",
	"./dot",
	"percent%20+plus&amp=?q#frag",
	"back\\slash",
	"star*brackets[]{}",
	"tab\tand\nnewline",
	"nul\x00byte",
	"nul\x00",
	"quote\"'<>",
	strings.Repeat("k", 1024),
	strings.Repeat("é", 512), // 1024 bytes of 2-byte runes
}

func testObjectKeys(e *env) {
	e.mkBucket("b")
	for i, k := range testKeys {
		data := []byte("content of key #" + string(rune('A'+i)))
		info := e.put("b", k, data)
		if info.Key != k {
			e.t.Errorf("Commit Key = %q, want %q", info.Key, k)
		}
	}
	for i, k := range testKeys {
		info := e.wantContent("b", k, "", []byte("content of key #"+string(rune('A'+i))))
		if info.Key != k {
			e.t.Errorf("OpenObject Key = %q, want %q", info.Key, k)
		}
	}
	want := slices.Clone(testKeys)
	sort.Strings(want)
	if got := e.walkKeys("b", "", ""); !slices.Equal(got, want) {
		e.t.Errorf("WalkObjects keys:\n got  %q\n want %q", got, want)
	}
	// Similar keys are distinct objects.
	e.del("b", "a/b", "")
	e.wantContent("b", "a//b", "", []byte("content of key #"+string(rune('A'+slices.Index(testKeys, "a//b")))))
	e.wantContent("b", "a", "", []byte("content of key #"+string(rune('A'+slices.Index(testKeys, "a")))))
	_, err := e.b.GetObjectInfo(e.ctx, "b", "a/b", "")
	e.wantErr("GetObjectInfo of deleted key", err, storage.ErrObjectNotFound)
}

func testObjectReaderSurvivesOverwrite(e *env) {
	for _, status := range []storage.VersioningStatus{storage.VersioningUnversioned, storage.VersioningEnabled} {
		e.runFresh(versioningName(status), func(e *env) {
			e.mkVersioned("b", status)
			v1 := randBytes(256<<10, 1)
			v2 := randBytes(128<<10, 2)
			e.put("b", "k", v1)
			_, r, err := e.b.OpenObject(e.ctx, "b", "k", "")
			e.noErr("OpenObject", err)
			defer r.Close()
			head := make([]byte, 1000)
			if _, err := io.ReadFull(r, head); err != nil {
				e.t.Fatalf("partial read: %v", err)
			}
			e.put("b", "k", v2) // overwrite while the reader is open
			rest, err := io.ReadAll(r)
			e.noErr("reading after overwrite", err)
			if got := append(head, rest...); !bytes.Equal(got, v1) {
				e.t.Errorf("reader opened before overwrite: %s", describeBytesDiff(v1, got))
			}
			if _, err := r.Seek(0, io.SeekStart); err != nil {
				e.t.Fatalf("Seek after overwrite: %v", err)
			}
			again, err := io.ReadAll(r)
			e.noErr("re-reading after overwrite", err)
			if !bytes.Equal(again, v1) {
				e.t.Errorf("re-read after overwrite: %s", describeBytesDiff(v1, again))
			}
			e.wantContent("b", "k", "", v2)
		})
	}
}

func testObjectReaderSurvivesDelete(e *env) {
	for _, status := range []storage.VersioningStatus{storage.VersioningUnversioned, storage.VersioningEnabled} {
		e.runFresh(versioningName(status), func(e *env) {
			e.mkVersioned("b", status)
			data := randBytes(300<<10, 3)
			info := e.put("b", "k", data)
			_, r, err := e.b.OpenObject(e.ctx, "b", "k", info.VersionID)
			e.noErr("OpenObject", err)
			defer r.Close()
			half := make([]byte, len(data)/2)
			if _, err := io.ReadFull(r, half); err != nil {
				e.t.Fatalf("partial read: %v", err)
			}
			// Permanently delete the exact version being read.
			e.del("b", "k", info.VersionID)
			if _, err := e.b.GetObjectInfo(e.ctx, "b", "k", ""); !isAny(err, storage.ErrObjectNotFound) {
				e.t.Fatalf("GetObjectInfo after deleting the only version: err = %v, want ErrObjectNotFound", err)
			}
			rest, err := io.ReadAll(r)
			e.noErr("reading after delete", err)
			if got := append(half, rest...); !bytes.Equal(got, data) {
				e.t.Errorf("reader opened before delete: %s", describeBytesDiff(data, got))
			}
		})
	}
}

func testObjectAbort(e *env) {
	e.mkBucket("b")
	w, err := e.b.NewObjectWriter(e.ctx, "b", "aborted")
	e.noErr("NewObjectWriter", err)
	if _, err := w.Write(randBytes(100<<10, 5)); err != nil {
		e.t.Fatalf("Write: %v", err)
	}
	e.noErr("Abort", w.Abort())
	_, err = e.b.GetObjectInfo(e.ctx, "b", "aborted", "")
	e.wantErr("GetObjectInfo after Abort", err, storage.ErrObjectNotFound)
	if keys := e.walkKeys("b", "", ""); len(keys) != 0 {
		e.t.Errorf("WalkObjects after Abort = %q, want nothing", keys)
	}
	if v := e.walkVersions("b", "", "", ""); len(v) != 0 {
		e.t.Errorf("WalkVersions after Abort = %v, want nothing", refsOf(v))
	}
	// Aborting a write over an existing object keeps the existing object.
	e.put("b", "existing", []byte("original"))
	w, err = e.b.NewObjectWriter(e.ctx, "b", "existing")
	e.noErr("NewObjectWriter", err)
	if _, err := w.Write([]byte("replacement")); err != nil {
		e.t.Fatalf("Write: %v", err)
	}
	e.noErr("Abort", w.Abort())
	e.wantContent("b", "existing", "", []byte("original"))
	// Aborting a writer without writing anything.
	w, err = e.b.NewObjectWriter(e.ctx, "b", "nothing")
	e.noErr("NewObjectWriter", err)
	e.noErr("Abort", w.Abort())
	// An aborted write leaves the bucket deletable.
	e.del("b", "existing", "")
	e.noErr("DeleteBucket after aborted writes", e.b.DeleteBucket(e.ctx, "b"))
}

func testObjectAbortAfterCommit(e *env) {
	e.mkBucket("b")
	w, err := e.b.NewObjectWriter(e.ctx, "b", "k")
	e.noErr("NewObjectWriter", err)
	if _, err := w.Write([]byte("committed")); err != nil {
		e.t.Fatalf("Write: %v", err)
	}
	_, err = w.Commit(e.ctx, simpleMeta([]byte("committed")), storage.CommitOptions{})
	e.noErr("Commit", err)
	e.noErr("Abort after Commit (must be a no-op)", w.Abort())
	e.wantContent("b", "k", "", []byte("committed"))
}

func testObjectWriterMissingBucket(e *env) {
	w, err := e.b.NewObjectWriter(e.ctx, "missing", "k")
	e.wantErr("NewObjectWriter on missing bucket", err, storage.ErrBucketNotFound)
	if w != nil {
		e.t.Error("NewObjectWriter returned a non-nil writer together with an error")
	}
}

// testObjectWriterBucketDeleted: a commit into a bucket deleted after the
// writer was created must fail (S3: NoSuchBucket) and not resurrect it.
func testObjectWriterBucketDeleted(e *env) {
	e.mkBucket("b")
	w, err := e.b.NewObjectWriter(e.ctx, "b", "k")
	e.noErr("NewObjectWriter", err)
	if _, err := w.Write([]byte("orphan")); err != nil {
		e.t.Fatalf("Write: %v", err)
	}
	e.noErr("DeleteBucket", e.b.DeleteBucket(e.ctx, "b"))
	_, err = w.Commit(e.ctx, simpleMeta([]byte("orphan")), storage.CommitOptions{})
	e.wantErr("Commit after the bucket was deleted", err, storage.ErrBucketNotFound)
	_ = w.Abort()
	_, err = e.b.GetBucket(e.ctx, "b")
	e.wantErr("GetBucket after failed commit", err, storage.ErrBucketNotFound)
}

func testObjectMetadataIsolation(e *env) {
	e.mkBucket("b")
	meta := fullMeta()
	want := fullMeta()
	committed := e.putMeta("b", "k", []byte("data"), meta)

	// Caller mutates the maps/slices it passed to Commit.
	meta.UserMeta["color"] = "MUTATED"
	meta.UserMeta["added"] = "x"
	meta.Tags["project"] = "MUTATED"
	meta.Checksums["CRC32"] = "MUTATED"
	meta.PartSizes[0] = -1
	meta.PartChecksums[0]["CRC32"] = "MUTATED"
	e.wantMeta("after mutating the caller's metadata", want, e.info("b", "k", "").ObjectMeta)

	// Caller mutates maps/slices returned by the backend.
	mutate := func(m storage.ObjectMeta) {
		if m.UserMeta != nil {
			m.UserMeta["color"] = "MUTATED"
		}
		if m.Tags != nil {
			m.Tags["project"] = "MUTATED"
		}
		if m.Checksums != nil {
			m.Checksums["CRC32"] = "MUTATED"
		}
		if len(m.PartSizes) > 0 {
			m.PartSizes[0] = -1
		}
		if len(m.PartChecksums) > 0 && m.PartChecksums[0] != nil {
			m.PartChecksums[0]["CRC32"] = "MUTATED"
		}
	}
	mutate(committed.ObjectMeta)
	e.wantMeta("after mutating the metadata returned by Commit", want, e.info("b", "k", "").ObjectMeta)
	mutate(e.info("b", "k", "").ObjectMeta)
	e.wantMeta("after mutating the metadata returned by GetObjectInfo", want, e.info("b", "k", "").ObjectMeta)
	info, _ := e.read("b", "k", "")
	mutate(info.ObjectMeta)
	e.wantMeta("after mutating the metadata returned by OpenObject", want, e.info("b", "k", "").ObjectMeta)
	for _, o := range e.walkObjects("b", "", "") {
		mutate(o.ObjectMeta)
	}
	for _, o := range e.walkVersions("b", "", "", "") {
		mutate(o.ObjectMeta)
	}
	e.wantMeta("after mutating the metadata returned by walks", want, e.info("b", "k", "").ObjectMeta)
	if !maps.Equal(meta.Tags, map[string]string{"project": "MUTATED", "k 2": "v=2&x"}) {
		e.t.Errorf("backend modified the caller's Tags map: %v", meta.Tags)
	}
}

// testObjectWriteBufferReuse: io.Writer implementations must not retain p,
// so the caller may reuse its buffer after Write returns.
func testObjectWriteBufferReuse(e *env) {
	e.mkBucket("b")
	w, err := e.b.NewObjectWriter(e.ctx, "b", "k")
	e.noErr("NewObjectWriter", err)
	buf := make([]byte, 4)
	var want []byte
	for i := range 64 {
		for j := range buf {
			buf[j] = byte('a' + (i+j)%26)
		}
		want = append(want, buf...)
		if _, err := w.Write(buf); err != nil {
			e.t.Fatalf("Write: %v", err)
		}
	}
	for j := range buf {
		buf[j] = '!'
	}
	_, err = w.Commit(e.ctx, simpleMeta(want), storage.CommitOptions{})
	e.noErr("Commit", err)
	e.wantContent("b", "k", "", want)
}

func testObjectNotFound(e *env) {
	e.mkBucket("b")
	_, err := e.b.GetObjectInfo(e.ctx, "b", "missing", "")
	e.wantErr("GetObjectInfo(missing)", err, storage.ErrObjectNotFound)
	info, r, err := e.b.OpenObject(e.ctx, "b", "missing", "")
	e.wantErr("OpenObject(missing)", err, storage.ErrObjectNotFound)
	if r != nil {
		e.t.Error("OpenObject returned a non-nil reader for a missing object")
		_ = r.Close()
	}
	if info.DeleteMarker {
		e.t.Error("OpenObject(missing) reported a delete marker")
	}
	e.put("b", "present", []byte("x"))
	_, err = e.b.GetObjectInfo(e.ctx, "b", "presen", "")
	e.wantErr("GetObjectInfo(prefix of an existing key)", err, storage.ErrObjectNotFound)
	_, err = e.b.GetObjectInfo(e.ctx, "b", "present/", "")
	e.wantErr("GetObjectInfo(existing key + \"/\")", err, storage.ErrObjectNotFound)
}

func testObjectUnversionedDelete(e *env) {
	e.mkBucket("b")
	// Deleting a missing key is not an error and creates nothing.
	res := e.del("b", "missing", "")
	if res != (storage.DeleteResult{}) {
		e.t.Errorf("DeleteObject(missing) = %+v, want DeleteResult{}", res)
	}
	if v := e.walkVersions("b", "", "", ""); len(v) != 0 {
		e.t.Errorf("deleting a missing key in an unversioned bucket created %v", refsOf(v))
	}

	e.put("b", "k", []byte("v"))
	res = e.del("b", "k", "")
	if res.DeleteMarker {
		e.t.Errorf("DeleteObject in an unversioned bucket = %+v, want no delete marker", res)
	}
	_, err := e.b.GetObjectInfo(e.ctx, "b", "k", "")
	e.wantErr("GetObjectInfo after delete", err, storage.ErrObjectNotFound)
	if v := e.walkVersions("b", "", "", ""); len(v) != 0 {
		e.t.Errorf("WalkVersions after unversioned delete = %v, want nothing", refsOf(v))
	}
	res = e.del("b", "k", "")
	if res != (storage.DeleteResult{}) {
		e.t.Errorf("second DeleteObject = %+v, want DeleteResult{}", res)
	}

	// Deleting the null version explicitly.
	e.put("b", "k2", []byte("v"))
	res = e.del("b", "k2", storage.NullVersionID)
	if res.VersionID != storage.NullVersionID || res.DeleteMarker {
		e.t.Errorf("DeleteObject(k2, null) = %+v, want {VersionID: null, DeleteMarker: false}", res)
	}
	_, err = e.b.GetObjectInfo(e.ctx, "b", "k2", "")
	e.wantErr("GetObjectInfo after deleting the null version", err, storage.ErrObjectNotFound)
	_, err = e.b.DeleteObject(e.ctx, "b", "k2", storage.NullVersionID)
	e.wantErr("DeleteObject(null) of a missing version", err, storage.ErrVersionNotFound)
}

func testObjectBucketIsolation(e *env) {
	e.mkBucket("one")
	e.mkBucket("two")
	e.put("one", "k", []byte("in one"))
	e.put("two", "k", []byte("in two"))
	e.wantContent("one", "k", "", []byte("in one"))
	e.wantContent("two", "k", "", []byte("in two"))
	e.del("one", "k", "")
	e.wantContent("two", "k", "", []byte("in two"))
	e.put("one", "only-in-one", []byte("x"))
	if keys := e.walkKeys("two", "", ""); !slices.Equal(keys, []string{"k"}) {
		e.t.Errorf("WalkObjects(two) = %q, want [k]", keys)
	}
	if info := e.info("two", "k", ""); info.Bucket != "two" {
		e.t.Errorf("GetObjectInfo Bucket = %q, want two", info.Bucket)
	}
}

func versioningName(s storage.VersioningStatus) string {
	if s == storage.VersioningUnversioned {
		return "Unversioned"
	}
	return string(s)
}
