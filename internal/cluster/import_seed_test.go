package cluster

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	pb "go.etcd.io/raft/v3/raftpb"
)

func TestOfflineBundleSeedsTwoVotingNodes(t *testing.T) {
	backend, settings := importFixture(t)
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	bundle, err := ExportStandaloneBundle(context.Background(), backend, settings, nil, "ROOTTESTKEY", "root-test-secret-key", bundleDir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	const clusterID = "offline-import-test"
	ca, certs := peerTestCertificates(t, clusterID, "a", "b")
	nodes := map[string]*Node{}
	servers := map[string]*httptest.Server{}
	for _, name := range []string{"a", "b"} {
		name := name
		servers[name] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nodes[name].PeerHandler().ServeHTTP(w, r)
		}))
	}
	peers := []config.ClusterPeer{{ID: "a", URL: "https://" + servers["a"].Listener.Addr().String()}, {ID: "b", URL: "https://" + servers["b"].Listener.Addr().String()}}
	for _, name := range []string{"a", "b"} {
		cfg := config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1], ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2}
		systemDir := filepath.Join(t.TempDir(), ".gos3")
		fingerprint, err := SeedOfflineNode(context.Background(), cfg, bundleDir, systemDir, "ROOTTESTKEY", "root-test-secret-key", 1024, nil)
		if err != nil || len(fingerprint) != 64 {
			t.Fatalf("seed %s: %q, %v", name, fingerprint, err)
		}
		if _, err := SeedOfflineNode(context.Background(), cfg, bundleDir, systemDir, "ROOTTESTKEY", "root-test-secret-key", 1024, nil); err == nil {
			t.Fatalf("seed %s overwrote initialized node", name)
		}
		node, err := OpenNode(cfg, filepath.Join(systemDir, "cluster"), 1024)
		if err != nil {
			t.Fatalf("open imported node %s: %v", name, err)
		}
		nodes[name] = node
		servers[name].TLS = node.PeerTLSConfig()
	}
	for _, server := range servers {
		server.StartTLS()
		defer server.Close()
	}
	contexts := map[string]context.CancelFunc{}
	done := map[string]chan error{}
	for name, node := range nodes {
		ctx, cancel := context.WithCancel(context.Background())
		contexts[name] = cancel
		result := make(chan error, 1)
		done[name] = result
		go func() { result <- node.Run(ctx) }()
	}
	defer func() {
		for _, cancel := range contexts {
			cancel()
		}
		for name, node := range nodes {
			if err := <-done[name]; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
			if err := node.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range []string{"a", "b"} {
		for {
			err := nodes[name].ReadBarrier(ctx)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				t.Fatalf("imported node %s never reached quorum: %v", name, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		bucket, err := NewBucketCatalog(nodes[name]).GetBucket(ctx, "photos")
		if err != nil || bucket.Versioning != "Enabled" {
			t.Fatalf("imported bucket on %s: %+v, %v", name, bucket, err)
		}
		for _, version := range bundle.Buckets[0].Versions {
			info, body, err := NewObjectCatalog(nodes[name]).GetVersion(ctx, "photos", "one", version.Info.VersionID)
			if err != nil || info.VersionID != version.Info.VersionID {
				t.Fatalf("imported version on %s: %+v, %q, %v", name, info, body, err)
			}
			if !version.Info.DeleteMarker && string(body) != "verified bytes" {
				t.Fatalf("imported body on %s: %q", name, body)
			}
		}
	}
}

func TestOfflineImportedPeersRejectDifferentBundles(t *testing.T) {
	backend, settings := importFixture(t)
	bundles := []string{filepath.Join(t.TempDir(), "bundle-a"), filepath.Join(t.TempDir(), "bundle-b")}
	for _, path := range bundles {
		if _, err := ExportStandaloneBundle(context.Background(), backend, settings, nil, "ROOTTESTKEY", "root-test-secret-key", path, 1024); err != nil {
			t.Fatal(err)
		}
	}
	const clusterID = "offline-mismatch-test"
	ca, certs := peerTestCertificates(t, clusterID, "a", "b")
	nodes := map[string]*Node{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodes["b"].PeerHandler().ServeHTTP(w, r)
	}))
	defer server.Close()
	peers := []config.ClusterPeer{{ID: "a", URL: "https://127.0.0.1:1"}, {ID: "b", URL: "https://" + server.Listener.Addr().String()}}
	for i, name := range []string{"a", "b"} {
		cfg := config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1], ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2}
		systemDir := filepath.Join(t.TempDir(), ".gos3")
		if _, err := SeedOfflineNode(context.Background(), cfg, bundles[i], systemDir, "ROOTTESTKEY", "root-test-secret-key", 1024, nil); err != nil {
			t.Fatal(err)
		}
		var err error
		nodes[name], err = OpenNode(cfg, filepath.Join(systemDir, "cluster"), 1024)
		if err != nil {
			t.Fatal(err)
		}
		defer nodes[name].Close()
	}
	if nodes["a"].transport.importFingerprint == nodes["b"].transport.importFingerprint {
		t.Fatal("independent bundles unexpectedly have the same fingerprint")
	}
	server.TLS = nodes["b"].PeerTLSConfig()
	server.StartTLS()
	msg := &pb.Message{From: new(nodes["a"].transport.self), To: new(nodes["b"].transport.self)}
	if err := nodes["a"].transport.Send(context.Background(), msg); err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("mismatched Raft peer accepted: %v", err)
	}
	replica, err := nodes["a"].transport.Replica("b")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("verified bytes")
	if err := replica.Stage(context.Background(), "different-bundle", payload, sha256.Sum256(payload)); err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("mismatched payload peer accepted: %v", err)
	}
}

func TestOfflineSeedRequiresEmptyTarget(t *testing.T) {
	backend, settings := importFixture(t)
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	if _, err := ExportStandaloneBundle(context.Background(), backend, settings, nil, "ROOTTESTKEY", "root-test-secret-key", bundleDir, 1024); err != nil {
		t.Fatal(err)
	}
	systemDir := filepath.Join(t.TempDir(), ".gos3")
	if err := os.Mkdir(systemDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(systemDir, "unexpected.db"), []byte("old data"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.ClusterConfig{Enabled: true, ID: "empty-target-test", NodeID: "a", Peers: []config.ClusterPeer{{ID: "a", URL: "https://a.example:9443"}, {ID: "b", URL: "https://b.example:9443"}}, ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2}
	if _, err := SeedOfflineNode(context.Background(), cfg, bundleDir, systemDir, "ROOTTESTKEY", "root-test-secret-key", 1024, nil); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("nonempty target accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(systemDir, "cluster")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cluster state created after refusal: %v", err)
	}
}
