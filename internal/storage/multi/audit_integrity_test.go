package multi

import (
	"context"
	"errors"
	"fmt"
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

type retainedDeletionBacking struct {
	storage.Backend
	down, retained atomic.Bool
}

type afterDeleteBacking struct {
	storage.Backend
	afterDelete func()
}

func (b *afterDeleteBacking) BucketCaps(ctx context.Context, name string) (storage.BucketCaps, error) {
	return storage.CapsOf(ctx, b.Backend, name)
}

func (b *afterDeleteBacking) NewBucketCaps(ctx context.Context, opts storage.CreateBucketOptions) (storage.BucketCaps, error) {
	return storage.NewCapsOf(ctx, b.Backend, opts)
}

func (b *afterDeleteBacking) DeleteBucket(ctx context.Context, name string) error {
	if err := b.Backend.DeleteBucket(ctx, name); err != nil {
		return err
	}
	b.afterDelete()
	return nil
}

func (b *retainedDeletionBacking) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	if b.down.Load() {
		return nil, errFault
	}
	return nil, nil // the interrupted deletion is hidden from listings
}

func (b *retainedDeletionBacking) RetainsBucketForRecovery(name string) bool {
	return name == "vault" && b.retained.Load()
}

func (b *retainedDeletionBacking) ReleaseBucket(_ context.Context, name string) error {
	if !b.RetainsBucketForRecovery(name) {
		return storage.ErrBucketNotFound
	}
	b.retained.Store(false)
	return nil
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

func TestRecoveredBackingKeepsHiddenBucketReleasable(t *testing.T) {
	bk := memBackings(1)
	b := &retainedDeletionBacking{Backend: bk[0].Backend}
	b.down.Store(true)
	b.retained.Store(true)
	bk[0].Backend = b
	path := filepath.Join(t.TempDir(), "placements.json")
	if err := writeState(path, &stateFile{
		Version: stateVersion,
		Buckets: map[string]*placementRecord{"vault": {
			Mode: ModeSingle, Members: []memberRecord{{Backing: "a", Synced: true}},
		}},
		Deleting: map[string]bool{"vault": true},
	}); err != nil {
		t.Fatal(err)
	}
	r := openRouter(t, Options{Backings: bk, StatePath: path})
	if _, err := r.lookup("vault"); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Fatalf("unresolved deletion was served: %v", err)
	}
	b.down.Store(false)
	r.adoptPending(r.topology.Load().backings["a"])
	if _, err := r.lookup("vault"); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Fatalf("retained deletion became available after adoption: %v", err)
	}
	saved, err := loadState(path)
	if err != nil || !saved.Deleting["vault"] {
		t.Fatalf("recovered deletion intent = %+v, %v", saved, err)
	}
	if err := r.ReleaseBucket(context.Background(), "vault"); err != nil {
		t.Fatalf("hidden bucket could not be released after recovery: %v", err)
	}
	if b.retained.Load() {
		t.Fatal("backing retained the released bucket")
	}
}

func TestStartupRetainsHiddenDeletionIntent(t *testing.T) {
	for _, withIntent := range []bool{false, true} {
		t.Run(fmt.Sprint("saved intent=", withIntent), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "placements.json")
			st := &stateFile{Version: stateVersion, Buckets: map[string]*placementRecord{"vault": {
				Mode: ModeSingle, Members: []memberRecord{{Backing: "a", Synced: true}},
			}}}
			if withIntent {
				st.Deleting = map[string]bool{"vault": true}
			}
			if err := writeState(path, st); err != nil {
				t.Fatal(err)
			}
			open := func() (*Router, *retainedDeletionBacking) {
				b := &retainedDeletionBacking{Backend: memBackings(1)[0].Backend}
				b.retained.Store(true)
				return openRouter(t, Options{Backings: []Backing{{Name: "a", Backend: b}}, StatePath: path}), b
			}
			r, _ := open()
			if _, err := r.lookup("vault"); !errors.Is(err, storage.ErrBucketUnavailable) {
				t.Fatalf("retained deletion became available on startup: %v", err)
			}
			saved, err := loadState(path)
			if err != nil || !saved.Deleting["vault"] {
				t.Fatalf("startup deletion intent = %+v, %v", saved, err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r, b := open()
			if _, err := r.lookup("vault"); !errors.Is(err, storage.ErrBucketUnavailable) {
				t.Fatalf("retained deletion became available after another restart: %v", err)
			}
			if err := r.ReleaseBucket(context.Background(), "vault"); err != nil || b.retained.Load() {
				t.Fatalf("release after restart: %v, retained=%v", err, b.retained.Load())
			}
		})
	}
}

