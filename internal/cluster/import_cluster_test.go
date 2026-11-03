package cluster

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestOfflineClusterMigrationExportsVerifiedVoters(t *testing.T) {
	ctx := context.Background()
	backend, settings := importFixture(t)
	root := t.TempDir()
	standaloneBundle := filepath.Join(root, "standalone-bundle")
	if _, err := ExportStandaloneBundle(ctx, backend, settings, nil, "ROOTTESTKEY", "root-test-secret-key", standaloneBundle, 1<<20); err != nil {
		t.Fatal(err)
	}
	oldPeers := []config.ClusterPeer{{ID: "a", URL: "https://a.example:9443"}, {ID: "b", URL: "https://b.example:9443"}}
	var members []OfflineClusterMember
	for _, nodeID := range []string{"a", "b"} {
		cfg := config.ClusterConfig{Enabled: true, ID: "old-cluster", NodeID: nodeID, Peers: oldPeers,
			ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2, StageMemoryBytes: 1 << 20}
		systemDir := filepath.Join(root, "old-"+nodeID)
		if _, err := SeedOfflineNode(ctx, cfg, standaloneBundle, systemDir, "ROOTTESTKEY", "root-test-secret-key", cfg.StageMemoryBytes, nil); err != nil {
			t.Fatal(err)
		}
		store, err := OpenStageStore(filepath.Join(systemDir, "cluster", "stages.db"), cfg.StageMemoryBytes)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		members = append(members, OfflineClusterMember{Config: cfg, SystemDir: systemDir, Store: store})
	}
	source, recovered, iamDocument, err := OpenOfflineClusterSource(ctx, members)
	if err != nil {
		t.Fatal(err)
	}
	migratedBundle := filepath.Join(root, "migrated-bundle")
	got, err := ExportStandaloneBundle(ctx, source, recovered, iamDocument, "ROOTTESTKEY", "root-test-secret-key", migratedBundle, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Buckets) != 1 || len(got.Buckets[0].Versions) != 2 {
		t.Fatalf("migrated inventory: %+v", got.Buckets)
	}
	newCfg := config.ClusterConfig{Enabled: true, ID: "new-cluster", NodeID: "c",
		Peers:        []config.ClusterPeer{{ID: "c", URL: "https://c.example:9443"}, {ID: "d", URL: "https://d.example:9443"}},
		ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2, StageMemoryBytes: 1 << 20}
	if _, err := SeedOfflineNode(ctx, newCfg, migratedBundle, filepath.Join(root, "new-c"), "ROOTTESTKEY", "root-test-secret-key", newCfg.StageMemoryBytes, nil); err != nil {
		t.Fatal(err)
	}
	if err := members[1].Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(members[1].SystemDir, "cluster", "stages.db")); err != nil {
		t.Fatal(err)
	}
	brokenSource, brokenSettings, brokenIAM, err := OpenOfflineClusterSource(ctx, members)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ExportStandaloneBundle(ctx, brokenSource, brokenSettings, brokenIAM, "ROOTTESTKEY", "root-test-secret-key", filepath.Join(root, "broken-bundle"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "verified durable copies") {
		t.Fatalf("missing durable voter accepted: %v", err)
	}
}

