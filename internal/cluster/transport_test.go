package cluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

func peerTestCertificates(t *testing.T, clusterID string, names ...string) (string, map[string][2]string) {
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
		uri, err := url.Parse(peerIdentity(clusterID, name))
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, URIs: []*url.URL{uri}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, key.Public(), caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPath, keyPath := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
		if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = [2]string{certPath, keyPath}
	}
	return caPath, files
}

func TestPeerTransportAuthenticatesBothDirections(t *testing.T) {
	const clusterID = "transport-test"
	ca, certs := peerTestCertificates(t, clusterID, "node-a", "node-b", "intruder")
	peers := []config.ClusterPeer{{ID: "node-a", URL: "https://127.0.0.1:1"}, {ID: "node-b", URL: "https://127.0.0.1:2"}}
	makeTransport := func(name string, step func(context.Context, *pb.Message) error) *PeerTransport {
		t.Helper()
		tr, err := NewPeerTransport(config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1]}, step)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	a := makeTransport("node-a", func(context.Context, *pb.Message) error { return nil })
	received := make(chan *pb.Message, 1)
	b := makeTransport("node-b", func(_ context.Context, msg *pb.Message) error { received <- msg; return nil })
	stages, err := OpenStageStore(filepath.Join(t.TempDir(), "node-b-stage.db"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer stages.Close()
	b.stage = stages
	server := httptest.NewUnstartedServer(b.Handler())
	server.TLS = b.ServerTLSConfig()
	// httptest chooses a port before StartTLS; update the destination address.
	server.StartTLS()
	defer server.Close()
	peer := a.byID[b.self]
	peer.origin = server.URL
	peer.url = server.URL + "/raft"
	a.byID[b.self] = peer

	msg := &pb.Message{Type: pb.MessageType_MsgHeartbeat.Enum(), From: proto.Uint64(a.self), To: proto.Uint64(b.self), Term: proto.Uint64(1)}
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got.GetFrom() != a.self || got.GetTo() != b.self {
			t.Fatalf("wrong message: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("peer did not receive authenticated message")
	}
	replica, err := a.Replica("node-b")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("remote payload"))
	if err := replica.Stage(context.Background(), "remote-op", []byte("remote payload"), digest); err != nil {
		t.Fatal(err)
	}
	if err := replica.Persist(context.Background(), "remote-op"); err != nil {
		t.Fatal(err)
	}
	stored, err := stages.Read(context.Background(), "remote-op")
	if err != nil || string(stored) != "remote payload" {
		t.Fatalf("remote durable payload = %q, %v", stored, err)
	}

	spoof := proto.Clone(msg).(*pb.Message)
	spoof.From = proto.Uint64(b.self)
	data, err := proto.Marshal(spoof)
	if err != nil {
		t.Fatal(err)
	}
	res, err := peer.client.Post(peer.url, "application/x-protobuf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("spoofed sender accepted: HTTP %d", res.StatusCode)
	}

	unknown := makeTransport("node-a", func(context.Context, *pb.Message) error { return nil })
	otherIdentity, err := tls.LoadX509KeyPair(certs["intruder"][0], certs["intruder"][1])
	if err != nil {
		t.Fatal(err)
	}
	unknownPeer := unknown.byID[b.self]
	unknownPeer.url = server.URL + "/raft"
	unknownPeer.client.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{otherIdentity}
	unknown.byID[b.self] = unknownPeer
	res, err = unknownPeer.client.Post(unknownPeer.url, "application/x-protobuf", bytes.NewReader(mustMarshalMessage(t, msg)))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("unconfigured certificate accepted: HTTP %d", res.StatusCode)
	}
	select {
	case <-received:
		t.Fatal("rejected message reached consensus")
	default:
	}
}

func TestPeerTransportRejectsWrongLocalIdentity(t *testing.T) {
	ca, certs := peerTestCertificates(t, "actual", "a")
	_, err := NewPeerTransport(config.ClusterConfig{Enabled: true, ID: "expected", NodeID: "a", Peers: []config.ClusterPeer{{ID: "a", URL: "https://localhost:1"}, {ID: "b", URL: "https://localhost:2"}}, TLSCA: ca, TLSCert: certs["a"][0], TLSKey: certs["a"][1]}, func(context.Context, *pb.Message) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "local certificate") {
		t.Fatalf("expected local identity error, got %v", err)
	}
}

