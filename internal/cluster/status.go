package cluster

import (
	"context"
	"time"

	"go.etcd.io/raft/v3"
)

// Status is an authenticated operator view of one cluster node. Peer match
// indexes are reported only by the leader, which owns Raft's progress map.
type Status struct {
	ClusterID      string               `json:"clusterId"`
	NodeID         string               `json:"nodeId"`
	LeaderID       string               `json:"leaderId,omitempty"`
	Leader         bool                 `json:"leader"`
	Available      bool                 `json:"available"`
	Error          string               `json:"error,omitempty"`
	CommitIndex    uint64               `json:"commitIndex"`
	AppliedIndex   uint64               `json:"appliedIndex"`
	Lag            uint64               `json:"lag"`
	ReplicaCount   int                  `json:"replicaCount"`
	MinMemoryAcks  int                  `json:"minMemoryAcks"`
	MinDurableAcks int                  `json:"minDurableAcks"`
	LastWrite      AcknowledgmentStatus `json:"lastWrite"`
	LastRepair     ReplicaRepairStatus  `json:"lastRepair"`
	Peers          []PeerStatus         `json:"peers"`
}

// AcknowledgmentStatus is the latest write coordinated through this node.
// Counts are distinct replicas. It is diagnostic state, not a durable log.
type AcknowledgmentStatus struct {
	OperationID        string    `json:"operationId,omitempty"`
	MemoryAcks         int       `json:"memoryAcks"`
	DurableAcks        int       `json:"durableAcks"`
	ConsensusCommitted bool      `json:"consensusCommitted"`
	Error              string    `json:"error,omitempty"`
	UpdatedAt          time.Time `json:"updatedAt,omitempty"`
}

// ReplicaRepairStatus reports the most recent local placement scan.
type ReplicaRepairStatus struct {
	Repaired  int       `json:"repaired"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
}

func (n *Node) recordRepair(repaired int, err error) {
	n.repairMu.Lock()
	defer n.repairMu.Unlock()
	n.lastRepair = ReplicaRepairStatus{Repaired: repaired, CheckedAt: time.Now().UTC()}
	if err != nil {
		n.lastRepair.Error = err.Error()
	}
}

func (n *Node) beginAcknowledgments(id string) uint64 {
	n.ackMu.Lock()
	defer n.ackMu.Unlock()
	n.ackSeq++
	n.lastAck = AcknowledgmentStatus{OperationID: id, UpdatedAt: time.Now().UTC()}
	return n.ackSeq
}

func (n *Node) updateAcknowledgments(sequence uint64, memory, durable int) {
	n.ackMu.Lock()
	defer n.ackMu.Unlock()
	if sequence == n.ackSeq {
		n.lastAck.MemoryAcks, n.lastAck.DurableAcks = memory, durable
		n.lastAck.UpdatedAt = time.Now().UTC()
	}
}

func (n *Node) finishAcknowledgments(sequence uint64, err error) {
	n.ackMu.Lock()
	defer n.ackMu.Unlock()
	if sequence == n.ackSeq {
		n.lastAck.ConsensusCommitted = err == nil
		if err != nil {
			n.lastAck.Error = err.Error()
		}
		n.lastAck.UpdatedAt = time.Now().UTC()
	}
}

type PeerStatus struct {
	ID            string `json:"id"`
	Local         bool   `json:"local"`
	Leader        bool   `json:"leader"`
	MatchIndex    uint64 `json:"matchIndex,omitempty"`
	Lag           uint64 `json:"lag,omitempty"`
	ProgressKnown bool   `json:"progressKnown"`
}

// Status checks whether a quorum-confirmed read is possible, then reports
// local Raft progress and the configured placement/acknowledgment policy.
func (n *Node) Status(ctx context.Context) Status {
	state := n.consensus.Status()
	commit := state.HardState.GetCommit()
	out := Status{
		ClusterID: n.cfg.ID, NodeID: n.cfg.NodeID,
		Leader:      state.RaftState == raft.StateLeader,
		CommitIndex: commit, AppliedIndex: state.Applied,
		ReplicaCount:  n.cfg.ReplicaCount,
		MinMemoryAcks: n.cfg.MinMemoryAcks, MinDurableAcks: n.cfg.MinDurableAcks,
		Peers: make([]PeerStatus, 0, len(n.cfg.Peers)),
	}
	if commit > state.Applied {
		out.Lag = commit - state.Applied
	}
	n.ackMu.Lock()
	out.LastWrite = n.lastAck
	n.ackMu.Unlock()
	n.repairMu.Lock()
	out.LastRepair = n.lastRepair
	n.repairMu.Unlock()
	for _, peer := range n.cfg.Peers {
		id := RaftID(n.cfg.ID, peer.ID)
		if id == state.Lead {
			out.LeaderID = peer.ID
		}
		entry := PeerStatus{ID: peer.ID, Local: peer.ID == n.cfg.NodeID, Leader: id == state.Lead}
		if progress, ok := state.Progress[id]; ok {
			entry.MatchIndex = progress.Match
			entry.ProgressKnown = true
			if commit > progress.Match {
				entry.Lag = commit - progress.Match
			}
		}
		out.Peers = append(out.Peers, entry)
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := n.ReadBarrier(probe); err != nil {
		out.Error = err.Error()
	} else {
		out.Available = true
	}
	return out
}
