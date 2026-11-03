package server

import (
	"context"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func TestClusterServesImportedStandaloneVersions(t *testing.T) {
	const clusterID = "server-import"
	ctx := context.Background()
	source := memory.New()
	defer source.Close()
	if err := source.CreateBucket(ctx, "imported", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := source.SetBucketVersioning(ctx, "imported", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	writer, err := source.NewObjectWriter(ctx, "imported", "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("imported bytes")); err != nil {
		t.Fatal(err)
	}
	version, err := writer.Commit(ctx, storage.ObjectMeta{ETag: "imported-etag"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.DeleteObject(ctx, "imported", "file.txt", ""); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	base := testConfig()
	if _, err := cluster.ExportStandaloneBundle(ctx, source, config.SettingsFrom(base), nil,
		base.Auth.RootAccessKey, base.Auth.RootSecretKey, bundleDir, 1024); err != nil {
		t.Fatal(err)
	}
	ca, certs := clusterServerCerts(t, clusterID, "a", "b")
	peerListeners := map[string]net.Listener{}
	for _, name := range []string{"a", "b"} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peerListeners[name] = listener
	}
	peers := []config.ClusterPeer{{ID: "a", URL: "https://" + peerListeners["a"].Addr().String()}, {ID: "b", URL: "https://" + peerListeners["b"].Addr().String()}}
	servers := map[string]*Server{}
	for _, name := range []string{"a", "b"} {
		cfg := base
		cfg.Storage.Backend = config.BackendFS
		cfg.Storage.Path = t.TempDir()
		cfg.Cluster = config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers,
			TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1],
			ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2}
		if _, err := cluster.SeedOfflineNode(ctx, cfg.Cluster, bundleDir, cfg.SystemDir(),
			cfg.Auth.RootAccessKey, cfg.Auth.RootSecretKey, 256<<20, nil); err != nil {
			t.Fatal(err)
		}
		srv, err := New(cfg, testLogger(t), withPeerListener(peerListeners[name]))
		if err != nil {
			t.Fatal(err)
		}
		servers[name] = srv
	}
	cancels := map[string]context.CancelFunc{}
	done := map[string]chan error{}
	for name, srv := range servers {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		serveCtx, cancel := context.WithCancel(ctx)
		cancels[name] = cancel
		done[name] = make(chan error, 1)
		go func(s *Server, ch chan error) { ch <- s.Serve(serveCtx, listener) }(srv, done[name])
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
		for name, ch := range done {
			select {
			case err := <-ch:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("%s shutdown: %v", name, err)
				}
			case <-time.After(15 * time.Second):
				t.Errorf("%s did not stop", name)
			}
		}
	}()
	for name, srv := range servers {
		select {
		case <-srv.Ready():
		case err := <-done[name]:
			t.Fatalf("%s startup: %v", name, err)
		case <-time.After(15 * time.Second):
			t.Fatalf("%s did not become ready", name)
		}
		if !slices.Contains(srv.Info().Features, "cluster") || !slices.Contains(srv.Info().Features, "lifecycle") {
			t.Fatalf("%s omitted active cluster or lifecycle feature: %v", name, srv.Info().Features)
		}
		objectURL := srv.URL() + "/imported/file.txt"
		if r := do(t, "GET", objectURL, nil, &rootCreds, nil); r.status != 404 {
			t.Fatalf("%s latest delete marker: %d %s", name, r.status, r.body)
		}
		if r := do(t, "GET", objectURL+"?versionId="+url.QueryEscape(version.VersionID), nil, &rootCreds, nil); r.status != 200 || string(r.body) != "imported bytes" {
			t.Fatalf("%s imported version: %d %s", name, r.status, r.body)
		}
	}
}
