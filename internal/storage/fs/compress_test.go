package fs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
)

var deflate = storage.WriteOptions{Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1}}

func putCompressed(t *testing.T, b *Backend, bucket, key string, data []byte) storage.ObjectInfo {
	t.Helper()
	w, err := b.NewObjectWriterWith(ctx, bucket, key, deflate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	info, err := w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestCompressedDataFile checks that a compressed version's data file holds
// StoredSize bytes, survives a restart and its recovery pass, and that
// damage to it is reported as an error rather than returned as content.
func TestCompressedDataFile(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir)
	mkBucket(t, b, "bkt", storage.VersioningEnabled)
	data := []byte(strings.Repeat("compressible content; ", 40_000))
	info := putCompressed(t, b, "bkt", "k", data)
	plain := put(t, b, "bkt", "plain", "plain data")
	if info.Compression != storage.CompressionDeflate || info.StoredSize >= info.Size/4 || info.Size != int64(len(data)) {
		t.Fatalf("info = %+v", info)
	}
	files := dataFiles(t, b)
	if len(files) != 2 {
		t.Fatalf("data files = %v", files)
	}
	var compressedFile string
	for _, f := range files {
		st, err := os.Stat(filepath.Join(b.dataDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() == info.StoredSize {
			compressedFile = filepath.Join(b.dataDir, f)
		}
	}
	if compressedFile == "" {
		t.Fatalf("no data file of %d bytes among %v", info.StoredSize, files)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the record and the file are recognised (nothing is
	// quarantined) and the content reads back.
	b = openTest(t, dir)
	waitRecovery(t, b)
	if got := dataFiles(t, b); len(got) != 2 {
		t.Fatalf("data files after recovery = %v", got)
	}
	got, err := b.GetObjectInfo(ctx, "bkt", "k", info.VersionID)
	if err != nil || got.Compression != info.Compression || got.StoredSize != info.StoredSize || got.Size != info.Size {
		t.Fatalf("GetObjectInfo after reopen = %+v, %v", got, err)
	}
	if s := read(t, b, "bkt", "k", ""); s != string(data) {
		t.Fatal("content differs after reopen")
	}
	if s := read(t, b, "bkt", "plain", plain.VersionID); s != "plain data" {
		t.Fatalf("plain = %q", s)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Damage inside a block is detected when it is read.
	raw, err := os.ReadFile(compressedFile)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/3] ^= 0xff
	if err := os.WriteFile(compressedFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, dir)
	_, r, err := b.OpenObject(ctx, "bkt", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.Copy(io.Discard, r); !errors.Is(err, compress.ErrCorrupt) {
		t.Fatalf("reading damaged data: %v", err)
	}

	// A truncated file fails the size check at open.
	if err := os.WriteFile(compressedFile, raw[:len(raw)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.OpenObject(ctx, "bkt", "k", ""); !errors.Is(err, errCorrupt) {
		t.Fatalf("OpenObject of a truncated file: %v", err)
	}
}

// TestCompressedMultipartLeavesNoFiles checks that a compressed
// completion removes the upload's parts and leaves one data file.
func TestCompressedMultipartLeavesNoFiles(t *testing.T) {
	b, bkt := newBucket(t, "")
	up, err := b.CreateMultipartUpload(ctx, bkt, "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p := strings.Repeat("part data ", 50_000)
	uploadPart(t, b, bkt, "mp", up.UploadID, 1, p)
	uploadPart(t, b, bkt, "mp", up.UploadID, 2, p)
	info, err := b.CompleteMultipartUploadWith(ctx, bkt, "mp", up.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "x-2"}, storage.CommitOptions{}, deflate)
	if err != nil {
		t.Fatal(err)
	}
	if info.Compression == "" || info.Size != int64(2*len(p)) {
		t.Fatalf("info = %+v", info)
	}
	if s := read(t, b, bkt, "mp", ""); s != p+p {
		t.Fatal("content differs")
	}
	if got := dataFiles(t, b); len(got) != 1 {
		t.Fatalf("data files = %v", got)
	}
	if got := tmpFiles(t, b); len(got) != 0 {
		t.Fatalf("tmp files = %v", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(b.root, ".gos3", "uploads")); len(entries) != 0 {
		t.Fatalf("upload directories left: %d", len(entries))
	}
	// A failed precondition keeps the upload and leaves no data file.
	up, _ = b.CreateMultipartUpload(ctx, bkt, "mp", storage.ObjectMeta{}, "")
	uploadPart(t, b, bkt, "mp", up.UploadID, 1, p)
	_, err = b.CompleteMultipartUploadWith(ctx, bkt, "mp", up.UploadID, []int{1}, storage.ObjectMeta{},
		storage.CommitOptions{IfNoneMatch: true}, deflate)
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("conditional completion: %v", err)
	}
	if got := dataFiles(t, b); len(got) != 1 || !bytes.Equal([]byte(read(t, b, bkt, "mp", "")), []byte(p+p)) {
		t.Fatalf("data files after a failed completion = %v", got)
	}
}
