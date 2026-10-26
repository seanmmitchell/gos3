package resticsnap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

func TestRepositoryLockBlocksMaintenanceDuringRestore(t *testing.T) {
	s := &Service{repoLocks: map[string]*repositoryGate{}}
	repo := s.repositoryLock(restic.Location{Repository: "repo"})
	releaseRead, err := repo.read(context.Background()) // restore holds this through its last read
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		l := s.repositoryLock(restic.Location{Repository: "repo"})
		release, err := l.write(context.Background()) // Forget and Prune acquire this lock
		if err == nil {
			release()
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("maintenance entered during restore")
	case <-time.After(20 * time.Millisecond):
	}
	other := s.repositoryLock(restic.Location{Repository: "other"})
	if other == repo {
		t.Fatal("different repositories share a lock")
	}
	releaseOther, err := other.write(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	releaseOther()
	releaseRead()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not resume after restore")
	}
}

func TestRepositoryLockWaitRespectsContext(t *testing.T) {
	repo := &repositoryGate{}
	readRelease, err := repo.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = repo.write(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("maintenance wait: got %v, want deadline exceeded", err)
	}
	// A canceled writer must not keep later restores waiting.
	otherReadRelease, err := repo.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	otherReadRelease()
	readRelease()

	writeRelease, err := repo.write(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = repo.read(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restore wait: got %v, want deadline exceeded", err)
	}
	writeRelease()
}

func TestMaintenanceWaitRespectsContext(t *testing.T) {
	s := &Service{repoLocks: map[string]*repositoryGate{}}
	loc := restic.Location{Repository: "repo"}
	release, err := s.repositoryLock(loc).read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.forget(ctx, loc, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Forget wait: got %v, want context canceled", err)
	}
	if err := s.prune(ctx, loc, restic.PruneOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prune wait: got %v, want context canceled", err)
	}
}
