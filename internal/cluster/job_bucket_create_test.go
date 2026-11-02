package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

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
