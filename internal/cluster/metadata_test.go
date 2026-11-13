package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
)

func metadataCommand(t *testing.T, ns MetadataNamespace, key, value string) []byte {
	t.Helper()
	data, err := EncodeMetadataCommand(MetadataCommand{Namespace: ns, Key: key, Value: []byte(value)})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMetadataListPageUsesExclusiveCursor(t *testing.T) {
	store, err := OpenMetadataStore(filepath.Join(t.TempDir(), "pages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 260; i++ {
		key := fmt.Sprintf("version\x00bucket\x00%03d", i)
		if err := store.Apply(uint64(i+1), metadataCommand(t, MetadataVersions, key, key)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Apply(261, metadataCommand(t, MetadataVersions, "latest\x00bucket\x00key", "skip")); err != nil {
		t.Fatal(err)
	}
	var got []MetadataRecord
	after := ""
	for {
		page, err := store.ListPage(MetadataVersions, "version\x00bucket\x00", after, 128)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page...)
		after = page[len(page)-1].Key
	}
	if len(got) != 260 {
		t.Fatalf("paged records = %d, want 260", len(got))
	}
	for i, item := range got {
		want := fmt.Sprintf("version\x00bucket\x00%03d", i)
		if item.Key != want || string(item.Value) != want {
			t.Fatalf("record %d = %+v, want %q", i, item, want)
		}
	}
}

func TestMetadataTransactionPrefixDigestGuardsLargeMultipartSet(t *testing.T) {
	store, err := OpenMetadataStore(filepath.Join(t.TempDir(), "parts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const prefix = "part\x00bucket\x00upload\x00"
	for i := 1; i <= 130; i++ {
		key := fmt.Sprintf("%s%05d", prefix, i)
		if err := store.Apply(uint64(i), metadataCommand(t, MetadataJobs, key, fmt.Sprintf("etag-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	makeHash := func() string {
		records, err := store.List(MetadataJobs, prefix)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		for _, record := range records {
			digestPrefixRecord(h, []byte(record.Key), record.Value)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	oldHash := makeHash()
	if err := store.Apply(131, metadataCommand(t, MetadataJobs, prefix+"00042", "replaced")); err != nil {
		t.Fatal(err)
	}
	commit := func(index uint64, id, digest string) {
		tx := MetadataTransaction{OperationID: id, PrefixDigests: []MetadataPrefixDigest{{Namespace: MetadataJobs, Prefix: prefix, SHA256: digest}}, Changes: []MetadataCommand{{Namespace: MetadataJobs, Key: "upload\x00bucket\x00upload", Delete: true}}, DeletePrefixes: []MetadataPrefix{{Namespace: MetadataJobs, Prefix: prefix}}}
		encoded, err := EncodeMetadataTransaction(tx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Apply(index, encoded); err != nil {
			t.Fatal(err)
		}
	}
	const rejected = "dddddddddddddddddddddddddddddddd"
	commit(132, rejected, oldHash)
	if applied, found, err := store.Outcome(rejected); err != nil || !found || applied {
		t.Fatalf("changed part accepted: %v, %v, %v", applied, found, err)
	}
	if records, err := store.List(MetadataJobs, prefix); err != nil || len(records) != 130 {
		t.Fatalf("rejected completion deleted parts: %d, %v", len(records), err)
	}
	const accepted = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	commit(133, accepted, makeHash())
	if applied, found, err := store.Outcome(accepted); err != nil || !found || !applied {
		t.Fatalf("unchanged parts rejected: %v, %v, %v", applied, found, err)
	}
	if records, err := store.List(MetadataJobs, prefix); err != nil || len(records) != 0 {
		t.Fatalf("accepted completion retained parts: %d, %v", len(records), err)
	}
}

func TestMetadataCommandsApplyThroughTwoVoterRaft(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	nodes := map[uint64]*Consensus{}
	state := map[uint64]*MetadataStore{}
	metadataPaths := map[uint64]string{}
	raftPaths := map[uint64]string{}
	for _, id := range []uint64{1, 2} {
		id := id
		metadataPaths[id] = filepath.Join(dir, fmt.Sprintf("metadata-%d.db", id))
		raftPaths[id] = filepath.Join(dir, fmt.Sprintf("raft-%d.db", id))
		store, err := OpenMetadataStore(metadataPaths[id])
		if err != nil {
			t.Fatal(err)
		}
		state[id] = store
		defer store.Close()
		node, err := OpenConsensusWithMetadata(raftPaths[id], id, []uint64{1, 2},
			func(ctx context.Context, msg *pb.Message) error { return nodes[msg.GetTo()].Step(ctx, msg) }, store)
		if err != nil {
			t.Fatal(err)
		}
		nodes[id] = node
	}
	done := make(chan error, 2)
	for _, node := range nodes {
		go func() { done <- node.Run(ctx) }()
	}
	closed := false
	closeNodes := func() {
		if closed {
			return
		}
		closed = true
		cancel()
		for range nodes {
			<-done
		}
		for _, node := range nodes {
			if err := node.Close(); err != nil {
				t.Error(err)
			}
		}
	}
	defer closeNodes()
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
			time.Sleep(20 * time.Millisecond)
		}
	}
	if leader == nil {
		t.Fatal("no leader")
	}
	command := metadataCommand(t, MetadataBuckets, "photos", "versioned")
	var operationID [16]byte
	operationID[0] = 9
	writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := leader.ProposeWithID(writeCtx, operationID, command); err != nil {
		t.Fatal(err)
	}
	for id, node := range nodes {
		if err := node.ReadBarrier(writeCtx); err != nil {
			t.Fatalf("node %d read barrier: %v", id, err)
		}
		value, found, err := state[id].Get(MetadataBuckets, "photos")
		if err != nil || !found || string(value) != "versioned" {
			t.Fatalf("node %d state = %q, %v, %v", id, value, found, err)
		}
		index, err := state[id].AppliedIndex()
		if err != nil || index != node.store.applied {
			t.Fatalf("node %d metadata index %d, Raft applied %d: %v", id, index, node.store.applied, err)
		}
	}
	if err := leader.Snapshot(writeCtx, state[leader.id].Snapshot); err != nil {
		t.Fatal(err)
	}
	closeNodes()
	reopened, err := OpenConsensusWithMetadata(raftPaths[1], 1, []uint64{1, 2}, func(context.Context, *pb.Message) error { return nil }, state[1])
	if err != nil {
		t.Fatalf("matching Raft and metadata restart: %v", err)
	}
	reopened.node.Stop()
	if err := reopened.store.close(); err != nil {
		t.Fatal(err)
	}
	if err := state[1].Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(metadataPaths[1]); err != nil {
		t.Fatal(err)
	}
	empty, err := OpenMetadataStore(metadataPaths[1])
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if reopened, err := OpenConsensusWithMetadata(raftPaths[1], 1, []uint64{1, 2}, func(context.Context, *pb.Message) error { return nil }, empty); err == nil {
		reopened.node.Stop()
		_ = reopened.store.close()
		t.Fatal("lost metadata store was accepted despite a committed Raft log")
	}
}

func TestMetadataStoreApplyReplayAndSnapshotRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := OpenMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first := metadataCommand(t, MetadataBuckets, "photos", "versioned")
	if err := store.Apply(3, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(3, first); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := store.Apply(3, metadataCommand(t, MetadataBuckets, "photos", "different")); err == nil {
		t.Fatal("conflicting replay accepted")
	}
	if err := store.Apply(4, metadataCommand(t, MetadataIAM, "alice", "readonly")); err != nil {
		t.Fatal(err)
	}
	image, err := store.Snapshot(5)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	value, found, err := store.Get(MetadataBuckets, "photos")
	if err != nil || !found || string(value) != "versioned" {
		t.Fatalf("reopened bucket = %q, %v, %v", value, found, err)
	}
	if err := store.Apply(4, metadataCommand(t, MetadataIAM, "alice", "readonly")); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(6, metadataCommand(t, MetadataSettings, "region", "us-east-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(5, image); !errors.Is(err, errMetadataIndexRegression) {
		t.Fatalf("stale restore: %v", err)
	}
	if err := store.Restore(7, image); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Get(MetadataSettings, "region"); err != nil || found {
		t.Fatalf("snapshot did not replace settings: %v, %v", found, err)
	}
	value, found, err = store.Get(MetadataIAM, "alice")
	if err != nil || !found || string(value) != "readonly" {
		t.Fatalf("restored IAM = %q, %v, %v", value, found, err)
	}
	deleteCommand, err := EncodeMetadataCommand(MetadataCommand{Namespace: MetadataIAM, Key: "alice", Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(8, deleteCommand); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Get(MetadataIAM, "alice"); err != nil || found {
		t.Fatalf("delete: %v, %v", found, err)
	}
}

func TestMetadataStoreRejectsInvalidCommandsAndImages(t *testing.T) {
	store, err := OpenMetadataStore(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, command := range []MetadataCommand{
		{Namespace: "local-peers", Key: "a", Value: []byte("url")},
		{Namespace: MetadataVersions, Key: ""},
		{Namespace: MetadataRoles, Key: "r", Value: []byte("value"), Delete: true},
	} {
		if _, err := EncodeMetadataCommand(command); !errors.Is(err, errInvalidMetadataCommand) {
			t.Fatalf("accepted %+v: %v", command, err)
		}
	}
	if err := store.Apply(1, []byte(`{"namespace":"buckets","key":"b","unexpected":true}`)); err == nil {
		t.Fatal("unknown command field accepted")
	}
	if err := store.Apply(1, append(metadataCommand(t, MetadataBuckets, "b", "v"), []byte(`{}`)...)); err == nil {
		t.Fatal("trailing command accepted")
	}
	if err := store.Restore(1, []byte(`{"applied":0,"records":{"buckets":{}}}`)); err == nil {
		t.Fatal("incomplete snapshot accepted")
	}
	image, err := store.Snapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(1, append(bytes.Clone(image), []byte(`{}`)...)); err == nil {
		t.Fatal("trailing snapshot accepted")
	}
}

func TestMetadataTransactionAtomicConditionsRetryAndSnapshot(t *testing.T) {
	store, err := OpenMetadataStore(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const firstID = "00112233445566778899aabbccddeeff"
	first := MetadataTransaction{OperationID: firstID,
		Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: "photos", Exists: false}},
		Changes: []MetadataCommand{
			{Namespace: MetadataBuckets, Key: "photos", Value: []byte("versioned")},
			{Namespace: MetadataSettings, Key: "region", Value: []byte("us-east-1")},
		}}
	encoded, err := EncodeMetadataTransaction(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(1, encoded); err != nil {
		t.Fatal(err)
	}
	if applied, found, err := store.Outcome(firstID); err != nil || !found || !applied {
		t.Fatalf("successful outcome = %v, %v, %v", applied, found, err)
	}
	for _, item := range []struct {
		ns        MetadataNamespace
		key, want string
	}{
		{MetadataBuckets, "photos", "versioned"}, {MetadataSettings, "region", "us-east-1"},
	} {
		value, found, err := store.Get(item.ns, item.key)
		if err != nil || !found || string(value) != item.want {
			t.Fatalf("%s/%s = %q, %v, %v", item.ns, item.key, value, found, err)
		}
	}
	records, err := store.List(MetadataBuckets, "pho")
	if err != nil || len(records) != 1 || records[0].Key != "photos" || string(records[0].Value) != "versioned" {
		t.Fatalf("bucket prefix list = %+v, %v", records, err)
	}
	const rejectedID = "11112222333344445555666677778888"
	rejected := MetadataTransaction{OperationID: rejectedID,
		Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: "photos", Exists: true, Value: []byte("unversioned")}},
		Changes: []MetadataCommand{
			{Namespace: MetadataBuckets, Key: "photos", Delete: true},
			{Namespace: MetadataIAM, Key: "alice", Value: []byte("admin")},
		}}
	encodedRejected, err := EncodeMetadataTransaction(rejected)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(2, encodedRejected); err != nil {
		t.Fatal(err)
	}
	if applied, found, err := store.Outcome(rejectedID); err != nil || !found || applied {
		t.Fatalf("rejected outcome = %v, %v, %v", applied, found, err)
	}
	if _, found, err := store.Get(MetadataIAM, "alice"); err != nil || found {
		t.Fatalf("rejected transaction changed IAM: %v, %v", found, err)
	}
	if value, found, err := store.Get(MetadataBuckets, "photos"); err != nil || !found || string(value) != "versioned" {
		t.Fatalf("rejected transaction changed bucket: %q, %v, %v", value, found, err)
	}
	if err := store.Apply(3, encodedRejected); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	changed := MetadataTransaction{OperationID: rejectedID, Changes: []MetadataCommand{{Namespace: MetadataBuckets, Key: "photos", Delete: true}}}
	changedBytes, err := EncodeMetadataTransaction(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(4, changedBytes); err == nil {
		t.Fatal("operation ID reused with different transaction bytes")
	}
	image, err := store.Snapshot(3)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := OpenMetadataStore(filepath.Join(t.TempDir(), "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Restore(3, image); err != nil {
		t.Fatal(err)
	}
	if applied, found, err := restored.Outcome(rejectedID); err != nil || !found || applied {
		t.Fatalf("snapshot lost rejected outcome: %v, %v, %v", applied, found, err)
	}
	if applied, found, err := restored.Outcome(firstID); err != nil || !found || !applied {
		t.Fatalf("snapshot lost committed outcome: %v, %v, %v", applied, found, err)
	}
}

func TestMetadataTransactionValidation(t *testing.T) {
	const id = "00112233445566778899aabbccddeeff"
	for _, transaction := range []MetadataTransaction{
		{OperationID: "bad", Changes: []MetadataCommand{{Namespace: MetadataBuckets, Key: "b"}}},
		{OperationID: id},
		{OperationID: id, Changes: []MetadataCommand{{Namespace: MetadataBuckets, Key: "b"}, {Namespace: MetadataBuckets, Key: "b"}}},
		{OperationID: id, Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: "b", Value: []byte("unexpected")}}, Changes: []MetadataCommand{{Namespace: MetadataBuckets, Key: "b"}}},
		{OperationID: id, PayloadSHA256: "bad", Changes: []MetadataCommand{{Namespace: MetadataBuckets, Key: "b"}}},
	} {
		if _, err := EncodeMetadataTransaction(transaction); !errors.Is(err, errInvalidMetadataCommand) {
			t.Fatalf("accepted invalid transaction %+v: %v", transaction, err)
		}
	}
	store, err := OpenMetadataStore(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Apply(1, []byte(`{"transaction":{"operationId":"`+id+`","changes":[{"namespace":"buckets","key":"b"}],"extra":true}}`)); err == nil {
		t.Fatal("unknown transaction field accepted")
	}
}

func TestMetadataTransactionPrefixChecksAndDeletes(t *testing.T) {
	store, err := OpenMetadataStore(filepath.Join(t.TempDir(), "prefix.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i, command := range []MetadataCommand{
		{Namespace: MetadataBuckets, Key: "bucket/a", Value: []byte("a")},
		{Namespace: MetadataBuckets, Key: "config\x00a\x00tagging", Value: []byte("tags")},
		{Namespace: MetadataBuckets, Key: "config\x00ab\x00tagging", Value: []byte("keep")},
		{Namespace: MetadataVersions, Key: "latest\x00a\x00k", Value: []byte("v1")},
	} {
		encoded, err := EncodeMetadataCommand(command)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if err := store.Apply(uint64(i+1), encoded); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	makeDelete := func(id string) MetadataTransaction {
		return MetadataTransaction{OperationID: id,
			Conditions:     []MetadataCondition{{Namespace: MetadataBuckets, Key: "bucket/a", Exists: true, Value: []byte("a")}},
			AbsentPrefixes: []MetadataPrefix{{Namespace: MetadataVersions, Prefix: "latest\x00a\x00"}},
			DeletePrefixes: []MetadataPrefix{{Namespace: MetadataBuckets, Prefix: "config\x00a\x00"}},
			Changes:        []MetadataCommand{{Namespace: MetadataBuckets, Key: "bucket/a", Delete: true}},
		}
	}
	const rejectedID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rejected, err := EncodeMetadataTransaction(makeDelete(rejectedID))
	if err != nil {
		t.Fatalf("prefix rejection: %v", err)
	}
	if err := store.Apply(5, rejected); err != nil {
		t.Fatalf("prefix rejection: %v", err)
	}
	if applied, found, err := store.Outcome(rejectedID); err != nil || !found || applied {
		t.Fatalf("nonempty prefix outcome: %v, %v, %v", applied, found, err)
	}
	if _, found, err := store.Get(MetadataBuckets, "config\x00a\x00tagging"); err != nil || !found {
		t.Fatalf("rejected prefix delete removed config: %v, %v", found, err)
	}
	deleteLatest, err := EncodeMetadataCommand(MetadataCommand{Namespace: MetadataVersions, Key: "latest\x00a\x00k", Delete: true})
	if err != nil {
		t.Fatalf("delete latest: %v", err)
	}
	if err := store.Apply(6, deleteLatest); err != nil {
		t.Fatalf("delete latest: %v", err)
	}
	const acceptedID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	accepted, err := EncodeMetadataTransaction(makeDelete(acceptedID))
	if err != nil {
		t.Fatalf("prefix delete: %v", err)
	}
	if err := store.Apply(7, accepted); err != nil {
		t.Fatalf("prefix delete: %v", err)
	}
	if applied, found, err := store.Outcome(acceptedID); err != nil || !found || !applied {
		t.Fatalf("empty prefix outcome: %v, %v, %v", applied, found, err)
	}
	for _, key := range []string{"bucket/a", "config\x00a\x00tagging"} {
		if _, found, err := store.Get(MetadataBuckets, key); err != nil || found {
			t.Fatalf("deleted key %q: %v, %v", key, found, err)
		}
	}
	if value, found, err := store.Get(MetadataBuckets, "config\x00ab\x00tagging"); err != nil || !found || string(value) != "keep" {
		t.Fatalf("adjacent bucket config changed: %q, %v, %v", value, found, err)
	}
	const lateWriteID = "cccccccccccccccccccccccccccccccc"
	lateWrite, err := EncodeMetadataTransaction(MetadataTransaction{OperationID: lateWriteID,
		Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: "bucket/a", Exists: true, Value: []byte("a")}},
		Changes:    []MetadataCommand{{Namespace: MetadataVersions, Key: "latest\x00a\x00late", Value: []byte("v2")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(8, lateWrite); err != nil {
		t.Fatal(err)
	}
	if applied, found, err := store.Outcome(lateWriteID); err != nil || !found || applied {
		t.Fatalf("write after bucket delete outcome: %v, %v, %v", applied, found, err)
	}
	if _, found, err := store.Get(MetadataVersions, "latest\x00a\x00late"); err != nil || found {
		t.Fatalf("late object appeared after bucket delete: %v, %v", found, err)
	}
}
