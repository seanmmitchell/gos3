package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
)

// A configured node cannot start without its authenticated peer identity.
func TestClusterModeRequiresPeerIdentity(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Path = t.TempDir()
	cfg.Auth.RootAccessKey = testAccessKey
	cfg.Auth.RootSecretKey = testSecretKey
	cfg.Cluster = config.ClusterConfig{
		Enabled:        true,
		ID:             "cluster-a",
		NodeID:         "a",
		Peers:          []config.ClusterPeer{{ID: "a", URL: "https://a.example:9443"}, {ID: "b", URL: "https://b.example:9443"}},
		TLSCert:        "peer.crt",
		TLSKey:         "peer.key",
		TLSCA:          "ca.crt",
		ReplicaCount:   2,
		MinMemoryAcks:  2,
		MinDurableAcks: 2,
	}
	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "loading peer certificate") {
		t.Fatalf("New() = %v, want peer identity error", err)
	}
}

func TestClusterDataPathRequiresOfflineImport(t *testing.T) {
	for _, name := range []string{"meta.db", "iam.json", "settings.json", "placements.json", "backings.json", "root-credentials.json", "server-id"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte("old state"), 0o600); err != nil {
				t.Fatal(err)
			}
			s := &Server{systemDir: dir}
			if err := s.checkClusterDataDir(); err == nil || !strings.Contains(err.Error(), "gos3 cluster import") {
				t.Fatalf("existing standalone %s: %v", name, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "cluster")); !os.IsNotExist(err) {
				t.Fatalf("cluster state created before refusing standalone data: %v", err)
			}
		})
	}
	if err := (&Server{systemDir: t.TempDir()}).checkClusterDataDir(); err != nil {
		t.Fatalf("empty node: %v", err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "cluster"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cluster", "raft.db"), []byte("raft state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server-id"), []byte("node identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Server{systemDir: dir}).checkClusterDataDir(); err != nil {
		t.Fatalf("existing cluster node: %v", err)
	}
}
