package cluster

import (
	"path/filepath"
	"testing"

	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
)

func TestRaftStoreRecoversCommittedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft.db")
	s, restart, err := openRaftStore(path)
	if err != nil || restart {
		t.Fatalf("new store: restart=%t, err=%v", restart, err)
	}
	entry := &pb.Entry{Index: new(uint64(1)), Term: new(uint64(1)), Data: []byte("mutation")}
	hard := &pb.HardState{Term: new(uint64(1)), Commit: new(uint64(1))}
	if err := s.save(raft.Ready{Entries: []*pb.Entry{entry}, HardState: hard}); err != nil {
		t.Fatal(err)
	}
	conf := &pb.ConfState{Voters: []uint64{1, 2}}
	if err := s.markApplied(1, conf); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, restart, err = openRaftStore(path)
	if err != nil || !restart {
		t.Fatalf("reopened store: restart=%t, err=%v", restart, err)
	}
	defer s.close()
	if s.applied != 1 {
		t.Fatalf("applied=%d, want 1", s.applied)
	}
	gotHard, gotConf, err := s.memory.InitialState()
	if err != nil || gotHard.GetCommit() != 1 || len(gotConf.GetVoters()) != 2 {
		t.Fatalf("initial state: hard=%v, conf=%v, err=%v", gotHard, gotConf, err)
	}
	entries, err := s.memory.Entries(1, 2, 1024)
	if err != nil || len(entries) != 1 || string(entries[0].GetData()) != "mutation" {
		t.Fatalf("entries=%v, err=%v", entries, err)
	}
}

func TestRaftStoreOverwritesUncommittedSuffix(t *testing.T) {
	s, _, err := openRaftStore(filepath.Join(t.TempDir(), "raft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	first := []*pb.Entry{
		{Index: new(uint64(1)), Term: new(uint64(1))},
		{Index: new(uint64(2)), Term: new(uint64(1))},
		{Index: new(uint64(3)), Term: new(uint64(1))},
	}
	if err := s.save(raft.Ready{Entries: first}); err != nil {
		t.Fatal(err)
	}
	if err := s.save(raft.Ready{Entries: []*pb.Entry{{Index: new(uint64(2)), Term: new(uint64(2))}}}); err != nil {
		t.Fatal(err)
	}
	last, err := s.memory.LastIndex()
	if err != nil || last != 2 {
		t.Fatalf("last index=%d, err=%v; want 2", last, err)
	}
	entries, err := s.memory.Entries(1, 3, 1024)
	if err != nil || entries[1].GetTerm() != 2 {
		t.Fatalf("overwritten suffix=%v, err=%v", entries, err)
	}
}

func TestRaftStoreNodeIdentityIsPersistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft.db")
	s, _, err := openRaftStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.bindNodeID(1); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, _, err = openRaftStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.bindNodeID(1); err != nil {
		t.Fatal(err)
	}
	if err := s.bindNodeID(2); err == nil {
		t.Fatal("the same log was accepted for another node")
	}
}
