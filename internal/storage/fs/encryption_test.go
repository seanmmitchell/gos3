package fs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestEncryptionRoundTrip(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "compressed"}[compressed], func(t *testing.T) {
			dir := t.TempDir()
			b := openDurable(t, dir)
			mkBucket(t, b, "encrypted", storage.VersioningEnabled)
			data := bytes.Repeat([]byte("highly confidential object content."), 10000)
			opts := storage.WriteOptions{Encryption: storage.EncryptionAES256}
			if compressed {
				opts.Compression = deflate.Compression
			}
			w, err := b.NewObjectWriterWith(ctx, "encrypted", "secret", opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Write(data); err != nil {
				t.Fatal(err)
			}
			// Large upload staging must already be ciphertext.
			for _, path := range tmpFiles(t, b) {
				raw, _ := os.ReadFile(filepath.Join(b.tmpDir, path))
				if bytes.Contains(raw, data[:32]) {
					t.Fatal("plaintext staging")
				}
			}
			info, err := w.Commit(ctx, storage.ObjectMeta{SSE: "AES256", ETag: "etag"}, storage.CommitOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if info.Size != int64(len(data)) || info.Encryption != storage.EncryptionAES256 {
				t.Fatalf("info: %+v", info)
			}
			_, bid, did, err := b.lookupObject(ctx, "encrypted", "secret", info.VersionID)
			if err != nil {
				t.Fatal(err)
			}
			path := b.dataPath(bid, did)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, data[:32]) || int64(len(raw)) != info.PhysicalSize() {
				t.Fatal("invalid ciphertext storage")
			}
			if err = b.Close(); err != nil {
				t.Fatal(err)
			}
			b = openTest(t, dir)
			_, rd, err := b.OpenObject(ctx, "encrypted", "secret", info.VersionID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = rd.Seek(65530, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			part := make([]byte, 111)
			if _, err = io.ReadFull(rd, part); err != nil || !bytes.Equal(part, data[65530:65641]) {
				t.Fatalf("range: %v", err)
			}
			_, _ = rd.Seek(0, io.SeekStart)
			got, err := io.ReadAll(rd)
			_ = rd.Close()
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("roundtrip: %v", err)
			}
			// Corrupted ciphertext must never be returned as plaintext.
			raw[len(raw)/2] ^= 1
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			_, rd, err = b.OpenObject(ctx, "encrypted", "secret", info.VersionID)
			if err == nil {
				_, err = io.ReadAll(rd)
				_ = rd.Close()
			}
			if err == nil {
				t.Fatal("tampered object was accepted")
			}
		})
	}
}

func TestEncryptionMissingKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir)
	mkBucket(t, b, "b", storage.VersioningUnversioned)
	if err := b.PrepareEncryption(ctx, "b", storage.EncryptionAES256); err != nil {
		t.Fatal(err)
	}
	id, _ := b.encryptionBucketID(ctx, "b")
	_ = b.Close()
	if err := os.Remove(filepath.Join(dir, ".gos3", "keys", id)); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, dir)
	if err := b.PrepareEncryption(ctx, "b", storage.EncryptionAES256); err == nil {
		t.Fatal("missing key was silently regenerated")
	}
}

