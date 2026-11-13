package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

const maxRaftMessageBytes = 8 << 20

// PeerTransport accepts Raft messages only from a configured node whose
// certificate binds its node ID to the cluster ID. HTTPS also authenticates
// the destination, preventing a stale or misconfigured address from receiving
// cluster messages.
type PeerTransport struct {
	clusterID         string
	importFingerprint string
	self              uint64
	byID              map[uint64]peerClient
	serverTLS         *tls.Config
	step              func(context.Context, *pb.Message) error
	stage             DurableReplicaStore
}

type peerClient struct {
	name   string
	origin string
	url    string
	client *http.Client
}

// RaftID maps a stable textual node ID to a nonzero Raft ID. The full peer
// list is checked for collisions when a transport is constructed.
func RaftID(clusterID, nodeID string) uint64 {
	hash := sha256.Sum256([]byte(clusterID + "\x00" + nodeID))
	id := binary.BigEndian.Uint64(hash[:8])
	if id == 0 {
		return binary.BigEndian.Uint64(hash[8:16]) | 1
	}
	return id
}

func peerIdentity(clusterID, nodeID string) string {
	return "spiffe://gos3/" + clusterID + "/" + nodeID
}

func hasIdentity(cert *x509.Certificate, clusterID, nodeID string) bool {
	if cert == nil || len(cert.URIs) != 1 {
		return false
	}
	return cert.URIs[0].String() == peerIdentity(clusterID, nodeID)
}

// NewPeerTransport loads a static, mutually authenticated peer set. The
// caller supplies the consensus Step method and serves Handler on the local
// peer origin using ServerTLSConfig.
func NewPeerTransport(cfg config.ClusterConfig, step func(context.Context, *pb.Message) error) (*PeerTransport, error) {
	return NewPeerTransportWithStages(cfg, step, nil)
}

// NewPeerTransportWithStages also enables authenticated replica staging RPCs.
func NewPeerTransportWithStages(cfg config.ClusterConfig, step func(context.Context, *pb.Message) error, stage DurableReplicaStore) (*PeerTransport, error) {
	if !cfg.Enabled || len(cfg.Peers) < 2 || cfg.ID == "" || cfg.NodeID == "" || step == nil {
		return nil, errors.New("cluster: peer transport needs an enabled cluster, two peers and a step function")
	}
	identity, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("cluster: loading peer certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(identity.Certificate[0])
	if err != nil || !hasIdentity(leaf, cfg.ID, cfg.NodeID) {
		return nil, errors.New("cluster: local certificate has no matching cluster and node URI identity")
	}
	caPEM, err := os.ReadFile(cfg.TLSCA)
	if err != nil {
		return nil, fmt.Errorf("cluster: reading peer CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("cluster: peer CA contains no certificates")
	}
	t := &PeerTransport{
		clusterID: cfg.ID,
		self:      RaftID(cfg.ID, cfg.NodeID),
		byID:      make(map[uint64]peerClient, len(cfg.Peers)),
		step:      step,
		stage:     stage,
		serverTLS: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert},
	}
	seenNames := make(map[string]bool, len(cfg.Peers))
	for _, peer := range cfg.Peers {
		parsed, err := url.Parse(peer.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || peer.ID == "" {
			return nil, fmt.Errorf("cluster: invalid peer origin for %q", peer.ID)
		}
		if seenNames[peer.ID] {
			return nil, fmt.Errorf("cluster: duplicate peer ID %q", peer.ID)
		}
		seenNames[peer.ID] = true
		id := RaftID(cfg.ID, peer.ID)
		if _, exists := t.byID[id]; exists {
			return nil, errors.New("cluster: peer IDs collide in Raft ID mapping")
		}
		if peer.ID == cfg.NodeID && id != t.self {
			return nil, errors.New("cluster: local peer ID mismatch")
		}
		peerName := peer.ID
		clientTLS := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{identity},
			RootCAs:      pool,
			ServerName:   parsed.Hostname(),
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 || !hasIdentity(state.PeerCertificates[0], cfg.ID, peerName) {
					return errors.New("cluster: destination certificate does not match configured peer ID")
				}
				return nil
			},
		}
		t.byID[id] = peerClient{
			name:   peer.ID,
			origin: peer.URL,
			url:    peer.URL + "/raft",
			client: &http.Client{Transport: &http.Transport{
				TLSClientConfig:     clientTLS,
				MaxIdleConnsPerHost: 2,
			}, Timeout: 3 * time.Second},
		}
	}
	if !seenNames[cfg.NodeID] {
		return nil, errors.New("cluster: local node is not in the peer set")
	}
	return t, nil
}

