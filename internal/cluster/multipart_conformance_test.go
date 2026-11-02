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
