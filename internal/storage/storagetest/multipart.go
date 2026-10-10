package storagetest

import (
	"bytes"
	"cmp"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func multipartTests() []testCase {
	return []testCase{
		{"CreateAndGet", testMultipartCreateGet},
		{"UploadIDsUnique", testMultipartUniqueIDs},
		{"ListUploadsOrder", testMultipartListUploadsOrder},
		{"PartsListAndReplace", testMultipartParts},
		{"PartAbort", testMultipartPartAbort},
		{"CompleteAll", testMultipartCompleteAll},
		{"CompleteSubsetAndOrder", testMultipartCompleteSubset},
		{"CompleteMissingPart", testMultipartCompleteMissingPart},
		{"CompleteChecksPartETags", testMultipartCompletePartETags},
		{"CompleteLarge", testMultipartCompleteLarge},
		{"CompleteEmptyPart", testMultipartCompleteEmptyPart},
		{"UploadGoneAfterCompleteOrAbort", testMultipartGone},
		{"WrongKey", testMultipartWrongKey},
		{"PartCommitAfterAbort", testMultipartPartCommitAfterAbort},
		{"KeepsBucketNonEmpty", testMultipartKeepsBucketNonEmpty},
		{"CompleteHonoursVersioning", testMultipartVersioning},
		{"MetadataIsolation", testMultipartIsolation},
	}
}

// multipartETag computes the S3 multipart ETag of parts as the S3 layer does.
func multipartETag(parts ...[]byte) string {
	var sums []byte
	for _, p := range parts {
		s := md5.Sum(p)
		sums = append(sums, s[:]...)
	}
	s := md5.Sum(sums)
	return fmt.Sprintf("%s-%d", hex.EncodeToString(s[:]), len(parts))
}

// completeMeta returns the metadata the S3 layer would pass to
// CompleteMultipartUpload for parts.
func completeMeta(base storage.ObjectMeta, parts ...[]byte) storage.ObjectMeta {
	m := base
	m.ETag = multipartETag(parts...)
	m.PartSizes = nil
	m.PartChecksums = nil
	for i, p := range parts {
		m.PartSizes = append(m.PartSizes, int64(len(p)))
		m.PartChecksums = append(m.PartChecksums, map[string]string{"CRC32": fmt.Sprintf("part-%d", i+1)})
	}
	return m
}

func (e *env) complete(bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta) storage.ObjectInfo {
	e.t.Helper()
	info, err := e.b.CompleteMultipartUpload(e.ctx, bucket, key, uploadID, partNumbers, meta, storage.CommitOptions{})
	e.noErr(fmt.Sprintf("CompleteMultipartUpload(%s/%q, parts %v)", bucket, key, partNumbers), err)
	return info
}

// wantUploadGone checks every upload-scoped operation reports
// ErrUploadNotFound for uploadID.
func (e *env) wantUploadGone(what, bucket, key, uploadID string) {
	e.t.Helper()
	nf := storage.ErrUploadNotFound
	_, err := e.b.GetMultipartUpload(e.ctx, bucket, key, uploadID)
	e.wantErr(what+": GetMultipartUpload", err, nf)
	w, err := e.b.NewPartWriter(e.ctx, bucket, key, uploadID, 1)
	e.wantErr(what+": NewPartWriter", err, nf)
	if w != nil {
		_ = w.Abort()
	}
	_, err = e.b.ListParts(e.ctx, bucket, key, uploadID)
	e.wantErr(what+": ListParts", err, nf)
	_, err = e.b.CompleteMultipartUpload(e.ctx, bucket, key, uploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{})
	e.wantErr(what+": CompleteMultipartUpload", err, nf)
	e.wantErr(what+": AbortMultipartUpload", e.b.AbortMultipartUpload(e.ctx, bucket, key, uploadID), nf)
	ups, err := e.b.ListMultipartUploads(e.ctx, bucket)
	e.noErr(what+": ListMultipartUploads", err)
	for _, u := range ups {
		if u.UploadID == uploadID && u.Key == key {
			e.t.Errorf("%s: ListMultipartUploads still lists upload %q of %q", what, uploadID, key)
		}
	}
}

func testMultipartCreateGet(e *env) {
	e.mkBucket("b")
	meta := fullMeta()
	meta.ETag, meta.PartSizes, meta.PartChecksums = "", nil, nil // unknown at creation
	start := time.Now()
	u, err := e.b.CreateMultipartUpload(e.ctx, "b", "dir/日本.bin", meta, "alice")
	e.noErr("CreateMultipartUpload", err)
	if u.UploadID == "" {
		e.t.Fatal("CreateMultipartUpload returned an empty UploadID")
	}
	if u.Bucket != "b" || u.Key != "dir/日本.bin" || u.Initiator != "alice" {
		e.t.Errorf("CreateMultipartUpload Bucket/Key/Initiator = %q/%q/%q, want b/dir/日本.bin/alice", u.Bucket, u.Key, u.Initiator)
	}
	e.checkTime("UploadInfo.Initiated", u.Initiated, start)
	e.wantMeta("CreateMultipartUpload", meta, u.Meta)

	got, err := e.b.GetMultipartUpload(e.ctx, "b", "dir/日本.bin", u.UploadID)
	e.noErr("GetMultipartUpload", err)
	if got.UploadID != u.UploadID || got.Bucket != u.Bucket || got.Key != u.Key || got.Initiator != u.Initiator || !got.Initiated.Equal(u.Initiated) {
		e.t.Errorf("GetMultipartUpload = %+v, want %+v", got, u)
	}
	e.wantMeta("GetMultipartUpload", meta, got.Meta)

	ups, err := e.b.ListMultipartUploads(e.ctx, "b")
	e.noErr("ListMultipartUploads", err)
	if len(ups) != 1 || ups[0].UploadID != u.UploadID || ups[0].Key != u.Key || ups[0].Initiator != "alice" {
		e.t.Fatalf("ListMultipartUploads = %+v, want only %q", ups, u.UploadID)
	}
	e.wantMeta("ListMultipartUploads", meta, ups[0].Meta)

	parts, err := e.b.ListParts(e.ctx, "b", "dir/日本.bin", u.UploadID)
	e.noErr("ListParts of a new upload", err)
	if len(parts) != 0 {
		e.t.Errorf("ListParts of a new upload = %+v, want none", parts)
	}
	// An upload is not an object.
	_, err = e.b.GetObjectInfo(e.ctx, "b", "dir/日本.bin", "")
	e.wantErr("GetObjectInfo of an in-progress upload", err, storage.ErrObjectNotFound)
	if keys := e.walkKeys("b", "", ""); len(keys) != 0 {
		e.t.Errorf("WalkObjects lists %q, want nothing while only an upload exists", keys)
	}

	_, err = e.b.GetMultipartUpload(e.ctx, "b", "dir/日本.bin", "no-such-upload")
	e.wantErr("GetMultipartUpload(unknown ID)", err, storage.ErrUploadNotFound)
	_, err = e.b.ListParts(e.ctx, "b", "dir/日本.bin", "no-such-upload")
	e.wantErr("ListParts(unknown ID)", err, storage.ErrUploadNotFound)
	_, err = e.b.NewPartWriter(e.ctx, "b", "dir/日本.bin", "no-such-upload", 1)
	e.wantErr("NewPartWriter(unknown ID)", err, storage.ErrUploadNotFound)
}

func testMultipartUniqueIDs(e *env) {
	e.mkBucket("b")
	e.mkBucket("c")
	seen := map[string]bool{}
	for i := range 20 {
		bucket := []string{"b", "c"}[i%2]
		u := e.createUpload(bucket, "same-key", storage.ObjectMeta{})
		if seen[u.UploadID] {
			e.t.Errorf("upload ID %q issued twice", u.UploadID)
		}
		seen[u.UploadID] = true
		if len(u.UploadID) > 1024 {
			e.t.Errorf("upload ID is %d bytes long", len(u.UploadID))
		}
	}
	ups, err := e.b.ListMultipartUploads(e.ctx, "b")
	e.noErr("ListMultipartUploads", err)
	if len(ups) != 10 {
		e.t.Errorf("ListMultipartUploads(b) returned %d uploads, want 10 (uploads of another bucket must not be listed)", len(ups))
	}
	for _, u := range ups {
		if u.Bucket != "b" || u.Key != "same-key" {
			e.t.Errorf("ListMultipartUploads(b) entry Bucket/Key = %q/%q", u.Bucket, u.Key)
		}
	}
}

func testMultipartListUploadsOrder(e *env) {
	e.mkBucket("b")
	ups, err := e.b.ListMultipartUploads(e.ctx, "b")
	e.noErr("ListMultipartUploads(empty)", err)
	if len(ups) != 0 {
		e.t.Fatalf("ListMultipartUploads on an empty bucket = %+v", ups)
	}
	var created []storage.UploadInfo
	for _, k := range []string{"m", "b", "m", "a/x", "z", "m", "a", "b", "é", "a-"} {
		created = append(created, e.createUpload("b", k, storage.ObjectMeta{}))
		time.Sleep(time.Millisecond) // distinct Initiated times where the backend resolves them
	}
	// Sorted by key, then Initiated (oldest first), then UploadID.
	want := slices.Clone(created)
	slices.SortStableFunc(want, func(x, y storage.UploadInfo) int {
		return cmp.Or(cmp.Compare(x.Key, y.Key), x.Initiated.Compare(y.Initiated), cmp.Compare(x.UploadID, y.UploadID))
	})
	ups, err = e.b.ListMultipartUploads(e.ctx, "b")
	e.noErr("ListMultipartUploads", err)
	ids := func(us []storage.UploadInfo) []string {
		out := make([]string, len(us))
		for i, u := range us {
			out[i] = u.Key + "#" + u.UploadID
		}
		return out
	}
	if !slices.Equal(ids(ups), ids(want)) {
		e.t.Errorf("ListMultipartUploads order:\n got  %q\n want %q", ids(ups), ids(want))
	}
	// Uploads of the same key keep their creation order (Initiated ascending).
	var mGot []string
	for _, u := range ups {
		if u.Key == "m" {
			mGot = append(mGot, u.UploadID)
		}
	}
	mWant := []string{created[0].UploadID, created[2].UploadID, created[5].UploadID}
	if !slices.Equal(mGot, mWant) {
		e.t.Errorf("uploads of key m listed as %q, want creation order %q", mGot, mWant)
	}

	// Completed and aborted uploads disappear from the list.
	e.noErr("AbortMultipartUpload", e.b.AbortMultipartUpload(e.ctx, "b", "m", created[2].UploadID))
	e.part("b", "z", created[4].UploadID, 1, []byte("z"))
	e.complete("b", "z", created[4].UploadID, []int{1}, completeMeta(storage.ObjectMeta{}, []byte("z")))
	ups, err = e.b.ListMultipartUploads(e.ctx, "b")
	e.noErr("ListMultipartUploads", err)
	if len(ups) != len(created)-2 {
		e.t.Errorf("ListMultipartUploads after one abort and one completion returned %d uploads, want %d", len(ups), len(created)-2)
	}
	for _, u := range ups {
		if u.UploadID == created[2].UploadID || u.UploadID == created[4].UploadID {
			e.t.Errorf("ListMultipartUploads still lists finished upload %s#%s", u.Key, u.UploadID)
		}
	}
}

func testMultipartParts(e *env) {
	e.mkBucket("b")
	u := e.createUpload("b", "k", storage.ObjectMeta{})
	start := time.Now()
	p3 := []byte("part three")
	p1 := []byte("part one, the first")
	p2 := []byte("2")
	for _, p := range []struct {
		n    int
		data []byte
	}{{3, p3}, {1, p1}, {10000, []byte("max part number")}, {2, p2}} {
		meta := storage.PartMeta{ETag: etagOf(p.data), Checksums: map[string]string{"CRC32C": fmt.Sprintf("crc-%d", p.n)}}
		info, err := uploadPart(e.ctx, e.b, "b", "k", u.UploadID, p.n, p.data, meta)
		e.noErr(fmt.Sprintf("upload part %d", p.n), err)
		if info.PartNumber != p.n || info.Size != int64(len(p.data)) || info.ETag != meta.ETag || !maps.Equal(info.Checksums, meta.Checksums) {
			e.t.Errorf("part %d Commit = %+v, want PartNumber %d Size %d ETag %s Checksums %v", p.n, info, p.n, len(p.data), meta.ETag, meta.Checksums)
		}
		e.checkTime(fmt.Sprintf("part %d LastModified", p.n), info.LastModified, start)
	}
	parts, err := e.b.ListParts(e.ctx, "b", "k", u.UploadID)
	e.noErr("ListParts", err)
	var nums []int
	for _, p := range parts {
		nums = append(nums, p.PartNumber)
		if p.Checksums["CRC32C"] != fmt.Sprintf("crc-%d", p.PartNumber) {
			e.t.Errorf("ListParts part %d Checksums = %v", p.PartNumber, p.Checksums)
		}
		if p.LastModified.IsZero() {
			e.t.Errorf("ListParts part %d has a zero LastModified", p.PartNumber)
		}
	}
	if !slices.Equal(nums, []int{1, 2, 3, 10000}) {
		e.t.Fatalf("ListParts part numbers = %v, want [1 2 3 10000] (sorted)", nums)
	}
	if parts[0].Size != int64(len(p1)) || parts[0].ETag != etagOf(p1) {
		e.t.Errorf("ListParts part 1 Size/ETag = %d/%s, want %d/%s", parts[0].Size, parts[0].ETag, len(p1), etagOf(p1))
	}

	// Re-uploading a part number replaces it.
	p2b := []byte("replacement for part two, longer")
	info, err := uploadPart(e.ctx, e.b, "b", "k", u.UploadID, 2, p2b, storage.PartMeta{ETag: etagOf(p2b)})
	e.noErr("replace part 2", err)
	if info.Size != int64(len(p2b)) {
		e.t.Errorf("replaced part Size = %d, want %d", info.Size, len(p2b))
	}
	parts, err = e.b.ListParts(e.ctx, "b", "k", u.UploadID)
	e.noErr("ListParts", err)
	if len(parts) != 4 || parts[1].PartNumber != 2 || parts[1].Size != int64(len(p2b)) || parts[1].ETag != etagOf(p2b) || len(parts[1].Checksums) != 0 {
		e.t.Errorf("ListParts after replacing part 2 = %+v, want 4 parts with part 2 of %d bytes, ETag %s and no checksums", parts, len(p2b), etagOf(p2b))
	}

	meta := completeMeta(storage.ObjectMeta{ContentType: "text/plain"}, p1, p2b, p3)
	obj := e.complete("b", "k", u.UploadID, []int{1, 2, 3}, meta)
	want := slices.Concat(p1, p2b, p3)
	if obj.Size != int64(len(want)) {
		e.t.Errorf("completed Size = %d, want %d", obj.Size, len(want))
	}
	e.wantContent("b", "k", "", want)
}

func testMultipartPartAbort(e *env) {
	e.mkBucket("b")
	u := e.createUpload("b", "k", storage.ObjectMeta{})
	e.part("b", "k", u.UploadID, 1, []byte("kept"))
	// Aborting a replacement keeps the committed part.
	w, err := e.b.NewPartWriter(e.ctx, "b", "k", u.UploadID, 1)
	e.noErr("NewPartWriter", err)
	if _, err := w.Write([]byte("discarded replacement")); err != nil {
		e.t.Fatalf("Write: %v", err)
	}
	e.noErr("PartWriter.Abort", w.Abort())
	// Aborting a new part number leaves no part.
	w, err = e.b.NewPartWriter(e.ctx, "b", "k", u.UploadID, 2)
	e.noErr("NewPartWriter", err)
	if _, err := w.Write([]byte("discarded")); err != nil {
		e.t.Fatalf("Write: %v", err)
	}
	e.noErr("PartWriter.Abort", w.Abort())
	// Abort after Commit is a no-op.
	w, err = e.b.NewPartWriter(e.ctx, "b", "k", u.UploadID, 3)
	e.noErr("NewPartWriter", err)
	if _, err := w.Write([]byte("three")); err != nil {
		e.t.Fatalf("Write: %v", err)
	}
	_, err = w.Commit(e.ctx, storage.PartMeta{ETag: etagOf([]byte("three"))})
	e.noErr("PartWriter.Commit", err)
	e.noErr("PartWriter.Abort after Commit", w.Abort())

	parts, err := e.b.ListParts(e.ctx, "b", "k", u.UploadID)
	e.noErr("ListParts", err)
	if len(parts) != 2 || parts[0].PartNumber != 1 || parts[0].Size != 4 || parts[1].PartNumber != 3 || parts[1].Size != 5 {
		e.t.Fatalf("ListParts after aborted part writes = %+v, want parts 1 (4 bytes) and 3 (5 bytes)", parts)
	}
	e.complete("b", "k", u.UploadID, []int{1, 3}, completeMeta(storage.ObjectMeta{}, []byte("kept"), []byte("three")))
	e.wantContent("b", "k", "", []byte("keptthree"))
}

func testMultipartCompleteAll(e *env) {
	e.mkBucket("b")
	base := fullMeta()
	base.ETag, base.PartSizes, base.PartChecksums = "", nil, nil
	u, err := e.b.CreateMultipartUpload(e.ctx, "b", "obj", base, "bob")
	e.noErr("CreateMultipartUpload", err)
	parts := [][]byte{randBytes(1000, 1), randBytes(2000, 2), randBytes(3, 3)}
	for i, p := range parts {
		e.part("b", "obj", u.UploadID, i+1, p)
	}
	meta := completeMeta(base, parts...)
	start := time.Now()
	info := e.complete("b", "obj", u.UploadID, []int{1, 2, 3}, meta)
	want := slices.Concat(parts...)
	if info.Bucket != "b" || info.Key != "obj" || info.VersionID != storage.NullVersionID || !info.IsLatest || info.DeleteMarker {
		e.t.Errorf("CompleteMultipartUpload info = %+v, want latest null version of b/obj", info)
	}
	if info.Size != int64(len(want)) {
		e.t.Errorf("CompleteMultipartUpload Size = %d, want %d", info.Size, len(want))
	}
	e.checkTime("completed LastModified", info.LastModified, start)
	// The metadata passed to Complete is stored verbatim (including
	// PartSizes, PartChecksums and the multipart ETag).
	e.wantMeta("CompleteMultipartUpload result", meta, info.ObjectMeta)
	got := e.wantContent("b", "obj", "", want)
	e.wantMeta("OpenObject of completed upload", meta, got.ObjectMeta)
	e.wantInfo("GetObjectInfo vs Complete", info, e.info("b", "obj", ""))
	e.wantUploadGone("after CompleteMultipartUpload", "b", "obj", u.UploadID)
}

func testMultipartCompleteSubset(e *env) {
	e.mkBucket("b")
	u := e.createUpload("b", "k", storage.ObjectMeta{})
	data := map[int][]byte{}
	for _, n := range []int{1, 2, 3, 4, 7} {
		data[n] = fmt.Appendf(nil, "<part %d>", n)
		e.part("b", "k", u.UploadID, n, data[n])
	}
	// Parts 2 and 7 are not listed and are discarded; listed parts are
	// concatenated in the given order (validating ascending order is the S3
	// layer's job, the backend follows the list).
	order := []int{4, 1, 3}
	info := e.complete("b", "k", u.UploadID, order, completeMeta(storage.ObjectMeta{}, data[4], data[1], data[3]))
	want := slices.Concat(data[4], data[1], data[3])
	if info.Size != int64(len(want)) {
		e.t.Errorf("Size = %d, want %d", info.Size, len(want))
	}
	got := e.wantContent("b", "k", "", want)
	if !slices.Equal(got.PartSizes, []int64{int64(len(data[4])), int64(len(data[1])), int64(len(data[3]))}) {
		e.t.Errorf("PartSizes = %v, want the sizes passed to Complete", got.PartSizes)
	}
	e.wantUploadGone("after completing a subset of parts", "b", "k", u.UploadID)
	// Nothing of the unlisted parts remains: the bucket can be emptied and deleted.
	e.del("b", "k", "")
	e.noErr("DeleteBucket after completion with unlisted parts", e.b.DeleteBucket(e.ctx, "b"))
}

func testMultipartCompleteMissingPart(e *env) {
	e.mkBucket("b")
	e.put("b", "k", []byte("existing"))
	u := e.createUpload("b", "k", storage.ObjectMeta{})
	e.part("b", "k", u.UploadID, 1, []byte("one"))
	e.part("b", "k", u.UploadID, 3, []byte("three"))
	for _, list := range [][]int{{1, 2, 3}, {2}, {1, 3, 4}, {1, 10000}} {
		_, err := e.b.CompleteMultipartUpload(e.ctx, "b", "k", u.UploadID, list, completeMeta(storage.ObjectMeta{}, []byte("x")), storage.CommitOptions{})
		e.wantErr(fmt.Sprintf("CompleteMultipartUpload(parts %v) with missing parts", list), err, storage.ErrPartNotFound)
	}
	// The contract only removes the upload on success; S3 likewise keeps it
	// after InvalidPart so the client can retry. Nothing is published.
	e.wantContent("b", "k", "", []byte("existing"))
	e.wantHistory("b", "k", vref{VersionID: storage.NullVersionID})
	parts, err := e.b.ListParts(e.ctx, "b", "k", u.UploadID)
	e.noErr("ListParts after a failed completion", err)
	if len(parts) != 2 {
		e.t.Fatalf("ListParts after a failed completion = %+v, want parts 1 and 3", parts)
	}
	// Retrying with the right list succeeds.
	e.complete("b", "k", u.UploadID, []int{1, 3}, completeMeta(storage.ObjectMeta{}, []byte("one"), []byte("three")))
	e.wantContent("b", "k", "", []byte("onethree"))
}

// testMultipartCompletePartETags: with CommitOptions.PartETags the
// completion fails with ErrPartNotFound when a part no longer has the ETag
// the caller validated (it was replaced in between), and publishes nothing.
func testMultipartCompletePartETags(e *env) {
	e.mkBucket("b")
	u := e.createUpload("b", "k", storage.ObjectMeta{})
	one := e.part("b", "k", u.UploadID, 1, []byte("one"))
	two := e.part("b", "k", u.UploadID, 2, []byte("two"))
	// Part 2 is replaced after the caller listed it.
	e.part("b", "k", u.UploadID, 2, []byte("TWO"))
	meta := completeMeta(storage.ObjectMeta{}, []byte("one"), []byte("two"))
	_, err := e.b.CompleteMultipartUpload(e.ctx, "b", "k", u.UploadID, []int{1, 2}, meta,
		storage.CommitOptions{PartETags: []string{one.ETag, two.ETag}})
	e.wantErr("CompleteMultipartUpload with a replaced part", err, storage.ErrPartNotFound)
	_, err = e.b.GetObjectInfo(e.ctx, "b", "k", "")
	e.wantErr("GetObjectInfo after a failed completion", err, storage.ErrObjectNotFound)
	if _, err := e.b.ListParts(e.ctx, "b", "k", u.UploadID); err != nil {
		e.t.Fatalf("upload gone after a failed completion: %v", err)
	}
	// With the current ETags it succeeds.
	_, err = e.b.CompleteMultipartUpload(e.ctx, "b", "k", u.UploadID, []int{1, 2}, completeMeta(storage.ObjectMeta{}, []byte("one"), []byte("TWO")),
		storage.CommitOptions{PartETags: []string{one.ETag, etagOf([]byte("TWO"))}})
	e.noErr("CompleteMultipartUpload with the current part ETags", err)
	e.wantContent("b", "k", "", []byte("oneTWO"))
}

func testMultipartCompleteLarge(e *env) {
	e.mkBucket("b")
	u := e.createUpload("b", "big", storage.ObjectMeta{})
	parts := [][]byte{randBytes(5<<20, 10), randBytes(5<<20+7, 11), randBytes(1<<20+3, 12)}
	for i, p := range parts {
		w, err := e.b.NewPartWriter(e.ctx, "b", "big", u.UploadID, i+1)
		e.noErr("NewPartWriter", err)
		for off := 0; off < len(p); off += 300_001 { // irregular chunks
			if _, err := w.Write(p[off:min(off+300_001, len(p))]); err != nil {
				e.t.Fatalf("Write part %d: %v", i+1, err)
			}
		}
		info, err := w.Commit(e.ctx, storage.PartMeta{ETag: etagOf(p)})
		e.noErr("PartWriter.Commit", err)
		if info.Size != int64(len(p)) {
			e.t.Errorf("part %d Size = %d, want %d", i+1, info.Size, len(p))
		}
	}
	info := e.complete("b", "big", u.UploadID, []int{1, 2, 3}, completeMeta(storage.ObjectMeta{}, parts...))
	want := slices.Concat(parts...)
	if info.Size != int64(len(want)) {
		e.t.Errorf("Size = %d, want %d", info.Size, len(want))
	}
	_, got := e.read("b", "big", "")
	if !bytes.Equal(got, want) {
		e.t.Errorf("multipart content: %s", describeBytesDiff(want, got))
	}
}

func testMultipartCompleteEmptyPart(e *env) {
	e.mkBucket("b")
	u := e.createUpload("b", "empty", storage.ObjectMeta{})
	info := e.part("b", "empty", u.UploadID, 1, nil)
	if info.Size != 0 {
		e.t.Errorf("empty part Size = %d", info.Size)
	}
	obj := e.complete("b", "empty", u.UploadID, []int{1}, completeMeta(storage.ObjectMeta{}, nil))
	if obj.Size != 0 {
		e.t.Errorf("object from one empty part Size = %d, want 0", obj.Size)
	}
	e.wantContent("b", "empty", "", []byte{})
}

func testMultipartGone(e *env) {
	e.mkBucket("b")
	u1 := e.uploadWithPart("b", "k", []byte("one"))
	e.complete("b", "k", u1, []int{1}, completeMeta(storage.ObjectMeta{}, []byte("one")))
	e.wantUploadGone("after complete", "b", "k", u1)

	u2 := e.uploadWithPart("b", "k", []byte("two"))
	e.noErr("AbortMultipartUpload", e.b.AbortMultipartUpload(e.ctx, "b", "k", u2))
	e.wantUploadGone("after abort", "b", "k", u2)
	// Aborting does not touch the object.
	e.wantContent("b", "k", "", []byte("one"))

	// Aborting an upload without parts.
	u3 := e.createUpload("b", "k", storage.ObjectMeta{})
	e.noErr("AbortMultipartUpload without parts", e.b.AbortMultipartUpload(e.ctx, "b", "k", u3.UploadID))
	e.wantUploadGone("after abort without parts", "b", "k", u3.UploadID)
	e.wantUploadGone("unknown upload", "b", "k", "never-existed")
}

// testMultipartWrongKey: an upload ID is bound to its bucket and key.
func testMultipartWrongKey(e *env) {
	e.mkBucket("b")
	e.mkBucket("c")
	id := e.uploadWithPart("b", "k", []byte("data"))
	e.wantUploadGone("other key", "b", "k2", id)
	e.wantUploadGone("other bucket", "c", "k", id)
	// The upload is unaffected.
	parts, err := e.b.ListParts(e.ctx, "b", "k", id)
	e.noErr("ListParts", err)
	if len(parts) != 1 {
		e.t.Fatalf("ListParts = %+v, want 1 part", parts)
	}
	e.complete("b", "k", id, []int{1}, completeMeta(storage.ObjectMeta{}, []byte("data")))
	e.wantContent("b", "k", "", []byte("data"))
	_, err = e.b.GetObjectInfo(e.ctx, "b", "k2", "")
	e.wantErr("GetObjectInfo(k2)", err, storage.ErrObjectNotFound)
}

// testMultipartPartCommitAfterAbort: a part writer whose upload is aborted
// (or completed) before Commit must fail (S3: NoSuchUpload) and must not
// resurrect the upload.
func testMultipartPartCommitAfterAbort(e *env) {
	e.mkBucket("b")
	for _, finish := range []string{"abort", "complete"} {
		e.run(finish, func(e *env) {
			key := "k-" + finish
			id := e.uploadWithPart("b", key, []byte("one"))
			w, err := e.b.NewPartWriter(e.ctx, "b", key, id, 2)
			e.noErr("NewPartWriter", err)
			if _, err := w.Write([]byte("late part")); err != nil {
				e.t.Fatalf("Write: %v", err)
			}
			if finish == "abort" {
				e.noErr("AbortMultipartUpload", e.b.AbortMultipartUpload(e.ctx, "b", key, id))
			} else {
				e.complete("b", key, id, []int{1}, completeMeta(storage.ObjectMeta{}, []byte("one")))
			}
			_, err = w.Commit(e.ctx, storage.PartMeta{ETag: etagOf([]byte("late part"))})
			e.wantErr("PartWriter.Commit after the upload finished", err, storage.ErrUploadNotFound)
			_ = w.Abort()
			e.wantUploadGone("after a late part commit", "b", key, id)
		})
	}
}

func testMultipartKeepsBucketNonEmpty(e *env) {
	e.mkBucket("b")
	id := e.uploadWithPart("b", "k", []byte("p"))
	e.wantErr("DeleteBucket with an in-progress upload", e.b.DeleteBucket(e.ctx, "b"), storage.ErrBucketNotEmpty)
	e.noErr("AbortMultipartUpload", e.b.AbortMultipartUpload(e.ctx, "b", "k", id))
	e.noErr("DeleteBucket after aborting the upload", e.b.DeleteBucket(e.ctx, "b"))
}

func testMultipartVersioning(e *env) {
	e.run("Unversioned", func(e *env) {
		e.mkBucket("u")
		e.put("u", "k", []byte("old"))
		id := e.uploadWithPart("u", "k", []byte("new"))
		info := e.complete("u", "k", id, []int{1}, completeMeta(storage.ObjectMeta{}, []byte("new")))
		if info.VersionID != storage.NullVersionID {
			e.t.Errorf("VersionID = %q, want null", info.VersionID)
		}
		e.wantHistory("u", "k", vref{VersionID: storage.NullVersionID})
		e.wantContent("u", "k", "", []byte("new"))
	})
	e.run("Enabled", func(e *env) {
		e.mkVersioned("v", storage.VersioningEnabled)
		old := e.put("v", "k", []byte("old"))
		id := e.uploadWithPart("v", "k", []byte("new"))
		info := e.complete("v", "k", id, []int{1}, completeMeta(storage.ObjectMeta{}, []byte("new")))
		e.checkVersionID("completed upload", info.VersionID)
		if info.VersionID == old.VersionID || !info.IsLatest {
			e.t.Errorf("completed version %q IsLatest %v, want a new latest version", info.VersionID, info.IsLatest)
		}
		e.wantHistory("v", "k", vref{VersionID: info.VersionID}, vref{VersionID: old.VersionID})
		e.wantContent("v", "k", old.VersionID, []byte("old"))
		e.wantContent("v", "k", info.VersionID, []byte("new"))
	})
	e.run("EnabledWhenCompleted", func(e *env) {
		// Versioning is evaluated at completion, not at creation.
		e.mkBucket("w")
		e.put("w", "k", []byte("null"))
		id := e.uploadWithPart("w", "k", []byte("new"))
		e.setVersioning("w", storage.VersioningEnabled)
		info := e.complete("w", "k", id, []int{1}, completeMeta(storage.ObjectMeta{}, []byte("new")))
		e.checkVersionID("completed upload", info.VersionID)
		e.wantHistory("w", "k", vref{VersionID: info.VersionID}, vref{VersionID: storage.NullVersionID})
	})
	e.run("Suspended", func(e *env) {
		e.mkVersioned("s", storage.VersioningEnabled)
		v1 := e.put("s", "k", []byte("v1"))
		e.setVersioning("s", storage.VersioningSuspended)
		e.put("s", "k", []byte("null"))
		id := e.uploadWithPart("s", "k", []byte("new null"))
		info := e.complete("s", "k", id, []int{1}, completeMeta(storage.ObjectMeta{}, []byte("new null")))
		if info.VersionID != storage.NullVersionID {
			e.t.Errorf("VersionID = %q, want null", info.VersionID)
		}
		e.wantHistory("s", "k", vref{VersionID: storage.NullVersionID}, vref{VersionID: v1.VersionID})
		e.wantContent("s", "k", storage.NullVersionID, []byte("new null"))
	})
}

func testMultipartIsolation(e *env) {
	e.mkBucket("b")
	meta := storage.ObjectMeta{ContentType: "a/b", UserMeta: map[string]string{"k": "v"}, Tags: map[string]string{"t": "1"}}
	u, err := e.b.CreateMultipartUpload(e.ctx, "b", "k", meta, "")
	e.noErr("CreateMultipartUpload", err)
	meta.UserMeta["k"] = "MUTATED"
	meta.Tags["x"] = "added"
	u.Meta.UserMeta["k"] = "MUTATED-2"
	got, err := e.b.GetMultipartUpload(e.ctx, "b", "k", u.UploadID)
	e.noErr("GetMultipartUpload", err)
	if !maps.Equal(got.Meta.UserMeta, map[string]string{"k": "v"}) || !maps.Equal(got.Meta.Tags, map[string]string{"t": "1"}) {
		e.t.Errorf("upload metadata changed by caller mutations: UserMeta %v Tags %v", got.Meta.UserMeta, got.Meta.Tags)
	}

	pm := storage.PartMeta{ETag: etagOf([]byte("p")), Checksums: map[string]string{"SHA256": "orig"}}
	pi, err := uploadPart(e.ctx, e.b, "b", "k", u.UploadID, 1, []byte("p"), pm)
	e.noErr("upload part", err)
	pm.Checksums["SHA256"] = "MUTATED"
	if pi.Checksums != nil {
		pi.Checksums["SHA256"] = "MUTATED-2"
	}
	parts, err := e.b.ListParts(e.ctx, "b", "k", u.UploadID)
	e.noErr("ListParts", err)
	if len(parts) != 1 || parts[0].Checksums["SHA256"] != "orig" {
		e.t.Fatalf("part checksums changed by caller mutations: %+v", parts)
	}
	parts[0].Checksums["SHA256"] = "MUTATED-3"
	parts, err = e.b.ListParts(e.ctx, "b", "k", u.UploadID)
	e.noErr("ListParts", err)
	if parts[0].Checksums["SHA256"] != "orig" {
		e.t.Errorf("part checksums changed by mutating a ListParts result: %v", parts[0].Checksums)
	}

	cm := completeMeta(storage.ObjectMeta{UserMeta: map[string]string{"final": "yes"}}, []byte("p"))
	want := completeMeta(storage.ObjectMeta{UserMeta: map[string]string{"final": "yes"}}, []byte("p"))
	e.complete("b", "k", u.UploadID, []int{1}, cm)
	cm.UserMeta["final"] = "MUTATED"
	cm.PartSizes[0] = -1
	cm.PartChecksums[0]["CRC32"] = "MUTATED"
	e.wantMeta("completed object after caller mutations", want, e.info("b", "k", "").ObjectMeta)
}
