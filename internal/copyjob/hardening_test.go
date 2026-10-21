package copyjob

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// The ordinary bucket description stays cached when an external native
// bucket is replaced. Only the job-specific identity read sees the remote.
type externalNativeIdentity struct {
	*memory.Backend
	remoteCreatedAt time.Time
}

func (b *externalNativeIdentity) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	info, err := b.Backend.GetBucket(ctx, name)
	info.Kind = storage.BucketKindNative
	info.Location = "s3://remote/"
	return info, err
}

func (b *externalNativeIdentity) GetBucketIdentity(ctx context.Context, name string) (storage.BucketInfo, error) {
	info, err := b.GetBucket(ctx, name)
	info.CreatedAt = b.remoteCreatedAt
	return info, err
}

func TestCopyJobChecksExternalNativeIncarnation(t *testing.T) {
	native := &externalNativeIdentity{Backend: memory.New(), remoteCreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	router := openRouter(t, multi.Backing{Name: "external", Kind: "s3", Backend: native}, memBacking("local"))
	createBucket(t, router, "src", storage.CreateBucketOptions{Backing: "external"}, storage.VersioningUnversioned)
	createBucket(t, router, "dst", storage.CreateBucketOptions{Backing: "local"}, storage.VersioningUnversioned)
	s := New(Options{Router: router, Logger: testLogger()})
	t.Cleanup(s.Close)
	sp, _, err := s.check(t.Context(), KindCopy, []byte(`{"source":{"bucket":"src"},"target":{"bucket":"dst"},"options":{"onExisting":"merge"}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if !sp.BucketBindings.Source.CreatedAt.Equal(native.remoteCreatedAt) {
		t.Fatalf("job pinned a cached creation time: %+v", sp.BucketBindings.Source)
	}
	r := &run{s: s, sp: sp, cp: checkpoint{Phase: phaseCopying}, tr: newTracker(Status{})}
	if err := r.refresh(t.Context()); err != nil {
		t.Fatalf("unchanged remote: %v", err)
	}
	native.remoteCreatedAt = native.remoteCreatedAt.Add(time.Hour)
	if err := r.refresh(t.Context()); !errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "source bucket") {
		t.Fatalf("replaced remote source: %v", err)
	}
}

func TestCopyJobPinsBucketIncarnations(t *testing.T) {
	router := openRouter(t, memBacking("a"))
	createBucket(t, router, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	createBucket(t, router, "dst", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	s := New(Options{Router: router, Logger: testLogger()})
	t.Cleanup(s.Close)
	sp, _, err := s.check(t.Context(), KindCopy, []byte(`{"source":{"bucket":"src"},"target":{"bucket":"dst"},"options":{"onExisting":"merge"}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if sp.BucketBindings.Source.empty() || sp.BucketBindings.Target.empty() {
		t.Fatalf("missing bucket bindings: %+v", sp.BucketBindings)
	}
	makeRun := func() *run { return &run{s: s, sp: sp, cp: checkpoint{Phase: phaseCopying}, tr: newTracker(Status{})} }
	if err := makeRun().refresh(t.Context()); err != nil {
		t.Fatalf("unchanged buckets: %v", err)
	}
	if err := router.DeleteBucket(context.Background(), "src"); err != nil {
		t.Fatal(err)
	}
	createBucket(t, router, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	if err := makeRun().refresh(t.Context()); !errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "source bucket") {
		t.Fatalf("replaced source: %v", err)
	}
	newSource, err := router.GetBucket(t.Context(), "src")
	if err != nil {
		t.Fatal(err)
	}
	sp.BucketBindings.Source = identityOf(newSource)
	if err := router.DeleteBucket(context.Background(), "dst"); err != nil {
		t.Fatal(err)
	}
	createBucket(t, router, "dst", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	if err := makeRun().refresh(t.Context()); !errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "target bucket") {
		t.Fatalf("replaced target: %v", err)
	}
}

func TestFailedMirrorRechecksConflictsOnResume(t *testing.T) {
	router := openRouter(t, memBacking("a"))
	createBucket(t, router, "src", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	createBucket(t, router, "other", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	createBucket(t, router, "dst", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	for range copier.MaxKeyVersions + 1 {
		put(t, router, "src", "hot", "x")
	}
	e := newEnv(t, router, "")
	first := e.mustSubmit(map[string]any{"kind": KindMirror, "source": map[string]any{"bucket": "src"},
		"target": map[string]any{"bucket": "dst"}})
	failed := e.waitState(first.ID, jobs.StateFailed)
	if !strings.Contains(failed.LastError, "too many versions") {
		t.Fatalf("first failure: %s", failed.LastError)
	}
	second := e.mustSubmit(map[string]any{"kind": KindMirror, "source": map[string]any{"bucket": "other"},
		"target": map[string]any{"bucket": "dst"}})
	e.waitIdle(second.ID)
	if err := e.m.Resume(first.ID); err != nil {
		t.Fatal(err)
	}
	resumed := e.waitJob(first.ID, "resumed mirror conflict", func(j jobs.Job) bool {
		return j.State == jobs.StateFailed && strings.Contains(j.LastError, second.ID)
	})
	if resumed.Phase == phasePreparing {
		t.Fatalf("conflict was not checked after preparation: %+v", resumed)
	}
}
