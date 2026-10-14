package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
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

func TestResticRemoteS3Integration(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("install restic >= 0.17.1 to run integration")
	}
	remote := newTestEnv(t)
	remote.createBucket("repository")
	ctx := context.Background()
	dir := t.TempDir()
	b, err := storagefs.Open(dir, storagefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.CreateBucket(ctx, "archive", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg := storage.ResticS3{Endpoint: remote.srv.URL, Bucket: "repository", Prefix: "archive", AccessKey: testRoot.AccessKey, SecretKey: testRoot.SecretKey, Region: "us-east-1"}
	if err = b.ConfigureResticBacking(ctx, &cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(storage.EncryptionSettings{Provider: storage.EncryptionRestic})
	if err = b.PutBucketConfig(ctx, "archive", storage.ConfigEncryptionProvider, raw); err != nil {
		t.Fatal(err)
	}
	source := newTestEnv(t, withOptions(func(o *Options) { o.Backend = b }))
	objExpect(t, source.signed("PUT", "/archive?encryption", []byte(`<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`), nil), 200, "")
	data := bytes.Repeat([]byte("remotely archived confidential data"), 1000)
	objExpect(t, source.signed("PUT", "/archive/key", data, nil), 200, "")
	info, err := b.GetObjectInfo(ctx, "archive", "key", "")
	if err != nil {
		t.Fatal(err)
	}
	if info.Encryption != storage.EncryptionRestic {
		t.Fatal("S3 default did not select restic")
	}
	r := source.signed("GET", "/archive/key", nil, nil)
	if r.StatusCode != 200 || !bytes.Equal(r.Data, data) || r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Fatalf("remote S3 restore failed: %d", r.StatusCode)
	}
	r = source.signed("GET", "/archive/key", nil, objHdr("Range", "bytes=12-51"))
	if r.StatusCode != 206 || !bytes.Equal(r.Data, data[12:52]) {
		t.Fatalf("remote S3 range failed: %d", r.StatusCode)
	}
	// The remote S3 bucket sees encrypted repository objects, not source bytes.
	err = remote.backend.WalkObjects(ctx, "repository", "", "", func(i storage.ObjectInfo) error {
		_, r, e := remote.backend.OpenObject(ctx, "repository", i.Key, "")
		if e != nil {
			return e
		}
		defer r.Close()
		raw, e := io.ReadAll(r)
		if bytes.Contains(raw, data[:32]) {
			t.Error("plaintext reached remote S3")
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	// Invalid credentials fail the new write; the previous snapshot survives.
	cfg.SecretKey = "invalid-secret"
	if err = b.ConfigureResticBacking(ctx, &cfg); err == nil {
		t.Fatal("wrong remote credentials accepted")
	}
	saved, locked, err := b.ResticBackingSettings(ctx)
	if err != nil || !locked || saved.SecretKey != testRoot.SecretKey {
		t.Fatal("failed update replaced working backing settings")
	}
	r = source.signed("GET", "/archive/key", nil, nil)
	if r.StatusCode != 200 || !bytes.Equal(r.Data, data) {
		t.Fatal("failed update lost archive access")
	}

}
