package multi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// kinded is a fake s3 backing over an inner backend (which must import
// versions): it holds native and wrapped buckets as far as the router can
// tell (storage.BucketCapabilities, BucketReleaser, HealthProber and
// KindReporter). Native buckets refuse imported versions and keep no
// history to copy; wrapped buckets can do everything. Wrapped buckets live
// in containers: the default one ("", only with home) and those of
// containers. Copies (CreateBucketOptions.Import without a kind) go to the
// default container when there is one.
type kinded struct {
	storage.Backend
	def        storage.BucketKind
	home       bool
	containers map[string]bool
	// gate, when set, makes CreateBucket wait for it to be closed (or for
	// its context to end) after recording the call; creating is closed
	// when the first call starts waiting.
	gate     chan struct{}
	creating chan struct{}
	once     sync.Once

	mu       sync.Mutex
	kinds    map[string]storage.BucketKind
	released map[string]bool
	created  map[string]storage.CreateBucketOptions // by bucket, as received
	probeErr error
	probes   int
}

func newKinded(inner storage.Backend, def storage.BucketKind, home bool, containers ...string) *kinded {
	k := &kinded{Backend: inner, def: def, home: home, containers: map[string]bool{},
		kinds: map[string]storage.BucketKind{}, released: map[string]bool{}, created: map[string]storage.CreateBucketOptions{}}
	for _, c := range containers {
		k.containers[c] = true
	}
	return k
}

var (
	_ storage.BucketCapabilities = (*kinded)(nil)
	_ storage.BucketReleaser     = (*kinded)(nil)
	_ storage.HealthProber       = (*kinded)(nil)
	_ storage.KindReporter       = (*kinded)(nil)
	_ storage.VersionImporter    = (*kinded)(nil)
)

func kindCaps(kind storage.BucketKind) storage.BucketCaps {
	if kind == storage.BucketKindNative {
		return storage.BucketCaps{Kind: kind, Clone: true, Versioning: true, ObjectLock: true}
	}
	return storage.BucketCaps{Kind: kind, Import: true, History: true, Clone: true, Versioning: true, ObjectLock: true, Compression: true}
}

// newKind is the kind of a bucket created with opts.
func (k *kinded) newKind(opts storage.CreateBucketOptions) (storage.BucketKind, error) {
	kind, t := k.def, opts.Target
	switch {
	case t != nil && t.Kind != "":
		kind = t.Kind
	case opts.Import && k.home:
		kind = storage.BucketKindWrapped
	}
	switch kind {
	case storage.BucketKindNative:
	case storage.BucketKindWrapped:
		c := ""
		if t != nil {
			c = t.Container
		}
		if (c == "" && !k.home) || (c != "" && !k.containers[c]) {
			return "", fmt.Errorf("%w: no container %q", storage.ErrInvalidTarget, c)
		}
	default:
		return "", fmt.Errorf("%w: kind %q", storage.ErrInvalidTarget, kind)
	}
	return kind, nil
}

func (k *kinded) BucketCaps(_ context.Context, bucket string) (storage.BucketCaps, error) {
	k.mu.Lock()
	kind, ok := k.kinds[bucket]
	k.mu.Unlock()
	if !ok {
		return storage.BucketCaps{}, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bucket)
	}
	return kindCaps(kind), nil
}

func (k *kinded) NewBucketCaps(_ context.Context, opts storage.CreateBucketOptions) (storage.BucketCaps, error) {
	kind, err := k.newKind(opts)
	if err != nil {
		return storage.BucketCaps{}, err
	}
	return kindCaps(kind), nil
}

