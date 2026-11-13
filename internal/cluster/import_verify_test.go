package cluster

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func importFixture(t *testing.T) (storage.Backend, config.Settings) {
	t.Helper()
	ctx := context.Background()
	backend := memory.New()
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.CreateBucket(ctx, "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := backend.SetBucketVersioning(ctx, "photos", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := backend.PutBucketConfig(ctx, "photos", storage.ConfigTagging, []byte("<Tagging/>")); err != nil {
		t.Fatal(err)
	}
	writer, err := backend.NewObjectWriter(ctx, "photos", "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("verified bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Commit(ctx, storage.ObjectMeta{ETag: "etag-one"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.DeleteObject(ctx, "photos", "one", ""); err != nil {
		t.Fatal(err)
	}
	return backend, config.SettingsFrom(config.Default())
}

func TestVerifyStandaloneChecksVersionsIAMAndSettings(t *testing.T) {
	backend, settings := importFixture(t)
	ctx := context.Background()
	result, err := VerifyStandalone(ctx, backend, settings, nil, "ROOTTESTKEY", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Buckets) != 1 || len(result.Buckets[0].Versions) != 2 || string(result.Buckets[0].Configs[storage.ConfigTagging]) != "<Tagging/>" {
		t.Fatalf("verified inventory: %+v", result)
	}
	if _, err := VerifyStandalone(ctx, backend, settings, []byte(`{"version":0}`), "ROOTTESTKEY", 1024); err == nil || !strings.Contains(err.Error(), "IAM") {
		t.Fatalf("corrupt IAM accepted: %v", err)
	}
	if _, err := VerifyStandalone(ctx, backend, settings, nil, "ROOTTESTKEY", 3); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("oversized version accepted: %v", err)
	}
	if _, err := VerifyStandalone(ctx, backend, settings, bytes.Repeat([]byte("x"), 1<<20+1), "ROOTTESTKEY", 1024); err == nil || !strings.Contains(err.Error(), "IAM exceeds") {
		t.Fatalf("oversized IAM accepted: %v", err)
	}
	if err := backend.PutBucketConfig(ctx, "photos", storage.ConfigTagging, bytes.Repeat([]byte("x"), 1<<20+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyStandalone(ctx, backend, settings, nil, "ROOTTESTKEY", 1024); err == nil || !strings.Contains(err.Error(), "consensus record limit") {
		t.Fatalf("oversized bucket configuration accepted: %v", err)
	}
	if err := backend.PutBucketConfig(ctx, "photos", storage.ConfigTagging, []byte("<Tagging/>")); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CreateMultipartUpload(ctx, "photos", "pending", storage.ObjectMeta{}, "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyStandalone(ctx, backend, settings, nil, "ROOTTESTKEY", 1024); err == nil || !strings.Contains(err.Error(), "unfinished multipart") {
		t.Fatalf("unfinished upload accepted: %v", err)
	}
}

type shortImportReader struct{ *bytes.Reader }

func (*shortImportReader) Close() error { return nil }

type truncatedImportBackend struct{ storage.Backend }

func (b truncatedImportBackend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	info, err := b.Backend.GetObjectInfo(ctx, bucket, key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	return info, &shortImportReader{bytes.NewReader([]byte("short"))}, nil
}

func TestVerifyStandaloneRejectsTruncatedObject(t *testing.T) {
	backend, settings := importFixture(t)
	_, err := VerifyStandalone(context.Background(), truncatedImportBackend{backend}, settings, nil, "ROOTTESTKEY", 1024)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("truncated source object accepted: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation: %v", err)
	}
}

func TestExportStandaloneBundleWritesVerifiedPayloads(t *testing.T) {
	backend, settings := importFixture(t)
	destination := filepath.Join(t.TempDir(), "bundle")
	bundle, err := ExportStandaloneBundle(context.Background(), backend, settings, nil, "ROOTTESTKEY", "root-test-secret-key", destination, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Version != importBundleVersion || len(bundle.Buckets) != 1 || len(bundle.Buckets[0].Versions) != 2 {
		t.Fatalf("bundle metadata: %+v", bundle)
	}
	if _, err := os.Stat(filepath.Join(destination, importManifestName)); err != nil {
		t.Fatal(err)
	}
	if loaded, fingerprint, err := LoadImportBundle(context.Background(), destination, "ROOTTESTKEY", "root-test-secret-key", 1024); err != nil || fingerprint == "" || len(loaded.Buckets) != 1 {
		t.Fatalf("load verified import bundle: %+v, %q, %v", loaded, fingerprint, err)
	}
	if _, _, err := LoadImportBundle(context.Background(), destination, "ROOTTESTKEY", "wrong-secret", 1024); err == nil {
		t.Fatal("bundle accepted different root credentials")
	}
	var bodyPath string
	for _, version := range bundle.Buckets[0].Versions {
		if version.Info.DeleteMarker {
			continue
		}
		bodyPath = filepath.Join(destination, "payloads", version.OperationID)
		body, err := os.ReadFile(bodyPath)
		if err != nil || string(body) != "verified bytes" {
			t.Fatalf("exported payload = %q, %v", body, err)
		}
	}
	if _, err := ExportStandaloneBundle(context.Background(), backend, settings, nil, "ROOTTESTKEY", "root-test-secret-key", destination, 1024); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing bundle overwritten: %v", err)
	}
	if err := os.WriteFile(bodyPath, []byte("tampered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadImportBundle(context.Background(), destination, "ROOTTESTKEY", "root-test-secret-key", 1024); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("tampered bundle accepted: %v", err)
	}
}
