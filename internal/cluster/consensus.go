package cluster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

// Consensus runs one voting member. The caller supplies a transport that
// authenticates the destination and an idempotent state-machine apply
// function. The committed index is saved after apply; an interrupted apply
// can be called again after restart.
type Consensus struct {
	id      uint64
	voters  []uint64
	node    raft.Node
	store   *raftStore
	send    func(context.Context, *pb.Message) error
	apply   func(uint64, []byte) error
	advance func(uint64) error
	restore func(uint64, []byte) error
	stopped chan struct{}
	mu      sync.Mutex
	pending map[[16]byte]chan error
	reads   map[[16]byte]chan uint64
	applied uint64
	notice  chan struct{}
	snaps   chan snapshotRequest
}

type snapshotRequest struct {
	make func(uint64) ([]byte, error)
	done chan error
}

// OpenConsensus loads a node's Raft log or bootstraps the fixed voter set.
// The caller must call Run once and Close when it finishes.
func OpenConsensus(path string, id uint64, voters []uint64, send func(context.Context, *pb.Message) error, apply func(uint64, []byte) error) (*Consensus, error) {
	return OpenConsensusWithSnapshots(path, id, voters, send, apply, nil)
}

// OpenConsensusWithSnapshots installs an idempotent state-machine restore
// hook. The hook must durably replace the application state at the snapshot
// index before returning; a crash may cause the same snapshot to be restored
// again. The application must persist state applied after a snapshot too.
func OpenConsensusWithSnapshots(path string, id uint64, voters []uint64, send func(context.Context, *pb.Message) error, apply func(uint64, []byte) error, restore func(uint64, []byte) error) (*Consensus, error) {
	if id == 0 || len(voters) < 2 || send == nil || apply == nil {
		return nil, errors.New("cluster: Raft needs a node ID, at least two voters, a transport and an apply function")
	}
	seen := make(map[uint64]bool, len(voters))
	for _, voter := range voters {
		if voter == 0 || seen[voter] {
			return nil, errors.New("cluster: voter IDs must be unique and nonzero")
		}
		seen[voter] = true
	}
	if !seen[id] {
		return nil, errors.New("cluster: this node must be a voter")
	}
	s, restart, err := openRaftStore(path)
	if err != nil {
		return nil, err
	}
	if err := s.bindNodeID(id); err != nil {
		_ = s.close()
		return nil, err
	}
	if err := s.bindVoters(voters); err != nil {
		_ = s.close()
		return nil, err
	}
	snapshot, err := s.memory.Snapshot()
	if err != nil {
		_ = s.close()
		return nil, err
	}
	if !raft.IsEmptySnap(snapshot) && !sameVoters(snapshot.GetMetadata().GetConfState(), voters) {
		_ = s.close()
		return nil, errors.New("cluster: persisted snapshot membership differs from fixed voters")
	}
	if !raft.IsEmptySnap(snapshot) && s.applied < snapshot.GetMetadata().GetIndex() {
		if restore == nil {
			_ = s.close()
			return nil, errors.New("cluster: persisted snapshot needs a state-machine restore hook")
		}
		if err := restore(snapshot.GetMetadata().GetIndex(), snapshot.GetData()); err != nil {
			_ = s.close()
			return nil, fmt.Errorf("cluster: restoring persisted snapshot: %w", err)
		}
		if err := s.markApplied(snapshot.GetMetadata().GetIndex(), snapshot.GetMetadata().GetConfState()); err != nil {
			_ = s.close()
			return nil, err
		}
	}
	cfg := &raft.Config{
		ID:              id,
		ElectionTick:    10,
		HeartbeatTick:   1,
		Storage:         s.memory,
		Applied:         s.applied,
		MaxSizePerMsg:   1 << 20,
		MaxInflightMsgs: 256,
		CheckQuorum:     true,
		PreVote:         true,
	}
	var node raft.Node
	if restart {
		// Fixed membership was persisted before the first Ready, even if
		// a crash prevented the bootstrap ConfChanges from being applied.
		node = raft.RestartNode(cfg)
	} else {
		peers := make([]raft.Peer, len(voters))
		for i, voter := range voters {
			peers[i] = raft.Peer{ID: voter}
		}
		node = raft.StartNode(cfg, peers)
	}
	return &Consensus{id: id, voters: slices.Clone(voters), node: node, store: s, send: send, apply: apply, restore: restore, stopped: make(chan struct{}), pending: make(map[[16]byte]chan error), reads: make(map[[16]byte]chan uint64), applied: s.applied, notice: make(chan struct{}), snaps: make(chan snapshotRequest)}, nil
}

