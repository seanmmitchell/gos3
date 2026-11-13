package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// A stage lease outlives every admission that could count its memory ack.
// Expiry also reclaims stages orphaned by a coordinator crash or lost cleanup.
const maxAdmissionDuration = 5 * time.Minute
const stageLeaseDuration = 2 * maxAdmissionDuration
const replicaCleanupTimeout = 3 * time.Second

// SelectReplicas uses rendezvous hashing to assign an object key to a stable
// ordered set of voting nodes. Peer list order does not affect placement.
func SelectReplicas(clusterID, bucket, key string, peers []string, count int) ([]string, error) {
	if clusterID == "" || bucket == "" || len(peers) < 2 || count < 2 || count > len(peers) {
		return nil, errors.New("cluster: invalid replica placement inputs")
	}
	type scored struct {
		id   string
		hash [32]byte
	}
	seen := make(map[string]bool, len(peers))
	ordered := make([]scored, 0, len(peers))
	for _, id := range peers {
		if id == "" || seen[id] {
			return nil, errors.New("cluster: placement peers must be distinct and nonempty")
		}
		seen[id] = true
		ordered = append(ordered, scored{id: id, hash: sha256.Sum256([]byte(clusterID + "\x00" + bucket + "\x00" + key + "\x00" + id))})
	}
	slices.SortFunc(ordered, func(a, b scored) int {
		if cmp := slices.Compare(b.hash[:], a.hash[:]); cmp != 0 {
			return cmp
		}
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})
	result := make([]string, count)
	for i := range result {
		result[i] = ordered[i].id
	}
	return result, nil
}

// ReplicaStage is a trusted, authenticated node. Stage must return only when
// the complete bytes have been checked against digest and retained in memory.
// Persist must return only when that same operation is committed to an
// fsync-enabled filesystem or an acknowledged S3 backing.
type ReplicaStage interface {
	Stage(context.Context, string, []byte, [32]byte) error
	Persist(context.Context, string) error
	// Release frees only volatile staging; it must never delete durable bytes.
	Release(context.Context, string) error
}

// AdmitWrite stages the exact payload on selected distinct nodes, records
// durable acknowledgments only after a successful Persist, and invokes
// publish only after both thresholds are met. The caller must publish through
// consensus; returning from publish is the earliest point for a write 200.
func AdmitWrite(ctx context.Context, operationID string, payload []byte, selected []string, policy AckPolicy, nodes map[string]ReplicaStage, publish func(context.Context) error) error {
	return admitWrite(ctx, operationID, payload, selected, policy, nodes, publish, nil)
}

// progress runs in the coordinator goroutine after each distinct-node count
// changes. It is for operator diagnostics; only a successful publish allows
// the caller to acknowledge the client write.
func admitWrite(ctx context.Context, operationID string, payload []byte, selected []string, policy AckPolicy, nodes map[string]ReplicaStage, publish func(context.Context) error, progress func(memory, durable int)) error {
	if operationID == "" || publish == nil {
		return errors.New("cluster: write admission needs an operation ID and consensus publish function")
	}
	acks, err := NewAcks(policy, len(selected))
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(selected))
	for _, id := range selected {
		if id == "" || seen[id] || nodes[id] == nil {
			return errors.New("cluster: selected replica nodes must be distinct and available")
		}
		seen[id] = true
	}
	digest := sha256.Sum256(payload)
	ctx, cancelAdmission := context.WithTimeout(ctx, maxAdmissionDuration)
	defer cancelAdmission()
	stageCtx, cancelStages := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancelStages()
		// Wait for cleanup on reachable replicas before a caller retries, but
		// do not let an unreachable peer delay the acknowledgment indefinitely.
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		timer := time.NewTimer(replicaCleanupTimeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	}()
	type result struct {
		id      string
		durable bool
		err     error
	}
	// A replica can report memory and durable acceptance separately. Buffer
	// both reports so the coordinator may return once its policy is met while
	// a slower or failed third replica is still finishing.
	results := make(chan result, 2*len(selected))
	for _, id := range selected {
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			defer func() {
				// Keep memory-only acknowledgments valid through publication,
				// even if this replica's durable backing failed. Stage may have
				// succeeded remotely even when its response was lost.
				<-stageCtx.Done()
				cleanup, cancel := context.WithTimeout(context.Background(), replicaCleanupTimeout)
				defer cancel()
				_ = nodes[id].Release(cleanup, operationID)
			}()
			if err := nodes[id].Stage(stageCtx, operationID, payload, digest); err != nil {
				results <- result{id: id, err: err}
				return
			}
			results <- result{id: id}
			if err := nodes[id].Persist(stageCtx, operationID); err != nil {
				results <- result{id: id, err: err}
				return
			}
			results <- result{id: id, durable: true}
		}(id)
	}
	remaining := len(selected)
	for remaining > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r := <-results:
			if r.err != nil {
				remaining--
				continue
			}
			if err := acks.Record(r.id, r.durable); err != nil {
				return err
			}
			if progress != nil {
				memory, durable := acks.Counts()
				progress(memory, durable)
			}
			if r.durable {
				remaining--
			}
			if acks.Satisfied() {
				if err := ctx.Err(); err != nil {
					return err
				}
				return publish(ctx)
			}
		}
	}
	return fmt.Errorf("operation %s: %w", operationID, acks.Require())
}

// AdmitProposal binds replica staging and the Raft command to the same
// operation ID. A retry with that ID cannot publish different command bytes.
func AdmitProposal(ctx context.Context, id [16]byte, payload, command []byte, selected []string, policy AckPolicy, nodes map[string]ReplicaStage, consensus *Consensus) error {
	return admitProposal(ctx, id, payload, command, selected, policy, nodes, consensus, nil)
}

func admitProposal(ctx context.Context, id [16]byte, payload, command []byte, selected []string, policy AckPolicy, nodes map[string]ReplicaStage, consensus *Consensus, progress func(memory, durable int)) error {
	if consensus == nil {
		return errors.New("cluster: consensus is unavailable")
	}
	return admitWrite(ctx, hex.EncodeToString(id[:]), payload, selected, policy, nodes, func(ctx context.Context) error {
		return consensus.ProposeWithID(ctx, id, command)
	}, progress)
}
