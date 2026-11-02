package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/storagetest"
)

// Run the storage contract against real two-voter Raft nodes. Each
// conformance subtest gets a fresh shared namespace and durable stages.
func TestClusterBackendConformance(t *testing.T) {
	storagetest.RunWithOptions(t, newClusterBackend, storagetest.Options{NoControlKeys: true, StressDivisor: 2})
}

func newClusterBackend(t *testing.T) storage.Backend {
	backend := NewS3Backend(newTwoNodeCluster(t)["a"])
	backend.operationTimeout = time.Minute
	return backend
}

func TestConditionalDeleteBindsCheckedNullVersion(t *testing.T) {
	backend := NewS3Backend(newTwoNodeCluster(t)["a"])
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := backend.CreateBucket(ctx, "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := backend.SetBucketVersioning(ctx, "photos", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	old, err := NewObjectPutIntent("photos", "k", []byte("old"), storage.ObjectMeta{ETag: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.objects.PutVersion(ctx, old); err != nil {
		t.Fatal(err)
	}
	marker, err := NewObjectPutIntent("photos", "k", nil, storage.ObjectMeta{})
	if err != nil {
		t.Fatal(err)
	}
	marker.DeleteMarker = true
	refused := errors.New("latest version changed")
	inserted := false
	marker.CheckLatest = func(info storage.ObjectInfo, found bool) error {
		if !found {
			return errors.New("latest version disappeared")
		}
		if info.ETag == "new" {
			return refused
		}
		if !inserted {
			inserted = true
			next, err := NewObjectPutIntent("photos", "k", []byte("new"), storage.ObjectMeta{ETag: "new"})
			if err != nil {
				return err
			}
			_, err = backend.objects.PutVersion(ctx, next)
			return err
		}
		return nil
	}
	if _, err := backend.objects.PutVersion(ctx, marker); !errors.Is(err, refused) {
		t.Fatalf("stale conditional delete: got %v, want %v", err, refused)
	}
	latest, err := backend.GetObjectInfo(ctx, "photos", "k", "")
	if err != nil || latest.DeleteMarker || latest.ETag != "new" {
		t.Fatalf("concurrent replacement did not survive: %+v, %v", latest, err)
	}
}

func newTwoNodeCluster(t *testing.T) map[string]*Node {
	t.Helper()
	const clusterID = "multipart-conformance"
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
		var err error
		nodes[name], err = OpenNode(cfg, filepath.Join(t.TempDir(), name), 16<<20)
		if err != nil {
			t.Fatal(err)
		}
		servers[name].TLS = nodes[name].PeerTLSConfig()
	}
	for _, server := range servers {
		server.StartTLS()
	}
	cancels := map[string]context.CancelFunc{}
	runners := map[string]chan error{}
	for name, node := range nodes {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[name] = cancel
		result := make(chan error, 1)
		runners[name] = result
		go func() { result <- node.Run(ctx) }()
	}
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
		for name, node := range nodes {
			if err := <-runners[name]; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
			if err := node.Close(); err != nil {
				t.Error(err)
			}
		}
		for _, server := range servers {
			server.Close()
		}
	})
	return nodes
}
