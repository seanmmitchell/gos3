package resticsnap

import (
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

func TestRepositoryLockBlocksMaintenanceDuringRestore(t *testing.T) {
	s := &Service{repoLocks: map[string]*sync.RWMutex{}}
	repo := s.repositoryLock(restic.Location{Repository: "repo"})
	repo.RLock() // restore holds this through its last read
	done := make(chan struct{})
	go func() {
		l := s.repositoryLock(restic.Location{Repository: "repo"})
		l.Lock() // Forget and Prune acquire this lock
		l.Unlock()
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
	other.Lock()
	other.Unlock()
	repo.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not resume after restore")
	}
}