func TestDelayedAdoptionDiscoversRetainedDeletion(t *testing.T) {
	bk := memBackings(1)
	b := &retainedDeletionBacking{Backend: bk[0].Backend}
	b.down.Store(true)
	bk[0].Backend = b
	path := filepath.Join(t.TempDir(), "placements.json")
	if err := writeState(path, &stateFile{Version: stateVersion, Buckets: map[string]*placementRecord{"vault": {
		Mode: ModeSingle, Members: []memberRecord{{Backing: "a", Synced: true}},
	}}}); err != nil {
		t.Fatal(err)
	}
	r := openRouter(t, Options{Backings: bk, StatePath: path})
	b.retained.Store(true)
	b.down.Store(false)
	r.adoptPending(r.topology.Load().backings["a"])
	if _, err := r.lookup("vault"); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Fatalf("newly discovered tombstone became available: %v", err)
	}
	saved, err := loadState(path)
	if err != nil || !saved.Deleting["vault"] {
		t.Fatalf("newly discovered deletion intent = %+v, %v", saved, err)
	}
	if err := r.ReleaseBucket(context.Background(), "vault"); err != nil {
		t.Fatalf("release discovered tombstone: %v", err)
	}
}

func TestRetainedDeletionNeverServesReplica(t *testing.T) {
	bk := memBackings(2)
	b := &retainedDeletionBacking{Backend: bk[0].Backend}
	b.retained.Store(true)
	bk[0].Backend = b
	ctx := context.Background()
	if err := bk[1].Backend.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "placements.json")
	if err := writeState(path, &stateFile{Version: stateVersion, Buckets: map[string]*placementRecord{"vault": {
		Mode: ModeActivePassive, Members: []memberRecord{{Backing: "a", Synced: true}, {Backing: "b", Synced: true}},
	}}, Deleting: map[string]bool{"vault": true}}); err != nil {
		t.Fatal(err)
	}
	r := openRouter(t, Options{Backings: bk, StatePath: path})
	if _, err := r.GetBucket(ctx, "vault"); !errors.Is(err, storage.ErrBucketUnavailable) {
		t.Fatalf("retained deletion served a replica: %v", err)
	}
	if _, err := bk[1].Backend.GetBucket(ctx, "vault"); err != nil {
		t.Fatalf("retained replica was purged: %v", err)
	}
	if err := r.ReleaseBucket(ctx, "vault"); !errors.Is(err, ErrConflict) {
		t.Fatalf("release with a replica: %v, want conflict", err)
	}
}

func TestInterruptedDeleteWithLivePrimaryRecovers(t *testing.T) {
	bk := memBackings(1)
	ctx := context.Background()
	if err := bk[0].Backend.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "placements.json")
	if err := writeState(path, &stateFile{Version: stateVersion, Buckets: map[string]*placementRecord{"vault": {
		Mode: ModeSingle, Members: []memberRecord{{Backing: "a", Synced: true}},
	}}, Deleting: map[string]bool{"vault": true}}); err != nil {
		t.Fatal(err)
	}
	r := openRouter(t, Options{Backings: bk, StatePath: path})
	if _, err := r.GetBucket(ctx, "vault"); err != nil {
		t.Fatalf("live primary did not recover: %v", err)
	}
	saved, err := loadState(path)
	if err != nil || saved.Deleting["vault"] {
		t.Fatalf("live primary kept deletion intent: %+v, %v", saved, err)
	}
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