func (k *kinded) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	kind, err := k.newKind(opts)
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.created[name] = opts
	k.mu.Unlock()
	if k.gate != nil {
		k.once.Do(func() { close(k.creating) })
		select {
		case <-k.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	inner := opts
	inner.Target, inner.Import = nil, false
	if err := k.Backend.CreateBucket(ctx, name, inner); err != nil {
		return err
	}
	k.mu.Lock()
	k.kinds[name] = kind
	k.mu.Unlock()
	return nil
}

func (k *kinded) creation(name string) (storage.CreateBucketOptions, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	opts, ok := k.created[name]
	return opts, ok
}

func (k *kinded) DeleteBucket(ctx context.Context, name string) error {
	if err := k.Backend.DeleteBucket(ctx, name); err != nil {
		return err
	}
	k.mu.Lock()
	delete(k.kinds, name)
	k.mu.Unlock()
	return nil
}

func (k *kinded) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	infos, err := k.Backend.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.DeleteFunc(infos, func(in storage.BucketInfo) bool { return k.released[in.Name] }), nil
}

func (k *kinded) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	k.mu.Lock()
	native := k.kinds[bucket] == storage.BucketKindNative
	k.mu.Unlock()
	if native {
		return nil, fmt.Errorf("native bucket %q: %w", bucket, errors.ErrUnsupported)
	}
	return k.Backend.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
}

func (k *kinded) ReleaseBucket(_ context.Context, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch kind, ok := k.kinds[name]; {
	case !ok:
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	case kind != storage.BucketKindNative:
		return fmt.Errorf("wrapped bucket %q: %w", name, storage.ErrNotSupported)
	}
	delete(k.kinds, name)
	k.released[name] = true
	return nil
}

func (k *kinded) ProbeHealth(context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.probes++
	return k.probeErr
}

func (k *kinded) setProbeErr(err error) {
	k.mu.Lock()
	k.probeErr = err
	k.mu.Unlock()
}

func (k *kinded) BucketKinds() ([]storage.BucketKind, storage.BucketKind) {
	if !k.home && len(k.containers) == 0 {
		return []storage.BucketKind{storage.BucketKindNative}, k.def
	}
	return []storage.BucketKind{storage.BucketKindNative, storage.BucketKindWrapped}, k.def
}

// bucketOutage wraps a backend and fails the calls on one bucket with
// storage.ErrBucketUnavailable while down is set, as an s3 backing does
// when that bucket's remote location fails; the backing itself (its
// listing and its other buckets) keeps working.
type bucketOutage struct {
	storage.Backend
	bucket string
	down   atomic.Bool
}

func (o *bucketOutage) err(bucket string) error {
	if bucket == o.bucket && o.down.Load() {
		return fmt.Errorf("remote bucket of %q: %w", bucket, storage.ErrBucketUnavailable)
	}
	return nil
}

func (o *bucketOutage) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	if err := o.err(name); err != nil {
		return storage.BucketInfo{}, err
	}
	return o.Backend.GetBucket(ctx, name)
}

func (o *bucketOutage) GetObjectInfo(ctx context.Context, bucket, key, v string) (storage.ObjectInfo, error) {
	if err := o.err(bucket); err != nil {
		return storage.ObjectInfo{}, err
	}
	return o.Backend.GetObjectInfo(ctx, bucket, key, v)
}

func (o *bucketOutage) OpenObject(ctx context.Context, bucket, key, v string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if err := o.err(bucket); err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	return o.Backend.OpenObject(ctx, bucket, key, v)
}

func (o *bucketOutage) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	if err := o.err(bucket); err != nil {
		return nil, err
	}
	return o.Backend.NewObjectWriter(ctx, bucket, key)
}

func (o *bucketOutage) DeleteObject(ctx context.Context, bucket, key, v string) (storage.DeleteResult, error) {
	if err := o.err(bucket); err != nil {
		return storage.DeleteResult{}, err
	}
	return o.Backend.DeleteObject(ctx, bucket, key, v)
}

func (o *bucketOutage) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if err := o.err(bucket); err != nil {
		return err
	}
	return o.Backend.WalkObjects(ctx, bucket, prefix, after, fn)
}

func (o *bucketOutage) WalkVersions(ctx context.Context, bucket, prefix, km, vm string, fn func(storage.ObjectInfo) error) error {
	if err := o.err(bucket); err != nil {
		return err
	}
	return o.Backend.WalkVersions(ctx, bucket, prefix, km, vm, fn)
}

func (o *bucketOutage) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	if err := o.err(bucket); err != nil {
		return nil, err
	}
	return o.Backend.GetBucketConfig(ctx, bucket, key)
}