// OpenConsensusWithMetadata binds Raft to its durable shared metadata. A
// missing or stale metadata DB must not silently serve an empty namespace.
// Metadata may be one or more committed entries ahead of Raft's applied
// marker after a crash between its fsync and markApplied; replay is safe.
func OpenConsensusWithMetadata(path string, id uint64, voters []uint64, send func(context.Context, *pb.Message) error, metadata *MetadataStore) (*Consensus, error) {
	if metadata == nil {
		return nil, errors.New("cluster: metadata store is required")
	}
	c, err := OpenConsensusWithSnapshots(path, id, voters, send, metadata.Apply, metadata.Restore)
	if err != nil {
		return nil, err
	}
	stateIndex, err := metadata.AppliedIndex()
	if err == nil {
		var hard *pb.HardState
		hard, _, err = c.store.memory.InitialState()
		if err == nil && (stateIndex < c.store.applied || stateIndex > hard.GetCommit()) {
			err = fmt.Errorf("cluster: metadata index %d disagrees with Raft applied %d and committed %d", stateIndex, c.store.applied, hard.GetCommit())
		}
	}
	if err != nil {
		c.node.Stop()
		_ = c.store.close()
		return nil, err
	}
	c.advance = metadata.Advance
	return c, nil
}

// Run ticks Raft, persists each Ready before sending its messages, then
// applies committed entries in order. A persistence or apply error stops
// this voting member instead of allowing it to serve stale state.
func (c *Consensus) Run(ctx context.Context) error {
	sendCtx, stopSending := context.WithCancel(ctx)
	outboxes := make(map[uint64]chan *pb.Message, len(c.voters)-1)
	var senders sync.WaitGroup
	for _, id := range c.voters {
		if id == c.id {
			continue
		}
		outbox := make(chan *pb.Message, 256)
		outboxes[id] = outbox
		senders.Add(1)
		go func(ch <-chan *pb.Message) {
			defer senders.Done()
			for {
				select {
				case <-sendCtx.Done():
					return
				case msg := <-ch:
					if err := c.send(sendCtx, msg); err != nil {
						if sendCtx.Err() != nil {
							return
						}
						c.node.ReportUnreachable(msg.GetTo())
						if msg.GetType() == pb.MessageType_MsgSnap {
							c.node.ReportSnapshot(msg.GetTo(), raft.SnapshotFailure)
						}
					} else if msg.GetType() == pb.MessageType_MsgSnap {
						c.node.ReportSnapshot(msg.GetTo(), raft.SnapshotFinish)
					}
				}
			}
		}(outbox)
	}
	defer func() {
		stopSending()
		senders.Wait()
		close(c.stopped)
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			c.node.Tick()
		case request := <-c.snaps:
			data, err := request.make(c.store.applied)
			if err == nil {
				err = c.store.createSnapshot(data)
			}
			request.done <- err
		case ready, ok := <-c.node.Ready():
			if !ok {
				return errors.New("cluster: Raft node stopped")
			}
			if !raft.IsEmptySnap(ready.Snapshot) && c.restore == nil {
				return errors.New("cluster: snapshot state-machine restoration is not configured")
			}
			if !raft.IsEmptySnap(ready.Snapshot) && !sameVoters(ready.Snapshot.GetMetadata().GetConfState(), c.voters) {
				return errors.New("cluster: incoming snapshot membership differs from fixed voters")
			}
			if err := c.store.save(ready); err != nil {
				return err
			}
			if !raft.IsEmptySnap(ready.Snapshot) {
				index := ready.Snapshot.GetMetadata().GetIndex()
				if err := c.restore(index, ready.Snapshot.GetData()); err != nil {
					return fmt.Errorf("cluster: restoring incoming snapshot: %w", err)
				}
				if err := c.store.markApplied(index, ready.Snapshot.GetMetadata().GetConfState()); err != nil {
					return err
				}
				c.signalApplied(index)
			}
			for _, msg := range ready.Messages {
				// Ready was persisted before a peer worker may transmit it.
				// A full queue is treated as unreachable; Raft retries it.
				select {
				case outboxes[msg.GetTo()] <- msg:
				default:
					c.node.ReportUnreachable(msg.GetTo())
					if msg.GetType() == pb.MessageType_MsgSnap {
						c.node.ReportSnapshot(msg.GetTo(), raft.SnapshotFailure)
					}
				}
			}
			for _, entry := range ready.CommittedEntries {
				if entry.GetIndex() <= c.store.applied {
					continue
				}
				var conf *pb.ConfState
				var proposalID [16]byte
				var hasProposal bool
				var proposalHash [32]byte
				switch entry.GetType() {
				case pb.EntryType_EntryNormal:
					if len(entry.GetData()) != 0 {
						if len(entry.GetData()) < len(proposalID) {
							return fmt.Errorf("cluster: committed entry %d has a truncated proposal", entry.GetIndex())
						}
						copy(proposalID[:], entry.GetData()[:len(proposalID)])
						hasProposal = true
						command := entry.GetData()[len(proposalID):]
						proposalHash = sha256.Sum256(command)
						prior, err := c.store.operationApplied(proposalID, proposalHash)
						if err != nil {
							return fmt.Errorf("cluster: checking committed operation %d: %w", entry.GetIndex(), err)
						}
						if !prior {
							if err := c.apply(entry.GetIndex(), command); err != nil {
								return fmt.Errorf("cluster: applying committed entry %d: %w", entry.GetIndex(), err)
							}
						}
					}
				case pb.EntryType_EntryConfChange:
					var change pb.ConfChange
					if err := proto.Unmarshal(entry.GetData(), &change); err != nil {
						return fmt.Errorf("cluster: decoding committed membership: %w", err)
					}
					conf = c.node.ApplyConfChange(&change)
				case pb.EntryType_EntryConfChangeV2:
					var change pb.ConfChangeV2
					if err := proto.Unmarshal(entry.GetData(), &change); err != nil {
						return fmt.Errorf("cluster: decoding committed membership: %w", err)
					}
					conf = c.node.ApplyConfChange(&change)
				default:
					return fmt.Errorf("cluster: unknown Raft entry type %d", entry.GetType())
				}
				if conf != nil && !c.allowedMembership(conf) {
					return errors.New("cluster: committed membership change violates the fixed voter set")
				}
				if c.advance != nil {
					if err := c.advance(entry.GetIndex()); err != nil {
						return fmt.Errorf("cluster: advancing metadata at entry %d: %w", entry.GetIndex(), err)
					}
				}
				var operationID *[16]byte
				if hasProposal {
					operationID = &proposalID
				}
				if err := c.store.markOperationApplied(entry.GetIndex(), conf, operationID, proposalHash); err != nil {
					return fmt.Errorf("cluster: marking entry %d applied: %w", entry.GetIndex(), err)
				}
				c.signalApplied(entry.GetIndex())
				if hasProposal {
					c.mu.Lock()
					if done := c.pending[proposalID]; done != nil {
						delete(c.pending, proposalID)
						done <- nil
					}
					c.mu.Unlock()
				}
			}
			for _, state := range ready.ReadStates {
				if len(state.RequestCtx) != 16 {
					continue
				}
				var id [16]byte
				copy(id[:], state.RequestCtx)
				c.mu.Lock()
				if done := c.reads[id]; done != nil {
					delete(c.reads, id)
					done <- state.Index
				}
				c.mu.Unlock()
			}
			c.node.Advance()
		}
	}
}

