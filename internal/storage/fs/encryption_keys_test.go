package fs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
)

// putEncrypted stores data as an AES256-encrypted object, writing it in
// pieces of at most step bytes.
func putEncrypted(t *testing.T, b *Backend, bucket, key string, data []byte, step int, comp storage.Compression) storage.ObjectInfo {
	t.Helper()
	w, err := b.NewObjectWriterWith(ctx, bucket, key, storage.WriteOptions{Encryption: storage.EncryptionAES256, Compression: comp})
	if err != nil {
		t.Fatal(err)
	}
	for p := data; len(p) > 0; {
		n := min(step, len(p))
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	info, err := w.Commit(ctx, storage.ObjectMeta{SSE: "AES256"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// readObject returns the content of a version.
func readObject(t *testing.T, b *Backend, bucket, key, vid string) []byte {
	t.Helper()
	_, rd, err := b.OpenObject(ctx, bucket, key, vid)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestEncryptionSizes covers empty objects and sizes around the stream's
// chunk boundaries, checking that the data file size matches PhysicalSize.
func TestEncryptionSizes(t *testing.T) {
	const c = crypt.ChunkSize
	b := openTest(t, "")
	mkBucket(t, b, "b", storage.VersioningEnabled)
	for _, n := range []int{0, 1, c - 1, c, c + 1, 3 * c, 3*c + 5} {
		for _, step := range []int{1000, writeBufSize, 1 << 20} {
			data := bytes.Repeat([]byte{byte(n), 1, 2, 3, 4, 5, 6}, n/7+1)[:n]
			info := putEncrypted(t, b, "b", "k", data, step, storage.Compression{})
			if got := readObject(t, b, "b", "k", info.VersionID); !bytes.Equal(got, data) {
				t.Fatalf("size %d step %d: content mismatch", n, step)
			}
			_, bid, did, err := b.lookupObject(ctx, "b", "k", info.VersionID)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				if did != "" || info.PhysicalSize() != 0 {
					t.Fatalf("empty object has data file %q, physical size %d", did, info.PhysicalSize())
				}
				continue
			}
			st, err := os.Stat(b.dataPath(bid, did))
			if err != nil || st.Size() != info.PhysicalSize() {
				t.Fatalf("size %d: data file %v, physical size %d", n, err, info.PhysicalSize())
			}
		}
	}
}

// TestEncryptionMultipartParts completes encrypted uploads whose parts
// cross chunk boundaries (one is empty), with and without compression.
func TestEncryptionMultipartParts(t *testing.T) {
	const c = crypt.ChunkSize
	for _, comp := range []storage.Compression{{}, deflate.Compression} {
		b := openTest(t, "")
		mkBucket(t, b, "b", storage.VersioningEnabled)
		meta := storage.ObjectMeta{SSE: "AES256", Encryption: storage.EncryptionAES256}
		u, err := b.CreateMultipartUpload(ctx, "b", "k", meta, "")
		if err != nil {
			t.Fatal(err)
		}
		var want []byte
		for i, n := range []int{c + 1, 0, 5, 3 * c} {
			part := bytes.Repeat([]byte{byte(i + 1)}, n)
			want = append(want, part...)
			pw, err := b.NewPartWriter(ctx, "b", "k", u.UploadID, i+1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pw.Write(part); err != nil {
				t.Fatal(err)
			}
			if _, err = pw.Commit(ctx, storage.PartMeta{ETag: "e"}); err != nil {
				t.Fatal(err)
			}
		}
		info, err := b.CompleteMultipartUploadWith(ctx, "b", "k", u.UploadID, []int{1, 2, 3, 4}, meta,
			storage.CommitOptions{}, storage.WriteOptions{Encryption: storage.EncryptionAES256, Compression: comp})
		if err != nil {
			t.Fatal(err)
		}
		if got := readObject(t, b, "b", "k", info.VersionID); !bytes.Equal(got, want) {
			t.Fatalf("compression %q: content mismatch", comp.Algorithm)
		}
	}
}

// TestEncryptionMultipartTamperedPart checks that a damaged part file fails
// the completion instead of publishing unauthenticated data.
func TestEncryptionMultipartTamperedPart(t *testing.T) {
	b := openTest(t, "")
	mkBucket(t, b, "b", storage.VersioningEnabled)
	meta := storage.ObjectMeta{SSE: "AES256", Encryption: storage.EncryptionAES256}
	u, err := b.CreateMultipartUpload(ctx, "b", "k", meta, "")
	if err != nil {
		t.Fatal(err)
	}
	pw, err := b.NewPartWriter(ctx, "b", "k", u.UploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pw.Write(bytes.Repeat([]byte("part"), 1000))
	if _, err = pw.Commit(ctx, storage.PartMeta{ETag: "e"}); err != nil {
		t.Fatal(err)
	}
	files := listFiles(t, b.uploadDir)
	if len(files) != 1 {
		t.Fatalf("part files: %v", files)
	}
	path := filepath.Join(b.uploadDir, files[0])
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[10] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = b.CompleteMultipartUploadWith(ctx, "b", "k", u.UploadID, []int{1}, meta,
		storage.CommitOptions{}, storage.WriteOptions{Encryption: storage.EncryptionAES256})
	if !errors.Is(err, crypt.ErrCorrupt) {
		t.Fatalf("tampered part: %v", err)
	}
	if _, err := b.GetObjectInfo(ctx, "b", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("published an object from a tampered part: %v", err)
	}
}

// TestBucketKeyCachedLookupDoesNotBlock checks that a bucket whose key is
// being loaded (file and bbolt I/O) does not block cached key lookups of
// other buckets.
func TestBucketKeyCachedLookupDoesNotBlock(t *testing.T) {
	b := openTest(t, "")
	ids := map[string]string{}
	for _, name := range []string{"slow", "fast"} {
		mkBucket(t, b, name, storage.VersioningUnversioned)
		if err := b.PrepareEncryption(ctx, name, storage.EncryptionAES256); err != nil {
			t.Fatal(err)
		}
		ids[name], _ = b.encryptionBucketID(ctx, name)
	}
	// Simulate a key load in progress for "slow".
	e := &bucketKeyEntry{}
	b.keyMu.Lock()
	b.keys[ids["slow"]] = e
	b.keyMu.Unlock()
	e.load.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := b.bucketKey(ids["fast"], true)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		e.load.Unlock()
		t.Fatal("cached lookup blocked by another bucket's key load")
	}
	go func() {
		_, err := b.bucketKey(ids["slow"], false)
		done <- err
	}()
	e.load.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestBucketKeyMissingIsNotCached checks that a missing key fails reads and
// is never regenerated, and that restoring the key file is picked up
// without a restart.
func TestBucketKeyMissingIsNotCached(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir)
	mkBucket(t, b, "b", storage.VersioningUnversioned)
	data := bytes.Repeat([]byte("secret"), 1000)
	putEncrypted(t, b, "b", "k", data, 1<<20, storage.Compression{})
	id, _ := b.encryptionBucketID(ctx, "b")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, ".gos3", "keys", id)
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, dir)
	for range 2 {
		if _, _, err := b.OpenObject(ctx, "b", "k", ""); err == nil {
			t.Fatal("read without key")
		}
		if _, err := b.NewObjectWriterWith(ctx, "b", "k2", storage.WriteOptions{Encryption: storage.EncryptionAES256}); err == nil {
			t.Fatal("missing key was regenerated")
		}
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key file was recreated: %v", err)
	}
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readObject(t, b, "b", "k", ""); !bytes.Equal(got, data) {
		t.Fatal("content mismatch after restoring the key")
	}
}

// TestOpenTightensKeyPermissions checks that key and recovery files
// restored with loose modes are made private again at Open.
func TestOpenTightensKeyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits on Windows (see config.RestrictDir)")
	}
	dir := t.TempDir()
	b := openTest(t, dir)
	mkBucket(t, b, "b", storage.VersioningUnversioned)
	if err := b.PrepareEncryption(ctx, "b", storage.EncryptionAES256); err != nil {
		t.Fatal(err)
	}
	id, _ := b.encryptionBucketID(ctx, "b")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, ".gos3", "keys")
	recovery := filepath.Join(keys, id+".restic.json")
	if err := os.WriteFile(recovery, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	loose := map[string]os.FileMode{keys: 0o755, filepath.Join(keys, id): 0o644, recovery: 0o644}
	for p, m := range loose {
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	openTest(t, dir)
	for p, was := range loose {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != was&^0o077 {
			t.Errorf("%s: mode %v, want %v", p, got, was&^0o077)
		}
	}
}
