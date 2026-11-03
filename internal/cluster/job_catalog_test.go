package cluster

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestJobCatalogSharesConditionalRecordsAcrossVoters(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, node := range nodes {
		for {
			if err := node.ReadBarrier(ctx); err == nil {
				break
			}
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	a, b := NewJobCatalog(nodes["a"]), NewJobCatalog(nodes["b"])
	first, err := NewJobMutation("copy_1", nil, false, []byte(`{"state":"queued"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CompareAndSwap(ctx, first); err != nil {
		t.Fatal(err)
	}
	ack := nodes["a"].Status(ctx).LastWrite
	if ack.MemoryAcks != 2 || ack.DurableAcks != 2 || !ack.ConsensusCommitted {
		t.Fatalf("job create succeeded below acknowledgment policy: %+v", ack)
	}
	if err := a.CompareAndSwap(ctx, first); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	got, found, err := b.Get(ctx, "copy_1")
	if err != nil || !found || !bytes.Equal(got, first.Next) {
		t.Fatalf("follower read: %q, %t, %v", got, found, err)
	}
	stale, err := NewJobMutation("copy_1", nil, false, []byte(`{"state":"running"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.CompareAndSwap(ctx, stale); !errors.Is(err, ErrJobRecordConflict) {
		t.Fatalf("stale create: %v", err)
	}
	next, err := NewJobMutation("copy_1", got, true, []byte(`{"state":"running"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.CompareAndSwap(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := a.CompareAndSwap(ctx, next); err != nil {
		t.Fatalf("idempotent update retry: %v", err)
	}
	records, err := a.List(ctx)
	if err != nil || len(records) != 1 || records[0].ID != "copy_1" || !bytes.Equal(records[0].Data, next.Next) {
		t.Fatalf("shared list: %+v, %v", records, err)
	}
	remove, err := NewJobMutation("copy_1", next.Next, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CompareAndSwap(ctx, remove); err != nil {
		t.Fatal(err)
	}
	if got, found, err := b.Get(ctx, "copy_1"); err != nil || found || got != nil {
		t.Fatalf("shared delete: %q, %t, %v", got, found, err)
	}
}