func TestConsensusAcrossMutualTLSPeers(t *testing.T) {
	const clusterID = "raft-transport"
	ca, certs := peerTestCertificates(t, clusterID, "a", "b")
	servers := make(map[string]*httptest.Server)
	transports := make(map[string]*PeerTransport)
	nodes := make(map[string]*Consensus)
	stages := make(map[string]*StageStore)
	for _, name := range []string{"a", "b"} {
		name := name
		servers[name] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			transports[name].Handler().ServeHTTP(w, r)
		}))
	}
	defer func() {
		for _, server := range servers {
			server.Close()
		}
	}()
	peers := []config.ClusterPeer{
		{ID: "a", URL: "https://" + servers["a"].Listener.Addr().String()},
		{ID: "b", URL: "https://" + servers["b"].Listener.Addr().String()},
	}
	for _, name := range []string{"a", "b"} {
		name := name
		stage, err := OpenStageStore(filepath.Join(t.TempDir(), name+"-stage.db"), 1024)
		if err != nil {
			t.Fatal(err)
		}
		defer stage.Close()
		stages[name] = stage
		tr, err := NewPeerTransportWithStages(config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1]}, func(ctx context.Context, msg *pb.Message) error {
			return nodes[name].Step(ctx, msg)
		}, stage)
		if err != nil {
			t.Fatal(err)
		}
		transports[name] = tr
		servers[name].TLS = tr.ServerTLSConfig()
	}
	for _, name := range []string{"a", "b"} {
		servers[name].StartTLS()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	applied := make(map[string]chan string)
	for _, name := range []string{"a", "b"} {
		name := name
		applied[name] = make(chan string, 1)
		tr := transports[name]
		node, err := OpenConsensus(filepath.Join(t.TempDir(), name+".db"), tr.SelfID(), tr.Voters(), tr.Send, func(_ uint64, command []byte) error {
			applied[name] <- string(command)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		nodes[name] = node
	}
	done := make(chan error, 2)
	for _, node := range nodes {
		go func() { done <- node.Run(ctx) }()
	}
	defer func() {
		cancel()
		for range nodes {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("consensus stopped: %v", err)
			}
		}
		for _, node := range nodes {
			if err := node.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	var leader *Consensus
	deadline := time.Now().Add(8 * time.Second)
	for leader == nil && time.Now().Before(deadline) {
		for _, node := range nodes {
			if node.Status().RaftState == raft.StateLeader {
				leader = node
				break
			}
		}
		if leader == nil {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if leader == nil {
		t.Fatal("mTLS peers did not elect a leader")
	}
	writeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	var leaderTransport *PeerTransport
	for _, tr := range transports {
		if tr.SelfID() == leader.id {
			leaderTransport = tr
		}
	}
	replicas := make(map[string]ReplicaStage)
	for _, name := range []string{"a", "b"} {
		replica, err := leaderTransport.Replica(name)
		if err != nil {
			t.Fatal(err)
		}
		replicas[name] = replica
	}
	var operationID [16]byte
	operationID[0] = 42
	if err := AdmitProposal(writeCtx, operationID, []byte("replicated bytes"), []byte("committed"), []string{"a", "b"}, AckPolicy{Memory: 2, Durable: 2}, replicas, leader); err != nil {
		t.Fatal(err)
	}
	for name, node := range nodes {
		if err := node.ReadBarrier(writeCtx); err != nil {
			t.Fatalf("node %s read barrier: %v", name, err)
		}
		select {
		case got := <-applied[name]:
			if got != "committed" {
				t.Fatalf("node %s applied %q", name, got)
			}
		default:
			t.Fatalf("node %s did not apply committed write", name)
		}
		stored, err := stages[name].Read(writeCtx, hex.EncodeToString(operationID[:]))
		if err != nil || string(stored) != "replicated bytes" {
			t.Fatalf("node %s durable bytes = %q, %v", name, stored, err)
		}
	}
}

func mustMarshalMessage(t *testing.T, msg *pb.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