func (o *bucketOutage) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	if err := o.err(bucket); err != nil {
		return nil, err
	}
	return o.Backend.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
}

// kindedRouter opens a router over a memory backing "a" (the default) and
// a kinded backing "s" (wrapped by default, with a home and container
// "c1").
func kindedRouter(t *testing.T, opts Options) (*Router, *kinded) {
	t.Helper()
	k := newKinded(memory.New(), storage.BucketKindWrapped, true, "c1")
	opts.Backings = append([]Backing{
		{Name: "a", Kind: "memory", Backend: memory.New()},
		{Name: "s", Kind: "s3", Backend: k},
	}, opts.Backings...)
	return openRouter(t, opts), k
}

// A native bucket can be neither replicated nor moved, and copies never go
// to a native bucket; a wrapped bucket on the same backing can be
// replicated.
func TestNativeBucketsAreNotCopied(t *testing.T) {
	ctx := context.Background()
	r, k := kindedRouter(t, Options{})
	native := &storage.BucketTarget{Kind: storage.BucketKindNative}
	if err := r.CreateBucket(ctx, "nat", storage.CreateBucketOptions{Backing: "s", Target: native}); err != nil {
		t.Fatal(err)
	}
	_, err := r.SetReplication(ctx, "nat", ModeActivePassive, []string{"a"})
	if !errors.Is(err, ErrInvalidPlacement) || !strings.Contains(err.Error(), `bucket "nat" on backing "s" is a native bucket`) {
		t.Fatalf("replica of a native bucket: %v", err)
	}
	if _, err := r.Move(ctx, "nat", "", "a", nil); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("move of a native bucket: %v", err)
	}
	if _, err := r.SetReplication(ctx, "nat", ModeSingle, nil); err != nil {
		t.Fatalf("single placement of a native bucket: %v", err)
	}

	if err := r.CreateBucket(ctx, "plain", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = r.Move(ctx, "plain", "", "s", native)
	if !errors.Is(err, ErrInvalidPlacement) || !strings.Contains(err.Error(), "native bucket") {
		t.Fatalf("move into a native bucket: %v", err)
	}
	if _, ok := k.creation("plain"); ok {
		t.Fatal("a refused move created the target bucket")
	}

	wrapped := &storage.BucketTarget{Kind: storage.BucketKindWrapped}
	if err := r.CreateBucket(ctx, "wr", storage.CreateBucketOptions{Backing: "s", Target: wrapped}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "wr", "k", "data", storage.ObjectMeta{})
	if _, err := r.SetReplication(ctx, "wr", ModeActivePassive, []string{"a"}); err != nil {
		t.Fatalf("replica of a wrapped bucket: %v", err)
	}
	waitState(t, r, "wr", StateInSync)
	a := r.topology.Load().backings["a"].Backend
	if got := read(t, a, "wr", "k", ""); got != "data" {
		t.Fatalf("replica of the wrapped bucket: %q", got)
	}
}

