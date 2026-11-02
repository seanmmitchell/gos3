package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/server"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestClusterImportCommandsRequireStoppedSourceAndEmptyTarget(t *testing.T) {
	clearEnv(t)
	root := t.TempDir()
	sourceConfig := config.Default()
	sourceConfig.Storage.Path = filepath.Join(root, "source")
	sourceConfig.Auth.RootAccessKey = "import-root-key"
	sourceConfig.Auth.RootSecretKey = "import-root-secret-key-12345"
	sourcePath := filepath.Join(root, "source.json")
	writeImportTestConfig(t, sourcePath, sourceConfig)
	logger, logControl := server.NewLogger(io.Discard, "error", "text")
	source, err := server.New(sourceConfig, logger, server.WithLogControl(logControl), server.WithOfflineInspection())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := source.Backend().CreateBucket(ctx, "imported", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := source.Backend().SetBucketVersioning(ctx, "imported", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	writer, err := source.Backend().NewObjectWriter(ctx, "imported", "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "source bytes"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Commit(ctx, storage.ObjectMeta{ETag: "test-etag"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Backend().DeleteObject(ctx, "imported", "file.txt", ""); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(root, "bundle")
	args := []string{"cluster", "import", "export", "--source-config", sourcePath, "--output", bundlePath}
	if code, _, message := runCmd(t, args...); code != 1 || (!strings.Contains(message, "in use") && !strings.Contains(message, "locked")) {
		t.Fatalf("live source export: code %d, %s", code, message)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if code, output, message := runCmd(t, args...); code != 0 || !strings.Contains(output, "exported 1 buckets and 2 versions") {
		t.Fatalf("export: code %d, %s, %s", code, output, message)
	}
	if code, _, message := runCmd(t, args...); code != 1 || !strings.Contains(message, "already exists") {
		t.Fatalf("overwrite bundle: code %d, %s", code, message)
	}
	targetConfig := config.Default()
	targetConfig.Storage.Path = filepath.Join(root, "target")
	targetConfig.Auth = sourceConfig.Auth
	targetConfig.Cluster = config.ClusterConfig{
		Enabled: true, ID: "import-cluster", NodeID: "a",
		Peers: []config.ClusterPeer{{ID: "a", URL: "https://a.example:9443"}, {ID: "b", URL: "https://b.example:9443"}},
		TLSCA: "ca.pem", TLSCert: "a.pem", TLSKey: "a.key",
		ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2,
	}
	targetPath := filepath.Join(root, "target.json")
	writeImportTestConfig(t, targetPath, targetConfig)
	seedArgs := []string{"cluster", "import", "seed", "--cluster-config", targetPath, "--bundle", bundlePath}
	targetLock, err := server.LockSystemDirForImport(targetConfig.SystemDir())
	if err != nil {
		t.Fatal(err)
	}
	if code, _, message := runCmd(t, seedArgs...); code != 1 || !strings.Contains(message, "in use") {
		_ = targetLock.Close()
		t.Fatalf("locked target seed: code %d, %s", code, message)
	}
	if err := targetLock.Close(); err != nil {
		t.Fatal(err)
	}
	if code, output, message := runCmd(t, seedArgs...); code != 0 || !strings.Contains(output, "seeded node a") {
		t.Fatalf("seed: code %d, %s, %s", code, output, message)
	}
	if code, _, message := runCmd(t, seedArgs...); code != 1 || !strings.Contains(message, "already has cluster state") {
		t.Fatalf("reseed: code %d, %s", code, message)
	}
	peerConfig := targetConfig
	peerConfig.Storage.Path = filepath.Join(root, "peer")
	peerConfig.Cluster.NodeID = "b"
	peerPath := filepath.Join(root, "peer.json")
	writeImportTestConfig(t, peerPath, peerConfig)
	if code, _, message := runCmd(t, "cluster", "import", "seed", "--cluster-config", peerPath, "--bundle", bundlePath); code != 0 {
		t.Fatalf("seed second voter: code %d, %s", code, message)
	}
	migratedPath := filepath.Join(root, "migrated")
	migrateArgs := []string{"cluster", "migrate", "export", "--source-config", targetPath, "--source-config", peerPath, "--output", migratedPath}
	oldLock, err := server.LockSystemDirForImport(peerConfig.SystemDir())
	if err != nil {
		t.Fatal(err)
	}
	if code, _, message := runCmd(t, migrateArgs...); code != 1 || !strings.Contains(message, "in use") {
		_ = oldLock.Close()
		t.Fatalf("running old voter migration: code %d, %s", code, message)
	}
	if err := oldLock.Close(); err != nil {
		t.Fatal(err)
	}
	if code, output, message := runCmd(t, migrateArgs...); code != 0 || !strings.Contains(output, "exported 1 buckets and 2 versions from old cluster") {
		t.Fatalf("migrate export: code %d, %s, %s", code, output, message)
	}
	newConfig := targetConfig
	newConfig.Storage.Path = filepath.Join(root, "new-voter")
	newConfig.Cluster.ID = "replacement-cluster"
	newConfig.Cluster.NodeID = "c"
	newConfig.Cluster.Peers = []config.ClusterPeer{{ID: "c", URL: "https://c.example:9443"}, {ID: "d", URL: "https://d.example:9443"}}
	newPath := filepath.Join(root, "new.json")
	writeImportTestConfig(t, newPath, newConfig)
	if code, output, message := runCmd(t, "cluster", "import", "seed", "--cluster-config", newPath, "--bundle", migratedPath); code != 0 || !strings.Contains(output, "seeded node c") {
		t.Fatalf("seed replacement voter: code %d, %s, %s", code, output, message)
	}
}

func writeImportTestConfig(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	data, err := cfg.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineImportRejectsUnmigratedBackgroundState(t *testing.T) {
	systemDir := t.TempDir()
	jobsDir := filepath.Join(systemDir, "jobs")
	if err := os.Mkdir(jobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	jobsPath := filepath.Join(jobsDir, "jobs.json")
	if err := os.WriteFile(jobsPath, []byte(`{"version":1,"jobs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyOfflineAuxiliaryState(systemDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobsPath, []byte(`{"version":1,"jobs":[{"id":"pending"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyOfflineAuxiliaryState(systemDir); err == nil || !strings.Contains(err.Error(), "cannot yet be imported") {
		t.Fatalf("pending job: %v", err)
	}
	if err := os.WriteFile(jobsPath, []byte(`{"version":1,"jobs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotsPath := filepath.Join(systemDir, "restic-snapshots.json")
	if err := os.WriteFile(snapshotsPath, []byte(`{"version":1,"buckets":{"photos":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyOfflineAuxiliaryState(systemDir); err == nil || !strings.Contains(err.Error(), "cannot yet be imported") {
		t.Fatalf("snapshot scheduler state: %v", err)
	}
}
