package multi

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

type pauseConfigRead struct {
	*memory.Backend
	armed            atomic.Bool
	started, release chan struct{}
}

func (b *pauseConfigRead) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	data, err := b.Backend.GetBucketConfig(ctx, bucket, key)
	if key == storage.ConfigPolicy && b.armed.CompareAndSwap(true, false) {
		close(b.started)
		<-b.release
	}
	return data, err
}

func TestFullSyncDoesNotOverwriteNewerConfig(t *testing.T) {
	bk := memBackings(2)
	pause := &pauseConfigRead{Backend: bk[0].Backend.(*memory.Backend), started: make(chan struct{}), release: make(chan struct{})}
	bk[0].Backend = pause
	r := openRouter(t, Options{Backings: bk})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutBucketConfig(ctx, "vault", storage.ConfigPolicy, []byte("old")); err != nil {
		t.Fatal(err)
	}
	pause.armed.Store(true)
	if _, err := r.SetReplication(ctx, "vault", ModeActivePassive, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pause.started:
	case <-time.After(5 * time.Second):
		t.Fatal("full sync did not reach configuration")
	}
	result := make(chan error, 1)
	go func() { result <- r.PutBucketConfig(ctx, "vault", storage.ConfigPolicy, []byte("new")) }()
	var done bool
	select {
	case err := <-result:
		if err != nil {
			t.Error(err)
		}
		done = true
	case <-time.After(50 * time.Millisecond):
	}
	close(pause.release)
	if !done {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	waitState(t, r, "vault", StateInSync)
	for _, b := range bk {
		data, err := b.Backend.GetBucketConfig(ctx, "vault", storage.ConfigPolicy)
		if err != nil || string(data) != "new" {
			t.Fatalf("config rolled back on %s: %q %v", b.Name, data, err)
		}
	}
}

func TestReplicaRefusesUnrelatedSameNameBucket(t *testing.T) {
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := bk[1].Backend.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, bk[1].Backend, "vault", "unrelated", "keep me", storage.ObjectMeta{})
	if _, err := r.SetReplication(ctx, "vault", ModeActivePassive, []string{"b"}); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("replication accepted unrelated bucket: %v", err)
	}
	if got := read(t, bk[1].Backend, "vault", "unrelated", ""); got != "keep me" {
		t.Fatalf("unrelated data changed: %q", got)
	}
}

func TestDivergedSyncPreservesStrongerMetadataLock(t *testing.T) {
	r, a, b := lockedBucket(t)
	ctx := context.Background()
	v := put(t, r, "vault", "k", "data", storage.ObjectMeta{})
	waitSame(t, "vault", a, b)
	deadline := time.Now().Add(time.Hour)
	if _, err := b.UpdateObjectMeta(ctx, "vault", "k", v.VersionID, func(m *storage.ObjectMeta) error {
		m.LegalHold = true
		m.RetentionMode = "COMPLIANCE"
		m.RetainUntil = deadline
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	top := r.topology.Load()
	if err := r.syncKey(ctx, top.backings["a"], top.backings["b"], "vault", "k", true, nil); !errors.Is(err, errRestoredLocked) {
		t.Fatalf("lock merge = %v", err)
	}
	for _, backend := range []storage.Backend{a, b} {
		got, err := backend.GetObjectInfo(ctx, "vault", "k", v.VersionID)
		if err != nil || !got.LegalHold || got.RetentionMode != "COMPLIANCE" || !got.RetainUntil.Equal(deadline) {
			t.Fatalf("lock weakened: %+v, %v", got, err)
		}
	}
}

func TestForcedPromotionMarksSiblingCopiesDiverged(t *testing.T) {
	bk := memBackings(3)
	r := openRouter(t, Options{Backings: bk})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "vault", ModeActivePassive, []string{"b", "c"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "vault", StateInSync)
	r.mu.Lock()
	bs := r.buckets["vault"]
	r.promoteLocked(bs, 1, true)
	for _, m := range bs.rec.Members[1:] {
		if m.Synced || !m.Diverged {
			t.Errorf("sibling not protected: %+v", m)
		}
	}
	r.mu.Unlock()
}

func TestReconcileRefusesUnsyncedPrimaryReplacement(t *testing.T) {
	for _, missingBacking := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing bucket", true: "missing backing"}[missingBacking], func(t *testing.T) {
			bk := memBackings(2)
			ctx := context.Background()
			if err := bk[1].Backend.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "placements.json")
			st := &stateFile{Version: stateVersion, Buckets: map[string]*placementRecord{"vault": {Mode: ModeActivePassive, Members: []memberRecord{{Backing: "a", Synced: true}, {Backing: "b", Synced: false}}}}}
			if err := writeState(path, st); err != nil {
				t.Fatal(err)
			}
			if missingBacking {
				bk = bk[1:]
			}
			r, err := Open(ctx, Options{Backings: bk, Default: "b", StatePath: path})
			if err == nil {
				r.Close()
				t.Fatal("unsynchronized replica silently promoted")
			}
		})
	}
}