func TestEncryptionRejectsSwappedObjects(t *testing.T) {
	b := openTest(t, "")
	mkBucket(t, b, "b", storage.VersioningUnversioned)
	var paths []string
	for _, name := range []string{"first", "other"} {
		w, err := b.NewObjectWriterWith(ctx, "b", name, storage.WriteOptions{Encryption: storage.EncryptionAES256})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(name))
		_, err = w.Commit(ctx, storage.ObjectMeta{SSE: "AES256"}, storage.CommitOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, bid, did, _ := b.lookupObject(ctx, "b", name, "")
		paths = append(paths, b.dataPath(bid, did))
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths[1], raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, rd, err := b.OpenObject(ctx, "b", "other", "")
	if err == nil {
		_, err = io.ReadAll(rd)
		_ = rd.Close()
	}
	if err == nil {
		t.Fatal("accepted ciphertext belonging to a different object")
	}
}

func TestEncryptionMultipart(t *testing.T) {
	b := openTest(t, "")
	mkBucket(t, b, "b", storage.VersioningEnabled)
	meta := storage.ObjectMeta{SSE: "AES256", Encryption: storage.EncryptionAES256}
	u, err := b.CreateMultipartUpload(ctx, "b", "k", meta, "")
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("secret multipart data"), 10000)
	pw, err := b.NewPartWriter(ctx, "b", "k", u.UploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pw.Write(data)
	if _, err = pw.Commit(ctx, storage.PartMeta{ETag: "one"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range listFiles(t, b.uploadDir) {
		raw, _ := os.ReadFile(filepath.Join(b.uploadDir, path))
		if bytes.Contains(raw, data[:32]) {
			t.Fatal("plaintext part")
		}
	}
	info, err := b.CompleteMultipartUploadWith(ctx, "b", "k", u.UploadID, []int{1}, meta, storage.CommitOptions{}, storage.WriteOptions{Encryption: storage.EncryptionAES256})
	if err != nil {
		t.Fatal(err)
	}
	_, rd, err := b.OpenObject(ctx, "b", "k", info.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rd)
	_ = rd.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("multipart: %v", err)
	}
}

func TestResticLocalIntegration(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("install restic >= 0.17.1 to run integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	b := openTest(t, dir)
	mkBucket(t, b, "archive", storage.VersioningEnabled)
	if err := b.PrepareEncryption(ctx, "archive", storage.EncryptionRestic); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("restic archived object"), 10000)
	w, err := b.NewObjectWriterWith(ctx, "archive", "k", storage.WriteOptions{Encryption: storage.EncryptionRestic})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	info, err := w.Commit(ctx, storage.ObjectMeta{SSE: "AES256"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	b = openTest(t, dir)
	_, rd, err := b.OpenObject(ctx, "archive", "k", info.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rd.Seek(123, io.SeekStart)
	got, err := io.ReadAll(rd)
	_ = rd.Close()
	if err != nil || !bytes.Equal(got, data[123:]) {
		t.Fatalf("restic roundtrip: %v", err)
	}
	if files := tmpFiles(t, b); len(files) != 0 {
		t.Fatalf("temporary files leaked: %v", files)
	}
	// Snapshots continue to exist for archival after the S3 object is removed.
	_, bid, did, _ := b.lookupObject(ctx, "archive", "k", info.VersionID)
	ref, err := os.ReadFile(b.dataPath(bid, did))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.DeleteObject(ctx, "archive", "k", info.VersionID); err != nil {
		t.Fatal(err)
	}
	cmd, err := b.resticCommand(ctx, bid, "dump", string(ref), "/object")
	if err != nil {
		t.Fatal(err)
	}
	got, err = cmd.Output()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("archived snapshot: %v", err)
	}
	if err = b.DeleteBucket(ctx, "archive"); err != nil {
		t.Fatal(err)
	}
	// Recovery credentials and source identity survive deletion of the bucket.
	raw, err := os.ReadFile(filepath.Join(b.sysDir, "keys", bid+".restic.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg storage.EncryptionSettings
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cmd, err = resticCommandFor(ctx, cfg, "snapshots", "--json")
	if err != nil {
		t.Fatal(err)
	}
	got, err = cmd.Output()
	if err != nil || !bytes.Contains(got, []byte("gos3-key:"+base64.RawURLEncoding.EncodeToString([]byte("k")))) || !bytes.Contains(got, []byte("gos3-bucket:"+base64.RawURLEncoding.EncodeToString([]byte("archive")))) {
		t.Fatalf("archive source identity missing: %v", err)
	}
}
