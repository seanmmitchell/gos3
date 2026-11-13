// Package cluster contains the admission and consensus primitives for a
// distributed gos3 cluster.
package cluster

import (
	"errors"
	"fmt"
)

// ErrInsufficientAcks means a mutation has not met its configured admission
// policy. Callers must not publish it or return success to an S3 client.
var ErrInsufficientAcks = errors.New("cluster: insufficient write acknowledgments")

// AckPolicy is the minimum number of distinct nodes that must accept a full
// write in memory and commit it to persistent storage. A durable ack also
// counts as a memory ack from that node.
type AckPolicy struct {
	Memory  int
	Durable int
}

// Validate rejects a policy that cannot be satisfied by the selected nodes.
func (p AckPolicy) Validate(replicas int) error {
	if replicas < 2 || p.Memory < 1 || p.Memory > replicas || p.Durable < 1 || p.Durable > replicas {
		return fmt.Errorf("cluster: invalid acknowledgement policy (replicas=%d, memory=%d, durable=%d)", replicas, p.Memory, p.Durable)
	}
	return nil
}

// Acks tracks one write. It is owned by the write coordinator, which calls
// Record only after validating the complete payload and the node identity.
type Acks struct {
	policy AckPolicy
	nodes  map[string]bool // true means a persistent commit was acknowledged
}

func NewAcks(policy AckPolicy, replicas int) (*Acks, error) {
	if err := policy.Validate(replicas); err != nil {
		return nil, err
	}
	return &Acks{policy: policy, nodes: make(map[string]bool, replicas)}, nil
}

// Record is idempotent. A durable acknowledgement upgrades an earlier memory
// acknowledgement from the same node; duplicates never increase either count.
func (a *Acks) Record(nodeID string, durable bool) error {
	if nodeID == "" {
		return errors.New("cluster: empty acknowledgement node ID")
	}
	a.nodes[nodeID] = a.nodes[nodeID] || durable
	return nil
}

func (a *Acks) Counts() (memory, durable int) {
	for _, persisted := range a.nodes {
		memory++
		if persisted {
			durable++
		}
	}
	return memory, durable
}

func (a *Acks) Satisfied() bool {
	memory, durable := a.Counts()
	return memory >= a.policy.Memory && durable >= a.policy.Durable
}

func (a *Acks) Require() error {
	if a.Satisfied() {
		return nil
	}
	memory, durable := a.Counts()
	return fmt.Errorf("%w: memory %d/%d, durable %d/%d", ErrInsufficientAcks, memory, a.policy.Memory, durable, a.policy.Durable)
}