func TestInterruptedDeleteDoesNotPromoteReplica(t *testing.T) {
	bk := memBackings(2)
	ctx := context.Background()
	if err := bk[1].Backend.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "placements.json")
	st := &stateFile{
		Version: stateVersion,
		Buckets: map[string]*placementRecord{"vault": {
			Mode:    ModeActivePassive,
			Members: []memberRecord{{Backing: "a", Synced: true}, {Backing: "b", Synced: true}},
		}},
		Deleting: map[string]bool{"vault": true},
	}
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}
	r := openRouter(t, Options{Backings: bk, StatePath: path})
	if _, err := r.GetBucket(ctx, "vault"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("deleted bucket was resurrected: %v", err)
	}
	waitFor(t, "replica purged", func() bool {
		_, err := bk[1].Backend.GetBucket(ctx, "vault")
		return errors.Is(err, storage.ErrBucketNotFound)
	})
}

func TestInterruptedDeleteWaitsForUnknownPrimary(t *testing.T) {
	bk := memBackings(2)
	ctx := context.Background()
	if err := bk[1].Backend.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "placements.json")
	if err := writeState(path, &stateFile{
		Version: stateVersion,
		Buckets: map[string]*placementRecord{"vault": {
			Mode:    ModeActivePassive,
			Members: []memberRecord{{Backing: "a", Synced: true}, {Backing: "b", Synced: true}},
		}},
		Deleting: map[string]bool{"vault": true},
	}); err != nil {
		t.Fatal(err)
	}
	f := &faulty{Backend: bk[0].Backend}
	f.down.Store(true)
	bk[0].Backend = f
	r := openRouter(t, Options{Backings: bk, StatePath: path})
	if _, err := r.GetBucket(ctx, "vault"); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Fatalf("deletion intent was served from replica: %v", err)
	}
	f.down.Store(false)
	r.adoptPending(r.topology.Load().backings["a"])
	if _, err := r.GetBucket(ctx, "vault"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("deleted bucket was resurrected after primary recovered: %v", err)
	}
	waitFor(t, "replica purged", func() bool {
		_, err := bk[1].Backend.GetBucket(ctx, "vault")
		return errors.Is(err, storage.ErrBucketNotFound)
	})
}

func TestInterruptedDeleteDropsUnconfiguredReplicaBeforeRecovery(t *testing.T) {
	bk := memBackings(3)
	ctx := context.Background()
	if err := bk[0].Backend.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "placements.json")
	if err := writeState(path, &stateFile{
		Version: stateVersion,
		Buckets: map[string]*placementRecord{"vault": {
			Mode:    ModeActivePassive,
			Members: []memberRecord{{Backing: "a", Synced: true}, {Backing: "b", Synced: true}},
		}},
		Deleting: map[string]bool{"vault": true},
	}); err != nil {
		t.Fatal(err)
	}
	f := &faulty{Backend: bk[0].Backend}
	f.down.Store(true)
	bk[0].Backend = f
	r := openRouter(t, Options{Backings: []Backing{bk[0], bk[2]}, StatePath: path})
	if _, err := r.GetBucket(ctx, "vault"); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Fatalf("unresolved deletion was served: %v", err)
	}
	f.down.Store(false)
	r.adoptPending(r.topology.Load().backings["a"])
	if _, err := r.GetBucket(ctx, "vault"); err != nil {
		t.Fatalf("existing primary did not recover: %v", err)
	}
	placement, err := r.Placement("vault")
	if err != nil || placement.Mode != ModeSingle || len(placement.Members) != 1 || placement.Members[0].Backing != "a" {
		t.Fatalf("recovered placement = %+v, %v", placement, err)
	}
}

