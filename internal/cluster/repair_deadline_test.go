package cluster

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	bolt "go.etcd.io/bbolt"
)

func TestVerifiedReadDoesNotWaitForLocalRepair(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	b := NewS3Backend(nodes["a"])
	if err := b.CreateBucket(ctx, "bucket", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	w, err := b.NewObjectWriter(ctx, "bucket", "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	record, err := b.objects.lookupRecord(ctx, "bucket", "key", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := nodes["a"].stages.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(stageBucket).Delete([]byte(record.OperationID)) }); err != nil {
		t.Fatal(err)
	}
	nodes["a"].stages.mu.Lock()
	locked := true
	defer func() {
		if locked {
			nodes["a"].stages.mu.Unlock()
		}
	}()
	done := make(chan error, 1)
	var id [16]byte
	decoded, _ := hex.DecodeString(record.OperationID)
	copy(id[:], decoded)
	var digest [32]byte
	decoded, _ = hex.DecodeString(record.Digest)
	copy(digest[:], decoded)
	readCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	go func() { _, err := nodes["a"].ReadVerifiedPayload(readCtx, id, "bucket", "key", digest, 7); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		nodes["a"].stages.mu.Unlock()
		locked = false
		<-done
		t.Fatal("healthy peer payload was blocked beyond request deadline by local repair mutex")
	}
}

func waitForRepairedPayload(t *testing.T, n *Node, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := n.stages.Read(t.Context(), id); err == nil && string(body) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background repair did not restore verified bytes")
}
