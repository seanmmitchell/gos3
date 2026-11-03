package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"

	"crypto/tls"

	"github.com/seanmmitchell/gos3/internal/config"
	pb "go.etcd.io/raft/v3/raftpb"
)

// Node composes the durable state machine, Raft voter, authenticated peer
// transport, local payload staging and background replica repair.
type Node struct {
	cfg        config.ClusterConfig
	metadata   *MetadataStore
	stages     *StageStore
	payloads   DurableReplicaStore
	stageMax   int64
	transport  *PeerTransport
	consensus  *Consensus
	mu         sync.Mutex
	running    bool
	closed     bool
	closeOnce  sync.Once
	closeErr   error
	ackMu      sync.Mutex
	ackSeq     uint64
	lastAck    AcknowledgmentStatus
	repairMu   sync.Mutex
	lastRepair ReplicaRepairStatus
}

func OpenNode(cfg config.ClusterConfig, dir string, maxMemoryBytes int64) (*Node, error) {
	return openNode(cfg, dir, maxMemoryBytes, nil)
}

// OpenNodeWithReplicaStore uses a node-specific durable backing for object
// payloads while Raft and metadata remain on the local system directory.
func OpenNodeWithReplicaStore(cfg config.ClusterConfig, dir string, maxMemoryBytes int64, store DurableReplicaStore) (*Node, error) {
	if store == nil || store.MaxMemoryBytes() != maxMemoryBytes {
		return nil, errors.New("cluster: replica store must match the configured memory limit")
	}
	return openNode(cfg, dir, maxMemoryBytes, store)
}

func openNode(cfg config.ClusterConfig, dir string, maxMemoryBytes int64, store DurableReplicaStore) (*Node, error) {
	if !cfg.Enabled || dir == "" || maxMemoryBytes < 1 {
		return nil, errors.New("cluster: enabled node needs a persistent directory and positive staging memory limit")
	}
	policy := AckPolicy{Memory: cfg.MinMemoryAcks, Durable: cfg.MinDurableAcks}
	if cfg.ReplicaCount < 2 || cfg.ReplicaCount > len(cfg.Peers) {
		return nil, errors.New("cluster: invalid replica count")
	}
	if err := policy.Validate(cfg.ReplicaCount); err != nil {
		return nil, err
	}
	var stage *StageStore
	var err error
	if store == nil {
		stage, err = OpenStageStore(filepath.Join(dir, "stages.db"), maxMemoryBytes)
		if err != nil {
			return nil, err
		}
		store = stage
	}
	metadata, err := OpenMetadataStore(filepath.Join(dir, "metadata.db"))
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	node := &Node{cfg: cfg, metadata: metadata, stages: stage, payloads: store, stageMax: maxMemoryBytes}
	importFingerprint, _, err := metadata.Get(MetadataSettings, importFingerprintKey)
	if err != nil {
		_ = metadata.Close()
		_ = store.Close()
		return nil, err
	}
	transport, err := NewPeerTransportWithStages(cfg, func(ctx context.Context, msg *pb.Message) error {
		if node.consensus == nil {
			return errors.New("cluster: consensus not ready")
		}
		return node.consensus.Step(ctx, msg)
	}, store)
	if err != nil {
		_ = metadata.Close()
		_ = store.Close()
		return nil, err
	}
	transport.importFingerprint = string(importFingerprint)
	node.transport = transport
	consensus, err := OpenConsensusWithMetadata(filepath.Join(dir, "raft.db"), transport.SelfID(), transport.Voters(), transport.Send, metadata)
	if err != nil {
		_ = metadata.Close()
		_ = store.Close()
		return nil, err
	}
	node.consensus = consensus
	return node, nil
}

func (n *Node) Run(ctx context.Context) error {
	n.mu.Lock()
	if n.running || n.closed {
		n.mu.Unlock()
		return errors.New("cluster: node already running or closed")
	}
	n.running = true
	n.mu.Unlock()
	repairCtx, cancelRepair := context.WithCancel(ctx)
	repairDone := make(chan struct{})
	go func() {
		defer close(repairDone)
		n.repairLoop(repairCtx)
	}()
	err := n.consensus.Run(ctx)
	cancelRepair()
	<-repairDone
	return err
}

func (n *Node) PeerHandler() http.Handler             { return n.transport.Handler() }
func (n *Node) PeerTLSConfig() *tls.Config            { return n.transport.ServerTLSConfig() }
func (n *Node) Metadata() *MetadataStore              { return n.metadata }
func (n *Node) ReadBarrier(ctx context.Context) error { return n.consensus.ReadBarrier(ctx) }

func (n *Node) GetMetadata(ctx context.Context, ns MetadataNamespace, key string) ([]byte, bool, error) {
	if err := n.ReadBarrier(ctx); err != nil {
		return nil, false, err
	}
	return n.metadata.Get(ns, key)
}

func (n *Node) ListMetadata(ctx context.Context, ns MetadataNamespace, prefix string) ([]MetadataRecord, error) {
	if err := n.ReadBarrier(ctx); err != nil {
		return nil, err
	}
	return n.metadata.List(ns, prefix)
}

func (n *Node) HasMetadataPrefix(ctx context.Context, ns MetadataNamespace, prefix string) (bool, error) {
	if err := n.ReadBarrier(ctx); err != nil {
		return false, err
	}
	return n.metadata.HasPrefix(ns, prefix)
}