func TestDeleteContinuesAfterFinalStateSaveFails(t *testing.T) {
	bk := []Backing{
		fsBacking(t, "a", filepath.Join(t.TempDir(), "a")),
		fsBacking(t, "b", filepath.Join(t.TempDir(), "b")),
	}
	statePath := filepath.Join(t.TempDir(), "placements.json")
	var failFinal atomic.Bool
	bk[0].Backend = &afterDeleteBacking{Backend: bk[0].Backend, afterDelete: func() {
		// The first save has succeeded; only the post-delete save fails.
		failFinal.Store(true)
	}}
	r := openRouter(t, Options{Backings: bk, StatePath: statePath, NewBucketMode: ModeActivePassive, NewBucketReplicas: []string{"b"}})
	writer := stateWriter(func(path string, st *stateFile) error {
		if failFinal.Load() {
			return errors.New("injected final save failure")
		}
		return writeState(path, st)
	})
	r.stateWriter.Store(&writer)
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "vault", StateInSync)
	var changes changeLog
	unsubscribe := r.Subscribe(changes.add)
	defer unsubscribe()
	if err := r.DeleteBucket(ctx, "vault"); err != nil {
		t.Fatalf("completed delete failed because of the final state save: %v", err)
	}
	changes.expect(t, "completed delete", Change{Kind: ChangeBucketDeleted, Bucket: "vault"})
	waitFor(t, "replica purge after the final save failed", func() bool {
		_, err := bk[1].Backend.GetBucket(ctx, "vault")
		return errors.Is(err, storage.ErrBucketNotFound)
	})
	st, err := loadState(statePath)
	if err != nil || !st.Deleting["vault"] {
		t.Fatalf("durable deletion intent = %+v, %v", st, err)
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

func TestMoveKeepsSourceUntilPlacementIsSaved(t *testing.T) {
	bk := memBackings(2)
	statePath := filepath.Join(t.TempDir(), "placements.json")
	r := openRouter(t, Options{Backings: bk, StatePath: statePath})
	var failCutover atomic.Bool
	var refused atomic.Int32
	failCutover.Store(true)
	writer := stateWriter(func(path string, st *stateFile) error {
		if rec := st.Buckets["m"]; failCutover.Load() && rec != nil && rec.Move == nil && rec.primary() == "b" {
			refused.Add(1)
			return errors.New("injected cutover save failure")
		}
		return writeState(path, st)
	})
	r.stateWriter.Store(&writer)
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "m", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "m", "k", "data", storage.ObjectMeta{})
	if _, err := r.Move(ctx, "m", "", "b", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cutover save to fail", func() bool { return refused.Load() > 0 })
	waitFor(t, "the failed cutover to be undone", func() bool {
		st, err := r.Placement("m")
		return err == nil && st.Primary == "a" && st.Move != nil
	})
	if got := read(t, bk[0].Backend, "m", "k", ""); got != "data" {
		t.Fatalf("source copy after the failed cutover = %q", got)
	}
	saved, err := loadState(statePath)
	if err != nil || saved.Buckets["m"] == nil || saved.Buckets["m"].Move == nil || saved.Buckets["m"].primary() != "a" {
		t.Fatalf("saved placement after the failed cutover = %+v, %v", saved, err)
	}
	put(t, r, "m", "k2", "after", storage.ObjectMeta{})

	failCutover.Store(false)
	waitFor(t, "the retried cutover", func() bool {
		st, err := r.Placement("m")
		return err == nil && st.Primary == "b" && st.Move == nil
	})
	waitFor(t, "the source copy to be deleted", func() bool {
		_, err := bk[0].Backend.GetBucket(ctx, "m")
		return errors.Is(err, storage.ErrBucketNotFound)
	})
	if got := read(t, r, "m", "k2", ""); got != "after" {
		t.Fatalf("write during the failed cutover = %q", got)
	}
	saved, err = loadState(statePath)
	if err != nil || saved.Buckets["m"] == nil || saved.Buckets["m"].Move != nil || saved.Buckets["m"].primary() != "b" {
		t.Fatalf("saved placement after the move = %+v, %v", saved, err)
	}
}

func TestFailoverPlacementSaveIsRetried(t *testing.T) {
	fa := &faulty{Backend: memory.New()}
	statePath := filepath.Join(t.TempDir(), "placements.json")
	r := openRouter(t, Options{
		Backings:      []Backing{{Name: "a", Backend: fa}, {Name: "b", Backend: memory.New()}},
		NewBucketMode: ModeActivePassive, NewBucketReplicas: []string{"b"},
		AutoFailover: true, StatePath: statePath,
	})
	var failSaves atomic.Bool
	writer := stateWriter(func(path string, st *stateFile) error {
		if failSaves.Load() {
			return errors.New("injected save failure")
		}
		return writeState(path, st)
	})
	r.stateWriter.Store(&writer)
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "ap", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "ap", "k", "data", storage.ObjectMeta{})
	waitState(t, r, "ap", StateInSync)

	failSaves.Store(true)
	fa.down.Store(true)
	waitFor(t, "automatic failover", func() bool {
		st, _ := r.Placement("ap")
		return st.Primary == "b"
	})
	if saved, err := loadState(statePath); err != nil || saved.Buckets["ap"].primary() != "a" {
		t.Fatalf("saved placement while saves fail = %+v, %v", saved, err)
	}
	// Nothing else saves the placements while the primary stays down: only
	// the retry can persist the failover.
	failSaves.Store(false)
	waitFor(t, "the failover to be saved", func() bool {
		saved, err := loadState(statePath)
		return err == nil && saved.Buckets["ap"] != nil && saved.Buckets["ap"].primary() == "b"
	})
}
