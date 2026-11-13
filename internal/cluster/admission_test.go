package cluster

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectReplicasStableAcrossPeerOrder(t *testing.T) {
	peers := []string{"one", "two", "three", "four"}
	a, err := SelectReplicas("cluster", "bucket", "key", peers, 3)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(peers)
	b, err := SelectReplicas("cluster", "bucket", "key", peers, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a, b) {
		t.Fatalf("placement changed with peer order: %v, %v", a, b)
	}
	if _, err := SelectReplicas("cluster", "bucket", "key", []string{"one", "one"}, 2); err == nil {
		t.Fatal("duplicate placement peer accepted")
	}
}

type failingReplica struct {
	stage   ReplicaStage
	stageOK bool
	storeOK bool
}

func (r failingReplica) Stage(ctx context.Context, id string, data []byte, digest [32]byte) error {
	if !r.stageOK {
		return errors.New("memory unavailable")
	}
	return r.stage.Stage(ctx, id, data, digest)
}

func (r failingReplica) Persist(ctx context.Context, id string) error {
	if !r.storeOK {
		return errors.New("durable store unavailable")
	}
	return r.stage.Persist(ctx, id)
}

func (r failingReplica) Release(ctx context.Context, id string) error {
	return r.stage.Release(ctx, id)
}

func TestAdmissionNeverPublishesBelowBothThresholds(t *testing.T) {
	ctx := context.Background()
	stores := make(map[string]*StageStore)
	for _, id := range []string{"one", "two", "three"} {
		store, err := OpenStageStore(filepath.Join(t.TempDir(), id+".db"), 1024)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		stores[id] = store
	}
	var published atomic.Int32
	publish := func(context.Context) error { published.Add(1); return nil }
	selected := []string{"one", "two", "three"}
	policy := AckPolicy{Memory: 2, Durable: 2}
	nodes := map[string]ReplicaStage{
		"one":   failingReplica{stage: stores["one"], stageOK: true, storeOK: true},
		"two":   failingReplica{stage: stores["two"], stageOK: true, storeOK: false},
		"three": failingReplica{stage: stores["three"], stageOK: false, storeOK: true},
	}
	memory, durable := 0, 0
	progress := func(m, d int) { memory, durable = m, d }
	if err := admitWrite(ctx, "op-1", []byte("payload"), selected, policy, nodes, publish, progress); !errors.Is(err, ErrInsufficientAcks) {
		t.Fatalf("expected durable threshold failure, got %v", err)
	}
	if memory != 2 || durable != 1 {
		t.Fatalf("failed write progress = %d memory, %d durable", memory, durable)
	}
	if published.Load() != 0 {
		t.Fatal("published below durable threshold")
	}
	nodes["two"] = failingReplica{stage: stores["two"], stageOK: true, storeOK: true}
	if err := admitWrite(ctx, "op-1", []byte("payload"), selected, policy, nodes, publish, progress); err != nil {
		t.Fatal(err)
	}
	if memory != 2 || durable != 2 {
		t.Fatalf("committed write progress = %d memory, %d durable", memory, durable)
	}
	if published.Load() != 1 {
		t.Fatalf("publish count = %d, want 1", published.Load())
	}
	for _, id := range []string{"one", "two"} {
		data, err := stores[id].Read(ctx, "op-1")
		if err != nil || string(data) != "payload" {
			t.Fatalf("node %s durable payload = %q, %v", id, data, err)
		}
	}
	if err := AdmitWrite(ctx, "op-2", []byte("payload"), []string{"one", "one"}, policy, nodes, publish); err == nil {
		t.Fatal("duplicate node counted toward thresholds")
	}
}

type stalledReplica struct{}

func (stalledReplica) Release(context.Context, string) error { return nil }

func (stalledReplica) Stage(ctx context.Context, _ string, _ []byte, _ [32]byte) error {
	<-ctx.Done()
	return ctx.Err()
}

func (stalledReplica) Persist(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestAdmissionUsesConfiguredThresholdWhenThirdReplicaStalls(t *testing.T) {
	stores := make([]*StageStore, 2)
	for i := range stores {
		store, err := OpenStageStore(filepath.Join(t.TempDir(), string(rune('a'+i))+".db"), 1024)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = store
		defer store.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var published atomic.Int32
	err := AdmitWrite(ctx, "two-of-three", []byte("payload"), []string{"a", "b", "c"}, AckPolicy{Memory: 2, Durable: 2}, map[string]ReplicaStage{
		"a": stores[0], "b": stores[1], "c": stalledReplica{},
	}, func(context.Context) error { published.Add(1); return nil })
	if err != nil || published.Load() != 1 {
		t.Fatalf("two healthy durable replicas must publish despite a stalled third: %v, published=%d", err, published.Load())
	}
	strictCtx, strictCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer strictCancel()
	err = AdmitWrite(strictCtx, "three-memory", []byte("payload"), []string{"a", "b", "c"}, AckPolicy{Memory: 3, Durable: 2}, map[string]ReplicaStage{
		"a": stores[0], "b": stores[1], "c": stalledReplica{},
	}, func(context.Context) error { published.Add(1); return nil })
	if !errors.Is(err, context.DeadlineExceeded) || published.Load() != 1 {
		t.Fatalf("three-memory policy must reject the same stalled replica: %v, published=%d", err, published.Load())
	}
}

func TestStageStoreVerifiesAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stage.db")
	store, err := OpenStageStore(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	digest := sha256.Sum256([]byte("data"))
	if err := store.Stage(ctx, "a", []byte("bad"), digest); err == nil {
		t.Fatal("corrupt payload accepted")
	}
	if err := store.Stage(ctx, "a", []byte("data"), digest); err != nil {
		t.Fatal(err)
	}
	if err := store.Stage(ctx, "b", []byte("x"), sha256.Sum256([]byte("x"))); !errors.Is(err, ErrStageFull) {
		t.Fatalf("expected memory limit, got %v", err)
	}
	if err := store.Stage(ctx, "a", []byte("different"), sha256.Sum256([]byte("different"))); err == nil {
		t.Fatal("operation ID reused with a different payload")
	}
	if err := store.Persist(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStageStore(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Stage(ctx, "a", []byte("data"), digest); err != nil {
		t.Fatalf("idempotent retry after restart: %v", err)
	}
	if err := store.Persist(ctx, "a"); err != nil {
		t.Fatalf("idempotent durable retry after restart: %v", err)
	}
	got, err := store.Read(ctx, "a")
	if err != nil || string(got) != "data" {
		t.Fatalf("recovered payload = %q, %v", got, err)
	}
	if err := store.Stage(ctx, "a", []byte("other"), sha256.Sum256([]byte("other"))); err == nil {
		t.Fatal("durable operation ID reused with a different payload")
	}
}

func TestStageStoreDoesNotAcknowledgeFailedCommit(t *testing.T) {
	store, err := OpenStageStore(filepath.Join(t.TempDir(), "stage.db"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Stage(ctx, "op", []byte("data"), sha256.Sum256([]byte("data"))); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx, "op"); err == nil {
		t.Fatal("closed persistent store returned a durable acknowledgment")
	}
}
