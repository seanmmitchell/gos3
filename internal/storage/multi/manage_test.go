package multi

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

type pauseCopyDeletion struct {
	*memory.Backend
	release chan struct{}
}

func (b *pauseCopyDeletion) DeleteBucket(ctx context.Context, bucket string) error {
	select {
	case <-b.release:
		return b.Backend.DeleteBucket(ctx, bucket)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestPlacementConflictsWhileCopyIsBeingDeleted(t *testing.T) {
	for _, operation := range []string{"move", "replicate"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			bk := memBackings(2)
			pause := &pauseCopyDeletion{Backend: bk[1].Backend.(*memory.Backend), release: make(chan struct{})}
			bk[1].Backend = pause
			r := openRouter(t, Options{Backings: bk})
			release := sync.OnceFunc(func() { close(pause.release) })
			t.Cleanup(release)
			if err := r.CreateBucket(ctx, "bucket", storage.CreateBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			put(t, r, "bucket", "key", "keep me", storage.ObjectMeta{})
			if _, err := r.SetReplication(ctx, "bucket", ModeActivePassive, []string{"b"}); err != nil {
				t.Fatal(err)
			}
			waitState(t, r, "bucket", StateInSync)
			if _, err := r.SetReplication(ctx, "bucket", ModeSingle, nil); err != nil {
				t.Fatal(err)
			}
			change := func() error {
				if operation == "move" {
					_, err := r.Move(ctx, "bucket", "", "b", nil)
					return err
				}
				_, err := r.SetReplication(ctx, "bucket", ModeActivePassive, []string{"b"})
				return err
			}
			if err := change(); !errors.Is(err, ErrConflict) {
				t.Fatalf("%s while deleting the old copy: %v, want ErrConflict", operation, err)
			}
			release()
			waitFor(t, "old copy deletion and placement retry", func() bool {
				err := change()
				if err != nil && !errors.Is(err, ErrConflict) {
					t.Fatalf("%s after deleting the old copy: %v", operation, err)
				}
				return err == nil
			})
			waitState(t, r, "bucket", StateInSync)
			if got := read(t, bk[1].Backend, "bucket", "key", ""); got != "keep me" {
				t.Fatalf("new copy has %q, want original data", got)
			}
		})
	}
}

func TestPlacementConflictsAfterMoveTargetIsCreated(t *testing.T) {
	ctx := context.Background()
	bk := memBackings(2)
	pause := &pauseConfigRead{Backend: bk[0].Backend.(*memory.Backend), started: make(chan struct{}), release: make(chan struct{})}
	bk[0].Backend = pause
	r := openRouter(t, Options{Backings: bk})
	defer close(pause.release)
	if err := r.CreateBucket(ctx, "bucket", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	pause.armed.Store(true)
	if _, err := r.Move(ctx, "bucket", "", "b", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pause.started:
	case <-time.After(5 * time.Second):
		t.Fatal("move did not reach configuration sync")
	}
	if _, err := bk[1].Backend.GetBucket(ctx, "bucket"); err != nil {
		t.Fatalf("move target was not created: %v", err)
	}
	if _, err := r.Move(ctx, "bucket", "", "b", nil); !errors.Is(err, ErrConflict) {
		t.Errorf("second move after target creation: %v, want ErrConflict", err)
	}
	if _, err := r.SetReplication(ctx, "bucket", ModeActivePassive, []string{"b"}); !errors.Is(err, ErrConflict) {
		t.Errorf("replication during move: %v, want ErrConflict", err)
	}
}
