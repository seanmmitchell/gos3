package storagetest

import (
	"bytes"
	"fmt"
	"io"
	"maps"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Tests of storage.CompressingBackend: compression must be invisible to
// readers (content, Size, seeking) and reported in ObjectInfo.

var deflate1 = storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1}

func (e *env) compressing() storage.CompressingBackend {
	e.t.Helper()
	cb, ok := e.b.(storage.CompressingBackend)
	if !ok {
		e.t.Skip("backend does not implement storage.CompressingBackend")
	}
	return cb
}

// textBytes returns n bytes of compressible, non-repeating-looking text.
func textBytes(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "line %d: the quick brown fox jumps over the lazy dog\n", i)
	}
	return b.Bytes()[:n]
}

// putWith writes data in uneven chunks through NewObjectWriterWith.
func (e *env) putWith(cb storage.CompressingBackend, bucket, key string, data []byte, c storage.Compression) storage.ObjectInfo {
	e.t.Helper()
	w, err := cb.NewObjectWriterWith(e.ctx, bucket, key, storage.WriteOptions{Compression: c})
	e.noErr(fmt.Sprintf("NewObjectWriterWith(%s/%q)", bucket, key), err)
	for p := data; len(p) > 0; {
		n := min(len(p), 100_003)
		if _, err := w.Write(p[:n]); err != nil {
			_ = w.Abort()
			e.t.Fatalf("write %s/%q: %v", bucket, key, err)
		}
		p = p[n:]
	}
	info, err := w.Commit(e.ctx, simpleMeta(data), storage.CommitOptions{})
	if err != nil {
		_ = w.Abort()
	}
	e.noErr(fmt.Sprintf("commit %s/%q", bucket, key), err)
	return info
}

// wantCompressed checks how info says a version of size bytes is stored.
func (e *env) wantCompressed(what string, info storage.ObjectInfo, size int64, compressed bool) {
	e.t.Helper()
	if info.Size != size {
		e.t.Errorf("%s: Size = %d, want %d", what, info.Size, size)
	}
	switch {
	case !compressed && (info.Compression != "" || info.StoredSize != 0):
		e.t.Errorf("%s: Compression = %q, StoredSize = %d, want an uncompressed version", what, info.Compression, info.StoredSize)
	case compressed && info.Compression != storage.CompressionDeflate:
		e.t.Errorf("%s: Compression = %q, want %q", what, info.Compression, storage.CompressionDeflate)
	case compressed && (info.StoredSize <= 0 || info.PhysicalSize() != info.StoredSize):
		e.t.Errorf("%s: StoredSize = %d, PhysicalSize = %d", what, info.StoredSize, info.PhysicalSize())
	}
}

