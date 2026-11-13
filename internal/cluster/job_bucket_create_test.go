package cluster

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"go.etcd.io/bbolt"
)

func TestCopyJobCreatedBucketSurvivesLostMigrationOutcomes(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	b := NewS3Backend(nodes["a"])
	created := time.Now().UTC()
	opts := storage.CreateBucketOptions{Owner: "alice", Import: true}
	first, err := b.CreateBucketForJob(ctx, "copy_migrated", created, "target", opts)
	if err != nil {
		t.Fatal(err)
	}
	id, err := hex.DecodeString(first.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	// Migration restores the bucket and job records into a fresh Raft log,
	// without the original operation outcomes.
	for _, node := range nodes {
		if err := node.ReadBarrier(ctx); err != nil {
			t.Fatal(err)
		}
		if err := node.consensus.store.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(raftOpsBucket).Delete(id) }); err != nil {
			t.Fatal(err)
		}
		if err := node.metadata.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(metadataOutcomes).Delete([]byte(first.InstanceID)) }); err != nil {
			t.Fatal(err)
		}
	}
	retried, err := NewS3Backend(nodes["b"]).CreateBucketForJob(ctx, "copy_migrated", created, "target", opts)
	if err != nil || retried.InstanceID != first.InstanceID {
		t.Fatalf("migrated job could not recognize its bucket: %+v %v", retried, err)
	}
}

func TestCopyJobBucketCreateRetriesWithStableIdentity(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	createdAt := time.Now().UTC()
	opts := storage.CreateBucketOptions{Owner: "alice", Import: true}
	a, b := NewS3Backend(nodes["a"]), NewS3Backend(nodes["b"])
	first, err := a.CreateBucketForJob(ctx, "copy_123", createdAt, "copy-target", opts)
	if err != nil {
		t.Fatal(err)
	}
	ack := nodes["a"].Status(ctx).LastWrite
	if ack.MemoryAcks != 2 || ack.DurableAcks != 2 || !ack.ConsensusCommitted {
		t.Fatalf("job target creation succeeded below acknowledgment policy: %+v", ack)
	}
	retried, err := b.CreateBucketForJob(ctx, "copy_123", createdAt, "copy-target", opts)
	if err != nil || retried.InstanceID != first.InstanceID || retried.CreatedAt != first.CreatedAt {
		t.Fatalf("retry changed target identity: first=%+v retry=%+v err=%v", first, retried, err)
	}
	if got, err := b.GetBucket(ctx, "copy-target"); err != nil || got.InstanceID != first.InstanceID {
		t.Fatalf("follower read of job target: %+v, %v", got, err)
	}
	if err := a.CreateBucket(ctx, "other-target", opts); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateBucketForJob(ctx, "copy_456", createdAt, "other-target", opts); !errors.Is(err, storage.ErrBucketExists) {
		t.Fatalf("job adopted a bucket created by another operation: %v", err)
	}
}
