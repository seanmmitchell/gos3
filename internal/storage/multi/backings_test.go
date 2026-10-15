package multi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func managedRouter(t *testing.T, path string) *Router {
	t.Helper()
	r := openRouter(t, Options{Backings: []Backing{{Name: "default", Kind: "memory", Backend: memory.New()}}})
	r.EnableBackingManagement(config.StorageConfig{Backend: "memory"}, path, BackingOpener{Open: func(d config.BackingDefinition) (Backing, error) {
		return Backing{Name: d.Name, Kind: d.Backend, Path: d.Path, Backend: memory.New()}, nil
	}})
	return r
}

func TestConfigureBackingsLive(t *testing.T) {
	ctx := context.Background()
	r := managedRouter(t, "")
	in, _ := r.BackingConfiguration()
	in.Backings = append(in.Backings, config.BackingDefinition{Name: "fast", Backend: "memory"})
	in.DefaultBacking = "fast"
	out, err := r.ConfigureBackings(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Backings()) != 2 || r.DefaultBacking() != "fast" {
		t.Fatal("topology was not applied")
	}
	if _, err := r.ConfigureBackings(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if err := r.CreateBucket(ctx, "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	p, _ := r.Placement("photos")
	if p.Primary != "fast" {
		t.Fatalf("new bucket on %q", p.Primary)
	}
	out.DefaultBacking = "default"
	out.Backings = nil
	if _, err := r.ConfigureBackings(ctx, out); !errors.Is(err, ErrConflict) {
		t.Fatalf("removed used backing: %v", err)
	}
	out, _ = r.BackingConfiguration()
	out.DefaultBacking = "default"
	if _, err := r.ConfigureBackings(ctx, out); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.Placement("photos"); p.Primary != "fast" {
		t.Fatal("default change moved existing bucket")
	}
}

func TestConfigureBackingsRejectsReferences(t *testing.T) {
	for _, kind := range []string{"replica", "move", "purge", "untracked"} {
		t.Run(kind, func(t *testing.T) {
			r := managedRouter(t, "")
			ctx := context.Background()
			in, _ := r.BackingConfiguration()
			in.Backings = []config.BackingDefinition{{Name: "fast", Backend: "memory"}}
			in, err := r.ConfigureBackings(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			switch kind {
			case "replica":
				r.buckets["photos"] = &bucketState{rec: &placementRecord{Members: []memberRecord{{Backing: "default"}, {Backing: "fast"}}}}
			case "move":
				r.buckets["photos"] = &bucketState{rec: &placementRecord{Members: []memberRecord{{Backing: "default"}}, Move: &moveRecord{To: "fast"}}}
			case "purge":
				r.purges[purgeRecord{Bucket: "photos", Backing: "fast"}] = &purgeJob{}
			case "untracked":
				_ = r.topology.Load().backings["fast"].Backend.CreateBucket(ctx, "photos", storage.CreateBucketOptions{})
			}
			r.mu.Unlock()
			in.Backings = nil
			_, err = r.ConfigureBackings(ctx, in)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("removed %s: %v", kind, err)
			}
			// Synthetic references above are only for validation, not worker state.
			r.mu.Lock()
			clear(r.buckets)
			clear(r.purges)
			r.mu.Unlock()
		})
	}
}

func TestConfigureBackingsPersistenceFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(path, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	r := managedRouter(t, filepath.Join(path, "backings.json"))
	in, _ := r.BackingConfiguration()
	in.Backings = []config.BackingDefinition{{Name: "fast", Backend: "memory"}}
	if _, err := r.ConfigureBackings(context.Background(), in); err == nil {
		t.Fatal("save succeeded through file parent")
	}
	if len(r.Backings()) != 1 {
		t.Fatal("failed save published topology")
	}
	got, _ := r.BackingConfiguration()
	if got.Revision != in.Revision {
		t.Fatal("failed save changed revision")
	}
}

func TestConfigureBackingsConcurrentReaders(t *testing.T) {
	r := managedRouter(t, "")
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				_ = r.Backings()
				_ = r.DefaultBacking()
				_, _ = r.ListBuckets(ctx)
			}
		})
	}
	for i := range 50 {
		in, _ := r.BackingConfiguration()
		in.Backings = []config.BackingDefinition{{Name: fmt.Sprintf("disk%d", i), Backend: "memory"}}
		in.DefaultBacking = in.Backings[0].Name
		if _, err := r.ConfigureBackings(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestRemovedBackingDoesNotRerouteOldBucket(t *testing.T) {
	r := managedRouter(t, "")
	ctx := context.Background()
	in, _ := r.BackingConfiguration()
	in.Backings = []config.BackingDefinition{{Name: "fast", Backend: "memory"}}
	in.DefaultBacking = "fast"
	in, err := r.ConfigureBackings(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateBucket(ctx, "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	oldBucket, _ := r.lookup("photos")
	oldBacking := r.readers(oldBucket, false)[0]
	if err := r.DeleteBucket(ctx, "photos"); err != nil {
		t.Fatal(err)
	}
	in.Backings = nil
	in.DefaultBacking = "default"
	in, err = r.ConfigureBackings(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Backings = []config.BackingDefinition{{Name: "fast", Backend: "memory"}}
	if _, err := r.ConfigureBackings(ctx, in); err != nil {
		t.Fatal(err)
	}
	// Requests that looked up the bucket before deletion retain its original
	// backing, even after that name has been reused for a different backend.
	if got := r.readers(oldBucket, false)[0]; got != oldBacking {
		t.Fatal("old request was routed to a replacement backing")
	}
}
