package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/replication"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func clusterServerCerts(t *testing.T, clusterID string, names ...string) (string, map[string][2]string) {
	t.Helper()
	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test cluster CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	files := make(map[string][2]string, len(names))
	for i, name := range names {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse("spiffe://gos3/" + clusterID + "/" + name)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, URIs: []*url.URL{uri}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, key.Public(), caKey)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPath, keyPath := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
		if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = [2]string{certPath, keyPath}
	}
	return caPath, files
}

type clusterPeerGate struct {
	path    string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newClusterPeerGate(path string) *clusterPeerGate {
	return &clusterPeerGate{path: path, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *clusterPeerGate) unblock() { g.once.Do(func() { close(g.release) }) }

func TestClusterNeverReturns200BeforeMemoryAndDurableAcks(t *testing.T) {
	const clusterID = "server-ack-gates"
	ca, certs := clusterServerCerts(t, clusterID, "a", "b")
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "replicas")
	listeners := map[string]net.Listener{}
	for _, name := range []string{"a", "b"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners[name] = ln
	}
	peers := []config.ClusterPeer{{ID: "a", URL: "https://" + listeners["a"].Addr().String()}, {ID: "b", URL: "https://" + listeners["b"].Addr().String()}}
	var gateMu sync.RWMutex
	var gate *clusterPeerGate
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gateMu.RLock()
			current := gate
			gateMu.RUnlock()
			if current != nil && r.URL.Path == current.path {
				select {
				case current.entered <- struct{}{}:
				default:
				}
				select {
				case <-current.release:
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	servers := map[string]*Server{}
	for _, name := range []string{"a", "b"} {
		cfg := testConfig()
		cfg.Storage.Backend = config.BackendFS
		cfg.Storage.Path = t.TempDir()
		if name == "b" {
			cfg.Storage.Backend = config.BackendS3
			cfg.Storage.S3 = &config.S3Config{
				Endpoint: remote.URL, HomeBucket: "replicas", Addressing: "path",
				AccessKey: s3clienttest.AccessKey, SecretKey: s3clienttest.SecretKey,
			}
		}
		cfg.Cluster = config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1], ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2}
		opts := []Option{withPeerListener(listeners[name])}
		if name == "b" {
			opts = append(opts, withPeerHandler(wrap))
		}
		srv, err := New(cfg, testLogger(t), opts...)
		if err != nil {
			t.Fatal(err)
		}
		servers[name] = srv
	}
	cancels := map[string]context.CancelFunc{}
	done := map[string]chan error{}
	for _, name := range []string{"a", "b"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancels[name] = cancel
		done[name] = make(chan error, 1)
		srv, ch := servers[name], done[name]
		go func() { ch <- srv.Serve(ctx, ln) }()
	}
	t.Cleanup(func() {
		gateMu.RLock()
		if gate != nil {
			gate.unblock()
		}
		gateMu.RUnlock()
		for _, cancel := range cancels {
			cancel()
		}
		for _, ch := range done {
			select {
			case err := <-ch:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(15 * time.Second):
				t.Error("cluster server did not stop")
			}
		}
	})
	for _, name := range []string{"a", "b"} {
		select {
		case <-servers[name].Ready():
		case err := <-done[name]:
			t.Fatalf("%s startup: %v", name, err)
		case <-time.After(15 * time.Second):
			t.Fatalf("%s did not become ready", name)
		}
	}
	url := servers["a"].URL()
	if r := do(t, "PUT", url+"/ack-bucket", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("create bucket: %d %s", r.status, r.body)
	}
	for _, step := range []struct {
		path    string
		key     string
		memory  int
		durable int
	}{{"/stage", "memory-gated", 1, 1}, {"/persist", "durability-gated", 2, 1}} {
		current := newClusterPeerGate(step.path)
		gateMu.Lock()
		gate = current
		gateMu.Unlock()
		result := make(chan response, 1)
		go func() { result <- do(t, "PUT", url+"/ack-bucket/"+step.key, []byte("payload"), &rootCreds, nil) }()
		select {
		case <-current.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("peer never reached %s", step.path)
		}
		select {
		case r := <-result:
			t.Fatalf("write returned before %s acknowledgment: %d %s", step.path, r.status, r.body)
		case <-time.After(250 * time.Millisecond):
		}
		status := servers["a"].clusterNode.Status(context.Background())
		if status.LastWrite.ConsensusCommitted || status.LastWrite.MemoryAcks != step.memory || status.LastWrite.DurableAcks != step.durable {
			t.Fatalf("write admitted before %s release: %+v", step.path, status.LastWrite)
		}
		current.unblock()
		select {
		case r := <-result:
			if r.status != 200 {
				t.Fatalf("write after %s release: %d %s", step.path, r.status, r.body)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("write did not finish after %s release", step.path)
		}
		gateMu.Lock()
		gate = nil
		gateMu.Unlock()
	}
	denyRemotePut := func(r *http.Request) int {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/_gos3-cluster/"+clusterID+"/b/") {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	remote.Fail.Store(&denyRemotePut)
	failed := do(t, "PUT", url+"/ack-bucket/remote-unavailable", []byte("payload"), &rootCreds, nil)
	if failed.status != http.StatusServiceUnavailable {
		t.Fatalf("S3 backing PUT failure returned %d, want 503: %s", failed.status, failed.body)
	}
	status := servers["a"].clusterNode.Status(context.Background())
	if status.LastWrite.ConsensusCommitted || status.LastWrite.DurableAcks >= 2 {
		t.Fatalf("failed S3 backing counted as durable or published: %+v", status.LastWrite)
	}
	remote.Fail.Store(nil)
	recovered := do(t, "PUT", url+"/ack-bucket/remote-unavailable", []byte("payload"), &rootCreds, nil)
	if recovered.status != http.StatusOK {
		t.Fatalf("write after S3 backing recovery: %d %s", recovered.status, recovered.body)
	}
	remoteCopies := 0
	if err := remote.Backend.WalkObjects(context.Background(), "replicas", "_gos3-cluster/"+clusterID+"/b/", "", func(storage.ObjectInfo) error {
		remoteCopies++
		return nil
	}); err != nil || remoteCopies < 2 {
		t.Fatalf("S3-backed voter has %d durable object copies: %v", remoteCopies, err)
	}
}

func TestClusterServesTwoNodeS3AndFailsClosed(t *testing.T) {
	const clusterID = "server-cluster"
	ca, certs := clusterServerCerts(t, clusterID, "a", "b")
	peerListeners := map[string]net.Listener{}
	for _, name := range []string{"a", "b"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peerListeners[name] = ln
	}
	peers := []config.ClusterPeer{{ID: "a", URL: "https://" + peerListeners["a"].Addr().String()}, {ID: "b", URL: "https://" + peerListeners["b"].Addr().String()}}
	servers := map[string]*Server{}
	configs := map[string]config.Config{}
	for _, name := range []string{"a", "b"} {
		cfg := testConfig()
		cfg.Storage.Backend = config.BackendFS
		cfg.Storage.Path = t.TempDir()
		cfg.Cluster = config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1], ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2}
		configs[name] = cfg
		srv, err := New(cfg, testLogger(t), withPeerListener(peerListeners[name]))
		if err != nil {
			t.Fatal(err)
		}
		servers[name] = srv
	}
	cancels := map[string]context.CancelFunc{}
	done := map[string]chan error{}
	for name, srv := range servers {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancels[name] = cancel
		done[name] = make(chan error, 1)
		go func() { done[name] <- srv.Serve(ctx, ln) }()
	}
	stop := func(name string) {
		t.Helper()
		if cancels[name] == nil {
			return
		}
		cancels[name]()
		cancels[name] = nil
		select {
		case err := <-done[name]:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("%s server stopped: %v", name, err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("%s server did not stop", name)
		}
	}
	t.Cleanup(func() { stop("a"); stop("b") })
	for name, srv := range servers {
		select {
		case <-srv.Ready():
		case err := <-done[name]:
			t.Fatalf("%s server startup: %v", name, err)
		case <-time.After(15 * time.Second):
			t.Fatalf("%s server did not become ready", name)
		}
	}
	aURL, bURL := servers["a"].URL(), servers["b"].URL()
	if r := do(t, "PUT", aURL+"/photos", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("create through a: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", aURL+"/photos/k", []byte("committed"), &rootCreds, nil); r.status != 200 {
		t.Fatalf("write through a: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", bURL+"/photos/k", nil, &rootCreds, nil); r.status != 200 || string(r.body) != "committed" {
		t.Fatalf("read through b: %d %s", r.status, r.body)
	}
	var writeStatus cluster.Status
	writeReply := do(t, "GET", aURL+"/_gos3/admin/v1/cluster", nil, &rootCreds, nil)
	writeReply.json(t, &writeStatus)
	if writeReply.status != 200 || writeStatus.LastWrite.MemoryAcks != 2 || writeStatus.LastWrite.DurableAcks != 2 || !writeStatus.LastWrite.ConsensusCommitted {
		t.Fatalf("acknowledged write status: %d %+v", writeReply.status, writeStatus.LastWrite)
	}
	if r := do(t, "GET", bURL+"/_gos3/admin/v1/config", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("admin through b: %d %s", r.status, r.body)
	}
	clusterReply := do(t, "GET", bURL+"/_gos3/admin/v1/cluster", nil, &rootCreds, nil)
	var clusterStatus cluster.Status
	clusterReply.json(t, &clusterStatus)
	if clusterReply.status != 200 || !clusterStatus.Available || clusterStatus.LeaderID == "" || clusterStatus.MinMemoryAcks != 2 || clusterStatus.MinDurableAcks != 2 || len(clusterStatus.Peers) != 2 {
		t.Fatalf("healthy cluster status: %d %+v", clusterReply.status, clusterStatus)
	}
	if r := do(t, "GET", bURL+"/_gos3/metrics", nil, &rootCreds, nil); r.status != 200 || !strings.Contains(string(r.body), "gos3_cluster_min_durable_acks 2") || !strings.Contains(string(r.body), "gos3_cluster_available 1") || !strings.Contains(string(r.body), "gos3_cluster_last_repair_repaired ") {
		t.Fatalf("cluster metrics: %d %s", r.status, r.body)
	}
	jobData, err := json.Marshal(jobs.Job{ID: "copy_queued", Kind: "copy", State: jobs.StateQueued,
		Labels: map[string]string{"source": "photos", "target": "replica"}, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	jobMutation, err := cluster.NewJobMutation("copy_queued", nil, false, jobData)
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.NewJobCatalog(servers["a"].clusterNode).CompareAndSwap(context.Background(), jobMutation); err != nil {
		t.Fatalf("commit shared job: %v", err)
	}
	checkpoint, err := cluster.NewJobMutation("replication_test", nil, false, []byte(`{"backfillComplete":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.NewJobCatalog(servers["b"].clusterNode).CompareAndSwap(context.Background(), checkpoint); err != nil {
		t.Fatalf("commit replication checkpoint: %v", err)
	}
	if r := do(t, "GET", bURL+"/_gos3/admin/v1/jobs", nil, &rootCreds, nil); r.status != 200 || !strings.Contains(string(r.body), `"copy_queued"`) || strings.Contains(string(r.body), "replication_test") {
		t.Fatalf("shared job listing through b: %d %s", r.status, r.body)
	}
	if r := do(t, "POST", bURL+"/_gos3/admin/v1/jobs/copy_queued/pause", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("pause shared job through b: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", aURL+"/_gos3/admin/v1/jobs/copy_queued", nil, &rootCreds, nil); r.status != 200 || !strings.Contains(string(r.body), `"state":"paused"`) {
		t.Fatalf("paused job through a: %d %s", r.status, r.body)
	}
	if r := do(t, "POST", aURL+"/_gos3/admin/v1/jobs/copy_queued/cancel", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("cancel shared job through a: %d %s", r.status, r.body)
	}
	if r := do(t, "DELETE", bURL+"/_gos3/admin/v1/jobs/copy_queued", nil, &rootCreds, nil); r.status != 204 {
		t.Fatalf("delete shared job through b: %d %s", r.status, r.body)
	}
	var settings config.Settings
	configReply := do(t, "GET", bURL+"/_gos3/admin/v1/config", nil, &rootCreds, nil)
	configReply.json(t, &settings)
	settings.Name = "shared-cluster-name"
	settingsDoc, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if r := do(t, "PUT", aURL+"/_gos3/admin/v1/config", settingsDoc, &rootCreds, map[string]string{"Content-Type": "application/json"}); r.status != 200 {
		t.Fatalf("settings write through a: %d %s", r.status, r.body)
	}
	configReply = do(t, "GET", bURL+"/_gos3/admin/v1/config", nil, &rootCreds, nil)
	configReply.json(t, &settings)
	if settings.Name != "shared-cluster-name" {
		t.Fatalf("settings read through b: %s", configReply.body)
	}
	userDoc := []byte(`{"name":"alice","policies":["readwrite"]}`)
	if r := do(t, "POST", aURL+"/_gos3/admin/v1/users", userDoc, &rootCreds, map[string]string{"Content-Type": "application/json"}); r.status != 201 {
		t.Fatalf("IAM write through a: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", bURL+"/_gos3/admin/v1/users/alice", nil, &rootCreds, nil); r.status != 200 || !strings.Contains(string(r.body), `"alice"`) {
		t.Fatalf("IAM read through b: %d %s", r.status, r.body)
	}
	versioning := []byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`)
	if r := do(t, "PUT", bURL+"/photos?versioning", versioning, &rootCreds, nil); r.status != 200 {
		t.Fatalf("versioning through b: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", bURL+"/photos/versioned", []byte("version one"), &rootCreds, nil); r.status != 200 || r.header.Get("X-Amz-Version-Id") == "" {
		t.Fatalf("versioned write through b: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", aURL+"/photos/versioned", nil, &rootCreds, nil); r.status != 200 || string(r.body) != "version one" {
		t.Fatalf("versioned read through a: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", bURL+"/replica", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("create replication destination: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", aURL+"/replica?versioning", versioning, &rootCreds, nil); r.status != 200 {
		t.Fatalf("enable destination versioning: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", aURL+"/copytarget", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("create cluster copy target: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", bURL+"/copytarget?versioning", versioning, &rootCreds, nil); r.status != 200 {
		t.Fatalf("version cluster copy target: %d %s", r.status, r.body)
	}
	cors := []byte(`<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><CORSRule><AllowedOrigin>https://example.com</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`)
	if r := do(t, "PUT", aURL+"/photos?cors", cors, &rootCreds, nil); r.status != 200 {
		t.Fatalf("source CORS configuration: %d %s", r.status, r.body)
	}
	firstCopy := do(t, "PUT", aURL+"/photos/copy-input", []byte("copy one"), &rootCreds, nil)
	secondCopy := do(t, "PUT", bURL+"/photos/copy-input", []byte("copy two"), &rootCreds, nil)
	if firstCopy.status != 200 || secondCopy.status != 200 || firstCopy.header.Get("X-Amz-Version-Id") == "" || secondCopy.header.Get("X-Amz-Version-Id") == "" {
		t.Fatalf("cluster copy source versions: %d %s; %d %s", firstCopy.status, firstCopy.body, secondCopy.status, secondCopy.body)
	}
	copyRequest := []byte(`{"kind":"copy","source":{"bucket":"photos","prefix":"copy-"},"target":{"bucket":"copytarget","prefix":"archive-"},"options":{"versions":"all"}}`)
	copyReply := do(t, "POST", bURL+"/_gos3/admin/v1/jobs", copyRequest, &rootCreds, map[string]string{"Content-Type": "application/json"})
	var copyJob jobs.Job
	if copyReply.status != 201 {
		t.Fatalf("submit shared copy job: %d %s", copyReply.status, copyReply.body)
	}
	copyReply.json(t, &copyJob)
	copyDeadline := time.Now().Add(20 * time.Second)
	for {
		jobReply := do(t, "GET", aURL+"/_gos3/admin/v1/jobs/"+copyJob.ID, nil, &rootCreds, nil)
		jobReply.json(t, &copyJob)
		if jobReply.status == 200 && copyJob.State == jobs.StateSucceeded {
			break
		}
		if jobReply.status != 200 || copyJob.State == jobs.StateFailed || time.Now().After(copyDeadline) {
			t.Fatalf("cluster copy job did not finish: %d %s", jobReply.status, jobReply.body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var copyStatus struct {
		Passes int64 `json:"passes"`
	}
	if err := json.Unmarshal(copyJob.Status, &copyStatus); err != nil || copyJob.Phase != "done" || copyStatus.Passes != 2 {
		t.Fatalf("shared copy progress: phase=%q status=%s err=%v", copyJob.Phase, copyJob.Status, err)
	}
	if r := do(t, "GET", bURL+"/copytarget?cors", nil, &rootCreds, nil); r.status != 200 || !strings.Contains(string(r.body), "https://example.com") {
		t.Fatalf("copied CORS configuration through b: %d %s", r.status, r.body)
	}
	sharedJob, found, err := cluster.NewJobCatalog(servers["b"].clusterNode).Get(context.Background(), copyJob.ID)
	var persistedJob jobs.Job
	if err != nil || !found || json.Unmarshal(sharedJob, &persistedJob) != nil || string(persistedJob.Checkpoint) != `{"pass":2}` {
		t.Fatalf("committed copy checkpoint: found=%t err=%v job=%s", found, err, sharedJob)
	}
	for _, version := range []struct{ id, body string }{
		{firstCopy.header.Get("X-Amz-Version-Id"), "copy one"},
		{secondCopy.header.Get("X-Amz-Version-Id"), "copy two"},
	} {
		r := do(t, "GET", aURL+"/copytarget/archive-input?versionId="+url.QueryEscape(version.id), nil, &rootCreds, nil)
		if r.status != 200 || string(r.body) != version.body {
			t.Fatalf("copied version %s through a: %d %s", version.id, r.status, r.body)
		}
	}
	badCreateRequest := []byte(`{"kind":"copy","source":{"bucket":"photos","prefix":"copy-"},"target":{"bucket":"badcopy","create":{"backing":"node-local"}},"options":{"configs":"none"}}`)
	if r := do(t, "POST", aURL+"/_gos3/admin/v1/jobs", badCreateRequest, &rootCreds, map[string]string{"Content-Type": "application/json"}); r.status != 400 {
		t.Fatalf("node-local backing selected by cluster job: %d %s", r.status, r.body)
	}
	createCopyRequest := []byte(`{"kind":"copy","source":{"bucket":"photos","prefix":"copy-"},"target":{"bucket":"newcopy","prefix":"archive-","create":{}},"options":{"configs":"none"}}`)
	createCopyReply := do(t, "POST", aURL+"/_gos3/admin/v1/jobs", createCopyRequest, &rootCreds, map[string]string{"Content-Type": "application/json"})
	var createdCopyJob jobs.Job
	if createCopyReply.status != 201 {
		t.Fatalf("submit target-creating cluster copy: %d %s", createCopyReply.status, createCopyReply.body)
	}
	createCopyReply.json(t, &createdCopyJob)
	createDeadline := time.Now().Add(20 * time.Second)
	for {
		jobReply := do(t, "GET", bURL+"/_gos3/admin/v1/jobs/"+createdCopyJob.ID, nil, &rootCreds, nil)
		jobReply.json(t, &createdCopyJob)
		if jobReply.status == 200 && createdCopyJob.State == jobs.StateSucceeded {
			break
		}
		if jobReply.status != 200 || createdCopyJob.State == jobs.StateFailed || time.Now().After(createDeadline) {
			t.Fatalf("target-creating cluster copy did not finish: %d %s", jobReply.status, jobReply.body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r := do(t, "GET", bURL+"/newcopy/archive-input?versionId="+url.QueryEscape(firstCopy.header.Get("X-Amz-Version-Id")), nil, &rootCreds, nil); r.status != 200 || string(r.body) != "copy one" {
		t.Fatalf("new target retained first source version: %d %s", r.status, r.body)
	}
	const roleARN = "arn:aws:iam::000000000000:role/cluster-replication"
	if err := servers["a"].iam.PutReplicationRole(context.Background(), iam.ReplicationRole{
		ARN: roleARN, Grants: []iam.ReplicationGrant{{SourceBucket: "photos", DestinationBucket: "replica", Actions: []string{iam.ReplicateObjectAction}}},
	}); err != nil {
		t.Fatalf("create shared replication role: %v", err)
	}
	rule := []byte(`<ReplicationConfiguration><Role>` + roleARN + `</Role><Rule><ID>all</ID><Status>Enabled</Status><Prefix></Prefix><Destination><Bucket>arn:aws:s3:::replica</Bucket></Destination><ExistingObjectReplication><Status>Enabled</Status></ExistingObjectReplication></Rule></ReplicationConfiguration>`)
	if r := do(t, "PUT", bURL+"/photos?replication", rule, &rootCreds, nil); r.status != 200 {
		t.Fatalf("put shared bucket replication rule: %d %s", r.status, r.body)
	}
	replicationDeadline := time.Now().Add(45 * time.Second)
	for {
		r := do(t, "GET", bURL+"/replica/versioned", nil, &rootCreds, nil)
		if r.status == 200 && string(r.body) == "version one" {
			break
		}
		if r.status != 404 || time.Now().After(replicationDeadline) {
			t.Fatalf("asynchronous cluster replication backfill through b: %d %s", r.status, r.body)
		}
		time.Sleep(350 * time.Millisecond)
	}
	progressBackend := servers["a"].backend.(interface {
		LoadReplicationProgress(context.Context, string) ([]byte, bool, error)
	})
	for {
		raw, found, err := progressBackend.LoadReplicationProgress(context.Background(), "photos")
		var progress replication.Progress
		if err == nil && found && json.Unmarshal(raw, &progress) == nil && progress.BackfillComplete && progress.LastScanAt.After(time.Time{}) {
			break
		}
		if time.Now().After(replicationDeadline) {
			t.Fatalf("shared replication checkpoint after backfill: found=%t progress=%+v err=%v", found, progress, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop("b")
	if r := do(t, "PUT", aURL+"/photos/after-loss", []byte("must-fail"), &rootCreds, nil); r.status != 503 || !strings.Contains(string(r.body), "<Code>ServiceUnavailable</Code>") {
		t.Fatalf("write below two-node policy: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", aURL+"/_gos3/health", nil, nil, nil); r.status != 200 {
		t.Fatalf("liveness after quorum loss: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", aURL+"/_gos3/admin/v1/config", nil, &rootCreds, nil); r.status != 503 {
		t.Fatalf("admin read after quorum loss: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", aURL+"/_gos3/admin/v1/jobs", nil, &rootCreds, nil); r.status != 503 {
		t.Fatalf("job listing after quorum loss: %d %s", r.status, r.body)
	}
	clusterReply = do(t, "GET", aURL+"/_gos3/admin/v1/cluster", nil, &rootCreds, nil)
	clusterReply.json(t, &clusterStatus)
	if clusterReply.status != 200 || clusterStatus.Available {
		t.Fatalf("degraded cluster status: %d %+v", clusterReply.status, clusterStatus)
	}
	peerPort := peerListeners["b"].Addr().String()
	restartedPeer, err := net.Listen("tcp", peerPort)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(configs["b"], testLogger(t), withPeerListener(restartedPeer))
	if err != nil {
		t.Fatal(err)
	}
	servers["b"] = restarted
	publicListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancels["b"] = cancel
	done["b"] = make(chan error, 1)
	go func() { done["b"] <- restarted.Serve(ctx, publicListener) }()
	select {
	case <-restarted.Ready():
	case err := <-done["b"]:
		t.Fatalf("restarted node failed: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("restarted node did not become ready")
	}
	if r := do(t, "GET", restarted.URL()+"/photos/versioned", nil, &rootCreds, nil); r.status != 200 || string(r.body) != "version one" {
		t.Fatalf("recovered version through b: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", aURL+"/photos/after-restart", []byte("recovered"), &rootCreds, nil); r.status != 200 {
		t.Fatalf("write after quorum recovery: %d %s", r.status, r.body)
	}
	clusterReply = do(t, "GET", restarted.URL()+"/_gos3/admin/v1/cluster", nil, &rootCreds, nil)
	clusterReply.json(t, &clusterStatus)
	if clusterReply.status != 200 || !clusterStatus.Available {
		t.Fatalf("recovered cluster status: %d %+v", clusterReply.status, clusterStatus)
	}
}

func TestClusterThreeNodesRetainTwoOfThreeWritesAfterLeaderLoss(t *testing.T) {
	const clusterID = "server-three-node"
	names := []string{"a", "b", "c"}
	ca, certs := clusterServerCerts(t, clusterID, names...)
	peerListeners := make(map[string]net.Listener, len(names))
	for _, name := range names {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peerListeners[name] = ln
	}
	peers := make([]config.ClusterPeer, 0, len(names))
	for _, name := range names {
		peers = append(peers, config.ClusterPeer{ID: name, URL: "https://" + peerListeners[name].Addr().String()})
	}
	servers := make(map[string]*Server, len(names))
	for _, name := range names {
		cfg := testConfig()
		cfg.Storage.Backend = config.BackendFS
		cfg.Storage.Path = t.TempDir()
		cfg.Cluster = config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1], ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2}
		srv, err := New(cfg, testLogger(t), withPeerListener(peerListeners[name]))
		if err != nil {
			t.Fatal(err)
		}
		servers[name] = srv
	}
	cancels := make(map[string]context.CancelFunc, len(names))
	done := make(map[string]chan error, len(names))
	for _, name := range names {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancels[name] = cancel
		done[name] = make(chan error, 1)
		srv := servers[name]
		ch := done[name]
		go func() { ch <- srv.Serve(ctx, ln) }()
	}
	stop := func(name string) {
		t.Helper()
		cancel := cancels[name]
		if cancel == nil {
			return
		}
		cancel()
		cancels[name] = nil
		select {
		case err := <-done[name]:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("%s server stopped: %v", name, err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("%s server did not stop", name)
		}
	}
	t.Cleanup(func() {
		for _, name := range names {
			stop(name)
		}
	})
	for _, name := range names {
		select {
		case <-servers[name].Ready():
		case err := <-done[name]:
			t.Fatalf("%s startup: %v", name, err)
		case <-time.After(15 * time.Second):
			t.Fatalf("%s did not become ready", name)
		}
	}
	if r := do(t, "PUT", servers["a"].URL()+"/three-node", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("create bucket: %d %s", r.status, r.body)
	}
	status := servers["a"].clusterNode.Status(context.Background())
	if !status.Available || status.LeaderID == "" {
		t.Fatalf("healthy three-node status: %+v", status)
	}
	survivors := make([]string, 0, 2)
	for _, name := range names {
		if name != status.LeaderID {
			survivors = append(survivors, name)
		}
	}
	key := ""
	for i := range 1000 {
		candidate := fmt.Sprintf("placed-%d", i)
		placed, err := cluster.SelectReplicas(clusterID, "three-node", candidate, names, 2)
		if err != nil {
			t.Fatal(err)
		}
		if (placed[0] == survivors[0] || placed[1] == survivors[0]) && (placed[0] == survivors[1] || placed[1] == survivors[1]) {
			key = candidate
			break
		}
	}
	if key == "" {
		t.Fatal("no key placed on the two surviving nodes")
	}
	stop(status.LeaderID)
	deadline := time.Now().Add(25 * time.Second)
	for {
		r := do(t, "PUT", servers[survivors[0]].URL()+"/three-node/"+key, []byte("two durable copies"), &rootCreds, nil)
		if r.status == 200 {
			break
		}
		if r.status != 503 || time.Now().After(deadline) {
			t.Fatalf("two-of-three write after leader loss: %d %s", r.status, r.body)
		}
		time.Sleep(150 * time.Millisecond)
	}
	for _, name := range survivors {
		if r := do(t, "GET", servers[name].URL()+"/three-node/"+key, nil, &rootCreds, nil); r.status != 200 || string(r.body) != "two durable copies" {
			t.Fatalf("read from surviving %s: %d %s", name, r.status, r.body)
		}
	}
	stop(survivors[1])
	if r := do(t, "PUT", servers[survivors[0]].URL()+"/three-node/no-quorum", []byte("must fail"), &rootCreds, nil); r.status != 503 || !strings.Contains(string(r.body), "<Code>ServiceUnavailable</Code>") {
		t.Fatalf("write after quorum loss: %d %s", r.status, r.body)
	}
}
