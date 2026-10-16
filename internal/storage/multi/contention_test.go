package multi

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Write contention on one user's own object is not a backing failure: it
// must neither mark the shared backing unhealthy nor fail over other
// buckets on it (H-04).
func TestContentionIsNotBackingFailure(t *testing.T) {
	a := fsBacking(t, "a", t.TempDir())
	r := openRouter(t, Options{
		Backings: []Backing{a, fsBacking(t, "b", t.TempDir())},
		// No periodic probes: a probe would hide a wrong setDown.
		HealthInterval: time.Hour,
	})
	ctx := context.Background()
	for _, name := range []string{"noisy", "victim"} {
		if err := r.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.SetReplication(ctx, "victim", ModeActiveActive, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "victim", StateInSync)
	info := put(t, r, "noisy", "k", "data", storage.ObjectMeta{})

	// Every attempt of the update is invalidated by a concurrent change to
	// the same version, as hundreds of parallel PutObjectTagging calls do,
	// until the fs backend gives up.
	n := 0
	_, err := r.UpdateObjectMeta(ctx, "noisy", "k", info.VersionID, func(m *storage.ObjectMeta) error {
		n++
		_, err := a.Backend.UpdateObjectMeta(ctx, "noisy", "k", info.VersionID, func(m *storage.ObjectMeta) error {
			m.Tags = map[string]string{"n": fmt.Sprint(n)}
			return nil
		})
		return err
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("contended UpdateObjectMeta = %v, want ErrConflict", err)
	}
	if isBackingError(err) {
		t.Fatal("ErrConflict counted as a backing failure")
	}
	time.Sleep(50 * time.Millisecond) // let a wrongly started failover run
	for _, b := range r.Backings() {
		if !b.Healthy || b.LastError != "" {
			t.Fatalf("backing %s healthy=%v lastError=%q after contention", b.Name, b.Healthy, b.LastError)
		}
	}
	if st, err := r.Placement("victim"); err != nil || st.Primary != "a" {
		t.Fatalf("victim placement after contention: %+v, %v", st, err)
	}
}
