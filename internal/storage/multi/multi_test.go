package multi

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/storagetest"
)

// The router must be a conforming backend in every mode.

func TestConformanceSingle(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		return openRouter(t, Options{Backings: memBackings(2), Default: "b"})
	})
}

func TestConformanceActivePassive(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		return openRouter(t, Options{Backings: memBackings(2), NewBucketMode: ModeActivePassive, NewBucketReplicas: []string{"b"}})
	})
}

func TestConformanceActiveActive(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		return openRouter(t, Options{Backings: memBackings(3), NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b", "c"}})
	})
}

// exercise makes a varied history in bucket through r: versions, delete
// markers, permanent deletes, metadata updates, configuration and a
// multipart upload.
func exercise(t *testing.T, r storage.Backend, bucket string) {
	t.Helper()
	ctx := context.Background()
	put(t, r, bucket, "plain", "v0", storage.ObjectMeta{ContentType: "text/plain"})
	if err := r.SetBucketVersioning(ctx, bucket, storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	v1 := put(t, r, bucket, "doc", "one", storage.ObjectMeta{})
	put(t, r, bucket, "doc", "two", storage.ObjectMeta{Tags: map[string]string{"a": "1"}})
	put(t, r, bucket, "doc", "three", storage.ObjectMeta{})
	if _, err := r.DeleteObject(ctx, bucket, "doc", v1.VersionID); err != nil {
		t.Fatal(err)
	}
	put(t, r, bucket, "gone", "x", storage.ObjectMeta{})
	if _, err := r.DeleteObject(ctx, bucket, "gone", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UpdateObjectMeta(ctx, bucket, "plain", storage.NullVersionID, func(m *storage.ObjectMeta) error {
		m.Tags = map[string]string{"team": "x"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutBucketConfig(ctx, bucket, storage.ConfigPolicy, []byte(`{"Version":"2012-10-17"}`)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBucketVersioning(ctx, bucket, storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	put(t, r, bucket, "plain", "v1 suspended", storage.ObjectMeta{})
	up, err := r.CreateMultipartUpload(ctx, bucket, "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for n, data := range []string{"part-one-", "part-two"} {
		pw, err := r.NewPartWriter(ctx, bucket, "mp", up.UploadID, n+1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Commit(ctx, storage.PartMeta{ETag: "e"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.CompleteMultipartUpload(ctx, bucket, "mp", up.UploadID, []int{1, 2}, storage.ObjectMeta{ETag: "mp-2"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestActivePassiveReplicates(t *testing.T) {
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "photos", storage.CreateBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "photos", "before", "existing data", storage.ObjectMeta{})
	st, err := r.SetReplication(ctx, "photos", ModeActivePassive, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != ModeActivePassive || len(st.Members) != 2 || st.Members[1].Role != RoleReplica {
		t.Fatalf("placement after SetReplication: %+v", st)
	}
	exercise(t, r, "photos")
	waitState(t, r, "photos", StateInSync)
	got := waitSame(t, "photos", bk[0].Backend, bk[1].Backend)
	if !strings.Contains(got, "config policy=") || !strings.Contains(got, `versioning="Suspended"`) {
		t.Fatalf("replica is missing bucket settings:\n%s", got)
	}
	// Uploads are not copied.
	if ups, _ := bk[1].Backend.ListMultipartUploads(ctx, "photos"); len(ups) != 0 {
		t.Fatalf("replica has uploads: %v", ups)
	}
	// Removing the replica deletes its copy.
	if _, err := r.SetReplication(ctx, "photos", ModeSingle, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the replica copy to be deleted", func() bool {
		_, err := bk[1].Backend.GetBucket(ctx, "photos")
		return errors.Is(err, storage.ErrBucketNotFound)
	})
}

func TestActiveActiveWritesSynchronously(t *testing.T) {
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk, NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b"}})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "aa", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "aa", StateInSync)
	if err := r.SetBucketVersioning(ctx, "aa", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	// Every acknowledged write is on the replica at once, same version ID.
	for i := range 20 {
		info := put(t, r, "aa", "k", strings.Repeat("x", i), storage.ObjectMeta{})
		got, err := bk[1].Backend.GetObjectInfo(ctx, "aa", "k", "")
		if err != nil || got.VersionID != info.VersionID || !got.LastModified.Equal(info.LastModified) {
			t.Fatalf("write %d: replica has %+v, %v; want version %s", i, got, err, info.VersionID)
		}
	}
	res, err := r.DeleteObject(ctx, "aa", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := bk[1].Backend.GetObjectInfo(ctx, "aa", "k", "")
	if err != nil || !got.DeleteMarker || got.VersionID != res.VersionID {
		t.Fatalf("delete marker on the replica: %+v, %v (want %s)", got, err, res.VersionID)
	}
	if err := r.CreateBucket(ctx, "aa2", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "aa2", StateInSync)
	exercise(t, r, "aa2")
	if a, b := dump(t, bk[0].Backend, "aa2"), dump(t, bk[1].Backend, "aa2"); a != b {
		t.Fatalf("copies differ right after the writes:\nprimary:\n%s\nreplica:\n%s", a, b)
	}
}

func TestActiveActiveFailover(t *testing.T) {
	fa := &faulty{Backend: memory.New()}
	r := openRouter(t, Options{
		Backings:      []Backing{{Name: "a", Backend: fa}, {Name: "b", Backend: memory.New()}},
		NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b"},
	})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "ha", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "ha", StateInSync)
	put(t, r, "ha", "k", "before", storage.ObjectMeta{})

	fa.down.Store(true)
	// Reads are served by the replica at once.
	for range 4 {
		if got := read(t, r, "ha", "k", ""); got != "before" {
			t.Fatalf("read during failure = %q", got)
		}
	}
	waitFor(t, "failover to b", func() bool {
		st, _ := r.Placement("ha")
		return st.Primary == "b"
	})
	put(t, r, "ha", "k", "after", storage.ObjectMeta{})
	if got := read(t, r, "ha", "k", ""); got != "after" {
		t.Fatalf("read after failover = %q", got)
	}

	// The old primary recovers, catches up and is in sync again.
	fa.down.Store(false)
	st := waitState(t, r, "ha", StateInSync)
	if st.Primary != "b" || st.Members[1].Backing != "a" {
		t.Fatalf("placement after recovery: %+v", st)
	}
	waitSame(t, "ha", fa.Backend, r.topology.Load().backings["b"].Backend)
}

// A publication refused by the caller's BeforePublish, or invalid
// compression options, say nothing about the backing: they must not mark
// the primary unhealthy (and so fail the bucket over), whether a single
// PUT or a multipart completion is refused.
func TestRefusedPublicationKeepsPrimaryHealthy(t *testing.T) {
	// No health probes: a probe would hide a wrong setDown by clearing it.
	r := openRouter(t, Options{
		Backings: memBackings(3), NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b", "c"},
		HealthInterval: time.Hour,
	})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "ha", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "ha", StateInSync)
	denied := errors.New("publication refused")
	refuse := storage.CommitOptions{BeforePublish: func(_, _ *storage.ObjectMeta, _ time.Time, _ func(string) []byte) error {
		return denied
	}}
	wantHealthy := func(after string) {
		t.Helper()
		for _, b := range r.Backings() {
			if !b.Healthy || b.LastError != "" {
				t.Fatalf("after %s: backing %s healthy=%v lastError=%q", after, b.Name, b.Healthy, b.LastError)
			}
		}
		if st, err := r.Placement("ha"); err != nil || st.Primary != "a" {
			t.Fatalf("after %s: placement %+v, %v", after, st, err)
		}
	}

	w, err := r.NewObjectWriter(ctx, "ha", "put")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, refuse); !errors.Is(err, denied) {
		t.Fatalf("refused Commit = %v, want %v", err, denied)
	}
	wantHealthy("a refused PUT")

	up, err := r.CreateMultipartUpload(ctx, "ha", "mp", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	pw, err := r.NewPartWriter(ctx, "ha", "mp", up.UploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write([]byte("part")); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Commit(ctx, storage.PartMeta{ETag: "p1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CompleteMultipartUpload(ctx, "ha", "mp", up.UploadID, []int{1}, storage.ObjectMeta{ETag: "mp-1"}, refuse); !errors.Is(err, denied) {
		t.Fatalf("refused CompleteMultipartUpload = %v, want %v", err, denied)
	}
	wantHealthy("a refused multipart completion")

	// Invalid compression options are the caller's mistake too.
	bad := storage.WriteOptions{Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 10}}
	if w, err := r.NewObjectWriterWith(ctx, "ha", "put", bad); err == nil {
		_ = w.Abort()
		t.Fatal("NewObjectWriterWith accepted deflate level 10")
	}
	if _, err := r.CompleteMultipartUploadWith(ctx, "ha", "mp", up.UploadID, []int{1}, storage.ObjectMeta{ETag: "mp-1"}, storage.CommitOptions{}, bad); err == nil {
		t.Fatal("CompleteMultipartUploadWith accepted deflate level 10")
	}
	wantHealthy("invalid compression options")
	if _, err := r.GetMultipartUpload(ctx, "ha", "mp", up.UploadID); err != nil {
		t.Fatalf("upload lost after refused completions: %v", err)
	}
}

func TestActivePassiveReadFallbackAndAutoFailover(t *testing.T) {
	fa := &faulty{Backend: memory.New()}
	r := openRouter(t, Options{
		Backings:      []Backing{{Name: "a", Backend: fa}, {Name: "b", Backend: memory.New()}},
		NewBucketMode: ModeActivePassive, NewBucketReplicas: []string{"b"},
		AutoFailover: true,
	})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "ap", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "ap", "k", "data", storage.ObjectMeta{})
	waitState(t, r, "ap", StateInSync)
	fa.down.Store(true)
	if got := read(t, r, "ap", "k", ""); got != "data" {
		t.Fatalf("fallback read = %q", got)
	}
	waitFor(t, "automatic failover", func() bool {
		st, _ := r.Placement("ap")
		return st.Primary == "b"
	})
	put(t, r, "ap", "k2", "new", storage.ObjectMeta{})
	fa.down.Store(false)
	waitState(t, r, "ap", StateInSync)
	waitSame(t, "ap", fa.Backend, r.topology.Load().backings["b"].Backend)
}

func TestActivePassiveNoAutoFailover(t *testing.T) {
	fa := &faulty{Backend: memory.New()}
	r := openRouter(t, Options{
		Backings:      []Backing{{Name: "a", Backend: fa}, {Name: "b", Backend: memory.New()}},
		NewBucketMode: ModeActivePassive, NewBucketReplicas: []string{"b"},
	})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "ap", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, r, "ap", "k", "data", storage.ObjectMeta{})
	waitState(t, r, "ap", StateInSync)
	fa.down.Store(true)
	time.Sleep(200 * time.Millisecond) // a few health checks
	if st, _ := r.Placement("ap"); st.Primary != "a" {
		t.Fatalf("failed over without AutoFailover: %+v", st)
	}
	if _, err := r.NewObjectWriter(ctx, "ap", "k"); !errors.Is(err, errFault) {
		t.Fatalf("write with the primary down: %v", err)
	}
	// Graceful promotion is refused, forced promotion works.
	if _, err := r.Promote(ctx, "ap", "b", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("graceful promote with the primary down: %v", err)
	}
	if _, err := r.Promote(ctx, "ap", "b", true); err != nil {
		t.Fatal(err)
	}
	put(t, r, "ap", "k", "new", storage.ObjectMeta{})
	fa.down.Store(false)
	waitState(t, r, "ap", StateInSync)
	waitSame(t, "ap", fa.Backend, r.topology.Load().backings["b"].Backend)
}

func TestGracefulPromote(t *testing.T) {
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk, NewBucketMode: ModeActivePassive, NewBucketReplicas: []string{"b"}})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "p", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	exercise(t, r, "p")
	waitState(t, r, "p", StateInSync)
	before := dump(t, bk[0].Backend, "p")
	st, err := r.Promote(ctx, "p", "b", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Primary != "b" || st.Members[1].Backing != "a" {
		t.Fatalf("placement after promote: %+v", st)
	}
	if got := dump(t, bk[1].Backend, "p"); got != before {
		t.Fatalf("new primary differs from the old one:\n%s\nvs\n%s", got, before)
	}
	put(t, r, "p", "after-promote", "x", storage.ObjectMeta{})
	waitState(t, r, "p", StateInSync)
	waitSame(t, "p", bk[0].Backend, bk[1].Backend)
}

func TestMoveKeepsVersionsAndAcceptsWrites(t *testing.T) {
	dir := t.TempDir()
	bk := []Backing{fsBacking(t, "disk1", filepath.Join(dir, "d1")), fsBacking(t, "disk2", filepath.Join(dir, "d2")), {Name: "mem", Backend: memory.New()}}
	r := openRouter(t, Options{Backings: bk})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "m", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	exercise(t, r, "m")
	before := dump(t, bk[0].Backend, "m")
	if _, err := r.Move(ctx, "m", "", "disk2"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Move(ctx, "m", "", "mem"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second move: %v", err)
	}
	// Writes keep working during the move.
	put(t, r, "m", "during", "moving", storage.ObjectMeta{})
	st := waitState(t, r, "m", StateInSync)
	if st.Primary != "disk2" || len(st.Members) != 1 {
		t.Fatalf("placement after move: %+v", st)
	}
	got := dump(t, bk[1].Backend, "m")
	if !strings.Contains(got, "during") {
		t.Fatalf("write during the move is missing:\n%s", got)
	}
	// Everything but the new key is as before (fs → fs keeps IDs).
	var lines []string
	for _, l := range strings.Split(got, "\n") {
		if !strings.HasPrefix(l, "during ") {
			lines = append(lines, l)
		}
	}
	if strings.Join(lines, "\n") != before {
		t.Fatalf("moved copy differs:\n%s\nwant\n%s", got, before)
	}
	waitFor(t, "the source copy to be deleted", func() bool {
		_, err := bk[0].Backend.GetBucket(ctx, "m")
		return errors.Is(err, storage.ErrBucketNotFound)
	})
	// And on to memory (fs IDs into the memory backend).
	if _, err := r.Move(ctx, "m", "", "mem"); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "m", StateInSync)
	waitFor(t, "move to mem", func() bool { st, _ := r.Placement("m"); return st.Primary == "mem" })
	if read(t, r, "m", "during", "") != "moving" || read(t, r, "m", "mp", "") != "part-one-part-two" {
		t.Fatal("data lost in the second move")
	}
}

func TestCancelMove(t *testing.T) {
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk, CutoverTimeout: time.Second})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "c", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	// Hold a writer open so the cutover cannot happen.
	w, err := r.NewObjectWriter(ctx, "c", "held")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Move(ctx, "c", "", "b"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	st, err := r.CancelMove(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if st.Move != nil || st.Primary != "a" {
		t.Fatalf("after cancel: %+v", st)
	}
	_ = w.Abort()
	waitFor(t, "the partial copy to be deleted", func() bool {
		_, err := bk[1].Backend.GetBucket(ctx, "c")
		return errors.Is(err, storage.ErrBucketNotFound)
	})
	if _, err := r.CancelMove(ctx, "c"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel without a move: %v", err)
	}
}

func TestDeleteBucketRemovesAllCopies(t *testing.T) {
	bk := memBackings(3)
	r := openRouter(t, Options{Backings: bk, NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b", "c"}})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "d", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "d", StateInSync)
	if err := r.DeleteBucket(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	for _, b := range bk {
		waitFor(t, "copy on "+b.Name+" deleted", func() bool {
			_, err := b.Backend.GetBucket(ctx, "d")
			return errors.Is(err, storage.ErrBucketNotFound)
		})
	}
	// Recreating works (and gets the default replicas again).
	if err := r.CreateBucket(ctx, "d", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "d", StateInSync)
}

func TestPersistenceAndAdoption(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "placements.json")
	d1, d2 := filepath.Join(dir, "d1"), filepath.Join(dir, "d2")

	// A bucket that exists on d2 before the router ever saw it.
	pre := fsBacking(t, "x", d2)
	if err := pre.Backend.CreateBucket(context.Background(), "legacy", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, pre.Backend, "legacy", "k", "old", storage.ObjectMeta{})
	_ = pre.Backend.Close()

	ctx := context.Background()
	r, err := Open(ctx, Options{Backings: []Backing{fsBacking(t, "d1", d1), fsBacking(t, "d2", d2)}, StatePath: state, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.Placement("legacy"); err != nil || st.Primary != "d2" {
		t.Fatalf("adopted bucket: %+v, %v", st, err)
	}
	if err := r.CreateBucket(ctx, "rep", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	exercise(t, r, "rep")
	if _, err := r.SetReplication(ctx, "rep", ModeActiveActive, []string{"d2"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "rep", StateInSync)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2 := openRouter(t, Options{Backings: []Backing{fsBacking(t, "d1", d1), fsBacking(t, "d2", d2)}, StatePath: state})
	st, err := r2.Placement("rep")
	if err != nil || st.Mode != ModeActiveActive || st.Primary != "d1" || len(st.Members) != 2 {
		t.Fatalf("placement after restart: %+v, %v", st, err)
	}
	// Replicas are verified after a restart, then in sync again.
	waitState(t, r2, "rep", StateInSync)
	if read(t, r2, "legacy", "k", "") != "old" {
		t.Fatal("adopted bucket lost its data")
	}

	// A backing removed from the configuration: its replicas are
	// forgotten; a bucket with no remaining copy refuses to start.
	_ = r2.Close()
	_, err = Open(ctx, Options{Backings: []Backing{fsBacking(t, "d1", d1)}, StatePath: state, Logger: testLogger(t)})
	if err == nil || !strings.Contains(err.Error(), `"legacy"`) {
		t.Fatalf("Open without the only backing of a bucket: %v", err)
	}
}

func TestPlacementValidation(t *testing.T) {
	r := openRouter(t, Options{Backings: memBackings(2)})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "v", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		mode     Mode
		replicas []string
		want     error
	}{
		{"bogus", nil, ErrInvalidPlacement},
		{ModeSingle, []string{"b"}, ErrInvalidPlacement},
		{ModeActivePassive, nil, ErrInvalidPlacement},
		{ModeActivePassive, []string{"a"}, ErrInvalidPlacement},
		{ModeActivePassive, []string{"b", "b"}, ErrInvalidPlacement},
		{ModeActiveActive, []string{"zz"}, ErrUnknownBacking},
	}
	for _, c := range cases {
		if _, err := r.SetReplication(ctx, "v", c.mode, c.replicas); !errors.Is(err, c.want) {
			t.Errorf("SetReplication(%s, %v) = %v, want %v", c.mode, c.replicas, err, c.want)
		}
	}
	if _, err := r.SetReplication(ctx, "nope", ModeSingle, nil); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("unknown bucket: %v", err)
	}
	if _, err := r.Move(ctx, "v", "", "a"); !errors.Is(err, ErrConflict) {
		t.Errorf("move onto itself: %v", err)
	}
	if _, err := r.Promote(ctx, "v", "b", false); !errors.Is(err, ErrInvalidPlacement) {
		t.Errorf("promote a non-member: %v", err)
	}
	bs := r.Backings()
	if len(bs) != 2 || !bs[0].Default || bs[0].Buckets != 1 || !bs[1].Healthy {
		t.Errorf("Backings() = %+v", bs)
	}
}

func TestGateQuiesce(t *testing.T) {
	var g gate
	ctx := context.Background()
	if err := g.enter(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan func())
	go func() {
		release, err := g.quiesce(ctx)
		if err != nil {
			t.Error(err)
		}
		done <- release
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("quiesce returned while an operation was in flight")
	default:
	}
	g.leave()
	release := <-done
	// New operations wait while quiesced.
	tctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := g.enter(tctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("enter while quiesced: %v", err)
	}
	release()
	if err := g.enter(ctx); err != nil {
		t.Fatal(err)
	}
	g.leave()
}

// Copies keep compression: the synchronous active-active copy and the
// background copy of an active-passive replica both store compressed
// versions compressed, with the same content.
func TestReplicasKeepCompression(t *testing.T) {
	bk := memBackings(3)
	r := openRouter(t, Options{Backings: bk, NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b"}})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "z", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "z", StateInSync)
	data := strings.Repeat("compressible text ", 4096)
	w, err := r.NewObjectWriterWith(ctx, "z", "k", storage.WriteOptions{
		Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	info, err := w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Compression == "" {
		t.Fatal("the primary stored the version uncompressed")
	}
	// Synchronous copy (active-active), then a background copy (a new
	// active-passive replica).
	if _, err := r.SetReplication(ctx, "z", ModeActivePassive, []string{"b", "c"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "z", StateInSync)
	for _, b := range bk {
		got, err := b.Backend.GetObjectInfo(ctx, "z", "k", "")
		if err != nil {
			t.Fatalf("%s: %v", b.Name, err)
		}
		if got.Compression != storage.CompressionDeflate || got.Size != int64(len(data)) || got.StoredSize >= got.Size {
			t.Errorf("%s stores %+v", b.Name, got)
		}
		if read(t, b.Backend, "z", "k", "") != data {
			t.Errorf("%s: content differs", b.Name)
		}
	}
}
