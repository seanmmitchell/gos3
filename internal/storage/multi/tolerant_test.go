package multi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// hanging is a backend whose ListBuckets blocks until its context ends.
type hanging struct{ storage.Backend }

func (hanging) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// noImporter is a backend without storage.VersionImporter (and without
// storage.BucketCapabilities): its buckets can neither receive copies nor
// be copied from.
type noImporter struct{ storage.Backend }

// unsupported is a backend that refuses versioning with
// storage.ErrNotSupported, like some remote S3 services.
type unsupported struct{ storage.Backend }

func (unsupported) SetBucketVersioning(context.Context, string, storage.VersioningStatus) error {
	return fmt.Errorf("versioning: %w", storage.ErrNotSupported)
}

func bucketNames(infos []storage.BucketInfo) []string {
	out := make([]string, 0, len(infos))
	for _, in := range infos {
		out = append(out, in.Name)
	}
	return out
}

// A backing that cannot be listed at startup does not fail Open: it starts
// unhealthy, its placements are kept, the other buckets are served, and
// its unplaced buckets are adopted once it recovers.
func TestOpenWithUnreachableBacking(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	state := filepath.Join(dir, "placements.json")
	da, db := filepath.Join(dir, "a"), filepath.Join(dir, "b")

	r, err := Open(ctx, Options{Backings: []Backing{fsBacking(t, "a", da), fsBacking(t, "b", db)}, StatePath: state, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ bucket, backing string }{{"ona", "a"}, {"onb", "b"}, {"rep", "a"}} {
		if err := r.CreateBucket(ctx, c.bucket, storage.CreateBucketOptions{Backing: c.backing}); err != nil {
			t.Fatal(err)
		}
		put(t, r, c.bucket, "k", c.bucket+"-data", storage.ObjectMeta{})
	}
	if _, err := r.SetReplication(ctx, "rep", ModeActivePassive, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "rep", StateInSync)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// A bucket created on b behind the router's back.
	pre := fsBacking(t, "x", db)
	if err := pre.Backend.CreateBucket(ctx, "legacy", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = pre.Backend.Close()

	fb := fsBacking(t, "b", db)
	f := &faulty{Backend: fb.Backend}
	f.down.Store(true)
	fb.Backend = f
	r2 := openRouter(t, Options{Backings: []Backing{fsBacking(t, "a", da), fb}, StatePath: state, OpenListTimeout: time.Second})

	if st := r2.Backings(); st[1].Healthy || !strings.Contains(st[1].LastError, errFault.Error()) {
		t.Fatalf("backing b after Open: %+v", st[1])
	}
	if p, err := r2.Placement("onb"); err != nil || p.Primary != "b" {
		t.Fatalf("placement on the unreachable backing: %+v, %v", p, err)
	}
	r2.mu.RLock()
	rec := r2.buckets["rep"].rec
	r2.mu.RUnlock()
	if rec.Mode != ModeActivePassive || len(rec.Members) != 2 || !rec.Members[1].Synced {
		t.Fatalf("replica on the unreachable backing changed: %+v", rec)
	}
	if got := read(t, r2, "ona", "k", ""); got != "ona-data" {
		t.Fatalf("read from the healthy backing: %q", got)
	}
	if got := read(t, r2, "rep", "k", ""); got != "rep-data" {
		t.Fatalf("read of the replicated bucket: %q", got)
	}
	infos, err := r2.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets with a backing down: %v", err)
	}
	if names := bucketNames(infos); !slices.Equal(names, []string{"ona", "rep"}) {
		t.Fatalf("ListBuckets = %v", names)
	}
	// Names without a placement may be taken on b: they are unavailable,
	// not missing, and cannot be created elsewhere meanwhile.
	if _, err := r2.Placement("legacy"); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("bucket of an unlisted backing: %v", err)
	}
	if _, err := r2.GetBucket(ctx, "nothing"); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("GetBucket of an unknown name while b is unlisted: %v", err)
	}
	if err := r2.CreateBucket(ctx, "legacy", storage.CreateBucketOptions{Backing: "a"}); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("CreateBucket shadowing a bucket of the unlisted backing: %v", err)
	}

	f.down.Store(false)
	waitFor(t, "the unplaced bucket to be adopted", func() bool {
		p, err := r2.Placement("legacy")
		return err == nil && p.Primary == "b"
	})
	if got := read(t, r2, "onb", "k", ""); got != "onb-data" {
		t.Fatalf("read after recovery: %q", got)
	}
	if _, err := r2.GetBucket(ctx, "nothing"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("GetBucket of an unknown name after adoption: %v", err)
	}
	// Read the state file once the router is closed: on Windows, reading
	// it while the router replaces it makes that save fail.
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(state)
	if err != nil || st.Buckets["legacy"] == nil || st.Buckets["onb"] == nil {
		t.Fatalf("saved placements: %+v, %v", st, err)
	}
}