func TestDeleteRequiresDurableIntent(t *testing.T) {
	bk := memBackings(1)
	r := openRouter(t, Options{Backings: bk, StatePath: filepath.Join(t.TempDir(), "placements.json")})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	r.opts.StatePath = filepath.Join(t.TempDir(), "missing", "placements.json")
	if err := r.DeleteBucket(ctx, "vault"); err == nil {
		t.Fatal("delete succeeded without persisting its intent")
	}
	if _, err := bk[0].Backend.GetBucket(ctx, "vault"); err != nil {
		t.Fatalf("primary was deleted before intent persisted: %v", err)
	}
}

func TestPurgeDoesNotImportIntoRecreatedBucket(t *testing.T) {
	r, a, b := lockedBucket(t)
	ctx := context.Background()
	v := put(t, b, "vault", "old", "old tenant", storage.ObjectMeta{LegalHold: true})
	if err := a.DeleteBucket(ctx, "vault"); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateBucket(ctx, "vault", storage.CreateBucketOptions{Owner: "new-tenant"}); err != nil {
		t.Fatal(err)
	}
	if err := r.purgeLocked(ctx, r.topology.Load().backings["b"], "vault", v, true); !errors.Is(err, errLockedConflict) {
		t.Fatalf("purge = %v", err)
	}
	if hasVersion(t, a, "old", v.VersionID) || !hasVersion(t, b, "old", v.VersionID) {
		t.Fatal("old data imported into recreated bucket or removed")
	}
}

func TestDivergedCopyDoesNotUndoAuthorizedDelete(t *testing.T) {
	r, a, b := lockedBucket(t)
	ctx := context.Background()
	v := put(t, r, "vault", "k", "deleted", storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: time.Now().Add(time.Hour)})
	waitSame(t, "vault", a, b)
	if _, err := a.DeleteObject(storage.WithGovernanceBypass(ctx), "vault", "k", v.VersionID); err != nil {
		t.Fatal(err)
	}
	top := r.topology.Load()
	if err := r.syncKey(ctx, top.backings["a"], top.backings["b"], "vault", "k", true, nil); !errors.Is(err, errLockedConflict) {
		t.Fatalf("ambiguous delete silently reconciled: %v", err)
	}
	if hasVersion(t, a, "k", v.VersionID) || !hasVersion(t, b, "k", v.VersionID) {
		t.Fatal("deleted version resurrected or protected copy discarded")
	}
}

func TestLockedConflictNeverMarksMemberSynced(t *testing.T) {
	r, a, b := lockedBucket(t)
	ctx := context.Background()
	if err := r.SetBucketVersioning(ctx, "vault", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	waitSame(t, "vault", a, b)
	r.mu.RLock()
	memberState := r.buckets["vault"].members["b"]
	r.mu.RUnlock()
	waitFor(t, "the initial replica worker to finish", func() bool {
		memberState.mu.Lock()
		defer memberState.mu.Unlock()
		return !memberState.running
	})
	put(t, a, "vault", "k", "primary", storage.ObjectMeta{})
	put(t, b, "vault", "k", "conflict", storage.ObjectMeta{LegalHold: true})
	r.mu.Lock()
	bs := r.buckets["vault"]
	next := bs.rec.clone()
	next.Members[1].Synced = false
	next.Members[1].Diverged = true
	bs.rec = next
	ms := bs.members["b"]
	r.mu.Unlock()
	ms.mu.Lock()
	ms.needFull = true
	ms.fullDone = false
	ms.running = true
	ms.mu.Unlock()
	r.work(ms)
	r.mu.RLock()
	member := bs.rec.Members[1]
	r.mu.RUnlock()
	if member.Synced || !member.Diverged {
		t.Fatalf("conflict marked synchronized: %+v", member)
	}
	ms.mu.Lock()
	lastErr := ms.lastErr
	ms.mu.Unlock()
	if lastErr == "" {
		t.Fatal("conflict not visible to operators")
	}
}