func testCompression(e *env) {
	e.compressing()

	e.runFresh("RoundTrip", func(e *env) {
		cb := e.compressing()
		e.mkBucket("c")
		data := textBytes(700_001)
		info := e.putWith(cb, "c", "text", data, deflate1)
		e.wantCompressed("Commit", info, int64(len(data)), true)
		if info.StoredSize >= info.Size/2 {
			e.t.Errorf("text stored in %d of %d bytes", info.StoredSize, info.Size)
		}
		e.wantCompressed("GetObjectInfo", e.info("c", "text", ""), int64(len(data)), true)
		e.wantCompressed("OpenObject", e.wantContent("c", "text", "", data), int64(len(data)), true)
		for _, o := range e.walkObjects("c", "", "") {
			e.wantCompressed("WalkObjects", o, int64(len(data)), true)
		}
		for _, v := range e.walkVersions("c", "", "", "") {
			e.wantCompressed("WalkVersions", v, int64(len(data)), true)
		}

		// Seeking works in content offsets.
		_, r, err := e.b.OpenObject(e.ctx, "c", "text", "")
		e.noErr("OpenObject", err)
		defer r.Close()
		for _, off := range []int64{0, 1, 262_143, 262_144, 600_000, int64(len(data)) - 1} {
			if _, err := r.Seek(off, io.SeekStart); err != nil {
				e.t.Fatalf("Seek(%d): %v", off, err)
			}
			buf := make([]byte, 5000)
			n, err := io.ReadFull(r, buf)
			want := data[off:min(off+5000, int64(len(data)))]
			if n != len(want) || !bytes.Equal(buf[:n], want) {
				e.t.Fatalf("read at %d: %d bytes (%v), want %d matching", off, n, err, len(want))
			}
		}

		// Incompressible data and data written without compression read
		// back the same way.
		noise := randBytes(300_000, 7)
		e.wantCompressed("random data", e.putWith(cb, "c", "noise", noise, deflate1), int64(len(noise)), true)
		e.wantContent("c", "noise", "", noise)
		e.wantCompressed("no compression", e.putWith(cb, "c", "plain", data, storage.Compression{}), int64(len(data)), false)
		e.wantContent("c", "plain", "", data)
		e.wantCompressed("NewObjectWriter", e.put("c", "plain2", data), int64(len(data)), false)
	})

	e.runFresh("EmptyObject", func(e *env) {
		cb := e.compressing()
		e.mkBucket("c")
		info := e.putWith(cb, "c", "empty", nil, deflate1)
		e.wantCompressed("empty", info, 0, false)
		e.wantContent("c", "empty", "", []byte{})
	})

	e.runFresh("Usage", func(e *env) {
		cb := e.compressing()
		e.mkVersioned("c", storage.VersioningEnabled)
		data := textBytes(400_000)
		old := e.putWith(cb, "c", "k", data, deflate1)
		e.put("c", "k", []byte("plain"))
		e.put("c", "other", []byte("12345"))
		e.wantContent("c", "k", old.VersionID, data)
		u, err := storage.ComputeBucketUsage(e.ctx, e.b, "c")
		e.noErr("ComputeBucketUsage", err)
		want := storage.BucketUsage{
			Objects: 2, Bytes: 10, StoredBytes: 10,
			Versions: 3, VersionBytes: int64(len(data)) + 10, StoredVersionBytes: old.StoredSize + 10,
			CompressedVersions: 1,
		}
		if u != want {
			e.t.Errorf("ComputeBucketUsage = %+v, want %+v", u, want)
		}
	})

	e.runFresh("UpdateMetaKeepsCompression", func(e *env) {
		cb := e.compressing()
		e.mkBucket("c")
		data := textBytes(300_000)
		e.putWith(cb, "c", "k", data, deflate1)
		info, err := e.b.UpdateObjectMeta(e.ctx, "c", "k", "", func(m *storage.ObjectMeta) error {
			m.Tags = map[string]string{"a": "b"}
			return nil
		})
		e.noErr("UpdateObjectMeta", err)
		e.wantCompressed("UpdateObjectMeta", info, int64(len(data)), true)
		got := e.wantContent("c", "k", "", data)
		e.wantCompressed("after update", got, int64(len(data)), true)
		if !maps.Equal(got.Tags, map[string]string{"a": "b"}) {
			e.t.Errorf("tags = %v", got.Tags)
		}
	})

	e.runFresh("Multipart", func(e *env) {
		cb := e.compressing()
		e.mkBucket("c")
		up := e.createUpload("c", "mp", storage.ObjectMeta{ContentType: "text/plain"})
		p1, p2 := textBytes(300_000), randBytes(1000, 3)
		e.part("c", "mp", up.UploadID, 1, p1)
		e.part("c", "mp", up.UploadID, 2, p2)
		want := append(bytes.Clone(p1), p2...)
		info, err := cb.CompleteMultipartUploadWith(e.ctx, "c", "mp", up.UploadID, []int{1, 2},
			storage.ObjectMeta{ETag: "x-2"}, storage.CommitOptions{}, storage.WriteOptions{Compression: deflate1})
		e.noErr("CompleteMultipartUploadWith", err)
		e.wantCompressed("CompleteMultipartUploadWith", info, int64(len(want)), true)
		e.wantContent("c", "mp", "", want)
		_, err = e.b.GetMultipartUpload(e.ctx, "c", "mp", up.UploadID)
		e.wantErr("GetMultipartUpload after completion", err, storage.ErrUploadNotFound)

		// Without compression it is CompleteMultipartUpload.
		up = e.createUpload("c", "mp2", storage.ObjectMeta{})
		e.part("c", "mp2", up.UploadID, 1, p1)
		info, err = cb.CompleteMultipartUploadWith(e.ctx, "c", "mp2", up.UploadID, []int{1},
			storage.ObjectMeta{ETag: "y-1"}, storage.CommitOptions{}, storage.WriteOptions{})
		e.noErr("CompleteMultipartUploadWith without compression", err)
		e.wantCompressed("uncompressed completion", info, int64(len(p1)), false)
		e.wantContent("c", "mp2", "", p1)
	})

	e.runFresh("InvalidOptions", func(e *env) {
		cb := e.compressing()
		e.mkBucket("c")
		up := e.createUpload("c", "mp", storage.ObjectMeta{})
		e.part("c", "mp", up.UploadID, 1, []byte("data"))
		for _, c := range []storage.Compression{
			{Algorithm: storage.CompressionDeflate, Level: 0},
			{Algorithm: storage.CompressionDeflate, Level: 10},
			{Algorithm: "zstd", Level: 3},
		} {
			if w, err := cb.NewObjectWriterWith(e.ctx, "c", "k", storage.WriteOptions{Compression: c}); err == nil {
				_ = w.Abort()
				e.t.Errorf("NewObjectWriterWith(%+v) succeeded", c)
			}
			if _, err := cb.CompleteMultipartUploadWith(e.ctx, "c", "mp", up.UploadID, []int{1},
				storage.ObjectMeta{}, storage.CommitOptions{}, storage.WriteOptions{Compression: c}); err == nil {
				e.t.Errorf("CompleteMultipartUploadWith(%+v) succeeded", c)
			}
		}
		_, err := cb.NewObjectWriterWith(e.ctx, "nobucket", "k", storage.WriteOptions{Compression: deflate1})
		e.wantErr("NewObjectWriterWith in a missing bucket", err, storage.ErrBucketNotFound)
		// Nothing was written and the upload survived the refused completions.
		_, err = e.b.GetObjectInfo(e.ctx, "c", "k", "")
		e.wantErr("GetObjectInfo after refused writers", err, storage.ErrObjectNotFound)
		_, err = e.b.GetObjectInfo(e.ctx, "c", "mp", "")
		e.wantErr("GetObjectInfo after refused completions", err, storage.ErrObjectNotFound)
		_, err = e.b.GetMultipartUpload(e.ctx, "c", "mp", up.UploadID)
		e.noErr("GetMultipartUpload after refused completions", err)
	})

	e.runFresh("Abort", func(e *env) {
		cb := e.compressing()
		e.mkBucket("c")
		w, err := cb.NewObjectWriterWith(e.ctx, "c", "k", storage.WriteOptions{Compression: deflate1})
		e.noErr("NewObjectWriterWith", err)
		_, _ = w.Write(textBytes(600_000))
		e.noErr("Abort", w.Abort())
		_, err = e.b.GetObjectInfo(e.ctx, "c", "k", "")
		e.wantErr("GetObjectInfo after Abort", err, storage.ErrObjectNotFound)
	})
}