// Replicas, default replicas included, are created on a kinded backing to
// receive copies (Import, the backing's default location), and new buckets
// get default replicas only when their kind can be replicated.
func TestReplicasCreatedForImport(t *testing.T) {
	ctx := context.Background()
	// A backing creating native buckets by default can hold replicas when
	// it has a container for them, but cannot be the default backing of
	// buckets with default replicas.
	nativeDefault := func(home bool) *kinded { return newKinded(memory.New(), storage.BucketKindNative, home) }
	bk := func(home bool) []Backing {
		return []Backing{
			{Name: "a", Kind: "memory", Backend: memory.New()},
			{Name: "b", Kind: "memory", Backend: memory.New()},
			{Name: "s", Kind: "s3", Backend: nativeDefault(home)},
		}
	}
	_, err := Open(ctx, Options{Backings: bk(true), Default: "s", NewBucketReplicas: []string{"a"}})
	if !errors.Is(err, ErrInvalidPlacement) || !strings.Contains(err.Error(), "native") {
		t.Fatalf("Open with a native-default default backing and default replicas: %v", err)
	}
	if _, err := Open(ctx, Options{Backings: bk(false), NewBucketReplicas: []string{"s"}}); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("Open with a default replica that has no container for copies: %v", err)
	}

	backings := bk(true)
	k := backings[2].Backend.(*kinded)
	r := openRouter(t, Options{Backings: backings, NewBucketReplicas: []string{"s"}})
	if err := r.CreateBucket(ctx, "x", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "x", "k", "data", storage.ObjectMeta{})
	waitState(t, r, "x", StateInSync)
	if opts, ok := k.creation("x"); !ok || !opts.Import || opts.Target != nil {
		t.Fatalf("default replica created with %+v (created: %v)", opts, ok)
	}
	if caps, err := k.BucketCaps(ctx, "x"); err != nil || caps.Kind != storage.BucketKindWrapped {
		t.Fatalf("default replica's kind: %+v, %v", caps, err)
	}

	// A replica added later is created the same way.
	if err := r.CreateBucket(ctx, "y", storage.CreateBucketOptions{Backing: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "y", ModeActivePassive, []string{"s"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "y", StateInSync)
	if opts, ok := k.creation("y"); !ok || !opts.Import || opts.Target != nil {
		t.Fatalf("replica created with %+v (created: %v)", opts, ok)
	}

	// Buckets created on the kinded backing: a native one gets no default
	// replica, a wrapped one does.
	r2, k2 := kindedRouter(t, Options{Backings: []Backing{{Name: "b", Kind: "memory", Backend: memory.New()}}, NewBucketReplicas: []string{"b"}})
	if err := r2.CreateBucket(ctx, "n", storage.CreateBucketOptions{Backing: "s", Target: &storage.BucketTarget{Kind: storage.BucketKindNative}}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r2.Placement("n"); p.Mode != ModeSingle || len(p.Members) != 1 {
		t.Fatalf("native bucket with a default replica: %+v", p)
	}
	if err := r2.CreateBucket(ctx, "w", storage.CreateBucketOptions{Backing: "s"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r2.Placement("w"); p.Mode != ModeActivePassive || len(p.Members) != 2 || p.Members[1].Backing != "b" {
		t.Fatalf("wrapped bucket without its default replica: %+v", p)
	}
	if opts, _ := k2.creation("w"); opts.Import || opts.Target != nil {
		t.Fatalf("primary created with %+v", opts)
	}
}

// A bucket target needs a backing with bucket kinds and is checked before
// anything is created.
func TestBucketTargetChecked(t *testing.T) {
	ctx := context.Background()
	r, k := kindedRouter(t, Options{})
	wrapped := &storage.BucketTarget{Kind: storage.BucketKindWrapped}
	if err := r.CreateBucket(ctx, "x", storage.CreateBucketOptions{Backing: "a", Target: wrapped}); !errors.Is(err, storage.ErrInvalidTarget) {
		t.Fatalf("target on a backing without kinds: %v", err)
	}
	if err := r.CreateBucket(ctx, "x", storage.CreateBucketOptions{Backing: "s",
		Target: &storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: "nope"}}); !errors.Is(err, storage.ErrInvalidTarget) {
		t.Fatalf("unknown container: %v", err)
	}
	if _, ok := k.creation("x"); ok {
		t.Fatal("an invalid target reached CreateBucket")
	}
	if _, err := r.Placement("x"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("placement after refused creations: %v", err)
	}
	if _, err := r.topology.Load().backings["a"].Backend.GetBucket(ctx, "x"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("bucket created despite its target: %v", err)
	}
	if !r.Backings()[0].Healthy || !r.Backings()[1].Healthy {
		t.Fatal("an invalid target marked a backing unhealthy")
	}

	c1 := &storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: "c1"}
	if err := r.CreateBucket(ctx, "x", storage.CreateBucketOptions{Backing: "s", Target: c1}); err != nil {
		t.Fatal(err)
	}
	if opts, ok := k.creation("x"); !ok || opts.Target == nil || *opts.Target != *c1 || opts.Backing != "" {
		t.Fatalf("created with %+v", opts)
	}

	// Moves too.
	_, err := r.Move(ctx, "x", "", "a", wrapped)
	if !errors.Is(err, ErrInvalidPlacement) || !errors.Is(err, storage.ErrInvalidTarget) {
		t.Fatalf("move with a target to a backing without kinds: %v", err)
	}
	if err := r.CreateBucket(ctx, "y", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = r.Move(ctx, "y", "", "s", &storage.BucketTarget{Container: "nope"})
	if !errors.Is(err, ErrInvalidPlacement) || !errors.Is(err, storage.ErrInvalidTarget) {
		t.Fatalf("move to an unknown container: %v", err)
	}
}

// A move to a kinded backing creates the target for import, with the
// move's target, which is persisted with the move and survives a restart.
func TestMoveTargetPersisted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	state := filepath.Join(dir, "placements.json")
	da := filepath.Join(dir, "a")

	k1 := newKinded(memory.New(), storage.BucketKindNative, true, "c1")
	k1.gate, k1.creating = make(chan struct{}), make(chan struct{})
	r1, err := Open(ctx, Options{
		Backings:  []Backing{fsBacking(t, "a", da), {Name: "s", Kind: "s3", Backend: k1}},
		StatePath: state, Logger: testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r1.Close() })
	if err := r1.CreateBucket(ctx, "m", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, r1, "m", "k", "moved data", storage.ObjectMeta{})
	target := &storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: "c1"}
	st, err := r1.Move(ctx, "m", "", "s", target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Move == nil || st.Move.Target == nil || *st.Move.Target != *target {
		t.Fatalf("move status: %+v", st.Move)
	}
	target.Container = "changed by the caller" // the router keeps its own copy
	<-k1.creating
	if opts, _ := k1.creation("m"); !opts.Import || opts.Target == nil || opts.Target.Container != "c1" {
		t.Fatalf("move target created with %+v", opts)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	saved, err := loadState(state)
	if err != nil {
		t.Fatal(err)
	}
	if mv := saved.Buckets["m"].Move; mv == nil || mv.To != "s" || mv.Target == nil ||
		mv.Target.Kind != storage.BucketKindWrapped || mv.Target.Container != "c1" {
		t.Fatalf("saved move: %+v", mv)
	}

	k2 := newKinded(memory.New(), storage.BucketKindNative, true, "c1")
	r2 := openRouter(t, Options{Backings: []Backing{fsBacking(t, "a", da), {Name: "s", Kind: "s3", Backend: k2}}, StatePath: state})
	waitFor(t, "the move to complete after the restart", func() bool {
		p, err := r2.Placement("m")
		return err == nil && p.Move == nil && p.Primary == "s"
	})
	if opts, _ := k2.creation("m"); !opts.Import || opts.Target == nil || opts.Target.Container != "c1" {
		t.Fatalf("move target created with %+v after the restart", opts)
	}
	if caps, err := k2.BucketCaps(ctx, "m"); err != nil || caps.Kind != storage.BucketKindWrapped {
		t.Fatalf("moved bucket's kind: %+v, %v", caps, err)
	}
	if got := read(t, r2, "m", "k", ""); got != "moved data" {
		t.Fatalf("moved data: %q", got)
	}
}

// ReleaseBucket forgets a single-copy bucket its backing can let go of,
// keeping the data.
func TestReleaseBucket(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "placements.json")
	f := &faulty{Backend: memory.New()}
	r, k := kindedRouter(t, Options{Backings: []Backing{{Name: "f", Kind: "memory", Backend: f}}, StatePath: state})

	if err := r.CreateBucket(ctx, "nat", storage.CreateBucketOptions{Backing: "s", Target: &storage.BucketTarget{Kind: storage.BucketKindNative}}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "nat", "k", "kept", storage.ObjectMeta{})

	// Only a single copy can be released.
	if err := r.CreateBucket(ctx, "wr", storage.CreateBucketOptions{Backing: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "wr", ModeActivePassive, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseBucket(ctx, "wr"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "remove replicas first") {
		t.Fatalf("release of a replicated bucket: %v", err)
	}
	if _, err := r.SetReplication(ctx, "wr", ModeSingle, nil); err != nil {
		t.Fatal(err)
	}
	// The backing decides which buckets it can release.
	if err := r.ReleaseBucket(ctx, "wr"); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("release of a wrapped bucket: %v", err)
	}
	if err := r.CreateBucket(ctx, "plain", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseBucket(ctx, "plain"); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("release on a backing that cannot release: %v", err)
	}
	// An unavailable target cannot be checked for unrelated same-name data.
	f.down.Store(true)
	if _, err := r.Move(ctx, "plain", "", "f", nil); err == nil {
		t.Fatal("move accepted an unverified unavailable target")
	}
	if err := r.ReleaseBucket(ctx, "plain"); !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("failed move changed placement: %v", err)
	}
	if _, err := r.Placement("wr"); err != nil {
		t.Fatalf("refused releases changed the placement: %v", err)
	}

	if err := r.ReleaseBucket(ctx, "nat"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Placement("nat"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("placement of a released bucket: %v", err)
	}
	infos, err := r.ListBuckets(ctx)
	if err != nil || slices.Contains(bucketNames(infos), "nat") {
		t.Fatalf("ListBuckets after the release: %v, %v", bucketNames(infos), err)
	}
	r.mu.RLock()
	purging := r.purges[purgeRecord{Bucket: "nat", Backing: "s"}] != nil
	r.mu.RUnlock()
	if purging {
		t.Fatal("a released bucket is being deleted")
	}
	if got := read(t, k.Backend, "nat", "k", ""); got != "kept" {
		t.Fatalf("data of the released bucket: %q", got)
	}
	if !r.Backings()[1].Healthy {
		t.Fatal("releasing marked the backing unhealthy")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	saved, err := loadState(state)
	if err != nil || saved.Buckets["nat"] != nil || saved.Buckets["wr"] == nil {
		t.Fatalf("saved placements: %+v, %v", saved, err)
	}
	for _, p := range saved.Purges {
		if p.Bucket == "nat" {
			t.Fatalf("saved purge of a released bucket: %+v", p)
		}
	}
}

// The health check uses storage.HealthProber when the backend has it.
func TestProbeUsesHealthProber(t *testing.T) {
	r, k := kindedRouter(t, Options{})
	k.setProbeErr(fmt.Errorf("home unreachable: %w", storage.ErrBackingUnavailable))
	waitFor(t, "the failed probe to mark the backing unhealthy", func() bool {
		st := r.Backings()[1]
		return !st.Healthy && strings.Contains(st.LastError, "home unreachable")
	})
	k.setProbeErr(nil)
	waitFor(t, "the backing to recover", func() bool { return r.Backings()[1].Healthy })
	k.mu.Lock()
	probes := k.probes
	k.mu.Unlock()
	if probes < 2 {
		t.Fatalf("ProbeHealth called %d times", probes)
	}
}

// Backings reports the bucket kinds of backings that have them;
// BackingBackend returns a backing's backend.
func TestBackingKindsAndBackend(t *testing.T) {
	r, k := kindedRouter(t, Options{})
	st := r.Backings()
	if len(st[0].Kinds) != 0 || st[0].DefaultKind != "" {
		t.Fatalf("kinds of a memory backing: %+v", st[0])
	}
	if !slices.Equal(st[1].Kinds, []string{"native", "wrapped"}) || st[1].DefaultKind != "wrapped" {
		t.Fatalf("kinds of a kinded backing: %+v", st[1])
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), `"kinds"`) != 1 || strings.Count(string(data), `"defaultKind"`) != 1 {
		t.Fatalf("JSON: %s", data)
	}
	if b, ok := r.BackingBackend("s"); !ok || b != storage.Backend(k) {
		t.Fatalf("BackingBackend(s) = %v, %v", b, ok)
	}
	if _, ok := r.BackingBackend("nope"); ok {
		t.Fatal("BackingBackend of an unknown backing")
	}
}

// Failures of one bucket's remote location, refusals by a remote service
// and invalid targets are not failures of the backing.
func TestBucketErrorsAreNotBackingFailures(t *testing.T) {
	for _, s := range []error{storage.ErrBucketUnavailable, storage.ErrRemoteDenied, storage.ErrRemoteRejected, storage.ErrInvalidTarget, storage.ErrResourceLimit} {
		if isBackingError(fmt.Errorf("x: %w", s)) {
			t.Errorf("%v counted as a backing failure", s)
		}
	}
	if !isBackingError(fmt.Errorf("x: %w", storage.ErrBackingUnavailable)) {
		t.Error("ErrBackingUnavailable not counted as a backing failure")
	}
}

// Reads fall back to another copy when a bucket's remote location is
// unavailable, without marking its backing unhealthy.
func TestBucketUnavailableFallsBack(t *testing.T) {
	ctx := context.Background()
	o := &bucketOutage{Backend: memory.New(), bucket: "x"}
	r := openRouter(t, Options{
		Backings: []Backing{
			{Name: "a", Kind: "s3", Backend: o},
			{Name: "b", Kind: "memory", Backend: memory.New()},
		},
		NewBucketReplicas: []string{"b"},
	})
	for _, name := range []string{"x", "y"} {
		if err := r.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		put(t, r, name, "k", name+"-data", storage.ObjectMeta{})
		waitState(t, r, name, StateInSync)
	}
	o.down.Store(true)
	if got := read(t, r, "x", "k", ""); got != "x-data" {
		t.Fatalf("read with the primary's bucket unavailable: %q", got)
	}
	if _, err := r.GetBucket(ctx, "x"); err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	var keys []string
	if err := r.WalkObjects(ctx, "x", "", "", func(oi storage.ObjectInfo) error {
		keys = append(keys, oi.Key)
		return nil
	}); err != nil || !slices.Equal(keys, []string{"k"}) {
		t.Fatalf("WalkObjects: %v, %v", keys, err)
	}
	// Writes go to the primary, whose bucket is unavailable: 503, the
	// backing stays healthy and serves its other buckets.
	w, err := r.NewObjectWriter(ctx, "x", "k2")
	if err == nil {
		_ = w.Abort()
	}
	if !errors.Is(err, storage.ErrBucketUnavailable) || !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("write to the unavailable bucket: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // a few health checks
	if st := r.Backings()[0]; !st.Healthy {
		t.Fatalf("an unavailable bucket marked its backing unhealthy: %+v", st)
	}
	if p, _ := r.Placement("x"); p.Primary != "a" {
		t.Fatalf("an unavailable bucket failed over: %+v", p)
	}
	put(t, r, "y", "k2", "more", storage.ObjectMeta{})
	if got := read(t, r, "y", "k2", ""); got != "more" {
		t.Fatalf("other bucket on the backing: %q", got)
	}
}

// An active-active replica whose bucket becomes unavailable stops
// receiving synchronous writes (its backing stays healthy) and catches up
// once the bucket is back.
func TestActiveActiveReplicaBucketUnavailable(t *testing.T) {
	ctx := context.Background()
	o := &bucketOutage{Backend: memory.New(), bucket: "x"}
	r := openRouter(t, Options{
		Backings: []Backing{
			{Name: "a", Kind: "memory", Backend: memory.New()},
			{Name: "b", Kind: "s3", Backend: o},
		},
		NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b"},
	})
	if err := r.CreateBucket(ctx, "x", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "x", "k1", "one", storage.ObjectMeta{})
	waitState(t, r, "x", StateInSync)

	o.down.Store(true)
	put(t, r, "x", "k2", "two", storage.ObjectMeta{})
	p, err := r.Placement("x")
	if err != nil {
		t.Fatal(err)
	}
	if m := p.Members[1]; m.State == StateInSync || !m.Healthy {
		t.Fatalf("replica with an unavailable bucket: %+v", m)
	}
	if !r.Backings()[1].Healthy {
		t.Fatal("an unavailable bucket marked its backing unhealthy")
	}
	if got := read(t, r, "x", "k2", ""); got != "two" {
		t.Fatalf("read while the replica is demoted: %q", got)
	}
	o.down.Store(false)
	waitState(t, r, "x", StateInSync)
	waitSame(t, "x", r.topology.Load().backings["a"].Backend, o.Backend)
}