// ReadVerifiedPayload uses a quorum read barrier, then checks each replica
// against the committed digest before accepting or repairing a local copy.
func (n *Node) ReadVerifiedPayload(ctx context.Context, id [16]byte, bucket, key string, expected [32]byte, size int64) ([]byte, error) {
	if size < 0 {
		return nil, errors.New("cluster: invalid committed payload size")
	}
	if err := n.ReadBarrier(ctx); err != nil {
		return nil, err
	}
	peerIDs := make([]string, 0, len(n.cfg.Peers))
	for _, peer := range n.cfg.Peers {
		peerIDs = append(peerIDs, peer.ID)
	}
	selected, err := SelectReplicas(n.cfg.ID, bucket, key, peerIDs, n.cfg.ReplicaCount)
	if err != nil {
		return nil, err
	}
	opID := hex.EncodeToString(id[:])
	var failures []error
	for _, peer := range selected {
		payload, err := n.transport.ReadPayload(ctx, peer, opID)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", peer, err))
			continue
		}
		if int64(len(payload)) != size || sha256.Sum256(payload) != expected {
			failures = append(failures, fmt.Errorf("%s: replica payload differs from committed version", peer))
			continue
		}
		if peer != n.cfg.NodeID {
			for _, placement := range selected {
				if placement == n.cfg.NodeID {
					if err := n.payloads.RepairCommitted(ctx, opID, payload, expected); err != nil {
						return nil, err
					}
					break
				}
			}
		}
		return payload, nil
	}
	return nil, fmt.Errorf("%w for operation %s: %w", ErrVerifiedReplicaUnavailable, opID, errors.Join(failures...))
}

// AdmitMetadataWrite acknowledges only after the configured distinct-node
// staging and durability thresholds and a Raft commit have completed.
func (n *Node) AdmitMetadataWrite(ctx context.Context, id [16]byte, bucket, key string, payload []byte, command MetadataCommand) error {
	digest := sha256.Sum256(payload)
	command.PayloadSHA256 = hex.EncodeToString(digest[:])
	data, err := EncodeMetadataCommand(command)
	if err != nil {
		return err
	}
	return n.admitCommand(ctx, id, bucket, key, payload, data)
}

// AdmitTransactionWrite stages the bytes under the configured acknowledgment
// policy and commits a checked multi-record transaction through Raft. A
// failed condition is recorded durably and returned after the commit.
func (n *Node) AdmitTransactionWrite(ctx context.Context, id [16]byte, bucket, key string, payload []byte, transaction MetadataTransaction) error {
	if transaction.OperationID != hex.EncodeToString(id[:]) {
		return errors.New("cluster: metadata transaction ID differs from Raft operation ID")
	}
	digest := sha256.Sum256(payload)
	transaction.PayloadSHA256 = hex.EncodeToString(digest[:])
	data, err := EncodeMetadataTransaction(transaction)
	if err != nil {
		return err
	}
	if err := n.admitCommand(ctx, id, bucket, key, payload, data); err != nil {
		return err
	}
	applied, found, err := n.metadata.Outcome(transaction.OperationID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("cluster: committed metadata transaction has no durable outcome")
	}
	if !applied {
		return ErrMetadataCondition
	}
	return nil
}

func (n *Node) admitCommand(ctx context.Context, id [16]byte, bucket, key string, payload, data []byte) error {
	// An uncertain response may be retried after a peer fails. If this exact
	// command was already applied locally, its original acknowledgments and
	// consensus commit are sufficient; do not demand fresh replica copies.
	prior, err := n.consensus.store.operationApplied(id, sha256.Sum256(data))
	if err != nil {
		return err
	}
	if prior {
		return nil
	}
	peerIDs := make([]string, 0, len(n.cfg.Peers))
	for _, peer := range n.cfg.Peers {
		peerIDs = append(peerIDs, peer.ID)
	}
	selected, err := SelectReplicas(n.cfg.ID, bucket, key, peerIDs, n.cfg.ReplicaCount)
	if err != nil {
		return err
	}
	replicas := make(map[string]ReplicaStage, len(selected))
	for _, peer := range selected {
		replica, err := n.transport.Replica(peer)
		if err != nil {
			return err
		}
		replicas[peer] = replica
	}
	sequence := n.beginAcknowledgments(hex.EncodeToString(id[:]))
	err = admitProposal(ctx, id, payload, data, selected, AckPolicy{Memory: n.cfg.MinMemoryAcks, Durable: n.cfg.MinDurableAcks}, replicas, n.consensus, func(memory, durable int) {
		n.updateAcknowledgments(sequence, memory, durable)
	})
	n.finishAcknowledgments(sequence, err)
	if err != nil {
		return fmt.Errorf("cluster: admitting %s/%s operation %s: %w", bucket, key, hex.EncodeToString(id[:]), err)
	}
	return nil
}

func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		n.mu.Lock()
		running := n.running
		n.closed = true
		n.mu.Unlock()
		if running {
			n.closeErr = n.consensus.Close()
		} else {
			n.consensus.node.Stop()
			n.closeErr = n.consensus.store.close()
		}
		n.closeErr = errors.Join(n.closeErr, n.payloads.Close(), n.metadata.Close())
	})
	return n.closeErr
}
