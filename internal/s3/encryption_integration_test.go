package s3

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	storagefs "github.com/seanmmitchell/gos3/internal/storage/fs"
)

func TestResticRejectsRecursiveRepositoryWrites(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("archive")
	ctx := context.Background()
	_ = e.backend.PutBucketConfig(ctx, "archive", storage.ConfigEncryptionProvider, []byte(`{"provider":"restic"}`))
	_ = e.backend.PutBucketConfig(ctx, "archive", storage.ConfigEncryption, []byte(`<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`))
	objExpect(t, e.signed("PUT", "/archive/config", []byte("repository"), http.Header{"User-Agent": {storage.ResticUserAgent}}), 400, "InvalidRequest")
}

func TestEncryptionS3RoundTrip(t *testing.T) {
	dir := t.TempDir()
	b, err := storagefs.Open(dir, storagefs.Options{FsyncData: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	e := newTestEnv(t, withOptions(func(o *Options) { o.Backend = b }))
	if err := b.CreateBucket(context.Background(), "encrypted", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("private-content-must-be-encrypted"), 5000)
	objExpect(t, e.signed("PUT", "/encrypted/k", data, objHdr("X-Amz-Server-Side-Encryption", "AES256")), 200, "")
	r := e.signed("GET", "/encrypted/k", nil, nil)
	if r.StatusCode != 200 || !bytes.Equal(r.Data, data) || r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Fatalf("GET: %+v", r)
	}
	if err := filepath.WalkDir(filepath.Join(dir, ".gos3", "data"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			if bytes.Contains(raw, data[:32]) {
				t.Error("plaintext data file")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	objExpect(t, e.signed("PUT", "/encrypted/kms", data, objHdr("X-Amz-Server-Side-Encryption", "aws:kms")), 501, "NotImplemented")
	// A legacy object is readable but must not advertise nonexistent encryption.
	w, _ := b.NewObjectWriter(context.Background(), "encrypted", "legacy")
	_, _ = w.Write([]byte("old"))
	_, err = w.Commit(context.Background(), storage.ObjectMeta{SSE: "AES256"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r = e.signed("HEAD", "/encrypted/legacy", nil, nil)
	if r.Header.Get("X-Amz-Server-Side-Encryption") != "" {
		t.Fatal("legacy object falsely reported as encrypted")
	}
}