// Open works when no backing answers, also when one hangs.
func TestOpenWithAllBackingsUnreachable(t *testing.T) {
	f := &faulty{Backend: memory.New()}
	f.down.Store(true)
	r := openRouter(t, Options{
		Backings:        []Backing{{Name: "a", Kind: "memory", Backend: hanging{memory.New()}}, {Name: "b", Kind: "memory", Backend: f}},
		OpenListTimeout: 100 * time.Millisecond,
	})
	st := r.Backings()
	if st[0].Healthy || !strings.Contains(st[0].LastError, "timed out") || st[1].Healthy {
		t.Fatalf("Backings() = %+v", st)
	}
	infos, err := r.ListBuckets(context.Background())
	if err != nil || len(infos) != 0 {
		t.Fatalf("ListBuckets = %v, %v", infos, err)
	}
	f.down.Store(false)
	waitFor(t, "b to recover", func() bool { return r.Backings()[1].Healthy })
}

// The cancellation of Open's context still fails Open.
func TestOpenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Open(ctx, Options{Backings: []Backing{{Name: "a", Kind: "memory", Backend: hanging{memory.New()}}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open with a canceled context: %v", err)
	}
}

// ListBuckets reports a bucket whose backing is down as last seen, and
// leaves out one it never saw, instead of failing.
func TestListBucketsWithBackingDown(t *testing.T) {
	ctx := context.Background()
	f := &faulty{Backend: memory.New()}
	r := openRouter(t, Options{Backings: []Backing{{Name: "a", Kind: "memory", Backend: memory.New()}, {Name: "b", Kind: "memory", Backend: f}},
		HealthInterval: time.Hour})
	for _, c := range []struct{ bucket, backing string }{{"x", "a"}, {"y", "b"}} {
		if err := r.CreateBucket(ctx, c.bucket, storage.CreateBucketOptions{Backing: c.backing}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.ListBuckets(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateBucket(ctx, "w", storage.CreateBucketOptions{Backing: "b"}); err != nil {
		t.Fatal(err)
	}
	f.down.Store(true)
	infos, err := r.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets with a backing down: %v", err)
	}
	if names := bucketNames(infos); !slices.Equal(names, []string{"x", "y"}) {
		t.Fatalf("ListBuckets = %v", names)
	}
	if infos[1].Backing != "b" {
		t.Fatalf("cached bucket info: %+v", infos[1])
	}
	if r.Backings()[1].Healthy {
		t.Fatal("failed listing did not mark the backing unhealthy")
	}
}

// Answers about what a backing supports are not backing failures.
func TestNotSupportedIsNotBackingFailure(t *testing.T) {
	ctx := context.Background()
	r := openRouter(t, Options{Backings: []Backing{{Name: "a", Kind: "memory", Backend: unsupported{memory.New()}}}})
	if err := r.CreateBucket(ctx, "x", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBucketVersioning(ctx, "x", storage.VersioningEnabled); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("SetBucketVersioning: %v", err)
	}
	if !r.Backings()[0].Healthy {
		t.Fatal("ErrNotSupported marked the backing unhealthy")
	}
	if isBackingError(fmt.Errorf("x: %w", storage.ErrInvalidBacking)) {
		t.Fatal("ErrInvalidBacking counted as a backing failure")
	}
	if !isBackingError(fmt.Errorf("x: %w", storage.ErrBackingUnavailable)) {
		t.Fatal("ErrBackingUnavailable not counted as a backing failure")
	}
}

// A backing without storage.VersionImporter can neither hold replicas nor
// receive moves, and the buckets on it can be neither replicated nor
// moved.
func TestNonImportingBackingRefusedAsCopyTarget(t *testing.T) {
	ctx := context.Background()
	bk := func() []Backing {
		return []Backing{
			{Name: "a", Kind: "memory", Backend: memory.New()},
			{Name: "b", Kind: "memory", Backend: memory.New()},
			{Name: "g", Kind: "memory", Backend: noImporter{memory.New()}},
		}
	}
	if _, err := Open(ctx, Options{Backings: bk(), NewBucketReplicas: []string{"g"}}); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("Open with a non-importing default replica: %v", err)
	}
	if _, err := Open(ctx, Options{Backings: bk(), Default: "g", NewBucketReplicas: []string{"a"}}); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("Open with a non-importing default backing and default replicas: %v", err)
	}
	r := openRouter(t, Options{Backings: bk(), NewBucketReplicas: []string{"b"}})
	if err := r.CreateBucket(ctx, "x", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "x", ModeActivePassive, []string{"b", "g"}); !errors.Is(err, ErrInvalidPlacement) ||
		!strings.Contains(err.Error(), "cannot receive copied versions") {
		t.Fatalf("non-importing replica: %v", err)
	}
	if _, err := r.Move(ctx, "x", "", "g", nil); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("move to a non-importing backing: %v", err)
	}
	// A bucket created on the non-importing backing gets no default
	// replica.
	if err := r.CreateBucket(ctx, "gw", storage.CreateBucketOptions{Backing: "g"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.Placement("gw"); p.Mode != ModeSingle || len(p.Members) != 1 {
		t.Fatalf("bucket on the non-importing backing: %+v", p)
	}
	if _, err := r.SetReplication(ctx, "gw", ModeActivePassive, []string{"a"}); !errors.Is(err, ErrInvalidPlacement) ||
		!strings.Contains(err.Error(), "no version history") {
		t.Fatalf("replica of a bucket on a non-importing backing: %v", err)
	}
	if _, err := r.Move(ctx, "gw", "", "a", nil); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("move of a bucket on a non-importing backing: %v", err)
	}
	if _, err := r.SetReplication(ctx, "gw", ModeSingle, nil); err != nil {
		t.Fatalf("single placement of a bucket on a non-importing backing: %v", err)
	}
}
