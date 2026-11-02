package cluster

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

func TestTwoVotersCommitAndRecover(t *testing.T) {
	dir := t.TempDir()
	paths := map[uint64]string{1: filepath.Join(dir, "one.db"), 2: filepath.Join(dir, "two.db")}
	for run := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		nodes := map[uint64]*Consensus{}
		applied := map[uint64]chan string{1: make(chan string, 8), 2: make(chan string, 8)}
		for _, id := range []uint64{1, 2} {
			id := id
			n, err := OpenConsensus(paths[id], id, []uint64{1, 2}, func(ctx context.Context, msg *pb.Message) error {
				peer := nodes[msg.GetTo()]
				if peer == nil {
					return fmt.Errorf("unknown peer %d", msg.GetTo())
				}
				return peer.Step(ctx, msg)
			}, func(_ uint64, data []byte) error {
				applied[id] <- string(data)
				return nil
			})
			if err != nil {
				t.Fatalf("run %d node %d open: %v", run, id, err)
			}
			nodes[id] = n
		}
		done := make(chan error, 2)
		for _, n := range nodes {
			go func() { done <- n.Run(ctx) }()
		}
		deadline := time.Now().Add(8 * time.Second)
		var leader *Consensus
		for leader == nil && time.Now().Before(deadline) {
			for _, n := range nodes {
				if n.Status().RaftState == raft.StateLeader {
					leader = n
					break
				}
			}
			if leader == nil {
				time.Sleep(25 * time.Millisecond)
			}
		}
		if leader == nil {
			t.Fatal("no leader elected")
		}
		command := fmt.Sprintf("write-%d", run)
		var operationID [16]byte
		operationID[0] = byte(run + 1)
		if err := leader.ProposeWithID(ctx, operationID, []byte(command)); err != nil {
			t.Fatal(err)
		}
		for id, ch := range applied {
			select {
			case got := <-ch:
				if got != command {
					t.Fatalf("node %d applied %q, want %q", id, got, command)
				}
			case <-time.After(8 * time.Second):
				t.Fatalf("node %d did not apply %q", id, command)
			}
		}
		if err := leader.ProposeWithID(ctx, operationID, []byte(command)); err != nil {
			t.Fatalf("idempotent retry: %v", err)
		}
		if err := leader.ProposeWithID(ctx, operationID, []byte("different")); err == nil {
			t.Fatal("operation ID accepted a different command")
		}
		// Simulate an uncertain retry that was appended before the first
		// commit became visible to the caller.
		duplicate := append(operationID[:], []byte(command)...)
		if err := leader.node.Propose(ctx, duplicate); err != nil {
			t.Fatal(err)
		}
		for _, node := range nodes {
			readCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			err := node.ReadBarrier(readCtx)
			stop()
			if err != nil {
				t.Fatal(err)
			}
		}
		for id, ch := range applied {
			select {
			case got := <-ch:
				t.Fatalf("node %d applied duplicate command %q", id, got)
			default:
			}
		}
		if run == 0 {
			if err := leader.Snapshot(ctx, func(index uint64) ([]byte, error) {
				if index == 0 {
					return nil, errors.New("snapshot requested before applied state")
				}
				return []byte("state-at-snapshot"), nil
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := leader.store.memory.Snapshot()
			if err != nil || string(snapshot.GetData()) != "state-at-snapshot" {
				t.Fatalf("snapshot = %v, %v", snapshot, err)
			}
		}
		for id, n := range nodes {
			readCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			err := n.ReadBarrier(readCtx)
			stop()
			if err != nil {
				t.Fatalf("node %d could not observe committed write: %v", id, err)
			}
		}
		cancel()
		for range nodes {
			<-done
		}
		for _, n := range nodes {
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestTwoVotersDoNotAcknowledgePartitionedWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var partition atomic.Bool
	nodes := map[uint64]*Consensus{}
	applied := make(chan string, 4)
	for _, id := range []uint64{1, 2} {
		id := id
		n, err := OpenConsensus(filepath.Join(t.TempDir(), fmt.Sprintf("%d.db", id)), id, []uint64{1, 2},
			func(ctx context.Context, msg *pb.Message) error {
				if partition.Load() {
					return fmt.Errorf("peer unavailable")
				}
				return nodes[msg.GetTo()].Step(ctx, msg)
			},
			func(_ uint64, data []byte) error { applied <- string(data); return nil })
		if err != nil {
			t.Fatal(err)
		}
		nodes[id] = n
	}
	done := make(chan error, 2)
	for _, n := range nodes {
		go func() { done <- n.Run(ctx) }()
	}
	defer func() {
		cancel()
		for range nodes {
			<-done
		}
		for _, n := range nodes {
			_ = n.Close()
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	var leader *Consensus
	for leader == nil && time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.Status().RaftState == raft.StateLeader {
				leader = n
				break
			}
		}
		if leader == nil {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if leader == nil {
		t.Fatal("no leader elected")
	}
	partition.Store(true)
	proposalCtx, stop := context.WithTimeout(ctx, 700*time.Millisecond)
	defer stop()
	if err := leader.Propose(proposalCtx, []byte("must-not-ack")); err == nil {
		t.Fatal("write acknowledged without the second voter")
	}
	readCtx, stopRead := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stopRead()
	if err := leader.ReadBarrier(readCtx); err == nil {
		t.Fatal("partitioned leader completed a linearizable read")
	}
	select {
	case got := <-applied:
		t.Fatalf("partitioned write %q applied without quorum", got)
	default:
	}
}

func TestRestartBeforeBootstrapMembershipApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap.db")
	open := func(voters []uint64) (*Consensus, error) {
		return OpenConsensus(path, 1, voters, func(context.Context, *pb.Message) error { return nil }, func(uint64, []byte) error { return nil })
	}
	node, err := open([]uint64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ready := <-node.node.Ready():
		if len(ready.Entries) == 0 {
			t.Fatal("expected bootstrap membership entries")
		}
		if err := node.store.save(ready); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("bootstrap did not produce a Ready batch")
	}
	// Simulate a crash before any committed configuration was applied.
	node.node.Stop()
	if err := node.store.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := open([]uint64{1, 2})
	if err != nil {
		t.Fatalf("restart after saved bootstrap: %v", err)
	}
	if err := restarted.store.markApplied(1, &pb.ConfState{Voters: []uint64{1}}); err != nil {
		t.Fatal(err)
	}
	restarted.node.Stop()
	if err := restarted.store.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err = open([]uint64{1, 2})
	if err != nil {
		t.Fatalf("restart after partial bootstrap membership: %v", err)
	}
	restarted.node.Stop()
	if err := restarted.store.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := open([]uint64{1, 3}); err == nil {
		t.Fatal("changed fixed voter set was accepted")
	}
}

func TestThreeVoterLeaderFailoverFencesOldLeader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var isolated atomic.Uint64
	nodes := map[uint64]*Consensus{}
	paths := map[uint64]string{}
	restores := map[uint64]*atomic.Int32{}
	for _, id := range []uint64{1, 2, 3} {
		id := id
		paths[id] = filepath.Join(t.TempDir(), fmt.Sprintf("%d.db", id))
		restores[id] = &atomic.Int32{}
		node, err := OpenConsensusWithSnapshots(paths[id], id, []uint64{1, 2, 3}, func(ctx context.Context, msg *pb.Message) error {
			if msg.GetFrom() == isolated.Load() || msg.GetTo() == isolated.Load() {
				return errors.New("peer partitioned")
			}
			return nodes[msg.GetTo()].Step(ctx, msg)
		}, func(uint64, []byte) error { return nil }, func(index uint64, data []byte) error {
			if index == 0 || string(data) != "quorum-state" {
				return fmt.Errorf("invalid repair snapshot: index=%d data=%q", index, data)
			}
			restores[id].Add(1)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		nodes[id] = node
	}
	done := make(chan error, len(nodes))
	for _, node := range nodes {
		go func() { done <- node.Run(ctx) }()
	}
	defer func() {
		cancel()
		for range nodes {
			<-done
		}
		for _, node := range nodes {
			_ = node.Close()
		}
	}()
	waitLeader := func(exclude uint64) *Consensus {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			for id, node := range nodes {
				if id != exclude && node.Status().RaftState == raft.StateLeader {
					return node
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("remaining quorum did not elect a leader")
		return nil
	}
	oldLeader := waitLeader(0)
	isolated.Store(oldLeader.id)
	proposalCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	if err := oldLeader.Propose(proposalCtx, []byte("isolated-write")); err == nil {
		t.Fatal("isolated old leader acknowledged a write")
	}
	stop()
	newLeader := waitLeader(oldLeader.id)
	writeCtx, stopWrite := context.WithTimeout(ctx, 3*time.Second)
	defer stopWrite()
	if err := newLeader.Propose(writeCtx, []byte("new-leader-write")); err != nil {
		t.Fatalf("new quorum could not commit: %v", err)
	}
	if err := newLeader.Snapshot(writeCtx, func(uint64) ([]byte, error) { return []byte("quorum-state"), nil }); err != nil {
		t.Fatalf("new leader could not snapshot: %v", err)
	}
	isolated.Store(0)
	readCtx, stopRead := context.WithTimeout(ctx, 5*time.Second)
	defer stopRead()
	if err := oldLeader.ReadBarrier(readCtx); err != nil {
		t.Fatalf("returning node did not catch up: %v", err)
	}
	if restores[oldLeader.id].Load() == 0 {
		t.Fatal("returning node caught up without installing the compacted snapshot")
	}
}

func TestPersistedSnapshotRestoredBeforeRaftRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.db")
	store, _, err := openRaftStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindNodeID(1); err != nil {
		t.Fatal(err)
	}
	if err := store.bindVoters([]uint64{1, 2}); err != nil {
		t.Fatal(err)
	}
	snapshot := &pb.Snapshot{Data: []byte("snapshot-state"), Metadata: &pb.SnapshotMetadata{Index: proto.Uint64(5), Term: proto.Uint64(2), ConfState: &pb.ConfState{Voters: []uint64{1, 2}}}}
	if err := store.save(raft.Ready{Snapshot: snapshot, HardState: &pb.HardState{Term: proto.Uint64(2), Commit: proto.Uint64(5)}}); err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	send := func(context.Context, *pb.Message) error { return nil }
	apply := func(uint64, []byte) error { return nil }
	if _, err := OpenConsensus(path, 1, []uint64{1, 2}, send, apply); err == nil {
		t.Fatal("unapplied snapshot opened without a restore hook")
	}
	var restored int
	node, err := OpenConsensusWithSnapshots(path, 1, []uint64{1, 2}, send, apply, func(index uint64, data []byte) error {
		if index != 5 || string(data) != "snapshot-state" {
			t.Fatalf("wrong snapshot: index=%d data=%q", index, data)
		}
		restored++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored != 1 || node.store.applied != 5 {
		t.Fatalf("restore count=%d, applied=%d", restored, node.store.applied)
	}
	node.node.Stop()
	if err := node.store.close(); err != nil {
		t.Fatal(err)
	}
	node, err = OpenConsensusWithSnapshots(path, 1, []uint64{1, 2}, send, apply, func(uint64, []byte) error {
		restored++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored != 1 {
		t.Fatalf("already applied snapshot replayed %d times", restored)
	}
	node.node.Stop()
	if err := node.store.close(); err != nil {
		t.Fatal(err)
	}
}

func TestSlowPeerDoesNotBlockHealthyQuorum(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nodes := make(map[uint64]*Consensus)
	for _, id := range []uint64{1, 2, 3} {
		id := id
		node, err := OpenConsensus(filepath.Join(t.TempDir(), fmt.Sprintf("%d.db", id)), id, []uint64{1, 2, 3}, func(ctx context.Context, msg *pb.Message) error {
			if msg.GetTo() == 3 {
				select {
				case <-time.After(2 * time.Second):
					return errors.New("slow peer")
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nodes[msg.GetTo()].Step(ctx, msg)
		}, func(uint64, []byte) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		nodes[id] = node
	}
	done := make(chan error, len(nodes))
	for _, node := range nodes {
		go func() { done <- node.Run(ctx) }()
	}
	defer func() {
		cancel()
		for range nodes {
			<-done
		}
		for _, node := range nodes {
			_ = node.Close()
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	var leader *Consensus
	for time.Now().Before(deadline) && leader == nil {
		for _, id := range []uint64{1, 2} {
			if nodes[id].Status().RaftState == raft.StateLeader {
				leader = nodes[id]
				break
			}
		}
		if leader == nil {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if leader == nil {
		t.Fatal("healthy quorum did not elect a leader")
	}
	writeCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if err := leader.Propose(writeCtx, []byte("quorum-write")); err != nil {
		t.Fatalf("slow third peer delayed healthy quorum: %v", err)
	}
}
