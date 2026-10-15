package memory

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

var aes256 = storage.WriteOptions{Encryption: storage.EncryptionAES256}

// putAES256 stores data with storage.EncryptionAES256.
func putAES256(t *testing.T, b *Backend, key string, data []byte, opts storage.CommitOptions) storage.ObjectInfo {
	t.Helper()
	w, err := b.NewObjectWriterWith(ctx, "bkt", key, aes256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	info, err := w.Commit(ctx, storage.ObjectMeta{SSE: "AES256"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestEncryptionRecordedNotTransformed checks that AES256 versions record
// their encryption, keep their bytes as they are and read back, including
// empty objects, multipart completions and imported versions.
func TestEncryptionRecordedNotTransformed(t *testing.T) {
	b, _ := newBucket(t, storage.VersioningEnabled)
	if err := b.PrepareEncryption(ctx, "bkt", storage.EncryptionAES256); err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareEncryption(ctx, "bkt", storage.EncryptionRestic); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("restic: %v", err)
	}
	for _, data := range []string{"", "x", "some content"} {
		info := putAES256(t, b, "k", []byte(data), storage.CommitOptions{})
		if info.Encryption != storage.EncryptionAES256 || info.Size != int64(len(data)) {
			t.Fatalf("info %+v", info)
		}
		if got := read(t, b, "bkt", "k", info.VersionID); got != data {
			t.Fatalf("read %q, want %q", got, data)
		}
		v, _, err := b.buckets["bkt"].ix.Find("k", "")
		if err != nil {
			t.Fatal(err)
		}
		if stored := v.Data; string(stored) != data {
			t.Fatalf("stored %q, want the content as is", stored)
		}
	}

	// Empty multipart completion.
	u, err := b.CreateMultipartUpload(ctx, "bkt", "mp", storage.ObjectMeta{SSE: "AES256", Encryption: storage.EncryptionAES256}, "")
	if err != nil {
		t.Fatal(err)
	}
	pw, err := b.NewPartWriter(ctx, "bkt", "mp", u.UploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Commit(ctx, storage.PartMeta{ETag: "e"}); err != nil {
		t.Fatal(err)
	}
	info, err := b.CompleteMultipartUploadWith(ctx, "bkt", "mp", u.UploadID, []int{1}, storage.ObjectMeta{SSE: "AES256"},
		storage.CommitOptions{}, aes256)
	if err != nil {
		t.Fatal(err)
	}
	if info.Encryption != storage.EncryptionAES256 || read(t, b, "bkt", "mp", "") != "" {
		t.Fatalf("multipart %+v", info)
	}

	// Empty imported version.
	vw, err := b.NewVersionWriterWith(ctx, "bkt", "imp", aes256)
	if err != nil {
		t.Fatal(err)
	}
	info, err = vw.Commit(ctx, storage.ObjectInfo{VersionID: "AAAAAAAAAAAAAAAAAAAAAA", LastModified: time.Now(),
		ObjectMeta: storage.ObjectMeta{SSE: "AES256"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Encryption != storage.EncryptionAES256 || read(t, b, "bkt", "imp", info.VersionID) != "" {
		t.Fatalf("import %+v", info)
	}
}

// TestEncryptionBeforePublishCannotRelabel checks that a BeforePublish hook
// changing ObjectMeta.Encryption does not change the recorded encoding.
func TestEncryptionBeforePublishCannotRelabel(t *testing.T) {
	b, _ := newBucket(t, storage.VersioningUnversioned)
	info := putAES256(t, b, "k", []byte("data"), storage.CommitOptions{
		BeforePublish: func(meta, _ *storage.ObjectMeta, _ time.Time, _ func(string) []byte) error {
			meta.Encryption = ""
			meta.ContentType = "text/x-hook"
			return nil
		},
	})
	if info.Encryption != storage.EncryptionAES256 || info.ContentType != "text/x-hook" {
		t.Fatalf("info %+v", info)
	}
}

// TestEncryptionSmallReadAllocations checks that opening an AES256 object
// and reading one byte does not copy the object.
func TestEncryptionSmallReadAllocations(t *testing.T) {
	b, _ := newBucket(t, storage.VersioningUnversioned)
	putAES256(t, b, "k", bytes.Repeat([]byte("x"), 16<<20), storage.CommitOptions{})
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, rd, err := b.OpenObject(ctx, "bkt", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rd, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	_ = rd.Close()
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
		t.Fatalf("a 1-byte read allocated %d bytes", n)
	}
}