func (t *PeerTransport) SelfID() uint64 { return t.self }

func (t *PeerTransport) Voters() []uint64 {
	voters := make([]uint64, 0, len(t.byID))
	for id := range t.byID {
		voters = append(voters, id)
	}
	slices.Sort(voters)
	return voters
}

func (t *PeerTransport) ServerTLSConfig() *tls.Config { return t.serverTLS.Clone() }

func (t *PeerTransport) Send(ctx context.Context, msg *pb.Message) error {
	if msg == nil || msg.GetFrom() != t.self || msg.GetTo() == t.self {
		return errors.New("cluster: invalid outbound Raft sender or destination")
	}
	peer, ok := t.byID[msg.GetTo()]
	if !ok {
		return errors.New("cluster: outbound Raft destination is not a configured peer")
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if len(data) > maxRaftMessageBytes {
		return errors.New("cluster: Raft message exceeds transport limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Gos3-Import-Fingerprint", t.importFingerprint)
	res, err := peer.client.Do(req)
	if err != nil {
		return fmt.Errorf("cluster: sending Raft message to %s: %w", peer.name, err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1024))
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cluster: peer %s rejected Raft message with HTTP %d", peer.name, res.StatusCode)
	}
	return nil
}

func (t *PeerTransport) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "peer certificate required", http.StatusForbidden)
			return
		}
		if r.Header.Get("X-Gos3-Import-Fingerprint") != t.importFingerprint {
			http.Error(w, "cluster import fingerprint mismatch", http.StatusConflict)
			return
		}
		if r.URL.Path == "/stage" || r.URL.Path == "/persist" || r.URL.Path == "/payload" || r.URL.Path == "/release" {
			t.handleStage(w, r)
			return
		}
		if r.URL.Path != "/raft" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRaftMessageBytes))
		if err != nil {
			http.Error(w, "invalid Raft message", http.StatusBadRequest)
			return
		}
		var msg pb.Message
		if err := proto.Unmarshal(body, &msg); err != nil || msg.GetTo() != t.self {
			http.Error(w, "invalid Raft message", http.StatusBadRequest)
			return
		}
		peer, ok := t.byID[msg.GetFrom()]
		if !ok || msg.GetFrom() == t.self || !hasIdentity(r.TLS.PeerCertificates[0], t.clusterID, peer.name) {
			http.Error(w, "peer identity mismatch", http.StatusForbidden)
			return
		}
		if err := t.step(r.Context(), &msg); err != nil {
			http.Error(w, "Raft message unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (t *PeerTransport) authenticatedMember(cert *x509.Certificate) bool {
	for id, peer := range t.byID {
		if id != t.self && hasIdentity(cert, t.clusterID, peer.name) {
			return true
		}
	}
	return false
}

func (t *PeerTransport) handleStage(w http.ResponseWriter, r *http.Request) {
	if t.stage == nil {
		http.NotFound(w, r)
		return
	}
	if !t.authenticatedMember(r.TLS.PeerCertificates[0]) {
		http.Error(w, "peer identity mismatch", http.StatusForbidden)
		return
	}
	id := r.Header.Get("X-Gos3-Operation-ID")
	if id == "" || len(id) > 128 {
		http.Error(w, "invalid operation ID", http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/release" {
		if err := t.stage.Release(r.Context(), id); err != nil {
			http.Error(w, "replica stage cleanup unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/payload" {
		payload, err := t.stage.Read(r.Context(), id)
		if err != nil {
			http.Error(w, "replica payload unavailable", http.StatusNotFound)
			return
		}
		digest := sha256.Sum256(payload)
		w.Header().Set("X-Gos3-SHA256", hex.EncodeToString(digest[:]))
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
		return
	}
	if r.URL.Path == "/persist" {
		if err := t.stage.Persist(r.Context(), id); err != nil {
			http.Error(w, "replica durable commit unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rawDigest, err := hex.DecodeString(r.Header.Get("X-Gos3-SHA256"))
	if err != nil || len(rawDigest) != sha256.Size {
		http.Error(w, "invalid payload digest", http.StatusBadRequest)
		return
	}
	var digest [32]byte
	copy(digest[:], rawDigest)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, t.stage.MaxMemoryBytes()))
	if err != nil {
		http.Error(w, "invalid staged payload", http.StatusBadRequest)
		return
	}
	if err := t.stage.Stage(r.Context(), id, body, digest); err != nil {
		http.Error(w, "replica memory stage unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Replica returns a local or remote staging endpoint for a configured voter.
func (t *PeerTransport) Replica(nodeID string) (ReplicaStage, error) {
	id := RaftID(t.clusterID, nodeID)
	peer, ok := t.byID[id]
	if !ok || peer.name != nodeID {
		return nil, errors.New("cluster: replica is not a configured voter")
	}
	if id == t.self {
		if t.stage == nil {
			return nil, errors.New("cluster: local staging store unavailable")
		}
		return t.stage, nil
	}
	return remoteReplica{peer: peer, importFingerprint: t.importFingerprint}, nil
}

type remoteReplica struct {
	peer              peerClient
	importFingerprint string
}

func (r remoteReplica) request(ctx context.Context, path, id string, data []byte, digest *[32]byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.peer.origin+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("X-Gos3-Operation-ID", id)
	// These operations are idempotent under their operation ID. Let the
	// transport replay them if a reused connection loses its response.
	req.Header.Set("Idempotency-Key", id)
	req.Header.Set("X-Gos3-Import-Fingerprint", r.importFingerprint)
	if digest != nil {
		req.Header.Set("X-Gos3-SHA256", hex.EncodeToString(digest[:]))
	}
	client := &http.Client{Transport: r.peer.client.Transport}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1024))
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cluster: replica %s rejected %s with HTTP %d", r.peer.name, path, res.StatusCode)
	}
	return nil
}

func (r remoteReplica) Stage(ctx context.Context, id string, data []byte, digest [32]byte) error {
	return r.request(ctx, "/stage", id, data, &digest)
}

func (r remoteReplica) Persist(ctx context.Context, id string) error {
	return r.request(ctx, "/persist", id, nil, nil)
}

func (r remoteReplica) Release(ctx context.Context, id string) error {
	return r.request(ctx, "/release", id, nil, nil)
}

// ReadPayload fetches a durable replica over the mutually authenticated peer
// connection. The response digest is checked independently of HTTPS framing.
func (t *PeerTransport) ReadPayload(ctx context.Context, nodeID, id string) ([]byte, error) {
	if id == "" || len(id) > 128 {
		return nil, errors.New("cluster: invalid staging operation ID")
	}
	peerID := RaftID(t.clusterID, nodeID)
	peer, ok := t.byID[peerID]
	if !ok || peer.name != nodeID {
		return nil, errors.New("cluster: payload replica is not a configured voter")
	}
	if peerID == t.self {
		if t.stage == nil {
			return nil, errors.New("cluster: local staging store unavailable")
		}
		return t.stage.Read(ctx, id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer.origin+"/payload", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Gos3-Operation-ID", id)
	req.Header.Set("X-Gos3-Import-Fingerprint", t.importFingerprint)
	client := &http.Client{Transport: peer.client.Transport}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cluster: fetching payload from %s: %w", nodeID, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cluster: replica %s has no payload (HTTP %d)", nodeID, res.StatusCode)
	}
	if t.stage == nil {
		return nil, errors.New("cluster: local staging store unavailable")
	}
	rawDigest, err := hex.DecodeString(res.Header.Get("X-Gos3-SHA256"))
	if err != nil || len(rawDigest) != sha256.Size {
		return nil, errors.New("cluster: replica payload digest missing or invalid")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, t.stage.MaxMemoryBytes()+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > t.stage.MaxMemoryBytes() {
		return nil, ErrStageFull
	}
	digest := sha256.Sum256(data)
	if !bytes.Equal(digest[:], rawDigest) {
		return nil, errors.New("cluster: replica payload digest mismatch")
	}
	return data, nil
}