func TestOfflineClusterMigrationAfterLiveCommit(t *testing.T) {
	const clusterID = "live-migration"
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
	var members []OfflineClusterMember
	for _, name := range []string{"a", "b"} {
		cfg := config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers,
			TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1],
			ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2, StageMemoryBytes: 1 << 20}
		systemDir := filepath.Join(t.TempDir(), name)
		var err error
		nodes[name], err = OpenNode(cfg, filepath.Join(systemDir, "cluster"), cfg.StageMemoryBytes)
		if err != nil {
			t.Fatal(err)
		}
		servers[name].TLS = nodes[name].PeerTLSConfig()
		members = append(members, OfflineClusterMember{Config: cfg, SystemDir: systemDir})
	}
	for _, server := range servers {
		server.StartTLS()
		defer server.Close()
	}
	cancels := map[string]context.CancelFunc{}
	done := map[string]chan error{}
	for name, node := range nodes {
		runCtx, cancel := context.WithCancel(context.Background())
		cancels[name] = cancel
		result := make(chan error, 1)
		done[name] = result
		go func() { result <- node.Run(runCtx) }()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range []string{"a", "b"} {
		for {
			if err := nodes[name].ReadBarrier(ctx); err == nil {
				break
			}
			if ctx.Err() != nil {
				t.Fatalf("voter %s did not become ready: %v", name, ctx.Err())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	settings, err := config.NewSharedStore(config.SettingsFrom(config.Default()), SettingsStateStore{Node: nodes["a"]})
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	backend := NewS3Backend(nodes["b"])
	if err := backend.CreateBucket(ctx, "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := backend.SetBucketVersioning(ctx, "photos", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	w, err := backend.NewObjectWriter(ctx, "photos", "live.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("committed on old voters")); err != nil {
		t.Fatal(err)
	}
	version, err := w.Commit(ctx, storage.ObjectMeta{ETag: "live-etag"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job, err := NewJobMutation("copy_queued", nil, false, []byte(`{"state":"queued","kind":"copy"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewJobCatalog(nodes["a"]).CompareAndSwap(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := nodes[name].ReadBarrier(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a", "b"} {
		cancels[name]()
	}
	for _, name := range []string{"a", "b"} {
		if err := <-done[name]; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if err := nodes[name].Close(); err != nil {
			t.Fatal(err)
		}
	}
	for i := range members {
		store, err := OpenStageStore(filepath.Join(members[i].SystemDir, "cluster", "stages.db"), members[i].Config.StageMemoryBytes)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		members[i].Store = store
	}
	source, recovered, iam, err := OpenOfflineClusterSource(ctx, members)
	if err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(t.TempDir(), "bundle")
	bundle, err := ExportStandaloneBundle(ctx, source, recovered, iam, "ROOTTESTKEY", "root-test-secret-key", bundlePath, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Buckets) != 1 || len(bundle.Buckets[0].Versions) != 1 || bundle.Buckets[0].Versions[0].Info.VersionID != version.VersionID {
		t.Fatalf("live committed version missing from migration: %+v", bundle.Buckets)
	}
	if len(bundle.Jobs) != 1 || bundle.Jobs[0].ID != job.ID || string(bundle.Jobs[0].Data) != string(job.Next) {
		t.Fatalf("live job missing from migration: %+v", bundle.Jobs)
	}
	newCfg := config.ClusterConfig{Enabled: true, ID: "new-job-cluster", NodeID: "c",
		Peers:        []config.ClusterPeer{{ID: "c", URL: "https://c.example:9443"}, {ID: "d", URL: "https://d.example:9443"}},
		ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2, StageMemoryBytes: 1 << 20}
	newDir := filepath.Join(t.TempDir(), "new-c")
	if _, err := SeedOfflineNode(ctx, newCfg, bundlePath, newDir, "ROOTTESTKEY", "root-test-secret-key", newCfg.StageMemoryBytes, nil); err != nil {
		t.Fatal(err)
	}
	metadata, err := OpenMetadataStore(filepath.Join(newDir, "cluster", "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	if got, found, err := metadata.Get(MetadataJobs, jobRecordKey(job.ID)); err != nil || !found || !bytes.Equal(got, job.Next) {
		t.Fatalf("seeded job record: %q, %t, %v", got, found, err)
	}
}

func TestImportJobRecordsRejectMalformedAndDuplicateState(t *testing.T) {
	for _, records := range [][]JobRecord{
		{{ID: "bad/id", Data: []byte(`{"state":"queued"}`)}},
		{{ID: "copy_1", Data: []byte(`not json`)}},
		{{ID: "copy_1", Data: []byte(`{}`)}, {ID: "copy_1", Data: []byte(`{}`)}},
	} {
		if err := validateImportJobs(records); err == nil {
			t.Fatalf("invalid job records accepted: %+v", records)
		}
	}
}
