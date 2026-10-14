package fs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestResticBackingLocationAndPersistence(t *testing.T) {
	b := openTest(t, "")
	ctx := context.Background()
	if err := b.ConfigureResticBacking(ctx, nil); err != nil {
		t.Fatal(err)
	}
	s, locked, err := b.ResticBackingSettings(ctx)
	if err != nil || s != nil || locked {
		t.Fatal("unexpected local backing settings")
	}
	remote := &storage.ResticS3{Endpoint: "https://s3.example.test", Bucket: "archives", Prefix: "one", AccessKey: "a", SecretKey: "s"}
	if got := b.resticRepository("bucket-id", remote); got != "s3:https://s3.example.test/archives/one/bucket-id" {
		t.Fatalf("repository: %s", got)
	}
	if err := os.MkdirAll(filepath.Join(b.sysDir, "keys"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.sysDir, "keys", "bucket-id.restic.json"), []byte(`{"initialized":true,"repository":"s3:https://s3.example.test/archives/one/bucket-id","password":"derived"}`), filePerm); err != nil {
		t.Fatal(err)
	}
	if err := b.checkResticLocation(remote); err != nil {
		t.Fatal(err)
	}
	remote.Prefix = "two"
	if err := b.checkResticLocation(remote); err == nil {
		t.Fatal("allowed archive location to change")
	}
	if err := b.checkResticLocation(nil); err == nil {
		t.Fatal("allowed remote archives to fall back to local storage")
	}
}

func TestResticBackingStartupConfiguration(t *testing.T) {
	dir := t.TempDir()
	cfg := &storage.ResticS3{Endpoint: "https://s3.example.test", Bucket: "archives", Prefix: "one", AccessKey: "a", SecretKey: "s"}
	b, err := Open(dir, Options{ResticS3: cfg})
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := b.ResticBackingSettings(context.Background())
	if err != nil || s.SecretKey != "s" {
		t.Fatal("startup credentials not passed to backing")
	}
	// A persisted explicit local setting overrides the startup setting.
	if err := b.ConfigureResticBacking(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	b, err = Open(dir, Options{ResticS3: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	s, _, err = b.ResticBackingSettings(context.Background())
	if err != nil || s != nil {
		t.Fatal("persisted local setting not restored")
	}
}