func sameVoters(conf *pb.ConfState, voters []uint64) bool {
	if conf == nil || len(conf.GetLearners()) != 0 || len(conf.GetVotersOutgoing()) != 0 || len(conf.GetLearnersNext()) != 0 || conf.GetAutoLeave() {
		return false
	}
	actual, expected := slices.Clone(conf.GetVoters()), slices.Clone(voters)
	slices.Sort(actual)
	slices.Sort(expected)
	return slices.Equal(actual, expected)
}

func (c *Consensus) allowedMembership(conf *pb.ConfState) bool {
	if sameVoters(conf, c.voters) {
		return true
	}
	if c.store.conf != nil && sameVoters(c.store.conf, c.voters) {
		return false
	}
	if len(conf.GetLearners()) != 0 || len(conf.GetVotersOutgoing()) != 0 || len(conf.GetLearnersNext()) != 0 || conf.GetAutoLeave() {
		return false
	}
	previous := 0
	if c.store.conf != nil {
		previous = len(c.store.conf.GetVoters())
	}
	if len(conf.GetVoters()) <= previous {
		return false
	}
	allowed := make(map[uint64]bool, len(c.voters))
	for _, id := range c.voters {
		allowed[id] = true
	}
	for _, id := range conf.GetVoters() {
		if !allowed[id] {
			return false
		}
	}
	return len(conf.GetVoters()) <= len(c.voters)
}

