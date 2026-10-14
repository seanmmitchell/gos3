package fs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestEncryptionProvidersFollowResticInstallation(t *testing.T) {
	b := openTest(t, "")
	old := resticLookPath
	t.Cleanup(func() { resticLookPath = old })
	resticLookPath = func(string) (string, error) { return "", errors.New("not found") }
	if got, err := b.EncryptionProviders(ctx, "any"); err != nil || !slices.Equal(got, []string{storage.EncryptionAES256}) {
		t.Fatalf("without restic: %v %v", got, err)
	}
	resticLookPath = func(string) (string, error) { return "/usr/bin/restic", nil }
	if got, err := b.EncryptionProviders(ctx, "any"); err != nil || !slices.Equal(got, []string{storage.EncryptionAES256, storage.EncryptionRestic}) {
		t.Fatalf("with restic: %v %v", got, err)
	}
}

// An upload created by a build that only recorded SSE (Encryption "") is
// encrypted at completion, so its new parts must be staged encrypted.
func TestLegacySSEUploadStagesPartsEncrypted(t *testing.T) {
	b := openTest(t, "")
	mkBucket(t, b, "b", storage.VersioningEnabled)
	legacy := storage.ObjectMeta{SSE: "AES256"}
	u, err := b.CreateMultipartUpload(ctx, "b", "k", legacy, "")
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("legacy multipart secret"), 10000)
	pw, err := b.NewPartWriter(ctx, "b", "k", u.UploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err = pw.Commit(ctx, storage.PartMeta{ETag: "one"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range listFiles(t, b.uploadDir) {
		raw, _ := os.ReadFile(filepath.Join(b.uploadDir, path))
		if bytes.Contains(raw, data[:32]) {
			t.Fatal("legacy SSE upload staged a plaintext part")
		}
	}
	meta := legacy
	meta.Encryption = storage.EncryptionAES256
	info, err := b.CompleteMultipartUploadWith(ctx, "b", "k", u.UploadID, []int{1}, meta, storage.CommitOptions{},
		storage.WriteOptions{Encryption: storage.EncryptionAES256})
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
		t.Fatalf("read back: %v", err)
	}
}