// Snapshot captures the state machine at an applied index while Run is not
// applying another command. makeImage must return the complete state at that
// index; the image is persisted before old log entries are compacted.
func (c *Consensus) Snapshot(ctx context.Context, makeImage func(uint64) ([]byte, error)) error {
	if makeImage == nil {
		return errors.New("cluster: snapshot image function is required")
	}
	request := snapshotRequest{make: makeImage, done: make(chan error, 1)}
	select {
	case c.snaps <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopped:
		return fmt.Errorf("%w: stopped before snapshot", ErrConsensusUnavailable)
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopped:
		return fmt.Errorf("%w: stopped during snapshot", ErrConsensusUnavailable)
	}
}

func (c *Consensus) signalApplied(index uint64) {
	c.mu.Lock()
	if index > c.applied {
		c.applied = index
		close(c.notice)
		c.notice = make(chan struct{})
	}
	c.mu.Unlock()
}

// Step accepts a Raft message only after the caller authenticates its
// sender as the configured peer with the matching node ID.
func (c *Consensus) Step(ctx context.Context, msg *pb.Message) error {
	if msg == nil || msg.GetTo() != c.id {
		return errors.New("cluster: Raft message addressed to another node")
	}
	return c.node.Step(ctx, msg)
}

func (c *Consensus) Propose(ctx context.Context, command []byte) error {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	return c.ProposeWithID(ctx, id, command)
}

// ProposeWithID lets an internal retry reuse its original operation ID. Once
// applied, the same ID and command return successfully without another state
// machine mutation; reusing the ID for different bytes is rejected.
func (c *Consensus) ProposeWithID(ctx context.Context, id [16]byte, command []byte) error {
	if len(command) == 0 {
		return errors.New("cluster: empty proposal")
	}
	ready := make(chan error, 1)
	c.mu.Lock()
	if _, exists := c.pending[id]; exists {
		c.mu.Unlock()
		return errors.New("cluster: operation is already being proposed")
	}
	c.pending[id] = ready
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	prior, err := c.store.operationApplied(id, sha256.Sum256(command))
	if err != nil {
		return err
	}
	if prior {
		return nil
	}
	data := make([]byte, 0, len(id)+len(command))
	data = append(data, id[:]...)
	data = append(data, command...)
	if err := c.node.Propose(ctx, data); err != nil {
		return err
	}
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopped:
		return fmt.Errorf("%w: stopped before the proposal committed", ErrConsensusUnavailable)
	}
}

// ReadBarrier waits for a quorum-confirmed ReadIndex and for this node to
// apply through that index. Call it before serving a cluster read, including
// on a follower. A partition or stale leader cannot complete the barrier.
func (c *Consensus) ReadBarrier(ctx context.Context) error {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	ready := make(chan uint64, 1)
	c.mu.Lock()
	c.reads[id] = ready
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.reads, id)
		c.mu.Unlock()
	}()
	if err := c.node.ReadIndex(ctx, id[:]); err != nil {
		return err
	}
	var index uint64
	retry := time.NewTicker(200 * time.Millisecond)
	defer retry.Stop()
	waiting := true
	for waiting {
		select {
		case index = <-ready:
			waiting = false
		case <-retry.C:
			// Raft can drop an index request while this follower has no
			// known leader. Reissuing the same context is safe.
			if err := c.node.ReadIndex(ctx, id[:]); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopped:
			return fmt.Errorf("%w: stopped before the read index completed", ErrConsensusUnavailable)
		}
	}
	for {
		c.mu.Lock()
		if c.applied >= index {
			c.mu.Unlock()
			return nil
		}
		notice := c.notice
		c.mu.Unlock()
		select {
		case <-notice:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopped:
			return fmt.Errorf("%w: stopped before the read index was applied", ErrConsensusUnavailable)
		}
	}
}

func (c *Consensus) Status() raft.Status { return c.node.Status() }

func (c *Consensus) Close() error {
	c.node.Stop()
	<-c.stopped
	return c.store.close()
}
